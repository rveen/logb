package decimate

// Event is one occurrence: a position on the axis and what was recorded there.
type Event struct {
	X     float64 `json:"x"`
	Run   uint32  `json:"run"`
	Label string  `json:"label"`
}

// Density is a count of events over a span, for when there are too many to draw
// or to send individually.
//
// The span is a DATA frame, not a pixel bucket. A frame is the unit Tier 1
// summarises, so this costs no decoding at all — and it is honest about its own
// resolution in a way a pixel bucket would not be, because the count really is
// "this many events happened somewhere in here" rather than "at this x".
type Density struct {
	X0 float64 `json:"x0"`
	X1 float64 `json:"x1"`
	N  uint32  `json:"n"`
}

// EventsFromStats builds a density view out of per-frame presence counts.
//
// Frames with no events are dropped rather than sent as zeroes: an event lane
// is sparse by nature, and a long run of empty buckets is bytes spent saying
// nothing happened.
func EventsFromStats(stats []FrameStat, from, to float64) []Density {
	out := make([]Density, 0, len(stats))
	for _, s := range stats {
		if s.NPresent == 0 {
			continue
		}
		x0, x1 := s.First, s.Last
		if x1 < from || x0 > to {
			continue
		}
		// Clamped to the window so a frame straddling the edge does not draw a
		// band running off the chart, which would read as events outside the
		// range the user asked for.
		if x0 < from {
			x0 = from
		}
		if x1 > to {
			x1 = to
		}
		out = append(out, Density{X0: x0, X1: x1, N: s.NPresent})
	}
	return out
}
