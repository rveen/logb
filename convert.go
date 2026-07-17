package logb

import (
	"math"
)

// Conversion turns a stored raw value into a physical one.
//
// Every conversion is closed-form and decodable without a parser. MDF4's
// algebraic conversion — an embedded text formula the reader must evaluate — is
// deliberately absent: it is a scripting language smuggled into a data format
// and a code execution surface in a file handed to you by a third party.
type Conversion interface {
	// Apply converts a raw value. It returns float64 for numeric conversions and
	// string for the text ones, which is why it returns any.
	Apply(raw float64) any
	kind() uint8
}

const (
	convIdentity uint8 = iota
	convLinear
	convRational
	convTable
	convTableInterp
	convValueToText
	convRangeToText
)

// Identity leaves the raw value alone.
type Identity struct{}

func (Identity) kind() uint8           { return convIdentity }
func (Identity) Apply(raw float64) any { return raw }

// Linear computes A + B*raw.
type Linear struct{ A, B float64 }

func (Linear) kind() uint8             { return convLinear }
func (c Linear) Apply(raw float64) any { return c.A + c.B*raw }

// Rational computes (p1x² + p2x + p3) / (p4x² + p5x + p6).
type Rational struct{ P [6]float64 }

func (Rational) kind() uint8 { return convRational }
func (c Rational) Apply(raw float64) any {
	x := raw
	num := c.P[0]*x*x + c.P[1]*x + c.P[2]
	den := c.P[3]*x*x + c.P[4]*x + c.P[5]
	return num / den
}

// Table looks raw up in a key/value table. Interp selects linear interpolation
// between keys; without it, the nearest key at or below raw wins.
type Table struct {
	Keys   []float64
	Vals   []float64
	Interp bool
}

func (c Table) kind() uint8 {
	if c.Interp {
		return convTableInterp
	}
	return convTable
}

func (c Table) Apply(raw float64) any {
	if len(c.Keys) == 0 {
		return raw
	}
	if raw <= c.Keys[0] {
		return c.Vals[0]
	}
	last := len(c.Keys) - 1
	if raw >= c.Keys[last] {
		return c.Vals[last]
	}
	i := 0
	for i < last && c.Keys[i+1] <= raw {
		i++
	}
	if !c.Interp {
		return c.Vals[i]
	}
	span := c.Keys[i+1] - c.Keys[i]
	if span == 0 {
		return c.Vals[i]
	}
	t := (raw - c.Keys[i]) / span
	return c.Vals[i] + t*(c.Vals[i+1]-c.Vals[i])
}

// ValueToText decodes an enumeration.
type ValueToText struct {
	Keys    []float64
	Texts   []string
	Default string
}

func (ValueToText) kind() uint8 { return convValueToText }
func (c ValueToText) Apply(raw float64) any {
	for i, k := range c.Keys {
		if k == raw {
			return c.Texts[i]
		}
	}
	return c.Default
}

// RangeToText decodes a value into the text of the first range containing it.
type RangeToText struct {
	Los     []float64
	His     []float64
	Texts   []string
	Default string
}

func (RangeToText) kind() uint8 { return convRangeToText }
func (c RangeToText) Apply(raw float64) any {
	for i := range c.Los {
		if raw >= c.Los[i] && raw <= c.His[i] {
			return c.Texts[i]
		}
	}
	return c.Default
}

// extractBits pulls a field's raw bits out of a record.
//
// Bit numbering follows the byte order (SPEC.md §6.2). Both orders name the
// field's first bit with bit_offset and run bit_width bits upward from it; they
// differ only in where a byte's bits start being counted:
//
//	little-endian: bit n is byte n/8, bit n%8 counting from the LSB
//	big-endian:    bit n is byte n/8, bit n%8 counting from the MSB
//
// Each order is contiguous in its own numbering, which is what makes the rule
// unconditional — there is no unaligned special case, and no jump. The
// big-endian half is exactly DBC's Motorola convention; TestDBCMotorola proves
// it against the reference algorithm.
func extractBits(rec []byte, bitOff, bitWidth uint32, bigEndian bool) (uint64, error) {
	if bitWidth == 0 || bitWidth > 64 {
		return 0, ErrCorrupt
	}
	if uint64(bitOff)+uint64(bitWidth) > uint64(len(rec))*8 {
		return 0, ErrCorrupt
	}

	if bigEndian {
		// Whole bytes from a byte boundary: the same bits, read a byte at a
		// time. Identical to the general loop below, which TestBitOrderPaths
		// checks at every offset and width.
		if bitOff%8 == 0 && bitWidth%8 == 0 {
			var v uint64
			start := bitOff / 8
			for i := uint32(0); i < bitWidth/8; i++ {
				v = v<<8 | uint64(rec[start+i])
			}
			return v, nil
		}
		var v uint64
		for i := uint32(0); i < bitWidth; i++ {
			p := bitOff + i
			v = v<<1 | uint64(rec[p/8]>>(7-p%8))&1
		}
		return v, nil
	}

	var v uint64
	var got uint32
	byteIdx := bitOff / 8
	bitIdx := bitOff % 8
	for got < bitWidth {
		chunk := uint64(rec[byteIdx]) >> bitIdx
		v |= chunk << got
		got += 8 - bitIdx
		byteIdx++
		bitIdx = 0
	}
	if bitWidth < 64 {
		v &= (uint64(1) << bitWidth) - 1
	}
	return v, nil
}

