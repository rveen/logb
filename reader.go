package logb

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Reader scans frames from a Logb stream.
//
// It needs only io.Reader: nothing in the format points forward, so decoding is
// a single pass. Damage — a failed CRC, a truncated tail — ends the scan
// cleanly rather than erroring, per the format's crash-safety rule. Check
// Truncated afterwards to find out whether the file ended by intent.
type Reader struct {
	r   io.Reader
	off uint64

	// Truncated reports that the scan stopped at damage rather than at a clean
	// end. Every batch returned before it was set is intact and trustworthy.
	Truncated bool

	// Meta collects key/value frames as they are seen, in order.
	Meta []Meta

	// Attachments collects embedded files as they are seen.
	Attachments map[string][]byte

	// Closed reports that an END frame was seen: some writer closed cleanly.
	// In a concatenated file this says nothing about where the data stops.
	Closed bool

	// Unsupported collects what this reader could frame and parse but not decode:
	// a schema using an axis mode this version does not define, or a DATA frame
	// using a codec or filter it does not implement. The affected streams and
	// frames are skipped and everything else in the file reads normally, which is
	// §4.2's promise — a v0.1 reader decodes a v0.9 file's v0.1 streams and
	// ignores the rest. None of it is damage, so none of it sets Truncated; none
	// of it is silence either, so it is recorded here.
	Unsupported []error

	// OnFrame, if set, is called for every frame the scan passes, in file order,
	// after its CRC validates and before it is interpreted. It exists because the
	// sequence of frames is the format, and Next deliberately hides it: SYNC,
	// SCHEMA, META, ATTACH, RUN, INDEX and END are all applied internally and
	// only records come back. A tool that shows a file's structure — cmd/logbdump
	// — needs both, so it watches here and reads batches from Next; because this
	// fires during Next, the two interleave in file order on their own.
	//
	// A nil OnFrame costs one branch per frame.
	OnFrame func(Frame)

	// OnSchema, if set, is called each time a SCHEMA frame binds a stream,
	// with the id it was bound to. Like OnFrame it fires during Next.
	//
	// It exists because Next only ever hands out a schema attached to a batch,
	// and a stream can be declared without carrying a single record — a channel
	// that was configured but never fired, a segment restated after a cut. Such
	// a stream is part of what the file says, and a tool that lists a file's
	// contents has no other way to see it.
	//
	// The id is passed because it is the only thing that distinguishes two
	// bindings of the same schema, and because it is segment-scoped: every SYNC
	// frame rebinds every id (§6.6), so a caller accumulating across segments
	// must key on Schema.UUID and treat the id as routing, not identity.
	//
	// A nil OnSchema costs one branch per schema frame.
	OnSchema func(s *Schema, streamID uint16)

	streams map[uint16]*Schema // segment-scoped; cleared at every sync frame
	runs    map[uint32]*Run
	seq     uint64
}

// Frame is one frame's header, as the scan passes it. See Reader.OnFrame.
type Frame struct {
	// Offset is the frame header's position, counted from the start of whatever
	// the Reader was handed. For a whole file that is the file offset; for a
	// Resync'd tail it is relative to the sync frame the scan entered at.
	Offset   uint64
	Type     FrameType
	Flags    uint8
	StreamID uint16
	Len      uint32 // payload bytes, excluding the 8-byte header and the CRC
}

// Size is the frame's total footprint: header, payload, and CRC.
func (f Frame) Size() uint64 { return 12 + uint64(f.Len) }

// Batch is one DATA frame: a run of records sharing a schema and a run.
type Batch struct {
	Schema   *Schema
	Run      *Run // nil when the stream has no runs
	RunID    uint32
	AxisBase AxisVal
	Count    uint32

	// Data holds Count fixed-size records, decompressed and de-filtered,
	// followed by any tails.
	Data []byte

	// tails[i][k] is the bytes of record i's k-th variable-length field, in
	// field-declaration order. Nil when the schema has no variable fields, which
	// is the overwhelmingly common case: a stream with no tail pays nothing.
	tails [][][]byte
}

