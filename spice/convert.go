package spice

import (
	"encoding/binary"
	"io"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/internal/tick"
)

// Options controls Convert.
type Options struct {
	// Codec compresses the DATA frames. Zero value is no compression.
	Codec logb.Codec

	// Name is the stream name. Empty means one derived from the analysis:
	// "tran", "ac", "dc", "op", …
	Name string

	// UUID overrides the stream identity. The default is derived from the
	// file's title and plotname, so re-importing the same run twice produces
	// the same stream rather than two that will not merge.
	UUID *[16]byte

	// Attach is embedded as ATTACH frames: a netlist, a model library.
	Attach map[string][]byte
}

// Convert reads a SPICE raw file and writes it as a Logb file.
func Convert(r io.Reader, w io.Writer, o Options) error {
	raw, err := ReadRaw(r)
	if err != nil {
		return err
	}
	return Write(raw, w, o)
}

// Write maps an already-parsed raw file onto Logb.
func Write(raw *Raw, w io.Writer, o Options) error {
	l := raw.Layout()
	analysis := analysisOf(raw.Plotname)

	s, err := schemaOf(raw, l, analysis, o)
	if err != nil {
		return err
	}

	// Run boundaries are explicit in Logb (§6.5). LTspice leaves them to be
	// guessed at, and the only signal it gives is the axis restarting — so that
	// guess is made here, once, at import, and never again by a reader. Without
	// the stepped flag there is nothing to split: one run.
	bounds := []int{0, raw.Points}
	if raw.Stepped() {
		bounds = runBounds(raw, l)
	}

	vw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}
	vw.Codec = o.Codec
	if err := vw.AddStream(s); err != nil {
		return err
	}
	for i := 0; i+1 < len(bounds); i++ {
		if err := vw.AddRun(&logb.Run{ID: uint32(i), Index: uint32(i)}); err != nil {
			return err
		}
	}
	if err := vw.BeginSegment(0); err != nil {
		return err
	}

	for name, data := range o.Attach {
		if err := vw.WriteAttach(name, data); err != nil {
			return err
		}
	}

	for i := 0; i+1 < len(bounds); i++ {
		from, to := bounds[i], bounds[i+1]
		base, recs := encode(raw, s, l, from, to)
		if err := vw.WriteData(s, base, uint32(i), uint32(to-from), recs); err != nil {
			return err
		}
	}

	for _, kv := range [][2]string{
		{"title", raw.Title},
		{"date", raw.Date},
		{"command", raw.Command},
		{"sim.analysis", analysis},
		{"source.format", "spice.raw"},
		{"source.flags", strings.Join(raw.Flags, " ")},
		{"spice.backannotation", strings.Join(raw.Backanno, "\n")},
	} {
		if kv[1] == "" {
			continue
		}
		if err := vw.WriteMeta(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return vw.Close()
}

// schemaOf builds the stream schema: variable 0 becomes the axis, the rest
// become fields (SPEC.md §11).
func schemaOf(raw *Raw, l Layout, analysis string, o Options) (*logb.Schema, error) {
	name := o.Name
	if name == "" {
		name = analysis
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("logb/spice/"+raw.Title+"/"+raw.Plotname))
	if o.UUID != nil {
		id = *o.UUID
	}

	s := &logb.Schema{
		UUID: id,
		Name: name,
		Meta: map[string]string{"sim.analysis": analysis},
	}

	axisVar := raw.Vars[0]
	kind := strings.ToLower(axisVar.Type)
	switch {
	case analysis == "op":
		// An operating point has one point and no independent variable. The
		// record index is the axis; every variable, including the first, is an
		// ordinary field.
		s.AxisKind = logb.AxisIndex
		s.AxisMode = logb.AxisImplicit
		s.AxisStep = logb.TickVal(1)

	case kind == "time":
		// A transient's timestep is not uniform, so the axis is stored per
		// record — as an integer tick count, because Schema.AxisAt computes an
		// explicit time axis as base + int64(field)*scale. Seconds in a float
		// field would be truncated to whole seconds.
		exp, err := pickExponent(raw, l)
		if err != nil {
			return nil, err
		}
		s.AxisKind = logb.AxisTime
		s.AxisMode = logb.AxisExplicit
		s.AxisExp = exp
		s.AxisUnit = "s"
		s.AxisScale = logb.TickVal(1)
		s.AxisField = 0

	default:
		// Frequency, or a swept source in a DC analysis. Non-time axes are
		// f64 quantities, so the axis field carries the value itself.
		s.AxisKind = logb.AxisOther
		if kind == "frequency" {
			s.AxisKind = logb.AxisFrequency
		}
		s.AxisMode = logb.AxisExplicit
		s.AxisUnit = unitOf(axisVar.Type)
		s.AxisScale = logb.FloatVal(1)
		s.AxisField = 0
	}

	// Variable 0 occupies the wide first slot whatever the analysis. For a
	// stream with an axis it becomes the axis field — an i64 tick count for
	// time, the stored f64 otherwise; for an operating point it is an ordinary
	// field that happens to be eight bytes wide.
	var bit uint32
	if analysis == "op" {
		s.Fields = append(s.Fields, varField(axisVar, 0, uint32(l.AxisBytes)*8, l))
		bit = uint32(l.AxisBytes) * 8
	} else {
		f := logb.Field{
			Name:      axisVar.Name,
			BitOffset: 0,
			BitWidth:  64,
			Type:      logb.TypeFloat,
			Unit:      unitOf(axisVar.Type),
			Meta:      map[string]string{"spice.type": axisVar.Type},
		}
		if s.AxisKind == logb.AxisTime {
			// The field holds ticks, not seconds, so its unit is the tick: a
			// reader printing the stored value with unit "s" would report a
			// nanosecond transient as ten million seconds.
			f.Type = logb.TypeSint
			f.Unit = tick.Unit(s.AxisExp)
			f.Meta["axis.ticks"] = "true"
		}
		s.Fields = append(s.Fields, f)
		bit = 64
	}

	width := uint32(l.VarBytes) * 8
	for _, v := range raw.Vars[1:] {
		s.Fields = append(s.Fields, varField(v, bit, width, l))
		bit += width
	}
	s.RecordBits = bit

	return s, s.Validate()
}

// varField makes a field for one SPICE variable.
func varField(v Var, bitOff, bitWidth uint32, l Layout) logb.Field {
	f := logb.Field{
		Name:      v.Name,
		BitOffset: bitOff,
		BitWidth:  bitWidth,
		Type:      logb.TypeFloat,
		Unit:      unitOf(v.Type),
		Meta:      map[string]string{"spice.type": v.Type},
	}
	if l.Components == 2 {
		// A complex value is one quantity with a real and an imaginary half,
		// not two fields (§6.3).
		f.Type = logb.TypeComplex
	}
	return f
}

// encode rewrites points [from,to) as Logb records and returns the axis value of
// the first one.
//
// Everything but the axis is copied verbatim: the raw file already stores
// little-endian f32/f64, in the order the schema declares. Only the axis changes
// representation, and only for a time axis, where seconds become ticks.
func encode(raw *Raw, s *logb.Schema, l Layout, from, to int) (logb.AxisVal, []byte) {
	recBytes := s.RecordBytes()
	out := make([]byte, (to-from)*recBytes)

	for i := from; i < to; i++ {
		src := raw.Values[i*l.PointBytes : (i+1)*l.PointBytes]
		dst := out[(i-from)*recBytes:]

		if s.AxisKind == logb.AxisIndex {
			copy(dst, src)
			continue
		}
		axis := math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(src))) + raw.Offset
		if s.AxisKind == logb.AxisTime {
			binary.LittleEndian.PutUint64(dst, uint64(tick.Of(axis, s.AxisExp)))
		} else {
			binary.LittleEndian.PutUint64(dst, math.Float64bits(axis))
		}
		// The axis occupies 8 bytes in the record whatever its type; a complex
		// axis's imaginary half is dropped, so the copy starts after it.
		copy(dst[8:], src[l.AxisBytes:])
	}

	base := logb.FloatVal(0)
	if s.AxisKind == logb.AxisTime {
		base = logb.TickVal(0)
	}
	return base, out
}

