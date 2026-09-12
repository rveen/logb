package dbc

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/rveen/logb"
)

// decode reads a payload back through the path a recording takes: the
// message's Schema, a Logb writer and the reader. Encode is only as good as
// its agreement with that path, so that is what the tests hold it to — and
// that path is checked against Vector's reference algorithm in logb's own
// TestDBCMotorola. It returns each field's value, absent ones left out.
func decode(t *testing.T, m *Message, payload []byte) map[string]any {
	t.Helper()
	s, err := Schema(m, SchemaOptions{Namespace: "t", AxisExp: -9})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, err := logb.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	rec := make([]byte, AxisBits/8+len(payload))
	copy(rec[AxisBits/8:], payload)
	if err := w.WriteData(s, logb.TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]any{}
	for i := 1; i < len(s.Fields); i++ {
		v, err := b.Value(0, i)
		if errors.Is(err, logb.ErrFieldAbsent) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", s.Fields[i].Name, err)
		}
		vals[s.Fields[i].Name] = v
	}
	return vals
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// TestEncodeRoundTrip encodes physical values, names and flags into the test
// database's messages and reads them back through a recording.
func TestEncodeRoundTrip(t *testing.T) {
	d := parse(t, sample)
	for _, tc := range []struct {
		msg  string
		in   map[string]any
		want map[string]any
	}{
		{"EngineData",
			map[string]any{"EngineSpeed": 3000.0, "CoolantTemp": 90.0, "EngineRunning": true},
			map[string]any{"EngineSpeed": 3000.0, "CoolantTemp": 90.0, "EngineRunning": true}},
		// Motorola, unaligned and crossing bytes, beside an Intel signal and a
		// value table given by name.
		{"VehicleStatus",
			map[string]any{"VehicleSpeed": 123.45, "Odometer": 98765.4, "Gear": "Reverse"},
			map[string]any{"VehicleSpeed": 123.45, "Odometer": 98765.4, "Gear": "Reverse"}},
		{"J1939Message",
			map[string]any{"Torque": -100.0},
			map[string]any{"Torque": -100.0}},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			m := d.Message(tc.msg)
			p, err := m.Encode(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			got := decode(t, m, p)
			for name, want := range tc.want {
				g := got[name]
				gf, gok := g.(float64)
				wf, wok := want.(float64)
				if gok && wok && near(gf, wf) || g == want {
					continue
				}
				t.Errorf("%s: wrote %v, read back %v (payload %x)", name, want, g, p)
			}
		})
	}
}

// TestEncodeLayouts places a signal at every layout a classic frame allows —
// Intel and Motorola, every start bit, every width to 64, signed and unsigned —
// and a random raw value in each, and reads it back through a recording. A
// misplaced bit anywhere in the insertion shows up as a different value.
func TestEncodeLayouts(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	checked := 0
	for _, big := range []bool{false, true} {
		for start := 0; start < 64; start++ {
			for length := 2; length <= 64; length++ {
				sg := &Signal{Name: "S", Start: start, Length: length, BigEndian: big,
					Signed: rng.Intn(2) == 0, Factor: 1}
				if int(sg.BitOffset())+length > 64 {
					continue
				}
				m := &Message{ID: 1, Name: "M", Length: 8, Signals: []*Signal{sg}}

				raw := rng.Uint64() & mask(length)
				var in float64
				if sg.Signed {
					in = float64(int64(raw<<(64-length)) >> (64 - length))
				} else {
					in = float64(raw)
				}
				// Beyond 53 bits a float64 cannot say every raw value; those
				// widths are exercised at values it can.
				if length > 53 {
					in = math.Trunc(in / 2048)
				}
				p, err := m.Encode(map[string]any{"S": in})
				if err != nil {
					t.Fatalf("start=%d len=%d big=%v: %v", start, length, big, err)
				}
				got := decode(t, m, p)["S"]
				var g float64
				switch v := got.(type) {
				case uint64:
					g = float64(v)
				case int64:
					g = float64(v)
				default:
					t.Fatalf("start=%d len=%d: read back a %T", start, length, got)
				}
				if g != in {
					t.Fatalf("start=%d len=%d big=%v signed=%v: wrote %v, read back %v (payload %x)",
						start, length, big, sg.Signed, in, g, p)
				}
				checked++
			}
		}
	}
	t.Logf("%d layouts written and read back", checked)
}

