package index

import (
	"bytes"
	"math"
	"os"
	"testing"
)

const testFile = "../../testdata/can-example.logb"

func open(t *testing.T) *File {
	t.Helper()
	fi, err := Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// fullSeries materialises every sample of one field.
//
// Since Phase 3 the scan keeps only per-frame statistics, so getting samples
// back means decoding — which is the point: the samples are recoverable on
// demand rather than resident. Fine for a 15 kB test file; production paths go
// through query.Query, which bounds what it decodes.
func fullSeries(t *testing.T, fi *File, a *Accessor, st *Stream, fd *Field, run uint32) *Series {
	t.Helper()
	batches, err := a.Decode(st.Frames(run))
	if err != nil {
		t.Fatalf("%s.%s: %v", st.Name, fd.Name, err)
	}
	return SeriesFrom(batches, fd, fi.Epoch)
}

func TestScanFileLevel(t *testing.T) {
	fi := open(t)

	if fi.Truncated {
		t.Error("clean file reported as truncated")
	}
	if !fi.Closed {
		t.Error("file has an END frame but Closed is false")
	}
	if len(fi.Unsupported) != 0 {
		t.Errorf("unsupported: %v", fi.Unsupported)
	}
	if len(fi.Streams) != 4 {
		t.Fatalf("streams = %d, want 4", len(fi.Streams))
	}

	// The example's axis is zero-based: writeSegment uses segBase = seg*1e9,
	// and the recording's wall-clock start reaches the file only as the
	// time.anchor metadata (SPEC §5.2), never as an axis value.
	if !fi.HasEpoch || fi.Epoch != 0 {
		t.Errorf("epoch = %d (have=%v), want 0", fi.Epoch, fi.HasEpoch)
	}

	if len(fi.Attachments) != 1 || fi.Attachments[0].Name != "example.dbc" {
		t.Errorf("attachments = %+v, want one example.dbc", fi.Attachments)
	}

	var anchor bool
	for _, m := range fi.Meta {
		if m.Key == "time.anchor" {
			anchor = true
		}
	}
	if !anchor {
		// The anchor is written after the records it dates. Missing it is the
		// signature failure of reading metadata from a range rather than from
		// a whole-file pass.
		t.Error("time.anchor missing: metadata was not collected over the whole file")
	}
}

func TestFieldClassification(t *testing.T) {
	fi := open(t)
	want := map[string]map[string]Class{
		"EngineData": {
			"EngineSpeed": ClassNumeric, "CoolantTemp": ClassNumeric,
			"ThrottlePos": ClassNumeric, "EngineRunning": ClassCategorical,
		},
		"VehicleStatus": {
			"VehicleSpeed": ClassNumeric, "Odometer": ClassNumeric,
			// Gear carries a value_to_text conversion, Brake is a bool. Both
			// are states: a min/max envelope over them would be meaningless
			// and interpolating between two of them would be a lie.
			"Gear": ClassCategorical, "Brake": ClassCategorical,
		},
		"can0.raw": {
			// A byte blob has no y value, but it does have a position on the
			// axis and something to show there. That is an event lane.
			"can_id": ClassNumeric, "dlc": ClassNumeric, "payload": ClassEvent,
		},
		"events": {
			"severity": ClassCategorical, "message": ClassEvent,
		},
	}

	for name, fields := range want {
		st := streamNamed(t, fi, name)
		for fname, class := range fields {
			f := fieldNamed(t, st, fname)
			if f.Class != class {
				t.Errorf("%s.%s class = %s, want %s", name, fname, f.Class, class)
			}
		}
	}
}

// TestAxisFieldIsDerived checks that the field carrying an explicit axis is
// identified from the schema rather than by name. cmd/logbdump hardcodes
// `fd.Name == "t_us"`; nothing in the format says an axis field is called that.
func TestAxisFieldIsDerived(t *testing.T) {
	for _, name := range []string{"can0.raw", "events"} {
		st := streamNamed(t, open(t), name)
		if st.AxisMode != "explicit" {
			t.Fatalf("%s axis mode = %s, want explicit", name, st.AxisMode)
		}
		n := 0
		for i := range st.Fields {
			if st.Fields[i].IsAxis {
				n++
				if st.Fields[i].Name != "t_us" {
					t.Errorf("%s: axis field is %q", name, st.Fields[i].Name)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d axis fields, want 1", name, n)
		}
	}
}

// TestValuesMatchReference pins the first decoded record of each stream against
// what cmd/logbdump prints for the same file. logbdump is the reference
// rendering; if these two disagree, one of them is lying to the user.
func TestValuesMatchReference(t *testing.T) {
	fi, a := accessor(t)

	cases := []struct {
		stream, field string
		want          float64
		label         string
	}{
		// EngineData +0.000000s EngineSpeed=800rpm CoolantTemp=61degC ThrottlePos=4%
		{"EngineData", "EngineSpeed", 800, ""},
		{"EngineData", "CoolantTemp", 61, ""},
		{"EngineData", "ThrottlePos", 4, ""},
		{"EngineData", "EngineRunning", 1, "true"},
		// VehicleStatus +0.000000s VehicleSpeed=30km/h Odometer=40312.6km Gear="3" Brake=true
		{"VehicleStatus", "VehicleSpeed", 30, ""},
		// Odometer is the 24-bit unaligned Motorola field — the one the bit
		// numbering rules exist for (CAN.md). If the ordering is wrong this is
		// where it shows.
		{"VehicleStatus", "Odometer", 40312.6, ""},
		{"VehicleStatus", "Gear", 3, "3"},
		{"VehicleStatus", "Brake", 1, "true"},
		// can0.raw +0.000000s can_id=256 dlc=8
		{"can0.raw", "can_id", 256, ""},
		{"can0.raw", "dlc", 8, ""},
		// events +0.000000s severity="info"
		{"events", "severity", 0, "info"},
	}

	for _, c := range cases {
		st := streamNamed(t, fi, c.stream)
		f := fieldNamed(t, st, c.field)
		s := fullSeries(t, fi, a, st, f, 0)
		if s.Len() == 0 {
			t.Errorf("%s.%s: no series", c.stream, c.field)
			continue
		}
		if !s.Present[0] {
			t.Errorf("%s.%s: first sample absent", c.stream, c.field)
			continue
		}
		if math.Abs(s.Vals[0]-c.want) > 1e-6 {
			t.Errorf("%s.%s = %v, want %v", c.stream, c.field, s.Vals[0], c.want)
		}
		if c.label != "" {
			if got := f.Label(s.Vals[0]); got != c.label {
				t.Errorf("%s.%s label = %q, want %q", c.stream, c.field, got, c.label)
			}
		}
		if s.Axis.At(0) != 0 {
			t.Errorf("%s.%s first axis = %v, want 0", c.stream, c.field, s.Axis.At(0))
		}
	}
}

// TestAxisIsMonotonic guards the rebasing arithmetic. The example is written as
// three one-second segments, so a stream's samples must run 0..~3e9 ticks in
// order across the segment joins.
func TestAxisIsMonotonic(t *testing.T) {
	fi, a := accessor(t)
	st := streamNamed(t, fi, "EngineData")
	f := fieldNamed(t, st, "EngineSpeed")
	s := fullSeries(t, fi, a, st, f, 0)

	if s.Len() != 300 {
		t.Fatalf("samples = %d, want 300 (3 segments x 100)", s.Len())
	}
	for i := 1; i < s.Len(); i++ {
		if s.Axis.Ticks[i] <= s.Axis.Ticks[i-1] {
			t.Fatalf("axis not increasing at %d: %d then %d", i, s.Axis.Ticks[i-1], s.Axis.Ticks[i])
		}
	}
	// 300 samples at 10 ms, zero-based, so the last is at 2.99 s.
	if got := s.Axis.Ticks[s.Len()-1]; got != 2_990_000_000 {
		t.Errorf("last tick = %d, want 2990000000", got)
	}
}

// TestPresenceIsNeverSilentlyZero is the guard-correctness test. Wherever a
// sample is absent the value must be NaN, so that code which ignores Present
// fails loudly rather than drawing a zero (SPEC §6.2).
func TestPresenceIsNeverSilentlyZero(t *testing.T) {
	fi, a := accessor(t)
	for _, st := range fi.Streams {
		for i := range st.Fields {
			if st.Fields[i].IsAxis || st.Fields[i].Class == ClassBlob {
				continue
			}
			for _, run := range st.Runs {
				s := fullSeries(t, fi, a, st, &st.Fields[i], run.ID)
				for j, present := range s.Present {
					if !present && !math.IsNaN(s.Vals[j]) {
						t.Fatalf("%s.%s[%d]: absent sample carries value %v",
							st.Name, st.Fields[i].Name, j, s.Vals[j])
					}
				}
			}
		}
	}
}

func streamNamed(t *testing.T, fi *File, name string) *Stream {
	t.Helper()
	for _, s := range fi.Streams {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no stream %q", name)
	return nil
}

func fieldNamed(t *testing.T, st *Stream, name string) *Field {
	t.Helper()
	for i := range st.Fields {
		if st.Fields[i].Name == name {
			return &st.Fields[i]
		}
	}
	t.Fatalf("stream %s has no field %q", st.Name, name)
	return nil
}

// TestTruncationSweep cuts the file at every byte and indexes each result.
//
// Rule 2 says a file truncated by power loss is a valid file containing every
// record up to the last intact frame, so every one of these is a file the
// viewer will be asked to open. None may error, panic, or produce a series
// whose axis and value slices disagree.
func TestTruncationSweep(t *testing.T) {
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut <= len(data); cut++ {
		fi, err := Scan(bytes.NewReader(data[:cut]), testFile, int64(cut))
		if cut < 16 {
			// Short of a whole file header there is nothing to identify.
			if err == nil {
				t.Errorf("cut=%d: accepted a %d-byte header", cut, cut)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cut=%d: %v", cut, err)
		}
		for _, st := range fi.Streams {
			// Frames and their statistics must stay parallel however the file
			// was cut; a mismatch would misattribute every summary.
			if len(st.FrameList) != len(st.stats) {
				t.Fatalf("cut=%d %s: %d frames but %d stat rows",
					cut, st.Name, len(st.FrameList), len(st.stats))
			}
			records := 0
			for _, f := range st.FrameList {
				records += int(f.Count)
				if f.End() > uint64(cut) {
					t.Fatalf("cut=%d %s: frame ends at %d, past the file", cut, st.Name, f.End())
				}
			}
			if records != st.Records {
				t.Fatalf("cut=%d %s: frames hold %d records, stream says %d",
					cut, st.Name, records, st.Records)
			}
		}
	}
}
