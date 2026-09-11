// Package spice imports SPICE raw files into Logb.
//
// It reads the raw files LTspice and ngspice write: the binary format, with
// the ASCII header of LTspice IV and ngspice or the UTF-16LE header of LTspice
// XVII, and the ASCII (Values:) format. It maps them onto the model SPEC.md
// §11 describes: the first variable becomes the axis, the rest become fields,
// the type column becomes a unit plus field metadata, and a stepped sweep's run
// boundaries become RUN frames instead of something the reader has to guess at.
//
// The quirks are this package's problem and not the format's. LTspice writes
// the axis variable as f64 even when every other variable is f32, and marks
// points by setting the sign bit of the time value, so its axis is read as an
// absolute value. ngspice writes every value as f64 without a flag saying so,
// and its axis can be negative (a DC sweep). `Flags: compressed` is LTspice's
// own scheme, which is refused rather than misread.
package spice

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Var is one entry of the raw file's Variables: block.
type Var struct {
	Index int
	Name  string
	Type  string // the SPICE type column: time, frequency, voltage, device_current, …
}

// Dialect is the program that wrote a raw file.
type Dialect int

const (
	DialectAuto    Dialect = iota // detect it from the header (ReadOptions only)
	DialectLTspice                // f32 values unless Flags: double
	DialectNgspice                // f64 values always
)

func (d Dialect) String() string {
	switch d {
	case DialectLTspice:
		return "LTspice"
	case DialectNgspice:
		return "ngspice"
	}
	return "auto"
}

// ReadOptions controls ReadRawOptions.
type ReadOptions struct {
	// Dialect overrides the detection, which takes a file whose Command: line
	// starts with "ngspice" for ngspice and any other file for LTspice.
	Dialect Dialect
}

// Raw is a parsed SPICE raw file: its header, and the binary block verbatim.
//
// Values is not decoded here. Decoding it needs the flags and the variable list,
// which is what Layout computes, and the importer streams over it once rather
// than materialising a matrix of float64 the way rveen/ltspice does. The values
// of an ASCII file are converted to the binary layout of an all-f64 file.
type Raw struct {
	Title    string
	Date     string
	Plotname string
	Command  string
	Flags    []string
	Vars     []Var
	Points   int
	Offset   float64
	Backanno []string
	Values   []byte

	// XVII reports whether the header was UTF-16LE, which is the only thing that
	// distinguishes an LTspice XVII file from an LTspice IV one.
	XVII bool

	// Dialect is the program that wrote the file.
	Dialect Dialect

	// ASCII reports a file in the ASCII (Values:) format.
	ASCII bool
}

var (
	// ErrNotRaw reports a file that does not begin like a SPICE raw file.
	ErrNotRaw = errors.New("spice: not a SPICE raw file")

	// ErrCompressed reports LTspice's own compression, which is undocumented.
	// It is refused rather than guessed at.
	ErrCompressed = errors.New("spice: compressed raw files are not supported")

	// ErrFastAccess reports the column-major rewrite LTspice calls fastaccess.
	// SPEC.md §8 maps it onto filter=transpose, but reading it back is a
	// different layout on disk and this importer does not implement it.
	ErrFastAccess = errors.New("spice: fastaccess (column-major) raw files are not supported")

	// ErrShortValues reports a binary block smaller than No. Points promises.
	ErrShortValues = errors.New("spice: binary block is shorter than the header claims")

	// ErrLongValues reports a binary block larger than No. Points promises: a
	// second plot in the same file, or values read with the wrong layout.
	ErrLongValues = errors.New("spice: binary block is longer than the header claims")
)

