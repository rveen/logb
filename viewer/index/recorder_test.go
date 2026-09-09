package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

func recUID(s string) [16]byte { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(s)) }

// busSchema is an explicit-axis stream, the shape a bus recorder writes.
func busSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       recUID("rec/can0.raw"),
		Name:       "can0.raw",
		RecordBits: 64 + 32 + 8 + 64,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "can_id", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint},
			{Name: "flags", BitOffset: 96, BitWidth: 8, Type: logb.TypeUint},
			{Name: "payload", BitOffset: 104, BitWidth: 64, Type: logb.TypeBytes},
		},
	}
}

func busRec(t uint64, id uint32, flags byte, pay uint64) []byte {
	b := make([]byte, 21)
	binary.LittleEndian.PutUint64(b[0:], t)
	binary.LittleEndian.PutUint32(b[8:], id)
	b[12] = flags
	binary.LittleEndian.PutUint64(b[13:], pay)
	return b
}

// setpointSchema is a held stream, so the comparison covers HOLD frames.
func setpointSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       recUID("rec/psu1.ch1"),
		Name:       "psu1.ch1",
		RecordBits: 128,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "v.set", BitOffset: 64, BitWidth: 32, Type: logb.TypeFloat, Unit: "V", Hold: true},
			{Name: "v.meas", BitOffset: 96, BitWidth: 32, Type: logb.TypeFloat, Unit: "V"},
		},
	}
}

func setpointRec(t uint64, set, meas float32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], t)
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(set))
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(meas))
	return b
}

// writeRecorded produces a file through a Recorder and returns its bytes
// alongside the index the Recorder built while writing it.
func writeRecorded(t *testing.T, path string, codec logb.Codec) ([]byte, *File) {
	t.Helper()

	var out bytes.Buffer
	r, err := NewRecorder(&out, path)
	if err != nil {
		t.Fatal(err)
	}
	r.Codec = codec

	bus, psu := busSchema(), setpointSchema()
	if err := r.AddStream(bus); err != nil {
		t.Fatal(err)
	}
	if err := r.AddStream(psu); err != nil {
		t.Fatal(err)
	}

	const epoch = 1_700_000_000_000_000_000
	if err := r.BeginSegment(epoch); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta("recorder", "index.Recorder"); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteAttach("source.dbc", []byte("VERSION \"x\"\n")); err != nil {
		t.Fatal(err)
	}

	for seg := 0; seg < 4; seg++ {
		if seg > 0 {
			if err := r.BeginSegment(epoch + int64(seg)*1e9); err != nil {
				t.Fatal(err)
			}
		}
		base := logb.TickVal(int64(seg) * 1e9)

		// A hundred bus frames per segment, values that vary so the Tier 1
		// minima and maxima are not all identical.
		var recs []byte
		for i := 0; i < 100; i++ {
			recs = append(recs, busRec(uint64(i)*1_000_000, uint32(0x100+i%3),
				byte(i%7), uint64(i*i))...)
		}
		if err := r.WriteData(bus, base, 0, 100, recs); err != nil {
			t.Fatal(err)
		}

		// The setpoint moves once per segment and is restated thereafter.
		set := float32(12 + seg)
		if err := r.WriteData(psu, base, 0, 1, setpointRec(0, set, set-0.02)); err != nil {
			t.Fatal(err)
		}
		if err := r.SetHold(psu, base, 0, []bool{false, true, false},
			setpointRec(0, set, set-0.02)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("the recorder refused to index: %v", err)
	}

	b := out.Bytes()
	return b, r.File(int64(len(b)))
}

// This is the claim the whole idea rests on: an index built while writing is
// the index a scan would have produced, not merely a similar one. If they can
// differ, the cheap path is a second opinion and cannot be trusted.
func TestRecordedIndexEqualsScannedIndex(t *testing.T) {
	for _, codec := range []struct {
		name string
		c    logb.Codec
	}{
		{"none", logb.CodecNone},
		{"zstd", logb.CodecZstd},
	} {
		t.Run(codec.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rec.logb")
			data, recorded := writeRecorded(t, path, codec.c)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			scanned, err := Scan(bytes.NewReader(data), path, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			diffFiles(t, recorded, scanned)
		})
	}
}

// The sidecar a Recorder saves must be accepted by a later Open, which must
// then not scan at all.
func TestRecordedSidecarIsUsedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.logb")

	// Record to the real path, so that Save fingerprints the file on disk:
	// a sidecar written against a half-flushed file is rejected at open.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecorder(f, path)
	if err != nil {
		t.Fatal(err)
	}
	replay(t, rec)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

	var missed error
	fi, err := OpenWith(path, Options{OnCacheMiss: func(e error) { missed = e }})
	if err != nil {
		t.Fatal(err)
	}
	if missed != nil {
		t.Fatalf("the sidecar the recorder wrote was rejected: %v", missed)
	}
	if !fi.Cached {
		t.Error("Open scanned the file instead of using the recorded sidecar")
	}

	// And it must agree with a scan of the same file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := Scan(bytes.NewReader(raw), path, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	diffFiles(t, fi, scanned)
}

