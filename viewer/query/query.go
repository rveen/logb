// Package query answers chart requests against an indexed Logb file.
//
// It picks between two tiers, and the choice is the whole scaling story:
//
//   - Tier 2, exact. When the requested window spans few enough DATA frames to
//     decode inside a frame budget, those frames are decoded and reduced from
//     the samples themselves. This is what the user sees when zoomed in, which
//     is when exactness matters.
//   - Tier 1, statistics. Otherwise the answer comes from the per-frame min,
//     max and presence counts computed during indexing. No decoding happens at
//     all, so a whole-file overview of a hundred million samples is immediate.
//
// A DATA frame is the unit of both, because it is the smallest thing that can
// be decompressed. That also means no intermediate pyramid level is needed:
// frames are already the granularity.
package query

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/rveen/logb"
	"github.com/rveen/logb/viewer/decimate"
	"github.com/rveen/logb/viewer/index"
)

// Defaults chosen to be comfortable rather than clever. Tune with the fields
// on Query if a real file argues otherwise.
const (
	// DefaultMaxDecodeFrames bounds the exact path. Past this, a request is
	// served from statistics instead of stalling the chart.
	DefaultMaxDecodeFrames = 256
	// DefaultCacheBytes bounds the decoded-frame cache.
	DefaultCacheBytes = 128 << 20
)

// Tier names which path answered a request, so the UI can say so honestly
// rather than implying a precision it does not have.
type Tier string

const (
	TierExact Tier = "exact"
	TierStats Tier = "stats"
)

// Query serves decimated views of an indexed file.
type Query struct {
	File *index.File

	// MaxDecodeFrames bounds how many frames the exact path will decode.
	MaxDecodeFrames int
	// MaxEvents bounds how many individual events a lane will send before it
	// falls back to density. Zero means DefaultMaxEvents.
	MaxEvents int

	acc   *index.Accessor
	cache *batchCache
}

func New(f *index.File, acc *index.Accessor) *Query {
	return &Query{
		File:            f,
		MaxDecodeFrames: DefaultMaxDecodeFrames,
		acc:             acc,
		cache:           newBatchCache(DefaultCacheBytes),
	}
}

// SetCacheBytes resizes the decoded-frame cache.
func (q *Query) SetCacheBytes(n int64) { q.cache = newBatchCache(n) }

// CacheStats reports hits, misses, resident bytes and entry count.
func (q *Query) CacheStats() (hits, misses, bytes int64, entries int) {
	return q.cache.stats()
}

// Envelope reduces a numeric field over a window to at most buckets points.
func (q *Query) Envelope(st *index.Stream, fd *index.Field, run uint32, from, to float64, buckets int) (decimate.Envelope, Tier, error) {
	frames := selectFrames(st, run, from, to)
	if len(frames) == 0 {
		// Nothing recorded in this window is not an error; it is an empty
		// chart, and the client draws it as one.
		return decimate.Envelope{X: []float64{}, Min: []float64{}, Max: []float64{}, N: []int32{}}, TierExact, nil
	}

	if len(frames) <= q.MaxDecodeFrames {
		batches, err := q.batches(frames)
		if err != nil {
			return decimate.Envelope{}, "", err
		}
		s := index.SeriesFrom(batches, fd, q.File.Epoch)
		return decimate.Numeric(s, from, to, buckets, scaleOf(st)), TierExact, nil
	}

	stats := q.frameStats(st, fd, frames)
	return decimate.NumericFromStats(stats, from, to, buckets, scaleOf(st)), TierStats, nil
}

// States reduces a categorical field over a window to state bands.
func (q *Query) States(st *index.Stream, fd *index.Field, run uint32, from, to float64, buckets int) ([]decimate.State, Tier, error) {
	frames := selectFrames(st, run, from, to)
	if len(frames) == 0 {
		return []decimate.State{}, TierExact, nil
	}

	if len(frames) <= q.MaxDecodeFrames {
		batches, err := q.batches(frames)
		if err != nil {
			return nil, "", err
		}
		s := index.SeriesFrom(batches, fd, q.File.Epoch)
		out := decimate.Categorical(s, fd, from, to, buckets, scaleOf(st))
		if out == nil {
			out = []decimate.State{}
		}
		return out, TierExact, nil
	}

	stats := q.frameStats(st, fd, frames)
	out := decimate.CategoricalFromStats(stats, fd.Label, from, to)
	if out == nil {
		out = []decimate.State{}
	}
	return out, TierStats, nil
}

