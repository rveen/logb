package index

import (
	"math"

	"github.com/rveen/logb"
)

// Stat summarises one field over one DATA frame.
//
// This is Tier 1, and it is the whole reason a large file is viewable. The
// index pass has to decompress every frame anyway just to find the frames; the
// marginal cost of also reducing each field while the batch is hot is small,
// and what it buys is a whole-file overview at any zoom without decoding
// anything at all. A DATA frame is the natural leaf bucket, because it is the
// smallest thing that can be decompressed.
//
// Roughly 40 bytes per field per frame: 10k frames of 50 fields is about 20 MB,
// against the tens of gigabytes the decoded samples would occupy.
type Stat struct {
	// Min and Max bound the present samples. Meaningful for numeric fields.
	Min, Max float64
	// First and Last are the frame's first and last present values, which is
	// what a categorical run needs to join correctly across frames.
	First, Last float64
	// NPresent counts samples that were actually in the record. Zero means the
	// whole frame is a gap for this field — an absent guarded field, not a run
	// of zeros (SPEC §6.2).
	NPresent uint32
	// Distinct is set when the frame held more than one distinct raw value.
	// For a categorical field that is the difference between "this frame is
	// third gear" and "something happened in here that a single band cannot
	// honestly describe".
	Distinct bool
}

// Empty reports whether the frame held no present sample for this field.
func (s Stat) Empty() bool { return s.NPresent == 0 }

// statAccum builds a Stat as samples arrive.
type statAccum struct {
	s     Stat
	begun bool
}

func (a *statAccum) add(v float64, present bool) {
	if !present {
		return
	}
	if !a.begun {
		a.begun = true
		a.s.Min, a.s.Max = v, v
		a.s.First, a.s.Last = v, v
		a.s.NPresent = 1
		return
	}
	if v < a.s.Min {
		a.s.Min = v
	}
	if v > a.s.Max {
		a.s.Max = v
	}
	if v != a.s.First {
		// Cheaper than tracking a set, and it answers the only question the
		// renderer asks: can this frame be drawn as one state, or not?
		a.s.Distinct = true
	}
	a.s.Last = v
	a.s.NPresent++
}

// addPresence records that a sample existed without recording a value.
//
// For an event field that is the whole of Tier 1: there is no minimum of a set
// of strings, and the bounds stay NaN rather than being given a number that
// would then have to be ignored everywhere downstream.
func (a *statAccum) addPresence(present bool) {
	if present {
		a.s.NPresent++
	}
}

func (a *statAccum) result() Stat {
	if !a.begun {
		// No numeric sample was ever added — either the field was absent
		// throughout, or it is an event field that only ever counts. NPresent
		// carries over either way; it is the one thing that is still meaningful.
		return Stat{
			Min: math.NaN(), Max: math.NaN(), First: math.NaN(), Last: math.NaN(),
			NPresent: a.s.NPresent,
		}
	}
	return a.s
}

// statsFor reduces every plottable field of one batch to a Stat.
//
// The batch is already decompressed and de-filtered at this point, so this is
// the cheapest moment in the file's life to ask these questions.
func statsFor(b *logb.Batch, fields []Field) []Stat {
	acc := make([]statAccum, len(fields))
	for i := 0; i < int(b.Count); i++ {
		for fx := range fields {
			fd := &fields[fx]
			if fd.IsAxis || fd.Class == ClassBlob {
				continue
			}
			// An event field has no numeric value, so its bounds stay NaN — but
			// its presence count is exactly the event density, which is what a
			// zoomed-out event lane draws instead of a mark per record. Free
			// here, and it is the only place in the file's life where asking is
			// free.
			if fd.Class == ClassEvent {
				acc[fx].addPresence(present(b, i, fd))
				continue
			}
			v, p := sample(b, i, fd)
			acc[fx].add(v, p)
		}
	}
	out := make([]Stat, len(fields))
	for i := range acc {
		out[i] = acc[i].result()
	}
	return out
}
