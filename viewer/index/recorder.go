package index

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/rveen/logb"
)

// ErrNotIndexable is returned by a Recorder asked to write a stream it cannot
// summarise as it goes. See Recorder.
var ErrNotIndexable = errors.New("index: stream has a variable-length field")

// A Recorder builds the index while a file is written, so that opening it
// afterwards costs no scan.
//
// A scan re-reads the whole file, checks every CRC, decompresses every DATA
// frame and decodes every record, purely to learn things the writer knew and
// then discarded. The writer already knows where each frame landed, and at
// WriteData it holds the records uncompressed and unfiltered — which is the
// cheapest moment in a file's life to reduce them. Recording the index there
// is the same answer arrived at for almost nothing.
//
// The index it produces must be the index a scan would produce, and that is
// tested rather than assumed: the same builder, the same statsFor, and the same
// Batch decoding path serve both. What differs is only where the batch came
// from.
//
// # Use
//
// A Recorder is a logb.Writer, so everything that writer does it does:
//
//	r, err := index.NewRecorder(f, path)
//	r.AddStream(s)
//	r.BeginSegment(t)
//	r.WriteData(s, base, 0, n, records)
//	r.Close()
//	r.Save()          // the sidecar; index.Open now finds it and does not scan
//
// The methods that carry payloads this package needs — WriteData, WriteMeta and
// WriteAttach — are shadowed. Reaching past them to the embedded logb.Writer
// writes a correct file with an incomplete index, so do not.
//
// # What it cannot do
//
// A stream with a variable-length field is refused. Tier 1 reduces a field by
// decoding it, and decoding a record's tail needs the tail table a reader builds
// when it parses the frame — which a writer holding only the bytes it is about
// to emit does not have. Rather than produce an index that silently lacks
// statistics for such a stream, the Recorder records the refusal and Save
// returns it, leaving the file to be opened the way it always was: by scanning.
type Recorder struct {
	*logb.Writer

	path string
	idx  builder

	byUUID  map[[16]byte]*Stream
	streams []*Stream

	meta        []logb.Meta
	attachments []Attachment

	// epoch is the earliest absolute tick seen on any time axis. Frames and
	// restatements are kept in absolute ticks and rebased only into the
	// snapshot File builds, so that a File may be taken more than once while
	// the recording is still growing.
	epoch     int64
	haveEpoch bool

	// holdOffset latches the position of the HOLD frame whose OnHold has not
	// fired yet, exactly as the scan does.
	holdOffset uint64

	// err is the first reason this index cannot be trusted. It is sticky:
	// once the index is incomplete it does not become complete again.
	err error

	started time.Time
}

// NewRecorder writes a Logb file to w and indexes it as it goes. path is where
// the file will live, which is what the sidecar is keyed to; it is not opened
// or written by the Recorder.
func NewRecorder(w io.Writer, path string) (*Recorder, error) {
	lw, err := logb.NewWriter(w)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		Writer:  lw,
		path:    path,
		byUUID:  map[[16]byte]*Stream{},
		epoch:   math.MaxInt64,
		started: time.Now(),
	}
	lw.OnFrame = func(f logb.Frame) {
		if f.Type == logb.FrameHold {
			r.holdOffset = f.Offset
		}
		r.idx.onFrame(f)
	}
	lw.OnSchema = func(s *logb.Schema, id uint16) {
		r.idx.onSchema(s, id)
		r.stream(s)
	}
	lw.OnHold = func(h *logb.Hold) {
		if st := r.byUUID[h.Schema.UUID]; st != nil {
			st.noteHold(h, r.holdOffset)
		}
	}
	return r, nil
}

// stream returns the model for a schema, declaring it on first sight.
//
// A stream that is declared and never writes a record still belongs in the
// tree: it is part of what the file says.
func (r *Recorder) stream(s *logb.Schema) *Stream {
	if st := r.byUUID[s.UUID]; st != nil {
		return st
	}
	for i := range s.Fields {
		if s.Fields[i].Variable {
			r.fail(fmt.Errorf("%w: %q field %q", ErrNotIndexable, s.Name, s.Fields[i].Name))
			break
		}
	}
	st := newStream(s)
	r.byUUID[s.UUID] = st
	r.streams = append(r.streams, st)
	return st
}

func (r *Recorder) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Err reports why the index is incomplete, or nil.
func (r *Recorder) Err() error { return r.err }

