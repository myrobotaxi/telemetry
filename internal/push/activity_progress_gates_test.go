package push

import (
	"math"
	"testing"
	"time"
)

// The two gates and the one re-anchor rule that a wrong reading gets past
// (MYR-398 review). Each test here is a sequence rather than a single call,
// because all three defects are invisible one push at a time — they are
// properties of what the phone is shown OVER a leg.

// TestETASourceDoesNotInflateOnTraffic walks the exact sequence the ETA
// re-anchor bug produced, and is the regression test for it.
//
// The reading is minutes, so a value ABOVE the baseline is traffic, not a
// longer road. Re-anchoring on it silently redefines a 12-minute leg as a
// 40-minute one, and because every later fraction is then larger than the last,
// the monotone clamp waves the inflation through: the car ends up rendered at
// 0.85 when it is six minutes from a leg that started at twelve — roughly half
// way. Nothing downstream can catch that, which is why it is pinned here.
func TestETASourceDoesNotInflateOnTraffic(t *testing.T) {
	steps := []struct {
		minutes int
		want    float64
		note    string
	}{
		{12, 0, "the leg opens at its baseline"},
		{6, 0.5, "half the minutes gone is half the track"},
		{20, 0.5, "traffic: held by the clamp, and the baseline must not move"},
		{8, 0.5, "still worse than half way; the floor holds it"},
		{6, 0.5, "back where it was — and it must still read half way"},
	}

	var anchor ProgressAnchor
	for i, step := range steps {
		got, next := computeProgress(legRC(statusEnroute, nil, intPtr(step.minutes)), anchor, fixedNow)
		if got == nil {
			t.Fatalf("step %d (%d min): progress omitted — %s", i, step.minutes, step.note)
		}
		if math.Abs(*got-step.want) > 1e-9 {
			t.Errorf("step %d (%d min): progress = %s, want %v — %s",
				i, step.minutes, fmtPtr(got), step.want, step.note)
		}
		if next.Baseline != 12 {
			t.Errorf("step %d (%d min): baseline = %v, want the leg's original 12 — a rescaled ETA baseline is a permanently inflated track",
				i, step.minutes, next.Baseline)
		}
		anchor = next
	}
}

// TestDormantReservationOpensNoTrackUntilDispatch is the dispatch gate end to
// end: the owner's errand must leave nothing behind, and the real leg must open
// at the start of the track rather than wherever the errand ended.
func TestDormantReservationOpensNoTrackUntilDispatch(t *testing.T) {
	var anchor ProgressAnchor

	// The owner accepts tomorrow's reservation and drives to the shops. The
	// ride reads `accepted` throughout, so nothing but the dormancy gate
	// distinguishes this from a car on its way to the rider.
	for _, remaining := range []float64{5, 3, 1, 0} {
		got, next := computeProgress(dormantRC(miles(remaining), intPtr(9)), anchor, fixedNow)
		if got != nil {
			t.Fatalf("a dormant reservation sent progress %s for a car %v miles into the OWNER's errand",
				fmtPtr(got), remaining)
		}
		if next != (ProgressAnchor{}) {
			t.Fatalf("a dormant reservation stored anchor %+v; it would become the real leg's floor", next)
		}
		anchor = next
	}

	// The sweeper dispatches it and the car sets off for the rider, twenty
	// miles away. The track must open at zero, not at the errand's 0.99.
	got, next := computeProgress(legRC(statusAccepted, miles(20), intPtr(25)), anchor, fixedNow)
	if got == nil || *got != 0 {
		t.Fatalf("the dispatched pickup leg opened at %s, want 0", fmtPtr(got))
	}
	if next.Leg != ProgressLegPickup || next.Baseline != 20 {
		t.Errorf("dispatched anchor = %+v, want the pickup leg baselined at 20", next)
	}
}

// pollerRC is a leg whose ROW STAMP is always fresh — the state MYR-394's
// active-ride position poller leaves behind, writing position every ~25s
// without ever touching etaMinutes or tripDistanceRemaining — carrying whatever
// nav reading the car last actually said.
func pollerRC(dist *float64, now time.Time) RideContext {
	fresh := now.Add(-5 * time.Second)
	return RideContext{
		Status:             statusEnroute,
		TripMilesRemaining: dist,
		NavUpdatedAt:       &fresh,
		DispatchUnderway:   true,
	}
}

// TestFrozenNavReadingStopsAdvancingTheTrack pins the honest half of the
// freshness gate — the half that survives a writer keeping the car's row fresh
// while its navigation says nothing new.
//
// Every call below presents a fresh NavUpdatedAt, so navFresh passes every
// time. What must stop the track is the READING's own age.
func TestFrozenNavReadingStopsAdvancingTheTrack(t *testing.T) {
	// A first, trustworthy reading opens the leg and dates itself.
	got, anchor := computeProgress(pollerRC(miles(10), fixedNow), ProgressAnchor{}, fixedNow)
	if got == nil || *got != 0 {
		t.Fatalf("opening progress = %s, want 0", fmtPtr(got))
	}
	if anchor.Reading != 10 || !anchor.ReadingAt.Equal(fixedNow) {
		t.Fatalf("anchor reading = %v at %v, want 10 at %v", anchor.Reading, anchor.ReadingAt, fixedNow)
	}

	// The car moves; the reading changes and the stamp moves with it.
	later := fixedNow.Add(time.Minute)
	got, anchor = computeProgress(pollerRC(miles(4), later), anchor, later)
	if got == nil || *got != 0.6 {
		t.Fatalf("progress after a real six miles = %s, want 0.6", fmtPtr(got))
	}
	if !anchor.ReadingAt.Equal(later) {
		t.Errorf("reading stamp = %v, want it to move with the reading to %v", anchor.ReadingAt, later)
	}

	// Now nav goes quiet: the SAME four miles, still behind a fresh row stamp.
	// Inside the horizon it is simply the current reading and still derives.
	atHorizon := later.Add(ProgressFreshFor)
	got, held := computeProgress(pollerRC(miles(4), atHorizon), anchor, atHorizon)
	if got == nil || *got != 0.6 {
		t.Fatalf("progress at the horizon = %s, want 0.6", fmtPtr(got))
	}
	if !held.ReadingAt.Equal(later) {
		t.Errorf("an unchanged reading moved its own stamp to %v; the gate would never fire", held.ReadingAt)
	}

	// Past the horizon the reading is stale however fresh the row is, so the
	// server HOLDS rather than deriving — and it must not re-date the reading,
	// or the next push would treat it as new again.
	past := later.Add(ProgressFreshFor + time.Second)
	got, after := computeProgress(pollerRC(miles(4), past), anchor, past)
	if got == nil || *got != 0.6 {
		t.Fatalf("a frozen reading past the horizon = %s, want the held 0.6", fmtPtr(got))
	}
	if !after.ReadingAt.Equal(later) {
		t.Errorf("the held push re-dated the reading to %v, want it left at %v", after.ReadingAt, later)
	}

	// A car that starts talking again re-dates and advances immediately.
	got, resumed := computeProgress(pollerRC(miles(2), past), after, past)
	if got == nil || *got != 0.8 {
		t.Fatalf("progress after nav resumed = %s, want 0.8", fmtPtr(got))
	}
	if !resumed.ReadingAt.Equal(past) {
		t.Errorf("resumed reading stamp = %v, want %v", resumed.ReadingAt, past)
	}
}