// signExtend widens a bitWidth-bit two's complement value to int64.
func signExtend(v uint64, bitWidth uint32) int64 {
	if bitWidth >= 64 {
		return int64(v)
	}
	shift := 64 - bitWidth
	return int64(v<<shift) >> shift
}

// rawValue decodes a field's stored value without applying its conversion.
//
// It handles the fixed portion only. A variable-length field's bytes live in the
// tail and are resolved by Batch.Raw, which has the tail; this function is not
// given one and refuses such a field rather than guessing.
//
// Returned bytes and strings for fixed blob fields alias rec, and therefore
// alias Batch.Data: they are not copied, so that adding a payload field costs an
// unrelated channel nothing (rule 6). A caller that retains them past the batch
// must copy.
func rawValue(rec []byte, f *Field) (any, error) {
	if f.Variable {
		return nil, ErrBadVariableField
	}
	switch f.Type {
	case TypeBytes, TypeString:
		// A blob is a slice of whole bytes; Schema.Validate refuses any other
		// shape, and this repeats the check because a record may arrive from a
		// file whose writer never ran Validate.
		if f.BitOffset%8 != 0 || f.BitWidth%8 != 0 {
			return nil, ErrUnalignedBlob
		}
		off, n := int(f.BitOffset/8), int(f.BitWidth/8)
		if off+n > len(rec) {
			return nil, ErrCorrupt
		}
		if f.Type == TypeString {
			return string(rec[off : off+n]), nil
		}
		return rec[off : off+n], nil

	case TypeBool:
		// A single bit still has a numbering: bit 37 of a Motorola payload is
		// not bit 37 of an Intel one, so the field's byte order selects which.
		v, err := extractBits(rec, f.BitOffset, 1, f.BigEndian)
		return v != 0, err

	case TypeUint:
		v, err := extractBits(rec, f.BitOffset, f.BitWidth, f.BigEndian)
		return v, err

	case TypeSint:
		v, err := extractBits(rec, f.BitOffset, f.BitWidth, f.BigEndian)
		if err != nil {
			return nil, err
		}
		return signExtend(v, f.BitWidth), nil

	case TypeFloat:
		v, err := extractBits(rec, f.BitOffset, f.BitWidth, f.BigEndian)
		if err != nil {
			return nil, err
		}
		switch f.BitWidth {
		case 32:
			return float64(math.Float32frombits(uint32(v))), nil
		case 64:
			return math.Float64frombits(v), nil
		default:
			return nil, ErrCorrupt
		}

	case TypeComplex:
		// Real and imaginary parts are one quantity sharing a name, a unit, and a
		// conversion — which is why this is a type and not two adjacent fields.
		half := f.BitWidth / 2
		re, err := extractBits(rec, f.BitOffset, half, f.BigEndian)
		if err != nil {
			return nil, err
		}
		im, err := extractBits(rec, f.BitOffset+half, half, f.BigEndian)
		if err != nil {
			return nil, err
		}
		switch half {
		case 32:
			return complex(float64(math.Float32frombits(uint32(re))),
				float64(math.Float32frombits(uint32(im)))), nil
		case 64:
			return complex(math.Float64frombits(re), math.Float64frombits(im)), nil
		default:
			return nil, ErrCorrupt
		}
	}
	return nil, ErrCorrupt
}

// toFloat coerces a decoded numeric value for conversion input.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case uint64:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}