// A stream with a variable-length field cannot be summarised from the bytes a
// writer is about to emit, and the Recorder must say so rather than write an
// index that quietly lacks statistics.
func TestRecorderRefusesVariableFields(t *testing.T) {
	s := &logb.Schema{
		UUID:       recUID("rec/events"),
		Name:       "events",
		RecordBits: 64,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "msg", Type: logb.TypeString, Variable: true},
		},
	}
	var out bytes.Buffer
	r, err := NewRecorder(&out, filepath.Join(t.TempDir(), "e.logb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginSegment(1); err != nil {
		t.Fatal(err)
	}
	if r.Err() == nil {
		t.Fatal("a variable-length stream was accepted for indexing")
	}
	if err := r.Save(); err == nil {
		t.Error("Save wrote a sidecar for an index it knew was incomplete")
	}
}

// replay writes the same content as writeRecorded, through an existing
// Recorder, so that a file on disk and its sidecar come from one pass.
func replay(t *testing.T, r *Recorder) {
	t.Helper()
	bus, psu := busSchema(), setpointSchema()
	r.Codec = logb.CodecZstd
	if err := r.AddStream(bus); err != nil {
		t.Fatal(err)
	}
	if err := r.AddStream(psu); err != nil {
		t.Fatal(err)
	}
	const epoch = 1_700_000_000_000_000_000
	if err := r.BeginSegment(epoch); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteMeta("recorder", "index.Recorder"); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteAttach("source.dbc", []byte("VERSION \"x\"\n")); err != nil {
		t.Fatal(err)
	}
	for seg := 0; seg < 4; seg++ {
		if seg > 0 {
			if err := r.BeginSegment(epoch + int64(seg)*1e9); err != nil {
				t.Fatal(err)
			}
		}
		base := logb.TickVal(int64(seg) * 1e9)
		var recs []byte
		for i := 0; i < 100; i++ {
			recs = append(recs, busRec(uint64(i)*1_000_000, uint32(0x100+i%3),
				byte(i%7), uint64(i*i))...)
		}
		if err := r.WriteData(bus, base, 0, 100, recs); err != nil {
			t.Fatal(err)
		}
		set := float32(12 + seg)
		if err := r.WriteData(psu, base, 0, 1, setpointRec(0, set, set-0.02)); err != nil {
			t.Fatal(err)
		}
		if err := r.SetHold(psu, base, 0, []bool{false, true, false},
			setpointRec(0, set, set-0.02)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// diffFiles compares two index models field by field, reporting the first
// disagreement in each area rather than a single opaque inequality.
func diffFiles(t *testing.T, got, want *File) {
	t.Helper()

	if got.Epoch != want.Epoch || got.HasEpoch != want.HasEpoch {
		t.Errorf("epoch: recorded %d/%v, scanned %d/%v",
			got.Epoch, got.HasEpoch, want.Epoch, want.HasEpoch)
	}
	if len(got.Meta) != len(want.Meta) {
		t.Errorf("meta: recorded %d pairs, scanned %d", len(got.Meta), len(want.Meta))
	} else {
		for i := range got.Meta {
			if got.Meta[i] != want.Meta[i] {
				t.Errorf("meta %d: recorded %v, scanned %v", i, got.Meta[i], want.Meta[i])
			}
		}
	}
	if len(got.Attachments) != len(want.Attachments) {
		t.Errorf("attachments: recorded %d, scanned %d", len(got.Attachments), len(want.Attachments))
	} else {
		for i := range got.Attachments {
			a, b := got.Attachments[i], want.Attachments[i]
			if a.Name != b.Name || a.Size != b.Size || !bytes.Equal(a.Data, b.Data) {
				t.Errorf("attachment %d: recorded %s/%d, scanned %s/%d", i, a.Name, a.Size, b.Name, b.Size)
			}
		}
	}

	diffFrames(t, got.Frames, want.Frames)

	if len(got.Streams) != len(want.Streams) {
		t.Fatalf("streams: recorded %d, scanned %d", len(got.Streams), len(want.Streams))
	}
	for i := range got.Streams {
		a, b := got.Streams[i], want.Streams[i]
		if a.UUID != b.UUID || a.Name != b.Name {
			t.Errorf("stream %d identity: recorded %s/%s, scanned %s/%s", i, a.Name, a.UUID, b.Name, b.UUID)
			continue
		}
		if a.Records != b.Records {
			t.Errorf("stream %s: recorded %d records, scanned %d", a.Name, a.Records, b.Records)
		}
		if a.AxisMin != b.AxisMin || a.AxisMax != b.AxisMax || a.HasSpan != b.HasSpan {
			t.Errorf("stream %s span: recorded [%v,%v]%v, scanned [%v,%v]%v",
				a.Name, a.AxisMin, a.AxisMax, a.HasSpan, b.AxisMin, b.AxisMax, b.HasSpan)
		}
		if len(a.FrameList) != len(b.FrameList) {
			t.Errorf("stream %s: recorded %d frames, scanned %d", a.Name, len(a.FrameList), len(b.FrameList))
		} else {
			for j := range a.FrameList {
				if a.FrameList[j] != b.FrameList[j] {
					t.Errorf("stream %s frame %d: recorded %+v, scanned %+v",
						a.Name, j, a.FrameList[j], b.FrameList[j])
					break
				}
			}
		}
		diffStats(t, a, b)
		diffHolds(t, a, b)
	}
}

func diffFrames(t *testing.T, got, want *FrameIndex) {
	t.Helper()
	if len(got.Segments) != len(want.Segments) {
		t.Errorf("segments: recorded %d, scanned %d", len(got.Segments), len(want.Segments))
	} else {
		for i := range got.Segments {
			a, b := got.Segments[i], want.Segments[i]
			if a.Sync != b.Sync {
				t.Errorf("segment %d sync: recorded %+v, scanned %+v", i, a.Sync, b.Sync)
			}
			if fmt.Sprint(a.Schemas) != fmt.Sprint(b.Schemas) {
				t.Errorf("segment %d schemas: recorded %v, scanned %v", i, a.Schemas, b.Schemas)
			}
			if fmt.Sprint(a.UUIDs) != fmt.Sprint(b.UUIDs) {
				t.Errorf("segment %d uuids: recorded %v, scanned %v", i, a.UUIDs, b.UUIDs)
			}
		}
	}
	if len(got.Data) != len(want.Data) {
		t.Fatalf("data frames: recorded %d, scanned %d", len(got.Data), len(want.Data))
	}
	for i := range got.Data {
		if got.Data[i] != want.Data[i] {
			t.Fatalf("data frame %d:\n recorded %+v\n scanned  %+v", i, got.Data[i], want.Data[i])
		}
	}
}

func diffStats(t *testing.T, got, want *Stream) {
	t.Helper()
	if len(got.stats) != len(want.stats) {
		t.Errorf("stream %s: recorded %d stat rows, scanned %d", got.Name, len(got.stats), len(want.stats))
		return
	}
	for i := range got.stats {
		if len(got.stats[i]) != len(want.stats[i]) {
			t.Errorf("stream %s row %d: recorded %d fields, scanned %d",
				got.Name, i, len(got.stats[i]), len(want.stats[i]))
			return
		}
		for j := range got.stats[i] {
			a, b := got.stats[i][j], want.stats[i][j]
			// NaN is the ordinary result for a field with no numeric sample,
			// and NaN != NaN, so compare rendered.
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("stream %s frame %d field %d: recorded %+v, scanned %+v",
					got.Name, i, j, a, b)
				return
			}
		}
	}
}

func diffHolds(t *testing.T, got, want *Stream) {
	t.Helper()
	if len(got.Holds) != len(want.Holds) {
		t.Errorf("stream %s: recorded %d holds, scanned %d", got.Name, len(got.Holds), len(want.Holds))
		return
	}
	for i := range got.Holds {
		a, b := got.Holds[i], want.Holds[i]
		if a.Axis != b.Axis || a.RunID != b.RunID || a.Offset != b.Offset ||
			fmt.Sprint(a.Vals) != fmt.Sprint(b.Vals) || fmt.Sprint(a.Present) != fmt.Sprint(b.Present) {
			t.Errorf("stream %s hold %d:\n recorded %+v\n scanned  %+v", got.Name, i, a, b)
			return
		}
	}
}