// WriteData writes a DATA frame and indexes it.
//
// The batch summarised here is the one the writer is about to emit, decoded
// through the same Batch that a reader would build from the same bytes. The
// DATA frame's placement arrives through OnFrame during the write and is
// paired with this batch afterwards, which is the same latch-and-pair the scan
// makes between OnFrame and Reader.Next.
func (r *Recorder) WriteData(s *logb.Schema, base logb.AxisVal, runID uint32, recordCount uint32, records []byte) error {
	if err := r.Writer.WriteData(s, base, runID, recordCount, records); err != nil {
		return err
	}
	b := &logb.Batch{
		Schema:   s,
		RunID:    runID,
		AxisBase: base,
		Count:    recordCount,
		Data:     records,
	}
	r.idx.onBatch(b)

	st := r.stream(s)
	st.noteRun(runID, nil)
	st.Records += int(recordCount)
	st.stats = append(st.stats, statsFor(b, st.Fields))

	if s.AxisKind == logb.AxisTime && recordCount > 0 {
		if first, err := b.Axis(0); err == nil {
			if t := first.Ticks(); t < r.epoch {
				r.epoch, r.haveEpoch = t, true
			}
		}
	}
	return nil
}

// WriteMeta writes a metadata pair and keeps it for the index.
func (r *Recorder) WriteMeta(key, value string) error {
	if err := r.Writer.WriteMeta(key, value); err != nil {
		return err
	}
	r.meta = append(r.meta, logb.Meta{Key: key, Value: value})
	return nil
}

// WriteAttach embeds a file and keeps it for the index.
func (r *Recorder) WriteAttach(name string, data []byte) error {
	if err := r.Writer.WriteAttach(name, data); err != nil {
		return err
	}
	r.attachments = append(r.attachments, Attachment{
		Name: name,
		Size: len(data),
		Data: append([]byte(nil), data...),
	})
	return nil
}

// AddRun declares a run and keeps its parameters for the index.
func (r *Recorder) AddRun(run *logb.Run) error {
	if err := r.Writer.AddRun(run); err != nil {
		return err
	}
	for _, st := range r.streams {
		st.noteRun(run.ID, run)
	}
	return nil
}

// File returns the index of everything written so far.
//
// It may be called more than once, including while the recording is still
// growing: frames and restatements are held in absolute ticks and rebased onto
// the epoch only in the snapshot returned here, so no call disturbs the next.
//
// size is the file's length in bytes. The Recorder cannot know it, because it
// writes to an io.Writer and not to a file; a caller stats the path or counts
// what it has written. Save supplies it by stat.
func (r *Recorder) File(size int64) *File {
	fi := &File{
		Path:        r.path,
		Size:        size,
		Meta:        r.meta,
		Attachments: append([]Attachment(nil), r.attachments...),
		Streams:     append([]*Stream(nil), r.streams...),
	}

	// The frame index is copied before rebasing, because rebasing subtracts
	// the epoch in place and a second call would subtract it twice.
	snap := &FrameIndex{
		Segments: r.idx.idx.Segments,
		Data:     append([]DataFrame(nil), r.idx.idx.Data...),
	}
	if r.haveEpoch {
		fi.Epoch, fi.HasEpoch = r.epoch, true
		snap.rebaseFrames(r.epoch)
		for _, st := range fi.Streams {
			// Idempotent: a Hold keeps its absolute ticks and recomputes Axis.
			st.rebaseHolds(r.epoch)
		}
	}
	snap.sortSegmentRuns()
	fi.Frames = snap

	// Frames arrive in the order stats were appended, so grouping by UUID keeps
	// the two parallel. Rebuilt from scratch each time, since this may run
	// again over a longer recording.
	for _, st := range fi.Streams {
		st.FrameList = st.FrameList[:0]
	}
	for _, d := range snap.Data {
		if st := r.byUUID[d.UUID]; st != nil {
			st.FrameList = append(st.FrameList, d)
		}
	}
	for _, st := range fi.Streams {
		st.span()
	}
	sortFile(fi)
	return fi
}

// Save writes the sidecar for the file just recorded, so that index.Open finds
// a valid cache and never scans.
//
// It must be called after the file has been closed and flushed to path: the
// sidecar is validated against the source's size, modification time and a hash
// of its first bytes, and one written against a half-flushed file would be
// rejected at open and quietly cost a scan.
func (r *Recorder) Save() error {
	if r.err != nil {
		return r.err
	}
	size, err := fileSize(r.path)
	if err != nil {
		return err
	}
	fi := r.File(size)
	if len(fi.Streams) == 0 && len(fi.Frames.Data) == 0 {
		return errors.New("index: nothing was recorded")
	}
	for _, st := range fi.Streams {
		if len(st.FrameList) != len(st.stats) {
			// Only reachable if frame collection and statistics fall out of
			// step, which would misattribute every summary in the stream.
			return fmt.Errorf("index: stream %q has %d frames but %d stat rows",
				st.Name, len(st.FrameList), len(st.stats))
		}
	}
	sort.Slice(fi.Attachments, func(i, j int) bool { return fi.Attachments[i].Name < fi.Attachments[j].Name })
	return SaveSidecar(fi, time.Since(r.started))
}
