package query

import (
	"fmt"

	"github.com/rveen/logb/viewer/index"
)

// DefaultRecordLimit bounds one page of the record table.
const DefaultRecordLimit = 200

// RecordPage is one page of decoded records.
type RecordPage struct {
	Records []index.Record
	Offset  int
	// More means the window holds records past this page.
	More bool
	// Total is how many records the window can hold. It is exact when every
	// overlapping frame lies wholly inside the window, and an upper bound
	// otherwise — the frames at the edges spill past it, and counting their
	// records exactly would mean decoding them just to answer "how many".
	Total      int
	TotalExact bool
	// Decoded counts the frames this page had to decompress. Zero for a page
	// served entirely from the cache.
	Decoded int
}

// Records returns one page of records from a window.
//
// run selects a single run, or nil for every run interleaved in file order — a
// stepped sweep is N traces on a chart (SPEC §6.5), but a table is a table, and
// showing what the file actually contains in order is the point of one.
//
// Paging does not decode what it can count. A frame lying wholly inside the
// window holds exactly the record count it declares, so skipping past it costs
// nothing; only the partially-overlapping frames at the window's edges have to
// be decompressed to know which of their records are in range. That is what
// makes an offset deep into a hundred-million-record file affordable.
func (q *Query) Records(st *index.Stream, run *uint32, from, to float64, offset, limit int) (*RecordPage, error) {
	if limit <= 0 {
		limit = DefaultRecordLimit
	}
	if offset < 0 {
		offset = 0
	}

	page := &RecordPage{Offset: offset, Records: []index.Record{}, TotalExact: true}

	frames := recordFrames(st, run, from, to)
	for _, f := range frames {
		page.Total += int(f.Count)
		if !inside(f, from, to) {
			page.TotalExact = false
		}
	}

	skip := offset
	for _, f := range frames {
		// The cheap path: a frame wholly inside the window contributes exactly
		// the records it declares, so it can be skipped without touching a byte.
		if skip >= int(f.Count) && inside(f, from, to) {
			skip -= int(f.Count)
			continue
		}
		if page.Decoded >= q.MaxDecodeFrames {
			// Out of budget with frames left to look at. Returning a short page
			// with More set is the honest answer; pretending the window ended
			// here would not be.
			page.More = true
			return page, nil
		}

		batches, err := q.batches([]index.DataFrame{f})
		if err != nil {
			return nil, err
		}
		page.Decoded++
		if len(batches) != 1 {
			return nil, fmt.Errorf("query: frame at %d decoded to %d batches", f.Offset, len(batches))
		}
		b := batches[0]

		for j := 0; j < int(b.Count); j++ {
			rec, ok := index.RecordAt(b, j, st, q.File.Epoch)
			if !ok || rec.Axis < from || rec.Axis > to {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			if len(page.Records) == limit {
				page.More = true
				return page, nil
			}
			page.Records = append(page.Records, rec)
		}
	}
	return page, nil
}

// EachRecord calls fn for every record of a window, in file order.
//
// This is the export path. It holds one frame at a time rather than a page, so
// a caller streaming a CSV of a whole recording never materialises more than a
// frame's worth of records — but it is also unbounded in time, so nothing
// user-facing should call it without first checking how much it is asking for.
// See CountRecords.
//
// It decodes around the cache deliberately. An export walks every frame once
// and never revisits one, so filling the cache with them would evict exactly
// the frames the charts are panning over, to no benefit.
func (q *Query) EachRecord(st *index.Stream, run *uint32, from, to float64, fn func(index.Record) error) error {
	for _, f := range recordFrames(st, run, from, to) {
		batches, err := q.acc.Decode([]index.DataFrame{f})
		if err != nil {
			return err
		}
		if len(batches) != 1 {
			return fmt.Errorf("query: frame at %d decoded to %d batches", f.Offset, len(batches))
		}
		b := batches[0]
		for j := 0; j < int(b.Count); j++ {
			rec, ok := index.RecordAt(b, j, st, q.File.Epoch)
			if !ok || rec.Axis < from || rec.Axis > to {
				continue
			}
			if err := fn(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// CountRecords reports how many records a window can hold, without decoding.
//
// The count is an upper bound: the frames at the window's edges spill past it.
// That is the right direction for a guard — it never lets through an export
// larger than it promised.
func (q *Query) CountRecords(st *index.Stream, run *uint32, from, to float64) int {
	n := 0
	for _, f := range recordFrames(st, run, from, to) {
		n += int(f.Count)
	}
	return n
}

// recordFrames picks the frames overlapping a window, optionally for one run.
func recordFrames(st *index.Stream, run *uint32, from, to float64) []index.DataFrame {
	var out []index.DataFrame
	for _, f := range st.FrameList {
		if run != nil && f.RunID != *run {
			continue
		}
		if f.Overlaps(from, to) {
			out = append(out, f)
		}
	}
	return out
}

// inside reports whether every record of a frame falls in the window.
func inside(f index.DataFrame, from, to float64) bool {
	return f.First() >= from && f.Last() <= to
}
