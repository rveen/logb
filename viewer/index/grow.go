package index

import (
	"encoding/hex"
	"fmt"
)

// resumePoint finds where a scan may restart to pick up a file's new tail.
//
// The last cached segment is re-read rather than trusted. A file that grew was
// probably being written when it was indexed, so its final segment may have
// been mid-write: more frames could have landed in it after the cache was
// taken. Re-reading one segment is cheap and removes the whole class of
// off-by-one-frame staleness.
//
// A file with no SYNC frame at all cannot be resumed into safely, so it falls
// back to a full scan.
func resumePoint(fi *File) (int64, bool) {
	segs := fi.Frames.Segments
	if len(segs) == 0 {
		return 0, false
	}
	last := segs[len(segs)-1]
	if last.Sync.Type == 0 {
		// A synthetic segment created for a file that opened with DATA rather
		// than SYNC. There is no boundary to resume from.
		return 0, false
	}
	return int64(last.Sync.Offset), true
}

// mergeTail joins a freshly scanned tail onto a cached index.
//
// resume is the file offset the tail scan started at, and everything at or
// after it is replaced by the tail: the tail is authoritative for that region
// because it read the file as it is now.
func mergeTail(cached, tail *File, resume int64) (*File, error) {
	// Keep only the segments that end before the resume point. The segment at
	// resume was re-read and arrives again from the tail.
	var keptSegs []*Segment
	for _, s := range cached.Frames.Segments {
		if int64(s.Sync.Offset) < resume {
			keptSegs = append(keptSegs, s)
		}
	}
	var keptData []DataFrame
	for _, d := range cached.Frames.Data {
		if int64(d.Offset) < resume {
			keptData = append(keptData, d)
		}
	}

	// Segment indices are positions in this slice, so the tail's must shift by
	// however many we kept.
	shift := len(keptSegs)
	segs := make([]*Segment, 0, shift+len(tail.Frames.Segments))
	segs = append(segs, keptSegs...)
	for _, s := range tail.Frames.Segments {
		s.Index += shift
		segs = append(segs, s)
	}

	data := make([]DataFrame, 0, len(keptData)+len(tail.Frames.Data))
	data = append(data, keptData...)
	for _, d := range tail.Frames.Data {
		d.Segment += shift
		data = append(data, d)
	}

	out := &File{
		Path:   cached.Path,
		Size:   tail.Size,
		Frames: &FrameIndex{Segments: segs, Data: data},
		// The tail read the end of the file, so its view of how the file
		// finishes is the current one.
		Truncated: tail.Truncated,
		Closed:    cached.Closed || tail.Closed,
	}

	// A time.anchor or an attachment may live in either part, and metadata is
	// only ever appended, so the cached ones still stand.
	out.Meta = append(out.Meta, cached.Meta...)
	out.Meta = append(out.Meta, tail.Meta...)
	out.Attachments = append(out.Attachments, cached.Attachments...)
	for _, a := range tail.Attachments {
		if !hasAttachment(out.Attachments, a.Name) {
			out.Attachments = append(out.Attachments, a)
		}
	}
	out.Unsupported = mergeStrings(cached.Unsupported, tail.Unsupported)

	// Epoch is the earliest sample anywhere. New data is appended, so it can
	// only ever confirm the cached one — but a file whose first segment held no
	// time axis could acquire one.
	switch {
	case cached.HasEpoch && tail.HasEpoch:
		out.Epoch, out.HasEpoch = min(cached.Epoch, tail.Epoch), true
	case cached.HasEpoch:
		out.Epoch, out.HasEpoch = cached.Epoch, true
	case tail.HasEpoch:
		out.Epoch, out.HasEpoch = tail.Epoch, true
	}

	// The tail rebased its frames onto its own epoch. If that differs from the
	// merged one, its offsets are wrong by the difference.
	if tail.HasEpoch && out.HasEpoch && tail.Epoch != out.Epoch {
		delta := tail.Epoch - out.Epoch
		for i := range data {
			if int64(data[i].Offset) >= resume && data[i].Time {
				data[i].FirstTick += delta
				data[i].LastTick += delta
			}
		}
	}

	// Streams: the cached ones carry their schemas and their statistics for the
	// kept frames; the tail brings both for the new region.
	byUUID := map[string]*Stream{}
	for _, st := range cached.Streams {
		clone := *st
		clone.FrameList = nil
		clone.stats = nil
		clone.Records = 0
		byUUID[st.UUID] = &clone
		out.Streams = append(out.Streams, &clone)
	}
	for _, st := range tail.Streams {
		if byUUID[st.UUID] == nil {
			// A stream that first appears in the new tail.
			clone := *st
			clone.FrameList = nil
			clone.stats = nil
			clone.Records = 0
			byUUID[st.UUID] = &clone
			out.Streams = append(out.Streams, &clone)
		}
	}

	if err := reattach(byUUID, cached, keptData, 0, resume, true); err != nil {
		return nil, err
	}
	if err := reattach(byUUID, tail, data, resume, 1<<62, false); err != nil {
		return nil, err
	}

	for _, st := range out.Streams {
		st.span()
	}
	sortFile(out)
	return out, nil
}

// reattach copies frames and their statistics from one source into the merged
// streams, for frames in [lo, hi).
//
// Statistics are positional: stats[k] describes src's k-th frame of that
// stream. Walking the source's own frame list keeps the two aligned.
func reattach(byUUID map[string]*Stream, src *File, frames []DataFrame, lo, hi int64, cachedSide bool) error {
	for _, srcStream := range src.Streams {
		dst := byUUID[srcStream.UUID]
		if dst == nil {
			return fmt.Errorf("index: merge lost stream %s", srcStream.UUID)
		}
		for k, f := range srcStream.FrameList {
			off := int64(f.Offset)
			if off < lo || off >= hi {
				continue
			}
			if k >= len(srcStream.stats) {
				return fmt.Errorf("index: stream %q frame %d has no statistics", srcStream.Name, k)
			}
			// Take the frame from the merged list so any epoch correction
			// applied there is carried, rather than the source's copy.
			merged, ok := findFrame(frames, f.Offset)
			if !ok {
				continue
			}
			dst.FrameList = append(dst.FrameList, merged)
			dst.stats = append(dst.stats, srcStream.stats[k])
			dst.Records += int(merged.Count)
		}
		for _, r := range srcStream.Runs {
			dst.noteRunValues(r)
		}
	}
	_ = cachedSide
	return nil
}

func findFrame(frames []DataFrame, offset uint64) (DataFrame, bool) {
	for _, f := range frames {
		if f.Offset == offset {
			return f, true
		}
	}
	return DataFrame{}, false
}

// noteRunValues records a run learned from another copy of the same stream.
func (s *Stream) noteRunValues(r *Run) {
	for _, existing := range s.Runs {
		if existing.ID == r.ID {
			if len(existing.Params) == 0 && len(r.Params) > 0 {
				existing.Index, existing.Params = r.Index, r.Params
			}
			return
		}
	}
	c := *r
	s.Runs = append(s.Runs, &c)
}

func hasAttachment(as []Attachment, name string) bool {
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}

func mergeStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// uuidHex is the string key streams are indexed by.
func uuidHex(u [16]byte) string { return hex.EncodeToString(u[:]) }