// Record returns the fixed portion of record i.
func (b *Batch) Record(i int) ([]byte, error) {
	n := b.Schema.RecordBytes()
	// Bound by Count, not by len(Data): with a tail region present, Data runs
	// past the fixed records, and a size check alone would happily return a
	// slice of tail bytes dressed up as a record.
	if i < 0 || uint32(i) >= b.Count || (i+1)*n > len(b.Data) {
		return nil, ErrCorrupt
	}
	return b.Data[i*n : (i+1)*n], nil
}

// Raw decodes field f of record i without applying its conversion.
//
// For a variable-length field this returns the tail bytes (§6.4); for every
// other field it reads the fixed portion. Returned bytes and strings alias
// b.Data and are not copied — see rawValue.
func (b *Batch) Raw(i, f int) (any, error) {
	if f < 0 || f >= len(b.Schema.Fields) {
		return nil, ErrCorrupt
	}
	fd := &b.Schema.Fields[f]

	// §6.2: an unsatisfied guard means the field is not in this record. This
	// runs before either branch below, including the variable one — a guarded
	// tail field still occupies its slot, so the tail stays walkable without
	// resolving discriminators, but its bytes are not this record's value.
	if fd.Guarded {
		rec, err := b.Record(i)
		if err != nil {
			return nil, err
		}
		ok, err := b.guardSatisfied(rec, fd)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrFieldAbsent
		}
	}

	if fd.Variable {
		k := b.Schema.varOrdinal(f)
		if i < 0 || i >= len(b.tails) || k < 0 || k >= len(b.tails[i]) {
			return nil, ErrCorrupt
		}
		blob := b.tails[i][k]
		switch fd.Type {
		case TypeBytes:
			return blob, nil
		case TypeString:
			return string(blob), nil
		}
		return nil, ErrBadVariableField
	}

	rec, err := b.Record(i)
	if err != nil {
		return nil, err
	}
	return rawValue(rec, fd)
}

// guardSatisfied reports whether fd's guard holds for the record in rec.
//
// The comparison is on raw bits, never on the converted value: a discriminator
// under a linear conversion would be compared as a float, and float equality is
// the silent-failure case this flag exists to prevent (§6.2).
func (b *Batch) guardSatisfied(rec []byte, fd *Field) (bool, error) {
	// Schema.Validate refuses an out-of-range guard, but a schema may arrive
	// from a file whose writer never ran it.
	if int(fd.GuardField) >= len(b.Schema.Fields) {
		return false, ErrCorrupt
	}
	g := &b.Schema.Fields[fd.GuardField]

	// A bool is one bit wide however the schema spelled it.
	width := g.BitWidth
	if g.Type == TypeBool {
		width = 1
	}
	v, err := extractBits(rec, g.BitOffset, width, g.BigEndian)
	if err != nil {
		return false, err
	}
	return v == fd.GuardValue, nil
}

// Value decodes field f of record i and applies its conversion.
func (b *Batch) Value(i, f int) (any, error) {
	raw, err := b.Raw(i, f)
	if err != nil {
		return nil, err
	}
	fd := &b.Schema.Fields[f]

	// Identity is type-preserving: it means "this value is already physical",
	// not "coerce it to float64". A bool channel with no conversion must read
	// back as a bool.
	if fd.Conv == nil {
		return raw, nil
	}
	if _, isIdentity := fd.Conv.(Identity); isIdentity {
		return raw, nil
	}

	// Table and text conversions are undefined for complex and rejected here.
	if z, isComplex := raw.(complex128); isComplex {
		switch c := fd.Conv.(type) {
		case Linear:
			return complex(c.A, 0) + complex(c.B, 0)*z, nil
		case Rational:
			num := complex(c.P[0], 0)*z*z + complex(c.P[1], 0)*z + complex(c.P[2], 0)
			den := complex(c.P[3], 0)*z*z + complex(c.P[4], 0)*z + complex(c.P[5], 0)
			return num / den, nil
		default:
			return nil, errors.New("logb: table and text conversions are undefined for complex fields")
		}
	}

	fv, ok := toFloat(raw)
	if !ok {
		return raw, nil
	}
	return fd.Conv.Apply(fv), nil
}

// Axis returns the axis value of record i.
func (b *Batch) Axis(i int) (AxisVal, error) {
	// Only the explicit mode reads a field; the implicit modes derive the axis
	// from the record index and cost zero bytes.
	if b.Schema.AxisMode != AxisExplicit {
		return b.Schema.AxisAt(b.AxisBase, i, 0), nil
	}
	raw, err := b.Raw(i, int(b.Schema.AxisField))
	if err != nil {
		return 0, err
	}
	fv, ok := toFloat(raw)
	if !ok {
		return 0, ErrCorrupt
	}
	return b.Schema.AxisAt(b.AxisBase, i, fv), nil
}

