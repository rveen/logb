package mdf

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"github.com/rveen/logb/internal/tick"
)

// Options controls Convert.
type Options struct {
	// Codec compresses the DATA frames. The zero value is no compression.
	Codec logb.Codec

	// Filter is applied to the fixed portion of each DATA frame. Transpose is
	// worth trying on measurement data — it is the same idea MDF spells
	// DZ zip_type 1 (§8).
	Filter logb.Filter

	// PerFrame is records per DATA frame, and so the granularity of both the
	// index and any decode. Default 65536.
	PerFrame int

	// FramesPerSegment bounds how far a reader must skip to resynchronise after
	// damage. Default 64.
	FramesPerSegment int

	// DBC, if set, decodes a CAN bus recording's frames into a stream of
	// signals per database message, alongside the raw frames. Without it a bus
	// recording converts to what it contains — identifiers and payload bytes —
	// because that is all a recording ever contains.
	DBC *dbc.File

	// Warn, if set, is told about everything the mapping could not carry
	// across. An importer that drops something in silence is worse than one
	// that refuses, because nobody finds out until the number is wrong.
	Warn func(format string, a ...any)
}

func (o *Options) setDefaults() {
	if o.PerFrame <= 0 {
		o.PerFrame = 65536
	}
	if o.FramesPerSegment <= 0 {
		o.FramesPerSegment = 64
	}
	if o.Warn == nil {
		o.Warn = func(string, ...any) {}
	}
}

// Convert reads an MDF4 file and writes it as a Logb file.
func Convert(r io.ReadSeeker, w io.Writer, o Options) error {
	f, err := ReadFile(r)
	if err != nil {
		return err
	}
	return Write(f, w, o)
}

// stream is one channel group, and what was decided about how to write it.
type stream struct {
	g      *Group
	s      *logb.Schema
	master *Channel

	// axisBytes is 8 when the axis is stored per record and 0 when it is
	// implicit. The MDF record follows it, byte for byte.
	axisBytes int

	// vlsdAt is where each variable-length channel's inlined samples sit, and
	// vlsdWidth how wide the slot is.
	vlsdAt    map[*Channel]int
	vlsdWidth map[*Channel]int

	// base is the implicit axis's first value and step its increment; both are
	// unused for an explicit axis, which stores the value in field 0.
	base, step logb.AxisVal
}

