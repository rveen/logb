package index

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/rveen/logb"
)

// fileHeaderSize is the 16-byte header every Logb file opens with.
const fileHeaderSize = 16

// Accessor decodes arbitrary DATA frames without rescanning the file.
//
// The core reader is single-pass by design and has no seek API, and SPEC §9
// forbids trusting the file's own INDEX frame over the frames themselves — so
// rather than fork the reader, this replays what it needs.
//
// Rule 3 says a Logb file can be cut anywhere and still decode, because every
// segment restates its schemas. The corollary is that a segment's preamble
// (file header, SYNC, SCHEMA, RUN) placed in front of any of that segment's
// DATA frames is a byte stream the unmodified reader will decode correctly:
//
//	io.MultiReader(prefix, sectionReader(frame), sectionReader(frame), ...)
//
// Nothing in the decode path reaches forwards or backwards. NewReader reads
// exactly the 16-byte header, Next is sequential io.ReadFull, and a DATA
// frame's axis_base, record_count, run_id, codec and filter all live in its own
// payload (SPEC §8). This is not a workaround; it is the format's design being
// used as advertised.
//
// One thing the synthesized stream is *not* is a valid file. It carries only
// the DATA frames asked for, which can break the per-run contiguity §6.5
// requires. The reader does not care — contiguity is enforced only by the
// writer — but the bytes must never be handed to a user as an exported range
// without regrouping by run first.
type Accessor struct {
	ra    io.ReaderAt
	idx   *FrameIndex
	close func() error

	mu     sync.Mutex
	header []byte
	prefix map[prefixKey][]byte
}

// prefixKey is (segment, stream_id) because a prefix carries one segment's
// preamble and one stream's schema. stream_id is meaningful only inside its
// segment (SPEC §6.6), which is exactly why the segment is part of the key.
type prefixKey struct {
	segment  int
	streamID uint16
}

// NewAccessor opens path for random access against an already-built index.
// Close it when done.
func NewAccessor(path string, idx *FrameIndex) (*Accessor, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a := NewAccessorAt(f, idx)
	a.close = f.Close
	return a, nil
}

// NewAccessorAt builds an accessor over an already-open source.
func NewAccessorAt(ra io.ReaderAt, idx *FrameIndex) *Accessor {
	return &Accessor{ra: ra, idx: idx, prefix: map[prefixKey][]byte{}}
}

func (a *Accessor) Close() error {
	if a.close != nil {
		return a.close()
	}
	return nil
}

// Index returns the frame index this accessor reads against.
func (a *Accessor) Index() *FrameIndex { return a.idx }

// Decode returns the batches of the given DATA frames, in the order given.
//
// The frames need not be adjacent in the file: each is self-contained given its
// schema, so feeding them back to back is sound even when other streams' frames
// sat between them.
func (a *Accessor) Decode(frames []DataFrame) ([]*logb.Batch, error) {
	var out []*logb.Batch
	for _, group := range groupBySegment(frames) {
		batches, err := a.decodeGroup(group)
		if err != nil {
			return nil, err
		}
		out = append(out, batches...)
	}
	return out, nil
}

// Range decodes the frames of a stream overlapping [from, to].
//
// The result is frame-aligned, so it generally spills past the requested
// window: a frame is the smallest thing that can be decompressed. Callers that
// need exact bounds filter the records afterwards.
func (a *Accessor) Range(uuid [16]byte, from, to float64) ([]*logb.Batch, error) {
	return a.Decode(a.idx.Select(uuid, from, to))
}

// groupBySegment splits frames into runs sharing a segment, preserving order.
// The prefix depends on the segment, so a group is the unit a single
// synthesized stream can serve.
func groupBySegment(frames []DataFrame) [][]DataFrame {
	var out [][]DataFrame
	for _, f := range frames {
		if n := len(out); n > 0 && out[n-1][0].Segment == f.Segment {
			out[n-1] = append(out[n-1], f)
			continue
		}
		out = append(out, []DataFrame{f})
	}
	return out
}

func (a *Accessor) decodeGroup(group []DataFrame) ([]*logb.Batch, error) {
	seg := group[0].Segment
	id := group[0].StreamID
	for _, f := range group {
		// One prefix carries one stream's schema, so a group must be one
		// stream. Selecting by UUID guarantees this; a caller assembling
		// frames by hand could break it, and would get silently empty results
		// as the reader skipped DATA frames for an unbound id.
		if f.StreamID != id {
			return nil, fmt.Errorf("index: group mixes stream ids %d and %d in segment %d", id, f.StreamID, seg)
		}
	}

	prefix, err := a.prefixFor(seg, id)
	if err != nil {
		return nil, err
	}

	readers := make([]io.Reader, 0, len(group)+1)
	readers = append(readers, bytes.NewReader(prefix))
	for _, f := range group {
		readers = append(readers, io.NewSectionReader(a.ra, int64(f.Offset), int64(f.Size())))
	}

	r, err := logb.NewReader(io.MultiReader(readers...))
	if err != nil {
		return nil, fmt.Errorf("index: synthesized stream rejected: %w", err)
	}

	// r.Meta and r.Attachments are deliberately ignored. A range reader sees
	// only the frames it was given, and file metadata is not among them —
	// time.anchor in particular is emitted after the records it dates (SPEC
	// §5.2), so a range would legitimately miss it. Metadata comes from the
	// whole-file scan and nowhere else.
	var out []*logb.Batch
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}

	// Truncated here does not mean the file is damaged — it means we handed the
	// reader something that did not end on a frame boundary, which can only be
	// our own index being wrong. It must be loud, never a silently short chart.
	if r.Truncated {
		return nil, fmt.Errorf("index: segment %d stream %d: synthesized stream truncated; the frame index is wrong", seg, id)
	}
	if len(out) != len(group) {
		return nil, fmt.Errorf("index: segment %d stream %d: decoded %d frames, expected %d", seg, id, len(out), len(group))
	}
	return out, nil
}