// NewReader reads and validates the file header.
func NewReader(r io.Reader) (*Reader, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, ErrBadMagic
	}
	if !bytes.Equal(hdr[:8], magic[:]) {
		return nil, ErrBadMagic
	}
	if binary.LittleEndian.Uint32(hdr[12:]) != crc32Of(hdr[:12]) {
		return nil, ErrCorrupt
	}
	major := binary.LittleEndian.Uint16(hdr[8:])
	if major != VersionMajor {
		return nil, ErrBadVersion
	}
	// An unknown higher minor is fine: unknown frames are skipped by length.
	return &Reader{
		r:           r,
		off:         16,
		streams:     map[uint16]*Schema{},
		runs:        map[uint32]*Run{},
		Attachments: map[string][]byte{},
	}, nil
}

// Next returns the next batch of records, or io.EOF at the end of the file.
//
// Frames that are not data — schemas, runs, metadata, attachments — are consumed
// and applied internally. Unknown frame types are skipped, which is the format's
// only extension mechanism and is enough.
func (r *Reader) Next() (*Batch, error) {
	for {
		fr, payload, err := r.frame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			// Damage. Every batch returned so far stands.
			r.Truncated = true
			return nil, io.EOF
		}
		if r.OnFrame != nil {
			r.OnFrame(fr)
		}
		streamID := fr.StreamID

		switch fr.Type {
		case FrameSync:
			// A sync frame rebinds every id. This is why concatenation works:
			// file B's stream_id 1 is simply a new binding, not a collision.
			r.streams = map[uint16]*Schema{}
			r.runs = map[uint32]*Run{}
			d := &dec{b: payload}
			d.raw(16)
			r.seq = d.u64()
			_ = d.i64() // wall_time_ns: a coarse seek hint, not an axis value

		case FrameSchema:
			s, err := r.decodeSchema(payload)
			if err != nil {
				r.Truncated = true
				return nil, io.EOF
			}
			if !s.AxisMode.known() {
				// A stream from a later version. Leaving the id unbound is the
				// whole mechanism: its DATA frames then hit the unbound-id path
				// below and are skipped, and the rest of the file is unaffected.
				r.Unsupported = append(r.Unsupported,
					fmt.Errorf("stream %q: %w (axis_mode=%d)", s.Name, ErrUnknownAxisMode, uint8(s.AxisMode)))
				delete(r.streams, streamID)
				continue
			}
			s.id = streamID
			r.streams[streamID] = s
			if r.OnSchema != nil {
				r.OnSchema(s, streamID)
			}

		case FrameRun:
			d := &dec{b: payload}
			run := &Run{ID: d.u32(), Index: d.u32(), Params: d.kv()}
			if d.err == nil {
				r.runs[run.ID] = run
			}

		case FrameMeta:
			d := &dec{b: payload}
			m := Meta{Key: d.str(), Value: d.str()}
			if d.err == nil {
				r.Meta = append(r.Meta, m)
			}

		case FrameAttach:
			d := &dec{b: payload}
			name := d.str()
			n := int(d.u32())
			data := d.raw(n)
			if d.err == nil {
				r.Attachments[name] = append([]byte(nil), data...)
			}

		case FrameData:
			s, ok := r.streams[streamID]
			if !ok {
				// A data frame for an unbound id: either a reader that started
				// at a sync frame lacking this schema, or a stream this version
				// cannot decode and left unbound on purpose (see Unsupported).
				continue
			}
			b, err := r.decodeData(s, payload)
			if err != nil {
				// A codec or filter from a later version is not damage: the
				// frame arrived intact and was understood, it just cannot be
				// carried out. Skip the frame, keep the scan, and say so.
				if errors.Is(err, ErrUnknownCodec) || errors.Is(err, ErrUnknownFilter) {
					r.Unsupported = append(r.Unsupported,
						fmt.Errorf("stream %q: %w", s.Name, err))
					continue
				}
				r.Truncated = true
				return nil, io.EOF
			}
			return b, nil

		case FrameEnd:
			// An END frame states that a writer closed cleanly at this point. It
			// is a statement about the past, not a command to stop: it has no
			// more authority over a reader than the index does. A concatenated
			// file has END frames in the middle, marking where each original
			// file ended, and scanning must continue through them (§6.6).
			r.Closed = true

		case FrameIndex:
			// Purely an accelerator. A reader must be able to rebuild it by
			// scanning and must not trust it over the frames themselves, so this
			// single-pass reader ignores it entirely.

		default:
			// Unknown frame type: skip. Already consumed by length.
		}
	}
}

