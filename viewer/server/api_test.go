package server

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
)

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	const path = "../../testdata/can-example.logb"
	f, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := index.NewAccessor(path, f.Frames)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acc.Close() })
	s := httptest.NewServer(New(f, query.New(f, acc), nil))
	t.Cleanup(s.Close)
	return s
}

func getJSON[T any](t *testing.T, s *httptest.Server, path string) T {
	t.Helper()
	r, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", path, r.Status)
	}
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return v
}

func status(t *testing.T, s *httptest.Server, path string) (int, string) {
	t.Helper()
	r, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, string(body)
}

func uuidOf(t *testing.T, s *httptest.Server, name string) string {
	t.Helper()
	f := getJSON[fileDTO](t, s, "/api/file")
	for _, st := range f.Streams {
		if st.Name == name {
			return st.UUID
		}
	}
	t.Fatalf("no stream %q", name)
	return ""
}

func TestFileEndpoint(t *testing.T) {
	s := serve(t)
	f := getJSON[fileDTO](t, s, "/api/file")

	if len(f.Streams) != 4 || !f.Closed || f.Truncated {
		t.Fatalf("unexpected file summary: streams=%d closed=%v truncated=%v",
			len(f.Streams), f.Closed, f.Truncated)
	}
	if len(f.Attachments) != 1 || f.Attachments[0].Name != "example.dbc" {
		t.Errorf("attachments = %+v", f.Attachments)
	}

	// Streams are addressed by UUID, never by stream_id: stream_id is
	// segment-scoped and rebound by every SYNC frame (SPEC §6.6).
	for _, st := range f.Streams {
		if len(st.UUID) != 32 {
			t.Errorf("stream %s: uuid = %q, want 32 hex chars", st.Name, st.UUID)
		}
	}

	// The axis field is not offered as a signal. A byte blob is: it has no
	// y value, but it has a position and a label, which draws as an event lane.
	can := streamDTOnamed(t, f, "can0.raw")
	for _, fd := range can.Fields {
		switch fd.Name {
		case "t_us":
			if !fd.IsAxis || fd.Plottable {
				t.Errorf("t_us: isAxis=%v plottable=%v, want true/false", fd.IsAxis, fd.Plottable)
			}
		case "payload":
			if fd.Class != "event" || !fd.Plottable {
				t.Errorf("payload: class=%s plottable=%v, want event/true", fd.Class, fd.Plottable)
			}
		case "can_id", "dlc":
			if !fd.Plottable {
				t.Errorf("%s: not plottable", fd.Name)
			}
		}
	}
}

func TestSeriesEnvelope(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "EngineData")

	d := getJSON[seriesDTO](t, s, "/api/series?stream="+u+"&field=EngineSpeed&points=5")
	if d.Exact {
		t.Error("300 samples into 5 buckets should not be exact")
	}
	if len(d.X) != 5 || len(d.Min) != 5 || len(d.Max) != 5 || len(d.N) != 5 {
		t.Fatalf("ragged response: x=%d min=%d max=%d n=%d", len(d.X), len(d.Min), len(d.Max), len(d.N))
	}
	if d.Unit != "rpm" {
		t.Errorf("unit = %q, want rpm", d.Unit)
	}
	total := int32(0)
	for i, n := range d.N {
		total += n
		if n == 0 {
			continue
		}
		if d.Min[i] == nil || d.Max[i] == nil {
			t.Errorf("bucket %d has n=%d but a null bound", i, n)
			continue
		}
		if *d.Min[i] > *d.Max[i] {
			t.Errorf("bucket %d: min %v > max %v", i, *d.Min[i], *d.Max[i])
		}
	}
	if total != 300 {
		t.Errorf("buckets cover %d samples, want 300", total)
	}
}