// prefixFor builds and caches the preamble for one stream of one segment.
//
// Order is load-bearing: the reader's SYNC handler *clears* its schema and run
// maps, because a sync frame rebinds every id (SPEC §6.6). A SCHEMA frame
// placed before the SYNC would be erased, and its DATA frames would then hit
// the unbound-id path and be skipped without an error.
func (a *Accessor) prefixFor(segment int, streamID uint16) ([]byte, error) {
	if err := a.ensureHeader(); err != nil {
		return nil, err
	}

	key := prefixKey{segment, streamID}

	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.prefix[key]; ok {
		return p, nil
	}

	if segment < 0 || segment >= len(a.idx.Segments) {
		return nil, fmt.Errorf("index: no segment %d", segment)
	}
	seg := a.idx.Segments[segment]

	schema, ok := seg.Schemas[streamID]
	if !ok {
		return nil, fmt.Errorf("index: segment %d has no schema for stream id %d", segment, streamID)
	}

	var buf bytes.Buffer
	buf.Write(a.header)

	// The SYNC frame is not strictly required — a fresh reader already starts
	// with empty maps, and the handler only records the sequence number. It is
	// replayed anyway so the synthesized stream is a well-formed segment, which
	// keeps it inspectable with cmd/logbdump when something goes wrong.
	if seg.Sync.Type == logb.FrameSync {
		if err := a.appendFrame(&buf, seg.Sync); err != nil {
			return nil, err
		}
	}
	if err := a.appendFrame(&buf, schema); err != nil {
		return nil, err
	}
	// Every RUN frame of the segment, not just the ones the caller will touch:
	// they are small, and a batch whose run is unbound comes back with a nil
	// Run, losing the parameters that distinguish a stepped sweep.
	for _, run := range seg.Runs {
		if err := a.appendFrame(&buf, run); err != nil {
			return nil, err
		}
	}

	p := buf.Bytes()
	a.prefix[key] = p
	return p, nil
}

// Schemas decodes a segment's SCHEMA frames without decoding any records.
//
// This is what lets a cached index be usable: the sidecar stores where every
// frame is and what each holds, but not the schemas themselves — conversions
// are an interface the core keeps closed, and re-deriving them from a cache
// would mean trusting a copy over the file. Replaying the segment's preamble
// through a real reader costs a handful of small reads and gets them from the
// file itself.
//
// The stream is header ‖ SYNC ‖ SCHEMA… and nothing else, so Next reaches EOF
// immediately; everything arrives through OnSchema.
func (a *Accessor) Schemas(segment int) (map[uint16]*logb.Schema, error) {
	if segment < 0 || segment >= len(a.idx.Segments) {
		return nil, fmt.Errorf("index: no segment %d", segment)
	}
	seg := a.idx.Segments[segment]

	if err := a.ensureHeader(); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.Write(a.header)
	if seg.Sync.Type == logb.FrameSync {
		if err := a.appendFrame(&buf, seg.Sync); err != nil {
			return nil, err
		}
	}
	// File order, so a later binding of an id wins exactly as it would on a
	// full scan.
	refs := make([]FrameRef, 0, len(seg.Schemas))
	for _, ref := range seg.Schemas {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Offset < refs[j].Offset })
	for _, ref := range refs {
		if err := a.appendFrame(&buf, ref); err != nil {
			return nil, err
		}
	}

	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return nil, err
	}
	out := map[uint16]*logb.Schema{}
	r.OnSchema = func(s *logb.Schema, id uint16) { out[id] = s }
	for {
		if _, err := r.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
	}
	if r.Truncated {
		return nil, fmt.Errorf("index: segment %d schema replay truncated", segment)
	}
	return out, nil
}

func (a *Accessor) ensureHeader() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.header != nil {
		return nil
	}
	h := make([]byte, fileHeaderSize)
	if _, err := a.ra.ReadAt(h, 0); err != nil {
		return fmt.Errorf("index: reading file header: %w", err)
	}
	a.header = h
	return nil
}

func (a *Accessor) appendFrame(buf *bytes.Buffer, ref FrameRef) error {
	b := make([]byte, ref.Size())
	if _, err := a.ra.ReadAt(b, int64(ref.Offset)); err != nil {
		return fmt.Errorf("index: reading %s frame at %d: %w", ref.Type, ref.Offset, err)
	}
	buf.Write(b)
	return nil
}
