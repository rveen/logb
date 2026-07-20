package index

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
)

func accessor(t *testing.T) (*File, *Accessor) {
	t.Helper()
	fi, err := Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAccessor(testFile, fi.Frames)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return fi, a
}

func uuidBytes(t *testing.T, s *Stream) [16]byte {
	t.Helper()
	b, err := hex.DecodeString(s.UUID)
	if err != nil || len(b) != 16 {
		t.Fatalf("bad uuid %q", s.UUID)
	}
	var u [16]byte
	copy(u[:], b)
	return u
}

func TestFrameIndexShape(t *testing.T) {
	fi, _ := accessor(t)
	x := fi.Frames

	// The example is written as three one-second segments.
	if len(x.Segments) != 3 {
		t.Errorf("segments = %d, want 3", len(x.Segments))
	}
	for _, seg := range x.Segments {
		if seg.Sync.Type != logb.FrameSync {
			t.Errorf("segment %d has no SYNC frame", seg.Index)
		}
		// Every segment restates every schema — that is what makes a file
		// decodable from any cut (rule 3).
		if len(seg.Schemas) != 4 {
			t.Errorf("segment %d declares %d schemas, want 4", seg.Index, len(seg.Schemas))
		}
	}

	// 4 streams x 3 segments, one DATA frame each in this file.
	if len(x.Data) != 12 {
		t.Errorf("data frames = %d, want 12", len(x.Data))
	}

	// Frame record counts must agree with what the scan actually decoded.
	for _, st := range fi.Streams {
		if got, want := x.Records(uuidBytes(t, st)), st.Records; got != want {
			t.Errorf("%s: index counts %d records, scan decoded %d", st.Name, got, want)
		}
	}
}

// TestFramesAreContiguousAndOrdered checks the offsets the whole design rests
// on: every frame must sit inside the file and none may overlap another.
func TestFramesAreContiguousAndOrdered(t *testing.T) {
	fi, _ := accessor(t)
	prev := uint64(fileHeaderSize)
	for i, d := range fi.Frames.Data {
		if d.Offset < prev {
			t.Fatalf("frame %d at %d overlaps something ending at %d", i, d.Offset, prev)
		}
		if d.End() > uint64(fi.Size) {
			t.Fatalf("frame %d ends at %d, past the %d-byte file", i, d.End(), fi.Size)
		}
		prev = d.End()
	}
}

// referenceSeries decodes a field by walking the whole file with the core
// reader and nothing else — no index, no accessor, no prefixes.
//
// This is the oracle. Comparing random access against our own scan would only
// prove the two agree with each other; comparing against a plain sequential
// logb.Reader proves they agree with the format.
func referenceSeries(t *testing.T, st *Stream, fd *Field, run uint32, epoch int64) *Series {
	t.Helper()
	f, err := os.Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r, err := logb.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var batches []*logb.Batch
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Schema.Name == st.Name && b.RunID == run {
			batches = append(batches, b)
		}
	}
	return SeriesFrom(batches, fd, epoch)
}

// TestRangeDecodeEqualsFullScan is the central correctness test.
//
// For every field of every stream, values obtained by replaying a synthesized
// prefix in front of individual DATA frames must equal what a plain sequential
// pass with the core reader produces — same axis positions, same values, same
// presence. If these ever disagree, the viewer is showing something the file
// does not say.
func TestRangeDecodeEqualsFullScan(t *testing.T) {
	fi, a := accessor(t)

	for _, st := range fi.Streams {
		for fx := range st.Fields {
			fd := &st.Fields[fx]
			if fd.IsAxis || fd.Class == ClassBlob {
				continue
			}
			want := referenceSeries(t, st, fd, 0, fi.Epoch)
			got := fullSeries(t, fi, a, st, fd, 0)

			if got.Len() != want.Len() {
				t.Fatalf("%s.%s: random access produced %d samples, the core reader %d",
					st.Name, fd.Name, got.Len(), want.Len())
			}
			for i := 0; i < want.Len(); i++ {
				if got.Axis.At(i) != want.Axis.At(i) {
					t.Fatalf("%s.%s[%d]: axis %v via random access, %v via the core reader",
						st.Name, fd.Name, i, got.Axis.At(i), want.Axis.At(i))
				}
				if got.Present[i] != want.Present[i] {
					t.Fatalf("%s.%s[%d]: presence %v via random access, %v via the core reader",
						st.Name, fd.Name, i, got.Present[i], want.Present[i])
				}
				if got.Present[i] && got.Vals[i] != want.Vals[i] {
					t.Fatalf("%s.%s[%d]: value %v via random access, %v via the core reader",
						st.Name, fd.Name, i, got.Vals[i], want.Vals[i])
				}
				if !got.Present[i] && !math.IsNaN(got.Vals[i]) {
					t.Fatalf("%s.%s[%d]: absent sample carries %v", st.Name, fd.Name, i, got.Vals[i])
				}
			}
		}
	}
}

