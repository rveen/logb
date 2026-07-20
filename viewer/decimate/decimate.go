// Package decimate reduces a decoded series to something a screen can show.
//
// A chart is at most a couple of thousand pixels wide. Sending it ten million
// points wastes bandwidth to draw the same columns of ink, so a range is
// reduced to one bucket per pixel carrying the minimum and maximum seen there.
// That preserves what a naive stride would throw away: a single-sample spike
// survives decimation, which is usually the thing the engineer opened the file
// to find.
//
// Two reductions live here because two kinds of signal live in a Logb file.
// A numeric field gets a min/max envelope. A categorical one — an enumeration
// under value_to_text, or a bool — does not: the mean of "reverse" and "third"
// is not a gear, and a line drawn between them implies the vehicle passed
// through the states in between. Those get run-length encoded into states.
package decimate

import (
	"math"
	"sort"

	"github.com/rveen/logb/viewer/index"
)

// Envelope is a decimated numeric series, one entry per bucket.
//
// N is not decoration. Where it is zero the bucket contains no present sample
// and the chart must break the line rather than joining across it: a guarded
// field that was absent, or a stretch of the axis the stream did not cover,
// are both real facts about the recording (SPEC §6.2).
type Envelope struct {
	X   []float64 `json:"x"`
	Min []float64 `json:"min"`
	Max []float64 `json:"max"`
	N   []int32   `json:"n"`
	// Exact is true when the range held few enough samples that no reduction
	// happened and X/Min/Max are the samples themselves.
	Exact bool `json:"exact"`
}

// State is one run of a categorical field: the value held from X0 until X1.
type State struct {
	X0    float64 `json:"x0"`
	X1    float64 `json:"x1"`
	Raw   float64 `json:"raw"`
	Label string  `json:"label"`
	// Mixed means the bucket held more than one distinct value and was too
	// narrow to resolve. Picking one of them to display would invent a fact,
	// so the UI hatches the band instead.
	Mixed bool `json:"mixed"`
	// Absent means the field was not present over this run.
	Absent bool `json:"absent"`
}

// Scale says how the axis is divided into buckets.
type Scale int

const (
	// Linear divides the range evenly. Correct for time, which SPEC §5.3
	// guarantees is never logarithmic.
	Linear Scale = iota
	// Log divides evenly in log space. A decade sweep bucketed linearly would
	// put nine tenths of its buckets in the last decade and render the first
	// decade as a single column.
	Log
)

// Range finds the half-open index range [lo, hi) of samples with from <= x <= to.
//
// The axis of a series is non-decreasing: batches are appended in file order
// and the format requires a stream's runs to be contiguous per segment, so a
// binary search is sound.
func Range(s *index.Series, from, to float64) (int, int) {
	n := s.Axis.Len()
	lo := sort.Search(n, func(i int) bool { return s.Axis.At(i) >= from })
	hi := sort.Search(n, func(i int) bool { return s.Axis.At(i) > to })
	return lo, hi
}

// Numeric reduces a range of a numeric series to at most buckets points.
//
// When the range holds no more samples than buckets, the samples are returned
// as they are and Exact is set. That matters: at high zoom the engineer is
// looking at individual samples, and a bucket centre would put them at
// positions they were never recorded at.
func Numeric(s *index.Series, from, to float64, buckets int, scale Scale) Envelope {
	lo, hi := Range(s, from, to)
	if buckets < 1 {
		buckets = 1
	}

	if hi-lo <= buckets {
		e := Envelope{Exact: true}
		for i := lo; i < hi; i++ {
			e.X = append(e.X, s.Axis.At(i))
			if s.Present[i] {
				e.Min = append(e.Min, s.Vals[i])
				e.Max = append(e.Max, s.Vals[i])
				e.N = append(e.N, 1)
			} else {
				// Absent, not zero. The value is meaningless and must not be
				// carried to the client where it could be drawn.
				e.Min = append(e.Min, math.NaN())
				e.Max = append(e.Max, math.NaN())
				e.N = append(e.N, 0)
			}
		}
		return e
	}

	pos := newPos(from, to, buckets, scale)
	e := Envelope{
		X:   make([]float64, buckets),
		Min: make([]float64, buckets),
		Max: make([]float64, buckets),
		N:   make([]int32, buckets),
	}
	for b := 0; b < buckets; b++ {
		e.X[b] = pos.centre(b)
		e.Min[b], e.Max[b] = math.NaN(), math.NaN()
	}
	for i := lo; i < hi; i++ {
		if !s.Present[i] {
			continue
		}
		b := pos.bucket(s.Axis.At(i))
		if b < 0 || b >= buckets {
			continue
		}
		v := s.Vals[i]
		if e.N[b] == 0 {
			e.Min[b], e.Max[b] = v, v
		} else {
			if v < e.Min[b] {
				e.Min[b] = v
			}
			if v > e.Max[b] {
				e.Max[b] = v
			}
		}
		e.N[b]++
	}
	return e
}

