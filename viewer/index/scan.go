package index

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/rveen/logb"
)

// Options control how a file is opened.
type Options struct {
	// NoCache skips reading and writing the sidecar index.
	NoCache bool
	// Progress, if set, is called during a scan with bytes consumed so far.
	// It fires often; a caller that wants to report to a human should rate
	// limit it themselves.
	Progress func(done, total int64)
	// OnCacheMiss, if set, is told why the sidecar was not used. Every reason
	// is recoverable — the open falls back to scanning — but a silent fallback
	// is indistinguishable from a cache that is quietly never working.
	OnCacheMiss func(error)
}

// Open indexes the Logb file at path, using a cached index when one is valid.
func Open(path string) (*File, error) { return OpenWith(path, Options{}) }

// OpenWith indexes the Logb file at path.
//
// The cache is only ever an accelerator. Every failure to read, validate or
// write it falls back to scanning, because a viewer that refuses to open a file
// over a stale cache would be worse than a slow one — and because SPEC §9's
// rule about the format's own index applies just as well to ours: it must be
// rebuildable by scanning, and never trusted over the frames.
func OpenWith(path string, o Options) (*File, error) {
	size, err := fileSize(path)
	if err != nil {
		return nil, err
	}

	if !o.NoCache {
		fi, err := openCached(path, size)
		if err == nil {
			return fi, nil
		}
		if o.OnCacheMiss != nil {
			o.OnCacheMiss(err)
		}
	}

	start := time.Now()
	fi, err := scanFile(path, size, 0, o.Progress)
	if err != nil {
		return nil, err
	}
	fi.Cached = false
	if !o.NoCache {
		// Best effort: a read-only directory with no usable cache dir is a
		// normal situation, not a failure.
		_ = SaveSidecar(fi, time.Since(start))
	}
	return fi, nil
}

// openCached tries to satisfy an open from the sidecar, extending it if the
// file has grown since.
func openCached(path string, size int64) (*File, error) {
	sc, err := LoadSidecar(path)
	if err != nil {
		return nil, err
	}
	grown := size - sc.SourceSize
	if grown < 0 {
		return nil, ErrSidecarStale
	}

	fi, err := sc.restore(path, size)
	if err != nil {
		return nil, fmt.Errorf("restoring cached index: %w", err)
	}
	if grown == 0 {
		fi.Cached = true
		return fi, nil
	}

	// The file grew. Nothing in a Logb file points forward and every segment
	// restates its schemas, so the indexed prefix stays valid; only the tail
	// needs reading. Resume from the start of the last cached segment, since a
	// segment may have been mid-write when the cache was taken.
	resume, ok := resumePoint(fi)
	if !ok {
		return nil, errors.New("no segment boundary to resume from")
	}
	tail, err := scanFile(path, size, resume, nil)
	if err != nil {
		return nil, fmt.Errorf("scanning the new tail from %d: %w", resume, err)
	}
	merged, err := mergeTail(fi, tail, resume)
	if err != nil {
		return nil, fmt.Errorf("merging the new tail: %w", err)
	}
	merged.Cached = true
	merged.Extended = true
	_ = SaveSidecar(merged, 0)
	return merged, nil
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// scanFile scans path from a byte offset that must be either 0 or the start of
// a SYNC frame.
//
// Resuming works because a reader only needs a file header and a segment: the
// header is replayed in front of the tail, exactly as Accessor does for a
// range. Frame offsets reported by the scan are relative to that synthesized
// stream, so they are remapped back to real file offsets.
func scanFile(path string, size, from int64, progress func(done, total int64)) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if from == 0 {
		return scanReader(bufio.NewReaderSize(f, 1<<20), path, size, 0, progress)
	}

	header := make([]byte, fileHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, err
	}
	tail := io.NewSectionReader(f, from, size-from)
	r := io.MultiReader(bytes.NewReader(header), tail)
	// The synthesized stream puts the tail at offset fileHeaderSize, so a
	// reported offset x corresponds to file offset x - fileHeaderSize + from.
	return scanReader(bufio.NewReaderSize(r, 1<<20), path, size, from-fileHeaderSize, progress)
}

// Scan reads r to the end and builds the file model.
//
// One pass, forward only, which is the only kind of pass the format supports:
// nothing in a Logb file points forward, so there is no footer to seek to and
// no shortcut to take. The INDEX frame exists but SPEC §9 makes it purely an
// accelerator that a reader must be able to rebuild by scanning and must not
// trust over the frames, so this ignores it exactly as the core reader does.
func Scan(r io.Reader, path string, size int64) (*File, error) {
	return scanReader(r, path, size, 0, nil)
}