// Has reports whether a flag is set, case-insensitively.
func (r *Raw) Has(flag string) bool {
	for _, f := range r.Flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

// Complex reports whether every value is a (real, imaginary) pair.
func (r *Raw) Complex() bool { return r.Has("complex") }

// Double reports whether the file has the double flag.
func (r *Raw) Double() bool { return r.Has("double") }

// Wide reports whether the non-axis variables are stored as f64: in a file
// with the double flag, in an ngspice file, and in an ASCII file, whose values
// are converted to f64.
func (r *Raw) Wide() bool { return r.Double() || r.Dialect == DialectNgspice || r.ASCII }

// Stepped reports a .step sweep: several runs concatenated in one file.
func (r *Raw) Stepped() bool { return r.Has("stepped") }

// Layout describes how one point is stored in the binary block.
type Layout struct {
	// AxisBytes is the size of variable 0. It is 8 per component, always: the
	// axis is f64 even in a file with no double flag.
	AxisBytes int
	// VarBytes is the size of every other variable.
	VarBytes int
	// PointBytes is the size of one whole point.
	PointBytes int
	// Components is 1 for a real file and 2 for a complex one.
	Components int
}

// Layout computes the on-disk size of a point.
func (r *Raw) Layout() Layout {
	comp := 1
	if r.Complex() {
		comp = 2
	}
	varSize := 4
	if r.Wide() {
		varSize = 8
	}
	l := Layout{
		AxisBytes:  8 * comp,
		VarBytes:   varSize * comp,
		Components: comp,
	}
	if len(r.Vars) > 0 {
		l.PointBytes = l.AxisBytes + (len(r.Vars)-1)*l.VarBytes
	}
	return l
}

// Axis reads the axis quantity of point i, as a float64 in SPICE units. The
// imaginary part of a complex axis — an AC sweep's frequency — is dropped, and
// in an LTspice binary file the sign bit LTspice uses as a marker is not part
// of the value.
func (r *Raw) Axis(l Layout, i int) float64 {
	return r.axis(r.Values[i*l.PointBytes:])
}

// axis reads the axis quantity at the start of a point.
func (r *Raw) axis(point []byte) float64 {
	a := math.Float64frombits(binary.LittleEndian.Uint64(point))
	if r.Dialect == DialectLTspice && !r.ASCII {
		a = math.Abs(a)
	}
	return a
}

// Index returns the index of the variable with the given name, compared
// case-insensitively, or -1.
func (r *Raw) Index(name string) int {
	for i, v := range r.Vars {
		if strings.EqualFold(v.Name, name) {
			return i
		}
	}
	return -1
}

// Value returns variable v of point i as a float64 in SPICE units: the real
// part of a complex value. Variable 0 is returned as stored, without the sign
// correction Axis applies to an LTspice axis.
func (r *Raw) Value(l Layout, i, v int) float64 {
	return real(r.ComplexValue(l, i, v))
}

// ComplexValue returns variable v of point i. The imaginary part of a value in
// a real file is 0.
func (r *Raw) ComplexValue(l Layout, i, v int) complex128 {
	off, size := 0, l.AxisBytes
	if v > 0 {
		off, size = l.AxisBytes+(v-1)*l.VarBytes, l.VarBytes
	}
	b := r.Values[i*l.PointBytes+off:]
	width := size / l.Components
	re := float(b, width)
	if l.Components == 1 {
		return complex(re, 0)
	}
	return complex(re, float(b[width:], width))
}

// float reads a little-endian f32 or f64.
func float(b []byte, width int) float64 {
	if width == 8 {
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	}
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
}

// ReadRaw parses a SPICE raw file, detecting its dialect.
func ReadRaw(rd io.Reader) (*Raw, error) {
	return ReadRawOptions(rd, ReadOptions{})
}

// ReadRawOptions parses a SPICE raw file.
func ReadRawOptions(rd io.Reader, o ReadOptions) (*Raw, error) {
	br := bufio.NewReaderSize(rd, 1<<16)

	// LTspice IV and ngspice write an ASCII header, LTspice XVII a UTF-16LE one.
	// Two bytes tell them apart: every raw file starts with "Title:", so a NUL
	// in the second byte is the UTF-16 high half of 'T'.
	probe, err := br.Peek(2)
	if err != nil || probe[0] != 'T' {
		return nil, ErrNotRaw
	}
	r := &Raw{XVII: probe[1] == 0}

	lines, ascii, err := readHeader(br, r.XVII)
	if err != nil {
		return nil, err
	}
	if err := r.parseHeader(lines); err != nil {
		return nil, err
	}

	r.Dialect = o.Dialect
	if r.Dialect == DialectAuto {
		r.Dialect = DialectLTspice
		if strings.HasPrefix(strings.ToLower(r.Command), "ngspice") {
			r.Dialect = DialectNgspice
		}
	}

	if ascii {
		r.ASCII = true
		r.Values, err = r.readASCII(br)
	} else {
		r.Values, err = io.ReadAll(br)
	}
	if err != nil {
		return nil, err
	}

	want := r.Points * r.Layout().PointBytes
	switch got := len(r.Values); {
	case got < want:
		return nil, fmt.Errorf("%w: %d points × %d bytes = %d, got %d",
			ErrShortValues, r.Points, r.Layout().PointBytes, want, got)
	case got > want:
		return nil, fmt.Errorf("%w: %d points × %d bytes = %d, got %d",
			ErrLongValues, r.Points, r.Layout().PointBytes, want, got)
	}
	return r, nil
}

// readHeader returns the header lines, consuming the reader up to and including
// the "Binary:" or "Values:" line, and whether the file is in the ASCII format.
func readHeader(br *bufio.Reader, utf16le bool) ([]string, bool, error) {
	var lines []string
	for {
		line, err := readLine(br, utf16le)
		if err != nil {
			return nil, false, err
		}
		switch strings.TrimRight(line, "\r\n") {
		case "Binary:":
			return lines, false, nil
		case "Values:":
			return lines, true, nil
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if len(lines) > 1<<20 {
			return nil, false, ErrNotRaw
		}
	}
}

func readLine(br *bufio.Reader, utf16le bool) (string, error) {
	if !utf16le {
		s, err := br.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("spice: premature end of header: %w", err)
		}
		return s, nil
	}
	var u []uint16
	var b [2]byte
	for {
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return "", fmt.Errorf("spice: premature end of header: %w", err)
		}
		c := binary.LittleEndian.Uint16(b[:])
		u = append(u, c)
		if c == '\n' {
			return string(utf16.Decode(u)), nil
		}
	}
}

