// Package example builds the worked Logb file that ships in testdata: a short
// CAN recording with invented messages.
//
// It lives here, rather than in cmd/logbgen, because two callers need it and one
// of them cannot import a main package: cmd/logbgen writes the file, and the
// golden test in package logb regenerates it to check the bytes have not moved.
//
// # Determinism
//
// Generate must produce identical bytes on every run, or the golden test is
// noise. Everything that would normally vary is pinned: the UUIDs are derived by
// name rather than randomly, the epoch is a constant, and the signal values come
// from a seeded PRNG. Nothing here reads the clock.
//
// # What it exercises
//
// The file is a demonstration, so it deliberately covers the parts of the spec
// that are easy to get wrong rather than the parts that are easy to write:
//
//   - can0.raw carries the wire bytes in a fixed bytes field (§6.2), which is
//     what a logger with no DBC actually records.
//   - EngineData and VehicleStatus overlay signals on those same payloads, mixing
//     Intel and Motorola in one frame — which §6.3 says real CAN does, and which
//     is the whole reason §6.2's bit numbering had to be settled. Odometer is
//     unaligned big-endian and crosses byte boundaries, the case that has no
//     defined meaning in MDF4.
//   - events uses a variable-length string, the only §6.4 tail in the file, and
//     is §6.7's event convention as written: an explicit axis, because events
//     are sporadic and an implicit one would claim they were periodic.
//   - Multiple segments restate every schema, so the file can be cut anywhere and
//     still decode (rule 3).
//   - One segment is written transposed and deflated, so both are exercised.
package example

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

// epoch is the recording's start: 2026-03-14T09:26:53Z, in nanoseconds. A
// constant, because Generate may not read the clock.
const epoch int64 = 1_773_480_413_000_000_000

// CAN ids of the invented messages.
const (
	idEngineData    = 0x100
	idVehicleStatus = 0x200
)

func uid(name string) [16]byte { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)) }

// The invented database. Carried in an ATTACH frame as text and never parsed by
// anything here — a reader that wants signals reads the schemas, which is the
// point: the bit layout is in the file, not in a sidecar the reader must go and
// find a parser for (rule 4).
const dbc = `VERSION "logb-example-1"

BO_ 256 EngineData: 8 ECM
 SG_ EngineSpeed : 0|16@1+ (0.25,0) [0|16383.75] "rpm" DASH
 SG_ CoolantTemp : 16|8@1+ (1,-40) [-40|215] "degC" DASH
 SG_ ThrottlePos : 24|8@1+ (0.4,0) [0|102] "%" DASH
 SG_ EngineRunning : 37|1@1+ (1,0) [0|1] "" DASH

BO_ 512 VehicleStatus: 8 ABS
 SG_ VehicleSpeed : 7|16@0+ (0.01,0) [0|655.35] "km/h" DASH
 SG_ Odometer : 23|24@0+ (0.1,0) [0|1677721.5] "km" DASH
 SG_ Gear : 39|4@1+ (1,0) [0|15] "" DASH
 SG_ Brake : 43|1@1+ (1,0) [0|1] "" DASH
`

// rawSchema is the wire: what a logger with no database writes. The payload is a
// fixed 8-byte blob, not a variable one — §6.4 is explicit that bus payloads
// belong in the fixed portion, because a tail costs the ability to seek.
func rawSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uid("logb/example/can0.raw"),
		Name:       "can0.raw",
		RecordBits: 136, // can_id(32) + dlc(8) + payload(64) + t_us(32)
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1000), // the field counts microseconds
		AxisField:  3,
		Fields: []logb.Field{
			{Name: "can_id", BitOffset: 0, BitWidth: 29, Type: logb.TypeUint,
				Desc: "CAN arbitration id"},
			{Name: "dlc", BitOffset: 32, BitWidth: 8, Type: logb.TypeUint,
				Desc: "data length code"},
			{Name: "payload", BitOffset: 40, BitWidth: 64, Type: logb.TypeBytes,
				Desc: "the eight wire bytes, exactly as the bus produced them",
				// Field-level metadata: how the payload is encoded, which is
				// neither a unit nor prose. The named database ships as an
				// attachment, as an artefact — never as something a reader
				// must parse to decode the file.
				Meta: map[string]string{
					"payload.encoding": "can.raw",
					"payload.schema":   "example.dbc",
				}},
			{Name: "t_us", BitOffset: 104, BitWidth: 32, Type: logb.TypeUint, Unit: "us",
				Desc: "axis: microseconds since the segment's axis_base"},
		},
		Meta: map[string]string{"bus": "can0", "bitrate": "500000"},
	}
}

