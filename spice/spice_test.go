package spice

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
)

func readRaw(t *testing.T, path string) *Raw {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := ReadRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestReadRaw checks the header against the fixture LTspice XVII wrote.
func TestReadRaw(t *testing.T) {
	r := readRaw(t, "../testdata/test.raw")

	if !r.XVII {
		t.Error("test.raw has a UTF-16 header; XVII = false")
	}
	if r.Plotname != "Transient Analysis" {
		t.Errorf("Plotname = %q", r.Plotname)
	}
	if len(r.Vars) != 31 || r.Points != 1165 {
		t.Fatalf("%d variables, %d points; want 31, 1165", len(r.Vars), r.Points)
	}
	if r.Vars[0].Name != "time" || r.Vars[0].Type != "time" {
		t.Errorf("first variable = %+v, want the time axis", r.Vars[0])
	}
	if r.Vars[1].Name != "V(n004)" || r.Vars[1].Type != "voltage" {
		t.Errorf("second variable = %+v", r.Vars[1])
	}
	if r.Complex() || r.Double() || r.Stepped() {
		t.Errorf("flags %v read as complex/double/stepped", r.Flags)
	}

	// 8 bytes of f64 time plus 30 f32 values.
	if l := r.Layout(); l.PointBytes != 128 {
		t.Errorf("point = %d bytes, want 128", l.PointBytes)
	}
	if got := len(r.Values); got != 1165*128 {
		t.Errorf("binary block = %d bytes, want %d", got, 1165*128)
	}
	if len(r.Backanno) != 2 {
		t.Errorf("Backannotation lines = %d, want 2", len(r.Backanno))
	}
}

// TestConvertTransient is the whole importer: every point of the fixture must
// come back with its axis and its values intact.
func TestConvertTransient(t *testing.T) {
	raw := readRaw(t, "../testdata/test.raw")
	l := raw.Layout()

	var out bytes.Buffer
	if err := Write(raw, &out, Options{Attach: map[string][]byte{"test.net": []byte("* netlist\n")}}); err != nil {
		t.Fatal(err)
	}

	rd, err := logb.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	point := 0
	batches := 0
	for {
		b, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		batches++
		s := b.Schema
		if s.AxisKind != logb.AxisTime || s.AxisMode != logb.AxisExplicit {
			t.Fatalf("axis = kind %v mode %v, want an explicit time axis", s.AxisKind, s.AxisMode)
		}
		// 1 ms of transient in femtoseconds is 1e12 ticks: exact in int64 and
		// still exact in the float64 Batch.Axis routes the field through.
		if s.AxisExp != -15 {
			t.Errorf("axis_exp = %d, want -15", s.AxisExp)
		}
		if len(s.Fields) != 31 {
			t.Fatalf("%d fields, want 31", len(s.Fields))
		}

		for i := 0; i < int(b.Count); i++ {
			src := raw.Values[point*l.PointBytes:]
			// The sign bit LTspice sets on some time values is a marker, not a
			// negative time.
			wantSec := math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(src)))

			a, err := b.Axis(i)
			if err != nil {
				t.Fatal(err)
			}
			// One tick of slack: seconds are rounded to femtoseconds on import.
			if got := a.Seconds(-15); math.Abs(got-wantSec) > 2e-15 {
				t.Fatalf("point %d: axis = %g s, want %g s", point, got, wantSec)
			}

			// Values are copied verbatim, so they must be bit-identical.
			for f := 1; f < 31; f++ {
				want := float64(math.Float32frombits(binary.LittleEndian.Uint32(src[8+(f-1)*4:])))
				v, err := b.Value(i, f)
				if err != nil {
					t.Fatal(err)
				}
				if v.(float64) != want {
					t.Fatalf("point %d, %s = %g, want %g", point, s.Fields[f].Name, v, want)
				}
			}
			point++
		}
	}
	if point != raw.Points {
		t.Fatalf("read back %d points, want %d", point, raw.Points)
	}
	if batches != 1 {
		t.Errorf("%d batches; an unstepped run is one run", batches)
	}

	if len(rd.Attachments) != 1 || rd.Attachments["test.net"] == nil {
		t.Errorf("attachments = %+v, want the netlist", rd.Attachments)
	}
	if got := metaOf(rd)["sim.analysis"]; got != "transient" {
		t.Errorf("sim.analysis = %q, want transient", got)
	}
}

// TestConvertOperatingPoint is the degenerate case: one point, no independent
// variable, so nothing may be mistaken for an axis.
func TestConvertOperatingPoint(t *testing.T) {
	raw := readRaw(t, "../testdata/test.op.raw")
	if raw.Points != 1 || len(raw.Vars) != 30 {
		t.Fatalf("%d points, %d variables; want 1, 30", raw.Points, len(raw.Vars))
	}

	var out bytes.Buffer
	if err := Write(raw, &out, Options{}); err != nil {
		t.Fatal(err)
	}
	rd, err := logb.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Schema.AxisKind != logb.AxisIndex {
		t.Errorf("axis kind = %v, want index", b.Schema.AxisKind)
	}
	if len(b.Schema.Fields) != 30 || b.Count != 1 {
		t.Fatalf("%d fields, %d records; want 30, 1", len(b.Schema.Fields), b.Count)
	}
	// V(vdd) is the 5 V supply, and is the eighth variable.
	v, err := b.Value(0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v.(float64)-5) > 1e-6 {
		t.Errorf("V(vdd) = %v, want 5", v)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Errorf("second batch: %v, want EOF", err)
	}
}

