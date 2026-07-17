// Package logb implements Logb v0.1 — a self-describing binary format for
// time-series measurement, bus-trace, and simulation recording. The name is
// "log" plus "b" for binary, the way .xlsb is to .xlsx.
//
// The design is a varve: one season's sediment couplet in a lake bed, laid down
// in sequence, never rewritten, and readable from any cut face of the core. That
// is this format's design, and geologists date events by counting them.
//
// See SPEC.md. The format is append-only and never points forward: a writer only
// appends, a reader only scans, and a file truncated by power loss is a valid
// file containing every record up to the last intact frame.
package logb

import (
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"time"
)

const (
	VersionMajor = 0
	VersionMinor = 1
)

// magic follows PNG's design: the high bit catches 7-bit-clean transports, the
// CRLF pair catches line-ending mangling in both directions, 0x1a stops `type` on
// DOS. PNG's trailing newline is not here — a four-letter name spends that byte,
// and the CRLF pair already breaks under the translation it guarded against.
var magic = [8]byte{0x89, 'L', 'O', 'G', 'B', '\r', '\n', 0x1a}

// syncPattern marks a segment boundary. A reader that has lost framing scans for
// these 16 bytes, backs up 8 to the frame header, and validates the CRC. The
// eight-byte token needs no NUL pad; the tail is random, and is entropy only.
var syncPattern = [16]byte{
	'L', 'O', 'G', 'B', 'S', 'Y', 'N', 'C',
	0xa7, 0x3e, 0x91, 0xd2, 0x5c, 0x68, 0x0b, 0xf4,
}

// crc32c is CRC-32 Castagnoli, which has hardware support on every target that
// matters.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

func crc32Of(b []byte) uint32                 { return crc32.Checksum(b, crcTable) }
func crc32Update(sum uint32, b []byte) uint32 { return crc32.Update(sum, crcTable, b) }

// FrameType identifies what a frame carries. See SPEC.md §3.3.
//
// It is exported because the sequence of frames is the format — a tool that
// inspects or repairs a file works at this level, not at the level of records.
type FrameType uint8

const (
	FrameSync   FrameType = 0x01
	FrameSchema FrameType = 0x10
	FrameMeta   FrameType = 0x11
	FrameAttach FrameType = 0x12
	FrameRun    FrameType = 0x13
	FrameData   FrameType = 0x20
	FrameIndex  FrameType = 0x30
	FrameEnd    FrameType = 0x40

	// FrameSign is reserved, not defined in v0.1. A signature over the preceding
	// segment fits the frame model without breaking rule 1; reserving the id now
	// keeps a third party from taking it for something else. Readers skip it by
	// length like any unknown frame.
	FrameSign FrameType = 0x50
)

func (t FrameType) String() string {
	switch t {
	case FrameSync:
		return "SYNC"
	case FrameSchema:
		return "SCHEMA"
	case FrameMeta:
		return "META"
	case FrameAttach:
		return "ATTACH"
	case FrameRun:
		return "RUN"
	case FrameData:
		return "DATA"
	case FrameIndex:
		return "INDEX"
	case FrameEnd:
		return "END"
	case FrameSign:
		return "SIGN"
	}
	return fmt.Sprintf("0x%02x", uint8(t))
}

// AxisKind is the physical meaning of a stream's independent variable.
type AxisKind uint8

const (
	AxisTime AxisKind = iota
	AxisFrequency
	AxisAngle
	AxisDistance
	AxisIndex
	AxisOther
)

// AxisMode says whether the axis is derived from the record index or stored.
type AxisMode uint8

const (
	AxisImplicit AxisMode = iota // axis = base + i*step
	AxisExplicit                 // axis = base + field*scale

	// AxisLog is implicit spacing in log space: axis = base * ratio^i, with the
	// ratio in AxisStep as an f64. A decade sweep with ten points per decade is
	// ratio = 10^(1/10) and costs zero bytes per record. Undefined for AxisTime,
	// which counts integer ticks.
	AxisLog
)

