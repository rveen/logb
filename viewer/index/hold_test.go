package index

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

func rackSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("rack/psu1.ch1")),
		Name:       "psu1.ch1",
		RecordBits: 8 * 16,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "v.set", BitOffset: 64, BitWidth: 32, Type: logb.TypeFloat, Unit: "V", Hold: true},
		},
	}
}

func rackRec(t uint64, set float32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], t)
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(set))
	return b
}

// rackFile writes a setpoint moved once at t=1s, then several empty segments
// restating it — the shape a long recording of a rack actually has.
func rackFile(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := logb.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rackRec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, logb.TickVal(1_000_000_000), 0, []bool{false, true},
		rackRec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.BeginSegment(0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestHoldFlagAndRestatementsIndexed(t *testing.T) {
	data := rackFile(t)
	fi, err := Scan(bytes.NewReader(data), "rack.logb", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Streams[0]

	if st.Fields[0].Hold {
		t.Error("the axis field is not held")
	}
	if !st.Fields[1].Hold {
		t.Fatal("v.set is held in the schema but not in the index")
	}
	if len(st.Holds) != 3 {
		t.Fatalf("holds = %d, want 3 (one per segment opened after the value was set)", len(st.Holds))
	}
	// The restatement is not a record. Counting it would put a spurious sample
	// on the channel at every segment boundary.
	if st.Records != 1 {
		t.Fatalf("records = %d, want 1", st.Records)
	}
	for i, h := range st.Holds {
		if !h.Present[1] {
			t.Fatalf("hold %d has no value for v.set", i)
		}
		if h.Vals[1] != 12 {
			t.Fatalf("hold %d v.set = %v, want 12", i, h.Vals[1])
		}
		// Rebased onto the file epoch, which is the first sample at t=1s.
		if h.Axis != 0 {
			t.Fatalf("hold %d axis = %v, want 0 after rebasing", i, h.Axis)
		}
	}
}

// TestHoldAtPrefersTheLaterEvidence is the rule HoldAt exists to apply: frame
// statistics and restatements both answer "what was in force here", and the
// more recent one wins.
func TestHoldAtPrefersTheLaterEvidence(t *testing.T) {
	var out bytes.Buffer
	w, err := logb.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	// Set to 12 V at 1 s, restated, then moved to 5 V at 2 s in a later frame.
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rackRec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, logb.TickVal(1_000_000_000), 0, []bool{false, true},
		rackRec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rackRec(2_000_000_000, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fi, err := Scan(bytes.NewReader(out.Bytes()), "rack.logb", int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Streams[0]

	// Axis positions are epoch-relative: the epoch is the first sample, so the
	// 1 s change sits at 0 and the 2 s change at 1e9 ticks.
	const (
		beforeAnything = -1.0
		between        = 0.5e9 // 1.5 s absolute
		afterBoth      = 2e9   // 3 s absolute
	)

	// Nothing was in force before the first record: a gap, not a zero.
	if _, _, ok := st.HoldAt(1, beforeAnything); ok {
		t.Error("a value was reported in force before anything was set")
	}
	// Between the two changes, the restatement and the first frame agree.
	v, since, ok := st.HoldAt(1, between)
	if !ok || v != 12 {
		t.Fatalf("between the changes: %v, %v", v, ok)
	}
	if since != 0 {
		t.Fatalf("since = %v, want 0 (the epoch-relative position of the 1 s change)", since)
	}
	// After the second change the frame evidence is newer than the restatement,
	// which still names the old value. The newer one must win.
	v, since, ok = st.HoldAt(1, afterBoth)
	if !ok || v != 5 {
		t.Fatalf("after both: %v (since %v), want 5 — the restatement is stale here", v, since)
	}
}

// TestHoldSurvivesACutFile is the point of the frame: with the segment that
// carried the change gone, the restatement is the only evidence left.
func TestHoldSurvivesACutFile(t *testing.T) {
	full := rackFile(t)

	// Keep the file header and drop everything up to the last segment, the way
	// a consumer joining a stream in progress sees it.
	whole, err := Scan(bytes.NewReader(full), "rack.logb", int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	segs := whole.Frames.Segments
	cut := int64(segs[len(segs)-1].Sync.Offset)

	tail := append(append([]byte{}, full[:16]...), full[cut:]...)
	fi, err := Scan(bytes.NewReader(tail), "cut.logb", int64(len(tail)))
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Streams[0]

	if st.Records != 0 {
		t.Fatalf("records = %d in the tail; the cut kept a record it should not have", st.Records)
	}
	if len(st.Holds) != 1 {
		t.Fatalf("holds = %d, want 1", len(st.Holds))
	}
	// No frame evidence at all here, so the restatement is doing all the work.
	v, _, ok := st.HoldAt(1, 1e12)
	if !ok || v != 12 {
		t.Fatalf("cut file: v.set = %v, ok = %v; want 12 — this is what the HOLD frame is for", v, ok)
	}
}
