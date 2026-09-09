package logb

import (
	"encoding/binary"
	"errors"
	"math"
)

// Errors returned only by Column. They are distinct from ErrCorrupt because
// none of them says the file is damaged: each says this field cannot be read
// this way, and Raw or Value will read it correctly.
var (
	// ErrColumnShape means the field is not a fixed-width, byte-aligned,
	// byte-sized slice of the record — a variable field, an unaligned one, or
	// a type with no column representation (bool, bytes, string, complex).
	ErrColumnShape = errors.New("logb: field is not a fixed-width byte-aligned column")

	// ErrColumnType means dst's element type is not the field's type. Column
	// never converts: a silent widening would put back exactly the per-element
	// cost the method exists to remove, and a narrowing would lose data.
	ErrColumnType = errors.New("logb: dst element type does not match the field")

	// ErrColumnGuarded means the field is guarded (§6.2), so a record may not
	// contain it at all. A plain slice has no way to say "absent", and writing
	// a zero there reproduces the bug guards exist to prevent. Read a guarded
	// field with Raw, which reports ErrFieldAbsent per record.
	ErrColumnGuarded = errors.New("logb: column of a guarded field; absence has no representation in a slice")
)

// ColumnType is the set of element types a column can be read into. The list is
// exact and closed on purpose — see ErrColumnType.
type ColumnType interface {
	int8 | uint8 | int16 | uint16 | int32 | uint32 | int64 | uint64 | float32 | float64
}

// Column copies field f of every record in the batch into dst, and returns how
// many elements it wrote.
//
// This is the bulk counterpart to Raw: no conversion is applied, and the values
// are the field's raw bits in the field's own type. A stream carrying ADC counts
// under a linear conversion yields the counts, which is what a plot wants — one
// scale applied to an axis beats a million applied to samples.
//
// It exists because Raw and Value are the wrong shape for a waveform. They box
// every value into an interface, so a one-megapoint acquisition costs a million
// interface conversions and the allocations behind them. A waveform is instead
// the case this format makes cheap: one fixed-width, byte-aligned field, whose
// records sit contiguously in Data once the frame is decompressed and
// de-filtered. Reading it is then a bounds check and a strided copy, with a byte
// swap where the field's order is not the machine's.
//
// dst may be shorter than the batch, in which case only len(dst) records are
// read; it may be longer, in which case the tail is untouched. Callers that
// want the whole batch size it with b.Count.
//
// The field must be fixed-width, byte-aligned, byte-sized, unguarded, and of
// exactly dst's element type. Anything else is refused rather than converted:
// see ErrColumnShape, ErrColumnType and ErrColumnGuarded. This is a free
// function rather than a method because Go methods cannot take type parameters.
func Column[T ColumnType](b *Batch, f int, dst []T) (int, error) {
	if b == nil || b.Schema == nil {
		return 0, ErrCorrupt
	}
	if f < 0 || f >= len(b.Schema.Fields) {
		return 0, ErrCorrupt
	}
	fd := &b.Schema.Fields[f]

	if fd.Guarded {
		return 0, ErrColumnGuarded
	}
	if fd.Variable || fd.BitOffset%8 != 0 || fd.BitWidth%8 != 0 {
		return 0, ErrColumnShape
	}
	if err := columnTypeOK[T](fd); err != nil {
		return 0, err
	}

	stride := b.Schema.RecordBytes()
	base := int(fd.BitOffset / 8)
	width := int(fd.BitWidth / 8)
	if stride <= 0 || base+width > stride {
		return 0, ErrColumnShape
	}

	n := int(b.Count)
	if len(dst) < n {
		n = len(dst)
	}
	if n == 0 {
		return 0, nil
	}
	// Bound against Data rather than trusting Count: with a tail region present
	// Data runs past the fixed records, and Count is a claim the frame made
	// about itself. Record(i) makes the same check one record at a time.
	if (n-1)*stride+base+width > len(b.Data) {
		return 0, ErrCorrupt
	}

	// A byte-aligned, byte-sized field is contiguous in both of §6.3's bit
	// numberings, so the only thing left of byte order is which end the bytes
	// read from — plain BigEndian or LittleEndian, no bit arithmetic.
	be := fd.BigEndian
	switch d := any(dst).(type) {
	case []uint8:
		copy8(d, b.Data, base, stride, n)
	case []int8:
		copy8(d, b.Data, base, stride, n)
	case []uint16:
		copy16(d, b.Data, base, stride, n, be)
	case []int16:
		copy16(d, b.Data, base, stride, n, be)
	case []uint32:
		copy32(d, b.Data, base, stride, n, be)
	case []int32:
		copy32(d, b.Data, base, stride, n, be)
	case []uint64:
		copy64(d, b.Data, base, stride, n, be)
	case []int64:
		copy64(d, b.Data, base, stride, n, be)
	case []float32:
		copyF32(d, b.Data, base, stride, n, be)
	case []float64:
		copyF64(d, b.Data, base, stride, n, be)
	default:
		return 0, ErrColumnType
	}
	return n, nil
}

