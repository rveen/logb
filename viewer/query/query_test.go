package query

import (
	"math"
	"testing"

	"github.com/rveen/logb/viewer/index"
)

const testFile = "../../testdata/can-example.logb"

func newTestQuery(t *testing.T) *Query {
	t.Helper()
	f, err := index.Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := index.NewAccessor(testFile, f.Frames)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acc.Close() })
	return New(f, acc)
}

func stream(t *testing.T, q *Query, name string) *index.Stream {
	t.Helper()
	for _, s := range q.File.Streams {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no stream %q", name)
	return nil
}

func field(t *testing.T, st *index.Stream, name string) *index.Field {
	t.Helper()
	for i := range st.Fields {
		if st.Fields[i].Name == name {
			return &st.Fields[i]
		}
	}
	t.Fatalf("%s has no field %q", st.Name, name)
	return nil
}

// TestTierSelection checks that the frame budget decides the path, and that
// each path says which it was.
func TestTierSelection(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")
	fd := field(t, st, "EngineSpeed")

	// Three frames, budget 256: decode them.
	_, tier, err := q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierExact {
		t.Errorf("tier = %s, want exact for 3 frames under a 256 budget", tier)
	}

	// Budget 2: three frames is too many, so statistics answer instead.
	q.MaxDecodeFrames = 2
	_, tier, err = q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierStats {
		t.Errorf("tier = %s, want stats once the budget is exceeded", tier)
	}
}

// TestStatsEnvelopeContainsExact is the safety property of Tier 1.
//
// Statistics are coarser than the samples — a frame's bounds get applied to
// every bucket it covers, because Tier 1 does not know where inside the frame
// the extreme fell. That must widen the envelope, never narrow it. A narrower
// envelope would hide a spike, which is usually the thing the file was opened
// to find.
func TestStatsEnvelopeContainsExact(t *testing.T) {
	q := newTestQuery(t)

	for _, name := range []string{"EngineSpeed", "CoolantTemp", "ThrottlePos"} {
		st := stream(t, q, "EngineData")
		fd := field(t, st, name)
		from, to := st.AxisMin, st.AxisMax

		q.MaxDecodeFrames = 256
		exact, tier, err := q.Envelope(st, fd, 0, from, to, 50)
		if err != nil || tier != TierExact {
			t.Fatalf("%s: exact pass: tier=%s err=%v", name, tier, err)
		}

		q.MaxDecodeFrames = 0
		stats, tier, err := q.Envelope(st, fd, 0, from, to, 50)
		if err != nil || tier != TierStats {
			t.Fatalf("%s: stats pass: tier=%s err=%v", name, tier, err)
		}

		if len(exact.Min) != len(stats.Min) {
			t.Fatalf("%s: exact has %d buckets, stats %d", name, len(exact.Min), len(stats.Min))
		}
		for i := range exact.Min {
			if exact.N[i] == 0 {
				continue
			}
			if stats.N[i] == 0 {
				t.Errorf("%s bucket %d: exact has %d samples but stats reports a gap",
					name, i, exact.N[i])
				continue
			}
			if stats.Min[i] > exact.Min[i] || stats.Max[i] < exact.Max[i] {
				t.Errorf("%s bucket %d: stats %v..%v does not contain exact %v..%v",
					name, i, stats.Min[i], stats.Max[i], exact.Min[i], exact.Max[i])
			}
		}
	}
}

// TestExactEnvelopeMatchesSamples pins the exact path against the samples
// themselves: over the whole file the envelope's extremes must equal the
// field's true extremes.
func TestExactEnvelopeMatchesSamples(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")
	fd := field(t, st, "EngineSpeed")

	s, err := q.Series(st, fd, 0)
	if err != nil {
		t.Fatal(err)
	}
	trueMin, trueMax := math.Inf(1), math.Inf(-1)
	for i, present := range s.Present {
		if !present {
			continue
		}
		trueMin = math.Min(trueMin, s.Vals[i])
		trueMax = math.Max(trueMax, s.Vals[i])
	}

	e, _, err := q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 50)
	if err != nil {
		t.Fatal(err)
	}
	gotMin, gotMax := math.Inf(1), math.Inf(-1)
	for i := range e.Min {
		if e.N[i] == 0 {
			continue
		}
		gotMin = math.Min(gotMin, e.Min[i])
		gotMax = math.Max(gotMax, e.Max[i])
	}
	if gotMin != trueMin || gotMax != trueMax {
		t.Errorf("envelope spans %v..%v, samples span %v..%v", gotMin, gotMax, trueMin, trueMax)
	}
}