// Write maps an already-parsed MDF file onto Logb.
//
// One channel group becomes one stream. The record is copied as it stands and
// the axis is prepended, so a field's bit offset in the Logb record is its MDF
// byte offset shifted by the eight bytes of axis — no repacking, no re-rounding,
// and the same bits come out the far end. That works because Logb numbers bits
// the way MDF does (§6.3): little-endian fields from the low bit of the first
// byte upward, big-endian ones from the high bit downward.
//
// The axis is prepended rather than written over the master channel's own bytes
// because an MDF master is not always a slot that a tick count fits: it can be
// a uint64 that only becomes seconds once a linear conversion is applied, or a
// 32-bit float. Eight bytes per record buys one code path for every file.
func Write(f *File, w io.Writer, o Options) error {
	o.setDefaults()

	streams := make([]*stream, 0, len(f.Groups))
	for i, g := range f.Groups {
		st, err := plan(f, g, i, &o)
		if err != nil {
			return err
		}
		streams = append(streams, st)
	}

	// With a database, a bus recording also yields a stream per message: the
	// signals someone actually wants to see. The frames stay — see bus.go.
	var signals []*decoded
	if o.DBC != nil {
		for _, st := range streams {
			c := asCAN(st.g)
			if c == nil {
				continue
			}
			d, err := decode(f, c, o.DBC, st.s.AxisExp, &o)
			if err != nil {
				return err
			}
			signals = append(signals, d...)
		}
		if len(signals) == 0 {
			o.Warn("no message in the database matched a frame in this recording")
		}
	}

	vw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}
	vw.Codec, vw.Filter = o.Codec, o.Filter
	for _, st := range streams {
		if err := vw.AddStream(st.s); err != nil {
			return err
		}
	}
	for _, d := range signals {
		if err := vw.AddStream(d.schema); err != nil {
			return err
		}
	}
	// MDF has no notion of a run — no sweep, no repeated measurement under
	// changed conditions — so everything is run 0 and no RUN frame is written
	// (§6.5).
	if err := vw.BeginSegment(f.StartTime.UnixNano()); err != nil {
		return err
	}

	for _, a := range f.Attach {
		if a.External {
			o.Warn("attachment %q is stored outside the file; only its name is kept", a.Name)
			continue
		}
		name := a.Name
		if name == "" {
			name = "attachment"
		}
		if err := vw.WriteAttach(name, a.Data); err != nil {
			return err
		}
	}

	// The database goes in with the data it explains. A converted recording that
	// names every signal but not what defined them is only nearly
	// self-contained: checking a suspect value, or finding out that the database
	// was wrong about one, would send you back to a file somebody still has to
	// have. The digest is there because two databases with the same name and
	// different contents is the normal state of affairs.
	if o.DBC != nil && len(o.DBC.Raw) > 0 {
		name := o.DBC.Name
		if name == "" {
			name = "database.dbc"
		}
		if err := vw.WriteAttach(name, o.DBC.Raw); err != nil {
			return err
		}
	}

	meta := [][2]string{
		{"source.format", "mdf4"},
		{"mdf.version", strconv.Itoa(int(f.Version))},
		{"mdf.program", f.Program},
		{"mdf.comment", f.Comment},
		{"recording.start", f.StartTime.Format("2006-01-02T15:04:05Z07:00")},
	}
	if o.DBC != nil {
		meta = append(meta,
			[2]string{"source.dbc", o.DBC.Name},
			[2]string{"source.dbc.sha256", o.DBC.SHA256()},
			[2]string{"source.dbc.messages", strconv.Itoa(len(o.DBC.Messages))})
	}
	if !f.Finalized {
		// Worth saying out loud: the record counts in this file were recovered
		// by walking the data, because the writer never came back to record
		// them.
		meta = append(meta, [2]string{"mdf.finalized", "false"})
	}
	for _, kv := range meta {
		if kv[1] == "" {
			continue
		}
		if err := vw.WriteMeta(kv[0], kv[1]); err != nil {
			return err
		}
	}
	if f.Events > 0 {
		o.Warn("%d event block(s) were not converted", f.Events)
	}
	if f.Transposed && o.Filter != logb.FilterTranspose {
		// The source file compressed this data column-major, which is what
		// Logb's transpose filter does (§8). Worth repeating rather than
		// discovering: on the fixture that arrives this way it is the
		// difference between 750 kB and 46.
		o.Warn("this file stored its data column-transposed; the transpose filter will likely compress it far better")
	}

	frames := 0
	for _, st := range streams {
		for from := 0; from < st.g.Records; from += o.PerFrame {
			to := min(from+o.PerFrame, st.g.Records)
			if frames > 0 && frames%o.FramesPerSegment == 0 {
				if err := vw.BeginSegment(f.StartTime.UnixNano()); err != nil {
					return err
				}
			}
			base, records, err := st.encode(from, to)
			if err != nil {
				return err
			}
			if err := vw.WriteData(st.s, base, 0, uint32(to-from), records); err != nil {
				return err
			}
			frames++
		}
	}

	for _, d := range signals {
		size := d.schema.RecordBytes()
		for from := 0; from < d.count; from += o.PerFrame {
			to := min(from+o.PerFrame, d.count)
			if frames > 0 && frames%o.FramesPerSegment == 0 {
				if err := vw.BeginSegment(f.StartTime.UnixNano()); err != nil {
					return err
				}
			}
			base := logb.TickVal(0)
			if err := vw.WriteData(d.schema, base, 0, uint32(to-from),
				d.records[from*size:to*size]); err != nil {
				return err
			}
			frames++
		}
	}
	return vw.Close()
}

