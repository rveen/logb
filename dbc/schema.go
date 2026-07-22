package dbc

import (
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/internal/tick"
)

// AxisBits is the space a decoded stream leaves in front of the payload for its
// axis: an i64 tick count, as everywhere else in this repository. Signals are
// declared past it, so a record is the axis followed by the frame's own bytes,
// unaltered.
const AxisBits = 64

// SchemaOptions controls Schema.
type SchemaOptions struct {
	// Namespace makes stream UUIDs deterministic and distinct between
	// recordings. Two imports of the same message from the same recording
	// should produce the same stream; two different recordings should not.
	Namespace string

	// Database names the file these definitions came from, for the schema's
	// metadata. A decoded stream should say what decoded it.
	Database string

	// AxisExp is the tick size of the time axis, as a power of ten seconds.
	AxisExp int8

	// Warn, if set, is told about anything the mapping could not carry across.
	Warn func(format string, a ...any)
}

// Schema turns a DBC message into a Logb stream schema.
//
// The record is the axis followed by the frame's payload **as received**: every
// signal keeps the position the database gives it, converted only in the sense
// that a Motorola signal's start bit is restated in Logb's big-endian numbering,
// which is the same numbering said in a way that does not need a walking rule
// (CAN.md). Nothing is shifted, masked, or scaled at import — a reader decodes
// the signal from the bytes the bus carried, which is the only way the value it
// reports can be checked against a trace.
//
// Multiplexing becomes §6.2 guards. A multiplexed DBC signal is the case that
// section was written for: the same bits mean different things in different
// frames, and a reader given the overlapping declarations alone would decode
// every variant and return plausible garbage for all but one.
func Schema(m *Message, o SchemaOptions) (*logb.Schema, error) {
	if o.Warn == nil {
		o.Warn = func(string, ...any) {}
	}
	length := m.Length
	if length <= 0 || length > 64 {
		return nil, fmt.Errorf("dbc: message %q has a length of %d bytes", m.Name, m.Length)
	}

	id := fmt.Sprintf("0x%X", m.ID)
	s := &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte(o.Namespace+"/dbc/"+id+"/"+m.Name)),
		Name:       m.Name,
		RecordBits: AxisBits + uint32(length)*8,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    o.AxisExp,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Meta: map[string]string{
			"can.id":      id,
			"can.frame":   frameKind(m.Extended),
			"can.length":  fmt.Sprint(length),
			"dbc.message": m.Name,
		},
	}
	if o.Database != "" {
		s.Meta["dbc.database"] = o.Database
	}
	if m.Sender != "" && m.Sender != "Vector__XXX" {
		s.Meta["can.sender"] = m.Sender
	}
	if m.Desc != "" {
		s.Meta["dbc.comment"] = m.Desc
	}

	s.Fields = append(s.Fields, logb.Field{
		Name:      "t",
		BitOffset: 0,
		BitWidth:  64,
		Type:      logb.TypeSint,
		Unit:      tick.Unit(o.AxisExp),
		Desc:      "frame timestamp",
		Meta:      map[string]string{"axis.ticks": "true"},
	})

	// The multiplexor must be declared before anything guards on it, and its
	// index is what the guard names. It is an ordinary signal otherwise.
	//
	// More than one multiplexor means the message is multiplexed on two levels,
	// and the whole message is refused rather than half-mapped: guarding the
	// second level's signals against the first level's selector would decode
	// them in frames that do not carry them, which is the precise failure §6.2
	// exists to prevent. A schema that is wrong about which bits are live is
	// worse than no schema, because it produces numbers.
	var mux *Signal
	for _, s := range m.Signals {
		if !s.Multiplexor {
			continue
		}
		if mux != nil || s.ExtendedMux {
			first := s.Name
			if mux != nil {
				first = mux.Name
			}
			return nil, fmt.Errorf("dbc: message %q multiplexes on more than one level (%q, then %q); "+
				"Logb guards do not chain (SPEC §6.2)", m.Name, first, s.Name)
		}
		mux = s
	}
	muxField := -1
	if mux != nil {
		f, err := field(mux, length)
		if err != nil {
			return nil, err
		}
		muxField = len(s.Fields)
		f.Desc = describe(mux, "multiplexor: selects which signals this frame carries")
		s.Fields = append(s.Fields, f)
	}

	for _, sg := range m.Signals {
		if sg == mux {
			continue
		}
		if sg.ExtendedMux {
			// Refused rather than decoded unguarded: a signal read out of a
			// frame that does not carry it produces a number, and a number that
			// is wrong is worse than one that is missing.
			o.Warn("message %s: signal %q uses extended multiplexing, which Logb cannot express; it is left out",
				m.Name, sg.Name)
			continue
		}
		f, err := field(sg, length)
		if err != nil {
			o.Warn("message %s: %v", m.Name, err)
			continue
		}
		if sg.Muxed {
			if muxField < 0 {
				o.Warn("message %s: signal %q is multiplexed but the message has no multiplexor; it is left out",
					m.Name, sg.Name)
				continue
			}
			f.Guarded = true
			f.GuardField = uint16(muxField)
			f.GuardValue = sg.MuxValue
		}
		s.Fields = append(s.Fields, f)
	}
	return s, s.Validate()
}