// TestEncodeLeavesNeighbours packs signals edge to edge in both byte orders
// and checks each reads back as itself: a write that spilled a bit would change
// its neighbour.
func TestEncodeLeavesNeighbours(t *testing.T) {
	m := parse(t, `BO_ 100 Packed: 8 ECU
 SG_ A : 0|3@1+ (1,0) [0|0] "" X
 SG_ B : 3|7@1+ (1,0) [0|0] "" X
 SG_ C : 10|13@1- (1,0) [0|0] "" X
 SG_ D : 31|9@0+ (1,0) [0|0] "" X
 SG_ E : 38|11@0- (1,0) [0|0] "" X
 SG_ F : 59|4@0+ (1,0) [0|0] "" X
`).Messages[0]
	in := map[string]any{"A": 5.0, "B": 100.0, "C": -4000.0, "D": 300.0, "E": -777.0, "F": 9.0}
	p, err := m.Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, m, p)
	for name, want := range in {
		var g float64
		switch v := got[name].(type) {
		case uint64:
			g = float64(v)
		case int64:
			g = float64(v)
		}
		if g != want.(float64) {
			t.Errorf("%s: wrote %v, read back %v (payload %x)", name, want, got[name], p)
		}
	}
}

// TestEncodeRounds: a value between two steps goes to the nearer one.
func TestEncodeRounds(t *testing.T) {
	m := parse(t, sample).Message("EngineData")
	for _, c := range []struct{ in, want float64 }{
		{3000.1, 3000.0},  // 12000.4 steps
		{3000.2, 3000.25}, // 12000.8 steps
	} {
		p, err := m.Encode(map[string]any{"EngineSpeed": c.in, "CoolantTemp": 0.0, "EngineRunning": false})
		if err != nil {
			t.Fatal(err)
		}
		if got := decode(t, m, p)["EngineSpeed"].(float64); !near(got, c.want) {
			t.Errorf("%v rpm was sent as %v, want %v", c.in, got, c.want)
		}
	}
}

// TestEncodeStartValues: a signal not given a value takes the database's start
// value, per signal or as the attribute's default, and one with neither is
// refused rather than zeroed.
func TestEncodeStartValues(t *testing.T) {
	const db = `BO_ 100 M: 8 ECU
 SG_ Set : 0|8@1+ (1,0) [0|0] "" X
 SG_ Default : 8|8@1+ (1,0) [0|0] "" X
 SG_ Given : 16|8@1+ (0.5,0) [0|0] "" X

BO_ 200 N: 8 ECU
 SG_ Bare : 0|8@1+ (1,0) [0|0] "" X
 SG_ Named : 8|8@1+ (1,0) [0|0] "" X
`
	withDefault := db + `BA_DEF_ SG_ "GenSigStartValue" INT 0 255;
BA_DEF_DEF_ "GenSigStartValue" 7;
BA_ "GenSigStartValue" SG_ 100 Set 42;
`
	// The start value is raw: Given's factor of 0.5 applies to the value it
	// is given, not to the start value.
	d := parse(t, withDefault)
	m := d.Message("M")
	p, err := m.Encode(map[string]any{"Given": 10.0})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p[:3], []byte{42, 7, 20}) {
		t.Errorf("payload %x, want 2a0714: Set from its own start value, Default from the default, Given from the request", p[:3])
	}

	// Without the default, a signal with no start value must be named.
	bare := parse(t, db+`BA_ "GenSigStartValue" SG_ 200 Named 1;`+"\n").Message("N")
	if _, err := bare.Encode(map[string]any{}); err == nil || !strings.Contains(err.Error(), "Bare") {
		t.Errorf("an unset signal with no start value: %v, want a refusal naming Bare", err)
	}
	if _, err := bare.Encode(map[string]any{"Bare": 3.0}); err != nil {
		t.Errorf("Bare given, Named from its start value: %v", err)
	}
}