// plan works out the schema and the record layout for one channel group.
func plan(f *File, g *Group, index int, o *Options) (*stream, error) {
	st := &stream{
		g:         g,
		master:    g.Master(),
		vlsdAt:    map[*Channel]int{},
		vlsdWidth: map[*Channel]int{},
	}
	name := g.Name
	if name == "" {
		name = fmt.Sprintf("group%d", index)
	}
	s := &logb.Schema{
		UUID: uuid.NewSHA1(uuid.NameSpaceOID,
			[]byte(fmt.Sprintf("logb/mdf/%d/%d/%s", f.StartTime.UnixNano(), index, name))),
		Name: name,
		Meta: map[string]string{},
	}
	if g.Comment != "" {
		s.Meta["mdf.comment"] = g.Comment
	}
	st.s = s

	if err := st.planAxis(o); err != nil {
		return nil, err
	}

	// The MDF record sits immediately after the axis, and the variable-length
	// samples — which have no fixed slot of their own in MDF — after that.
	bit := uint32(st.axisBytes) * 8
	off := st.axisBytes + g.RecordBytes + g.InvalBytes

	// Fields waiting for their invalidation bit's field to exist. Kept by
	// pointer rather than by name: two channels in a group may share a name,
	// and a guard pointing at the wrong one would be silent.
	type guard struct {
		field int
		c     *Channel
	}
	var guards []guard
	for _, c := range g.Channels {
		if c == st.master {
			continue // it is the axis
		}
		if c.Kind == VLSD {
			width := 0
			for _, sample := range g.VLSD[c] {
				width = max(width, len(sample))
			}
			st.vlsdAt[c], st.vlsdWidth[c] = off, width
			off += width
			s.Fields = append(s.Fields, vlsdField(c, uint32(st.vlsdAt[c])*8, uint32(width)*8))
			if width > 64 {
				o.Warn("channel %q inlines %d-byte samples; records are that much wider", c.Name, width)
			}
			continue
		}
		if c.Array {
			o.Warn("channel %q is an array; it is kept as one opaque field", c.Name)
		}
		fl, err := field(c, bit)
		if err != nil {
			return nil, err
		}
		if c.Conv != nil && c.Conv.Conv == nil && c.Conv.Kind != "none" {
			o.Warn("channel %q has a %s conversion, which Logb does not express; "+
				"its raw values are kept", c.Name, c.Conv.Kind)
		}
		if c.HasInvalBit {
			guards = append(guards, guard{len(s.Fields), c})
		}
		s.Fields = append(s.Fields, fl)
	}

	// An invalidation bit is a guard (§6.2): the sample is present when the bit
	// is clear. The bits are already in the record — they follow it — so this
	// costs a field declaration and no bytes.
	for _, gu := range guards {
		s.Fields = append(s.Fields, logb.Field{
			Name:      gu.c.Name + ".valid",
			BitOffset: uint32(st.axisBytes+g.RecordBytes)*8 + gu.c.InvalBit,
			BitWidth:  1,
			Type:      logb.TypeBool,
			Desc:      "invalidation bit; the sample is present when it is clear",
		})
		s.Fields[gu.field].Guarded = true
		s.Fields[gu.field].GuardField = uint16(len(s.Fields) - 1)
		s.Fields[gu.field].GuardValue = 0
	}

	s.RecordBits = uint32(off) * 8
	return st, s.Validate()
}

