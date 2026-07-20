package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
)

// State is the server's view of a file that may still be being indexed.
//
// Indexing a large file takes tens of seconds, and it cannot be made
// incremental: nothing in a Logb file points forward, so there is no footer to
// read and no shortcut to the answer. What can be avoided is making the user
// stare at a blank browser while it happens — the server comes up immediately
// and reports progress until the index is ready.
type State struct {
	mu    sync.RWMutex
	file  *index.File
	q     *query.Query
	done  int64
	total int64
	err   error
	ready bool

	// waiters are closed and replaced on every change, so an SSE stream can
	// block until something actually happens rather than polling.
	changed chan struct{}
}

func NewState() *State {
	return &State{changed: make(chan struct{})}
}

// Progress records how far a scan has got.
func (s *State) Progress(done, total int64) {
	s.mu.Lock()
	s.done, s.total = done, total
	s.mu.Unlock()
	s.notify()
}

// Ready publishes the finished index.
func (s *State) Ready(f *index.File, q *query.Query) {
	s.mu.Lock()
	s.file, s.q, s.ready = f, q, true
	s.mu.Unlock()
	s.notify()
}

// Fail publishes an indexing failure. The server stays up so the browser can
// show what went wrong rather than failing to connect.
func (s *State) Fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.notify()
}

func (s *State) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *State) snapshot() (f *index.File, q *query.Query, done, total int64, err error, ready bool, changed chan struct{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file, s.q, s.done, s.total, s.err, s.ready, s.changed
}

// statusDTO is what every data endpoint returns while indexing is in flight.
type statusDTO struct {
	Indexing bool    `json:"indexing"`
	Done     int64   `json:"done"`
	Total    int64   `json:"total"`
	Percent  float64 `json:"percent"`
	Error    string  `json:"error,omitempty"`
}

func (s *State) status() statusDTO {
	_, _, done, total, err, ready, _ := s.snapshot()
	d := statusDTO{Indexing: !ready, Done: done, Total: total}
	if total > 0 {
		d.Percent = float64(done) * 100 / float64(total)
	}
	if err != nil {
		d.Error = err.Error()
	}
	return d
}

// hold answers a request that arrived before the index was ready.
//
// 503 with Retry-After is the honest code: the resource genuinely exists and
// genuinely is not available yet. The body carries progress so the client can
// draw a bar without a second round trip.
func (s *State) hold(w http.ResponseWriter) {
	st := s.status()
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Content-Type", "application/json")
	code := http.StatusServiceUnavailable
	if st.Error != "" {
		code = http.StatusInternalServerError
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(st)
}

// handleProgress streams indexing progress as server-sent events.
//
// It emits one event immediately so a client that connects after indexing has
// already finished is told so rather than waiting for a change that will never
// come.
func (s *State) handleProgress(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		st := s.status()
		b, _ := json.Marshal(st)
		name := "progress"
		if !st.Indexing {
			name = "ready"
		}
		if st.Error != "" {
			name = "failed"
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		flusher.Flush()
		return st.Indexing && st.Error == ""
	}

	if !send() {
		return
	}
	for {
		_, _, _, _, _, _, changed := s.snapshot()
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			if !send() {
				return
			}
		case <-time.After(2 * time.Second):
			// A keepalive, so a proxy between here and the browser does not
			// decide the connection is idle and drop it.
			if !send() {
				return
			}
		}
	}
}