// engineSchema overlays signals on a payload, all little-endian. EngineRunning
// is the 1-bit flag at bit 37 that §6.2 names as the reason the bit-level model
// exists at all.
func engineSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uid("logb/example/EngineData"),
		Name:       "EngineData",
		RecordBits: 64,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(10_000_000), // 10 ms
		Fields: []logb.Field{
			{Name: "EngineSpeed", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint,
				Unit: "rpm", Conv: logb.Linear{A: 0, B: 0.25}},
			{Name: "CoolantTemp", BitOffset: 16, BitWidth: 8, Type: logb.TypeUint,
				Unit: "degC", Conv: logb.Linear{A: -40, B: 1}},
			{Name: "ThrottlePos", BitOffset: 24, BitWidth: 8, Type: logb.TypeUint,
				Unit: "%", Conv: logb.Linear{A: 0, B: 0.4}},
			{Name: "EngineRunning", BitOffset: 37, BitWidth: 1, Type: logb.TypeBool,
				Desc: "the bit-37 flag of SPEC.md §6.2"},
		},
		Meta: map[string]string{"can.id": "0x100", "can.sender": "ECM"},
	}
}

// vehicleSchema is the interesting one: Motorola signals beside Intel ones in a
// single frame. Odometer is unaligned and crosses byte boundaries — the case
// that is undefined in MDF4 and that §6.2's conformance vectors pin down.
//
// The Motorola bit offsets are what a DBC importer would compute from the DBC
// above: bit_offset = 8*(start/8) + (7 - start%8). VehicleSpeed's start bit 7
// becomes 0; Odometer's start bit 23 becomes 16.
func vehicleSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uid("logb/example/VehicleStatus"),
		Name:       "VehicleStatus",
		RecordBits: 64,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(20_000_000), // 20 ms
		Fields: []logb.Field{
			{Name: "VehicleSpeed", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint,
				BigEndian: true, Unit: "km/h", Conv: logb.Linear{A: 0, B: 0.01},
				Desc: "Motorola, byte-aligned (DBC start bit 7)"},
			{Name: "Odometer", BitOffset: 16, BitWidth: 24, Type: logb.TypeUint,
				BigEndian: true, Unit: "km", Conv: logb.Linear{A: 0, B: 0.1},
				Desc: "Motorola, crosses byte boundaries (DBC start bit 23)"},
			{Name: "Gear", BitOffset: 40, BitWidth: 4, Type: logb.TypeUint,
				Conv: logb.ValueToText{
					Keys:    []float64{0, 1, 2, 3, 4, 5, 6, 14, 15},
					Texts:   []string{"P", "1", "2", "3", "4", "5", "6", "R", "N"},
					Default: "?",
				}},
			{Name: "Brake", BitOffset: 44, BitWidth: 1, Type: logb.TypeBool},
		},
		Meta: map[string]string{"can.id": "0x200", "can.sender": "ABS"},
	}
}

// eventSchema is the file's only variable-length field, and the only thing here
// with a tail. §6.4 says tails exist for log strings and blobs; this is that.
//
// It is also §6.7's event convention as written: severity under value_to_text,
// message in the tail, and an explicit axis. The axis has to be explicit —
// events are sporadic, and an implicit one would space them evenly and claim
// they were periodic.
func eventSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uid("logb/example/events"),
		Name:       "events",
		RecordBits: 40, // severity + t_us; the message contributes no fixed bits
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisExplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisScale:  logb.TickVal(1000), // the field counts microseconds
		AxisField:  1,
		Fields: []logb.Field{
			{Name: "severity", BitOffset: 0, BitWidth: 8, Type: logb.TypeUint,
				Conv: logb.ValueToText{
					Keys:    []float64{0, 1, 2},
					Texts:   []string{"info", "warning", "error"},
					Default: "?",
				}},
			{Name: "t_us", BitOffset: 8, BitWidth: 32, Type: logb.TypeUint, Unit: "us",
				Desc: "axis: microseconds since the segment's axis_base"},
			{Name: "message", Type: logb.TypeString, Variable: true,
				Desc: "§6.4 tail: bit_width is 0, the bytes are in the tail"},
		},
	}
}

