package query

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/viewer/index"
)

// scanRecords reads a stream's records with a plain sequential logb.Reader —
// no index, no random access, no frame skipping, no paging.
//
// It shares RecordAt with the code under test, so it is not an independent
// check of how a value is decoded; that is Batch.Value's job and the core's
// tests cover it. What it does check independently is everything this package
// adds: which records the window contains, in what order, and that skipping
// frames by their declared counts lands in the same place a full read does.
type ref struct {
	axis float64
	run  uint32
	text []string
}

func scanRecords(t *testing.T, path, stream string, st *index.Stream, epoch int64) []ref {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r, err := logb.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []ref
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Schema.Name != stream {
			continue
		}
		for i := 0; i < int(b.Count); i++ {
			rec, ok := index.RecordAt(b, i, st, epoch)
			if !ok {
				continue
			}
			row := ref{axis: rec.Axis, run: rec.Run}
			for _, c := range rec.Cells {
				if !c.Present {
					row.text = append(row.text, "\x00absent")
					continue
				}
				row.text = append(row.text, c.Text)
			}
			out = append(out, row)
		}
	}
	return out
}

// page walks the whole window through the paging API, one page at a time, and
// flattens it the same way scanRecords does.
func page(t *testing.T, q *Query, st *index.Stream, run *uint32, from, to float64, size int) []ref {
	t.Helper()
	var out []ref
	for off := 0; ; off += size {
		p, err := q.Records(st, run, from, to, off, size)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range p.Records {
			row := ref{axis: rec.Axis, run: rec.Run}
			for _, c := range rec.Cells {
				if !c.Present {
					row.text = append(row.text, "\x00absent")
					continue
				}
				row.text = append(row.text, c.Text)
			}
			out = append(out, row)
		}
		if !p.More {
			return out
		}
		if off > 1<<20 {
			t.Fatal("paging did not terminate")
		}
	}
}

func same(t *testing.T, got, want []ref, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d records, sequential scan found %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i].axis != want[i].axis || got[i].run != want[i].run {
			t.Fatalf("%s: record %d at axis %v run %d, want axis %v run %d",
				what, i, got[i].axis, got[i].run, want[i].axis, want[i].run)
		}
		for k := range want[i].text {
			if got[i].text[k] != want[i].text[k] {
				t.Fatalf("%s: record %d field %d = %q, want %q",
					what, i, k, got[i].text[k], want[i].text[k])
			}
		}
	}
}

// TestRecordsMatchSequentialScan is the central correctness test for the table.
//
// The paging path skips whole frames without decoding them, decodes only the
// edges, and stitches pages together. None of that may change a single value,
// and the only way to know is to compare against the reader doing it the slow
// obvious way.
func TestRecordsMatchSequentialScan(t *testing.T) {
	q := newTestQuery(t)
	for _, name := range []string{"EngineData", "VehicleStatus", "can0.raw", "events"} {
		t.Run(name, func(t *testing.T) {
			st := stream(t, q, name)
			want := scanRecords(t, testFile, name, st, q.File.Epoch)
			if len(want) == 0 {
				t.Fatal("reference scan found no records")
			}
			// Several page sizes, because the interesting bugs live at the seam
			// between a page and the frame it started in the middle of.
			for _, size := range []int{1, 7, 50, 100, 1000} {
				got := page(t, q, st, nil, st.AxisMin, st.AxisMax, size)
				same(t, got, want, name)
			}
		})
	}
}

// TestRecordsWindowExcludesOutside checks the window is applied to records, not
// just to frames. Frames are the unit of decode and spill past the window; the
// records they carry must still be filtered.
func TestRecordsWindowExcludesOutside(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")
	all := scanRecords(t, testFile, "EngineData", st, q.File.Epoch)

	// A window covering the middle third, in axis units.
	span := st.AxisMax - st.AxisMin
	from := st.AxisMin + span/3
	to := st.AxisMin + 2*span/3

	var want []ref
	for _, r := range all {
		if r.axis >= from && r.axis <= to {
			want = append(want, r)
		}
	}
	if len(want) == 0 || len(want) == len(all) {
		t.Fatalf("bad test window: %d of %d records", len(want), len(all))
	}
	same(t, page(t, q, st, nil, from, to, 64), want, "windowed")
}