// TestSeriesExactAtHighZoom checks that zooming far enough in stops decimating
// and returns the samples themselves.
func TestSeriesExactAtHighZoom(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "EngineData")

	// 50 ms at 10 ms spacing: five or six samples, well under the budget.
	d := getJSON[seriesDTO](t, s, "/api/series?stream="+u+"&field=EngineSpeed&from=0&to=50000000&points=1000")
	if !d.Exact {
		t.Error("a 50 ms window should be returned exactly")
	}
	if len(d.X) < 5 || len(d.X) > 7 {
		t.Errorf("samples in 50 ms = %d, want ~6", len(d.X))
	}
	for i, n := range d.N {
		if n != 1 {
			t.Errorf("exact sample %d has n=%d, want 1", i, n)
		}
	}
}

func TestStatesResolveWhenZoomedIn(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "VehicleStatus")

	// Wide: far more transitions than buckets, so every bucket is mixed and
	// none may claim a single gear.
	wide := getJSON[statesDTO](t, s, "/api/states?stream="+u+"&field=Gear&points=4")
	for i, st := range wide.States {
		if !st.Mixed {
			t.Errorf("wide bucket %d not mixed", i)
		}
		if st.Label != "" {
			t.Errorf("wide bucket %d labelled %q: a mixed bucket must not name one of its states", i, st.Label)
		}
	}

	// Narrow: the gears resolve. logbdump prints Gear="3" then "4" then "3"
	// for the first three records, and this must agree with it.
	tight := getJSON[statesDTO](t, s, "/api/states?stream="+u+"&field=Gear&from=0&to=100000000&points=500")
	if len(tight.States) < 3 {
		t.Fatalf("states = %d, want several", len(tight.States))
	}
	for i, want := range []string{"3", "4", "3"} {
		if tight.States[i].Mixed {
			t.Errorf("state %d unexpectedly mixed", i)
			continue
		}
		if tight.States[i].Label != want {
			t.Errorf("state %d = %q, want %q", i, tight.States[i].Label, want)
		}
	}
}

// TestCategoricalRejectedFromSeries: a min/max envelope over an enumeration is
// meaningless, so the endpoint refuses rather than returning a plausible
// number the client would draw as a line.
func TestCategoricalRejectedFromSeries(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "VehicleStatus")
	code, body := status(t, s, "/api/series?stream="+u+"&field=Gear")
	if code != http.StatusBadRequest || !strings.Contains(body, "categorical") {
		t.Errorf("got %d %q, want 400 mentioning categorical", code, body)
	}
}

func TestBadRequests(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "EngineData")

	for _, c := range []struct{ path, want string }{
		{"/api/series?stream=nope&field=x", "unknown stream"},
		{"/api/series?stream=" + u + "&field=nope", "no field"},
		{"/api/series?stream=" + u + "&field=EngineSpeed&run=99", "no data for run"},
	} {
		code, body := status(t, s, c.path)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("GET %s: got %d %q, want 400 containing %q", c.path, code, body, c.want)
		}
	}
}

func TestAttachment(t *testing.T) {
	s := serve(t)
	code, body := status(t, s, "/api/attach/example.dbc")
	if code != http.StatusOK || len(body) != 499 {
		t.Errorf("got %d, %d bytes; want 200, 499 bytes", code, len(body))
	}
	if code, _ := status(t, s, "/api/attach/nope"); code != http.StatusNotFound {
		t.Errorf("missing attachment: got %d, want 404", code)
	}
}

// TestNullableEncodesAbsenceAsNull is the wire half of the absence rule.
//
// NaN is not representable in JSON and would make encoding/json fail the whole
// response; a zero would encode fine and then draw as a value the recording
// never contained. Null is the only honest encoding, and it is what uPlot
// renders as a break in the line.
func TestNullableEncodesAbsenceAsNull(t *testing.T) {
	got := nullable([]float64{1, math.NaN(), 3, math.Inf(1), math.Inf(-1)})
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	for i, want := range []bool{true, false, true, false, false} {
		if (got[i] != nil) != want {
			t.Errorf("index %d: non-nil = %v, want %v", i, got[i] != nil, want)
		}
	}
	if *got[0] != 1 || *got[2] != 3 {
		t.Errorf("finite values corrupted: %v %v", *got[0], *got[2])
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[1,null,3,null,null]" {
		t.Errorf("json = %s, want [1,null,3,null,null]", b)
	}
}

