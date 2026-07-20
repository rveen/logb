package index

import "github.com/rveen/logb"

// SeriesFrom builds the series for one field out of already-decoded batches.
//
// This is the Tier 2 path: when a window is narrow enough that decoding it is
// affordable, the samples are materialised for exactly that window and exactly
// that field, reduced, and thrown away. Memory stays bounded by the window
// rather than by the file.
//
// epoch rebases a time axis. Batches decoded through an Accessor carry absolute
// axis values, because a DATA frame stores an absolute axis_base; the rest of
// the viewer works in epoch-relative units, and mixing the two silently shifts
// a trace by the recording's start time.
func SeriesFrom(batches []*logb.Batch, fd *Field, epoch int64) *Series {
	s := &Series{}
	if len(batches) == 0 {
		return s
	}
	s.Axis.Time = batches[0].Schema.AxisKind == logb.AxisTime

	n := 0
	for _, b := range batches {
		n += int(b.Count)
	}
	s.Vals = make([]float64, 0, n)
	s.Present = make([]bool, 0, n)
	if s.Axis.Time {
		s.Axis.Ticks = make([]int64, 0, n)
	} else {
		s.Axis.Float = make([]float64, 0, n)
	}

	for _, b := range batches {
		for i := 0; i < int(b.Count); i++ {
			av, err := b.Axis(i)
			if err != nil {
				// No axis means no position to draw at. Skipping matches what
				// the scan does.
				continue
			}
			if s.Axis.Time {
				s.Axis.Ticks = append(s.Axis.Ticks, av.Ticks()-epoch)
			} else {
				s.Axis.Float = append(s.Axis.Float, av.Float())
			}
			v, present := sample(b, i, fd)
			s.Vals = append(s.Vals, v)
			s.Present = append(s.Present, present)
		}
	}
	return s
}