// runBounds finds the point indices where a stepped sweep restarts. The axis of
// a single run is monotonic; a step boundary is where it goes backwards.
func runBounds(raw *Raw, l Layout) []int {
	bounds := []int{0}
	prev := math.Inf(-1)
	for i := 0; i < raw.Points; i++ {
		a := raw.Axis(l, i)
		if a < prev {
			bounds = append(bounds, i)
		}
		prev = a
	}
	return append(bounds, raw.Points)
}

// pickExponent chooses the time axis tick size for this run. The choice itself
// is internal/tick's; all that is SPICE's about it is where the maximum comes
// from.
func pickExponent(raw *Raw, l Layout) (int8, error) {
	max := 0.0
	for i := 0; i < raw.Points; i++ {
		if a := raw.Axis(l, i) + math.Abs(raw.Offset); a > max {
			max = a
		}
	}
	return tick.Exp(max, tick.Exponents[0])
}

// analysisOf maps Plotname onto SPEC.md §11's sim.analysis values.
func analysisOf(plotname string) string {
	p := strings.ToLower(plotname)
	switch {
	case strings.HasPrefix(p, "transient"):
		return "transient"
	case strings.HasPrefix(p, "ac analysis"):
		return "ac"
	case strings.HasPrefix(p, "dc transfer"), strings.HasPrefix(p, "dc analysis"):
		return "dc"
	case strings.HasPrefix(p, "noise"):
		return "noise"
	case strings.HasPrefix(p, "operating point"):
		return "op"
	case strings.HasPrefix(p, "transfer function"):
		return "tf"
	case p == "":
		return "unknown"
	}
	return strings.ReplaceAll(p, " ", "_")
}

// unitOf maps the SPICE type column onto a unit. The column itself is kept in
// the field's metadata, so nothing is lost when the mapping has no answer.
func unitOf(spiceType string) string {
	switch t := strings.ToLower(spiceType); {
	case t == "time":
		return "s"
	case t == "frequency":
		return "Hz"
	case t == "voltage":
		return "V"
	case strings.HasSuffix(t, "current"):
		return "A"
	case t == "power":
		return "W"
	case t == "temperature":
		return "degC"
	}
	return ""
}
