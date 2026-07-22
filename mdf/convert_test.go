package mdf

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
)

// The importer's real test is not that it produces a file — it is that the file
// says the same thing the MDF said. Every fixture is converted, read back with
// the logb package, and compared sample by sample against what this package's
// own decoder makes of the original. Two decoders written from two
// specifications agreeing on every value of every channel is the only evidence
// worth having that the bit numbering carried across.

func convert(t *testing.T, path string, o Options) *logb.Reader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	if err := Convert(f, &buf, o); err != nil {
		t.Fatalf("convert %s: %v", path, err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	return r
}

// collect reads every batch of a converted file, grouped by stream name.
type stripe struct {
	schema *logb.Schema
	axis   []logb.AxisVal
	values [][]any // per record, per field
}

func collect(t *testing.T, r *logb.Reader) map[string]*stripe {
	t.Helper()
	out := map[string]*stripe{}
	// A channel group can be declared and never written to — the LIN side of a
	// CAN recording that saw no LIN traffic. Next only ever hands out a schema
	// attached to a batch, so an empty stream is visible here and nowhere else.
	r.OnSchema = func(s *logb.Schema, _ uint16) {
		if out[s.Name] == nil {
			out[s.Name] = &stripe{schema: s}
		}
	}
	for {
		b, err := r.Next()
		if err != nil {
			break
		}
		if b == nil {
			break
		}
		s := out[b.Schema.Name]
		if s == nil {
			s = &stripe{schema: b.Schema}
			out[b.Schema.Name] = s
		}
		for i := 0; i < int(b.Count); i++ {
			ax, err := b.Axis(i)
			if err != nil {
				t.Fatalf("axis of record %d: %v", i, err)
			}
			s.axis = append(s.axis, ax)
			row := make([]any, len(b.Schema.Fields))
			for f := range b.Schema.Fields {
				v, err := b.Value(i, f)
				if err != nil {
					v = err
				}
				row[f] = v
			}
			s.values = append(s.values, row)
		}
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	for _, name := range []string{
		"sample2.mf4",
		"sample3.mf4",
		"Discrete_deflate.mf4",
		"sample_compressed.mf4",
		"obd2-trunc.mf4",
	} {
		// Both framings, because transpose reorders every byte of the fixed
		// portion on the way out and back (§8) and the tail region does not
		// move with it.
		for _, opt := range []struct {
			name string
			o    Options
		}{
			{"zstd", Options{Codec: logb.CodecZstd}},
			{"transposed", Options{Codec: logb.CodecZstd, Filter: logb.FilterTranspose, PerFrame: 64}},
		} {
			t.Run(name+"/"+opt.name, func(t *testing.T) { roundTrip(t, name, opt.o) })
		}
	}
}

func roundTrip(t *testing.T, name string, o Options) {
	path := "../testdata/mdf/" + name

	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	m, err := ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	got := collect(t, convert(t, path, o))
	if len(got) != len(m.Groups) {
		t.Fatalf("got %d streams, want %d groups", len(got), len(m.Groups))
	}

	for _, g := range m.Groups {
		name := g.Name
		if name == "" {
			name = "group0"
		}
		st := got[name]
		if st == nil {
			t.Fatalf("stream %q missing from the converted file", name)
		}
		if len(st.values) != g.Records {
			t.Fatalf("stream %q: %d records, want %d", name, len(st.values), g.Records)
		}

		// Field index by name, so the comparison does not depend on the
		// order fields happened to be written in.
		idx := map[string]int{}
		for i, f := range st.schema.Fields {
			idx[f.Name] = i
		}

		master := g.Master()
		for i := 0; i < g.Records; i++ {
			rec := g.Record(i)
			for _, c := range g.Channels {
				if c == master {
					checkAxis(t, st, g, c, i)
					continue
				}
				j, ok := idx[c.Name]
				if !ok {
					t.Fatalf("stream %q: no field for channel %q", name, c.Name)
				}
				if c.Kind == VLSD {
					want := g.VLSD[c][i]
					b, ok := st.values[i][j].([]byte)
					if !ok {
						t.Fatalf("%s record %d: %q is %T, want bytes", name, i, c.Name, st.values[i][j])
					}
					// The slot is the widest sample in the group; a
					// shorter one is zero-padded up to it.
					if !bytes.Equal(b[:len(want)], want) {
						t.Fatalf("%s record %d: %q = % x, want % x", name, i, c.Name, b, want)
					}
					continue
				}
				want, err := c.Value(rec)
				if err != nil {
					t.Fatalf("%s record %d: decoding %q: %v", name, i, c.Name, err)
				}
				if !same(st.values[i][j], want) {
					t.Fatalf("%s record %d: %q = %v (%T), want %v (%T)",
						name, i, c.Name, st.values[i][j], st.values[i][j], want, want)
				}
			}
		}
	}
}

// checkAxis compares one record's axis against the master channel's own value.
func checkAxis(t *testing.T, st *stripe, g *Group, master *Channel, i int) {
	t.Helper()
	want, err := master.Float(g.Record(i))
	if err != nil {
		t.Fatal(err)
	}
	var got float64
	if st.schema.AxisKind == logb.AxisTime {
		got = st.axis[i].Seconds(st.schema.AxisExp)
	} else {
		got = st.axis[i].Float()
	}
	// One tick of slack: seconds are stored as an integer count of them, which
	// is the whole point, and the last one is rounded rather than truncated.
	tol := math.Pow10(int(st.schema.AxisExp))
	if st.schema.AxisKind != logb.AxisTime {
		tol = 0
	}
	if math.Abs(got-want) > tol {
		t.Fatalf("record %d: axis = %v, want %v (tolerance %v)", i, got, want, tol)
	}
}

// same compares a value read back from Logb against one decoded from MDF.
// Integers may legitimately arrive as either width of the same number.
func same(got, want any) bool {
	if g, ok := got.([]byte); ok {
		w, ok := want.([]byte)
		return ok && bytes.Equal(g, w)
	}
	if gf, ok := asFloat(got); ok {
		if wf, ok := asFloat(want); ok {
			return gf == wf || (math.IsNaN(gf) && math.IsNaN(wf))
		}
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// TestCANDetail pins the values a CAN recording is actually read for. An OBD2
// exchange has a request on 0x7DF and a reply on 0x7E8, and the payload is what
// the eight bytes of the frame say — not an offset into a block somewhere else,
// which is how MDF stores it.
func TestCANDetail(t *testing.T) {
	path := "../testdata/mdf/obd2-trunc.mf4"
	got := collect(t, convert(t, path, Options{}))

	st := got["CAN_DataFrame"]
	if st == nil {
		t.Fatal("no CAN_DataFrame stream")
	}
	if st.schema.AxisKind != logb.AxisTime {
		t.Fatalf("axis kind %v, want time", st.schema.AxisKind)
	}
	idx := map[string]int{}
	for i, f := range st.schema.Fields {
		idx[f.Name] = i
	}
	for _, name := range []string{"CAN_DataFrame.ID", "CAN_DataFrame.DLC", "CAN_DataFrame.DataBytes"} {
		if _, ok := idx[name]; !ok {
			t.Fatalf("no field %q; composed channels were not flattened", name)
		}
	}

	// The first four frames of this recording, by inspection of the source.
	wantID := []uint64{2024, 2015, 2024, 2015}
	for i, want := range wantID {
		if got := st.values[i][idx["CAN_DataFrame.ID"]]; !same(got, want) {
			t.Errorf("frame %d: ID = %v, want %v", i, got, want)
		}
		if got := st.values[i][idx["CAN_DataFrame.DLC"]]; !same(got, uint64(8)) {
			t.Errorf("frame %d: DLC = %v, want 8", i, got)
		}
	}
	payload, ok := st.values[0][idx["CAN_DataFrame.DataBytes"]].([]byte)
	if !ok || len(payload) != 8 {
		t.Fatalf("payload is %v, want 8 bytes", st.values[0][idx["CAN_DataFrame.DataBytes"]])
	}
	if want := []byte{3, 65, 11, 28, 255, 255, 255, 255}; !bytes.Equal(payload, want) {
		t.Errorf("payload = % x, want % x", payload, want)
	}

	// A field's variable-length origin is not lost, and the DLC field kept its
	// bit-level position: 4 bits at bit 2 of byte 13, past the eight the axis
	// added.
	if f := st.schema.Fields[idx["CAN_DataFrame.DataBytes"]]; f.Meta["mdf.vlsd"] != "true" {
		t.Error("the payload field does not record that it was variable-length")
	}
	if f := st.schema.Fields[idx["CAN_DataFrame.DLC"]]; f.BitOffset != 64+13*8+2 || f.BitWidth != 4 {
		t.Errorf("DLC is %d bits at bit %d, want 4 at %d", f.BitWidth, f.BitOffset, 64+13*8+2)
	}
}

// TestMetadata checks what travels alongside the samples: the attachment, the
// recording's wall clock, and the note that a file was never finalized.
func TestMetadata(t *testing.T) {
	r := convert(t, "../testdata/mdf/sample3.mf4", Options{})
	collect(t, r) // drain, so every frame is seen

	if got := len(r.Attachments); got != 1 {
		t.Fatalf("%d attachments, want 1", got)
	}
	data, ok := r.Attachments["user_embedded_display.dspf"]
	if !ok {
		t.Fatalf("attachment names: %v", keys(r.Attachments))
	}
	if len(data) != 741 {
		t.Errorf("attachment is %d bytes, want 741 (the AT block's original size)", len(data))
	}

	meta := map[string]string{}
	for _, m := range r.Meta {
		meta[m.Key] = m.Value
	}
	if meta["source.format"] != "mdf4" {
		t.Errorf("source.format = %q", meta["source.format"])
	}
	if meta["mdf.version"] != "410" {
		t.Errorf("mdf.version = %q, want 410", meta["mdf.version"])
	}
	if _, ok := meta["mdf.finalized"]; ok {
		t.Error("sample3 is finalized; nothing should say otherwise")
	}

	// The unfinalized one says so.
	r2 := convert(t, "../testdata/mdf/obd2-trunc.mf4", Options{})
	collect(t, r2)
	for _, m := range r2.Meta {
		if m.Key == "mdf.finalized" && m.Value == "false" {
			return
		}
	}
	t.Error("obd2-trunc.mf4 is unfinalized and the converted file does not record it")
}

// TestConversionSurvives is about sample3's speed channel, whose MDF conversion
// is a value-to-text table with a numeric default. Logb cannot hold both, and
// the measurement is what matters: the numbers must come through.
func TestConversionSurvives(t *testing.T) {
	got := collect(t, convert(t, "../testdata/mdf/sample3.mf4", Options{}))
	st := got["CCVS1_CPC"]
	if st == nil {
		t.Fatal("no CCVS1_CPC stream")
	}
	idx := -1
	for i, f := range st.schema.Fields {
		if f.Name == "VehSpd_Cval_CPC" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no speed field")
	}
	want := []float64{99.74609375, 99.7578125, 99.69140625}
	for i, w := range want {
		got, ok := asFloat(st.values[i][idx])
		if !ok || got != w {
			t.Errorf("record %d: speed = %v, want %v", i, st.values[i][idx], w)
		}
	}
	// The names the table did carry are still in the file, just not applied.
	f := st.schema.Fields[idx]
	if f.Meta["mdf.cc"] == "" {
		t.Error("the field does not say what conversion it came from")
	}
	found := false
	for k := range f.Meta {
		if len(k) > 12 && k[:12] == "mdf.cc.text." {
			found = true
		}
	}
	if !found {
		t.Errorf("the value-to-text entries were dropped rather than kept as metadata: %v", f.Meta)
	}
}

// TestAxisIsTicks checks the axis representation, which is the one place the
// bytes are not copied but recomputed.
func TestAxisIsTicks(t *testing.T) {
	got := collect(t, convert(t, "../testdata/mdf/sample2.mf4", Options{}))
	st := got["group0"]
	if st == nil {
		t.Fatalf("streams: %v", keysOf(got))
	}
	if st.schema.AxisMode != logb.AxisExplicit {
		t.Errorf("axis mode %v, want explicit", st.schema.AxisMode)
	}
	// A measurement's timestamps come from a clock, so the tick stops at the
	// nanosecond: finer would claim precision the instrument did not have, and
	// would cost the axis its time.Duration.
	if st.schema.AxisExp != -9 {
		t.Errorf("axis exponent %d, want -9 (nanoseconds)", st.schema.AxisExp)
	}
	if d, err := st.axis[0].Duration(st.schema.AxisExp); err != nil {
		t.Errorf("the axis cannot be read as a duration: %v", err)
	} else if d.Seconds() != 10 {
		t.Errorf("first axis value %v, want 10s", d)
	}
	f := st.schema.Fields[st.schema.AxisField]
	if f.Type != logb.TypeSint {
		t.Errorf("the axis field is %v, want sint: an explicit time axis counts ticks", f.Type)
	}
	if f.Unit == "s" {
		t.Error(`the axis field's unit is "s", but it stores ticks, not seconds`)
	}
	if got, want := st.axis[0].Seconds(st.schema.AxisExp), 10.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("first axis value %v, want %v", got, want)
	}
}

// TestWarnings checks that what could not be carried across is reported rather
// than dropped in silence.
func TestWarnings(t *testing.T) {
	f, err := os.Open("../testdata/mdf/Discrete_deflate.mf4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var warned []string
	var buf bytes.Buffer
	err = Convert(f, &buf, Options{Warn: func(format string, a ...any) {
		warned = append(warned, fmt.Sprintf(format, a...))
	}})
	if err != nil {
		t.Fatal(err)
	}
	// This file has two EV blocks, which are not converted.
	for _, w := range warned {
		if bytes.Contains([]byte(w), []byte("event")) {
			return
		}
	}
	t.Errorf("the file's events were dropped without a word: %v", warned)
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOf(m map[string]*stripe) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