// TestEncodeMultiplexed: the multiplexor's value decides which signals the
// frame carries, and only those may be given.
func TestEncodeMultiplexed(t *testing.T) {
	m := parse(t, `BO_ 100 Mux: 8 ECU
 SG_ Selector M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ Temperature m1 : 8|16@1+ (0.1,-40) [-40|100] "degC"  Tester
 SG_ Pressure m2 : 8|16@1+ (0.5,0) [0|1000] "kPa"  Tester
`).Messages[0]
	p, err := m.Encode(map[string]any{"Selector": 2.0, "Pressure": 250.5})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, m, p)
	if v, ok := got["Pressure"].(float64); !ok || !near(v, 250.5) {
		t.Errorf("Pressure read back as %v", got["Pressure"])
	}
	if _, ok := got["Temperature"]; ok {
		t.Error("Temperature decoded from a frame whose Selector is 2")
	}
	if _, err := m.Encode(map[string]any{"Selector": 2.0, "Temperature": 20.0}); err == nil {
		t.Error("Temperature given with Selector 2 was accepted")
	}
	if _, err := m.Encode(map[string]any{"Pressure": 1.0}); err == nil {
		t.Error("a multiplexed frame with no multiplexor value was accepted")
	}
}

// TestEncodeRefuses: what would otherwise be adjusted into a different value
// is refused, and says why.
func TestEncodeRefuses(t *testing.T) {
	d := parse(t, sample)
	engine := d.Message("EngineData")
	full := func(k string, v any) map[string]any {
		m := map[string]any{"EngineSpeed": 1000.0, "CoolantTemp": 20.0, "EngineRunning": false}
		m[k] = v
		return m
	}
	nested := parse(t, `BO_ 100 Nested: 8 ECU
 SG_ Service M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ PID m1M : 8|8@1+ (1,0) [0|255] ""  Tester
 SG_ Value m3 : 16|16@1+ (1,0) [0|0] ""  Tester
`).Messages[0]
	wide := parse(t, `BO_ 100 Wide: 8 ECU
 SG_ Raw : 0|8@1+ (1,0) [0|0] ""  Tester
`).Messages[0]

	for _, c := range []struct {
		name string
		m    *Message
		in   map[string]any
		want string
	}{
		{"above the declared range", engine, full("CoolantTemp", 216.0), "outside the range"},
		{"below the declared range", engine, full("CoolantTemp", -41.0), "outside the range"},
		{"beyond what the bits hold", wide, map[string]any{"Raw": 256.0}, "does not fit"},
		{"negative into unsigned bits", wide, map[string]any{"Raw": -1.0}, "does not fit"},
		{"a signal the message lacks", engine, full("Boost", 1.0), "no signal"},
		{"a name the table lacks", d.Message("VehicleStatus"),
			map[string]any{"VehicleSpeed": 0.0, "Odometer": 0.0, "Gear": "Fifth"}, "no value named"},
		{"a name for a signal with no table", engine, full("EngineSpeed", "fast"), "no value table"},
		{"not a number", engine, full("EngineSpeed", math.NaN()), "not a value"},
		{"an unset signal", engine, map[string]any{"EngineSpeed": 1000.0}, "no value for"},
		{"two levels of multiplexing", nested, map[string]any{"Service": 1.0}, "more than one level"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.m.Encode(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want a refusal mentioning %q", err, c.want)
			}
		})
	}
}

// TestParseStartValues checks the attribute reading on its own.
func TestParseStartValues(t *testing.T) {
	d := parse(t, `BO_ 2147483748 Ext: 8 ECU
 SG_ A : 0|8@1+ (1,0) [0|0] "" X
 SG_ B : 8|8@1+ (1,0) [0|0] "" X
BA_DEF_DEF_ "GenSigStartValue" 3;
BA_ "GenSigStartValue" SG_ 2147483748 A 9;
`)
	a, b := d.Messages[0].Signals[0], d.Messages[0].Signals[1]
	if !a.HasInitial || a.Initial != 9 {
		t.Errorf("A: %v %v, want its own start value 9, on an extended message", a.HasInitial, a.Initial)
	}
	if !b.HasInitial || b.Initial != 3 {
		t.Errorf("B: %v %v, want the default 3", b.HasInitial, b.Initial)
	}
	if fmt.Sprint(parse(t, sample).Messages[0].Signals[0].HasInitial) != "false" {
		t.Error("a database without the attribute gave a start value")
	}
}
