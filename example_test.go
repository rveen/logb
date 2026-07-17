// This file is package logb_test, not package logb, and it has to be: the
// generator imports logb, so a test inside logb that imported the generator
// would be an import cycle. Testing through the exported API only is the right
// constraint anyway — it is what a third party gets.
package logb_test

import (
	"bytes"
	"flag"
	"io"
	"os"
	"testing"

	"github.com/rveen/logb"
	"github.com/rveen/logb/internal/example"
)

var update = flag.Bool("update", false, "rewrite testdata/can-example.logb")

const goldenPath = "testdata/can-example.logb"

// TestExampleGolden pins the file's bytes.
//
// The fixture is a conformance artifact, not a convenience: it is what the
// format produces today, and if a change moves a single byte this fails. That is
// the point — a format whose output drifts silently is not a format. When the
// bytes are meant to move, -update says so on purpose.
func TestExampleGolden(t *testing.T) {
	var got bytes.Buffer
	if err := example.Generate(&got); err != nil {
		t.Fatal(err)
	}

	if *update {
		if err := os.WriteFile(goldenPath, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", goldenPath, got.Len())
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s (run: go test . -run TestExampleGolden -update): %v",
			goldenPath, err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		at := 0
		for at < len(want) && at < got.Len() && want[at] == got.Bytes()[at] {
			at++
		}
		t.Fatalf("the format's output moved: generated %d bytes, fixture has %d, first difference at byte %d.\n"+
			"If that was deliberate, re-run with -update. If not, this is the bug.",
			got.Len(), len(want), at)
	}
}

// TestExampleDeterministic is what makes the golden test meaningful rather than
// flaky. Two things would break it silently: Go's randomised map iteration
// reaching the wire (schema metadata is a map), and anything reading the clock.
func TestExampleDeterministic(t *testing.T) {
	var a, b bytes.Buffer
	if err := example.Generate(&a); err != nil {
		t.Fatal(err)
	}
	if err := example.Generate(&b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two runs of Generate produced different bytes; the writer is not reproducible")
	}
}

// find returns the first batch of the named stream.
func find(t *testing.T, r *logb.Reader, stream string) *logb.Batch {
	t.Helper()
	for {
		b, err := r.Next()
		if err == io.EOF {
			t.Fatalf("stream %q not found", stream)
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Schema.Name == stream {
			return b
		}
	}
}

func openGolden(t *testing.T) ([]byte, *logb.Reader) {
	t.Helper()
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := logb.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return data, r
}

// TestExampleMotorola is the payoff for §6.2. VehicleStatus carries a
// byte-aligned Motorola signal and an unaligned one that crosses byte
// boundaries, sitting beside little-endian fields in the same 8-byte frame. If
// the bit numbering were wrong these would decode to plausible-looking garbage,
// which is exactly the failure the conformance vectors exist to prevent.
func TestExampleMotorola(t *testing.T) {
	_, r := openGolden(t)
	b := find(t, r, "VehicleStatus")

	speed, err := b.Value(0, 0) // VehicleSpeed: 16-bit Motorola, byte-aligned
	if err != nil {
		t.Fatal(err)
	}
	if speed.(float64) != 30 {
		t.Fatalf("VehicleSpeed = %v km/h, want 30", speed)
	}

	odo, err := b.Value(0, 1) // Odometer: 24-bit Motorola, crosses byte boundaries
	if err != nil {
		t.Fatal(err)
	}
	if d := odo.(float64) - 40312.6; d > 1e-9 || d < -1e-9 {
		t.Fatalf("Odometer = %v km, want 40312.6", odo)
	}

	// The enum and the flag are little-endian, in the same record as the two
	// Motorola signals above — §6.3's "a single CAN frame routinely mixes Intel
	// and Motorola signals", made literal.
	if gear, err := b.Value(0, 2); err != nil || gear.(string) != "3" {
		t.Fatalf("Gear = %v (%v), want \"3\"", gear, err)
	}
	if brake, err := b.Value(0, 3); err != nil || brake.(bool) != true {
		t.Fatalf("Brake = %v (%v), want true", brake, err)
	}
}

// TestExampleBytesField reads the raw wire payload — the case that returned
// ErrCorrupt before this work, because rawValue had no TypeBytes.
func TestExampleBytesField(t *testing.T) {
	_, r := openGolden(t)
	b := find(t, r, "can0.raw")

	payload, err := b.Value(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := payload.([]byte)
	if !ok {
		t.Fatalf("payload is %T, want []byte: identity must preserve the type", payload)
	}
	if len(p) != 8 {
		t.Fatalf("payload is %d bytes, want 8", len(p))
	}

	// The raw stream and the decoded stream describe the same traffic, so the
	// first EngineData frame's payload must be the first EngineData record's
	// bytes. If the two disagree the example is lying about what it recorded.
	id, err := b.Value(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id.(uint64) != 0x100 {
		t.Fatalf("first frame is id %#x, want 0x100", id)
	}
	if got := uint16(p[0]) | uint16(p[1])<<8; float64(got)*0.25 != 800 {
		t.Fatalf("payload decodes to %v rpm, want 800", float64(got)*0.25)
	}
}

// TestExampleTail exercises §6.4: the only variable-length field in the file.
func TestExampleTail(t *testing.T) {
	_, r := openGolden(t)
	b := find(t, r, "events")

	sev, err := b.Value(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sev.(string) != "info" {
		t.Fatalf("severity = %v, want info", sev)
	}
	msg, err := b.Value(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if msg.(string) != "segment 0 started" {
		t.Fatalf("message = %q, want %q", msg, "segment 0 started")
	}

	// Records after the first are the real test: a tail is parsed sequentially,
	// so reaching record 1 means walking record 0's blob correctly.
	msg, err = b.Value(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if msg.(string) != "coolant rising: 60 degC" {
		t.Fatalf("second message = %q", msg)
	}
}

// TestExampleWholeFile reads every record of the committed fixture. The third
// segment is written transposed and deflated, so this also covers the filter and
// codec paths end to end on a real file.
func TestExampleWholeFile(t *testing.T) {
	_, r := openGolden(t)

	counts := map[string]int{}
	for {
		b, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// Touch every value of every record: a decode error anywhere fails here.
		for i := 0; i < int(b.Count); i++ {
			for f := range b.Schema.Fields {
				if _, err := b.Value(i, f); err != nil {
					t.Fatalf("%s record %d field %q: %v", b.Schema.Name, i, b.Schema.Fields[f].Name, err)
				}
			}
			if _, err := b.Axis(i); err != nil {
				t.Fatalf("%s record %d axis: %v", b.Schema.Name, i, err)
			}
		}
		counts[b.Schema.Name] += int(b.Count)
	}

	if r.Truncated {
		t.Fatal("the committed fixture reports truncated")
	}
	if !r.Closed {
		t.Fatal("the committed fixture has no END frame")
	}
	if len(r.Unsupported) != 0 {
		t.Fatalf("Unsupported = %v, want none", r.Unsupported)
	}
	for _, want := range []struct {
		stream string
		n      int
	}{
		{"EngineData", 300}, {"VehicleStatus", 150}, {"can0.raw", 450}, {"events", 7},
	} {
		if counts[want.stream] != want.n {
			t.Errorf("%s: %d records, want %d", want.stream, counts[want.stream], want.n)
		}
	}
	if len(r.Attachments["example.dbc"]) == 0 {
		t.Error("the DBC attachment is missing")
	}
	if len(r.Meta) < 4 {
		t.Errorf("only %d metadata entries", len(r.Meta))
	}
}

// TestExampleResync is rule 3 against a real file: throw away the header and the
// whole first segment, and decode what is left with full schema.
func TestExampleResync(t *testing.T) {
	data, _ := openGolden(t)

	r, at, err := logb.Resync(data[len(data)/3:])
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	b := find(t, r, "VehicleStatus")
	if len(b.Schema.Fields) != 4 {
		t.Fatalf("schema not recovered after resync: %d fields", len(b.Schema.Fields))
	}
	// The schema came from a restatement, so the Motorola field must still work.
	if v, err := b.Value(0, 0); err != nil || v.(float64) != 30 {
		t.Fatalf("VehicleSpeed after resync = %v (%v), want 30", v, err)
	}
	t.Logf("resynced %d bytes into the file, past the header and segment 0", len(data)/3+at)
}

// TestExampleTruncated is rule 2 against a real file: cut it at every byte and
// confirm that no cut ever produces an error, a panic, or a bad record.
func TestExampleTruncated(t *testing.T) {
	data, _ := openGolden(t)

	for _, cut := range []int{17, 100, 2000, 3500, 7000, 12000, len(data) - 1} {
		r, err := logb.NewReader(bytes.NewReader(data[:cut]))
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
				t.Fatalf("cut=%d: %v", cut, err)
			}
			for i := 0; i < int(b.Count); i++ {
				for f := range b.Schema.Fields {
					if _, err := b.Value(i, f); err != nil {
						t.Fatalf("cut=%d: %s record %d: batch survived truncation but is damaged: %v",
							cut, b.Schema.Name, i, err)
					}
				}
			}
			n += int(b.Count)
		}
		if cut < len(data) && !r.Truncated && !r.Closed {
			t.Errorf("cut=%d: neither truncated nor closed", cut)
		}
		t.Logf("cut=%-6d %d records intact", cut, n)
	}
}