// known reports whether this version defines the mode. A reader that meets one it
// does not know must skip the stream rather than guess: every mode computes the
// axis differently, so guessing means silently reporting a wrong axis for every
// record. This is what makes a future axis_mode safe to add, the same way
// ErrUnknownCodec makes a future codec safe to add.
func (m AxisMode) known() bool {
	switch m {
	case AxisImplicit, AxisExplicit, AxisLog:
		return true
	}
	return false
}

// DataType is a field's stored representation.
type DataType uint8

const (
	TypeUint DataType = iota
	TypeSint
	TypeFloat
	TypeBool
	TypeBytes
	TypeString
	TypeComplex
)

func (t DataType) String() string {
	switch t {
	case TypeUint:
		return "uint"
	case TypeSint:
		return "sint"
	case TypeFloat:
		return "float"
	case TypeBool:
		return "bool"
	case TypeBytes:
		return "bytes"
	case TypeString:
		return "string"
	case TypeComplex:
		return "complex"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Codec identifies the compression applied to a DATA frame payload.
type Codec uint8

const (
	CodecNone Codec = iota
	CodecZstd
	CodecLZ4
	CodecDeflate
)

// Filter identifies a reversible transform applied before compression.
type Filter uint8

const (
	FilterNone Filter = iota
	// FilterTranspose groups byte i of every record together, giving columnar
	// locality on a row-major layout. Applies only to the fixed portion.
	FilterTranspose
)

var (
	ErrBadMagic     = errors.New("logb: not a Logb file")
	ErrBadVersion   = errors.New("logb: unsupported major version")
	ErrCorrupt      = errors.New("logb: corrupt frame")
	ErrUnknownCodec = errors.New("logb: unknown codec")

	// ErrUnknownFilter reports a filter this version does not define. Like an
	// unknown codec, it is frame-fatal and must not be ignored: the records are
	// still permuted, and handing them back unfiltered would be handing back
	// garbage that looks like data.
	ErrUnknownFilter = errors.New("logb: unknown filter")

	// ErrTickTooFine reports that a stream's tick is finer than a nanosecond and
	// so cannot be carried by time.Duration, which is int64 nanoseconds.
	ErrTickTooFine = errors.New("logb: axis_exp finer than nanoseconds; time.Duration cannot represent it")

	// ErrLogAxisTime reports AxisLog on a time axis, which is undefined: time is
	// an integer count of ticks and a log-spaced tick count is not one.
	ErrLogAxisTime = errors.New("logb: axis_mode=log is undefined for axis_kind=time")

	// ErrUnknownAxisMode reports an axis_mode this version does not define —
	// presumably from a later one. It is stream-fatal: unlike an unknown frame
	// type, which is skippable, and unlike an unknown codec, which costs only its
	// own frame, an unknown axis mode makes every record's axis unknowable. The
	// stream is skipped whole (§4.2).
	ErrUnknownAxisMode = errors.New("logb: unknown axis_mode; stream is from a later version")

	// ErrBadVariableField reports a variable-length field that is not bytes or
	// string, or that claims bits in the fixed portion. §6.4: a variable field's
	// payload lives in the tail and it occupies no fixed bits, so bit_width is 0.
	ErrBadVariableField = errors.New("logb: variable-length field must be bytes or string with bit_width 0")

	// ErrUnalignedBlob reports a fixed bytes/string field that is not a whole
	// number of bytes on a byte boundary. Unlike an integer, a blob at bit 3 has
	// no meaning: there is no byte to return.
	ErrUnalignedBlob = errors.New("logb: fixed bytes/string field must be byte-aligned and byte-sized")

	// ErrConvOnBlob reports a non-identity conversion on a bytes or string field.
	// §7 defines the six numeric conversions for numeric types only.
	ErrConvOnBlob = errors.New("logb: conversions are undefined for bytes and string fields")

	// ErrRunInterleaved reports a DATA frame that returns to a run_id the stream
	// already left within this segment. See SPEC.md §6.5: runs are contiguous
	// per stream per segment, so a reader can close a run out when the id
	// changes instead of buffering the whole segment to find out.
	ErrRunInterleaved = errors.New("logb: run_id reappears after the stream left it; runs must be contiguous within a segment")
)

// AxisVal is the eight bytes of an axis quantity. Its interpretation depends on
// the stream's AxisKind: an int64 tick count for AxisTime, an IEEE float64
// otherwise. Same bytes, the kind selects the reading.
type AxisVal uint64

// TickVal makes an AxisVal from a tick count, for AxisTime streams.
func TickVal(ticks int64) AxisVal { return AxisVal(ticks) }

// FloatVal makes an AxisVal from a float, for every other axis kind.
func FloatVal(f float64) AxisVal { return AxisVal(math.Float64bits(f)) }

// Ticks reads the value as a tick count. Valid only for AxisTime.
func (a AxisVal) Ticks() int64 { return int64(a) }

// Float reads the value as a float. Valid for every kind except AxisTime.
func (a AxisVal) Float() float64 { return math.Float64frombits(uint64(a)) }

// Seconds converts a tick count to seconds using the stream's exponent. Lossy:
// a femtosecond tick has more precision than float64 can hold over a long run.
// Prefer comparing raw ticks.
func (a AxisVal) Seconds(exp int8) float64 {
	return float64(a.Ticks()) * math.Pow10(int(exp))
}

// Duration converts a tick count to a time.Duration.
//
// It fails with ErrTickTooFine when exp < -9. This is not a limitation worth
// working around: time.Duration is int64 nanoseconds and a femtosecond tick has
// no representation in it at all. Callers that need sub-nanosecond time must
// work in raw ticks.
func (a AxisVal) Duration(exp int8) (time.Duration, error) {
	if exp < -9 {
		return 0, fmt.Errorf("%w (axis_exp=%d)", ErrTickTooFine, exp)
	}
	ticks := a.Ticks()
	mul := int64(1)
	for i := int8(0); i < exp+9; i++ {
		mul *= 10
		if mul > math.MaxInt64/10 && i < exp+8 {
			return 0, fmt.Errorf("logb: tick scale 10^%d overflows Duration", exp)
		}
	}
	if ticks != 0 && (ticks > math.MaxInt64/mul || ticks < math.MinInt64/mul) {
		return 0, fmt.Errorf("logb: %d ticks at 10^%d overflows Duration", ticks, exp)
	}
	return time.Duration(ticks * mul), nil
}

// Field is one channel: a named slice of bits within a record, plus how to turn
// those bits into a physical value.
type Field struct {
	Name      string
	BitOffset uint32
	BitWidth  uint32
	Type      DataType
	BigEndian bool // a single CAN frame routinely mixes Intel and Motorola signals
	Variable  bool // payload lives in the record tail
	Unit      string
	Desc      string
	Conv      Conversion
}

// Schema defines a stream: its record layout, its axis, and its identity.
type Schema struct {
	// UUID states which logical stream this is, across segments and files.
	// Equal UUID means same stream; it is the writer's job to make that true.
	// Persist it across file rollover so the files concatenate into one
	// recording; generate a fresh one per instrument so two identically
	// configured loggers do not falsely merge.
	UUID [16]byte

	Name       string
	RecordBits uint32

	AxisKind  AxisKind
	AxisMode  AxisMode
	AxisExp   int8 // AxisTime only: one tick is 10^AxisExp seconds
	AxisUnit  string
	AxisStep  AxisVal // AxisImplicit: the step; AxisLog: the ratio, as f64
	AxisScale AxisVal // AxisExplicit only
	AxisField uint16  // AxisExplicit only: index into Fields

	Fields []Field
	Meta   map[string]string

	id uint16 // segment-scoped routing tag, assigned by the writer
}

// RecordBytes is the size of the fixed portion of a record.
func (s *Schema) RecordBytes() int { return int((s.RecordBits + 7) / 8) }

// varCount is the number of variable-length fields, which is how many
// length-prefixed blobs each record contributes to the tail region (§6.4).
func (s *Schema) varCount() int {
	n := 0
	for i := range s.Fields {
		if s.Fields[i].Variable {
			n++
		}
	}
	return n
}

// varOrdinal returns field f's position among the variable fields, or -1 if it
// is not one. Fixed fields — the hot path — pay a single bool test.
func (s *Schema) varOrdinal(f int) int {
	if !s.Fields[f].Variable {
		return -1
	}
	k := 0
	for i := 0; i < f; i++ {
		if s.Fields[i].Variable {
			k++
		}
	}
	return k
}

// Validate reports whether the schema is one this implementation will write or
// decode. It catches the combinations the spec leaves undefined rather than
// letting them reach a file, where the disagreement would be silent.
func (s *Schema) Validate() error {
	if !s.AxisMode.known() {
		return fmt.Errorf("%w (axis_mode=%d)", ErrUnknownAxisMode, uint8(s.AxisMode))
	}
	if s.AxisMode == AxisLog && s.AxisKind == AxisTime {
		return ErrLogAxisTime
	}
	if s.AxisMode == AxisLog {
		if r := s.AxisStep.Float(); !(r > 0) || math.IsInf(r, 0) || r == 1 {
			return fmt.Errorf("logb: axis ratio %v is not a usable log step (want finite, positive, != 1)", r)
		}
	}
	for i := range s.Fields {
		f := &s.Fields[i]
		blob := f.Type == TypeBytes || f.Type == TypeString

		if f.Variable {
			// A variable field has no bits in the fixed portion — its bytes are
			// in the tail. Anything else is a layout claim the format cannot
			// honour, so it is refused rather than silently ignored (§6.4).
			if !blob {
				return fmt.Errorf("%w: field %q is %v", ErrBadVariableField, f.Name, f.Type)
			}
			if f.BitWidth != 0 {
				return fmt.Errorf("%w: field %q has bit_width %d, want 0", ErrBadVariableField, f.Name, f.BitWidth)
			}
		} else if blob && (f.BitOffset%8 != 0 || f.BitWidth%8 != 0) {
			// A fixed bytes/string field is a slice of whole bytes. Bit-offset
			// blobs have no meaning: there is no byte to hand back.
			return fmt.Errorf("%w: field %q at bit %d, width %d", ErrUnalignedBlob, f.Name, f.BitOffset, f.BitWidth)
		}

		// §7: the six non-identity conversions take a numeric input. Applying one
		// to bytes or string is invalid — and a reader that let it through would
		// route the field into toFloat, fail, and silently hand back the raw
		// value as though no conversion had been asked for.
		if blob && f.Conv != nil {
			if _, isIdentity := f.Conv.(Identity); !isIdentity {
				return fmt.Errorf("%w: field %q is %v with a %T conversion", ErrConvOnBlob, f.Name, f.Type, f.Conv)
			}
		}
	}
	return nil
}

// AxisAt returns the axis value of record i within a batch starting at base.
//
// It assumes a known AxisMode and has no way to report that it is not: an unknown
// mode falls through to the explicit formula and yields base. Callers get a
// schema either from a Reader, which skips streams whose mode it does not know,
// or from their own code, where Validate is the check. Do not reach here with an
// unvalidated schema from a later version of the format.
func (s *Schema) AxisAt(base AxisVal, i int, explicit float64) AxisVal {
	if s.AxisKind == AxisTime {
		if s.AxisMode == AxisImplicit {
			return TickVal(base.Ticks() + int64(i)*s.AxisStep.Ticks())
		}
		return TickVal(base.Ticks() + int64(explicit)*s.AxisScale.Ticks())
	}
	switch s.AxisMode {
	case AxisImplicit:
		return FloatVal(base.Float() + float64(i)*s.AxisStep.Float())
	case AxisLog:
		// base * ratio^i. An AC decade sweep is uniform here and nowhere else,
		// and Pow of the index rather than repeated multiplication keeps the
		// last decade of a wide sweep from drifting.
		return FloatVal(base.Float() * math.Pow(s.AxisStep.Float(), float64(i)))
	}
	return FloatVal(base.Float() + explicit*s.AxisScale.Float())
}

// Run declares what a run_id means: one dataset within a stream, measured or
// simulated again under different conditions. A .step sweep, a Monte Carlo
// batch, a corner analysis.
type Run struct {
	ID     uint32
	Index  uint32 // ordinal within the sweep
	Params map[string]string
}

// Meta is a key/value record. Replaces MDF4's XML, which is invariably used as a
// flat dict anyway.
type Meta struct {
	Key   string
	Value string
}
