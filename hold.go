package logb

// Hold is a stream's held values as of a segment boundary: what a reader
// joining here would otherwise have had to guess.
//
// It exists because rule 3's promise — that a file cut anywhere still decodes —
// is only half true for a stream written on change. Schema is restated in every
// segment, so a reader landing mid-file knows that psu1.ch1.v.set exists and how
// its bits are laid out. It does not know the channel was moved to 12 V forty
// minutes ago, because the record that said so is in a segment it never saw.
// Schema is restated per segment; value is not. A HOLD frame restates it.
//
// A Hold is not a record and must not be plotted as one. It says "this value was
// already in force when the segment opened", and AxisBase says since when — a
// position that is normally before the segment, and is the difference between a
// step that starts at the left edge of a chart and one that starts where it
// really did.
type Hold struct {
	// Schema is the stream these values belong to.
	Schema *Schema

	// AxisBase is where on the axis the values were last written. It is an
	// axis value in the stream's own terms (§5), not a segment offset, and it
	// may precede the segment that carries the frame.
	AxisBase AxisVal

	// RunID is the run the values were written in, 0 if the stream has no runs.
	RunID uint32

	present []bool
	b       *Batch
}

// Has reports whether field f has a restated value.
//
// A held field with no value is the ordinary case at the start of a recording:
// a setpoint nobody has set yet. It is a gap, and a zero there would be a lie
// of exactly the kind §6.2 exists to prevent for guarded fields.
func (h *Hold) Has(f int) bool {
	return f >= 0 && f < len(h.present) && h.present[f]
}

// Raw returns field f's restated value without applying its conversion,
// or ErrFieldAbsent if the field has none.
func (h *Hold) Raw(f int) (any, error) {
	if f < 0 || f >= len(h.present) {
		return nil, ErrCorrupt
	}
	if !h.present[f] {
		return nil, ErrFieldAbsent
	}
	return h.b.Raw(0, f)
}

// Value returns field f's restated value with its conversion applied,
// or ErrFieldAbsent if the field has none.
func (h *Hold) Value(f int) (any, error) {
	if f < 0 || f >= len(h.present) {
		return nil, ErrCorrupt
	}
	if !h.present[f] {
		return nil, ErrFieldAbsent
	}
	return h.b.Value(0, f)
}

// decodeHold parses a HOLD frame payload against the schema it names.
//
// The payload is a presence bitmap followed by one record in the stream's own
// layout, so the record decodes through the ordinary path: a Batch of one. That
// is deliberate — a restated value that decoded by different rules than a
// written one would eventually disagree with it.
func decodeHold(s *Schema, payload []byte) (*Hold, error) {
	d := &dec{b: payload}
	h := &Hold{Schema: s}
	h.AxisBase = AxisVal(d.u64())
	h.RunID = d.u32()
	d.u32() // reserved
	bits := d.raw((len(s.Fields) + 7) / 8)
	if d.err != nil {
		return nil, d.err
	}

	h.present = make([]bool, len(s.Fields))
	for i := range h.present {
		h.present[i] = bits[i/8]&(1<<uint(i%8)) != 0
	}

	rec := payload[d.i:]
	fixed := s.RecordBytes()
	if len(rec) < fixed {
		return nil, ErrBadHold
	}
	h.b = &Batch{Schema: s, AxisBase: h.AxisBase, RunID: h.RunID, Count: 1, Data: rec}
	if nvar := s.varCount(); nvar > 0 {
		if err := h.b.parseTails(fixed, nvar); err != nil {
			return nil, err
		}
	}
	return h, nil
}
