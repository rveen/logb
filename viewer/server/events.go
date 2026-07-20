package server

import (
	"fmt"
	"net/http"

	"github.com/rveen/logb/viewer/decimate"
	"github.com/rveen/logb/viewer/index"
)

type eventsDTO struct {
	Stream string `json:"stream"`
	Field  string `json:"field"`
	Tier   string `json:"tier"`
	// Events carries the occurrences themselves when the window was narrow
	// enough; Density carries per-frame counts when it was not. Exactly one is
	// populated, and Tier says which — a lane showing counts must not look like
	// a lane showing events.
	Events  []decimate.Event   `json:"events"`
	Density []decimate.Density `json:"density"`
}

// handleEvents serves an event lane: a string or byte field drawn as a mark per
// record rather than as a line.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	_, q, ok := s.ready(w)
	if !ok {
		return
	}
	st, run, err := s.resolveStream(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fd, err := fieldOf(st, r.URL.Query().Get("field"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if fd.Class != index.ClassEvent {
		http.Error(w, fmt.Sprintf("%s.%s is not an event field: use /api/series or /api/states",
			st.Name, fd.Name), http.StatusBadRequest)
		return
	}
	from, to := rangeOf(r, st)

	events, density, tier, err := q.Events(st, fd, run, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, eventsDTO{
		Stream: st.Name, Field: fd.Name, Tier: string(tier),
		Events: events, Density: density,
	})
}

// fieldOf looks up a field by name.
func fieldOf(st *index.Stream, name string) (*index.Field, error) {
	for i := range st.Fields {
		if st.Fields[i].Name == name {
			return &st.Fields[i], nil
		}
	}
	return nil, fmt.Errorf("stream %s has no field %q", st.Name, name)
}