// Series decodes a whole stream/field/run into memory.
//
// Unbounded by design: it is for tests and for tools that genuinely want every
// sample, not for serving charts. Anything user-facing goes through Envelope or
// States, which bound their own memory.
func (q *Query) Series(st *index.Stream, fd *index.Field, run uint32) (*index.Series, error) {
	batches, err := q.batches(st.Frames(run))
	if err != nil {
		return nil, err
	}
	return index.SeriesFrom(batches, fd, q.File.Epoch), nil
}

// frameStats gathers the Tier 1 summaries for a set of frames.
func (q *Query) frameStats(st *index.Stream, fd *index.Field, frames []index.DataFrame) []decimate.FrameStat {
	out := make([]decimate.FrameStat, 0, len(frames))
	for _, f := range frames {
		ord := st.FrameOrdinal(f.Offset)
		if ord < 0 {
			continue
		}
		s := st.Stat(ord, fd.Index)
		out = append(out, decimate.FrameStat{
			First:    f.First(),
			Last:     f.Last(),
			Min:      s.Min,
			Max:      s.Max,
			NPresent: s.NPresent,
			Value:    s.First,
			Distinct: s.Distinct,
		})
	}
	return out
}

// batches decodes the given frames, serving what it can from the cache.
//
// Misses are decoded per segment, in parallel and bounded to the number of
// CPUs: each group needs its own synthesized prefix, and the groups are
// independent.
func (q *Query) batches(frames []index.DataFrame) ([]*logb.Batch, error) {
	out := make([]*logb.Batch, len(frames))
	var missing []index.DataFrame
	var missingAt []int

	for i, f := range frames {
		if b, ok := q.cache.get(f.Offset); ok {
			out[i] = b
			continue
		}
		missing = append(missing, f)
		missingAt = append(missingAt, i)
	}
	if len(missing) == 0 {
		return out, nil
	}

	groups := groupBySegment(missing)
	decoded := make([][]*logb.Batch, len(groups))
	errs := make([]error, len(groups))

	sem := make(chan struct{}, max(1, runtime.GOMAXPROCS(0)))
	var wg sync.WaitGroup
	for gi, g := range groups {
		wg.Add(1)
		go func(gi int, g []index.DataFrame) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			decoded[gi], errs[gi] = q.acc.Decode(g)
		}(gi, g)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// Stitch the decoded groups back into the caller's ordering.
	k := 0
	for gi, g := range groups {
		if len(decoded[gi]) != len(g) {
			return nil, fmt.Errorf("query: group %d decoded %d of %d frames", gi, len(decoded[gi]), len(g))
		}
		for j := range g {
			b := decoded[gi][j]
			out[missingAt[k]] = b
			q.cache.put(g[j].Offset, b)
			k++
		}
	}
	return out, nil
}

// groupBySegment splits frames into runs sharing a segment, preserving order.
// A synthesized prefix serves one segment, so a group is the unit of decode.
func groupBySegment(frames []index.DataFrame) [][]index.DataFrame {
	var out [][]index.DataFrame
	for _, f := range frames {
		if n := len(out); n > 0 && out[n-1][0].Segment == f.Segment {
			out[n-1] = append(out[n-1], f)
			continue
		}
		out = append(out, []index.DataFrame{f})
	}
	return out
}

// selectFrames picks a run's frames overlapping the window, in file order.
func selectFrames(st *index.Stream, run uint32, from, to float64) []index.DataFrame {
	var out []index.DataFrame
	for _, f := range st.FrameList {
		if f.RunID == run && f.Overlaps(from, to) {
			out = append(out, f)
		}
	}
	return out
}

// scaleOf picks the bucketing scale. SPEC §5.3 rejects a logarithmic time axis,
// so a time stream is always linear.
func scaleOf(st *index.Stream) decimate.Scale {
	if st.AxisMode == "log" {
		return decimate.Log
	}
	return decimate.Linear
}