// planAxis decides what the group's axis is and how it is stored.
func (st *stream) planAxis(o *Options) error {
	s, g, m := st.s, st.g, st.master

	if m == nil {
		// No independent variable: the record's position is all there is.
		s.AxisKind, s.AxisMode, s.AxisStep = logb.AxisIndex, logb.AxisImplicit, logb.TickVal(1)
		o.Warn("channel group %q has no master channel; its axis is the record index", s.Name)
		return nil
	}

	kind, unit := logb.AxisOther, m.Unit
	switch m.Sync {
	case SyncTime:
		kind = logb.AxisTime
		unit = "s"
	case SyncAngle:
		kind = logb.AxisAngle
		if unit == "" {
			unit = "rad"
		}
	case SyncDistance:
		kind = logb.AxisDistance
		if unit == "" {
			unit = "m"
		}
	case SyncIndex:
		kind = logb.AxisIndex
	}
	s.AxisKind, s.AxisUnit = kind, unit

	if m.Kind == VirtualMaster {
		// A virtual master's value *is* the record index, so the axis costs
		// nothing per record: Logb's implicit mode says the same thing (§5).
		// Its conversion, if any, gives the first value and the step.
		a, b := 0.0, 1.0
		if m.Conv != nil {
			if lin, ok := m.Conv.Conv.(logb.Linear); ok {
				a, b = lin.A, lin.B
			} else if m.Conv.Conv != nil {
				return fmt.Errorf("%w: virtual master %q has a %s conversion",
					ErrUnsupported, m.Name, m.Conv.Kind)
			}
		}
		s.AxisMode = logb.AxisImplicit
		if kind == logb.AxisTime {
			exp, err := tick.Exp(math.Abs(a)+math.Abs(b)*float64(g.Records), tick.Nanosecond)
			if err != nil {
				return err
			}
			s.AxisExp = exp
			s.AxisStep = logb.TickVal(tick.Of(b, exp))
			st.base, st.step = logb.TickVal(tick.Of(a, exp)), logb.TickVal(tick.Of(b, exp))
		} else {
			s.AxisStep = logb.FloatVal(b)
			st.base, st.step = logb.FloatVal(a), logb.FloatVal(b)
		}
		return nil
	}

	// A stored master: the axis is a field, and the field is the first one.
	st.axisBytes = 8
	s.AxisMode, s.AxisField = logb.AxisExplicit, 0
	f := logb.Field{
		Name:     m.Name,
		BitWidth: 64,
		Type:     logb.TypeFloat,
		Unit:     unit,
		Desc:     m.Desc,
		Meta:     map[string]string{"mdf.master": "true"},
	}
	if kind == logb.AxisTime {
		// A time axis counts integer ticks, because Schema.AxisAt computes it as
		// base + int64(field)*scale — seconds in a float field would be
		// truncated to whole seconds. The tick is chosen to hold this
		// recording's longest timestamp exactly.
		max := 0.0
		for i := 0; i < g.Records; i++ {
			v, err := m.Float(g.Record(i))
			if err != nil {
				return err
			}
			max = math.Max(max, math.Abs(v))
		}
		exp, err := tick.Exp(max, tick.Nanosecond)
		if err != nil {
			return err
		}
		s.AxisExp, s.AxisScale = exp, logb.TickVal(1)
		f.Type, f.Unit = logb.TypeSint, tick.Unit(exp)
		f.Meta["axis.ticks"] = "true"
	} else {
		s.AxisScale = logb.FloatVal(1)
	}
	if m.Name == "" {
		f.Name = "axis"
	}
	s.Fields = append(s.Fields, f)
	return nil
}

// encode lays out records [from,to) and returns the batch's base axis value.
func (st *stream) encode(from, to int) (logb.AxisVal, []byte, error) {
	g, s := st.g, st.s
	size := s.RecordBytes()
	out := make([]byte, (to-from)*size)

	for i := from; i < to; i++ {
		rec := g.Record(i)
		dst := out[(i-from)*size:]
		copy(dst[st.axisBytes:], rec)

		if st.axisBytes > 0 {
			v, err := st.master.Float(rec)
			if err != nil {
				return 0, nil, err
			}
			if s.AxisKind == logb.AxisTime {
				binary.LittleEndian.PutUint64(dst, uint64(tick.Of(v, s.AxisExp)))
			} else {
				binary.LittleEndian.PutUint64(dst, math.Float64bits(v))
			}
		}
		for c, at := range st.vlsdAt {
			// MDF keeps these samples outside the record and points at them;
			// Logb inlines them, because a bus payload is a fixed-width bytes
			// field and the indirection buys nothing (§6.4).
			sample := g.VLSD[c][i]
			copy(dst[at:at+st.vlsdWidth[c]], sample)
		}
	}

	base := st.base
	if s.AxisMode == logb.AxisImplicit && s.AxisKind == logb.AxisTime {
		base = logb.TickVal(st.base.Ticks() + int64(from)*st.step.Ticks())
	} else if s.AxisMode == logb.AxisImplicit {
		base = logb.FloatVal(st.base.Float() + float64(from)*st.step.Float())
	}
	return base, out, nil
}

