package logb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"
)

// rackSchema is the shape §5.1 is about: a supply channel written on change,
// with the three-way split between what was asked for, what was accepted and
// what was measured. The axis is explicit because a log-on-change stream is
// equally spaced in nothing.
func rackSchema() *Schema {
	return &Schema{
		UUID:       uid("rack/psu1.ch1"),
		Name:       "psu1.ch1",
		RecordBits: 8 * 16,
		AxisKind:   AxisTime,
		AxisMode:   AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  TickVal(1),
		AxisField:  0,
		Fields: []Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: TypeUint},
			{Name: "v.set", BitOffset: 64, BitWidth: 32, Type: TypeFloat, Unit: "V", Hold: true},
			{Name: "v.applied", BitOffset: 96, BitWidth: 32, Type: TypeFloat, Unit: "V", Hold: true},
		},
	}
}

func encodeRackRec(t uint64, set, applied float32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], t)
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(set))
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(applied))
	return b
}

// TestHoldSurvivesACut is the §5.1 claim itself: a reader handed the middle of
// a file recovers not only the schema but the value in force.
func TestHoldSurvivesACut(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}

	// Segment 1: the operator sets 12 V. Nothing is restated yet, because
	// nothing was in force when the segment opened.
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, encodeRackRec(1_000_000_000, 12, 11.98)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(1_000_000_000), 0, []bool{false, true, true},
		encodeRackRec(1_000_000_000, 12, 11.98)); err != nil {
		t.Fatal(err)
	}

	// Forty minutes of segments in which the setpoint does not move.
	for seg := 0; seg < 3; seg++ {
		if err := w.BeginSegment(0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	full := out.Bytes()

	// Throw away the header and the segment that carried the record.
	r, _, err := Resync(full[len(full)/2:])
	if err != nil {
		t.Fatalf("resync failed: %v", err)
	}
	var got *Hold
	r.OnHold = func(h *Hold) { got = h }
	for {
		if _, err := r.Next(); err != nil {
			break
		}
	}
	if r.Truncated {
		t.Fatal("cut file reported as damaged")
	}
	if got == nil {
		t.Fatal("no HOLD frame after a resync; the setpoint is unrecoverable")
	}

	if !got.Has(1) || !got.Has(2) {
		t.Fatalf("held fields not present: %v", got.present)
	}
	v, err := got.Value(1)
	if err != nil {
		t.Fatal(err)
	}
	if v.(float64) != 12 {
		t.Fatalf("v.set = %v, want 12", v)
	}
	// The axis says when it was set, which is before the segment that restates
	// it. That is the whole point: a step drawn from the left edge of the chart
	// would be a different claim.
	if got.AxisBase.Ticks() != 1_000_000_000 {
		t.Fatalf("AxisBase = %d, want 1e9", got.AxisBase.Ticks())
	}
	// The axis field itself was not restated, and says so.
	if got.Has(0) {
		t.Fatal("field 0 restated, but present said otherwise")
	}
	if _, err := got.Value(0); !errors.Is(err, ErrFieldAbsent) {
		t.Fatalf("absent field: %v, want ErrFieldAbsent", err)
	}
}

// TestHoldIsNotARecord: a restatement must never reach a plot as a sample, or
// every held channel gains a spurious point at every segment boundary.
func TestHoldIsNotARecord(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(1), 0, []bool{false, true, true}, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := w.BeginSegment(0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	holds := 0
	r.OnHold = func(*Hold) { holds++ }
	records := 0
	for {
		b, err := r.Next()
		if err != nil {
			break
		}
		records += int(b.Count)
	}
	if records != 1 {
		t.Fatalf("records = %d, want 1; a restatement was returned as data", records)
	}
	if holds != 4 {
		t.Fatalf("holds = %d, want 4 (one per segment opened after the value was set)", holds)
	}
}

// TestHoldIsSkippedByAReaderThatDoesNotKnowIt is the compatibility claim that
// makes 0x14 free: an unknown frame type is skipped by length, so a reader
// written before HOLD existed reads such a file exactly as it reads any other.
// The frame type is rewritten to an unallocated id to stand in for that reader.
func TestHoldIsSkippedByAReaderThatDoesNotKnowIt(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(1), 0, []bool{false, true, true}, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, encodeRackRec(2, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	patched := patchFrameType(t, out.Bytes(), FrameHold, 0x77)
	if patched == 0 {
		t.Fatal("no HOLD frame found to patch")
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for {
		b, err := r.Next()
		if err != nil {
			break
		}
		seen += int(b.Count)
	}
	if r.Truncated {
		t.Fatal("an unknown frame type was treated as damage")
	}
	if seen != 2 {
		t.Fatalf("records = %d, want 2", seen)
	}
}

// patchFrameType rewrites every frame of type from to type to, fixing each
// frame's CRC, and returns how many it changed. It walks the frame chain the
// way the reader does — length, type, flags, stream id, payload, CRC — rather
// than searching for bytes.
func patchFrameType(t *testing.T, file []byte, from, to FrameType) int {
	t.Helper()
	n := 0
	for i := 16; i+12 <= len(file); {
		length := int(binary.LittleEndian.Uint32(file[i:]))
		if i+8+length+4 > len(file) {
			t.Fatalf("frame at %d runs past the file", i)
		}
		if FrameType(file[i+4]) == from {
			file[i+4] = byte(to)
			sum := crc32Update(crc32Of(file[i:i+8]), file[i+8:i+8+length])
			binary.LittleEndian.PutUint32(file[i+8+length:], sum)
			n++
		}
		i += 8 + length + 4
	}
	return n
}

func TestHoldWithAVariableField(t *testing.T) {
	s := &Schema{
		UUID:       uid("rack/mode"),
		Name:       "psu1.mode",
		RecordBits: 8,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -9,
		AxisStep:   TickVal(1000),
		Fields: []Field{
			{Name: "code", BitOffset: 0, BitWidth: 8, Type: TypeUint, Hold: true},
			{Name: "label", Variable: true, Type: TypeString, Hold: true},
		},
	}
	rec := append([]byte{3}, encodeTail("constant-voltage")...)

	var out bytes.Buffer
	w, _ := NewWriter(&out)
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(500), 0, []bool{true, true}, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var got *Hold
	r.OnHold = func(h *Hold) { got = h }
	for {
		if _, err := r.Next(); err != nil {
			break
		}
	}
	if got == nil {
		t.Fatal("no hold")
	}
	v, err := got.Value(1)
	if err != nil {
		t.Fatal(err)
	}
	if v.(string) != "constant-voltage" {
		t.Fatalf("label = %q", v)
	}
	if v, err := got.Value(0); err != nil || v.(uint64) != 3 {
		t.Fatalf("code = %v, %v", v, err)
	}
}

func encodeTail(s string) []byte {
	b := make([]byte, 4+len(s))
	binary.LittleEndian.PutUint32(b, uint32(len(s)))
	copy(b[4:], s)
	return b
}

func TestSetHoldRejectsAMismatch(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(0), 0, []bool{true, true}, encodeRackRec(1, 5, 5)); !errors.Is(err, ErrBadHold) {
		t.Errorf("short present vector: %v, want ErrBadHold", err)
	}
	if err := w.SetHold(s, TickVal(0), 0, []bool{false, true, true}, make([]byte, 4)); !errors.Is(err, ErrBadHold) {
		t.Errorf("short record: %v, want ErrBadHold", err)
	}
	if err := w.SetHold(nil, TickVal(0), 0, nil, nil); !errors.Is(err, ErrBadHold) {
		t.Errorf("nil schema: %v, want ErrBadHold", err)
	}
}

func TestHoldFlagRoundTrips(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, true}
	for i, f := range b.Schema.Fields {
		if f.Hold != want[i] {
			t.Fatalf("field %q Hold = %v, want %v", f.Name, f.Hold, want[i])
		}
	}
}

// TestClearHoldStopsRestating: a released channel stops being restated rather
// than freezing at its last value forever.
func TestClearHold(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := rackSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, TickVal(1), 0, []bool{false, true, true}, encodeRackRec(1, 5, 5)); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	w.ClearHold(s)
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	holds := 0
	r.OnHold = func(*Hold) { holds++ }
	for {
		if _, err := r.Next(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			break
		}
	}
	if holds != 1 {
		t.Fatalf("holds = %d, want 1", holds)
	}
}

// TestAxisStepMustOpenASegment is §5.2: an operator turning the scope's
// timebase knob changes the sample interval, and the interval is declared in a
// frame that is only restated at a segment boundary. Writing the faster records
// under the old declaration is the silent-wrong-answer case.
func TestAxisStepMustOpenASegment(t *testing.T) {
	s := &Schema{
		UUID:       uid("scope/timebase"),
		Name:       "scope1.ch1",
		RecordBits: 16,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -12,
		AxisUnit:   "s",
		AxisStep:   TickVal(1_000_000), // 1 µs/sample
		Fields: []Field{
			{Name: "ch1", BitOffset: 0, BitWidth: 16, Type: TypeSint},
		},
	}
	rec := []byte{0, 0}

	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}

	// The knob turns: 100 ns/sample.
	s.AxisStep = TickVal(100_000)
	if err := w.WriteData(s, TickVal(0), 0, 1, rec); !errors.Is(err, ErrAxisStepChanged) {
		t.Fatalf("mid-segment timebase change: %v, want ErrAxisStepChanged", err)
	}

	// Opening a segment restates the schema, and the same write is then correct.
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 0, 1, rec); err != nil {
		t.Fatalf("after a segment boundary: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// And a reader sees both intervals, each governing its own segment.
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var steps []int64
	for {
		b, err := r.Next()
		if err != nil {
			break
		}
		steps = append(steps, b.Schema.AxisStep.Ticks())
	}
	if len(steps) != 2 || steps[0] != 1_000_000 || steps[1] != 100_000 {
		t.Fatalf("axis steps = %v, want [1000000 100000]", steps)
	}
}
