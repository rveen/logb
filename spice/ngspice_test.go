package spice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
)

// The ngspice fixtures are written by ngspice 47 from testdata/ngspice.cir,
// except ngspice43.dc.raw, which ngspice 43 writes from testdata/ngspice43.cir.

func near(a, b, rel float64) bool {
	return a == b || math.Abs(a-b) <= rel*math.Max(math.Abs(a), math.Abs(b))
}

// TestNgspiceOP reads the binary and the ASCII form of the same operating
// point: ngspice writes every value as f64, with no double flag.
func TestNgspiceOP(t *testing.T) {
	r := readRaw(t, "../testdata/ngspice.op.raw")
	a := readRaw(t, "../testdata/ngspice.op.ascii.raw")

	if r.Dialect != DialectNgspice || r.ASCII || !a.ASCII {
		t.Fatalf("dialect %v, ASCII %v/%v", r.Dialect, r.ASCII, a.ASCII)
	}
	l, la := r.Layout(), a.Layout()
	if l.PointBytes != 3*8 || la.PointBytes != 3*8 {
		t.Fatalf("point = %d/%d bytes, want 24", l.PointBytes, la.PointBytes)
	}

	for v := range r.Vars {
		if b, x := r.Value(l, 0, v), a.Value(la, 0, v); !near(b, x, 1e-14) {
			t.Errorf("%s: binary %g, ASCII %g", r.Vars[v].Name, b, x)
		}
	}

	vin := r.Value(l, 0, r.Index("V(IN)"))
	vout := r.Value(l, 0, r.Index("v(out)"))
	i := r.Value(l, 0, r.Index("i(v1)"))
	if vin != 5 {
		t.Errorf("v(in) = %g, want 5", vin)
	}
	// The source delivers the current through R1: i(v1) = -(vin - vout)/1k
	if !near(i, -(vin-vout)/1e3, 1e-9) {
		t.Errorf("i(v1) = %g, want %g", i, -(vin-vout)/1e3)
	}
	if r.Index("nothere") != -1 {
		t.Error("Index of a missing variable is not -1")
	}
}

