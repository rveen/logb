package mdf

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
)

// Decoding a bus recording is the whole point of the exercise: a CAN log holds
// frames, and what someone wants to see is EngineSpeed. These tests check that
// the signals the importer writes are the ones the payload bytes actually mean,
// computed independently from the standard's own formulas.

func obd2(t *testing.T) (*File, *dbc.File) {
	t.Helper()
	f, err := os.Open("../testdata/mdf/obd2-trunc.mf4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	m, err := ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbc.ParseFile("../testdata/obd2.dbc")
	if err != nil {
		t.Fatal(err)
	}
	return m, db
}

func decodeOBD2(t *testing.T, o Options) map[string]*stripe {
	t.Helper()
	m, db := obd2(t)
	o.DBC = db
	var buf bytes.Buffer
	if err := Write(m, &buf, o); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, r)
}

// TestDecodedSignals is the end-to-end claim: with a database, a recording of
// CAN frames becomes streams of named signals, and each value equals what the
// OBD2 standard says those payload bytes mean.
func TestDecodedSignals(t *testing.T) {
	got := decodeOBD2(t, Options{Codec: logb.CodecZstd})

	// The raw frames are still there. A decoded signal is an interpretation,
	// and discarding the evidence for it would mean re-importing to check it.
	if got["CAN_DataFrame"] == nil {
		t.Fatal("the raw frames are gone; decoding should add streams, not replace them")
	}
	st := got["OBD2_Response"]
	if st == nil {
		t.Fatalf("no decoded stream; got %v", keysOf(got))
	}

	idx := map[string]int{}
	for i, f := range st.schema.Fields {
		idx[f.Name] = i
	}
	for _, name := range []string{"PID", "EngineSpeed", "VehicleSpeed", "CoolantTemp"} {
		if _, ok := idx[name]; !ok {
			t.Fatalf("no field %q in %v", name, idx)
		}
	}

	// Recompute every signal from the frame bytes, by the OBD2 formulas, and
	// compare. This is independent of both the DBC mapping and Logb's decoder.
	m, _ := obd2(t)
	g := m.Groups[0]
	var idc, payloadc *Channel
	for _, c := range g.Channels {
		switch c.Name {
		case "CAN_DataFrame.ID":
			idc = c
		case "CAN_DataFrame.DataBytes":
			payloadc = c
		}
	}

	row := 0
	checked := map[string]int{}
	for i := 0; i < g.Records; i++ {
		v, err := idc.Raw(g.Record(i))
		if err != nil {
			t.Fatal(err)
		}
		if v.(uint64) != 0x7E8 {
			continue
		}
		p := g.VLSD[payloadc][i]
		if row >= len(st.values) {
			t.Fatalf("only %d decoded records for %d responses", len(st.values), row+1)
		}
		vals := st.values[row]
		row++

		if got := vals[idx["PID"]]; !same(got, uint64(p[2])) {
			t.Fatalf("record %d: PID = %v, want %d", row-1, got, p[2])
		}

		// A, B are the OBD2 names for the first two data bytes.
		a, b := float64(p[3]), 0.0
		if len(p) > 4 {
			b = float64(p[4])
		}
		var name string
		var want float64
		switch p[2] {
		case 0x04:
			name, want = "EngineLoad", a*0.39215686
		case 0x05:
			name, want = "CoolantTemp", a-40
		case 0x0B:
			name, want = "IntakePressure", a
		case 0x0C:
			name, want = "EngineSpeed", (256*a+b)/4
		case 0x0D:
			name, want = "VehicleSpeed", a
		case 0x0F:
			name, want = "IntakeAirTemp", a-40
		case 0x11:
			name, want = "ThrottlePosition", a*0.39215686
		case 0x1F:
			name, want = "RunTime", 256*a+b
		default:
			continue
		}
		checked[name]++

		f := idx[name]
		gotv, err := asFloatOf(vals[f])
		if err != nil {
			t.Fatalf("record %d: %s: %v", row-1, name, err)
		}
		if math.Abs(gotv-want) > 1e-6 {
			t.Errorf("record %d: %s = %v, want %v (payload % x)", row-1, name, gotv, want, p)
		}

		// Every other multiplexed signal must be absent, not merely wrong.
		for other, j := range idx {
			if other == name || j == f || !st.schema.Fields[j].Guarded {
				continue
			}
			if _, isErr := vals[j].(error); !isErr {
				t.Fatalf("record %d carries PID 0x%02X, but %q decoded to %v instead of being absent",
					row-1, p[2], other, vals[j])
			}
		}
	}

	if checked["EngineSpeed"] == 0 {
		t.Fatal("no engine speed was checked; the fixture should contain PID 0x0C")
	}
	t.Logf("checked %d signals across %d frames: %v", len(checked), row, checked)
}

