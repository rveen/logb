package decimate

import "math"

// FrameStat is one DATA frame's summary of one field: what Tier 1 knows without
// decoding anything.
//
// The frame is the leaf bucket. It is the smallest thing that can be
// decompressed, so it is the finest granularity available for free.
type FrameStat struct {
	// First and Last are the frame's axis extent, in the units Axis.At uses.
	First, Last float64
	// Min and Max bound the present samples.
	Min, Max float64
	// NPresent counts samples actually in the record. Zero means the whole
	// frame is a gap for this field, which must render as a break.
	NPresent uint32
	// Value is the single raw value a categorical frame held, valid only when
	// Distinct is false.
	Value float64
	// Distinct means the frame held more than one value.
	Distinct bool
}

// NumericFromStats reduces per-frame statistics into a bucket envelope.
//
// A frame usually spans several buckets, and Tier 1 knows only its overall
// bounds — not where inside the frame the minimum fell. Those bounds are
// therefore applied to every bucket the frame covers. That widens the envelope
// rather than narrowing it, which is the safe direction: a spike stays visible,
// and no bucket ever claims a tighter range than the data supports.
//
// The alternative — attributing a frame's bounds to one bucket and leaving its
// neighbours empty — would punch holes in a signal that is continuously
// present, and a hole means "absent" everywhere else in this viewer.
func NumericFromStats(stats []FrameStat, from, to float64, buckets int, scale Scale) Envelope {
	if buckets < 1 {
		buckets = 1
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

	for _, s := range stats {
		if s.NPresent == 0 {
			continue
		}
		lo, hi := spanBuckets(pos, s.First, s.Last, buckets)
		if lo < 0 {
			continue
		}
		// Spread the frame's sample count over the buckets it touches, so a
		// bucket's n stays an honest indication of density rather than
		// multiplying by the number of buckets the frame happened to cover.
		share := int32(s.NPresent) / int32(hi-lo+1)
		if share < 1 {
			share = 1
		}
		for b := lo; b <= hi; b++ {
			if e.N[b] == 0 {
				e.Min[b], e.Max[b] = s.Min, s.Max
			} else {
				e.Min[b] = math.Min(e.Min[b], s.Min)
				e.Max[b] = math.Max(e.Max[b], s.Max)
			}
			e.N[b] += share
		}
	}
	return e
}

// CategoricalFromStats reduces per-frame statistics into state bands.
//
// A frame that held one value becomes a band of that value. A frame that held
// more than one becomes a mixed band: Tier 1 does not know which values, or in
// what order, and inventing an answer is exactly what the mixed marker exists
// to avoid. Zooming in far enough moves the request to Tier 2, which knows.
func CategoricalFromStats(stats []FrameStat, label func(float64) string, from, to float64) []State {
	var out []State
	for _, s := range stats {
		st := State{X0: math.Max(s.First, from), X1: math.Min(s.Last, to)}
		switch {
		case s.NPresent == 0:
			st.Absent = true
		case s.Distinct:
			st.Mixed = true
		default:
			st.Raw = s.Value
			if label != nil {
				st.Label = label(s.Value)
			}
		}
		if st.X1 < st.X0 {
			continue
		}
		// Adjacent frames holding the same single value read as one state.
		if n := len(out); n > 0 && mergeable(out[n-1], st) {
			out[n-1].X1 = st.X1
			continue
		}
		out = append(out, st)
	}
	return out
}

func mergeable(a, b State) bool {
	if a.Absent != b.Absent || a.Mixed != b.Mixed {
		return false
	}
	if a.Absent || a.Mixed {
		return true
	}
	return a.Raw == b.Raw
}

// spanBuckets returns the inclusive bucket range a frame covers, or -1 when it
// falls entirely outside the window.
func spanBuckets(pos positioner, first, last float64, buckets int) (int, int) {
	lo := pos.bucket(first)
	hi := pos.bucket(last)
	if lo < 0 && hi < 0 {
		// A log-scaled positioner rejects non-positive values outright.
		return -1, -1
	}
	if lo < 0 {
		lo = 0
	}
	if hi < 0 {
		hi = buckets - 1
	}
	if lo > hi {
		lo, hi = hi, lo
	}
	if hi >= buckets {
		hi = buckets - 1
	}
	if lo < 0 {
		lo = 0
	}
	return lo, hi
}
