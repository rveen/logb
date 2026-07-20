// Package index turns a Logb file into a model a viewer can draw.
//
// Phase 1 is deliberately the simple thing: one full sequential pass that
// decodes every record into memory. That is correct for any file, and fast
// enough up to roughly 50 MB. The random-access and decimation machinery that
// makes 1 GB workable is layered on later; this package defines the model those
// layers have to produce, so it is worth getting the shape right now.
//
// Three things here are not incidental and must survive later optimisation:
//
//   - An absent sample is not a zero. A guarded field whose guard does not hold
//     returns logb.ErrFieldAbsent, and that is data, not an error (SPEC §6.2).
//     It is recorded in Present and must reach the screen as a gap.
//   - The axis is a tagged union. Time axes are int64 ticks; every other kind is
//     IEEE f64. Reading one as the other yields plausible nonsense.
//   - Runs are separate series. A stepped sweep is N traces sharing an axis, not
//     one trace (SPEC §6.5).
package index

import (
	"fmt"
	"math"
	"strconv"

	"github.com/rveen/logb"
)

// Class says how a field wants to be drawn. It is decided once, at index time,
// from the field's type and conversion.
type Class string

const (
	// ClassNumeric is a line chart: min/max envelopes are meaningful.
	ClassNumeric Class = "numeric"
	// ClassCategorical is a state band. Enumerated values, where min/max is
	// meaningless and interpolating between two states is a lie.
	ClassCategorical Class = "categorical"
	// ClassEvent is an event lane: a mark at each record, labelled with the
	// field's text. Strings and byte blobs have no numeric value to put on a
	// y-axis, but they do have a position on the axis and something to say
	// there, and a log's most interesting stream is often exactly that.
	ClassEvent Class = "event"
	// ClassBlob is not plottable at all. Complex values have no single position
	// to draw and no honest text rendering; reducing one to a magnitude would
	// be inventing a quantity the recording does not contain. Visible in the
	// tree and the record table, never a trace.
	ClassBlob Class = "blob"
)

// classify decides how a field is drawn.
//
// The order matters: a bool is categorical whatever its conversion, and a
// text conversion makes a field categorical whatever its underlying integer
// type. Table and TableInterp stay numeric — they are calibration curves, not
// enumerations.
func classify(fd *logb.Field) Class {
	switch fd.Type {
	case logb.TypeBytes, logb.TypeString:
		return ClassEvent
	case logb.TypeComplex:
		return ClassBlob
	case logb.TypeBool:
		return ClassCategorical
	}
	switch fd.Conv.(type) {
	case logb.ValueToText, logb.RangeToText:
		return ClassCategorical
	}
	return ClassNumeric
}

// Field is one field of one stream, with everything the UI needs to decide how
// to present it.
type Field struct {
	Index     int // index into the underlying logb.Schema.Fields
	Name      string
	Unit      string
	Desc      string
	Type      string
	Class     Class
	Guarded   bool // values may legitimately be absent; the UI shows this as "sparse"
	IsAxis    bool // this field carries the independent variable (AxisExplicit)
	BitOffset uint32
	BitWidth  uint32
	BigEndian bool
	Variable  bool
	Conv      string            // human-readable conversion description
	Meta      map[string]string // field-level metadata (SPEC's third level)

	conv logb.Conversion // retained for label lookup on categorical fields
}

// Label renders a raw value the way this field means it. For a categorical
// field that is the enumerated text; for anything else it is the number.
func (f *Field) Label(raw float64) string {
	if f.conv != nil {
		if s, ok := f.conv.Apply(raw).(string); ok {
			return s
		}
	}
	if f.Type == "bool" {
		if raw != 0 {
			return "true"
		}
		return "false"
	}
	return strconv.FormatFloat(raw, 'g', -1, 64)
}