// field maps one channel onto a Logb field at bit offset bit.
func field(c *Channel, bit uint32) (logb.Field, error) {
	f := logb.Field{
		Name:      c.Name,
		BitOffset: bit + c.ByteOffset*8 + c.BitOffset,
		BitWidth:  c.BitCount,
		Unit:      c.Unit,
		Desc:      c.Desc,
		BigEndian: c.BigEndian(),
		Meta:      map[string]string{},
	}
	if c.Conv != nil {
		f.Conv = c.Conv.Conv
		if c.Conv.Kind != "none" {
			f.Meta["mdf.cc"] = c.Conv.Kind
		}
		for k, v := range c.Conv.Notes {
			f.Meta[k] = v
		}
	}

	switch c.DataType {
	case DTUintLE, DTUintBE:
		f.Type = logb.TypeUint
	case DTIntLE, DTIntBE:
		f.Type = logb.TypeSint
	case DTFloatLE, DTFloatBE:
		f.Type = logb.TypeFloat
	case DTStringLatin1, DTStringUTF8:
		f.Type = logb.TypeString
	case DTStringUTF16LE, DTStringUTF16BE:
		// Logb strings are UTF-8. Handing back UTF-16 bytes as a string would
		// be a lie about their encoding, so they stay bytes and say so.
		f.Type = logb.TypeBytes
		f.Meta["mdf.encoding"] = "utf-16"
	case DTComplexLE, DTComplexBE:
		f.Type = logb.TypeComplex
	default:
		f.Type = logb.TypeBytes
	}

	if f.BitWidth == 0 {
		// A channel with no bits is MDF's "virtual data": its value is computed
		// from the record index rather than stored. Only a master gets that
		// treatment here, and this is not one.
		return f, fmt.Errorf("%w: channel %q occupies no bits", ErrUnsupported, c.Name)
	}

	switch f.Type {
	case logb.TypeUint, logb.TypeSint:
		if f.BitWidth > 64 {
			return f, fmt.Errorf("%w: channel %q is a %d-bit integer", ErrUnsupported, c.Name, f.BitWidth)
		}
	case logb.TypeFloat:
		if f.BitWidth != 32 && f.BitWidth != 64 {
			return f, fmt.Errorf("%w: channel %q is a %d-bit float", ErrUnsupported, c.Name, f.BitWidth)
		}
	case logb.TypeComplex:
		if f.BitWidth != 64 && f.BitWidth != 128 {
			return f, fmt.Errorf("%w: channel %q is a %d-bit complex", ErrUnsupported, c.Name, f.BitWidth)
		}
	case logb.TypeBytes, logb.TypeString:
		if f.BitOffset%8 != 0 || f.BitWidth%8 != 0 {
			return f, fmt.Errorf("%w: channel %q is %d bits at bit %d; a blob is whole bytes",
				ErrUnsupported, c.Name, f.BitWidth, f.BitOffset)
		}
	}
	if c.Array {
		f.Meta["mdf.array"] = "true"
	}
	return f, nil
}

// vlsdField describes a variable-length channel's inlined slot.
func vlsdField(c *Channel, bit, width uint32) logb.Field {
	f := logb.Field{
		Name:      c.Name,
		BitOffset: bit,
		BitWidth:  width,
		Type:      logb.TypeBytes,
		Unit:      c.Unit,
		Desc:      c.Desc,
		Meta:      map[string]string{"mdf.vlsd": "true"},
	}
	if c.DataType == DTStringLatin1 || c.DataType == DTStringUTF8 {
		f.Type = logb.TypeString
	}
	return f
}