// scanReader is Scan with an offset bias for resumed scans and an optional
// progress callback.
func scanReader(r io.Reader, path string, size int64, offsetBias int64, progress func(done, total int64)) (*File, error) {
	rd, err := logb.NewReader(r)
	if err != nil {
		return nil, err
	}

	fi := &File{Path: path, Size: size}
	byUUID := map[[16]byte]*Stream{}
	unsupported := map[string]bool{}

	// The Tier 0 frame index is built from the same pass. Frame offsets come
	// from OnFrame, so this works over a plain io.Reader; turning those offsets
	// into random access additionally needs an io.ReaderAt, which is what
	// NewAccessor wants.
	idx := &builder{offsetBias: uint64(offsetBias)}
	rd.OnFrame = func(fr logb.Frame) {
		idx.onFrame(fr)
		if progress != nil {
			progress(int64(fr.Offset)+offsetBias+int64(fr.Size()), size)
		}
	}
	rd.OnSchema = func(s *logb.Schema, id uint16) {
		idx.onSchema(s, id)
		// A stream can be declared and never write a record. It is part of what
		// the file says, so it belongs in the tree even with no series behind it.
		if byUUID[s.UUID] == nil {
			st := newStream(s)
			byUUID[s.UUID] = st
			fi.Streams = append(fi.Streams, st)
		}
	}

	// Absolute ticks are accumulated during the scan and rebased at the end.
	// The epoch cannot be known before the first sample, and the earliest
	// sample need not be in the first stream.
	epoch := int64(math.MaxInt64)
	haveEpoch := false

	for {
		b, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		idx.onBatch(b)

		st := byUUID[b.Schema.UUID]
		if st == nil {
			st = newStream(b.Schema)
			byUUID[b.Schema.UUID] = st
			fi.Streams = append(fi.Streams, st)
		}
		st.noteRun(b.RunID, b.Run)
		st.Records += int(b.Count)

		// Tier 1: reduce every field of the frame while the batch is hot. The
		// decompression is already paid for; the samples themselves are not
		// kept, because keeping them is what makes a large file impossible.
		st.stats = append(st.stats, statsFor(b, st.Fields))

		if b.Schema.AxisKind == logb.AxisTime {
			if first, err := b.Axis(0); err == nil && int(b.Count) > 0 {
				if t := first.Ticks(); t < epoch {
					epoch, haveEpoch = t, true
				}
			}
		}
	}

	// Metadata and attachments are only trustworthy after a whole-file pass.
	// time.anchor in particular is emitted after the records it dates (SPEC
	// §5.2), so a reader over a range would legitimately miss it.
	fi.Meta = rd.Meta
	for name, data := range rd.Attachments {
		fi.Attachments = append(fi.Attachments, Attachment{Name: name, Size: len(data), Data: data})
	}
	sort.Slice(fi.Attachments, func(i, j int) bool { return fi.Attachments[i].Name < fi.Attachments[j].Name })

	fi.Truncated = rd.Truncated
	fi.Closed = rd.Closed
	for _, u := range rd.Unsupported {
		noteUnsupported(unsupported, u.Error())
	}
	for u := range unsupported {
		fi.Unsupported = append(fi.Unsupported, u)
	}
	sort.Strings(fi.Unsupported)

	if haveEpoch {
		fi.Epoch, fi.HasEpoch = epoch, true
		idx.idx.rebaseFrames(epoch)
	}
	idx.idx.sortSegmentRuns()
	fi.Frames = &idx.idx

	// Attach each stream's frames, after rebasing so the two never disagree.
	// Frames arrive in batch order and stats were appended in that same order,
	// so grouping the frame list by UUID keeps the two parallel.
	for _, d := range idx.idx.Data {
		if st := byUUID[d.UUID]; st != nil {
			st.FrameList = append(st.FrameList, d)
		}
	}
	for _, st := range fi.Streams {
		if len(st.FrameList) != len(st.stats) {
			// Only reachable if frame collection and statistics ever fall out
			// of step, which would silently misattribute every summary.
			return nil, fmt.Errorf("index: stream %q has %d frames but %d stat rows",
				st.Name, len(st.FrameList), len(st.stats))
		}
		st.span()
	}
	sortFile(fi)
	return fi, nil
}

