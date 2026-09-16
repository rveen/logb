package logb

import (
	"bytes"
	"testing"
)

// A run declared per event must not be restated for ever: EndRun is what keeps
// a segment's preamble the size of what that segment carries, rather than of
// everything the producer has ever written.
func TestEndRunStopsRestatement(t *testing.T) {
	count := func(end bool) int {
		var buf bytes.Buffer
		w, err := NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		s := &Schema{UUID: [16]byte{1}, Name: "scope1.ch1", RecordBits: 32,
			AxisKind: AxisTime, AxisMode: AxisImplicit, AxisExp: -9, AxisUnit: "s", AxisStep: TickVal(1),
			Fields: []Field{{Name: "v", BitWidth: 32, Type: TypeFloat}}}
		if err := w.AddStream(s); err != nil {
			t.Fatal(err)
		}
		for i := uint32(1); i <= 20; i++ {
			if err := w.AddRun(&Run{ID: i, Index: i, Params: map[string]string{"vdiv": "0.5"}}); err != nil {
				t.Fatal(err)
			}
			if err := w.WriteData(s, TickVal(0), i, 1, make([]byte, 4)); err != nil {
				t.Fatal(err)
			}
			if end {
				w.EndRun(i)
			}
			if err := w.BeginSegment(0); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		runs := 0
		r, err := NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		r.OnFrame = func(f Frame) {
			if f.Type == FrameRun {
				runs++
			}
		}
		for {
			if _, err := r.Next(); err != nil {
				break
			}
		}
		return runs
	}
	// Without EndRun every segment restates every run so far: 20 declarations,
	// and run i restated by each of the 21-i segments opened after it.
	if got, want := count(false), 20+20*21/2; got != want {
		t.Fatalf("without EndRun: %d RUN frames, want %d", got, want)
	}
	// With it, each run is written once, in the segment that carries its data.
	if got, want := count(true), 20; got != want {
		t.Fatalf("with EndRun: %d RUN frames, want %d", got, want)
	}
}
