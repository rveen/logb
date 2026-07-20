package query

import (
	"github.com/rveen/logb"
	"github.com/rveen/logb/viewer/decimate"
	"github.com/rveen/logb/viewer/index"
)

// DefaultMaxEvents bounds how many individual events the exact path will send.
//
// Past this the lane is drawn as density instead. The limit is about the wire
// and the DOM rather than about decoding: ten thousand labelled marks is more
// than a lane can distinguish and more than a browser enjoys, long before it is
// more than the server can decode.
const DefaultMaxEvents = 2000

// Events returns the occurrences of an event field over a window.
//
// Two tiers, the same shape as the numeric and categorical paths. When the
// window spans few enough frames to decode and holds few enough events to draw,
// every event comes back with its label. Otherwise the lane is a density
// profile built from Tier 1 presence counts, which costs no decoding at all —
// and which is why those counts are gathered during the index pass even though
// an event field has no numeric value to summarise.
func (q *Query) Events(st *index.Stream, fd *index.Field, run *uint32, from, to float64) ([]decimate.Event, []decimate.Density, Tier, error) {
	frames := recordFrames(st, run, from, to)
	if len(frames) == 0 {
		return []decimate.Event{}, nil, TierExact, nil
	}

	// The record count bounds the event count from above — a field can be
	// absent from a record but never present twice — so this decides the tier
	// without decoding anything.
	records := 0
	for _, f := range frames {
		records += int(f.Count)
	}

	if len(frames) <= q.MaxDecodeFrames && records <= q.maxEvents() {
		batches, err := q.batches(frames)
		if err != nil {
			return nil, nil, "", err
		}
		return q.exactEvents(batches, st, fd, from, to), nil, TierExact, nil
	}

	stats := q.frameStats(st, fd, frames)
	return nil, decimate.EventsFromStats(stats, from, to), TierStats, nil
}

func (q *Query) maxEvents() int {
	if q.MaxEvents > 0 {
		return q.MaxEvents
	}
	return DefaultMaxEvents
}

// exactEvents pulls the labelled occurrences out of decoded batches.
func (q *Query) exactEvents(batches []*logb.Batch, st *index.Stream, fd *index.Field, from, to float64) []decimate.Event {
	out := []decimate.Event{}
	for _, b := range batches {
		for i := 0; i < int(b.Count); i++ {
			rec, ok := index.RecordAt(b, i, st, q.File.Epoch)
			if !ok || rec.Axis < from || rec.Axis > to {
				continue
			}
			// An absent event is not an event. A guarded string field whose
			// guard does not hold was not in the record (SPEC §6.2), and a mark
			// drawn there would claim something happened when nothing did.
			c := rec.Cells[fd.Index]
			if !c.Present {
				continue
			}
			out = append(out, decimate.Event{X: rec.Axis, Run: rec.Run, Label: c.Text})
		}
	}
	return out
}