// sortFile puts a file model into a stable presentation order.
func sortFile(fi *File) {
	sort.Slice(fi.Streams, func(i, j int) bool { return fi.Streams[i].Name < fi.Streams[j].Name })
	for _, s := range fi.Streams {
		sort.Slice(s.Runs, func(i, j int) bool { return s.Runs[i].ID < s.Runs[j].ID })
	}
	sort.Slice(fi.Attachments, func(i, j int) bool { return fi.Attachments[i].Name < fi.Attachments[j].Name })
}

func metaOf(m MetaKV) logb.Meta { return logb.Meta{Key: m.Key, Value: m.Value} }

// sample decodes one field of one record.
//
// The absent case is the important one. A guarded field whose guard does not
// hold returns ErrFieldAbsent, and SPEC §6.2 is explicit that this means the
// field is not in the record — not that it is zero. Returning NaN with
// present=false makes it impossible to accidentally plot it as a value.
func sample(b *logb.Batch, i int, fd *Field) (float64, bool) {
	var v any
	var err error
	if fd.Class == ClassCategorical {
		// Categorical fields keep their raw value: the enumeration text is
		// looked up for display, but bucketing and equality must happen on the
		// raw bits, the same way guards compare raw bits.
		v, err = b.Raw(i, fd.Index)
	} else {
		v, err = b.Value(i, fd.Index)
	}
	if err != nil {
		return math.NaN(), false
	}
	f, ok := asFloat(v)
	if !ok || !finite(f) {
		return math.NaN(), false
	}
	return f, true
}

// present reports whether a field was in a record, without decoding a value.
//
// For an event field there is no number to extract, and the only question worth
// asking is the one a guard answers: is this field in this record at all (SPEC
// §6.2). ErrFieldAbsent is data, not an error.
func present(b *logb.Batch, i int, fd *Field) bool {
	_, err := b.Raw(i, fd.Index)
	return err == nil
}

func newStream(s *logb.Schema) *Stream {
	st := &Stream{
		UUID:     hex.EncodeToString(s.UUID[:]),
		Name:     s.Name,
		AxisKind: axisKindName(s.AxisKind),
		AxisMode: axisModeName(s.AxisMode),
		AxisExp:  s.AxisExp,
		AxisUnit: s.AxisUnit,
		Meta:     s.Meta,
	}
	explicit := s.AxisMode == logb.AxisExplicit
	for i := range s.Fields {
		fd := &s.Fields[i]
		st.Fields = append(st.Fields, Field{
			Index:   i,
			Name:    fd.Name,
			Unit:    fd.Unit,
			Desc:    fd.Desc,
			Type:    fd.Type.String(),
			Class:   classify(fd),
			Guarded: fd.Guarded,
			// The field carrying the independent variable is not a signal in
			// its own right. Deriving this from the schema is what lets the
			// viewer avoid cmd/logbdump's hardcoded `fd.Name == "t_us"` check.
			IsAxis:    explicit && uint16(i) == s.AxisField,
			BitOffset: fd.BitOffset,
			BitWidth:  fd.BitWidth,
			BigEndian: fd.BigEndian,
			Variable:  fd.Variable,
			Conv:      convDesc(fd.Conv),
			Meta:      fd.Meta,
			conv:      fd.Conv,
		})
	}
	return st
}

func (s *Stream) noteRun(id uint32, r *logb.Run) {
	for _, existing := range s.Runs {
		if existing.ID == id {
			// A RUN frame may arrive in a later segment than the first DATA
			// frame that references the run, so fill in params if we now have
			// them and did not before.
			if r != nil && len(existing.Params) == 0 {
				existing.Index, existing.Params = r.Index, r.Params
			}
			return
		}
	}
	run := &Run{ID: id}
	if r != nil {
		run.Index, run.Params = r.Index, r.Params
	}
	s.Runs = append(s.Runs, run)
}

// span records the extent of the stream's axis from its frames.
//
// Taken from the frame index rather than from decoded samples, because the
// samples are no longer kept. Every DATA frame carries its own axis_base and
// its records' extent was measured during the scan, so this costs nothing.
func (s *Stream) span() {
	first := true
	for _, f := range s.FrameList {
		if f.Count == 0 {
			continue
		}
		lo, hi := f.First(), f.Last()
		if first {
			s.AxisMin, s.AxisMax, first = lo, hi, false
			continue
		}
		s.AxisMin = math.Min(s.AxisMin, lo)
		s.AxisMax = math.Max(s.AxisMax, hi)
	}
	s.HasSpan = !first
}

func noteUnsupported(seen map[string]bool, msg string) {
	// One line per distinct cause, not one per record: a stream that cannot be
	// decoded would otherwise produce millions of identical complaints.
	seen[msg] = true
}
