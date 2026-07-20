package index

import (
	"sort"

	"github.com/rveen/logb"
)

// FrameRef locates one frame in the file.
type FrameRef struct {
	Offset   uint64
	Len      uint32 // payload bytes, excluding the 8-byte header and the CRC
	Type     logb.FrameType
	StreamID uint16
}

// Size is the frame's total footprint: header, payload and CRC.
func (f FrameRef) Size() uint64 { return 12 + uint64(f.Len) }

// End is the offset one past the frame.
func (f FrameRef) End() uint64 { return f.Offset + f.Size() }

// Segment records the framing preamble a reader needs before it can decode any
// of a segment's DATA frames.
//
// This is what makes random access possible without forking the core reader.
// SPEC rule 3 says a file can be cut anywhere and still decode, because every
// segment restates its schemas; the corollary is that replaying a segment's
// preamble in front of an arbitrary DATA frame is enough to decode it.
type Segment struct {
	Index int
	Sync  FrameRef
	// Schemas and UUIDs are keyed by stream_id, which is segment-scoped: every
	// SYNC frame rebinds every id (SPEC §6.6). Two segments can use id 1 for
	// entirely different streams, so nothing outside a Segment may be keyed on
	// an id.
	Schemas map[uint16]FrameRef
	UUIDs   map[uint16][16]byte
	Runs    []FrameRef
}

// DataFrame is one DATA frame and what it holds, without its bytes.
//
// A frame is independently decodable given its schema: axis_base, record_count,
// run_id, the codec and the filter all live in the frame's own payload (SPEC
// §8). That is why an index of these is enough to seek by.
type DataFrame struct {
	FrameRef
	Segment int
	UUID    [16]byte
	RunID   uint32
	Count   uint32

	// The axis extent of the frame's records. Time is kept in int64 ticks and
	// everything else in f64, for the same reason logb.AxisVal is a tagged
	// union: reading one as the other yields plausible nonsense (SPEC §5).
	Time                  bool
	FirstTick, LastTick   int64
	FirstFloat, LastFloat float64
}

// First and Last report the frame's axis extent in the units Axis.At uses:
// epoch-relative ticks for a time axis, the axis unit otherwise.
func (d DataFrame) First() float64 {
	if d.Time {
		return float64(d.FirstTick)
	}
	return d.FirstFloat
}

func (d DataFrame) Last() float64 {
	if d.Time {
		return float64(d.LastTick)
	}
	return d.LastFloat
}

// Overlaps reports whether the frame holds any record in [from, to].
func (d DataFrame) Overlaps(from, to float64) bool {
	return d.Last() >= from && d.First() <= to
}

// FrameIndex is the Tier 0 index: where every frame is, and what each DATA
// frame contains.
//
// The file's own INDEX frame is deliberately not consulted. SPEC §9 makes it
// purely an accelerator that a reader must be able to rebuild by scanning and
// must not trust over the frames themselves, so this is built by scanning —
// which is also the only thing that works on a file that was never closed.
type FrameIndex struct {
	Segments []*Segment
	Data     []DataFrame // in file order
}

// Select returns the DATA frames of a stream that overlap [from, to], in file
// order.
func (x *FrameIndex) Select(uuid [16]byte, from, to float64) []DataFrame {
	var out []DataFrame
	for _, d := range x.Data {
		if d.UUID == uuid && d.Overlaps(from, to) {
			out = append(out, d)
		}
	}
	return out
}

// All returns every DATA frame of a stream, in file order.
func (x *FrameIndex) All(uuid [16]byte) []DataFrame {
	var out []DataFrame
	for _, d := range x.Data {
		if d.UUID == uuid {
			out = append(out, d)
		}
	}
	return out
}

// Records counts the records a stream holds.
func (x *FrameIndex) Records(uuid [16]byte) int {
	n := 0
	for _, d := range x.Data {
		if d.UUID == uuid {
			n += int(d.Count)
		}
	}
	return n
}

