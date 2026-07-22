package dbc

import (
	"strings"
	"testing"

	"github.com/rveen/logb"
)

const sample = `VERSION "test"

NS_ :
	CM_
	BA_DEF_

BS_:

BU_: ECM ABS Tester

BO_ 256 EngineData: 8 ECM
 SG_ EngineSpeed : 0|16@1+ (0.25,0) [0|16383.75] "rpm"  ABS,Tester
 SG_ CoolantTemp : 16|8@1+ (1,-40) [-40|215] "degC"  Tester
 SG_ EngineRunning : 37|1@1+ (1,0) [0|1] ""  Tester

BO_ 512 VehicleStatus: 8 ABS
 SG_ VehicleSpeed : 7|16@0+ (0.01,0) [0|655.35] "km/h"  Tester
 SG_ Odometer : 23|24@0+ (0.1,0) [0|1677721.5] "km"  Tester
 SG_ Gear : 40|4@1+ (1,0) [0|15] ""  Tester

BO_ 2566844926 J1939Message: 8 Vector__XXX
 SG_ Torque : 0|8@1-  (1,-125) [-125|125] "%"  Vector__XXX

CM_ BO_ 256 "Engine data, 10 ms";
CM_ SG_ 256 EngineSpeed "Crankshaft speed";
VAL_ 512 Gear 0 "Neutral" 1 "First" 2 "Second" 15 "Reverse" ;
`