// encodeEngine packs an EngineData payload, little-endian throughout.
//
// Values are rounded, not truncated, into their raw counts. Truncation is the
// classic quantisation bug: 30/0.01 is 2999.9999999999995 in float64, which
// stores as 2999 and reads back as 29.99 — a signal that is wrong by a count in
// a file that looks perfectly well-formed.
func encodeEngine(rpm float64, coolantC int, throttlePct float64, running bool) []byte {
	rec := make([]byte, 8)
	binary.LittleEndian.PutUint16(rec[0:], uint16(math.Round(rpm/0.25)))
	rec[2] = byte(coolantC + 40)
	rec[3] = byte(math.Round(throttlePct / 0.4))
	if running {
		rec[4] |= 1 << 5 // bit 37 = byte 4, bit 5
	}
	return rec
}

// encodeVehicle packs a VehicleStatus payload. The two Motorola signals are
// written MSB-first from their bit offsets, which is the same rule the reader
// applies in reverse — if this and extractBits ever disagree, the round-trip
// test says so.
func encodeVehicle(kmh float64, odoKm float64, gear int, brake bool) []byte {
	rec := make([]byte, 8)
	putBE(rec, 0, 16, uint64(math.Round(kmh/0.01)))
	putBE(rec, 16, 24, uint64(math.Round(odoKm/0.1)))
	rec[5] |= byte(gear) & 0x0f
	if brake {
		rec[5] |= 1 << 4
	}
	return rec
}

// putBE writes width bits of v at a big-endian bit offset: bit n is byte n/8,
// bit n%8 counting from the MSB (SPEC.md §6.2).
func putBE(rec []byte, off, width uint32, v uint64) {
	for i := uint32(0); i < width; i++ {
		bit := (v >> (width - 1 - i)) & 1
		p := off + i
		if bit == 1 {
			rec[p/8] |= 1 << (7 - p%8)
		}
	}
}

// Generate writes the example file.
func Generate(w io.Writer) error {
	vw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}

	raw, engine, vehicle, events := rawSchema(), engineSchema(), vehicleSchema(), eventSchema()
	for _, s := range []*logb.Schema{raw, engine, vehicle, events} {
		if err := vw.AddStream(s); err != nil {
			return fmt.Errorf("stream %q: %w", s.Name, err)
		}
	}

	if err := vw.BeginSegment(epoch); err != nil {
		return err
	}
	// A slice, not a map: Generate must emit the same bytes every run, and Go
	// randomises map iteration.
	for _, m := range [][2]string{
		{"time.base", "unix"},
		{"title", "Logb worked example — invented CAN traffic"},
		{"device", "logbgen"},
		{"bus.database", "example.dbc"},
	} {
		if err := vw.WriteMeta(m[0], m[1]); err != nil {
			return err
		}
	}
	if err := vw.WriteAttach("example.dbc", []byte(dbc)); err != nil {
		return err
	}

	rng := rand.New(rand.NewSource(20260314))

	// Three segments of one second each. A segment boundary restates every
	// schema, which is what lets a reader enter the file anywhere (rule 3), and
	// the third is written transposed and deflated to exercise both.
	for seg := 0; seg < 3; seg++ {
		if seg > 0 {
			vw.Codec, vw.Filter = logb.CodecNone, logb.FilterNone
			if seg == 2 {
				vw.Codec, vw.Filter = logb.CodecDeflate, logb.FilterTranspose
			}
			if err := vw.BeginSegment(epoch + int64(seg)*1e9); err != nil {
				return err
			}
		}
		segBase := int64(seg) * 1e9

		if err := writeSegment(vw, seg, segBase, rng, raw, engine, vehicle, events); err != nil {
			return err
		}
	}

	// The logger acquired a GPS fix late and can now date the records it already
	// wrote. Emitting this after them is what rule 1 permits and what MDF4 cannot
	// express (§5.2).
	if err := vw.WriteMeta("time.anchor", fmt.Sprintf("%d:%d", epoch+2_500_000_000, epoch+2_500_000_123)); err != nil {
		return err
	}
	return vw.Close()
}

