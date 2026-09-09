package logb

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The writer's account of what it emitted must be exactly what a reader later
// finds in the bytes. That equivalence is the whole warrant for building an
// index as a file is written instead of by scanning it: if the two hooks can
// disagree, an as-written index is a second opinion rather than the same
// answer arrived at cheaply.
func TestOnFrameMatchesTheReader(t *testing.T) {
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	var wrote []Frame
	w.OnFrame = func(f Frame) { wrote = append(wrote, f) }

	s := loggerSchema()
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	if err := w.BeginSegment(1_700_000_000_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMeta("recorder", "test"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteAttach("notes.txt", []byte("a short attachment")); err != nil {
		t.Fatal(err)
	}

	var recs []byte
	for i := 0; i < 100; i++ {
		recs = append(recs, encodeLoggerRec(uint16(i*40), int16(i-50), i%7 == 0)...)
	}
	// Several segments, so that the restated schemas are reported too.
	for seg := 0; seg < 3; seg++ {
		if seg > 0 {
			if err := w.BeginSegment(1_700_000_000_000_000_000 + int64(seg)*1e9); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.WriteData(s, TickVal(int64(seg)*1e9), 0, 100, recs); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var read []Frame
	r.OnFrame = func(f Frame) { read = append(read, f) }
	for {
		if _, err := r.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}

	if len(wrote) == 0 {
		t.Fatal("the writer reported no frames at all")
	}
	if len(wrote) != len(read) {
		t.Fatalf("the writer reported %d frames, the reader found %d", len(wrote), len(read))
	}
	for i := range wrote {
		if wrote[i] != read[i] {
			t.Fatalf("frame %d: the writer said %+v, the reader found %+v", i, wrote[i], read[i])
		}
	}

	// The offsets must actually be offsets: each frame begins where the
	// previous one ended, and the last ends at the end of the file.
	for i, f := range wrote {
		if i > 0 {
			prev := wrote[i-1]
			if end := prev.Offset + prev.Size(); f.Offset != end {
				t.Fatalf("frame %d begins at %d, but frame %d ended at %d",
					i, f.Offset, i-1, end)
			}
		}
	}
	last := wrote[len(wrote)-1]
	if end := last.Offset + last.Size(); end != uint64(out.Len()) {
		t.Errorf("the last frame ends at %d, the file is %d bytes", end, out.Len())
	}
}

// A frame the file does not contain must never be reported. The hook fires
// only once a frame's bytes have gone out, so a writer onto a failing sink
// reports a prefix of what it tried to write and never more.
func TestOnFrameSilentOnWriteFailure(t *testing.T) {
	// Room for the header and a few frames, then nothing.
	fw := &failAfter{n: 600}
	w, err := NewWriter(fw)
	if err != nil {
		t.Fatal(err)
	}
	var wrote []Frame
	w.OnFrame = func(f Frame) { wrote = append(wrote, f) }

	s := loggerSchema()
	_ = w.AddStream(s)
	_ = w.BeginSegment(1_700_000_000_000_000_000)
	for i := 0; i < 200; i++ {
		if err := w.WriteMeta("key", "value"); err != nil {
			break
		}
	}

	if len(wrote) == 0 {
		t.Fatal("no frame was reported, so the sink failed too early to test anything")
	}
	// Every reported frame must lie entirely within the bytes the sink took.
	for i, f := range wrote {
		if end := f.Offset + f.Size(); end > uint64(fw.written) {
			t.Fatalf("frame %d was reported ending at %d, but only %d bytes were accepted",
				i, end, fw.written)
		}
	}
	if err := w.WriteMeta("key", "value"); err == nil {
		t.Error("the writer kept succeeding past the sink's limit")
	}
}

// failAfter accepts n bytes and then refuses.
type failAfter struct {
	n       int
	written int
}

func (f *failAfter) Write(b []byte) (int, error) {
	if f.written >= f.n {
		return 0, errors.New("no more room")
	}
	room := f.n - f.written
	if len(b) <= room {
		f.written += len(b)
		return len(b), nil
	}
	f.written += room
	return room, io.ErrShortWrite
}