// Axis holds the independent variable for a series.
//
// It is a tagged union because the format's is: for AxisTime the quantity is an
// int64 count of ticks of 10^AxisExp seconds, and for every other axis kind it
// is an IEEE f64 in AxisUnit (SPEC §5). Ticks are stored epoch-relative, so
// they stay small and exact rather than being ~1.7e18 absolute nanoseconds.
type Axis struct {
	Time  bool
	Ticks []int64   // AxisTime: epoch-relative ticks
	Float []float64 // every other kind: the value in AxisUnit
}

func (a *Axis) Len() int {
	if a.Time {
		return len(a.Ticks)
	}
	return len(a.Float)
}

// At returns position i as a float64 for bucketing and display.
//
// For a time axis this is exact: epoch-relative ticks stay far below 2^53 for
// any realistic recording (2^53 ns is 104 days), which is precisely why the
// epoch is subtracted at index time rather than at the edge.
func (a *Axis) At(i int) float64 {
	if a.Time {
		return float64(a.Ticks[i])
	}
	return a.Float[i]
}

// Series is one field of one stream for one run, fully decoded.
//
// Vals always holds the *raw* value for categorical fields and the *converted*
// physical value for numeric ones. Present is not decoration: where it is false
// the field was absent from that record, and the value in Vals is meaningless.
type Series struct {
	Axis    Axis
	Vals    []float64
	Present []bool
}

func (s *Series) Len() int { return len(s.Vals) }

// Run is one run of a stream, with the parameters that distinguish it.
type Run struct {
	ID     uint32
	Index  uint32
	Params map[string]string
}

// Label renders a run for the signal tree. A stepped sweep is only legible if
// the parameter that was stepped is on screen.
func (r *Run) Label() string {
	if len(r.Params) == 0 {
		return fmt.Sprintf("run %d", r.ID)
	}
	s := ""
	for k, v := range r.Params {
		if s != "" {
			s += " "
		}
		s += k + "=" + v
	}
	return s
}

// Stream is one Logb stream, accumulated across every segment it appears in.
//
// Identity is the UUID, never the stream_id: stream_id is segment-scoped and is
// rebound by every SYNC frame, which is exactly what makes concatenation work
// (SPEC §6.6).
type Stream struct {
	UUID     string
	Name     string
	AxisKind string
	AxisMode string
	AxisExp  int8
	AxisUnit string
	Fields   []Field
	Runs     []*Run
	Meta     map[string]string
	Records  int

	// AxisMin and AxisMax bound every series in the stream, in the same units
	// Axis.At reports: epoch-relative ticks for time, the axis unit otherwise.
	// This is what the UI opens the chart on.
	AxisMin, AxisMax float64
	HasSpan          bool

	// FrameList is this stream's DATA frames in file order, filled once the
	// scan finishes and the axis has been rebased.
	FrameList []DataFrame

	// stats is Tier 1, indexed [frame ordinal][field index], parallel to
	// FrameList. Decoded samples are deliberately not kept: holding them is
	// what makes a large file impossible, and everything a chart needs at low
	// zoom is answerable from these.
	stats [][]Stat
}

// Stat returns the Tier 1 summary of one field over one of this stream's
// frames. Ordinals index FrameList.
func (s *Stream) Stat(frame, field int) Stat {
	if frame < 0 || frame >= len(s.stats) || field < 0 || field >= len(s.stats[frame]) {
		return Stat{}
	}
	return s.stats[frame][field]
}

// Frames returns the frames of this stream belonging to a run.
//
// Runs are never merged: a stepped sweep is N traces sharing an axis, not one
// trace (SPEC §6.5), and each frame carries exactly one run id.
func (s *Stream) Frames(run uint32) []DataFrame {
	out := make([]DataFrame, 0, len(s.FrameList))
	for _, f := range s.FrameList {
		if f.RunID == run {
			out = append(out, f)
		}
	}
	return out
}