// TestCacheServesRepeatRequests checks the property panning depends on: asking
// for an overlapping window again must not re-decode.
func TestCacheServesRepeatRequests(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")
	fd := field(t, st, "EngineSpeed")

	if _, _, err := q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 100); err != nil {
		t.Fatal(err)
	}
	_, missesAfterFirst, _, entries := q.CacheStats()
	if entries == 0 {
		t.Fatal("nothing cached after the first request")
	}

	// A second pass over the same window, and a narrower one inside it, must
	// both be served entirely from cache.
	if _, _, err := q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Envelope(st, fd, 0, 1.2e9, 1.4e9, 100); err != nil {
		t.Fatal(err)
	}
	hits, misses, _, _ := q.CacheStats()
	if misses != missesAfterFirst {
		t.Errorf("misses rose from %d to %d: repeat requests re-decoded", missesAfterFirst, misses)
	}
	if hits == 0 {
		t.Error("no cache hits on repeat requests")
	}
}

// TestCacheEviction checks the byte budget is actually enforced. An unbounded
// cache would defeat the whole point of not holding the file in memory.
func TestCacheEviction(t *testing.T) {
	q := newTestQuery(t)
	q.SetCacheBytes(1 << 10) // smaller than the file's frames

	st := stream(t, q, "EngineData")
	fd := field(t, st, "EngineSpeed")
	for i := 0; i < 3; i++ {
		if _, _, err := q.Envelope(st, fd, 0, st.AxisMin, st.AxisMax, 100); err != nil {
			t.Fatal(err)
		}
	}
	_, _, bytes, _ := q.CacheStats()
	if bytes > 1<<10 {
		t.Errorf("cache holds %d bytes, over its %d budget", bytes, 1<<10)
	}
}

// TestStatesTiers checks the categorical paths. Exact resolves gears; stats
// reports mixed rather than naming one, because at frame granularity it
// genuinely does not know.
func TestStatesTiers(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "VehicleStatus")
	fd := field(t, st, "Gear")

	// Zoomed in, decoding: the gears resolve and match what logbdump prints.
	got, tier, err := q.States(st, fd, 0, 0, 1e8, 500)
	if err != nil || tier != TierExact {
		t.Fatalf("tier=%s err=%v", tier, err)
	}
	for i, want := range []string{"3", "4", "3"} {
		if i >= len(got) {
			t.Fatalf("only %d states", len(got))
		}
		if got[i].Mixed || got[i].Label != want {
			t.Errorf("state %d = %q (mixed=%v), want %q", i, got[i].Label, got[i].Mixed, want)
		}
	}

	// Forced onto statistics: each frame held several gears, so each band is
	// mixed and none carries a label.
	q.MaxDecodeFrames = 0
	got, tier, err = q.States(st, fd, 0, st.AxisMin, st.AxisMax, 500)
	if err != nil || tier != TierStats {
		t.Fatalf("tier=%s err=%v", tier, err)
	}
	if len(got) == 0 {
		t.Fatal("no states from the stats path")
	}
	for i, s := range got {
		if !s.Mixed {
			t.Errorf("stats band %d not mixed, label=%q", i, s.Label)
		}
		if s.Label != "" {
			t.Errorf("stats band %d labelled %q: it cannot know which state", i, s.Label)
		}
	}
}

// TestEmptyWindow: a window with no records is an empty chart, not an error.
func TestEmptyWindow(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "EngineData")
	fd := field(t, st, "EngineSpeed")

	e, _, err := q.Envelope(st, fd, 0, 100e9, 200e9, 100)
	if err != nil {
		t.Fatalf("empty window errored: %v", err)
	}
	if len(e.X) != 0 {
		t.Errorf("empty window returned %d points", len(e.X))
	}

	s, _, err := q.States(st, field(t, stream(t, q, "VehicleStatus"), "Gear"), 0, 100e9, 200e9, 100)
	if err != nil {
		t.Fatalf("empty states window errored: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("empty window returned %d states", len(s))
	}
}
