package server

import (
	"encoding/binary"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/rveen/logb"
	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
)

// serveRack writes a supply channel set to 12 V at 1 s and restated in three
// later segments, then serves it. Only the first segment holds a record, so
// every window after it depends on the restatement.
func serveRack(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rack.logb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := logb.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	s := &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("rack/psu1.ch1")),
		Name:       "psu1.ch1",
		RecordBits: 8 * 16,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1),
		AxisField:  0,
		Fields: []logb.Field{
			{Name: "t", BitOffset: 0, BitWidth: 64, Type: logb.TypeUint},
			{Name: "v.set", BitOffset: 64, BitWidth: 32, Type: logb.TypeFloat, Unit: "V", Hold: true},
			// Sampled, not held: the readback moves on its own, so it is the
			// control for every assertion about held behaviour below.
			{Name: "v.meas", BitOffset: 96, BitWidth: 32, Type: logb.TypeFloat, Unit: "V"},
		},
	}
	rec := func(at uint64, v float32) []byte {
		b := make([]byte, 16)
		binary.LittleEndian.PutUint64(b[0:], at)
		binary.LittleEndian.PutUint32(b[8:], math.Float32bits(v))
		binary.LittleEndian.PutUint32(b[12:], math.Float32bits(v-0.02))
		return b
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetHold(s, logb.TickVal(1_000_000_000), 0, []bool{false, true, false},
		rec(1_000_000_000, 12)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.BeginSegment(0); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fi, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := index.NewAccessor(path, fi.Frames)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acc.Close() })
	srv := httptest.NewServer(New(fi, query.New(fi, acc), nil))
	t.Cleanup(srv.Close)
	return srv, fi.Streams[0].UUID
}

func TestFieldsReportHold(t *testing.T) {
	s, _ := serveRack(t)
	f := getJSON[fileDTO](t, s, "/api/file")
	if len(f.Streams) != 1 {
		t.Fatalf("streams = %d", len(f.Streams))
	}
	fields := f.Streams[0].Fields
	if fields[0].Hold {
		t.Error("the axis field is not held")
	}
	if !fields[1].Hold {
		t.Error("v.set is held and the API does not say so")
	}
}

// TestSeriesCarriesTheAnchor: a window opened after the only record still has
// to know what the channel is set to, or the trace starts nowhere.
func TestSeriesCarriesTheAnchor(t *testing.T) {
	s, uuid := serveRack(t)
	// A window well after the single record, in epoch-relative ticks.
	d := getJSON[seriesDTO](t, s,
		"/api/series?stream="+uuid+"&field=v.set&from=5e9&to=9e9&points=100")
	if !d.Hold {
		t.Error("series does not report the field as held")
	}
	if d.Anchor == nil {
		t.Fatal("no anchor: a held channel would start at the left edge of the window")
	}
	if d.Anchor.V != 12 {
		t.Errorf("anchor value = %v, want 12", d.Anchor.V)
	}
	// The anchor sits where the value was set, which is before the window. A
	// clamped anchor would claim the setpoint moved when the user scrolled.
	if d.Anchor.X != 0 {
		t.Errorf("anchor x = %v, want 0 (where it was set), not the window start", d.Anchor.X)
	}
}

func TestSeriesBinaryCarriesTheAnchorInHeaders(t *testing.T) {
	s, uuid := serveRack(t)
	r, err := http.Get(s.URL + "/api/series?stream=" + uuid + "&field=v.set&from=5e9&to=9e9&points=100&format=bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %s", r.Status)
	}
	if got := r.Header.Get("X-Logb-Hold"); got != "1" {
		t.Errorf("X-Logb-Hold = %q, want \"1\"", got)
	}
	if got := r.Header.Get("X-Logb-Anchor"); got != "0,12" {
		t.Errorf("X-Logb-Anchor = %q, want \"0,12\"", got)
	}
}

// TestNoAnchorBeforeAnythingWasSet: a setpoint nobody has set is a gap, and a
// zero there is the same lie an absent guarded sample would be.
func TestNoAnchorBeforeAnythingWasSet(t *testing.T) {
	s, uuid := serveRack(t)
	d := getJSON[seriesDTO](t, s,
		"/api/series?stream="+uuid+"&field=v.set&from=-9e9&to=-5e9&points=100")
	if d.Anchor != nil {
		t.Errorf("anchor %v before anything was set", *d.Anchor)
	}
}

// A field that is not held gets no anchor and no hold flag, so nothing about
// the ordinary path changes.
func TestUnheldFieldHasNoAnchor(t *testing.T) {
	s, uuid := serveRack(t)
	d := getJSON[seriesDTO](t, s,
		"/api/series?stream="+uuid+"&field=v.meas&from=-9e9&to=9e9&points=100")
	if d.Hold || d.Anchor != nil {
		t.Errorf("unheld field reported hold=%v anchor=%v", d.Hold, d.Anchor)
	}
}
