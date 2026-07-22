// Package spice imports SPICE raw files into Logb.
//
// It reads the binary raw file LTspice writes — the ASCII-header form of LTspice
// IV and the UTF-16LE form of XVII — and maps it onto the model SPEC.md §11
// describes: the first variable becomes the axis, the rest become fields, the
// type column becomes a unit plus field metadata, and a stepped sweep's run
// boundaries become RUN frames instead of something the reader has to guess at.
//
// The quirks are this package's problem and not the format's. The axis variable
// is f64 even when every other variable is f32; LTspice marks points by setting
// the sign bit of the time value, so time is read as an absolute value; and
// `Flags: compressed` is LTspice's own scheme, which is refused rather than
// misread.
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

// Raw is a parsed SPICE raw file: its header, and the binary block verbatim.
//
// Values is not decoded here. Decoding it needs the flags and the variable list,
// which is what Layout computes, and the importer streams over it once rather
// than materialising a matrix of float64 the way rveen/ltspice does.
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

// Double reports whether the non-axis variables are f64 rather than f32.
func (r *Raw) Double() bool { return r.Has("double") }

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
	if r.Double() {
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
// the sign bit LTspice uses as a marker is not part of the value.
func (r *Raw) Axis(l Layout, i int) float64 {
	return math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(r.Values[i*l.PointBytes:])))
}

// ReadRaw parses a SPICE raw file.
func ReadRaw(rd io.Reader) (*Raw, error) {
	br := bufio.NewReaderSize(rd, 1<<16)

	// LTspice IV writes an ASCII header, XVII a UTF-16LE one. Two bytes tell
	// them apart: every raw file starts with "Title:", so a NUL in the second
	// byte is the UTF-16 high half of 'T'.
	probe, err := br.Peek(2)
	if err != nil || probe[0] != 'T' {
		return nil, ErrNotRaw
	}
	r := &Raw{XVII: probe[1] == 0}

	lines, err := readHeader(br, r.XVII)
	if err != nil {
		return nil, err
	}
	if err := r.parseHeader(lines); err != nil {
		return nil, err
	}

	vals, err := io.ReadAll(br)
	if err != nil {
		return nil, err
	}
	r.Values = vals

	want := r.Points * r.Layout().PointBytes
	if len(vals) < want {
		return nil, fmt.Errorf("%w: %d points × %d bytes = %d, got %d",
			ErrShortValues, r.Points, r.Layout().PointBytes, want, len(vals))
	}
	return r, nil
}

// readHeader returns the header lines, consuming the reader up to and including
// the "Binary:" line.
func readHeader(br *bufio.Reader, utf16le bool) ([]string, error) {
	var lines []string
	for {
		line, err := readLine(br, utf16le)
		if err != nil {
			return nil, err
		}
		if strings.TrimRight(line, "\r\n") == "Binary:" {
			return lines, nil
		}
		if strings.TrimRight(line, "\r\n") == "Values:" {
			return nil, errors.New("spice: ASCII (Values:) raw files are not supported; this reads binary raw files")
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if len(lines) > 1<<20 {
			return nil, ErrNotRaw
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
