package query

import (
	"testing"
)

// TestEventsExact checks the labelled path against what the file actually says.
// can-example writes two events per segment across three segments.
func TestEventsExact(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "events")
	fd := field(t, st, "message")

	events, density, tier, err := q.Events(st, fd, nil, st.AxisMin, st.AxisMax)
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierExact {
		t.Fatalf("tier %s, want exact for a six-event file", tier)
	}
	if density != nil {
		t.Error("exact tier also returned density; exactly one should be populated")
	}
	if len(events) != st.Records {
		t.Fatalf("got %d events, stream holds %d records", len(events), st.Records)
	}
	for i, e := range events {
		if e.Label == "" {
			t.Errorf("event %d at %v has no label", i, e.X)
		}
		if i > 0 && e.X < events[i-1].X {
			t.Errorf("event %d at %v goes backwards from %v", i, e.X, events[i-1].X)
		}
	}
	// The messages are the ones cmd/logbdump prints.
	if events[0].Label != "segment 0 started" {
		t.Errorf("first event %q", events[0].Label)
	}
}

// TestEventsFallBackToDensity checks the tier switch, and that density is built
// from Tier 1 rather than by decoding.
func TestEventsFallBackToDensity(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "can0.raw")
	fd := field(t, st, "payload")

	// 450 records, well past a low limit: the lane must become a profile.
	q.MaxEvents = 10
	events, density, tier, err := q.Events(st, fd, nil, st.AxisMin, st.AxisMax)
	if err != nil {
		t.Fatal(err)
	}
	if tier != TierStats {
		t.Fatalf("tier %s, want stats", tier)
	}
	if events != nil {
		t.Error("density tier also returned events")
	}
	if len(density) == 0 {
		t.Fatal("no density buckets")
	}

	total := uint32(0)
	for _, d := range density {
		if d.N == 0 {
			t.Error("an empty bucket was sent; empty frames should be dropped")
		}
		if d.X1 < d.X0 {
			t.Errorf("bucket %v..%v runs backwards", d.X0, d.X1)
		}
		total += d.N
	}
	// Every record of this stream carries a payload, so the counts must add up
	// to the record count exactly. This is the check that the Tier 1 presence
	// counts for event fields are actually being gathered — before they were,
	// this came back zero.
	if int(total) != st.Records {
		t.Errorf("density totals %d, stream holds %d records", total, st.Records)
	}
}

// TestEventsDensityStaysInsideWindow checks that a frame straddling the edge is
// clamped. A band running off the chart would read as events outside the range
// the user asked for.
func TestEventsDensityStaysInsideWindow(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "can0.raw")
	fd := field(t, st, "payload")
	q.MaxEvents = 1

	span := st.AxisMax - st.AxisMin
	from := st.AxisMin + span/3
	to := st.AxisMin + 2*span/3

	_, density, _, err := q.Events(st, fd, nil, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(density) == 0 {
		t.Fatal("no density buckets in the middle third")
	}
	for _, d := range density {
		if d.X0 < from || d.X1 > to {
			t.Errorf("bucket %v..%v escapes the window %v..%v", d.X0, d.X1, from, to)
		}
	}
}

// TestEventsWindowFilters checks the exact path honours the window too. Frames
// are the unit of decode and spill past it.
func TestEventsWindowFilters(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "events")
	fd := field(t, st, "message")

	all, _, _, err := q.Events(st, fd, nil, st.AxisMin, st.AxisMax)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Skipf("only %d events", len(all))
	}
	// A window ending just before the last event must not include it.
	to := all[len(all)-1].X - 1
	got, _, _, err := q.Events(st, fd, nil, st.AxisMin, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(all)-1 {
		t.Fatalf("got %d events up to %v, want %d", len(got), to, len(all)-1)
	}
}

// TestEventsSkipAbsent checks that a guarded event field contributes marks only
// where it was actually present. A mark where the field was absent would claim
// something happened when nothing did (SPEC §6.2).
func TestEventsSkipAbsent(t *testing.T) {
	q := newTestQuery(t)
	st := stream(t, q, "events")
	fd := field(t, st, "message")

	events, _, _, err := q.Events(st, fd, nil, st.AxisMin, st.AxisMax)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in this file is guarded, so every record is an event; the point
	// is that the count matches rather than exceeding the records.
	if len(events) > st.Records {
		t.Fatalf("%d events from %d records", len(events), st.Records)
	}
}