// builder accumulates a FrameIndex during a scan.
//
// It is driven by logb.Reader's OnFrame and OnSchema hooks plus the batches
// Next returns. OnFrame fires for every frame in file order immediately before
// the frame is interpreted, and for a DATA frame Next returns right afterwards,
// so latching the last DATA frame seen and pairing it with the batch is sound
// — that ordering is guaranteed by the reader's control flow, not convention.
type builder struct {
	// offsetBias converts offsets reported by a resumed scan, which are
	// relative to the synthesized header-plus-tail stream, back into real file
	// offsets. Zero for a scan that started at the beginning.
	offsetBias uint64

	idx      FrameIndex
	cur      *Segment
	pending  FrameRef // the SCHEMA frame whose OnSchema has not fired yet
	lastData FrameRef
	haveData bool
}

func (b *builder) segment() *Segment {
	if b.cur == nil {
		// A file that starts with DATA rather than SYNC — a resynced tail, or
		// a writer that never began a segment. Give it somewhere to live.
		b.newSegment(FrameRef{})
	}
	return b.cur
}

func (b *builder) newSegment(sync FrameRef) {
	b.cur = &Segment{
		Index:   len(b.idx.Segments),
		Sync:    sync,
		Schemas: map[uint16]FrameRef{},
		UUIDs:   map[uint16][16]byte{},
	}
	b.idx.Segments = append(b.idx.Segments, b.cur)
}

// onFrame records a frame's placement.
func (b *builder) onFrame(f logb.Frame) {
	ref := FrameRef{Offset: f.Offset + b.offsetBias, Len: f.Len, Type: f.Type, StreamID: f.StreamID}
	switch f.Type {
	case logb.FrameSync:
		b.newSegment(ref)
	case logb.FrameSchema:
		// The UUID is not in hand yet; OnSchema fires next with the decoded
		// schema, and pairs with this.
		b.pending = ref
	case logb.FrameRun:
		s := b.segment()
		s.Runs = append(s.Runs, ref)
	case logb.FrameData:
		b.lastData, b.haveData = ref, true
	}
}

// onSchema binds the schema frame latched by onFrame to a stream identity.
func (b *builder) onSchema(s *logb.Schema, streamID uint16) {
	seg := b.segment()
	seg.Schemas[streamID] = b.pending
	seg.UUIDs[streamID] = s.UUID
}

// onBatch pairs the latched DATA frame with the records it turned out to hold.
func (b *builder) onBatch(batch *logb.Batch) {
	if !b.haveData {
		return
	}
	b.haveData = false

	d := DataFrame{
		FrameRef: b.lastData,
		Segment:  b.segment().Index,
		UUID:     batch.Schema.UUID,
		RunID:    batch.RunID,
		Count:    batch.Count,
		Time:     batch.Schema.AxisKind == logb.AxisTime,
	}
	if n := int(batch.Count); n > 0 {
		if first, err := batch.Axis(0); err == nil {
			if d.Time {
				d.FirstTick = first.Ticks()
			} else {
				d.FirstFloat = first.Float()
			}
		}
		if last, err := batch.Axis(n - 1); err == nil {
			if d.Time {
				d.LastTick = last.Ticks()
			} else {
				d.LastFloat = last.Float()
			}
		}
	}
	b.idx.Data = append(b.idx.Data, d)
}

// rebaseFrames shifts time-axis frame bounds onto the file epoch, matching what
// rebase does to the series themselves.
func (x *FrameIndex) rebaseFrames(epoch int64) {
	for i := range x.Data {
		if x.Data[i].Time {
			x.Data[i].FirstTick -= epoch
			x.Data[i].LastTick -= epoch
		}
	}
}

// sortSegmentRuns keeps a segment's RUN frames in file order, which is the
// order a prefix must replay them in.
func (x *FrameIndex) sortSegmentRuns() {
	for _, s := range x.Segments {
		sort.Slice(s.Runs, func(i, j int) bool { return s.Runs[i].Offset < s.Runs[j].Offset })
	}
}
