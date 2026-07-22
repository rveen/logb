package mdf

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode/utf16"
)

// Decoding a channel out of a record, by MDF's own rules.
//
// The importer needs this for exactly one channel — the master, whose seconds
// have to be known before they can become ticks — and everything else it copies
// without looking. It is written out in full anyway, because a reader that can
// only decode the column it happens to need is not a reader, and because the
// conversion's test compares what this produces against what Logb produces from
// the converted file. Two implementations written from two specifications
// agreeing on every sample of every fixture is worth more than one
// implementation agreeing with itself.

// Raw returns the channel's stored value in record rec, before conversion.
//
// Integers come back as uint64 or int64, floats as float64, text as string, and
// anything else as a []byte aliasing rec.
func (c *Channel) Raw(rec []byte) (any, error) {
	if c.Kind == VLSD {
		return nil, fmt.Errorf("mdf: %q is variable-length; its samples are in Group.VLSD", c.Name)
	}
	off := int(c.ByteOffset)
	n := (int(c.BitOffset) + int(c.BitCount) + 7) / 8
	if off < 0 || off+n > len(rec) {
		return nil, fmt.Errorf("mdf: channel %q needs bytes %d..%d of a %d-byte record",
			c.Name, off, off+n, len(rec))
	}
	b := rec[off : off+n]

	switch c.DataType {
	case DTStringLatin1, DTStringUTF8:
		s := string(b)
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i]
		}
		return s, nil
	case DTStringUTF16LE:
		return decodeUTF16(b, binary.LittleEndian), nil
	case DTStringUTF16BE:
		return decodeUTF16(b, binary.BigEndian), nil
	case DTBytes, DTMimeSample, DTMimeStream, DTCANopenDate, DTCANopenTime:
		return b, nil
	}

	// Numeric. Whole bytes on a byte boundary are the common case and are read
	// directly; anything else is assembled bit by bit.
	var bits uint64
	if c.BitOffset == 0 && c.BitCount%8 == 0 {
		if c.BigEndian() {
			for _, x := range b {
				bits = bits<<8 | uint64(x)
			}
		} else {
			for i := len(b) - 1; i >= 0; i-- {
				bits = bits<<8 | uint64(b[i])
			}
		}
	} else {
		var err error
		if bits, err = bitsOf(b, c.BitOffset, c.BitCount, c.BigEndian()); err != nil {
			return nil, fmt.Errorf("mdf: channel %q: %w", c.Name, err)
		}
	}

	switch c.DataType {
	case DTUintLE, DTUintBE:
		return bits, nil
	case DTIntLE, DTIntBE:
		return signExtend(bits, c.BitCount), nil
	case DTFloatLE, DTFloatBE:
		switch c.BitCount {
		case 32:
			return float64(math.Float32frombits(uint32(bits))), nil
		case 64:
			return math.Float64frombits(bits), nil
		}
		return nil, fmt.Errorf("mdf: channel %q is a %d-bit float", c.Name, c.BitCount)
	}
	return b, nil
}

// Value returns the channel's physical value: Raw with the conversion applied.
// A channel whose conversion had no Logb equivalent returns its raw value, which
// is what a conversion-free channel returns too.
func (c *Channel) Value(rec []byte) (any, error) {
	v, err := c.Raw(rec)
	if err != nil || c.Conv == nil || c.Conv.Conv == nil {
		return v, err
	}
	f, ok := asFloat(v)
	if !ok {
		return v, nil
	}
	return c.Conv.Conv.Apply(f), nil
}

// Float returns the channel's physical value as a number. It is how the
// importer reads a master channel, which may be a float, an integer, or an
// integer that only becomes seconds once its linear conversion is applied.
func (c *Channel) Float(rec []byte) (float64, error) {
	v, err := c.Value(rec)
	if err != nil {
		return 0, err
	}
	f, ok := asFloat(v)
	if !ok {
		return 0, fmt.Errorf("mdf: channel %q is %T, not a number", c.Name, v)
	}
	return f, nil
}

// Invalid reports whether the record marks this channel's sample as invalid.
// MDF stores that as a bit in the invalidation bytes that follow the record;
// Logb says the same thing with a guarded field (§6.2).
func (c *Channel) Invalid(rec []byte, recordBytes int) bool {
	if !c.HasInvalBit {
		return false
	}
	i := recordBytes + int(c.InvalBit/8)
	if i >= len(rec) {
		return false
	}
	return rec[i]&(1<<(c.InvalBit%8)) != 0
}

// bitsOf extracts bitCount bits starting at bitOffset within b.
//
// Little-endian channels number bits from the least significant bit of the
// first byte upward; big-endian ones from the most significant bit downward.
// This is the rule CAN.md §"bit numbering" spells out, and it is why a signal
// that crosses a byte boundary has one answer here and an argument everywhere
// else.
func bitsOf(b []byte, bitOffset, bitCount uint32, bigEndian bool) (uint64, error) {
	if bitCount == 0 || bitCount > 64 {
		return 0, fmt.Errorf("%d bits is not a value", bitCount)
	}
	if uint64(bitOffset)+uint64(bitCount) > uint64(len(b))*8 {
		return 0, fmt.Errorf("%d bits at bit %d need more than %d bytes", bitCount, bitOffset, len(b))
	}
	var v uint64
	if bigEndian {
		for i := uint32(0); i < bitCount; i++ {
			p := bitOffset + i
			v = v<<1 | uint64(b[p/8]>>(7-p%8))&1
		}
		return v, nil
	}
	for i := uint32(0); i < bitCount; i++ {
		p := bitOffset + i
		v |= (uint64(b[p/8]>>(p%8)) & 1) << i
	}
	return v, nil
}

func signExtend(v uint64, bits uint32) int64 {
	if bits >= 64 {
		return int64(v)
	}
	shift := 64 - bits
	return int64(v<<shift) >> shift
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

func decodeUTF16(b []byte, bo binary.ByteOrder) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = bo.Uint16(b[2*i:])
	}
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	return string(utf16.Decode(u))
}