// readASCII converts the Values: block of an ASCII raw file to the binary
// layout of an all-f64 file. Each point is its index followed by one value per
// variable; a complex value is written as "re,im".
func (r *Raw) readASCII(br *bufio.Reader) ([]byte, error) {

	data, err := io.ReadAll(br)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if r.XVII {
		u := make([]uint16, len(data)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(data[2*i:])
		}
		text = string(utf16.Decode(u))
	}

	tok := strings.Fields(text)
	per := 1 + len(r.Vars)
	switch want := r.Points * per; {
	case len(tok) < want:
		return nil, fmt.Errorf("%w: %d points × %d fields = %d, got %d", ErrShortValues, r.Points, per, want, len(tok))
	case len(tok) > want:
		return nil, fmt.Errorf("%w: %d points × %d fields = %d, got %d", ErrLongValues, r.Points, per, want, len(tok))
	}

	complexValues := r.Complex()
	out := make([]byte, 0, r.Points*len(r.Vars)*16)
	for p := 0; p < r.Points; p++ {
		t := tok[p*per : (p+1)*per]
		if i, err := strconv.Atoi(t[0]); err != nil || i != p {
			return nil, fmt.Errorf("spice: ASCII values: point %d has index %q", p, t[0])
		}
		for v, s := range t[1:] {
			re, im, hasIm := strings.Cut(s, ",")
			if complexValues != hasIm {
				return nil, fmt.Errorf("spice: ASCII values: point %d, %s: %q", p, r.Vars[v].Name, s)
			}
			parts := []string{re}
			if hasIm {
				parts = append(parts, im)
			}
			for _, x := range parts {
				f, err := strconv.ParseFloat(x, 64)
				if err != nil {
					return nil, fmt.Errorf("spice: ASCII values: point %d, %s: %w", p, r.Vars[v].Name, err)
				}
				out = binary.LittleEndian.AppendUint64(out, math.Float64bits(f))
			}
		}
	}
	return out, nil
}

func (r *Raw) parseHeader(lines []string) error {
	for i := 0; i < len(lines); i++ {
		s := lines[i]
		key, val, ok := strings.Cut(s, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)

		switch key {
		case "Title":
			r.Title = val
		case "Date":
			r.Date = val
		case "Plotname":
			r.Plotname = val
		case "Command":
			r.Command = val
		case "Backannotation":
			r.Backanno = append(r.Backanno, val)
		case "Flags":
			r.Flags = strings.Fields(val)
		case "No. Variables":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("spice: bad No. Variables %q: %w", val, err)
			}
			r.Vars = make([]Var, 0, n)
		case "No. Points":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("spice: bad No. Points %q: %w", val, err)
			}
			r.Points = n
		case "Offset":
			// Not fatal: a header that omits or mangles it costs an offset, not
			// the file.
			r.Offset, _ = strconv.ParseFloat(val, 64)
		case "Variables":
			n := cap(r.Vars)
			for j := 0; j < n && i+1 < len(lines); j++ {
				i++
				f := strings.Split(strings.TrimLeft(lines[i], "\t "), "\t")
				if len(f) < 2 {
					return fmt.Errorf("spice: bad variable line %q", lines[i])
				}
				idx, _ := strconv.Atoi(strings.TrimSpace(f[0]))
				v := Var{Index: idx, Name: strings.TrimSpace(f[1])}
				if len(f) > 2 {
					v.Type = strings.TrimSpace(f[2])
				}
				r.Vars = append(r.Vars, v)
			}
		}
	}

	if len(r.Vars) == 0 {
		return fmt.Errorf("%w: no variables", ErrNotRaw)
	}
	if r.Has("compressed") {
		return ErrCompressed
	}
	if r.Has("fastaccess") {
		return ErrFastAccess
	}
	return nil
}

// typeName is the first word of a type column in lower case: ngspice appends
// attributes such as "grid=3" or "dims=0".
func typeName(t string) string {
	f := strings.Fields(strings.ToLower(t))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