// TestSelectWindowsFrames checks that a time range picks exactly the frames
// overlapping it — the whole point of having an index.
func TestSelectWindowsFrames(t *testing.T) {
	fi, _ := accessor(t)
	st := streamNamed(t, fi, "EngineData")
	uuid := uuidBytes(t, st)

	all := fi.Frames.All(uuid)
	if len(all) != 3 {
		t.Fatalf("EngineData has %d frames, want 3", len(all))
	}

	// Each segment covers one second. A window inside the second segment must
	// select that frame alone.
	got := fi.Frames.Select(uuid, 1.2e9, 1.5e9)
	if len(got) != 1 || got[0].Segment != 1 {
		t.Errorf("mid-segment-1 window selected %d frames %v, want just segment 1", len(got), segmentsOf(got))
	}

	// A window straddling a segment boundary must select both.
	got = fi.Frames.Select(uuid, 0.9e9, 1.1e9)
	if len(got) != 2 {
		t.Errorf("straddling window selected %d frames %v, want 2", len(got), segmentsOf(got))
	}

	// A window past the end selects nothing, and decoding nothing is not an
	// error — it is an empty chart.
	got = fi.Frames.Select(uuid, 10e9, 11e9)
	if len(got) != 0 {
		t.Errorf("window past the end selected %d frames", len(got))
	}
}

// TestRangeRespectsWindow checks Accessor.Range end to end: the records it
// returns must cover the requested window and come from the right segments.
func TestRangeRespectsWindow(t *testing.T) {
	fi, a := accessor(t)
	st := streamNamed(t, fi, "EngineData")
	uuid := uuidBytes(t, st)

	batches, err := a.Range(uuid, 1.2e9, 1.5e9)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(batches))
	}
	// One frame of 100 records at 10 ms, based at 1 s.
	if batches[0].Count != 100 {
		t.Errorf("count = %d, want 100", batches[0].Count)
	}
	first, _ := batches[0].Axis(0)
	if got := first.Ticks() - fi.Epoch; got != 1_000_000_000 {
		t.Errorf("first tick = %d, want 1000000000", got)
	}
}

