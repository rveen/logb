package logb

import (
	"encoding/binary"
	"math"
	"sort"
)

// buf is a little-endian append-only encoder. All multi-byte integers in Logb are
// little-endian unless a field declares otherwise.
type buf struct{ b []byte }

func (e *buf) u8(v uint8)    { e.b = append(e.b, v) }
func (e *buf) i8(v int8)     { e.b = append(e.b, byte(v)) }
func (e *buf) u16(v uint16)  { e.b = binary.LittleEndian.AppendUint16(e.b, v) }
func (e *buf) u32(v uint32)  { e.b = binary.LittleEndian.AppendUint32(e.b, v) }
func (e *buf) u64(v uint64)  { e.b = binary.LittleEndian.AppendUint64(e.b, v) }
func (e *buf) i64(v int64)   { e.u64(uint64(v)) }
func (e *buf) f64(v float64) { e.u64(math.Float64bits(v)) }
func (e *buf) raw(v []byte)  { e.b = append(e.b, v...) }

// str writes a u32-prefixed UTF-8 string. Not NUL-terminated.
func (e *buf) str(s string) {
	e.u32(uint32(len(s)))
	e.b = append(e.b, s...)
}

// kv writes a key/value block with its keys sorted.
//
// Sorted because Go randomises map iteration, and without this a writer handed
// identical input would emit different bytes on every run. That would make the
// file unreproducible: no golden fixture, no content hash, no byte-comparing two
// recordings to see whether anything actually changed. The order carries no
// meaning to a reader — it is chosen only so that it is always the same one.
func (e *buf) kv(m map[string]string) {
	e.u32(uint32(len(m)))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e.str(k)
		e.str(m[k])
	}
}

func (e *buf) conv(c Conversion) {
	if c == nil {
		c = Identity{}
	}
	e.u8(c.kind())
	switch t := c.(type) {
	case Identity:
	case Linear:
		e.f64(t.A)
		e.f64(t.B)
	case Rational:
		for _, p := range t.P {
			e.f64(p)
		}
	case Table:
		e.u32(uint32(len(t.Keys)))
		for i := range t.Keys {
			e.f64(t.Keys[i])
			e.f64(t.Vals[i])
		}
	case ValueToText:
		e.u32(uint32(len(t.Keys)))
		for i := range t.Keys {
			e.f64(t.Keys[i])
			e.str(t.Texts[i])
		}
		e.str(t.Default)
	case RangeToText:
		e.u32(uint32(len(t.Los)))
		for i := range t.Los {
			e.f64(t.Los[i])
			e.f64(t.His[i])
			e.str(t.Texts[i])
		}
		e.str(t.Default)
	}
}

// dec is a little-endian decoder over a frame payload. It records the first
// error and yields zeroes thereafter, so a caller checks err once at the end.
type dec struct {
	b   []byte
	i   int
	err error
}

// need reports whether n more bytes are available. n < 0 is treated as damage
// rather than trusted: a u32 length from a hostile or corrupt file becomes a
// negative int on a 32-bit build, and without this test the slice below it would
// pass the bounds check and panic.
func (d *dec) need(n int) bool {
	if d.err != nil {
		return false
	}
	if n < 0 || d.i+n > len(d.b) {
		d.err = ErrCorrupt
		return false
	}
	return true
}

func (d *dec) u8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.b[d.i]
	d.i++
	return v
}

func (d *dec) i8() int8 { return int8(d.u8()) }

func (d *dec) u16() uint16 {
	if !d.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(d.b[d.i:])
	d.i += 2
	return v
}

func (d *dec) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(d.b[d.i:])
	d.i += 4
	return v
}

func (d *dec) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b[d.i:])
	d.i += 8
	return v
}

func (d *dec) i64() int64   { return int64(d.u64()) }
func (d *dec) f64() float64 { return math.Float64frombits(d.u64()) }

func (d *dec) raw(n int) []byte {
	if !d.need(n) {
		return nil
	}
	v := d.b[d.i : d.i+n]
	d.i += n
	return v
}

func (d *dec) str() string {
	n := int(d.u32())
	if !d.need(n) {
		return ""
	}
	v := string(d.b[d.i : d.i+n])
	d.i += n
	return v
}

func (d *dec) kv() map[string]string {
	n := int(d.u32())
	if d.err != nil || n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	for i := 0; i < n && d.err == nil; i++ {
		k := d.str()
		m[k] = d.str()
	}
	return m
}

func (d *dec) conv() Conversion {
	switch d.u8() {
	case convIdentity:
		return Identity{}
	case convLinear:
		return Linear{A: d.f64(), B: d.f64()}
	case convRational:
		var c Rational
		for i := range c.P {
			c.P[i] = d.f64()
		}
		return c
	case convTable, convTableInterp:
		return d.table(false)
	case convValueToText:
		n := int(d.u32())
		c := ValueToText{}
		for i := 0; i < n && d.err == nil; i++ {
			c.Keys = append(c.Keys, d.f64())
			c.Texts = append(c.Texts, d.str())
		}
		c.Default = d.str()
		return c
	case convRangeToText:
		n := int(d.u32())
		c := RangeToText{}
		for i := 0; i < n && d.err == nil; i++ {
			c.Los = append(c.Los, d.f64())
			c.His = append(c.His, d.f64())
			c.Texts = append(c.Texts, d.str())
		}
		c.Default = d.str()
		return c
	}
	d.err = ErrCorrupt
	return Identity{}
}

func (d *dec) table(interp bool) Conversion {
	n := int(d.u32())
	c := Table{Interp: interp}
	for i := 0; i < n && d.err == nil; i++ {
		c.Keys = append(c.Keys, d.f64())
		c.Vals = append(c.Vals, d.f64())
	}
	return c
}

// transpose groups byte i of every record together. MDF4's one genuinely good
// idea: columnar locality on a row-major layout for twenty lines of code.
func transpose(data []byte, recSize int) []byte {
	if recSize <= 1 || len(data)%recSize != 0 {
		return data
	}
	n := len(data) / recSize
	out := make([]byte, len(data))
	k := 0
	for c := 0; c < recSize; c++ {
		for r := 0; r < n; r++ {
			out[k] = data[r*recSize+c]
			k++
		}
	}
	return out
}

func detranspose(data []byte, recSize int) []byte {
	if recSize <= 1 || len(data)%recSize != 0 {
		return data
	}
	n := len(data) / recSize
	out := make([]byte, len(data))
	k := 0
	for c := 0; c < recSize; c++ {
		for r := 0; r < n; r++ {
			out[r*recSize+c] = data[k]
			k++
		}
	}
	return out
}