// columnTypeOK reports whether T is exactly fd's type and width.
//
// Width is part of the match, not a detail: a 24-bit Motorola signal is an
// ordinary thing for a CAN schema to hold and has no Go type, so it belongs on
// the Raw path rather than being widened into an int32 here.
func columnTypeOK[T ColumnType](fd *Field) error {
	var zero T
	var want DataType
	var bits uint32
	switch any(zero).(type) {
	case uint8:
		want, bits = TypeUint, 8
	case uint16:
		want, bits = TypeUint, 16
	case uint32:
		want, bits = TypeUint, 32
	case uint64:
		want, bits = TypeUint, 64
	case int8:
		want, bits = TypeSint, 8
	case int16:
		want, bits = TypeSint, 16
	case int32:
		want, bits = TypeSint, 32
	case int64:
		want, bits = TypeSint, 64
	case float32:
		want, bits = TypeFloat, 32
	case float64:
		want, bits = TypeFloat, 64
	default:
		return ErrColumnType
	}
	switch fd.Type {
	case TypeUint, TypeSint, TypeFloat:
	default:
		// bool, bytes, string, complex. None has a slice representation that
		// carries what the value means.
		return ErrColumnShape
	}
	if fd.Type != want || fd.BitWidth != bits {
		return ErrColumnType
	}
	return nil
}

// The copy helpers are split by width rather than by Go type so that each byte
// order is chosen once, outside the loop, and the read is a concrete call the
// compiler can inline rather than a binary.ByteOrder interface dispatch per
// element.

func copy8[T int8 | uint8](d []T, b []byte, base, stride, n int) {
	for i := range d[:n] {
		d[i] = T(b[base+i*stride])
	}
}

func copy16[T int16 | uint16](d []T, b []byte, base, stride, n int, be bool) {
	if be {
		for i := range d[:n] {
			d[i] = T(binary.BigEndian.Uint16(b[base+i*stride:]))
		}
		return
	}
	for i := range d[:n] {
		d[i] = T(binary.LittleEndian.Uint16(b[base+i*stride:]))
	}
}

func copy32[T int32 | uint32](d []T, b []byte, base, stride, n int, be bool) {
	if be {
		for i := range d[:n] {
			d[i] = T(binary.BigEndian.Uint32(b[base+i*stride:]))
		}
		return
	}
	for i := range d[:n] {
		d[i] = T(binary.LittleEndian.Uint32(b[base+i*stride:]))
	}
}

func copy64[T int64 | uint64](d []T, b []byte, base, stride, n int, be bool) {
	if be {
		for i := range d[:n] {
			d[i] = T(binary.BigEndian.Uint64(b[base+i*stride:]))
		}
		return
	}
	for i := range d[:n] {
		d[i] = T(binary.LittleEndian.Uint64(b[base+i*stride:]))
	}
}

func copyF32(d []float32, b []byte, base, stride, n int, be bool) {
	if be {
		for i := range d[:n] {
			d[i] = math.Float32frombits(binary.BigEndian.Uint32(b[base+i*stride:]))
		}
		return
	}
	for i := range d[:n] {
		d[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[base+i*stride:]))
	}
}

func copyF64(d []float64, b []byte, base, stride, n int, be bool) {
	if be {
		for i := range d[:n] {
			d[i] = math.Float64frombits(binary.BigEndian.Uint64(b[base+i*stride:]))
		}
		return
	}
	for i := range d[:n] {
		d[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[base+i*stride:]))
	}
}