// TestSteppedAC covers the two paths the LTspice fixtures do not have: complex
// values on a frequency axis, and a stepped sweep whose run boundaries must be
// recovered from the axis restarting.
func TestSteppedAC(t *testing.T) {
	const runs, points = 3, 4
	freqs := []float64{10, 100, 1000, 10000}

	var body bytes.Buffer
	for run := 0; run < runs; run++ {
		for i, f := range freqs {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(f))
			body.Write(b[:]) // frequency, real part
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(0))
			body.Write(b[:]) // frequency, imaginary part
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(run+1)))
			body.Write(b[:4]) // V(out), real
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(-i)))
			body.Write(b[:4]) // V(out), imaginary
		}
	}
	raw := &Raw{
		Title:    "stepped ac",
		Plotname: "AC Analysis",
		Flags:    []string{"complex", "forward", "log", "stepped"},
		Points:   runs * points,
		Vars: []Var{
			{0, "frequency", "frequency"},
			{1, "V(out)", "voltage"},
		},
		Values: body.Bytes(),
	}
	if l := raw.Layout(); l.PointBytes != 24 {
		t.Fatalf("point = %d bytes, want 24", l.PointBytes)
	}

	var out bytes.Buffer
	if err := Write(raw, &out, Options{}); err != nil {
		t.Fatal(err)
	}
	rd, err := logb.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for {
		b, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Schema.AxisKind != logb.AxisFrequency {
			t.Fatalf("axis kind = %v, want frequency", b.Schema.AxisKind)
		}
		if b.Count != uint32(points) {
			t.Fatalf("run %d has %d records, want %d", b.RunID, b.Count, points)
		}
		if b.RunID != uint32(seen) {
			t.Errorf("batch %d has run_id %d", seen, b.RunID)
		}
		for i := range freqs {
			a, err := b.Axis(i)
			if err != nil {
				t.Fatal(err)
			}
			if a.Float() != freqs[i] {
				t.Errorf("run %d, point %d: freq = %g, want %g", b.RunID, i, a.Float(), freqs[i])
			}
			v, err := b.Value(i, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := complex(float64(seen+1), float64(-i))
			if v.(complex128) != want {
				t.Errorf("run %d, point %d: V(out) = %v, want %v", b.RunID, i, v, want)
			}
		}
		seen++
	}
	if seen != runs {
		t.Fatalf("%d runs recovered, want %d", seen, runs)
	}
}

// TestRefusals: the two flags this importer will not guess at, and a file that
// is not a raw file at all.
func TestRefusals(t *testing.T) {
	if _, err := ReadRaw(bytes.NewReader([]byte("hello\n"))); err != ErrNotRaw {
		t.Errorf("garbage input: %v, want ErrNotRaw", err)
	}
	for _, tc := range []struct {
		flag string
		want error
	}{
		{"compressed", ErrCompressed},
		{"fastaccess", ErrFastAccess},
	} {
		hdr := "Title: x\nPlotname: Transient Analysis\nFlags: real " + tc.flag +
			"\nNo. Variables: 2\nNo. Points: 1\nVariables:\n\t0\ttime\ttime\n\t1\tV(a)\tvoltage\nBinary:\n"
		if _, err := ReadRaw(bytes.NewReader([]byte(hdr))); err != tc.want {
			t.Errorf("Flags: %s: %v, want %v", tc.flag, err, tc.want)
		}
	}
}

// TestASCIIHeader reads the LTspice IV form, whose header is not UTF-16.
func TestASCIIHeader(t *testing.T) {
	var f [8]byte
	binary.LittleEndian.PutUint64(f[:], math.Float64bits(1.5e-3))
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], math.Float32bits(2.5))
	hdr := "Title: iv\nPlotname: Transient Analysis\nFlags: real forward\n" +
		"No. Variables: 2\nNo. Points: 1\nOffset: 0.0\nVariables:\n\t0\ttime\ttime\n\t1\tV(a)\tvoltage\nBinary:\n"

	raw, err := ReadRaw(bytes.NewReader(append([]byte(hdr), append(f[:], v[:]...)...)))
	if err != nil {
		t.Fatal(err)
	}
	if raw.XVII {
		t.Error("an ASCII header read as XVII")
	}

	var out bytes.Buffer
	if err := Write(raw, &out, Options{}); err != nil {
		t.Fatal(err)
	}
	rd, err := logb.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	a, err := b.Axis(0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Ticks() != 1_500_000_000_000 {
		t.Errorf("axis = %d ticks, want 1.5e12 fs", a.Ticks())
	}
	d, err := a.Duration(-15)
	if err == nil {
		t.Errorf("Duration accepted a femtosecond tick: %v", d)
	}
}

func metaOf(r *logb.Reader) map[string]string {
	m := map[string]string{}
	for _, kv := range r.Meta {
		m[kv.Key] = kv.Value
	}
	return m
}
