package server

import (
	"testing"
)

// TestFramesEndpoint checks the frame map against what the example file is
// known to contain: three segments, each restating every schema, and DATA
// frames that partition the file without overlapping.
func TestFramesEndpoint(t *testing.T) {
	s := serve(t)
	m := getJSON[frameMapDTO](t, s, "/api/frames")

	if m.Size <= 0 {
		t.Fatalf("size %d", m.Size)
	}
	if len(m.Segments) < 2 {
		t.Fatalf("got %d segments, the example file has several", len(m.Segments))
	}
	if m.Total != len(m.Frames) || m.Truncated {
		t.Fatalf("total %d, listed %d, truncated %v — the example is far under the cap",
			m.Total, len(m.Frames), m.Truncated)
	}

	// Every segment restates every schema. That is what makes a file cut
	// anywhere still decodable (rule 3) and what makes concatenation work
	// (SPEC §6.6); a segment missing one would break random access into it.
	want := m.Segments[0].Schemas
	if want == 0 {
		t.Fatal("first segment declares no schemas")
	}
	for _, seg := range m.Segments {
		if seg.Schemas != want {
			t.Errorf("segment %d declares %d schemas, segment 0 declares %d",
				seg.Index, seg.Schemas, want)
		}
		if seg.End <= seg.Offset {
			t.Errorf("segment %d spans %d..%d", seg.Index, seg.Offset, seg.End)
		}
	}
}

// TestFramesDoNotOverlap is the property the whole random-access scheme rests
// on: a DATA frame is a byte range that can be handed to the reader on its own.
// Overlapping or out-of-order ranges would mean the index is wrong, and the
// synthesized-prefix decode would produce silently wrong values rather than an
// error.
func TestFramesDoNotOverlap(t *testing.T) {
	s := serve(t)
	m := getJSON[frameMapDTO](t, s, "/api/frames")

	if len(m.Frames) < 2 {
		t.Fatalf("only %d frames", len(m.Frames))
	}
	for i, f := range m.Frames {
		if f.Size == 0 {
			t.Errorf("frame %d at %d has zero size", i, f.Offset)
		}
		if f.Records == 0 {
			t.Errorf("frame %d at %d holds no records", i, f.Offset)
		}
		if f.Stream == "" {
			t.Errorf("frame %d at %d belongs to no known stream", i, f.Offset)
		}
		if uint64(m.Size) < f.Offset+f.Size {
			t.Errorf("frame %d runs to %d, past the %d-byte file", i, f.Offset+f.Size, m.Size)
		}
		if i > 0 {
			prev := m.Frames[i-1]
			if f.Offset < prev.Offset+prev.Size {
				t.Errorf("frame %d at %d overlaps the previous frame ending at %d",
					i, f.Offset, prev.Offset+prev.Size)
			}
		}
	}
}

// TestFramesRecordsMatchStreams checks the map against /api/file: the frames
// attributed to a stream must account for exactly the records that stream
// reports, or one of the two is lying about what the file holds.
func TestFramesRecordsMatchStreams(t *testing.T) {
	s := serve(t)
	m := getJSON[frameMapDTO](t, s, "/api/frames")
	f := getJSON[fileDTO](t, s, "/api/file")

	perStream := map[string]int{}
	for _, fr := range m.Frames {
		perStream[fr.UUID] += int(fr.Records)
	}
	for _, st := range f.Streams {
		if perStream[st.UUID] != st.Records {
			t.Errorf("%s: frames hold %d records, the stream reports %d",
				st.Name, perStream[st.UUID], st.Records)
		}
	}

	// The per-segment totals are computed over every frame, not just the
	// listed ones, so they must agree here where nothing was capped.
	segTotal := 0
	for _, seg := range m.Segments {
		segTotal += seg.Records
	}
	all := 0
	for _, fr := range m.Frames {
		all += int(fr.Records)
	}
	if segTotal != all {
		t.Errorf("segment totals sum to %d, frames to %d", segTotal, all)
	}
}

// TestFramesLimitIsHonest checks that a capped listing says so, and that the
// per-segment summary still covers the whole file.
func TestFramesLimitIsHonest(t *testing.T) {
	s := serve(t)
	full := getJSON[frameMapDTO](t, s, "/api/frames")
	if full.Total < 3 {
		t.Skipf("need at least 3 frames, have %d", full.Total)
	}

	m := getJSON[frameMapDTO](t, s, "/api/frames?limit=2")
	if len(m.Frames) != 2 {
		t.Fatalf("limit=2 listed %d frames", len(m.Frames))
	}
	if !m.Truncated {
		t.Error("a capped listing did not say it was capped")
	}
	if m.Total != full.Total {
		t.Errorf("total %d under a cap, %d without one", m.Total, full.Total)
	}

	capped, whole := 0, 0
	for _, seg := range m.Segments {
		capped += seg.Records
	}
	for _, seg := range full.Segments {
		whole += seg.Records
	}
	if capped != whole {
		t.Errorf("segment totals fell from %d to %d when the listing was capped; "+
			"the summary must cover every frame", whole, capped)
	}
}
