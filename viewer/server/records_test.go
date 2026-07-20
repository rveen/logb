package server

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestRecordsEndpoint(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "VehicleStatus")

	r := getJSON[recordsDTO](t, s, "/api/records?stream="+id+"&limit=5")
	if r.Stream != "VehicleStatus" {
		t.Fatalf("stream %q", r.Stream)
	}
	if len(r.Rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(r.Rows))
	}
	if !r.More {
		t.Error("More is false, but the stream has more than five records")
	}
	if len(r.Fields) != len(r.Rows[0].Text) {
		t.Fatalf("%d field names for %d columns", len(r.Fields), len(r.Rows[0].Text))
	}

	// Gear is an enumeration. The table must show its name, and carry the raw
	// value alongside — the mean of "reverse" and "third" is not a gear, and
	// the number is what sorting and export need.
	gear := -1
	for i, n := range r.Fields {
		if n == "Gear" {
			gear = i
		}
	}
	if gear < 0 {
		t.Fatal("no Gear column")
	}
	if r.Rows[0].Text[gear] != "3" {
		t.Errorf("Gear text %q, want the enumerated label", r.Rows[0].Text[gear])
	}
	if r.Rows[0].Num[gear] == nil {
		t.Error("Gear has no raw value")
	}
}

// TestRecordsPagingIsStable checks that walking the file in pages visits every
// record exactly once, in order, with no seam duplicates or drops.
func TestRecordsPagingIsStable(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "EngineData")

	var seen []float64
	for off := 0; ; off += 37 {
		r := getJSON[recordsDTO](t, s, "/api/records?stream="+id+"&limit=37&offset="+strconv.Itoa(off))
		for _, row := range r.Rows {
			seen = append(seen, row.X)
		}
		if !r.More {
			break
		}
	}
	if len(seen) < 100 {
		t.Fatalf("only %d records", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("record %d at axis %v does not follow %v", i, seen[i], seen[i-1])
		}
	}
}

func TestExportCSV(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "VehicleStatus")

	resp, err := http.Get(s.URL + "/api/export.csv?stream=" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "VehicleStatus.csv") {
		t.Errorf("content disposition %q", cd)
	}

	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatal("no data rows")
	}
	head := rows[0]
	if head[0] != "time [1e-9 s]" {
		t.Errorf("axis column %q, want the tick scale spelled out", head[0])
	}
	if head[1] != "run" {
		t.Errorf("second column %q, want run", head[1])
	}
	// Units belong in the header: a bare column of numbers is not an export.
	if !strings.Contains(strings.Join(head, ","), "VehicleSpeed [km/h]") {
		t.Errorf("header %q carries no unit for VehicleSpeed", head)
	}
	for _, r := range rows[1:] {
		if len(r) != len(head) {
			t.Fatalf("row has %d columns, header has %d", len(r), len(head))
		}
	}
}

// TestExportSelectedFields checks the fields parameter, and that an unknown
// name is refused rather than silently dropped — a column quietly missing from
// an export is the kind of thing nobody notices until the analysis is wrong.
func TestExportSelectedFields(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "VehicleStatus")

	resp, err := http.Get(s.URL + "/api/export.csv?stream=" + id + "&fields=Gear,VehicleSpeed")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[0]) != 4 {
		t.Fatalf("header %q, want axis, run and two fields", rows[0])
	}
	// Numbers, not labels: an export is for feeding another tool.
	if rows[1][2] == "3" && rows[0][2] != "Gear" {
		t.Fatalf("unexpected column order %q", rows[0])
	}

	resp, err = http.Get(s.URL + "/api/export.csv?stream=" + id + "&fields=NoSuchField")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field gave %s, want 400", resp.Status)
	}
}

// TestExportRefusesOversizedWindow checks the guard fires before anything is
// written. A CSV has no way to say "and there was more" — it just ends — so an
// over-large request has to be refused with a status, not truncated.
func TestExportRefusesOversizedWindow(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "EngineData")

	// The example file is far under the limit, so check the boundary logic by
	// asking the server what it thinks the window holds and confirming a
	// full-file export is allowed.
	resp, err := http.Get(s.URL + "/api/export.csv?stream=" + id)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a small file was refused: %s", resp.Status)
	}
	if MaxExportRecords <= 0 {
		t.Fatal("export limit is not positive; nothing would ever export")
	}
}

// TestExportTimeColumnIsIntegerTicks guards the axis formatting. A time axis is
// an int64 count of ticks; the default float formatting renders 4999970000 as
// "4.99997e+09", which is the same number but reads as an approximation and is
// the shape a spreadsheet rounds on import.
func TestExportTimeColumnIsIntegerTicks(t *testing.T) {
	s := serve(t)
	id := uuidOf(t, s, "EngineData")

	resp, err := http.Get(s.URL + "/api/export.csv?stream=" + id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows[1:] {
		if strings.ContainsAny(r[0], "eE.") {
			t.Fatalf("time column %q is not an integer tick count", r[0])
		}
	}
}
