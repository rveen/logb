package mdf

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/rveen/logb"
)

func read(t *testing.T, name string) *File {
	t.Helper()
	f, err := os.Open("../testdata/mdf/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	m, err := ReadFile(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// TestRead checks the reader against every fixture. The expected values are not
// this package's own output: they are what an unrelated MDF reader
// (golib/formats/mdf, itself written against asammdf) prints for the same files.
func TestRead(t *testing.T) {
	tests := []struct {
		file     string
		version  uint16
		final    bool
		groups   int
		group    string
		records  int
		channels int
		master   string
		first    []float64 // the master's first values
	}{
		{"sample2.mf4", 410, true, 1, "", 20, 4, "time",
			[]float64{10, 10.05, 10.1, 10.15, 10.2}},
		{"sample3.mf4", 410, true, 1, "CCVS1_CPC", 3, 2, "t",
			[]float64{145996.338439, 145996.43817, 145996.53830800002}},
		{"Discrete_deflate.mf4", 410, true, 1, "100ms_sync", 124, 2, "time",
			[]float64{0.11275759999998419, 0.21275760000000693, 0.31275760000002967}},
		{"sample_compressed.mf4", 410, true, 1, "AcqName", 100000, 2, "Time",
			[]float64{0.0031, 0.0062, 0.0093, 0.0124, 0.0155}},
		// Unfinalized: every cycle count in this file is zero, so the record
		// count below is one this reader worked out by walking the data.
		{"obd2-trunc.mf4", 411, false, 2, "CAN_DataFrame", 1619, 10, "Timestamp",
			[]float64{186.90885, 186.92370000000003, 186.92925000000002}},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			m := read(t, tt.file)
			if m.Version != tt.version {
				t.Errorf("version %d, want %d", m.Version, tt.version)
			}
			if m.Finalized != tt.final {
				t.Errorf("finalized %v, want %v", m.Finalized, tt.final)
			}
			if len(m.Groups) != tt.groups {
				t.Fatalf("%d groups, want %d", len(m.Groups), tt.groups)
			}
			g := m.Groups[0]
			if g.Name != tt.group {
				t.Errorf("group name %q, want %q", g.Name, tt.group)
			}
			if g.Records != tt.records {
				t.Errorf("%d records, want %d", g.Records, tt.records)
			}
			if len(g.Channels) != tt.channels {
				t.Errorf("%d channels, want %d", len(g.Channels), tt.channels)
			}
			m0 := g.Master()
			if m0 == nil {
				t.Fatal("no master channel")
			}
			if m0.Name != tt.master {
				t.Errorf("master %q, want %q", m0.Name, tt.master)
			}
			if m0.Sync != SyncTime {
				t.Errorf("master sync %v, want time", m0.Sync)
			}
			for i, want := range tt.first {
				got, err := m0.Float(g.Record(i))
				if err != nil {
					t.Fatal(err)
				}
				if math.Abs(got-want) > 1e-9 {
					t.Errorf("master[%d] = %v, want %v", i, got, want)
				}
			}
		})
	}
}

// TestComposition checks that a composed channel is taken apart. A CAN frame is
// stored as one bytes channel with its fields as members; keeping the container
// would mean a 14-byte blob where the ID, the DLC and the payload should be.
func TestComposition(t *testing.T) {
	g := read(t, "obd2-trunc.mf4").Groups[0]

	var names []string
	for _, c := range g.Channels {
		names = append(names, c.Name)
		if c.Name == "CAN_DataFrame" {
			t.Error("the composed parent is still a channel; its members cover the same bytes")
		}
	}
	want := map[string]struct{ byteOff, bitOff, bits uint32 }{
		"CAN_DataFrame.ID":  {8, 2, 29},
		"CAN_DataFrame.DLC": {13, 2, 4},
		"CAN_DataFrame.Dir": {12, 0, 1},
	}
	for _, c := range g.Channels {
		w, ok := want[c.Name]
		if !ok {
			continue
		}
		if c.ByteOffset != w.byteOff || c.BitOffset != w.bitOff || c.BitCount != w.bits {
			t.Errorf("%s at byte %d bit %d width %d, want %d/%d/%d",
				c.Name, c.ByteOffset, c.BitOffset, c.BitCount, w.byteOff, w.bitOff, w.bits)
		}
		if c.Parent == nil || c.Parent.Name != "CAN_DataFrame" {
			t.Errorf("%s does not know what it is part of", c.Name)
		}
		delete(want, c.Name)
	}
	if len(want) > 0 {
		t.Errorf("missing channels %v; found %v", want, names)
	}
}