// FrameOrdinal finds a frame's position in FrameList by file offset, so its
// Tier 1 statistics can be looked up. Returns -1 if absent.
func (s *Stream) FrameOrdinal(offset uint64) int {
	for i := range s.FrameList {
		if s.FrameList[i].Offset == offset {
			return i
		}
	}
	return -1
}

// File is a whole indexed Logb file.
type File struct {
	Path        string
	Size        int64
	Epoch       int64 // absolute ticks of the earliest time-axis sample in the file
	HasEpoch    bool
	Streams     []*Stream
	Meta        []logb.Meta
	Attachments []Attachment

	// Frames is the Tier 0 index: where every DATA frame is and what it holds.
	// Pair it with NewAccessor to decode ranges without rescanning.
	Frames *FrameIndex

	// Cached means this model came from a sidecar rather than a fresh scan.
	Cached bool
	// Extended means the file had grown since the cache was written and only
	// the new tail was scanned.
	Extended bool

	// Truncated means the scan stopped at damage rather than at a clean end.
	// Under rule 2 that is still a valid file and every record already read
	// stands, so this is a banner in the UI, not an error.
	Truncated bool
	// Closed means an END frame was seen. Note it does not mean end of file:
	// a concatenated file has END frames in the middle (SPEC §6.6).
	Closed bool
	// Unsupported lists streams or frames that were framed and understood but
	// could not be decoded — a codec or axis mode from a later version.
	Unsupported []string
}

// Attachment is a file carried inside the log, such as the DBC that describes
// its CAN payloads. The viewer surfaces it but never parses it.
type Attachment struct {
	Name string
	Size int
	Data []byte
}

// Records is the total number of records across every stream.
func (f *File) Records() int {
	n := 0
	for _, s := range f.Streams {
		n += s.Records
	}
	return n
}

// Stream returns the stream with the given UUID, or nil.
func (f *File) Stream(uuid string) *Stream {
	for _, s := range f.Streams {
		if s.UUID == uuid {
			return s
		}
	}
	return nil
}

// asFloat coerces a decoded value to a float64 for plotting.
//
// The core's own toFloat is unexported, and this needs to handle exactly the
// types logb.Value can return. Anything not plottable returns false rather than
// a zero, so a caller cannot mistake "not a number" for "the number zero".
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case uint64:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func axisKindName(k logb.AxisKind) string {
	switch k {
	case logb.AxisTime:
		return "time"
	case logb.AxisFrequency:
		return "frequency"
	case logb.AxisAngle:
		return "angle"
	case logb.AxisDistance:
		return "distance"
	case logb.AxisIndex:
		return "index"
	}
	return "other"
}

func axisModeName(m logb.AxisMode) string {
	switch m {
	case logb.AxisImplicit:
		return "implicit"
	case logb.AxisExplicit:
		return "explicit"
	case logb.AxisLog:
		return "log"
	}
	return "unknown"
}

// convDesc describes a conversion for the UI, mirroring what cmd/logbdump
// prints so the two tools agree about what a file says.
func convDesc(c logb.Conversion) string {
	switch v := c.(type) {
	case nil:
		return "identity"
	case logb.Identity:
		return "identity"
	case logb.Linear:
		return fmt.Sprintf("linear %g + %g*x", v.A, v.B)
	case logb.Rational:
		return "rational"
	case logb.Table:
		if v.Interp {
			return fmt.Sprintf("table_interp (%d points)", len(v.Keys))
		}
		return fmt.Sprintf("table (%d points)", len(v.Keys))
	case logb.ValueToText:
		return fmt.Sprintf("value_to_text (%d values)", len(v.Keys))
	case logb.RangeToText:
		return fmt.Sprintf("range_to_text (%d ranges)", len(v.Texts))
	}
	return "unknown"
}

// finite reports whether a value can be sent as JSON. NaN and ±Inf are not
// representable and would serialise as invalid JSON, so they are treated as
// absent — which is also what they mean on a chart.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