func (r *Reader) decodeSchema(payload []byte) (*Schema, error) {
	d := &dec{b: payload}
	s := &Schema{}
	copy(s.UUID[:], d.raw(16))
	s.Name = d.str()
	s.RecordBits = d.u32()
	s.AxisKind = AxisKind(d.u8())
	s.AxisMode = AxisMode(d.u8())
	s.AxisExp = d.i8()
	d.u8() // reserved
	s.AxisUnit = d.str()
	s.AxisStep = AxisVal(d.u64())
	s.AxisScale = AxisVal(d.u64())
	s.AxisField = d.u16()
	n := int(d.u16())
	for i := 0; i < n && d.err == nil; i++ {
		var f Field
		f.Name = d.str()
		f.BitOffset = d.u32()
		f.BitWidth = d.u32()
		f.Type = DataType(d.u8())
		f.BigEndian = d.u8() == 1
		flags := d.u8()
		f.Variable = flags&1 != 0
		f.Guarded = flags&2 != 0
		f.Unit = d.str()
		f.Desc = d.str()
		f.Conv = d.conv()
		if f.Guarded {
			f.GuardField = d.u16()
			f.GuardValue = d.u64()
		}
		f.Meta = d.kv()
		s.Fields = append(s.Fields, f)
	}
	s.Meta = d.kv()
	return s, d.err
}

func (r *Reader) decodeData(s *Schema, payload []byte) (*Batch, error) {
	d := &dec{b: payload}
	b := &Batch{Schema: s}
	b.AxisBase = AxisVal(d.u64())
	b.Count = d.u32()
	b.RunID = d.u32()
	codec := Codec(d.u8())
	filter := Filter(d.u8())
	d.u16() // reserved
	rawSize := d.u64()
	if d.err != nil {
		return nil, d.err
	}
	data := payload[d.i:]

	if codec != CodecNone {
		var err error
		data, err = decompress(codec, data, rawSize)
		if err != nil {
			return nil, err
		}
	}
	fixed := int(b.Count) * s.RecordBytes()

	switch filter {
	case FilterNone:
	case FilterTranspose:
		// §8: transpose covers the fixed portion only — tails are appended
		// untransposed, having no fixed stride to be transposed along.
		if fixed > len(data) {
			return nil, ErrCorrupt
		}
		out := detranspose(data[:fixed], s.RecordBytes())
		if fixed < len(data) {
			out = append(out, data[fixed:]...)
		}
		data = out
	default:
		// The records are permuted by a transform this version does not know.
		// Returning them unfiltered would be returning garbage shaped like data.
		return nil, fmt.Errorf("%w: %d", ErrUnknownFilter, filter)
	}
	b.Data = data

	if nvar := s.varCount(); nvar > 0 {
		if err := b.parseTails(fixed, nvar); err != nil {
			return nil, err
		}
	}
	b.Run = r.runs[b.RunID]
	return b, nil
}

// parseTails walks the tail region once (§6.4). The region follows every fixed
// record, and each record contributes one u32-length-prefixed blob per variable
// field, in field-declaration order.
//
// It is parsed sequentially and holds no pointers, so record i's blobs can only
// be reached by walking records 0..i-1. That is why §6.4 calls this the slow path
// and tells bus payloads to use fixed-width bytes fields instead: a variable
// field costs the ability to seek within a batch.
//
// Every length is bounds-checked, so a tail truncated by power loss fails as
// damage rather than reading off the end — rule 2, the same as any other torn
// frame.
func (b *Batch) parseTails(fixed, nvar int) error {
	if fixed > len(b.Data) {
		return ErrCorrupt
	}
	d := &dec{b: b.Data, i: fixed}
	b.tails = make([][][]byte, b.Count)
	for i := range b.tails {
		row := make([][]byte, nvar)
		for k := 0; k < nvar; k++ {
			n := d.u32()
			if uint64(n) > uint64(len(b.Data)) {
				return ErrCorrupt
			}
			row[k] = d.raw(int(n))
		}
		if d.err != nil {
			return d.err
		}
		b.tails[i] = row
	}
	return nil
}