// TestVLSD checks that variable-length samples are resolved rather than left as
// the offsets MDF stores.
func TestVLSD(t *testing.T) {
	g := read(t, "obd2-trunc.mf4").Groups[0]

	var payload *Channel
	for _, c := range g.Channels {
		if c.Kind == VLSD {
			payload = c
		}
	}
	if payload == nil {
		t.Fatal("no variable-length channel")
	}
	samples := g.VLSD[payload]
	if len(samples) != g.Records {
		t.Fatalf("%d samples for %d records", len(samples), g.Records)
	}
	// An OBD2 reply to a mode 01 request: length 3, mode 0x41, PID 0x0B.
	if want := []byte{3, 0x41, 0x0B, 28, 255, 255, 255, 255}; !bytes.Equal(samples[0], want) {
		t.Errorf("first payload % x, want % x", samples[0], want)
	}
	if _, err := payload.Raw(g.Record(0)); err == nil {
		t.Error("decoding a VLSD channel out of the record should fail; its bytes are not there")
	}
}

// TestUnsorted checks the demultiplexing of a record-id-tagged data group,
// including the group that saw no traffic at all.
func TestUnsorted(t *testing.T) {
	m := read(t, "obd2-trunc.mf4")
	if len(m.Groups) != 2 {
		t.Fatalf("%d groups, want 2 (CAN and LIN)", len(m.Groups))
	}
	can, lin := m.Groups[0], m.Groups[1]
	if can.Name != "CAN_DataFrame" || lin.Name != "LIN_Frame" {
		t.Fatalf("groups %q, %q", can.Name, lin.Name)
	}
	if can.Records == 0 {
		t.Error("no CAN records were demultiplexed")
	}
	if lin.Records != 0 {
		t.Errorf("%d LIN records, want 0: this recording has no LIN traffic", lin.Records)
	}
	// The VLSD group carried the payloads and is not a measurement of its own.
	for _, g := range m.Groups {
		if g.isVLSD {
			t.Error("a variable-length data group was kept as a channel group")
		}
	}
	// Every record must be whole: the file is cut mid-recording, and a partial
	// trailing record has to be dropped rather than padded.
	if len(can.Data) != can.Records*(can.RecordBytes+can.InvalBytes) {
		t.Errorf("%d bytes for %d records of %d", len(can.Data), can.Records, can.RecordBytes)
	}
}

func TestRefusals(t *testing.T) {
	t.Run("not mdf", func(t *testing.T) {
		_, err := ReadFile(bytes.NewReader(make([]byte, 128)))
		if !errors.Is(err, ErrNotMDF) {
			t.Errorf("got %v, want ErrNotMDF", err)
		}
	})
	t.Run("mdf3", func(t *testing.T) {
		b := make([]byte, 128)
		copy(b, "MDF     3.30    ")
		b[28], b[29] = 74, 1 // version 330
		_, err := ReadFile(bytes.NewReader(b))
		if !errors.Is(err, ErrVersion) {
			t.Errorf("got %v, want ErrVersion", err)
		}
	})
}

// ── The mapping's corners, which no fixture reaches ───────────────────────────

