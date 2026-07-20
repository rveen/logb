package server

import (
	"encoding/hex"
	"net/http"

	"github.com/rveen/logb/viewer/index"
)

// DefaultFrameLimit bounds how many frames /api/frames will enumerate.
//
// The frame map is an inspector, not a data path. A file with a million frames
// has nothing to say to a human one row at a time, and the per-segment
// summary is still complete — so the list is capped and the response says it
// was, rather than quietly showing the first few thousand as if they were all.
const DefaultFrameLimit = 4000

type frameMapDTO struct {
	Size     int64        `json:"size"`
	Segments []segmentDTO `json:"segments"`
	Frames   []frameDTO   `json:"frames"`
	// Total is how many DATA frames the file holds; Frames may be shorter.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

type segmentDTO struct {
	Index int `json:"index"`
	// Offset is where the segment's SYNC frame sits, or where its first frame
	// does when it has none — a resynced tail begins without one.
	Offset  uint64 `json:"offset"`
	End     uint64 `json:"end"`
	Schemas int    `json:"schemas"`
	Runs    int    `json:"runs"`
	Frames  int    `json:"frames"`
	Records int    `json:"records"`
}

type frameDTO struct {
	Offset  uint64  `json:"offset"`
	Size    uint64  `json:"size"`
	Segment int     `json:"segment"`
	Stream  string  `json:"stream"`
	UUID    string  `json:"uuid"`
	Run     uint32  `json:"run"`
	Records uint32  `json:"records"`
	First   float64 `json:"first"`
	Last    float64 `json:"last"`
}

// handleFrames serves the Tier 0 index: where every DATA frame is and what it
// holds.
//
// This is a visual cmd/logbdump, and it exists because the layout is the thing
// that makes the rest of the viewer possible. Segment boundaries are where a
// damaged file can be picked up again (rule 3), a frame is the unit of every
// decode and of every Tier 1 statistic, and the record counts per frame are
// what a decimation budget is spent on. When a chart looks wrong, this is where
// to look next.
//
// Note this is built by scanning, never from the file's own INDEX frame: SPEC
// §9 makes that an accelerator a reader must be able to rebuild and must not
// trust over the frames themselves.
func (s *Server) handleFrames(w http.ResponseWriter, r *http.Request) {
	f, _, ok := s.ready(w)
	if !ok {
		return
	}
	limit := intParam(r, "limit", DefaultFrameLimit, 1, 100000)

	byUUID := map[[16]byte]*index.Stream{}
	for _, st := range f.Streams {
		var key [16]byte
		if b, err := hexUUID(st.UUID); err == nil {
			key = b
			byUUID[key] = st
		}
	}

	dto := frameMapDTO{
		Size:     f.Size,
		Segments: make([]segmentDTO, 0, len(f.Frames.Segments)),
		Frames:   make([]frameDTO, 0, min(limit, len(f.Frames.Data))),
		Total:    len(f.Frames.Data),
	}

	// Per-segment totals come from every frame, not just the listed ones, so
	// the summary stays complete when the list is capped.
	perSeg := make([]segmentDTO, len(f.Frames.Segments))
	for i, seg := range f.Frames.Segments {
		perSeg[i] = segmentDTO{
			Index:   seg.Index,
			Offset:  seg.Sync.Offset,
			Schemas: len(seg.Schemas),
			Runs:    len(seg.Runs),
		}
	}

	for _, d := range f.Frames.Data {
		if d.Segment >= 0 && d.Segment < len(perSeg) {
			perSeg[d.Segment].Frames++
			perSeg[d.Segment].Records += int(d.Count)
			if e := d.End(); e > perSeg[d.Segment].End {
				perSeg[d.Segment].End = e
			}
			if perSeg[d.Segment].Offset == 0 {
				// A segment with no SYNC frame — a resynced tail. Its extent
				// starts at its first frame rather than at zero, which would
				// draw it as covering the whole file.
				perSeg[d.Segment].Offset = d.Offset
			}
		}
		if len(dto.Frames) >= limit {
			dto.Truncated = true
			continue
		}
		name, uuid := "", ""
		if st := byUUID[d.UUID]; st != nil {
			name, uuid = st.Name, st.UUID
		}
		dto.Frames = append(dto.Frames, frameDTO{
			Offset: d.Offset, Size: d.Size(), Segment: d.Segment,
			Stream: name, UUID: uuid, Run: d.RunID, Records: d.Count,
			First: d.First(), Last: d.Last(),
		})
	}
	dto.Segments = perSeg
	writeJSON(w, dto)
}

// hexUUID parses the hex form the model carries back into raw bytes.
func hexUUID(s string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}
