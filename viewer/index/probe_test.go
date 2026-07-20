package index

import (
	"fmt"
	"testing"
)

// TestProbe is a scratch dump of the indexed model, kept because it is the
// fastest way to see what a file actually contains. Run with -v.
func TestProbe(t *testing.T) {
	fi, a := accessor(t)
	t.Logf("size=%d epoch=%d closed=%v truncated=%v unsupported=%v",
		fi.Size, fi.Epoch, fi.Closed, fi.Truncated, fi.Unsupported)
	for _, m := range fi.Meta {
		t.Logf("meta %s = %s", m.Key, m.Value)
	}
	for _, a := range fi.Attachments {
		t.Logf("attach %s (%d bytes)", a.Name, a.Size)
	}
	for _, s := range fi.Streams {
		t.Logf("stream %s uuid=%s axis=%s/%s exp=%d unit=%q records=%d runs=%d",
			s.Name, s.UUID[:8], s.AxisKind, s.AxisMode, s.AxisExp, s.AxisUnit, s.Records, len(s.Runs))
		for i := range s.Fields {
			f := &s.Fields[i]
			var ser *Series
			if !f.IsAxis && f.Class != ClassBlob && len(s.FrameList) > 0 {
				ser = fullSeries(t, fi, a, s, f, 0)
			}
			n, present := 0, 0
			var first string
			if ser != nil {
				n = ser.Len()
				for _, p := range ser.Present {
					if p {
						present++
					}
				}
				if n > 0 && ser.Present[0] {
					first = fmt.Sprintf(" first=%s@%g", f.Label(ser.Vals[0]), ser.Axis.At(0))
				}
			}
			t.Logf("   %-14s %-12s %-4s bit=%d+%d guarded=%v axis=%v n=%d present=%d%s",
				f.Name, f.Class, f.Type, f.BitOffset, f.BitWidth, f.Guarded, f.IsAxis, n, present, first)
		}
	}
}
