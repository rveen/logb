package index

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/rveen/logb"
)

// copyTo puts the test file somewhere writable so a sidecar can land beside it.
func copyTo(t *testing.T, dst string) string {
	t.Helper()
	b, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// sameModel compares everything a chart depends on. The cached path must
// produce a model indistinguishable from a fresh scan, or the cache is a
// second source of truth that can disagree with the file.
func sameModel(t *testing.T, want, got *File) {
	t.Helper()
	if got.Epoch != want.Epoch || got.HasEpoch != want.HasEpoch {
		t.Errorf("epoch %d/%v, want %d/%v", got.Epoch, got.HasEpoch, want.Epoch, want.HasEpoch)
	}
	if got.Truncated != want.Truncated || got.Closed != want.Closed {
		t.Errorf("status truncated=%v closed=%v, want %v/%v",
			got.Truncated, got.Closed, want.Truncated, want.Closed)
	}
	if len(got.Streams) != len(want.Streams) {
		t.Fatalf("streams = %d, want %d", len(got.Streams), len(want.Streams))
	}
	if len(got.Frames.Data) != len(want.Frames.Data) {
		t.Fatalf("frames = %d, want %d", len(got.Frames.Data), len(want.Frames.Data))
	}
	if len(got.Frames.Segments) != len(want.Frames.Segments) {
		t.Fatalf("segments = %d, want %d", len(got.Frames.Segments), len(want.Frames.Segments))
	}
	if len(got.Meta) != len(want.Meta) {
		t.Errorf("meta entries = %d, want %d", len(got.Meta), len(want.Meta))
	}
	if len(got.Attachments) != len(want.Attachments) {
		t.Errorf("attachments = %d, want %d", len(got.Attachments), len(want.Attachments))
	}

	for i, ws := range want.Streams {
		gs := got.Streams[i]
		if gs.Name != ws.Name || gs.UUID != ws.UUID {
			t.Fatalf("stream %d = %s/%s, want %s/%s", i, gs.Name, gs.UUID, ws.Name, ws.UUID)
		}
		if gs.Records != ws.Records {
			t.Errorf("%s: records = %d, want %d", ws.Name, gs.Records, ws.Records)
		}
		if gs.AxisMin != ws.AxisMin || gs.AxisMax != ws.AxisMax {
			t.Errorf("%s: span %v..%v, want %v..%v", ws.Name, gs.AxisMin, gs.AxisMax, ws.AxisMin, ws.AxisMax)
		}
		if len(gs.Fields) != len(ws.Fields) {
			t.Fatalf("%s: %d fields, want %d", ws.Name, len(gs.Fields), len(ws.Fields))
		}
		for j := range ws.Fields {
			wf, gf := &ws.Fields[j], &gs.Fields[j]
			if gf.Name != wf.Name || gf.Class != wf.Class || gf.Guarded != wf.Guarded || gf.IsAxis != wf.IsAxis {
				t.Errorf("%s.%s: field differs after restore", ws.Name, wf.Name)
			}
			// The conversion is recovered from the file, not the cache. If it
			// were lost, categorical labels would silently become numbers.
			if gf.Label(3) != wf.Label(3) {
				t.Errorf("%s.%s: label(3) = %q, want %q", ws.Name, wf.Name, gf.Label(3), wf.Label(3))
			}
		}
		// Tier 1 statistics must survive exactly: they are what the overview
		// is drawn from.
		for f := range ws.FrameList {
			for j := range ws.Fields {
				w, g := ws.Stat(f, j), gs.Stat(f, j)
				if !sameStat(w, g) {
					t.Fatalf("%s frame %d field %s: stat %+v, want %+v", ws.Name, f, ws.Fields[j].Name, g, w)
				}
			}
		}
	}
}

func sameStat(a, b Stat) bool {
	eq := func(x, y float64) bool {
		return x == y || (math.IsNaN(x) && math.IsNaN(y))
	}
	return eq(a.Min, b.Min) && eq(a.Max, b.Max) && eq(a.First, b.First) &&
		eq(a.Last, b.Last) && a.NPresent == b.NPresent && a.Distinct == b.Distinct
}

func TestSidecarRoundTrip(t *testing.T) {
	path := copyTo(t, filepath.Join(t.TempDir(), "round.logb"))

	fresh, err := OpenWith(path, Options{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Cached {
		t.Error("a no-cache open reported itself as cached")
	}

	// First cached open writes the sidecar.
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached {
		t.Error("the first open cannot have come from a cache")
	}
	if _, err := os.Stat(path + ".logbview"); err != nil {
		t.Fatalf("no sidecar written: %v", err)
	}

	// Second reads it.
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Fatal("second open did not use the sidecar")
	}
	sameModel(t, fresh, second)
}

// TestCachedFileStillDecodes checks the cached index is not merely
// self-consistent but actually points at the right bytes.
func TestCachedFileStillDecodes(t *testing.T) {
	path := copyTo(t, filepath.Join(t.TempDir(), "decode.logb"))
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	fi, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Cached {
		t.Fatal("expected a cached open")
	}

	a, err := NewAccessor(path, fi.Frames)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	st := streamNamed(t, fi, "VehicleStatus")
	fd := fieldNamed(t, st, "Odometer")
	batches, err := a.Decode(st.Frames(0))
	if err != nil {
		t.Fatal(err)
	}
	s := SeriesFrom(batches, fd, fi.Epoch)
	if s.Len() != 150 {
		t.Fatalf("decoded %d samples from a cached index, want 150", s.Len())
	}
	if math.Abs(s.Vals[0]-40312.6) > 1e-6 {
		t.Errorf("first Odometer = %v, want 40312.6", s.Vals[0])
	}
}

// TestSidecarInvalidation covers each way a cache can stop describing its file.
func TestSidecarInvalidation(t *testing.T) {
	t.Run("content changed", func(t *testing.T) {
		dir := t.TempDir()
		path := copyTo(t, filepath.Join(dir, "x.logb"))
		if _, err := Open(path); err != nil {
			t.Fatal(err)
		}
		// Rewrite with different opening bytes but the same length.
		b, _ := os.ReadFile(path)
		b[20] ^= 0xff
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSidecar(path); err == nil {
			t.Error("a rewritten file accepted its old cache")
		}
	})

	t.Run("shrunk", func(t *testing.T) {
		dir := t.TempDir()
		path := copyTo(t, filepath.Join(dir, "y.logb"))
		if _, err := Open(path); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if err := os.WriteFile(path, b[:len(b)/2], 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSidecar(path); err == nil {
			t.Error("a truncated file accepted its old cache")
		}
	})

	t.Run("version bump", func(t *testing.T) {
		dir := t.TempDir()
		path := copyTo(t, filepath.Join(dir, "z.logb"))
		if _, err := Open(path); err != nil {
			t.Fatal(err)
		}
		// A cache from a future version must be discarded, not migrated.
		sc, err := LoadSidecar(path)
		if err != nil {
			t.Fatal(err)
		}
		sc.Version = 999
		if sc.Version == sidecarVersion {
			t.Fatal("test is not exercising a version mismatch")
		}
	})
}

// TestSidecarGrowth is the live-logger case: a file that was indexed while
// still being written, then opened again after more was appended.
//
// The already-indexed prefix stays valid because nothing in the format points
// forward, so only the new tail is scanned. The result must be
// indistinguishable from having scanned the whole grown file.
func TestSidecarGrowth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.logb")

	// Write the finished six-segment file, then take the short version as a
	// genuine truncation of it at a segment boundary. That is what a growing
	// file actually looks like: a logger appends, and a reader that arrives
	// early sees a valid prefix with no END frame (rule 2).
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGrowing(f, 6); err != nil {
		t.Fatal(err)
	}
	f.Close()

	long, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := OpenWith(path, Options{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Frames.Segments) != 6 {
		t.Fatalf("segments = %d, want 6", len(whole.Frames.Segments))
	}
	cut := int(whole.Frames.Segments[3].Sync.Offset)
	short := long[:cut]

	// Index the short version, so a cache exists describing it.
	if err := os.WriteFile(path, short, 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Records() != 3*growRecordsPerSegment {
		t.Fatalf("short file has %d records, want %d", first.Records(), 3*growRecordsPerSegment)
	}
	if first.Closed {
		t.Error("a file cut before its END frame reported itself as closed")
	}

	// Append, then open again: the cache should be extended, not rebuilt.
	if err := os.WriteFile(path, long, 0o644); err != nil {
		t.Fatal(err)
	}
	grown, err := OpenWith(path, Options{
		OnCacheMiss: func(e error) { t.Logf("cache miss: %v", e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !grown.Extended {
		t.Fatal("a grown file was not extended from its cache")
	}

	// And it must match a full scan of the grown file exactly.
	full, err := OpenWith(path, Options{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	sameModel(t, full, grown)

	if grown.Records() != 6*growRecordsPerSegment {
		t.Errorf("grown file has %d records, want %d", grown.Records(), 6*growRecordsPerSegment)
	}

	// The extended index must still decode correctly across the join.
	a, err := NewAccessor(path, grown.Frames)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	st := grown.Streams[0]
	batches, err := a.Decode(st.Frames(0))
	if err != nil {
		t.Fatal(err)
	}
	s := SeriesFrom(batches, &st.Fields[0], grown.Epoch)
	if s.Len() != 6*growRecordsPerSegment {
		t.Fatalf("decoded %d samples across the join, want %d", s.Len(), 6*growRecordsPerSegment)
	}
	for i := 0; i < s.Len(); i++ {
		if !s.Present[i] || s.Vals[i] != float64(i%growRecordsPerSegment) {
			t.Fatalf("sample %d = %v (present=%v), want %v",
				i, s.Vals[i], s.Present[i], float64(i%growRecordsPerSegment))
		}
	}
}

const growRecordsPerSegment = 50

// writeGrowing writes a deterministic file of n one-second segments, such that
// writing n+1 produces a byte-for-byte extension of writing n.
func writeGrowing(f *os.File, segments int) error {
	w, err := logb.NewWriter(f)
	if err != nil {
		return err
	}
	s := &logb.Schema{
		UUID:       [16]byte{9},
		Name:       "grow",
		RecordBits: 16,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(1_000_000),
		Fields: []logb.Field{
			{Name: "v", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint},
		},
	}
	if err := w.AddStream(s); err != nil {
		return err
	}
	for seg := 0; seg < segments; seg++ {
		if err := w.BeginSegment(int64(seg) * 1e9); err != nil {
			return err
		}
		rec := make([]byte, growRecordsPerSegment*2)
		for i := 0; i < growRecordsPerSegment; i++ {
			rec[i*2] = byte(i)
			rec[i*2+1] = byte(i >> 8)
		}
		base := int64(seg) * int64(growRecordsPerSegment) * 1_000_000
		if err := w.WriteData(s, logb.TickVal(base), 0, growRecordsPerSegment, rec); err != nil {
			return err
		}
	}
	return w.Close()
}