// field maps one signal onto a Logb field.
func field(sg *Signal, length int) (logb.Field, error) {
	f := logb.Field{
		Name:      sg.Name,
		BitOffset: AxisBits + sg.BitOffset(),
		BitWidth:  uint32(sg.Length),
		BigEndian: sg.BigEndian,
		Unit:      sg.Unit,
		Desc:      sg.Desc,
		Meta:      map[string]string{},
	}
	if sg.Length <= 0 || sg.Length > 64 {
		return f, fmt.Errorf("signal %q is %d bits wide", sg.Name, sg.Length)
	}
	if end := int(sg.BitOffset()) + sg.Length; end > length*8 {
		return f, fmt.Errorf("signal %q ends at bit %d of a %d-byte frame", sg.Name, end, length)
	}

	switch {
	case sg.Float:
		f.Type = logb.TypeFloat
	case sg.Signed:
		f.Type = logb.TypeSint
	case sg.Length == 1:
		// A one-bit unsigned signal is a flag. Saying so keeps it out of the
		// arithmetic it does not belong in — SPEC §7's objection to formats
		// that promote every bool to a float.
		f.Type = logb.TypeBool
	default:
		f.Type = logb.TypeUint
	}

	switch {
	case len(sg.Values) > 0:
		// An enumeration. Its factor and offset are conventionally 1 and 0; if
		// they are not, the names win, because a table of named states is not
		// something a linear scaling was meant to be applied to.
		keys := make([]float64, 0, len(sg.Values))
		for k := range sg.Values {
			keys = append(keys, float64(k))
		}
		sort.Float64s(keys)
		texts := make([]string, len(keys))
		for i, k := range keys {
			texts[i] = sg.Values[uint64(k)]
		}
		f.Conv = logb.ValueToText{Keys: keys, Texts: texts}
		f.Meta["dbc.values"] = fmt.Sprint(len(keys))

	case sg.Factor != 1 || sg.Offset != 0:
		// DBC's physical = raw*factor + offset, which is §7's A + B*raw.
		f.Conv = logb.Linear{A: sg.Offset, B: sg.Factor}
	}

	if sg.Min != 0 || sg.Max != 0 {
		f.Meta["dbc.min"] = trim(sg.Min)
		f.Meta["dbc.max"] = trim(sg.Max)
	}
	if len(f.Meta) == 0 {
		f.Meta = nil
	}
	return f, nil
}

func describe(sg *Signal, fallback string) string {
	if sg.Desc != "" {
		return sg.Desc
	}
	return fallback
}

func frameKind(extended bool) string {
	if extended {
		return "extended"
	}
	return "standard"
}

func trim(v float64) string { return fmt.Sprintf("%g", v) }
