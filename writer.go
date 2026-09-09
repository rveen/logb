package logb

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Writer appends frames to a Logb stream. It never seeks: every frame is
// complete when written, so a file cut short at any byte remains valid.
//
// A Writer needs only io.Writer — which is the whole point. An MDF4 writer needs
// io.WriteSeeker because its blocks reference each other by absolute offset and
// must be patched after the fact.
type Writer struct {
	w   io.Writer
	off uint64 // bytes written, for index entries

	streams []*Schema
	runs    []*Run
	nextID  uint16
	seq     uint64
	inSeg   bool

	Codec  Codec
	Filter Filter

	index map[[16]byte][]indexEntry
	order [][16]byte // index groups in first-seen order, for reproducible output

	// Run bookkeeping for the current segment, enforcing §6.5's contiguity rule.
	// Both are cleared at every segment boundary, because a run may legitimately
	// resume in a later segment.
	curRun  map[[16]byte]uint32
	pastRun map[[16]byte]map[uint32]bool

	// segStep is the axis_step each stream declared in the current segment's
	// SCHEMA frame, so a mid-segment change can be caught at the WriteData that
	// would have been misread. Cleared at every segment boundary.
	segStep map[[16]byte]AxisVal

	// hold is the current value of each stream's held fields, restated into
	// every segment this writer opens. It is writer state rather than a frame
	// the caller emits, because the frame's whole job is to be present in every
	// segment preamble, and a caller that had to remember it would forget.
	hold map[[16]byte]*holdState
}

type holdState struct {
	base    AxisVal
	runID   uint32
	present []bool
	record  []byte
}

type indexEntry struct {
	offset  uint64
	first   AxisVal
	records uint32
	runID   uint32
}

// NewWriter writes the file header and returns a Writer.
func NewWriter(w io.Writer) (*Writer, error) {
	ow := &Writer{
		w:       w,
		Codec:   CodecNone,
		Filter:  FilterNone,
		index:   map[[16]byte][]indexEntry{},
		curRun:  map[[16]byte]uint32{},
		pastRun: map[[16]byte]map[uint32]bool{},
		segStep: map[[16]byte]AxisVal{},
		hold:    map[[16]byte]*holdState{},
	}
	var e buf
	e.raw(magic[:])
	e.u16(VersionMajor)
	e.u16(VersionMinor)
	e.u32(crc32Of(e.b))
	return ow, ow.write(e.b)
}

func (w *Writer) write(b []byte) error {
	n, err := w.w.Write(b)
	w.off += uint64(n)
	return err
}

// AddStream registers a schema and assigns its segment-scoped routing tag. The
// schema is restated at the start of every segment.
func (w *Writer) AddStream(s *Schema) error {
	if s.UUID == ([16]byte{}) {
		return fmt.Errorf("logb: stream %q has zero UUID; identity is the writer's job", s.Name)
	}
	if err := s.Validate(); err != nil {
		return err
	}
	s.id = w.nextID
	w.nextID++
	w.streams = append(w.streams, s)
	if w.inSeg {
		return w.writeSchema(s)
	}
	return nil
}

// AddRun declares a run: one dataset within a stream, under different
// conditions. A logger never calls this.
func (w *Writer) AddRun(r *Run) error {
	w.runs = append(w.runs, r)
	if w.inSeg {
		return w.writeRun(r)
	}
	return nil
}

