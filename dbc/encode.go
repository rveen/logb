package dbc

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Message finds a message by name, or returns nil.
func (f *File) Message(name string) *Message {
	for _, m := range f.Messages {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// Encode builds the payload of one frame of m from physical signal values.
//
// It is decoding through Schema run backwards, and built to be: each signal is
// written at its BitOffset, in Logb's numbering and with the byte order its
// field declares, so the bits land exactly where a reader of the recorded frame
// looks for them. A Motorola signal needs nothing more than that, for the reason
// BitOffset gives.
//
// values maps a signal's name to its physical value or to a name from its value
// table. A number is scaled back to raw as (value - offset) / factor and rounded
// to the nearest step the signal can represent; a name is looked up. A signal
// that is not named takes the database's GenSigStartValue, and one the database
// gives no start value must be named — nothing goes into the frame that neither
// the request nor the database stated. A multiplexed message carries the
// signals its multiplexor's value selects, and only those.
//
// What would otherwise be adjusted is refused instead: a value outside the
// range the database declares for the signal, or one its bits cannot hold; a
// name its value table does not have; a signal the message does not have, or
// one this frame does not carry under the multiplexor value chosen; and a
// message multiplexed on more than one level, which Schema refuses too, so that
// no frame is sent that no recording of it could decode.
func (m *Message) Encode(values map[string]any) ([]byte, error) {
	if m.Length <= 0 || m.Length > 64 {
		return nil, fmt.Errorf("dbc: message %q has a length of %d bytes", m.Name, m.Length)
	}
	var mux *Signal
	for _, s := range m.Signals {
		if s.ExtendedMux || (s.Multiplexor && mux != nil) {
			return nil, fmt.Errorf("dbc: message %q multiplexes on more than one level, "+
				"which a recording could not decode (SPEC §6.2)", m.Name)
		}
		if s.Multiplexor {
			mux = s
		}
	}
	for name := range values {
		if !slices.ContainsFunc(m.Signals, func(s *Signal) bool { return s.Name == name }) {
			return nil, fmt.Errorf("dbc: message %q has no signal %q", m.Name, name)
		}
	}

	var muxRaw uint64
	if mux != nil {
		r, ok, err := rawValue(mux, values)
		switch {
		case err != nil:
			return nil, fmt.Errorf("dbc: message %q: %w", m.Name, err)
		case !ok:
			return nil, fmt.Errorf("dbc: message %q: its multiplexor %q selects which signals the frame carries, "+
				"and has neither a value nor a start value", m.Name, mux.Name)
		}
		muxRaw = r
	}

	data := make([]byte, m.Length)
	var missing []string
	for _, s := range m.Signals {
		if s.Muxed && (mux == nil || s.MuxValue != muxRaw) {
			if _, given := values[s.Name]; given {
				if mux == nil {
					return nil, fmt.Errorf("dbc: message %q: signal %q is multiplexed, and the message has no multiplexor",
						m.Name, s.Name)
				}
				return nil, fmt.Errorf("dbc: message %q: signal %q is carried when %s is %d, and this frame's %s is %d",
					m.Name, s.Name, mux.Name, s.MuxValue, mux.Name, muxRaw)
			}
			continue
		}
		r, ok, err := rawValue(s, values)
		if err != nil {
			return nil, fmt.Errorf("dbc: message %q: %w", m.Name, err)
		}
		if !ok {
			missing = append(missing, s.Name)
			continue
		}
		if err := put(data, s, r); err != nil {
			return nil, fmt.Errorf("dbc: message %q: %w", m.Name, err)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("dbc: message %q: no value for %s, and the database gives no start value to fall back on",
			m.Name, strings.Join(missing, ", "))
	}
	return data, nil
}

// rawValue is the raw bits for s: from the value it was given, or from its
// start value. ok is false when there is neither.
func rawValue(s *Signal, values map[string]any) (raw uint64, ok bool, err error) {
	v, given := values[s.Name]
	if !given {
		if !s.HasInitial {
			return 0, false, nil
		}
		// GenSigStartValue is raw already: it is not scaled, only checked.
		raw, err := fitRaw(s, s.Initial)
		if err != nil {
			return 0, false, fmt.Errorf("signal %q: start value: %w", s.Name, err)
		}
		return raw, true, nil
	}

	var phys float64
	switch x := v.(type) {
	case string:
		// Sorted, so that a table naming two values alike is answered the same
		// way every time.
		keys := make([]uint64, 0, len(s.Values))
		for k := range s.Values {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if s.Values[k] == x {
				raw, err := fitRaw(s, float64(k))
				if err != nil {
					return 0, false, fmt.Errorf("signal %q: %q: %w", s.Name, x, err)
				}
				return raw, true, nil
			}
		}
		if len(s.Values) == 0 {
			return 0, false, fmt.Errorf("signal %q has no value table to find %q in", s.Name, x)
		}
		return 0, false, fmt.Errorf("signal %q has no value named %q", s.Name, x)
	case float64:
		phys = x
	case float32:
		phys = float64(x)
	case int:
		phys = float64(x)
	case int64:
		phys = float64(x)
	case uint64:
		phys = float64(x)
	case bool:
		// A one-bit flag decodes as a bool, and is given as one.
		if x {
			phys = 1*s.Factor + s.Offset
		} else {
			phys = s.Offset
		}
	default:
		return 0, false, fmt.Errorf("signal %q: a value is a number or a name from its table, not %T", s.Name, v)
	}

	if math.IsNaN(phys) || math.IsInf(phys, 0) {
		return 0, false, fmt.Errorf("signal %q: %v is not a value", s.Name, phys)
	}
	// [0|0] is DBC for "no range declared", not a range that admits only zero.
	if s.Min != 0 || s.Max != 0 {
		tol := 1e-9 * math.Max(1, math.Max(math.Abs(s.Min), math.Abs(s.Max)))
		if phys < s.Min-tol || phys > s.Max+tol {
			return 0, false, fmt.Errorf("signal %q: %g%s is outside the range the database declares, [%g, %g]",
				s.Name, phys, unit(s), s.Min, s.Max)
		}
	}
	if s.Factor == 0 {
		return 0, false, fmt.Errorf("signal %q has a factor of zero, so no value can be encoded", s.Name)
	}
	r := (phys - s.Offset) / s.Factor
	if !s.Float {
		r = math.Round(r)
	}
	raw, err = fitRaw(s, r)
	if err != nil {
		return 0, false, fmt.Errorf("signal %q: %g%s: %w", s.Name, phys, unit(s), err)
	}
	return raw, true, nil
}

// fitRaw turns a raw value into the signal's bits, refusing one they cannot
// hold rather than truncating it into a different value.
func fitRaw(s *Signal, r float64) (uint64, error) {
	if s.Float {
		switch s.Length {
		case 32:
			if math.Abs(r) > math.MaxFloat32 {
				return 0, fmt.Errorf("raw %g does not fit a 32-bit float", r)
			}
			return uint64(math.Float32bits(float32(r))), nil
		case 64:
			return math.Float64bits(r), nil
		}
		return 0, fmt.Errorf("a float signal is 32 or 64 bits, not %d", s.Length)
	}
	if r != math.Trunc(r) {
		return 0, fmt.Errorf("raw %g is not a whole number", r)
	}
	n := s.Length
	if n <= 0 || n > 64 {
		return 0, fmt.Errorf("a signal of %d bits", n)
	}
	if s.Signed {
		lo, hi := -math.Ldexp(1, n-1), math.Ldexp(1, n-1)
		if r < lo || r >= hi {
			return 0, fmt.Errorf("raw %g does not fit %d signed bits, which hold %g to %g", r, n, lo, hi-1)
		}
		return uint64(int64(r)) & mask(n), nil
	}
	if r < 0 || r >= math.Ldexp(1, n) {
		return 0, fmt.Errorf("raw %g does not fit %d unsigned bits, which hold 0 to %g", r, n, math.Ldexp(1, n)-1)
	}
	return uint64(r), nil
}

func mask(n int) uint64 {
	if n >= 64 {
		return ^uint64(0)
	}
	return uint64(1)<<n - 1
}

// put writes raw into a signal's bits: from its BitOffset, in its byte order.
// Those are the position and the order the decoder reads, so a frame this
// writes decodes to the value it was given; the round-trip tests hold it to
// that through the reader itself.
func put(data []byte, s *Signal, raw uint64) error {
	off, n := s.BitOffset(), uint32(s.Length)
	if n == 0 || n > 64 || uint64(off)+uint64(n) > uint64(len(data))*8 {
		return fmt.Errorf("signal %q, %d bits from bit %d, does not fit a %d-byte frame", s.Name, n, off, len(data))
	}
	if s.BigEndian {
		// Most significant bit first, upward through Logb's big-endian
		// numbering, in which bit p is bit 7-p%8 of byte p/8.
		for i := uint32(0); i < n; i++ {
			p := off + i
			b := byte(raw>>(n-1-i)) & 1
			data[p/8] = data[p/8]&^(1<<(7-p%8)) | b<<(7-p%8)
		}
		return nil
	}
	// Least significant bit first, upward from the offset, in which bit p is
	// bit p%8 of byte p/8.
	for i := uint32(0); i < n; i++ {
		p := off + i
		b := byte(raw>>i) & 1
		data[p/8] = data[p/8]&^(1<<(p%8)) | b<<(p%8)
	}
	return nil
}

func unit(s *Signal) string {
	if s.Unit == "" {
		return ""
	}
	return " " + s.Unit
}
