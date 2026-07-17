package logb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/google/uuid"
)

func uid(s string) [16]byte {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(s))
}

// loggerSchema is the common case: a periodic sampler, implicit time axis,
// nanosecond ticks, bit-packed fields.
func loggerSchema() *Schema {
	return &Schema{
		UUID:       uid("logger/engine"),
		Name:       "engine",
		RecordBits: 32,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   TickVal(1_000_000), // 1 ms
		Fields: []Field{
			{Name: "rpm", BitOffset: 0, BitWidth: 16, Type: TypeUint, Unit: "1/min",
				Conv: Linear{A: 0, B: 0.25}},
			{Name: "temp", BitOffset: 16, BitWidth: 12, Type: TypeSint, Unit: "degC",
				Conv: Linear{A: -40, B: 0.1}},
			{Name: "fault", BitOffset: 28, BitWidth: 1, Type: TypeBool},
		},
	}
}

func encodeLoggerRec(rpm uint16, temp int16, fault bool) []byte {
	var v uint32
	v |= uint32(rpm)
	v |= uint32(uint16(temp)&0xfff) << 16
	if fault {
		v |= 1 << 28
	}
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func TestRoundTrip(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	s := loggerSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(1_700_000_000_000_000_000); err != nil {
		t.Fatal(err)
	}

	var recs []byte
	for i := 0; i < 100; i++ {
		recs = append(recs, encodeLoggerRec(uint16(i*40), int16(i-50), i%7 == 0)...)
	}
	if err := w.WriteData(s, TickVal(0), 0, 100, recs); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Count != 100 {
		t.Fatalf("count = %d, want 100", b.Count)
	}
	if b.Schema.Name != "engine" || b.Schema.UUID != s.UUID {
		t.Fatalf("schema identity lost: %q %x", b.Schema.Name, b.Schema.UUID)
	}

	// Values round-trip through their conversions.
	for i := 0; i < 100; i++ {
		v, err := b.Value(i, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := float64(i*40) * 0.25
		if v.(float64) != want {
			t.Fatalf("rpm[%d] = %v, want %v", i, v, want)
		}

		v, err = b.Value(i, 1)
		if err != nil {
			t.Fatal(err)
		}
		wantT := -40 + float64(i-50)*0.1
		if math.Abs(v.(float64)-wantT) > 1e-9 {
			t.Fatalf("temp[%d] = %v, want %v", i, v, wantT)
		}

		v, err = b.Value(i, 2)
		if err != nil {
			t.Fatal(err)
		}
		if v.(bool) != (i%7 == 0) {
			t.Fatalf("fault[%d] = %v", i, v)
		}
	}

	// Implicit axis: 1 ms apart, exactly.
	a, _ := b.Axis(50)
	if a.Ticks() != 50_000_000 {
		t.Fatalf("axis[50] = %d ticks, want 50000000", a.Ticks())
	}

	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if r.Truncated {
		t.Fatal("clean file reported truncated")
	}
}

// TestTruncation is the crash-safety rule: a file cut at an arbitrary byte is a
// valid file containing every record up to the last intact frame.
func TestTruncation(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := loggerSchema()
	w.AddStream(s)
	w.BeginSegment(0)

	rec := encodeLoggerRec(1000, 20, false)
	for i := 0; i < 10; i++ {
		if err := w.WriteData(s, TickVal(int64(i)*1e6), 0, 1, rec); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	full := out.Bytes()

	// Cut at every byte. Every prefix must either parse cleanly or stop at
	// damage — never error, never panic, never return a bad batch.
	for cut := 16; cut < len(full); cut++ {
		r, err := NewReader(bytes.NewReader(full[:cut]))
		if err != nil {
			continue // header itself incomplete
		}
		n := 0
		for {
			b, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("cut=%d: unexpected error %v", cut, err)
			}
			// Any batch we get back must be intact.
			v, err := b.Value(0, 0)
			if err != nil {
				t.Fatalf("cut=%d: batch %d damaged: %v", cut, n, err)
			}
			if v.(float64) != 250 {
				t.Fatalf("cut=%d: batch %d value %v, want 250", cut, n, v)
			}
			n++
		}
	}
}

// TestConcat is §6.6: two files concatenate by appending the second file's bytes
// minus its 16-byte header. No rewriting, no id remap.
func TestConcat(t *testing.T) {
	build := func(name string, rpm uint16) []byte {
		var out bytes.Buffer
		w, _ := NewWriter(&out)
		s := loggerSchema()
		s.UUID = uid(name)
		s.Name = name
		w.AddStream(s)
		w.BeginSegment(0)
		w.WriteData(s, TickVal(0), 0, 1, encodeLoggerRec(rpm, 0, false))
		w.Close()
		return out.Bytes()
	}

	// Both files use stream_id 1 internally for different streams. Under a
	// file-scoped id this would be a collision requiring a rewrite.
	a := build("box-left", 400)
	b := build("box-right", 800)

	joined := append(append([]byte{}, a...), b[16:]...)

	r, err := NewReader(bytes.NewReader(joined))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var vals []float64
	for {
		batch, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, batch.Schema.Name)
		v, _ := batch.Value(0, 0)
		vals = append(vals, v.(float64))
	}

	if len(names) != 2 {
		t.Fatalf("got %d batches, want 2: %v", len(names), names)
	}
	if names[0] != "box-left" || names[1] != "box-right" {
		t.Fatalf("streams did not rebind across concat: %v", names)
	}
	if vals[0] != 100 || vals[1] != 200 {
		t.Fatalf("values = %v, want [100 200]", vals)
	}
}

// TestResync is rule 3: a reader handed the middle of a file, with no access to
// the start, decodes with full schema.
func TestResync(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := loggerSchema()
	w.AddStream(s)
	for seg := 0; seg < 3; seg++ {
		w.BeginSegment(0)
		w.WriteData(s, TickVal(int64(seg)*1e9), 0, 1, encodeLoggerRec(uint16(seg*100), 0, false))
	}
	w.Close()
	full := out.Bytes()

	// Throw away the header and the first segment entirely.
	cutAt := len(full) / 2
	r, at, err := Resync(full[cutAt:])
	if err != nil {
		t.Fatalf("resync failed on a cut file: %v", err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatalf("no batch after resync: %v", err)
	}
	if b.Schema.Name != "engine" {
		t.Fatalf("schema not recovered after resync: %q", b.Schema.Name)
	}
	if len(b.Schema.Fields) != 3 {
		t.Fatalf("fields not recovered: %d", len(b.Schema.Fields))
	}
	t.Logf("resynced at offset %d of a %d-byte tail", at, len(full)-cutAt)
}

// TestFemtosecond is the ngspice case: an RF transient at femtosecond ticks
// stays exact, and time.Duration is correctly refused.
func TestFemtosecond(t *testing.T) {
	s := &Schema{
		UUID:       uid("sim/tran"),
		Name:       "tran",
		RecordBits: 64,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -15, // femtoseconds
		AxisUnit:   "s",
		AxisStep:   TickVal(10), // 10 fs timestep
		Fields: []Field{
			{Name: "V(out)", BitOffset: 0, BitWidth: 64, Type: TypeFloat, Unit: "V"},
		},
	}

	// A billion steps of 10 fs is 10 µs of simulation, exact in int64 ticks.
	a := s.AxisAt(TickVal(0), 1_000_000_000, 0)
	if a.Ticks() != 10_000_000_000 {
		t.Fatalf("axis = %d ticks, want 10000000000", a.Ticks())
	}
	if got := a.Seconds(-15); math.Abs(got-10e-6) > 1e-18 {
		t.Fatalf("seconds = %g, want 1e-5", got)
	}

	// The trap: time.Duration cannot represent a femtosecond tick at all.
	if _, err := a.Duration(-15); err == nil {
		t.Fatal("Duration accepted a femtosecond tick; it is int64 nanoseconds")
	}
	// Nanosecond ticks are fine.
	d, err := TickVal(1500).Duration(-9)
	if err != nil {
		t.Fatal(err)
	}
	if d.Nanoseconds() != 1500 {
		t.Fatalf("duration = %v", d)
	}
	// Microsecond ticks scale up.
	d, err = TickVal(3).Duration(-6)
	if err != nil {
		t.Fatal(err)
	}
	if d.Nanoseconds() != 3000 {
		t.Fatalf("duration = %v, want 3µs", d)
	}
}

// TestACAnalysis is the LTspice `Flags: complex` case: an AC sweep over a
// frequency axis with complex values.
func TestACAnalysis(t *testing.T) {
	s := &Schema{
		UUID:       uid("sim/ac"),
		Name:       "ac",
		RecordBits: 64 + 128,
		AxisKind:   AxisFrequency,
		AxisMode:   AxisExplicit,
		AxisUnit:   "Hz",
		AxisScale:  FloatVal(1),
		AxisField:  0,
		Fields: []Field{
			{Name: "frequency", BitOffset: 0, BitWidth: 64, Type: TypeFloat, Unit: "Hz"},
			{Name: "V(out)", BitOffset: 64, BitWidth: 128, Type: TypeComplex, Unit: "V"},
		},
	}

	var out bytes.Buffer
	w, _ := NewWriter(&out)
	w.AddStream(s)
	w.BeginSegment(0)

	freqs := []float64{10, 100, 1000, 10000}
	vals := []complex128{1 + 0i, 0.9 - 0.1i, 0.5 - 0.5i, 0.1 - 0.9i}
	var recs []byte
	for i := range freqs {
		rec := make([]byte, 24)
		binary.LittleEndian.PutUint64(rec[0:], math.Float64bits(freqs[i]))
		binary.LittleEndian.PutUint64(rec[8:], math.Float64bits(real(vals[i])))
		binary.LittleEndian.PutUint64(rec[16:], math.Float64bits(imag(vals[i])))
		recs = append(recs, rec...)
	}
	// A frequency axis carries its base as an f64, not as ticks.
	w.WriteData(s, FloatVal(0), 0, uint32(len(freqs)), recs)
	w.Close()

	r, _ := NewReader(bytes.NewReader(out.Bytes()))
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	for i := range freqs {
		v, err := b.Value(i, 1)
		if err != nil {
			t.Fatal(err)
		}
		if v.(complex128) != vals[i] {
			t.Fatalf("V(out)[%d] = %v, want %v", i, v, vals[i])
		}
		a, err := b.Axis(i)
		if err != nil {
			t.Fatal(err)
		}
		if a.Float() != freqs[i] {
			t.Fatalf("freq[%d] = %v, want %v", i, a.Float(), freqs[i])
		}
	}
}

// TestSteppedRuns is §6.5: the boundaries LTspice makes you guess at by
// checking whether the time axis returned to zero.
func TestSteppedRuns(t *testing.T) {
	s := &Schema{
		UUID:       uid("sim/dc"),
		Name:       "dc",
		RecordBits: 64,
		AxisKind:   AxisOther, // a swept source, not time
		AxisMode:   AxisImplicit,
		AxisUnit:   "V",
		AxisStep:   FloatVal(1),
		Fields: []Field{
			{Name: "I(R1)", BitOffset: 0, BitWidth: 64, Type: TypeFloat, Unit: "A"},
		},
	}

	var out bytes.Buffer
	w, _ := NewWriter(&out)
	w.AddStream(s)
	for i := 0; i < 3; i++ {
		w.AddRun(&Run{ID: uint32(i), Index: uint32(i),
			Params: map[string]string{"R1": []string{"1k", "2k", "3k"}[i]}})
	}
	w.BeginSegment(0)

	// A DC sweep from -5 V to +5 V: the axis legitimately passes through zero in
	// every run, which is exactly what defeats the `time == 0` heuristic.
	for run := 0; run < 3; run++ {
		var recs []byte
		for v := -5; v <= 5; v++ {
			rec := make([]byte, 8)
			binary.LittleEndian.PutUint64(rec, math.Float64bits(float64(v)/float64(run+1)))
			recs = append(recs, rec...)
		}
		w.WriteData(s, FloatVal(-5), uint32(run), 11, recs)
	}
	w.Close()

	r, _ := NewReader(bytes.NewReader(out.Bytes()))
	seen := map[uint32]string{}
	for {
		b, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Run == nil {
			t.Fatalf("run %d not bound", b.RunID)
		}
		seen[b.RunID] = b.Run.Params["R1"]

		// Axis crosses zero mid-run without ambiguity.
		a, _ := b.Axis(5)
		if a.Float() != 0 {
			t.Fatalf("axis[5] = %v, want 0", a.Float())
		}
	}
	if len(seen) != 3 || seen[0] != "1k" || seen[2] != "3k" {
		t.Fatalf("runs = %v", seen)
	}
}

// TestLateClock is §5.2: a logger that boots without an RTC dates its records
// retroactively, without seeking back.
func TestLateClock(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := loggerSchema()
	w.AddStream(s)
	w.BeginSegment(0)
	w.WriteMeta("time.base", "monotonic")
	w.WriteData(s, TickVal(0), 0, 1, encodeLoggerRec(100, 0, false))
	// GPS fixes only now — after the records it dates.
	w.WriteMeta("time.anchor", "5000000:1700000000000000000")
	w.Close()

	r, _ := NewReader(bytes.NewReader(out.Bytes()))
	for {
		if _, err := r.Next(); err == io.EOF {
			break
		}
	}
	if len(r.Meta) != 2 || r.Meta[1].Key != "time.anchor" {
		t.Fatalf("meta = %v", r.Meta)
	}
}

func TestTransposeAndDeflate(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	w.Codec = CodecDeflate
	w.Filter = FilterTranspose
	s := loggerSchema()
	w.AddStream(s)
	w.BeginSegment(0)

	var recs []byte
	for i := 0; i < 500; i++ {
		recs = append(recs, encodeLoggerRec(uint16(3000+i%3), int16(20), false)...)
	}
	if err := w.WriteData(s, TickVal(0), 0, 500, recs); err != nil {
		t.Fatal(err)
	}
	w.Close()

	r, _ := NewReader(bytes.NewReader(out.Bytes()))
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Data, recs) {
		t.Fatal("transpose+deflate did not round-trip")
	}
	t.Logf("2000 raw bytes -> %d on disk", out.Len())
}

func TestBitExtraction(t *testing.T) {
	// A 1-bit flag at bit 37 of a 64-bit payload: the reason the bit-level model
	// exists, and why an Arrow substrate could not have worked.
	rec := make([]byte, 8)
	binary.LittleEndian.PutUint64(rec, 1<<37)
	v, err := extractBits(rec, 37, 1, false)
	if err != nil || v != 1 {
		t.Fatalf("bit 37 = %d, %v", v, err)
	}

	// A 12-bit field straddling a byte boundary.
	binary.LittleEndian.PutUint64(rec, 0xABC<<4)
	v, err = extractBits(rec, 4, 12, false)
	if err != nil || v != 0xABC {
		t.Fatalf("straddling field = %x, %v", v, err)
	}

	// Byte-aligned big-endian: a Motorola signal.
	be := []byte{0x12, 0x34}
	v, err = extractBits(be, 0, 16, true)
	if err != nil || v != 0x1234 {
		t.Fatalf("BE = %x, %v", v, err)
	}

	// Unaligned big-endian is now defined rather than refused, and it crosses the
	// byte boundary without a special case: 0x12,0x34 is 00010010 00110100, and
	// twelve bits running up from MSB0 offset 3 are 100100011010 = 0x91a.
	if v, err := extractBits(be, 3, 12, true); err != nil || v != 0x91A {
		t.Fatalf("unaligned BE = %#x, %v; want 0x91a", v, err)
	}

	// Sign extension.
	if got := signExtend(0xFFF, 12); got != -1 {
		t.Fatalf("signExtend = %d, want -1", got)
	}
	if got := signExtend(0x7FF, 12); got != 2047 {
		t.Fatalf("signExtend = %d, want 2047", got)
	}
}

func TestConversions(t *testing.T) {
	tab := Table{Keys: []float64{0, 10, 20}, Vals: []float64{0, 100, 200}, Interp: true}
	if got := tab.Apply(5).(float64); got != 50 {
		t.Fatalf("interp = %v, want 50", got)
	}
	tab.Interp = false
	if got := tab.Apply(15).(float64); got != 100 {
		t.Fatalf("step = %v, want 100", got)
	}
	enum := ValueToText{Keys: []float64{0, 1}, Texts: []string{"off", "on"}, Default: "?"}
	if got := enum.Apply(1).(string); got != "on" {
		t.Fatalf("enum = %v", got)
	}
	if got := enum.Apply(9).(string); got != "?" {
		t.Fatalf("enum default = %v", got)
	}
	rat := Rational{P: [6]float64{0, 2, 0, 0, 0, 1}} // 2x/1
	if got := rat.Apply(21).(float64); got != 42 {
		t.Fatalf("rational = %v", got)
	}
}

func TestZeroUUIDRejected(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := loggerSchema()
	s.UUID = [16]byte{}
	if err := w.AddStream(s); err == nil {
		t.Fatal("zero UUID accepted; identity must be the writer's explicit call")
	}
}

func TestNotAnOLFFile(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte("hello world 1234"))); err != ErrBadMagic {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

// TestLogAxis is §5.3: an AC decade sweep is uniform in log space, so it costs
// zero bytes per record. This is the same sweep TestACAnalysis writes as an
// explicit f64 per point, at 8 bytes per record less.
func TestLogAxis(t *testing.T) {
	const perDecade = 10
	s := &Schema{
		UUID:     uid("sim/ac-log"),
		Name:     "ac",
		AxisKind: AxisFrequency,
		AxisMode: AxisLog,
		AxisUnit: "Hz",
		AxisStep: FloatVal(math.Pow(10, 1.0/perDecade)),
		Fields: []Field{
			{Name: "V(out)", BitOffset: 0, BitWidth: 128, Type: TypeComplex, Unit: "V"},
		},
	}
	s.RecordBits = 128

	var out bytes.Buffer
	w, _ := NewWriter(&out)
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	w.BeginSegment(0)

	const n = 3 * perDecade // 10 Hz to 10 kHz
	var recs []byte
	for i := 0; i < n; i++ {
		rec := make([]byte, 16)
		binary.LittleEndian.PutUint64(rec[0:], math.Float64bits(float64(i)))
		binary.LittleEndian.PutUint64(rec[8:], 0)
		recs = append(recs, rec...)
	}
	if err := w.WriteData(s, FloatVal(10), 0, n, recs); err != nil {
		t.Fatal(err)
	}
	w.Close()

	r, _ := NewReader(bytes.NewReader(out.Bytes()))
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Schema.AxisMode != AxisLog {
		t.Fatalf("axis mode = %v, want AxisLog", b.Schema.AxisMode)
	}
	// The decade boundaries are the points that must land exactly where a reader
	// expects them: 10, 100, 1000.
	for d, want := range map[int]float64{0: 10, perDecade: 100, 2 * perDecade: 1000} {
		a, err := b.Axis(d)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(a.Float()-want) > want*1e-12 {
			t.Fatalf("axis[%d] = %v, want %v", d, a.Float(), want)
		}
	}
}

// TestLogAxisTimeRejected: time is an integer count of ticks and a log-spaced
// tick count is not one. The writer refuses rather than writing a file whose
// axis two readers would disagree about.
func TestLogAxisTimeRejected(t *testing.T) {
	s := loggerSchema()
	s.AxisMode = AxisLog
	s.AxisStep = FloatVal(2)
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	if err := w.AddStream(s); !errors.Is(err, ErrLogAxisTime) {
		t.Fatalf("want ErrLogAxisTime, got %v", err)
	}
}

// TestRunsMustBeContiguous is §6.5: a writer may not shuffle runs. Returning to
// a run the stream already left is caught at write time, because a file that
// does it is one every reader has to cope with.
func TestRunsMustBeContiguous(t *testing.T) {
	s := loggerSchema()
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	w.AddStream(s)
	w.AddRun(&Run{ID: 0, Index: 0, Params: map[string]string{"R1": "1k"}})
	w.AddRun(&Run{ID: 1, Index: 1, Params: map[string]string{"R1": "2k"}})
	w.BeginSegment(0)

	rec := encodeLoggerRec(100, 20, false)
	if err := w.WriteData(s, TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(0), 1, 1, rec); err != nil {
		t.Fatal(err)
	}
	// Run 1 may continue...
	if err := w.WriteData(s, TickVal(1_000_000), 1, 1, rec); err != nil {
		t.Fatal(err)
	}
	// ...but run 0 is over and must not come back.
	if err := w.WriteData(s, TickVal(1_000_000), 0, 1, rec); !errors.Is(err, ErrRunInterleaved) {
		t.Fatalf("want ErrRunInterleaved, got %v", err)
	}

	// A new segment rebinds run scope, so the same run may resume there.
	if err := w.BeginSegment(0); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(s, TickVal(2_000_000), 0, 1, rec); err != nil {
		t.Fatalf("run 0 must be allowed to resume in a new segment: %v", err)
	}
}

// TestUnknownAxisModeSkipsStream is §4.2 applied to a schema rather than a frame:
// a v0.1 reader handed a stream from a later version must skip that stream and
// keep decoding every other one, rather than reporting a silently wrong axis for
// its records. The bytes here are what a hypothetical v0.2 writer with a fourth
// axis mode would produce.
func TestUnknownAxisModeSkipsStream(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	known := loggerSchema()
	future := loggerSchema()
	future.UUID = uid("logger/future")
	future.Name = "future"
	if err := w.AddStream(known); err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(future); err != nil {
		t.Fatal(err)
	}
	w.BeginSegment(0)

	rec := encodeLoggerRec(4000, 25, false)
	if err := w.WriteData(known, TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteData(future, TickVal(0), 0, 1, rec); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Rewrite the future stream's axis_mode to 3 in its SCHEMA frame, which is
	// the one thing this version's writer will not do for us. The byte sits at a
	// fixed offset past the uuid and the length-prefixed name, and the frame's
	// CRC has to be recomputed over the edit.
	file := out.Bytes()
	if !patchAxisMode(t, file, "future", 3) {
		t.Fatal("could not find the future stream's schema frame")
	}

	r, err := NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	var batches int
	for {
		b, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		batches++
		if b.Schema.Name != "engine" {
			t.Fatalf("decoded a batch of stream %q, which this version cannot decode", b.Schema.Name)
		}
		// The known stream is unaffected by its neighbour.
		if v, err := b.Value(0, 0); err != nil || v.(float64) != 1000 {
			t.Fatalf("rpm = %v (%v), want 1000", v, err)
		}
	}
	if batches != 1 {
		t.Fatalf("decoded %d batches, want 1: the future stream's data must be skipped", batches)
	}
	if r.Truncated {
		t.Fatal("Truncated set: a stream from a later version is not damage")
	}
	if len(r.Unsupported) != 1 || !errors.Is(r.Unsupported[0], ErrUnknownAxisMode) {
		t.Fatalf("Unsupported = %v, want one ErrUnknownAxisMode: skipping must not be silent", r.Unsupported)
	}
}

// patchAxisMode finds the SCHEMA frame of the named stream, overwrites its
// axis_mode byte, and fixes the frame CRC.
func patchAxisMode(t *testing.T, file []byte, name string, mode byte) bool {
	t.Helper()
	for i := 16; i+12 <= len(file); {
		length := int(binary.LittleEndian.Uint32(file[i:]))
		if i+12+length > len(file) {
			return false
		}
		if FrameType(file[i+4]) == FrameSchema {
			p := i + 8 // payload
			n := int(binary.LittleEndian.Uint32(file[p+16:]))
			if string(file[p+20:p+20+n]) == name {
				// uuid(16) + name(4+n) + record_bits(4) + axis_kind(1)
				file[p+16+4+n+4+1] = mode
				sum := crc32Of(file[i : i+8])
				sum = crc32Update(sum, file[p:p+length])
				binary.LittleEndian.PutUint32(file[p+length:], sum)
				return true
			}
		}
		i += 12 + length
	}
	return false
}

// TestUnknownFilterRejected: an unknown filter leaves the records permuted by a
// transform this version cannot undo, so the frame is skipped rather than
// returned as garbage — and, being a version gap rather than damage, it does not
// stop the scan.
func TestUnknownFilterRejected(t *testing.T) {
	var out bytes.Buffer
	w, _ := NewWriter(&out)
	s := loggerSchema()
	w.AddStream(s)
	w.BeginSegment(0)
	w.WriteData(s, TickVal(0), 0, 1, encodeLoggerRec(1000, 20, false))
	w.Close()

	// Set the filter byte of the DATA frame to a value from a later version.
	file := out.Bytes()
	if !patchDataFilter(t, file, 9) {
		t.Fatal("could not find the data frame")
	}

	r, _ := NewReader(bytes.NewReader(file))
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF with the frame skipped, got %v", err)
	}
	if r.Truncated {
		t.Fatal("Truncated set: a filter from a later version is not damage")
	}
	if len(r.Unsupported) != 1 || !errors.Is(r.Unsupported[0], ErrUnknownFilter) {
		t.Fatalf("Unsupported = %v, want one ErrUnknownFilter", r.Unsupported)
	}
}

// patchDataFilter overwrites the filter byte of the first DATA frame and fixes
// the frame CRC.
func patchDataFilter(t *testing.T, file []byte, filter byte) bool {
	t.Helper()
	for i := 16; i+12 <= len(file); {
		length := int(binary.LittleEndian.Uint32(file[i:]))
		if i+12+length > len(file) {
			return false
		}
		if FrameType(file[i+4]) == FrameData {
			p := i + 8
			file[p+17] = filter // axis_base(8) + record_count(4) + run_id(4) + codec(1)
			sum := crc32Of(file[i : i+8])
			sum = crc32Update(sum, file[p:p+length])
			binary.LittleEndian.PutUint32(file[p+length:], sum)
			return true
		}
		i += 12 + length
	}
	return false
}

// dbcMotorola is Vector's reference extraction for a DBC big-endian signal,
// transcribed independently of this package's implementation: start_bit names the
// signal's MSB in flat LSB-first numbering, and the walk goes down within a byte,
// jumping to bit 7 of the next byte on leaving one. It is the oracle, not a
// second copy of extractBits — the point of TestDBCMotorola is that two
// unrelated formulations agree.
func dbcMotorola(d []byte, start, length int) (uint64, bool) {
	var v uint64
	bit := start
	for i := 0; i < length; i++ {
		if bit < 0 || bit/8 >= len(d) {
			return 0, false // signal runs off the frame
		}
		v = v<<1 | uint64(d[bit/8]>>(bit%8))&1
		if bit%8 == 0 {
			bit += 15
		} else {
			bit--
		}
	}
	return v, true
}

// dbcStartToOffset converts a DBC start_bit to a big-endia Logb bit_offset. This
// one line is the entire cost of importing a Motorola signal: no data moves, and
// nothing is lost.
func dbcStartToOffset(start int) uint32 { return uint32(8*(start/8) + (7 - start%8)) }

// TestDBCMotorola is the claim §6.2 rests on: a big-endia Logb field is exactly a
// DBC Motorola signal, so a DBC importer is an offset transform and nothing more.
// If this ever fails, the format has silently forked from every CAN tool in
// existence and the spec's conformance vectors are wrong.
func TestDBCMotorola(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	checked := 0
	for trial := 0; trial < 300; trial++ {
		d := make([]byte, 8)
		rng.Read(d)
		for start := 0; start < 64; start++ {
			for length := 1; length <= 32; length++ {
				want, ok := dbcMotorola(d, start, length)
				if !ok {
					continue
				}
				got, err := extractBits(d, dbcStartToOffset(start), uint32(length), true)
				if err != nil {
					t.Fatalf("start=%d len=%d: %v", start, length, err)
				}
				if got != want {
					t.Fatalf("start=%d len=%d data=%x: extractBits=%#x, DBC reference=%#x",
						start, length, d, got, want)
				}
				checked++
			}
		}
	}
	t.Logf("agreed with the DBC reference algorithm on %d cases", checked)
}

// TestBitOrderPaths checks the byte-aligned fast path against the general bit
// loop at every offset and width, since extractBits has two implementations of
// one rule and a divergence between them would be invisible.
func TestBitOrderPaths(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	slow := func(d []byte, off, width uint32) uint64 {
		var v uint64
		for i := uint32(0); i < width; i++ {
			p := off + i
			v = v<<1 | uint64(d[p/8]>>(7-p%8))&1
		}
		return v
	}
	for trial := 0; trial < 200; trial++ {
		d := make([]byte, 8)
		rng.Read(d)
		for off := uint32(0); off < 64; off++ {
			for w := uint32(1); w <= 64 && off+w <= 64; w++ {
				got, err := extractBits(d, off, w, true)
				if err != nil {
					t.Fatal(err)
				}
				if want := slow(d, off, w); got != want {
					t.Fatalf("off=%d w=%d: fast path %#x, bit loop %#x", off, w, got, want)
				}
			}
		}
	}
}

// TestConformanceVectors is the table printed in SPEC.md §6.2. It exists so that
// no implementer ever has to recover the rule from prose — which is how MDF4 and
// DBC came to disagree in the first place. If this test and the spec table ever
// diverge, the spec is what needs fixing.
func TestConformanceVectors(t *testing.T) {
	for _, v := range conformanceVectors {
		got, err := extractBits(v.rec, v.off, v.width, v.big)
		if err != nil {
			t.Fatalf("%s: %v", v.desc, err)
		}
		if got != v.want {
			t.Fatalf("%s: got %#x, want %#x", v.desc, got, v.want)
		}
		if v.signed {
			if s := signExtend(got, v.width); s != v.wantSigned {
				t.Fatalf("%s: signed got %d, want %d", v.desc, s, v.wantSigned)
			}
		}
	}
}

var conformanceVectors = []struct {
	desc       string
	rec        []byte
	off, width uint32
	big        bool
	want       uint64
	signed     bool
	wantSigned int64
}{
	{desc: "LE 16-bit aligned", rec: []byte{0x12, 0x34}, off: 0, width: 16, want: 0x3412},
	{desc: "BE 16-bit aligned", rec: []byte{0x12, 0x34}, off: 0, width: 16, big: true, want: 0x1234},
	{desc: "LE 12-bit unaligned", rec: []byte{0x12, 0x34}, off: 4, width: 12, want: 0x341},
	{desc: "BE 12-bit unaligned, crosses byte", rec: []byte{0x12, 0x34}, off: 3, width: 12, big: true, want: 0x91A},
	{desc: "LE 1-bit flag", rec: []byte{0x00, 0x20}, off: 13, width: 1, want: 1},
	{desc: "BE 1-bit flag", rec: []byte{0x00, 0x20}, off: 10, width: 1, big: true, want: 1},
	{desc: "LE 32-bit aligned", rec: []byte{0x78, 0x56, 0x34, 0x12}, off: 0, width: 32, want: 0x12345678},
	{desc: "BE 32-bit aligned", rec: []byte{0x12, 0x34, 0x56, 0x78}, off: 0, width: 32, big: true, want: 0x12345678},
	{desc: "LE 12-bit signed -1", rec: []byte{0xFF, 0x0F}, off: 0, width: 12, want: 0xFFF, signed: true, wantSigned: -1},
	{desc: "BE 12-bit signed -1", rec: []byte{0xFF, 0xF0}, off: 0, width: 12, big: true, want: 0xFFF, signed: true, wantSigned: -1},
	{desc: "BE 6-bit within one byte", rec: []byte{0xA5}, off: 1, width: 6, big: true, want: 0x12},
	{desc: "LE 6-bit within one byte", rec: []byte{0xA5}, off: 1, width: 6, want: 0x12},
}