// TestPrefixIsCachedPerSegmentAndStream checks that the prefix cache keys on
// both parts. stream_id is segment-scoped, so caching on the id alone would
// serve one segment's schema for another segment's frames — which decodes
// without error and produces wrong values.
func TestPrefixIsCachedPerSegmentAndStream(t *testing.T) {
	fi, a := accessor(t)

	seen := map[[16]byte]bool{}
	for _, seg := range fi.Frames.Segments {
		for id, uuid := range seg.UUIDs {
			p, err := a.prefixFor(seg.Index, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(p) <= fileHeaderSize {
				t.Errorf("segment %d id %d: prefix is %d bytes", seg.Index, id, len(p))
			}
			if !bytes.HasPrefix(p, a.header) {
				t.Errorf("segment %d id %d: prefix does not open with the file header", seg.Index, id)
			}
			seen[uuid] = true
		}
	}
	if len(seen) != 4 {
		t.Errorf("prefixes cover %d streams, want 4", len(seen))
	}
	if len(a.prefix) != 12 {
		t.Errorf("cached %d prefixes, want 12 (4 streams x 3 segments)", len(a.prefix))
	}
}

// TestSyncMustPrecedeSchema pins the ordering rule that makes prefixes work.
//
// The reader's SYNC handler clears its schema map, so a prefix that replayed
// the schema first would have it erased, and the DATA frames would then be
// skipped as belonging to an unbound id — silently, with no error and no
// records.
func TestSyncMustPrecedeSchema(t *testing.T) {
	fi, a := accessor(t)
	st := streamNamed(t, fi, "EngineData")
	frame := fi.Frames.All(uuidBytes(t, st))[0]
	seg := fi.Frames.Segments[frame.Segment]

	read := func(ref FrameRef) []byte {
		b := make([]byte, ref.Size())
		if _, err := a.ra.ReadAt(b, int64(ref.Offset)); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// The correct order produces the batch. Doing this first also populates the
	// lazily-read file header that the comparison below needs.
	got, err := a.Decode([]DataFrame{frame})
	if err != nil || len(got) != 1 {
		t.Fatalf("correct order: %d batches, err %v", len(got), err)
	}

	var wrong bytes.Buffer
	wrong.Write(a.header)
	wrong.Write(read(seg.Schemas[frame.StreamID])) // schema first — the mistake
	wrong.Write(read(seg.Sync))
	wrong.Write(read(frame.FrameRef))

	r, err := logb.NewReader(bytes.NewReader(wrong.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, err := r.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 0 {
		t.Fatalf("schema-before-sync yielded %d batches; the ordering rule is not what we think", n)
	}
}

// TestConcatenatedFile checks per-segment stream_id rebinding, which is the
// thing UUID-keyed identity exists to survive (SPEC §6.6). Two copies of the
// file joined byte-wise must index as six segments and twice the records.
func TestConcatenatedFile(t *testing.T) {
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	// Byte concatenation, minus the second copy's 16-byte file header.
	joined := append(append([]byte{}, data...), data[fileHeaderSize:]...)

	fi, err := Scan(bytes.NewReader(joined), "joined.logb", int64(len(joined)))
	if err != nil {
		t.Fatal(err)
	}
	if len(fi.Frames.Segments) != 6 {
		t.Errorf("segments = %d, want 6", len(fi.Frames.Segments))
	}
	if len(fi.Streams) != 4 {
		t.Errorf("streams = %d, want 4: identity is the UUID, not the stream id", len(fi.Streams))
	}

	single, err := Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range fi.Streams {
		one := streamNamed(t, single, st.Name)
		if st.Records != 2*one.Records {
			t.Errorf("%s: joined has %d records, want %d", st.Name, st.Records, 2*one.Records)
		}
	}

	// Random access must still work across the join.
	a := NewAccessorAt(bytes.NewReader(joined), fi.Frames)
	st := streamNamed(t, fi, "EngineData")
	batches, err := a.Decode(fi.Frames.All(uuidBytes(t, st)))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, b := range batches {
		total += int(b.Count)
	}
	if total != st.Records {
		t.Errorf("range decode across the join gave %d records, want %d", total, st.Records)
	}
}

// TestMixedStreamGroupRejected checks the guard against assembling frames from
// two streams into one synthesized stream. A prefix carries one schema, so the
// other stream's frames would be skipped as unbound — silently returning too
// few records rather than failing.
func TestMixedStreamGroupRejected(t *testing.T) {
	fi, a := accessor(t)
	engine := fi.Frames.All(uuidBytes(t, streamNamed(t, fi, "EngineData")))[0]
	vehicle := fi.Frames.All(uuidBytes(t, streamNamed(t, fi, "VehicleStatus")))[0]
	if engine.Segment != vehicle.Segment {
		t.Skip("frames are not in the same segment")
	}

	if _, err := a.Decode([]DataFrame{engine, vehicle}); err == nil {
		t.Error("decoding a mixed-stream group should fail loudly")
	}
}

func segmentsOf(frames []DataFrame) []int {
	out := make([]int, len(frames))
	for i, f := range frames {
		out[i] = f.Segment
	}
	return out
}

// TestSilentStreamAppears covers what the upstream OnSchema hook was added for.
//
// Before it, a stream reached a caller only attached to a batch, so one that
// was declared and never wrote a record was invisible. A channel configured but
// never triggered is an ordinary thing for a file to contain, and a viewer that
// silently omits it is lying about the file's contents.
func TestSilentStreamAppears(t *testing.T) {
	mk := func(name string, id byte) *logb.Schema {
		s := &logb.Schema{
			UUID:       [16]byte{id},
			Name:       name,
			RecordBits: 16,
			AxisKind:   logb.AxisTime,
			AxisMode:   logb.AxisImplicit,
			AxisExp:    -9,
			AxisUnit:   "s",
			AxisStep:   logb.TickVal(1_000_000),
			Fields: []logb.Field{
				{Name: "v", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint, Unit: "V"},
			},
		}
		return s
	}

	loud, silent := mk("loud", 1), mk("silent", 2)

	var out bytes.Buffer
	w, err := logb.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*logb.Schema{loud, silent} {
		if err := w.AddStream(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteData(loud, logb.TickVal(0), 0, 1, []byte{0x2a, 0x00}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fi, err := Scan(bytes.NewReader(out.Bytes()), "silent.logb", int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}

	if len(fi.Streams) != 2 {
		t.Fatalf("streams = %d, want 2: a declared stream counts even with no records", len(fi.Streams))
	}
	quiet := streamNamed(t, fi, "silent")
	if quiet.Records != 0 {
		t.Errorf("silent stream has %d records, want 0", quiet.Records)
	}
	if quiet.HasSpan {
		t.Error("silent stream reports an axis span it cannot have")
	}
	if n := len(quiet.FrameList); n != 0 {
		t.Errorf("silent stream has %d frames", n)
	}

	noisy := streamNamed(t, fi, "loud")
	if noisy.Records != 1 {
		t.Errorf("loud stream has %d records, want 1", noisy.Records)
	}
	a := NewAccessorAt(bytes.NewReader(out.Bytes()), fi.Frames)
	batches, err := a.Decode(noisy.FrameList)
	if err != nil {
		t.Fatal(err)
	}
	if got := SeriesFrom(batches, &noisy.Fields[0], fi.Epoch); got.Len() != 1 || got.Vals[0] != 42 {
		t.Errorf("loud stream series = %+v, want one sample of 42", got)
	}
}

// TestRangeDecodeOnTruncatedFile checks that random access survives rule 2.
//
// A file cut mid-write is a valid file containing every record up to the last
// intact frame, so the index built from it is simply shorter. Every frame it
// does contain must still decode: a partial index must be a correct index, not
// one that points at a frame the file no longer holds.
//
// Sampled rather than exhaustive — the full sweep lives in TestTruncationSweep;
// this one pays for a decode per cut.
func TestRangeDecodeOnTruncatedFile(t *testing.T) {
	data, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}

	for cut := fileHeaderSize; cut <= len(data); cut += 37 {
		trimmed := data[:cut]
		fi, err := Scan(bytes.NewReader(trimmed), testFile, int64(cut))
		if err != nil {
			t.Fatalf("cut=%d: %v", cut, err)
		}
		a := NewAccessorAt(bytes.NewReader(trimmed), fi.Frames)

		for _, st := range fi.Streams {
			uuid := uuidBytes(t, st)
			frames := fi.Frames.All(uuid)
			batches, err := a.Decode(frames)
			if err != nil {
				t.Fatalf("cut=%d %s: %v", cut, st.Name, err)
			}

			// Every indexed frame must come back, with the record count the
			// index claimed for it.
			if len(batches) != len(frames) {
				t.Fatalf("cut=%d %s: decoded %d of %d frames", cut, st.Name, len(batches), len(frames))
			}
			total := 0
			for i, b := range batches {
				if b.Count != frames[i].Count {
					t.Fatalf("cut=%d %s frame %d: count %d, index said %d",
						cut, st.Name, i, b.Count, frames[i].Count)
				}
				total += int(b.Count)
			}
			if total != st.Records {
				t.Fatalf("cut=%d %s: range decode gave %d records, scan gave %d",
					cut, st.Name, total, st.Records)
			}
		}
	}
}