// Categorical reduces a range of an enumerated or boolean series to runs.
//
// Adjacent samples holding the same raw value collapse into one state. If that
// still leaves more runs than the screen can show, the range is bucketed and
// any bucket holding more than one distinct value is marked Mixed rather than
// having one of its values chosen arbitrarily.
func Categorical(s *index.Series, f *index.Field, from, to float64, buckets int, scale Scale) []State {
	lo, hi := Range(s, from, to)
	if buckets < 1 {
		buckets = 1
	}

	runs := runLength(s, f, lo, hi, to)
	if len(runs) <= buckets {
		return runs
	}

	pos := newPos(from, to, buckets, scale)
	out := make([]State, buckets)
	seen := make([]bool, buckets)
	for b := 0; b < buckets; b++ {
		out[b] = State{X0: pos.edge(b), X1: pos.edge(b + 1), Absent: true}
	}
	for i := lo; i < hi; i++ {
		b := pos.bucket(s.Axis.At(i))
		if b < 0 || b >= buckets {
			continue
		}
		if !s.Present[i] {
			continue
		}
		if !seen[b] {
			seen[b] = true
			out[b].Absent = false
			out[b].Raw = s.Vals[i]
			out[b].Label = f.Label(s.Vals[i])
			continue
		}
		if out[b].Raw != s.Vals[i] {
			out[b].Mixed = true
			out[b].Label = ""
		}
	}
	return out
}

// runLength collapses adjacent equal samples into runs.
//
// A run ends where the next sample differs, so its X1 is the next sample's
// position: the state held right up to the moment it changed. The final run
// extends to the end of the requested range, which is the honest reading —
// nothing in the file says the state ended before the data did.
func runLength(s *index.Series, f *index.Field, lo, hi int, to float64) []State {
	var out []State
	for i := lo; i < hi; i++ {
		x := s.Axis.At(i)
		absent := !s.Present[i]
		var raw float64
		if !absent {
			raw = s.Vals[i]
		}
		if n := len(out); n > 0 && out[n-1].Absent == absent && (absent || out[n-1].Raw == raw) {
			out[n-1].X1 = x
			continue
		}
		if n := len(out); n > 0 {
			out[n-1].X1 = x
		}
		st := State{X0: x, X1: x, Raw: raw, Absent: absent}
		if !absent {
			st.Label = f.Label(raw)
		}
		out = append(out, st)
	}
	if n := len(out); n > 0 {
		out[n-1].X1 = to
	}
	return out
}

// positioner maps axis values to bucket indices and back.
type positioner struct {
	from, to float64
	buckets  int
	log      bool
	lf, lt   float64 // log10 of from and to, when logarithmic
}

func newPos(from, to float64, buckets int, scale Scale) positioner {
	p := positioner{from: from, to: to, buckets: buckets}
	// A log axis needs a strictly positive range. Falling back to linear is
	// better than producing NaN bucket edges for a sweep that happens to
	// include or cross zero.
	if scale == Log && from > 0 && to > from {
		p.log = true
		p.lf, p.lt = math.Log10(from), math.Log10(to)
	}
	return p
}

func (p positioner) frac(x float64) float64 {
	if p.log {
		if x <= 0 {
			return -1
		}
		return (math.Log10(x) - p.lf) / (p.lt - p.lf)
	}
	if p.to == p.from {
		return 0
	}
	return (x - p.from) / (p.to - p.from)
}

func (p positioner) bucket(x float64) int {
	f := p.frac(x)
	if f < 0 {
		return -1
	}
	b := int(f * float64(p.buckets))
	if b >= p.buckets {
		// The sample sitting exactly on the upper bound belongs to the last
		// bucket, not to one past the end.
		b = p.buckets - 1
	}
	return b
}

func (p positioner) edge(b int) float64 {
	f := float64(b) / float64(p.buckets)
	if p.log {
		return math.Pow(10, p.lf+f*(p.lt-p.lf))
	}
	return p.from + f*(p.to-p.from)
}

func (p positioner) centre(b int) float64 {
	return (p.edge(b) + p.edge(b+1)) / 2
}