// TestEngineSpeedIsMotorola pins the one signal in the fixture that is
// big-endian. PID 0x0C is ((256*A)+B)/4, so its two bytes are Motorola — the
// case CAN.md is about — and the DBC start bit 31 must land at Logb bit offset
// 24, past the axis.
func TestEngineSpeedIsMotorola(t *testing.T) {
	got := decodeOBD2(t, Options{})
	st := got["OBD2_Response"]
	if st == nil {
		t.Fatal("no decoded stream")
	}
	for _, f := range st.schema.Fields {
		if f.Name != "EngineSpeed" {
			continue
		}
		if !f.BigEndian {
			t.Error("EngineSpeed should be big-endian")
		}
		if f.BitOffset != dbc.AxisBits+24 {
			t.Errorf("EngineSpeed at bit %d, want %d (DBC start bit 31)", f.BitOffset, dbc.AxisBits+24)
		}
		if f.BitWidth != 16 || f.Unit != "rpm" {
			t.Errorf("EngineSpeed is %d bits in %q", f.BitWidth, f.Unit)
		}
		return
	}
	t.Fatal("no EngineSpeed field")
}

// TestDatabaseTravelsWithTheData is the self-containment claim.
//
// A bus recording is not self-explanatory: it holds frames, and what they mean
// is in a database that is not in the file. That is true of MDF and equally true
// of a Logb file converted without one. Decoding at import fixes it only if the
// database comes too — otherwise the output says every signal's offset, unit and
// conversion but not what asserted them, and checking a suspect value means
// finding a file somebody else still has.
func TestDatabaseTravelsWithTheData(t *testing.T) {
	m, db := obd2(t)
	var buf bytes.Buffer
	if err := Write(m, &buf, Options{DBC: db}); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	streams := collect(t, r)

	// The database is in the file, byte for byte.
	raw, ok := r.Attachments["obd2.dbc"]
	if !ok {
		t.Fatalf("the database was not embedded; attachments: %v", keys(r.Attachments))
	}
	if !bytes.Equal(raw, db.Raw) {
		t.Errorf("the embedded database is %d bytes, the original %d", len(raw), len(db.Raw))
	}

	// And it reparses, which is the point of embedding it rather than a summary.
	back, err := dbc.Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the embedded database does not parse: %v", err)
	}
	if len(back.Messages) != len(db.Messages) {
		t.Errorf("reparsed %d messages, want %d", len(back.Messages), len(db.Messages))
	}

	meta := map[string]string{}
	for _, kv := range r.Meta {
		meta[kv.Key] = kv.Value
	}
	if meta["source.dbc"] != "obd2.dbc" {
		t.Errorf("source.dbc = %q, want the database's name", meta["source.dbc"])
	}
	if got := meta["source.dbc.sha256"]; got != db.SHA256() || got == "" {
		t.Errorf("source.dbc.sha256 = %q, want %q", got, db.SHA256())
	}

	// A decoded stream says which database and which message defined it.
	st := streams["OBD2_Response"]
	if st == nil {
		t.Fatal("no decoded stream")
	}
	if st.schema.Meta["dbc.database"] != "obd2.dbc" {
		t.Errorf("stream meta dbc.database = %q", st.schema.Meta["dbc.database"])
	}
	if st.schema.Meta["dbc.message"] != "OBD2_Response" {
		t.Errorf("stream meta dbc.message = %q", st.schema.Meta["dbc.message"])
	}
}

// TestNoDatabase checks that nothing changes without one: a recording is frames,
// and inventing signals from a file that does not describe them is not on offer.
func TestNoDatabase(t *testing.T) {
	got := decodeOBD2(t, Options{})
	with := len(got)
	m, _ := obd2(t)

	var buf bytes.Buffer
	if err := Write(m, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	without := len(collect(t, r))
	if without >= with {
		t.Errorf("%d streams without a database, %d with; the database should add streams", without, with)
	}
}

// TestUnknownFramesReported checks that frames the database does not describe
// are counted out loud rather than passed over.
func TestUnknownFramesReported(t *testing.T) {
	m, _ := obd2(t)
	db, err := dbc.Parse(bytes.NewReader([]byte(
		"BO_ 2024 OBD2_Response: 8 ECU\n SG_ PID : 16|8@1+ (1,0) [0|255] \"\"  T\n")))
	if err != nil {
		t.Fatal(err)
	}
	var warned []string
	var buf bytes.Buffer
	err = Write(m, &buf, Options{DBC: db, Warn: func(f string, a ...any) {
		warned = append(warned, sprintf(f, a...))
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warned {
		if contains(w, "0x7DF") && contains(w, "not in the database") {
			return
		}
	}
	t.Errorf("the requests on 0x7DF are not in this database and nothing said so: %v", warned)
}

func asFloatOf(v any) (float64, error) {
	if err, ok := v.(error); ok {
		return 0, err
	}
	f, ok := asFloat(v)
	if !ok {
		return 0, errors.New("not a number")
	}
	return f, nil
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }
