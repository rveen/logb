package server

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
)

// MaxExportRecords bounds a CSV export.
//
// The limit is checked against the frame index before anything is decoded, so
// an over-large request is refused with a 400 that says how large it was rather
// than by a response that stops in the middle. A CSV cannot say "and there was
// more" — it just ends — which makes silent truncation the one failure mode
// worth designing against here.
const MaxExportRecords = 2_000_000

// --- /api/records ----------------------------------------------------------

type recordsDTO struct {
	Stream string   `json:"stream"`
	Fields []string `json:"fields"`
	Rows   []rowDTO `json:"rows"`
	Offset int      `json:"offset"`
	More   bool     `json:"more"`
	// Total is exact only when every frame overlapping the window lies wholly
	// inside it; otherwise it is an upper bound. The UI says "up to" rather
	// than claiming a count it would have to decode the edges to know.
	Total int  `json:"total"`
	Exact bool `json:"totalExact"`
	Runs  bool `json:"perRun"`
}

type rowDTO struct {
	X   float64 `json:"x"`
	Run uint32  `json:"run"`
	// Text is what a human reads; Num is the same value as a number where one
	// exists. Both are empty/null for an absent field.
	Text []string   `json:"text"`
	Num  []*float64 `json:"num"`
}

// handleRecords serves the record table: the decoded records themselves, which
// is what a chart is a summary of.
func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request) {
	_, q, ok := s.ready(w)
	if !ok {
		return
	}
	st, run, err := s.resolveStream(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	from, to := rangeOf(r, st)
	offset := intParam(r, "offset", 0, 0, 1<<30)
	limit := intParam(r, "limit", query.DefaultRecordLimit, 1, 5000)

	page, err := q.Records(st, run, from, to, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dto := recordsDTO{
		Stream: st.Name,
		Fields: fieldNames(st),
		Rows:   make([]rowDTO, 0, len(page.Records)),
		Offset: page.Offset,
		More:   page.More,
		Total:  page.Total,
		Exact:  page.TotalExact,
		Runs:   run == nil,
	}
	for _, rec := range page.Records {
		row := rowDTO{
			X:    rec.Axis,
			Run:  rec.Run,
			Text: make([]string, len(rec.Cells)),
			Num:  make([]*float64, len(rec.Cells)),
		}
		for i, c := range rec.Cells {
			// An absent field stays empty in both columns. Not "0", not "null"
			// spelled as a value — the field was not in the record (SPEC §6.2),
			// and a table that shows a zero there is the bug the guard feature
			// exists to prevent.
			if !c.Present {
				continue
			}
			row.Text[i] = c.Text
			row.Num[i] = c.Num
		}
		dto.Rows = append(dto.Rows, row)
	}
	writeJSON(w, dto)
}

// --- /api/export.csv -------------------------------------------------------

// handleExport streams a window as CSV.
//
// Numbers, not labels: an export is for feeding another tool, and the raw value
// of a categorical field is what survives a round trip through a spreadsheet.
// The header row carries the unit so the numbers stay interpretable.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	_, q, ok := s.ready(w)
	if !ok {
		return
	}
	st, run, err := s.resolveStream(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	from, to := rangeOf(r, st)

	cols, err := exportColumns(st, r.URL.Query().Get("fields"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if n := q.CountRecords(st, run, from, to); n > MaxExportRecords {
		http.Error(w, fmt.Sprintf(
			"this window covers up to %d records, over the %d export limit; narrow the range first",
			n, MaxExportRecords), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", csvName(st)))

	cw := csv.NewWriter(w)
	head := []string{axisColumn(st), "run"}
	for _, i := range cols {
		fd := &st.Fields[i]
		if fd.Unit != "" {
			head = append(head, fd.Name+" ["+fd.Unit+"]")
			continue
		}
		head = append(head, fd.Name)
	}
	if err := cw.Write(head); err != nil {
		return
	}

	rec := make([]string, len(head))
	err = q.EachRecord(st, run, from, to, func(rc index.Record) error {
		rec[0] = axisCell(st, rc.Axis)
		rec[1] = strconv.FormatUint(uint64(rc.Run), 10)
		for k, i := range cols {
			c := rc.Cells[i]
			switch {
			case !c.Present:
				// An empty field, which is what every consumer of a CSV reads
				// as "no value". Writing 0 here would be a number the recording
				// never contained.
				rec[2+k] = ""
			case c.Num != nil:
				rec[2+k] = strconv.FormatFloat(*c.Num, 'g', -1, 64)
			default:
				rec[2+k] = c.Text
			}
		}
		return cw.Write(rec)
	})
	cw.Flush()
	if err != nil {
		// The header and some rows are already on the wire, so there is no
		// status left to set. Log-shaped truth beats a silently short file:
		// the connection is closed without a final flush, which is what makes
		// the client see an error rather than a complete-looking export.
		panic(http.ErrAbortHandler)
	}
}

// exportColumns resolves the requested field list, defaulting to every field
// the stream has apart from the one carrying an explicit axis — that one is
// already the first column.
func exportColumns(st *index.Stream, spec string) ([]int, error) {
	if strings.TrimSpace(spec) == "" {
		var out []int
		for i := range st.Fields {
			if !st.Fields[i].IsAxis {
				out = append(out, i)
			}
		}
		return out, nil
	}
	var out []int
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := -1
		for i := range st.Fields {
			if st.Fields[i].Name == name {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("stream %s has no field %q", st.Name, name)
		}
		out = append(out, found)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no fields selected")
	}
	return out, nil
}

// resolveStream looks up the stream and optional run named by a request.
//
// Unlike resolve, this does not require a run: a table shows what the file
// contains in file order, and restricting it to one run is a filter rather than
// the default.
func (s *Server) resolveStream(r *http.Request) (*index.Stream, *uint32, error) {
	f, _, _, _, _, _, _ := s.state.snapshot()
	qs := r.URL.Query()
	st := f.Stream(qs.Get("stream"))
	if st == nil {
		return nil, nil, fmt.Errorf("unknown stream %q", qs.Get("stream"))
	}
	if qs.Get("run") == "" {
		return st, nil, nil
	}
	n, err := strconv.ParseUint(qs.Get("run"), 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("bad run %q", qs.Get("run"))
	}
	run := uint32(n)
	return st, &run, nil
}

func fieldNames(st *index.Stream) []string {
	out := make([]string, len(st.Fields))
	for i := range st.Fields {
		out[i] = st.Fields[i].Name
	}
	return out
}

// axisColumn names the first CSV column after the axis it carries. A time axis
// is epoch-relative ticks of 10^axisExp seconds, which is worth saying in the
// header rather than leaving a column of bare integers.
func axisColumn(st *index.Stream) string {
	if st.AxisKind == "time" {
		return fmt.Sprintf("time [1e%d s]", st.AxisExp)
	}
	if st.AxisUnit != "" {
		return st.AxisKind + " [" + st.AxisUnit + "]"
	}
	return st.AxisKind
}

// axisCell formats the axis column.
//
// A time axis is an int64 count of ticks (SPEC §5), so it is written as an
// integer. The default float formatting turns 4999970000 into "4.99997e+09",
// which is the same number but reads as an approximation and is exactly the
// shape a spreadsheet rounds on import. Every other axis kind is genuinely an
// f64 and keeps shortest-round-trip formatting.
func axisCell(st *index.Stream, v float64) string {
	if st.AxisKind == "time" {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func csvName(st *index.Stream) string {
	name := st.Name
	if name == "" {
		name = "stream"
	}
	// Keep it to something every filesystem and every Content-Disposition
	// parser agrees about.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String() + ".csv"
}