// TestNgspiceTran compares the binary and ASCII transients point by point and
// converts the binary one.
func TestNgspiceTran(t *testing.T) {
	r := readRaw(t, "../testdata/ngspice.tran.raw")
	a := readRaw(t, "../testdata/ngspice.tran.ascii.raw")
	l, la := r.Layout(), a.Layout()

	if r.Points != a.Points || r.Points < 20 {
		t.Fatalf("points %d/%d", r.Points, a.Points)
	}
	for p := 0; p < r.Points; p++ {
		for v := range r.Vars {
			if b, x := r.Value(l, p, v), a.Value(la, p, v); !near(b, x, 1e-14) {
				t.Fatalf("point %d, %s: binary %g, ASCII %g", p, r.Vars[v].Name, b, x)
			}
		}
	}

	var out bytes.Buffer
	if err := Write(r, &out, Options{}); err != nil {
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
	if b.Schema.AxisKind != logb.AxisTime || int(b.Count) != r.Points {
		t.Fatalf("axis kind %v, %d records", b.Schema.AxisKind, b.Count)
	}
	for p := 0; p < r.Points; p++ {
		for f := 1; f < len(r.Vars); f++ {
			v, err := b.Value(p, f)
			if err != nil {
				t.Fatal(err)
			}
			if v.(float64) != r.Value(l, p, f) {
				t.Fatalf("point %d, field %d = %v, want %g", p, f, v, r.Value(l, p, f))
			}
		}
	}
}

// TestNgspiceDC: the axis of a DC sweep from -2 V to 2 V keeps its sign.
func TestNgspiceDC(t *testing.T) {
	r := readRaw(t, "../testdata/ngspice.dc.raw")
	l := r.Layout()
	if r.Points != 9 || r.Axis(l, 0) != -2 || r.Axis(l, 8) != 2 {
		t.Fatalf("%d points, axis %g … %g; want 9, -2 … 2", r.Points, r.Axis(l, 0), r.Axis(l, 8))
	}

	var out bytes.Buffer
	if err := Write(r, &out, Options{}); err != nil {
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
	if a.Float() != -2 {
		t.Errorf("converted axis starts at %g, want -2", a.Float())
	}
}

// TestNgspice43: ngspice 43 writes no Command: line, so its binary DC sweep is
// recognised by the size of its block. It is the circuit and the sweep of
// ngspice.dc.raw, and must read the same.
func TestNgspice43(t *testing.T) {
	r := readRaw(t, "../testdata/ngspice43.dc.raw")
	if r.Command != "" || r.Dialect != DialectNgspice {
		t.Fatalf("Command %q, dialect %v; want none, ngspice", r.Command, r.Dialect)
	}
	ref := readRaw(t, "../testdata/ngspice.dc.raw")
	if r.Points != ref.Points || len(r.Vars) != len(ref.Vars) {
		t.Fatalf("%d points × %d variables, ngspice 47 has %d × %d", r.Points, len(r.Vars), ref.Points, len(ref.Vars))
	}
	l, lr := r.Layout(), ref.Layout()
	for p := 0; p < r.Points; p++ {
		for v := range r.Vars {
			if a, b := r.Value(l, p, v), ref.Value(lr, p, v); !near(a, b, 1e-9) {
				t.Fatalf("point %d, %s: ngspice 43 %g, ngspice 47 %g", p, r.Vars[v].Name, a, b)
			}
		}
	}
	if r.Axis(l, 0) != -2 {
		t.Errorf("axis starts at %g, want -2", r.Axis(l, 0))
	}
}

// TestDetect: a header that names its program settles the dialect; one that
// names none is settled by the size of the block, and is ngspice when both
// layouts fit.
func TestDetect(t *testing.T) {
	f64 := func(x float64) []byte { return binary.LittleEndian.AppendUint64(nil, math.Float64bits(x)) }
	f32 := func(x float32) []byte { return binary.LittleEndian.AppendUint32(nil, math.Float32bits(x)) }
	cat := func(b ...[]byte) []byte { return bytes.Join(b, nil) }

	const (
		head = "Title: x\nPlotname: DC transfer characteristic\nFlags: real\n"
		two  = "No. Variables: 2\nNo. Points: 1\nVariables:\n\t0\tv(v-sweep)\tvoltage\n\t1\tv(out)\tvoltage\nBinary:\n"
		one  = "No. Variables: 1\nNo. Points: 1\nVariables:\n\t0\tv(v-sweep)\tvoltage\nBinary:\n"
	)
	narrow := cat(f64(-2), f32(1))
	for _, tc := range []struct {
		name, hdr string
		body      []byte
		want      Dialect
		axis      float64
		err       error
	}{
		{"f32 without Command", head + two, narrow, DialectLTspice, 2, nil},
		{"f64 without Command", head + two, cat(f64(-2), f64(1)), DialectNgspice, -2, nil},
		{"axis only", head + one, f64(-2), DialectNgspice, -2, nil},
		{"LTspice Command", head + "Command: Linear Technology Corporation LTspice IV\n" + two, narrow, DialectLTspice, 2, nil},
		{"Offset line", head + "Offset: 0.0\n" + two, narrow, DialectLTspice, 2, nil},
		{"ngspice Command beats size", head + "Command: ngspice-47\n" + two, narrow, 0, 0, ErrShortValues},
		{"neither fits", head + two, cat(narrow, f32(0)[:2]), 0, 0, ErrShortValues},
	} {
		r, err := ReadRaw(bytes.NewReader(append([]byte(tc.hdr), tc.body...)))
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: %v, want %v", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if r.Dialect != tc.want {
			t.Errorf("%s: dialect %v, want %v", tc.name, r.Dialect, tc.want)
		}
		if a := r.Axis(r.Layout(), 0); a != tc.axis {
			t.Errorf("%s: axis %g, want %g", tc.name, a, tc.axis)
		}
	}
}

// TestNgspiceAC: complex values, and a frequency axis whose type column is
// "frequency grid=3".
func TestNgspiceAC(t *testing.T) {
	r := readRaw(t, "../testdata/ngspice.ac.raw")
	l := r.Layout()
	if !r.Complex() || l.PointBytes != 4*16 {
		t.Fatalf("complex %v, point = %d bytes; want 64", r.Complex(), l.PointBytes)
	}
	if f := r.Axis(l, 0); f != 10 {
		t.Errorf("first frequency %g, want 10", f)
	}
	if v := r.ComplexValue(l, 0, r.Index("v(in)")); v != 1 {
		t.Errorf("v(in) = %v, want 1 (ac 1)", v)
	}

	var out bytes.Buffer
	if err := Write(r, &out, Options{}); err != nil {
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
	if b.Schema.AxisKind != logb.AxisFrequency || b.Schema.AxisUnit != "Hz" {
		t.Errorf("axis kind %v unit %q, want frequency in Hz", b.Schema.AxisKind, b.Schema.AxisUnit)
	}
}

// TestConvertNgspiceOperatingPoint is the operating point path of the
// importer: one point, and no axis.
func TestConvertNgspiceOperatingPoint(t *testing.T) {
	raw := readRaw(t, "../testdata/ngspice.op.raw")

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
	if b.Schema.AxisKind != logb.AxisIndex || len(b.Schema.Fields) != 3 || b.Count != 1 {
		t.Fatalf("axis kind %v, %d fields, %d records; want index, 3, 1", b.Schema.AxisKind, len(b.Schema.Fields), b.Count)
	}
	if v, err := b.Value(0, 0); err != nil || v.(float64) != 5 {
		t.Errorf("v(in) = %v (%v), want 5", v, err)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Errorf("second batch: %v, want EOF", err)
	}
}

// TestLayoutMismatch: a block that does not match the layout is an error, not
// a misread.
func TestLayoutMismatch(t *testing.T) {
	data, err := os.ReadFile("../testdata/ngspice.op.raw")
	if err != nil {
		t.Fatal(err)
	}

	// Read as LTspice, the 24-byte point would be 16 bytes (f64 + 2 × f32)
	if _, err := ReadRawOptions(bytes.NewReader(data), ReadOptions{Dialect: DialectLTspice}); !errors.Is(err, ErrLongValues) {
		t.Errorf("ngspice file read as LTspice: %v, want ErrLongValues", err)
	}
	if _, err := ReadRaw(bytes.NewReader(append(data, make([]byte, 8)...))); !errors.Is(err, ErrLongValues) {
		t.Errorf("trailing bytes: %v, want ErrLongValues", err)
	}
	if _, err := ReadRaw(bytes.NewReader(data[:len(data)-8])); !errors.Is(err, ErrShortValues) {
		t.Errorf("missing bytes: %v, want ErrShortValues", err)
	}
}