// TestRecordsSkipDoesNotDecodeWholeFrames is the property that makes a deep
// offset affordable. A frame lying wholly inside the window holds exactly the
// record count it declares, so paging past it must cost no decompression at
// all — only the partial frames at the edges.
func TestRecordsSkipDoesNotDecodeWholeFrames(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")

	total := 0
	for _, f := range st.FrameList {
		total += int(f.Count)
	}
	if len(st.FrameList) < 3 {
		t.Skipf("need at least 3 frames, have %d", len(st.FrameList))
	}

	// Land in the last frame. Every frame before it is wholly inside the
	// full-file window, so none of them may be touched.
	skip := total - int(st.FrameList[len(st.FrameList)-1].Count)
	p, err := q.Records(st, nil, st.AxisMin, st.AxisMax, skip, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 10 {
		t.Fatalf("got %d records, want 10", len(p.Records))
	}
	if p.Decoded != 1 {
		t.Errorf("decoded %d frames to reach offset %d; only the frame holding it should have been touched", p.Decoded, skip)
	}
	if !p.TotalExact || p.Total != total {
		t.Errorf("total %d (exact %v), want exactly %d", p.Total, p.TotalExact, total)
	}
}

// TestRecordsAbsentIsNotZero is the guard rule at the table level. A guarded
// field whose guard does not hold is not in the record (SPEC §6.2); it must
// come back absent, never as the number zero.
func TestRecordsAbsentIsNotZero(t *testing.T) {
	path := writeGuardedFile(t)

	idx, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := index.NewAccessor(path, idx.Frames)
	if err != nil {
		t.Fatal(err)
	}
	defer acc.Close()
	q := New(idx, acc)

	st := idx.Streams[0]
	boost := -1
	for i := range st.Fields {
		if st.Fields[i].Name == "boost" {
			boost = i
		}
	}
	if boost < 0 {
		t.Fatal("fixture has no boost field")
	}

	p, err := q.Records(st, nil, st.AxisMin, st.AxisMax, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	absent, present := 0, 0
	for _, rec := range p.Records {
		c := rec.Cells[boost]
		if !c.Present {
			absent++
			if c.Num != nil || c.Text != "" {
				t.Fatalf("absent cell carries a value: num=%v text=%q", c.Num, c.Text)
			}
			continue
		}
		present++
	}
	if absent == 0 {
		t.Fatal("no absent boost samples; the guard is not being honoured")
	}
	if present == 0 {
		t.Fatal("no present boost samples; the guard is rejecting everything")
	}
}

// writeGuardedFile builds a small file whose `boost` field is present in only
// some records, mirroring the awkward feature of the big fixture: a guard that
// compares raw bits, and an unsatisfied guard meaning absent rather than zero.
func writeGuardedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guarded.logb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w, err := logb.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	s := &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("logb/viewer/test/guarded")),
		Name:       "powertrain",
		RecordBits: 40,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(1_000_000),
		Fields: []logb.Field{
			{Name: "mode", BitOffset: 0, BitWidth: 8, Type: logb.TypeUint,
				Conv: logb.ValueToText{Keys: []float64{0, 2}, Texts: []string{"idle", "boost"}, Default: "?"}},
			{Name: "boost", BitOffset: 8, BitWidth: 16, Type: logb.TypeUint, Unit: "kPa",
				Conv: logb.Linear{B: 0.1}, Guarded: true, GuardField: 0, GuardValue: 2},
			{Name: "rpm", BitOffset: 24, BitWidth: 16, Type: logb.TypeUint, Unit: "1/min"},
		},
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}

	// Alternating blocks of ten, so both branches of the guard land inside
	// every frame rather than splitting neatly along frame boundaries.
	const n = 200
	rec := make([]byte, 5*n)
	for i := 0; i < n; i++ {
		b := rec[i*5:]
		if (i/10)%2 == 1 {
			b[0] = 2
			binary.LittleEndian.PutUint16(b[1:], uint16(100+i))
		}
		binary.LittleEndian.PutUint16(b[3:], uint16(800+i))
	}
	if err := w.WriteData(s, logb.TickVal(0), 0, n, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRecordsPerRunFilter checks that asking for one run gets one run, and that
// asking for none gets them all.
func TestRecordsPerRunFilter(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")

	all, err := q.Records(st, nil, st.AxisMin, st.AxisMax, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	one, err := q.Records(st, &zero, st.AxisMin, st.AxisMax, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range one.Records {
		if rec.Run != 0 {
			t.Fatalf("run filter let through run %d", rec.Run)
		}
	}
	if len(one.Records) > len(all.Records) {
		t.Fatalf("run 0 has %d records, more than all runs' %d", len(one.Records), len(all.Records))
	}
}