// TestInvalidationGuard checks that MDF's invalidation bits become Logb guards:
// a set bit means the sample was not measured, which §6.2 calls an absent
// field. No fixture in testdata uses them, so the group is built by hand.
func TestInvalidationGuard(t *testing.T) {
	speed := &Channel{
		Name: "speed", Unit: "km/h", Kind: Value,
		DataType: DTUintLE, ByteOffset: 8, BitCount: 16,
		HasInvalBit: true, InvalBit: 3,
	}
	time := &Channel{Name: "t", Kind: Master, Sync: SyncTime, DataType: DTFloatLE, BitCount: 64}
	g := &Group{
		Name: "g", Records: 2, RecordBytes: 10, InvalBytes: 1,
		Channels: []*Channel{time, speed},
		VLSD:     map[*Channel][][]byte{},
		Data:     make([]byte, 2*11),
	}
	// Record 0: t=1 s, speed=300, valid. Record 1: same bytes, bit 3 set.
	put := func(i int, sec float64, v uint16, inval byte) {
		rec := g.Data[i*11:]
		le64(rec, math.Float64bits(sec))
		rec[8], rec[9] = byte(v), byte(v>>8)
		rec[10] = inval
	}
	put(0, 1, 300, 0)
	put(1, 2, 300, 1<<3)

	var buf bytes.Buffer
	if err := Write(&File{Groups: []*Group{g}}, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, f := range b.Schema.Fields {
		if f.Name == "speed" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no speed field")
	}
	if !b.Schema.Fields[idx].Guarded {
		t.Fatal("the field is not guarded; its invalidation bit was dropped")
	}
	if v, err := b.Value(0, idx); err != nil || !same(v, uint64(300)) {
		t.Errorf("record 0: %v, %v — want 300", v, err)
	}
	if _, err := b.Value(1, idx); !errors.Is(err, logb.ErrFieldAbsent) {
		t.Errorf("record 1 was marked invalid; want ErrFieldAbsent, got %v", err)
	}
}

// TestVirtualMaster checks the axis of a group whose master is the record index
// itself. MDF stores no bytes for it, and neither does Logb: it is an implicit
// axis (§5), which is the one case where a converted record is smaller than the
// one it came from.
func TestVirtualMaster(t *testing.T) {
	master := &Channel{
		Name: "t", Kind: VirtualMaster, Sync: SyncTime,
		Conv: &Conversion{Conv: logb.Linear{A: 0, B: 0.01}, Kind: "linear"},
	}
	value := &Channel{Name: "v", Kind: Value, DataType: DTIntLE, BitCount: 16}
	g := &Group{
		Name: "virtual", Records: 3, RecordBytes: 2,
		Channels: []*Channel{master, value},
		VLSD:     map[*Channel][][]byte{},
		Data:     []byte{1, 0, 2, 0, 3, 0},
	}
	var buf bytes.Buffer
	if err := Write(&File{Groups: []*Group{g}}, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Schema.AxisMode != logb.AxisImplicit {
		t.Errorf("axis mode %v, want implicit", b.Schema.AxisMode)
	}
	if n := b.Schema.RecordBytes(); n != 2 {
		t.Errorf("record is %d bytes, want 2: a virtual master stores nothing", n)
	}
	for i, want := range []float64{0, 0.01, 0.02} {
		ax, err := b.Axis(i)
		if err != nil {
			t.Fatal(err)
		}
		if got := ax.Seconds(b.Schema.AxisExp); math.Abs(got-want) > 1e-12 {
			t.Errorf("axis[%d] = %v, want %v", i, got, want)
		}
	}
}

// TestTabNearest checks the one conversion whose rule differs between the two
// formats. MDF's table-without-interpolation picks the nearest key; Logb's
// picks the nearest at or below. The importer shifts the keys to the midpoints
// so that the second rule computes the first — exactly, not just on the keys.
func TestTabNearest(t *testing.T) {
	// Keys 0→10, 10→20, 30→40. Nearest-key boundaries fall at 5 and 20.
	conv := nearestTable([]float64{0, 10, 30}, []float64{10, 20, 40})
	for _, tc := range []struct{ in, want float64 }{
		{-1, 10}, {0, 10}, {4.9, 10},
		{5, 20}, {10, 20}, {19.9, 20},
		{20, 40}, {30, 40}, {100, 40},
	} {
		if got := conv.Apply(tc.in); got != tc.want {
			t.Errorf("%v → %v, want %v", tc.in, got, tc.want)
		}
	}
}

func le64(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
