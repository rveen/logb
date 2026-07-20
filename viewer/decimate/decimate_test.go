package decimate

import (
	"math"
	"testing"

	"github.com/rveen/logb/viewer/index"
)

// series builds a synthetic time series. A nil present slice means all present.
func series(vals []float64, present []bool) *index.Series {
	s := &index.Series{Axis: index.Axis{Time: true}}
	for i, v := range vals {
		s.Axis.Ticks = append(s.Axis.Ticks, int64(i))
		p := present == nil || present[i]
		if !p {
			v = math.NaN()
		}
		s.Vals = append(s.Vals, v)
		s.Present = append(s.Present, p)
	}
	return s
}

// TestSpikeSurvives is the whole reason for min/max rather than striding.
// One anomalous sample in ten thousand must still reach the screen — it is
// usually the thing the file was opened to find.
func TestSpikeSurvives(t *testing.T) {
	vals := make([]float64, 10000)
	for i := range vals {
		vals[i] = 1
	}
	vals[6543] = 999

	e := Numeric(series(vals, nil), 0, 9999, 100, Linear)
	if e.Exact {
		t.Fatal("10000 samples into 100 buckets should have been reduced")
	}
	var peak float64
	for _, m := range e.Max {
		if m > peak {
			peak = m
		}
	}
	if peak != 999 {
		t.Errorf("peak = %v, want 999: the spike was decimated away", peak)
	}
}

// TestAbsentBucketsAreGaps checks that a stretch of absent samples produces
// buckets with N=0 and NaN bounds, so the client breaks the line instead of
// drawing a value the recording never contained.
func TestAbsentBucketsAreGaps(t *testing.T) {
	vals := make([]float64, 1000)
	present := make([]bool, 1000)
	for i := range vals {
		vals[i] = float64(i)
		present[i] = i < 300 || i >= 700
	}

	e := Numeric(series(vals, present), 0, 999, 10, Linear)
	for b, n := range e.N {
		gap := b >= 3 && b < 7
		if gap && n != 0 {
			t.Errorf("bucket %d: n = %d, want 0 (samples are absent there)", b, n)
		}
		if gap && (!math.IsNaN(e.Min[b]) || !math.IsNaN(e.Max[b])) {
			t.Errorf("bucket %d: bounds %v..%v, want NaN", b, e.Min[b], e.Max[b])
		}
		if !gap && n == 0 {
			t.Errorf("bucket %d: n = 0, want samples", b)
		}
	}
}

// TestExactBelowBudget checks that a range with fewer samples than buckets is
// returned untouched. At that zoom the user is looking at individual samples,
// and moving them to bucket centres would put them where they never happened.
func TestExactBelowBudget(t *testing.T) {
	s := series([]float64{5, 7, 9}, nil)
	e := Numeric(s, 0, 2, 100, Linear)
	if !e.Exact {
		t.Fatal("3 samples into 100 buckets should be exact")
	}
	if len(e.X) != 3 || e.X[0] != 0 || e.X[2] != 2 {
		t.Errorf("x = %v, want the sample positions", e.X)
	}
	if e.Min[1] != 7 || e.Max[1] != 7 {
		t.Errorf("sample 1 = %v..%v, want 7..7", e.Min[1], e.Max[1])
	}
}

// TestRangeIsInclusive pins the search bounds, which decide whether the sample
// sitting exactly on a zoom edge is drawn.
func TestRangeIsInclusive(t *testing.T) {
	s := series([]float64{0, 1, 2, 3, 4}, nil)
	lo, hi := Range(s, 1, 3)
	if lo != 1 || hi != 4 {
		t.Errorf("Range(1,3) = %d,%d, want 1,4", lo, hi)
	}
}

func TestLogBucketing(t *testing.T) {
	// A three-decade sweep. Bucketed linearly, everything below 100 would land
	// in the first bucket; in log space the decades divide evenly.
	s := &index.Series{Axis: index.Axis{Time: false}}
	for i := 0; i < 3000; i++ {
		x := math.Pow(10, float64(i)/1000)
		s.Axis.Float = append(s.Axis.Float, x)
		s.Vals = append(s.Vals, x)
		s.Present = append(s.Present, true)
	}

	e := Numeric(s, 1, 1000, 30, Log)
	for b, n := range e.N {
		if n == 0 {
			t.Fatalf("log bucket %d empty: the sweep was bucketed linearly", b)
		}
	}
	// Each bucket spans a tenth of a decade, so the first must stay near 1.
	if e.X[0] > 1.2 {
		t.Errorf("first log bucket centre = %v, want near 1", e.X[0])
	}
}

func categoricalField() *index.Field { return &index.Field{Name: "Gear", Type: "uint"} }

// TestRunLength checks that adjacent equal states collapse and that a run is
// held until the moment it changes.
func TestRunLength(t *testing.T) {
	s := series([]float64{1, 1, 1, 2, 2, 3}, nil)
	got := Categorical(s, categoricalField(), 0, 5, 100, Linear)
	if len(got) != 3 {
		t.Fatalf("runs = %d, want 3", len(got))
	}
	want := []struct{ x0, x1, raw float64 }{{0, 3, 1}, {3, 5, 2}, {5, 5, 3}}
	for i, w := range want {
		if got[i].X0 != w.x0 || got[i].X1 != w.x1 || got[i].Raw != w.raw {
			t.Errorf("run %d = %+v, want x0=%v x1=%v raw=%v", i, got[i], w.x0, w.x1, w.raw)
		}
	}
}

// TestMixedRatherThanArbitrary is the categorical counterpart of the spike
// test. When a bucket is too narrow to resolve, the answer is "more than one
// state happened here", never one of them picked arbitrarily.
func TestMixedRatherThanArbitrary(t *testing.T) {
	vals := make([]float64, 1000)
	for i := range vals {
		vals[i] = float64(i % 7) // changes every sample, so runs cannot collapse
	}
	got := Categorical(series(vals, nil), categoricalField(), 0, 999, 10, Linear)
	if len(got) != 10 {
		t.Fatalf("states = %d, want 10 buckets", len(got))
	}
	for i, st := range got {
		if !st.Mixed {
			t.Errorf("bucket %d not marked mixed, label = %q", i, st.Label)
		}
		if st.Label != "" {
			t.Errorf("bucket %d has label %q: a mixed bucket must not name one of its states", i, st.Label)
		}
	}
}

// TestAbsentRunsAreDistinct checks that absence is its own state rather than
// being folded into whichever value happened to sit beside it.
func TestAbsentRunsAreDistinct(t *testing.T) {
	present := []bool{true, true, false, false, true}
	got := Categorical(series([]float64{1, 1, 0, 0, 1}, present), categoricalField(), 0, 4, 100, Linear)
	if len(got) != 3 {
		t.Fatalf("runs = %d, want 3 (value, absent, value)", len(got))
	}
	if got[1].Absent != true || got[0].Absent || got[2].Absent {
		t.Errorf("absence pattern = %v,%v,%v, want false,true,false",
			got[0].Absent, got[1].Absent, got[2].Absent)
	}
}