// frame reads one frame and validates its CRC. The returned Frame records where
// the frame started, which is why the offset is captured before any read.
func (r *Reader) frame() (Frame, []byte, error) {
	at := r.off

	hdr := make([]byte, 8)
	n, err := io.ReadFull(r.r, hdr)
	if err != nil {
		if n == 0 && (errors.Is(err, io.EOF)) {
			return Frame{}, nil, io.EOF
		}
		return Frame{}, nil, ErrCorrupt
	}
	f := Frame{
		Offset:   at,
		Len:      binary.LittleEndian.Uint32(hdr[0:]),
		Type:     FrameType(hdr[4]),
		Flags:    hdr[5],
		StreamID: binary.LittleEndian.Uint16(hdr[6:]),
	}

	payload := make([]byte, f.Len)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return Frame{}, nil, ErrCorrupt
	}
	var crc [4]byte
	if _, err := io.ReadFull(r.r, crc[:]); err != nil {
		return Frame{}, nil, ErrCorrupt
	}
	want := binary.LittleEndian.Uint32(crc[:])
	got := crc32Update(crc32Of(hdr), payload)
	if want != got {
		return Frame{}, nil, ErrCorrupt
	}
	r.off += f.Size()
	return f, payload, nil
}

// zstdDec is built once and shared: DecodeAll is safe for concurrent use, and a
// decoder built with a nil source is the stateless one-shot form.
var zstdDec = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil)
})

// maxAllocHint bounds a preallocation taken from raw_size, which is a u64 read
// straight out of the frame and therefore states what a possibly-corrupt file
// claims, not what is true. A damaged length field must not be able to demand an
// arbitrary allocation before a byte has been decompressed — that would hand rule
// 2 a way to fail: the frame is about to be rejected on CRC anyway, and it should
// not take the process down on the way. Capping the hint costs a valid file
// nothing, since the buffer still grows to whatever the payload really needs.
const maxAllocHint = 16 << 20

func allocHint(rawSize uint64) int {
	if rawSize > maxAllocHint {
		return maxAllocHint
	}
	return int(rawSize)
}

func decompress(c Codec, data []byte, rawSize uint64) ([]byte, error) {
	switch c {
	case CodecZstd:
		d, err := zstdDec()
		if err != nil {
			return nil, err
		}
		return d.DecodeAll(data, make([]byte, 0, allocHint(rawSize)))
	case CodecDeflate:
		zr := flate.NewReader(bytes.NewReader(data))
		defer zr.Close()
		out := make([]byte, 0, allocHint(rawSize))
		buf := bytes.NewBuffer(out)
		if _, err := io.Copy(buf, zr); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, ErrUnknownCodec
}

// Resync scans forward for the next sync pattern and returns a Reader positioned
// at that segment. It is how you decode a file whose beginning you do not have:
// a cut recording, a tapped stream, a partially recovered card.
//
// This is the property MDF4's link graph cannot provide at all.
func Resync(data []byte) (*Reader, int, error) {
	for i := 0; i+24 <= len(data); i++ {
		if !bytes.Equal(data[i:i+16], syncPattern[:]) {
			continue
		}
		// Back up 8 bytes to the frame header and validate.
		if i < 8 {
			continue
		}
		start := i - 8
		length := binary.LittleEndian.Uint32(data[start:])
		end := start + 8 + int(length) + 4
		if end > len(data) || FrameType(data[start+4]) != FrameSync {
			continue
		}
		sum := crc32Of(data[start : start+8])
		sum = crc32Update(sum, data[start+8:start+8+int(length)])
		if sum != binary.LittleEndian.Uint32(data[start+8+int(length):]) {
			continue
		}
		return &Reader{
			r:           bytes.NewReader(data[start:]),
			off:         uint64(start),
			streams:     map[uint16]*Schema{},
			runs:        map[uint32]*Run{},
			Attachments: map[string][]byte{},
		}, start, nil
	}
	return nil, 0, errors.New("logb: no sync pattern found")
}