func parse(t *testing.T, s string) *File {
	t.Helper()
	d, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParse(t *testing.T) {
	d := parse(t, sample)

	if d.Version != "test" {
		t.Errorf("version %q", d.Version)
	}
	if got := strings.Join(d.Nodes, ","); got != "ECM,ABS,Tester" {
		t.Errorf("nodes %q", got)
	}
	if len(d.Messages) != 3 {
		t.Fatalf("%d messages, want 3", len(d.Messages))
	}

	m := d.Messages[0]
	if m.ID != 256 || m.Extended || m.Name != "EngineData" || m.Length != 8 || m.Sender != "ECM" {
		t.Errorf("message = %+v", m)
	}
	if m.Desc != "Engine data, 10 ms" {
		t.Errorf("comment %q", m.Desc)
	}
	if len(m.Signals) != 3 {
		t.Fatalf("%d signals, want 3", len(m.Signals))
	}
	s := m.Signals[0]
	if s.Name != "EngineSpeed" || s.Start != 0 || s.Length != 16 || s.BigEndian || s.Signed {
		t.Errorf("signal = %+v", s)
	}
	if s.Factor != 0.25 || s.Offset != 0 || s.Unit != "rpm" {
		t.Errorf("scaling = %v/%v %q", s.Factor, s.Offset, s.Unit)
	}
	if s.Desc != "Crankshaft speed" {
		t.Errorf("signal comment %q", s.Desc)
	}
	if got := strings.Join(s.Receivers, ","); got != "ABS,Tester" {
		t.Errorf("receivers %q", got)
	}
	if got := m.Signals[1].Offset; got != -40 {
		t.Errorf("CoolantTemp offset %v, want -40", got)
	}

	// An extended (29-bit) message: the DBC marks it with bit 31 of the id.
	j := d.Messages[2]
	if !j.Extended {
		t.Error("J1939Message should be an extended frame")
	}
	if j.ID != 2566844926-(1<<31) {
		t.Errorf("extended id %d", j.ID)
	}
	if !j.Signals[0].Signed {
		t.Error("Torque is declared signed")
	}

	// A value table.
	gear := d.Messages[1].Signals[2]
	if len(gear.Values) != 4 || gear.Values[15] != "Reverse" {
		t.Errorf("VAL_ table = %v", gear.Values)
	}
}

// TestMotorolaOffset is the claim CAN.md rests on, applied to the signals in
// internal/example: a DBC start bit becomes a Logb big-endian bit offset by
// 8*(start/8) + (7 - start%8), and nothing else changes.
func TestMotorolaOffset(t *testing.T) {
	d := parse(t, sample)
	v := d.Messages[1]

	for _, tc := range []struct {
		name string
		want uint32
	}{
		{"VehicleSpeed", 0}, // start bit 7 → 0
		{"Odometer", 16},    // start bit 23 → 16, unaligned and byte-crossing
		{"Gear", 40},        // Intel: unchanged
	} {
		var sg *Signal
		for _, s := range v.Signals {
			if s.Name == tc.name {
				sg = s
			}
		}
		if sg == nil {
			t.Fatalf("no signal %q", tc.name)
		}
		if got := sg.BitOffset(); got != tc.want {
			t.Errorf("%s: bit offset %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSchema checks the mapping onto Logb, including that the schema validates —
// which is where a bad guard or an overlong field would be caught.
func TestSchema(t *testing.T) {
	d := parse(t, sample)
	s, err := Schema(d.Messages[0], SchemaOptions{Namespace: "t", AxisExp: -9})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "EngineData" {
		t.Errorf("name %q", s.Name)
	}
	if s.AxisKind != logb.AxisTime || s.AxisMode != logb.AxisExplicit {
		t.Errorf("axis %v/%v", s.AxisKind, s.AxisMode)
	}
	if s.RecordBits != 64+64 {
		t.Errorf("record is %d bits, want 128 (axis + 8 payload bytes)", s.RecordBits)
	}
	if s.Meta["can.id"] != "0x100" {
		t.Errorf("can.id = %q", s.Meta["can.id"])
	}
	byName := map[string]logb.Field{}
	for _, f := range s.Fields {
		byName[f.Name] = f
	}
	if f := byName["EngineSpeed"]; f.BitOffset != 64 || f.BitWidth != 16 || f.Type != logb.TypeUint {
		t.Errorf("EngineSpeed = %+v", f)
	}
	if f := byName["EngineSpeed"]; f.Conv != (logb.Linear{A: 0, B: 0.25}) {
		t.Errorf("EngineSpeed conversion = %v", f.Conv)
	}
	// A one-bit unsigned signal is a flag, not a number to do arithmetic on.
	if f := byName["EngineRunning"]; f.Type != logb.TypeBool {
		t.Errorf("EngineRunning is %v, want bool", f.Type)
	}
	// The value table becomes §7's value-to-text.
	v, err := Schema(d.Messages[1], SchemaOptions{Namespace: "t", AxisExp: -9})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Fields {
		if f.Name != "Gear" {
			continue
		}
		conv, ok := f.Conv.(logb.ValueToText)
		if !ok {
			t.Fatalf("Gear conversion is %T, want ValueToText", f.Conv)
		}
		if got := conv.Apply(15); got != "Reverse" {
			t.Errorf("Gear 15 → %v, want Reverse", got)
		}
	}
}

// TestMultiplex is the case SPEC §6.2 was written for: signals that share bits
// and are told apart by a multiplexor. They must become guarded fields, and a
// reader must report the ones that are not selected as absent rather than
// decoding the bits that happen to sit there.
func TestMultiplex(t *testing.T) {
	const muxed = `BO_ 100 Mux: 8 ECU
 SG_ Selector M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ Temperature m1 : 8|16@1+ (0.1,-40) [0|0] "degC"  Tester
 SG_ Pressure m2 : 8|16@1+ (0.5,0) [0|0] "kPa"  Tester
`
	d := parse(t, muxed)
	s, err := Schema(d.Messages[0], SchemaOptions{Namespace: "t", AxisExp: -9})
	if err != nil {
		t.Fatal(err)
	}

	var sel, temp, pres int = -1, -1, -1
	for i, f := range s.Fields {
		switch f.Name {
		case "Selector":
			sel = i
		case "Temperature":
			temp = i
		case "Pressure":
			pres = i
		}
	}
	if sel < 0 || temp < 0 || pres < 0 {
		t.Fatalf("fields: %v", s.Fields)
	}
	// Both variants sit on the same bits — that is the point.
	if s.Fields[temp].BitOffset != s.Fields[pres].BitOffset {
		t.Error("the multiplexed signals should overlap")
	}
	for _, i := range []int{temp, pres} {
		f := s.Fields[i]
		if !f.Guarded || int(f.GuardField) != sel {
			t.Errorf("%s is not guarded on the multiplexor: %+v", f.Name, f)
		}
	}
	if s.Fields[temp].GuardValue != 1 || s.Fields[pres].GuardValue != 2 {
		t.Errorf("guard values %d/%d, want 1/2", s.Fields[temp].GuardValue, s.Fields[pres].GuardValue)
	}
}

// TestExtendedMuxRefused checks that a database Logb cannot express is reported
// and left out, not decoded anyway. Guards do not chain (§6.2), so a signal
// multiplexed two levels deep would otherwise be read out of frames that do not
// contain it — a number that is wrong rather than one that is missing.
func TestExtendedMuxRefused(t *testing.T) {
	const nested = `BO_ 100 Nested: 8 ECU
 SG_ Service M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ PID m1M : 8|8@1+ (1,0) [0|255] ""  Tester
 SG_ Value m3 : 16|16@1+ (1,0) [0|0] ""  Tester
`
	d := parse(t, nested)
	_, err := Schema(d.Messages[0], SchemaOptions{Namespace: "t", AxisExp: -9})
	if err == nil {
		t.Fatal("a two-level multiplexed message should be refused")
	}
	if !strings.Contains(err.Error(), "chain") {
		t.Errorf("error should say why: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse(strings.NewReader("this is not a database\n")); err == nil {
		t.Error("want an error for a file with no messages")
	}
	if _, err := Parse(strings.NewReader("BO_ 100 M: 8 X\n SG_ Broken : bad\n")); err == nil {
		t.Error("want an error for a malformed signal")
	}
}

// TestMultiLine checks a comment split across lines, which real databases
// contain and a line-at-a-time parser gets wrong.
func TestMultiLine(t *testing.T) {
	d := parse(t, "BO_ 100 M: 8 X\n SG_ S : 0|8@1+ (1,0) [0|0] \"\"  Y\n"+
		"CM_ SG_ 100 S \"first line\nsecond line\";\n")
	if got := d.Messages[0].Signals[0].Desc; got != "first line\nsecond line" {
		t.Errorf("comment = %q", got)
	}
}