// BeginSegment starts a segment: a sync frame followed by a restatement of every
// schema and run. This is what makes a cut file decodable (§4) and what makes
// stream_id segment-scoped, so files concatenate without id collisions (§6.6).
//
// wallTimeNs is a coarse seek hint — when the segment was written, not what its
// streams' axes mean. Pass 0 if unknown.
func (w *Writer) BeginSegment(wallTimeNs int64) error {
	var e buf
	e.raw(syncPattern[:])
	e.u64(w.seq)
	e.i64(wallTimeNs)
	if err := w.frame(FrameSync, 0, e.b); err != nil {
		return err
	}
	w.seq++
	w.inSeg = true

	// Run scoping is per segment, like stream ids: a run that spans a segment
	// boundary is contiguous in each, which is all §6.5 asks for.
	clear(w.curRun)
	clear(w.pastRun)
	clear(w.segStep)

	for _, s := range w.streams {
		if err := w.writeSchema(s); err != nil {
			return err
		}
	}
	for _, r := range w.runs {
		if err := w.writeRun(r); err != nil {
			return err
		}
	}
	// After the runs, because a HOLD frame names the run its values were last
	// written in.
	for _, s := range w.streams {
		if h, ok := w.hold[s.UUID]; ok {
			if err := w.writeHold(s, h); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetHold records the current value of a stream's held fields, to be restated
// in the preamble of every segment this writer opens from now on.
//
// record is one record in the stream's own layout — the same bytes WriteData
// takes, for a single record, including its tails if the schema has variable
// fields. present says which fields the record actually carries a value for,
// and must have one entry per field: a setpoint that has never been set has no
// value to restate, and a zero there would be a lie rather than a gap.
//
// base is where on the axis these values were last written, which may be far
// before the segment that restates them — that is the point. runID is the run
// they were written in.
//
// Values are restated, not written: SetHold emits nothing on its own and adds
// no record to the stream. A writer calls it beside the WriteData that carried
// the change.
func (w *Writer) SetHold(s *Schema, base AxisVal, runID uint32, present []bool, record []byte) error {
	if s == nil || len(present) != len(s.Fields) {
		return ErrBadHold
	}
	if len(record) < s.RecordBytes() {
		return ErrBadHold
	}
	w.hold[s.UUID] = &holdState{
		base:    base,
		runID:   runID,
		present: append([]bool(nil), present...),
		record:  append([]byte(nil), record...),
	}
	return nil
}

// ClearHold drops a stream's held values, so later segments restate nothing for
// it. A channel that was released, or an instrument that went away.
func (w *Writer) ClearHold(s *Schema) {
	if s != nil {
		delete(w.hold, s.UUID)
	}
}

func (w *Writer) writeHold(s *Schema, h *holdState) error {
	var e buf
	e.u64(uint64(h.base))
	e.u32(h.runID)
	e.u32(0) // reserved
	bits := make([]byte, (len(s.Fields)+7)/8)
	for i, p := range h.present {
		if p {
			bits[i/8] |= 1 << uint(i%8)
		}
	}
	e.raw(bits)
	e.raw(h.record)
	return w.frame(FrameHold, s.id, e.b)
}

func (w *Writer) writeSchema(s *Schema) error {
	// What this segment now claims the spacing is. A stream's schema is read
	// from the caller's own *Schema at every restatement, so changing AxisStep
	// and opening a segment is how a scope's timebase change is expressed.
	w.segStep[s.UUID] = s.AxisStep

	var e buf
	e.raw(s.UUID[:])
	e.str(s.Name)
	e.u32(s.RecordBits)
	e.u8(uint8(s.AxisKind))
	e.u8(uint8(s.AxisMode))
	e.i8(s.AxisExp)
	e.u8(0) // reserved
	e.str(s.AxisUnit)
	e.u64(uint64(s.AxisStep))
	e.u64(uint64(s.AxisScale))
	e.u16(s.AxisField)
	e.u16(uint16(len(s.Fields)))
	for i := range s.Fields {
		f := &s.Fields[i]
		e.str(f.Name)
		e.u32(f.BitOffset)
		e.u32(f.BitWidth)
		e.u8(uint8(f.Type))
		if f.BigEndian {
			e.u8(1)
		} else {
			e.u8(0)
		}
		var flags uint8
		if f.Variable {
			flags |= 1
		}
		if f.Guarded {
			flags |= 2
		}
		if f.Hold {
			flags |= 4
		}
		e.u8(flags)
		e.str(f.Unit)
		e.str(f.Desc)
		e.conv(f.Conv)
		// Present only when guarded: an unguarded field pays nothing for a
		// feature it does not use, and every schema is restated per segment.
		if f.Guarded {
			e.u16(f.GuardField)
			e.u64(f.GuardValue)
		}
		e.kv(f.Meta)
	}
	e.kv(s.Meta)
	return w.frame(FrameSchema, s.id, e.b)
}

func (w *Writer) writeRun(r *Run) error {
	var e buf
	e.u32(r.ID)
	e.u32(r.Index)
	e.kv(r.Params)
	return w.frame(FrameRun, 0, e.b)
}

// WriteMeta appends a key/value pair. Emitted after the records it describes is
// legal and useful: a logger that boots without an RTC writes time.anchor here
// once GPS fixes, retroactively dating records already on disk.
func (w *Writer) WriteMeta(key, value string) error {
	var e buf
	e.str(key)
	e.str(value)
	return w.frame(FrameMeta, 0, e.b)
}

// WriteAttach embeds a file: a DBC, a calibration, a netlist.
func (w *Writer) WriteAttach(name string, data []byte) error {
	var e buf
	e.str(name)
	e.u32(uint32(len(data)))
	e.raw(data)
	return w.frame(FrameAttach, 0, e.b)
}

// WriteData appends a batch of records for a stream.
//
// records holds recordCount fixed-size records, followed by any tails. base is
// the axis value of the first record, in ticks for AxisTime streams and as a
// float64 otherwise.
func (w *Writer) WriteData(s *Schema, base AxisVal, runID uint32, recordCount uint32, records []byte) error {
	if !w.inSeg {
		if err := w.BeginSegment(0); err != nil {
			return err
		}
	}

	// §5.2: axis_step is schema-scoped, and a schema is only restated at a
	// segment boundary. Changing it under a segment's declared value would make
	// every record of this frame decode to a wrong axis position, silently.
	if step, declared := w.segStep[s.UUID]; declared && step != s.AxisStep {
		return fmt.Errorf("%w (stream %q, declared %d, now %d)",
			ErrAxisStepChanged, s.Name, int64(step), int64(s.AxisStep))
	}

	// §6.5: within a segment, a stream's runs are contiguous. A writer that has
	// runs to interleave buffers them or starts a new segment; either is cheaper
	// than every reader having to cope with a shuffled file.
	if cur, started := w.curRun[s.UUID]; !started || cur != runID {
		if w.pastRun[s.UUID][runID] {
			return fmt.Errorf("%w (stream %q, run %d)", ErrRunInterleaved, s.Name, runID)
		}
		if started {
			if w.pastRun[s.UUID] == nil {
				w.pastRun[s.UUID] = map[uint32]bool{}
			}
			w.pastRun[s.UUID][cur] = true
		}
		w.curRun[s.UUID] = runID
	}

	if _, seen := w.index[s.UUID]; !seen {
		w.order = append(w.order, s.UUID)
	}
	w.index[s.UUID] = append(w.index[s.UUID], indexEntry{
		offset:  w.off,
		first:   base,
		records: recordCount,
		runID:   runID,
	})

	payload := records
	filter := w.Filter
	if filter == FilterTranspose {
		// §8: transpose covers the fixed portion only. Running it over the tails
		// as well would scramble them — or, worse, silently do nothing, because
		// transpose declines any input that is not a whole number of records and
		// fixed+tails rarely is.
		fixed := int(recordCount) * s.RecordBytes()
		if fixed > len(records) {
			return fmt.Errorf("logb: stream %q: %d records need %d bytes, got %d",
				s.Name, recordCount, fixed, len(records))
		}
		payload = transpose(records[:fixed], s.RecordBytes())
		if fixed < len(records) {
			payload = append(payload, records[fixed:]...)
		}
	}
	raw := len(payload)

	codec := w.Codec
	if codec != CodecNone {
		var err error
		payload, err = compress(codec, payload)
		if err != nil {
			return err
		}
	}

	var e buf
	e.u64(uint64(base))
	e.u32(recordCount)
	e.u32(runID)
	e.u8(uint8(codec))
	e.u8(uint8(filter))
	e.u16(0) // reserved
	e.u64(uint64(raw))
	e.raw(payload)
	return w.frame(FrameData, s.id, e.b)
}

// Close writes the index and the end frame. A file that never reaches Close —
// power loss, a killed process — is still valid; it simply lacks both.
func (w *Writer) Close() error {
	if err := w.writeIndex(); err != nil {
		return err
	}
	return w.frame(FrameEnd, 0, nil)
}

func (w *Writer) writeIndex() error {
	// Offsets are stored as distances backwards from the start of this INDEX
	// frame, not as absolute file positions. An absolute offset would be wrong
	// the moment the file is concatenated onto another (§6.6); a self-relative
	// one stays correct wherever the file lands.
	base := w.off

	var e buf
	e.u32(uint32(len(w.order)))
	for _, uuid := range w.order {
		entries := w.index[uuid]
		e.raw(uuid[:])
		e.u32(uint32(len(entries)))
		for _, en := range entries {
			e.u64(base - en.offset)
			e.u64(uint64(en.first))
			e.u32(en.records)
			e.u32(en.runID)
		}
	}
	return w.frame(FrameIndex, 0, e.b)
}

// frame writes one frame: header, payload, CRC over both.
func (w *Writer) frame(t FrameType, streamID uint16, payload []byte) error {
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:], uint32(len(payload)))
	hdr[4] = byte(t)
	hdr[5] = 0
	binary.LittleEndian.PutUint16(hdr[6:], streamID)

	sum := crc32Of(hdr)
	sum = crc32Update(sum, payload)

	if err := w.write(hdr); err != nil {
		return err
	}
	if err := w.write(payload); err != nil {
		return err
	}
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], sum)
	return w.write(tail[:])
}

// zstdEnc is built once and shared: EncodeAll is safe for concurrent use, and
// standing up an encoder per DATA frame would cost more than compressing one.
var zstdEnc = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil)
})

func compress(c Codec, data []byte) ([]byte, error) {
	switch c {
	case CodecZstd:
		e, err := zstdEnc()
		if err != nil {
			return nil, err
		}
		return e.EncodeAll(data, nil), nil
	case CodecDeflate:
		var out bytes.Buffer
		zw, err := flate.NewWriter(&out, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	return nil, fmt.Errorf("%w: %d", ErrUnknownCodec, c)
}