func streamDTOnamed(t *testing.T, f fileDTO, name string) streamDTO {
	t.Helper()
	for _, st := range f.Streams {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("no stream %q", name)
	return streamDTO{}
}

// TestBinarySeriesMatchesJSON checks the two encodings agree.
//
// The binary path exists to skip a parse and halve the bytes, not to be a
// second, subtly different answer. NaN carries absence there where JSON has to
// spell it null, and those must line up exactly with the counts.
func TestBinarySeriesMatchesJSON(t *testing.T) {
	s := serve(t)
	u := uuidOf(t, s, "EngineData")
	q := "stream=" + u + "&field=EngineSpeed&points=64"

	want := getJSON[seriesDTO](t, s, "/api/series?"+q)

	r, err := http.Get(s.URL + "/api/series?" + q + "&format=bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("binary series: %s", r.Status)
	}
	if got := r.Header.Get("X-Logb-Tier"); got == "" {
		t.Error("no X-Logb-Tier header on the binary response")
	}
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}

	if string(buf[0:4]) != seriesMagic {
		t.Fatalf("magic = %q, want %q", buf[0:4], seriesMagic)
	}
	if v := binary.LittleEndian.Uint16(buf[4:]); v != seriesVersion {
		t.Fatalf("version = %d, want %d", v, seriesVersion)
	}
	exact := buf[6]&flagExact != 0
	if exact != want.Exact {
		t.Errorf("exact = %v, want %v", exact, want.Exact)
	}

	n := int(binary.LittleEndian.Uint32(buf[8:]))
	if n != len(want.X) {
		t.Fatalf("binary has %d buckets, JSON %d", n, len(want.X))
	}
	if len(buf) != seriesHeader+n*(8+8+8+4) {
		t.Fatalf("payload is %d bytes, want %d", len(buf), seriesHeader+n*(8+8+8+4))
	}

	f64 := func(off, i int) float64 {
		return math.Float64frombits(binary.LittleEndian.Uint64(buf[off+i*8:]))
	}
	xOff, minOff, maxOff := seriesHeader, seriesHeader+n*8, seriesHeader+n*16
	cntOff := seriesHeader + n*24

	// The browser reads these as TypedArray views straight over the response,
	// and `new Float64Array(buf, off, n)` throws unless off is a multiple of 8.
	// Go can read any offset, so nothing else here would notice — and the
	// symptom is every numeric chart silently blank.
	for _, off := range []int{xOff, minOff, maxOff} {
		if off%8 != 0 {
			t.Errorf("float64 section starts at %d, which is not 8-byte aligned", off)
		}
	}
	if cntOff%4 != 0 {
		t.Errorf("int32 section starts at %d, which is not 4-byte aligned", cntOff)
	}

	for i := 0; i < n; i++ {
		if f64(xOff, i) != want.X[i] {
			t.Fatalf("bucket %d: x %v binary, %v JSON", i, f64(xOff, i), want.X[i])
		}
		count := int32(binary.LittleEndian.Uint32(buf[cntOff+i*4:]))
		if count != want.N[i] {
			t.Fatalf("bucket %d: n %d binary, %d JSON", i, count, want.N[i])
		}

		bmin, bmax := f64(minOff, i), f64(maxOff, i)
		if count == 0 {
			// Absence: NaN in binary, null in JSON. Both must say the same
			// thing, and neither may say zero.
			if !math.IsNaN(bmin) || !math.IsNaN(bmax) {
				t.Fatalf("bucket %d is empty but carries %v..%v", i, bmin, bmax)
			}
			if want.Min[i] != nil || want.Max[i] != nil {
				t.Fatalf("bucket %d is empty but JSON carries a bound", i)
			}
			continue
		}
		if want.Min[i] == nil || want.Max[i] == nil {
			t.Fatalf("bucket %d has n=%d but a null JSON bound", i, count)
		}
		if bmin != *want.Min[i] || bmax != *want.Max[i] {
			t.Fatalf("bucket %d: %v..%v binary, %v..%v JSON",
				i, bmin, bmax, *want.Min[i], *want.Max[i])
		}
	}
}
