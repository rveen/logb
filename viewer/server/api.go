// Package server exposes an indexed Logb file over HTTP for the browser UI.
//
// The wire format is deliberately boring: JSON, with axis values as float64
// offsets from the file epoch. The one rule that is not boring is that absence
// must survive the trip. A bucket with no present sample is sent with n=0 and
// a null bound, never a zero — a zero would draw as a value the recording
// never contained (SPEC §6.2).
package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/rveen/logb/viewer/decimate"
	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
)

// Server serves one indexed file.
type Server struct {
	state *State
	mux   *http.ServeMux
}

// New builds a handler for an already-indexed file. ui may be nil, in which
// case only the API is served.
func New(f *index.File, q *query.Query, ui http.Handler) *Server {
	st := NewState()
	st.Ready(f, q)
	return NewWithState(st, ui)
}

// NewWithState builds a handler that can come up before indexing has finished.
func NewWithState(state *State, ui http.Handler) *Server {
	s := &Server{state: state, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/file", s.handleFile)
	s.mux.HandleFunc("GET /api/series", s.handleSeries)
	s.mux.HandleFunc("GET /api/states", s.handleStates)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/frames", s.handleFrames)
	s.mux.HandleFunc("GET /api/records", s.handleRecords)
	s.mux.HandleFunc("GET /api/export.csv", s.handleExport)
	s.mux.HandleFunc("GET /api/attach/{name}", s.handleAttach)
	s.mux.HandleFunc("GET /api/progress", state.handleProgress)
	if ui != nil {
		s.mux.Handle("/", ui)
	}
	return s
}

// ready returns the indexed file and query, or false if indexing is still in
// flight — in which case the caller must have already answered the request.
func (s *Server) ready(w http.ResponseWriter) (*index.File, *query.Query, bool) {
	f, q, _, _, err, ok, _ := s.state.snapshot()
	if !ok || err != nil {
		s.state.hold(w)
		return nil, nil, false
	}
	return f, q, true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// --- /api/file -------------------------------------------------------------

type fileDTO struct {
	Path        string      `json:"path"`
	Size        int64       `json:"size"`
	Epoch       int64       `json:"epoch"`
	HasEpoch    bool        `json:"hasEpoch"`
	Truncated   bool        `json:"truncated"`
	Closed      bool        `json:"closed"`
	Unsupported []string    `json:"unsupported"`
	Meta        []kvDTO     `json:"meta"`
	Attachments []attachDTO `json:"attachments"`
	Streams     []streamDTO `json:"streams"`
}

type kvDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type attachDTO struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

type streamDTO struct {
	UUID     string            `json:"uuid"`
	Name     string            `json:"name"`
	AxisKind string            `json:"axisKind"`
	AxisMode string            `json:"axisMode"`
	AxisExp  int8              `json:"axisExp"`
	AxisUnit string            `json:"axisUnit"`
	AxisMin  float64           `json:"axisMin"`
	AxisMax  float64           `json:"axisMax"`
	HasSpan  bool              `json:"hasSpan"`
	Records  int               `json:"records"`
	Meta     map[string]string `json:"meta"`
	Runs     []runDTO          `json:"runs"`
	Fields   []fieldDTO        `json:"fields"`
}

type runDTO struct {
	ID     uint32            `json:"id"`
	Index  uint32            `json:"index"`
	Label  string            `json:"label"`
	Params map[string]string `json:"params"`
}

type fieldDTO struct {
	Index     int               `json:"index"`
	Name      string            `json:"name"`
	Unit      string            `json:"unit"`
	Desc      string            `json:"desc"`
	Type      string            `json:"type"`
	Class     string            `json:"class"`
	Guarded   bool              `json:"guarded"`
	IsAxis    bool              `json:"isAxis"`
	BitOffset uint32            `json:"bitOffset"`
	BitWidth  uint32            `json:"bitWidth"`
	BigEndian bool              `json:"bigEndian"`
	Variable  bool              `json:"variable"`
	Conv      string            `json:"conv"`
	Meta      map[string]string `json:"meta"`
	// Plottable is false for blobs and for the field carrying an explicit
	// axis. The UI still lists them — they are part of what the file says —
	// but they never become a trace.
	Plottable bool `json:"plottable"`
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	f, _, ok := s.ready(w)
	if !ok {
		return
	}
	dto := fileDTO{
		Path:        f.Path,
		Size:        f.Size,
		Epoch:       f.Epoch,
		HasEpoch:    f.HasEpoch,
		Truncated:   f.Truncated,
		Closed:      f.Closed,
		Unsupported: nonNilStrings(f.Unsupported),
		Meta:        []kvDTO{},
		Attachments: []attachDTO{},
		Streams:     []streamDTO{},
	}
	for _, m := range f.Meta {
		dto.Meta = append(dto.Meta, kvDTO{m.Key, m.Value})
	}
	for _, a := range f.Attachments {
		dto.Attachments = append(dto.Attachments, attachDTO{a.Name, a.Size})
	}
	for _, st := range f.Streams {
		sd := streamDTO{
			UUID: st.UUID, Name: st.Name,
			AxisKind: st.AxisKind, AxisMode: st.AxisMode,
			AxisExp: st.AxisExp, AxisUnit: st.AxisUnit,
			AxisMin: st.AxisMin, AxisMax: st.AxisMax, HasSpan: st.HasSpan,
			Records: st.Records, Meta: st.Meta,
			Runs: []runDTO{}, Fields: []fieldDTO{},
		}
		for _, run := range st.Runs {
			sd.Runs = append(sd.Runs, runDTO{run.ID, run.Index, run.Label(), run.Params})
		}
		for i := range st.Fields {
			fd := &st.Fields[i]
			sd.Fields = append(sd.Fields, fieldDTO{
				Index: fd.Index, Name: fd.Name, Unit: fd.Unit, Desc: fd.Desc,
				Type: fd.Type, Class: string(fd.Class), Guarded: fd.Guarded,
				IsAxis: fd.IsAxis, BitOffset: fd.BitOffset, BitWidth: fd.BitWidth,
				BigEndian: fd.BigEndian, Variable: fd.Variable, Conv: fd.Conv,
				Meta: fd.Meta,
				// A stream can be declared and never write a record — visible
				// through Reader.OnSchema since Phase 2. It belongs in the tree
				// because it is part of what the file says, but there is
				// nothing behind it to plot.
				//
				// ClassBlob is the only class with nothing to draw at all. An
				// event field has no y value, but it does have a position on
				// the axis and something to say there, which is a lane.
				Plottable: !fd.IsAxis && fd.Class != index.ClassBlob && st.Records > 0,
			})
		}
		dto.Streams = append(dto.Streams, sd)
	}
	writeJSON(w, dto)
}

// --- /api/series and /api/states -------------------------------------------

type seriesDTO struct {
	Stream string `json:"stream"`
	Field  string `json:"field"`
	Unit   string `json:"unit"`
	Run    uint32 `json:"run"`
	Exact  bool   `json:"exact"`
	// Tier says which path answered: "exact" decoded the samples, "stats"
	// reduced per-frame summaries without decoding. The UI shows it rather
	// than implying a precision the answer does not have.
	Tier string     `json:"tier"`
	X    []float64  `json:"x"`
	Min  []*float64 `json:"min"`
	Max  []*float64 `json:"max"`
	N    []int32    `json:"n"`
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	_, q, ok := s.ready(w)
	if !ok {
		return
	}
	st, fd, run, err := s.resolve(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fd.Class == index.ClassCategorical {
		http.Error(w, "field is categorical: use /api/states", http.StatusBadRequest)
		return
	}
	from, to := rangeOf(r, st)
	points := intParam(r, "points", 1000, 1, 20000)

	e, tier, err := q.Envelope(st, fd, run, from, to, points)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("format") == "bin" {
		writeSeriesBinary(w, e, string(tier))
		return
	}
	writeJSON(w, seriesDTO{
		Stream: st.Name, Field: fd.Name, Unit: fd.Unit, Run: run,
		Exact: e.Exact, Tier: string(tier),
		X: e.X, Min: nullable(e.Min), Max: nullable(e.Max), N: e.N,
	})
}

type statesDTO struct {
	Stream string           `json:"stream"`
	Field  string           `json:"field"`
	Run    uint32           `json:"run"`
	Tier   string           `json:"tier"`
	States []decimate.State `json:"states"`
}

func (s *Server) handleStates(w http.ResponseWriter, r *http.Request) {
	_, q, ok := s.ready(w)
	if !ok {
		return
	}
	st, fd, run, err := s.resolve(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	from, to := rangeOf(r, st)
	points := intParam(r, "points", 1000, 1, 20000)

	states, tier, err := q.States(st, fd, run, from, to, points)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, statesDTO{Stream: st.Name, Field: fd.Name, Run: run, Tier: string(tier), States: states})
}

// resolve looks up the stream, field and run named by the request.
func (s *Server) resolve(r *http.Request) (*index.Stream, *index.Field, uint32, error) {
	f, _, _, _, _, _, _ := s.state.snapshot()
	qs := r.URL.Query()
	st := f.Stream(qs.Get("stream"))
	if st == nil {
		return nil, nil, 0, fmt.Errorf("unknown stream %q", qs.Get("stream"))
	}

	fd, err := fieldOf(st, qs.Get("field"))
	if err != nil {
		return nil, nil, 0, err
	}
	if fd.IsAxis || fd.Class == index.ClassBlob {
		return nil, nil, 0, fmt.Errorf("%s.%s is not plottable", st.Name, fd.Name)
	}
	// An event field has no numeric value at all, so reducing it here would
	// produce a chart of nothing but gaps rather than an error. Say where it
	// belongs instead.
	if fd.Class == index.ClassEvent {
		return nil, nil, 0, fmt.Errorf("%s.%s is an event field: use /api/events", st.Name, fd.Name)
	}

	run := uint32(intParam(r, "run", 0, 0, math.MaxInt32))
	if len(st.Frames(run)) == 0 {
		return nil, nil, 0, fmt.Errorf("%s.%s has no data for run %d", st.Name, fd.Name, run)
	}
	return st, fd, run, nil
}

// rangeOf reads the requested window, defaulting to the stream's whole extent.
func rangeOf(r *http.Request, st *index.Stream) (float64, float64) {
	from, to := st.AxisMin, st.AxisMax
	if v, err := strconv.ParseFloat(r.URL.Query().Get("from"), 64); err == nil {
		from = v
	}
	if v, err := strconv.ParseFloat(r.URL.Query().Get("to"), 64); err == nil {
		to = v
	}
	if to < from {
		from, to = to, from
	}
	return from, to
}

// --- /api/attach -----------------------------------------------------------

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	f, _, ok := s.ready(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	for _, a := range f.Attachments {
		if a.Name == name {
			// Attachments are opaque payloads carried by the log — a DBC, a
			// config dump. The viewer surfaces them and never parses them, so
			// they are served as bytes and typed conservatively.
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Write(a.Data)
			return
		}
	}
	http.Error(w, "no such attachment", http.StatusNotFound)
}

// --- helpers ---------------------------------------------------------------

// nullable converts NaN bounds to JSON null.
//
// This is the wire-level half of the absence rule. NaN is not representable in
// JSON, and encoding/json would refuse the whole response; sending 0 instead
// would be worse than refusing, because it draws.
func nullable(vs []float64) []*float64 {
	out := make([]*float64, len(vs))
	for i, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[i] = &vs[i]
	}
	return out
}

func intParam(r *http.Request, name string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