func writeSegment(vw *logb.Writer, seg int, segBase int64, rng *rand.Rand,
	raw, engine, vehicle, events *logb.Schema) error {

	const (
		enginePerSec  = 100 // 10 ms
		vehiclePerSec = 50  // 20 ms
	)

	// EngineData: a plausible drive — revs rising and falling, coolant warming.
	var eng []byte
	for i := 0; i < enginePerSec; i++ {
		t := float64(seg) + float64(i)/enginePerSec
		rpm := 800 + 2200*(0.5+0.5*tri(t))
		coolant := 60 + int(t*3) + rng.Intn(2)
		throttle := 4 + 60*(0.5+0.5*tri(t))
		eng = append(eng, encodeEngine(rpm, coolant, throttle, true)...)
	}
	if err := vw.WriteData(engine, logb.TickVal(segBase), 0, enginePerSec, eng); err != nil {
		return err
	}

	// VehicleStatus: speed tracking the revs, odometer only ever climbing.
	var veh []byte
	for i := 0; i < vehiclePerSec; i++ {
		t := float64(seg) + float64(i)/vehiclePerSec
		kmh := 30 + 25*(0.5+0.5*tri(t))
		odo := 40312.6 + t*0.02
		gear := 3 + i%2
		veh = append(veh, encodeVehicle(kmh, odo, gear, i%25 == 0)...)
	}
	if err := vw.WriteData(vehicle, logb.TickVal(segBase), 0, vehiclePerSec, veh); err != nil {
		return err
	}

	// can0.raw: the same traffic as it appeared on the wire, undecoded. A real
	// logger writes only this, because it has never seen the database.
	var rw []byte
	n := uint32(0)
	for i := 0; i < enginePerSec; i++ {
		t := float64(seg) + float64(i)/enginePerSec
		us := uint32(i * 10_000)
		rpm := 800 + 2200*(0.5+0.5*tri(t))
		coolant := 60 + int(t*3)
		throttle := 4 + 60*(0.5+0.5*tri(t))
		rw = append(rw, encodeRaw(idEngineData, encodeEngine(rpm, coolant, throttle, true), us)...)
		n++

		if i%2 == 0 {
			kmh := 30 + 25*(0.5+0.5*tri(t))
			odo := 40312.6 + t*0.02
			rw = append(rw, encodeRaw(idVehicleStatus, encodeVehicle(kmh, odo, 3+i%2, i%50 == 0), us+120)...)
			n++
		}
	}
	if err := vw.WriteData(raw, logb.TickVal(segBase), 0, n, rw); err != nil {
		return err
	}

	// events: two per segment, each with a variable-length message in the tail.
	// The offsets are deliberately irregular — that is the whole reason §6.7
	// specifies an explicit axis, and evenly spaced ones would hide a reader
	// that ignored the axis field and counted records instead.
	type event struct {
		severity byte
		us       uint32
		text     string
	}
	msgs := []event{
		{0, 0, fmt.Sprintf("segment %d started", seg)},
		{1, 137_500, fmt.Sprintf("coolant rising: %d degC", 60+seg*3)},
	}
	if seg == 2 {
		msgs = append(msgs, event{2, 402_317, "DTC P0301 set: cylinder 1 misfire detected"})
	}
	fixed := make([]byte, 0, len(msgs)*5)
	tail := make([]byte, 0, 128)
	for _, m := range msgs {
		var rec [5]byte
		rec[0] = m.severity
		binary.LittleEndian.PutUint32(rec[1:], m.us)
		fixed = append(fixed, rec[:]...)
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(m.text)))
		tail = append(tail, l[:]...)
		tail = append(tail, m.text...)
	}
	// §6.4 and §8: every fixed record first, then the whole tail region.
	return vw.WriteData(events, logb.TickVal(segBase), 0, uint32(len(msgs)), append(fixed, tail...))
}

// encodeRaw packs one wire frame: id, dlc, the eight payload bytes, and the
// microsecond axis field.
func encodeRaw(id uint32, payload []byte, us uint32) []byte {
	rec := make([]byte, 17)
	binary.LittleEndian.PutUint32(rec[0:], id)
	rec[4] = 8
	copy(rec[5:13], payload)
	binary.LittleEndian.PutUint32(rec[13:], us)
	return rec
}

// tri is a triangle wave on t, period 1. Deliberately not math.Sin: the values
// this drives are meant to be read in a dump, and a linear ramp is obvious at a
// glance where a sinusoid is just numbers. It is also exactly reproducible with
// no library floating-point behaviour in the way.
func tri(t float64) float64 {
	x := t - float64(int(t))
	if x < 0.5 {
		return 4*x - 1
	}
	return 3 - 4*x
}
