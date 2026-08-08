package push

import (
	"testing"
	"time"
)

// MYR-409 — the two questions the Arriving rung has to answer, and the proof
// that neither answer lies in either direction.
//
// Freshness ("is this reading recent") is navFresh, now keyed on the nav
// reading rather than on the car's row. Relevance ("is this reading about US")
// is navReadingIsOurs, keyed on the leg's dispatch instant. The tests below are
// built as FRAME SEQUENCES rather than as single states, because both failure
// modes this issue cares about are things that happen over time: a rung burnt
// by a leftover value, and a gate that flickers off and on across ticks.

// navSeqContext builds the pickup-leg context a sequence step presents.
func navSeqContext(eta int, readingAt, dispatchedAt *time.Time) RideContext {
	return RideContext{
		Status:             statusAccepted,
		ETAMinutes:         intPtr(eta),
		NavUpdatedAt:       readingAt,
		PickupDispatchedAt: dispatchedAt,
		DispatchUnderway:   true,
	}
}

// TestNavRelevance_LeftoverReadingCannotBurnTheRung walks the exact production
// sequence MYR-409 was filed for, one frame at a time.
//
// The car is its owner's, mid-errand, streaming continuously. A rider's instant
// ride is accepted. Before this issue, EVERY step below resolved Arriving:
// `lastUpdated` was refreshed by the motion frames, and `minutesToArrival` kept
// the small value its finished errand had left behind. The rung burnt on step
// one and the rider never heard from the card again until the car turned up.
func TestNavRelevance_LeftoverReadingCannotBurnTheRung(t *testing.T) {
	// The owner's errand ended here; nothing has updated navigation since.
	errandReading := fixedNow.Add(-9 * time.Minute)
	// The rider's ride is accepted and the pickup is pushed to the car now.
	dispatch := fixedNow

	steps := []struct {
		name  string
		after time.Duration
		want  AlertPhase
	}{
		// The accept push itself. The car is streaming — a motion frame lands
		// every second — but no nav frame has arrived since the dispatch.
		{"accept push", 0, AlertPhaseEnroute},
		// Thirty seconds of motion frames later. The row stamp would be half a
		// second old here; the NAV stamp is still nine and a half minutes old.
		{"30s of motion frames", 30 * time.Second, AlertPhaseEnroute},
		// Two minutes of it. Still Enroute, still honest.
		{"2m of motion frames", 2 * time.Minute, AlertPhaseEnroute},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			now := fixedNow.Add(st.after)
			rc := navSeqContext(1, &errandReading, &dispatch)
			if got := alertPhaseFor(rc, now); got != st.want {
				t.Fatalf("phase = %d, want %d — a %v-old nav reading from the owner's "+
					"errand resolved the rider's Arriving rung",
					got, st.want, now.Sub(errandReading).Round(time.Second))
			}
		})
	}
}

// TestNavRelevance_FreshErrandReadingIsStillNotOurs is the arm a per-reading
// stamp alone leaves wide open, and the reason navReadingIsOurs exists.
//
// Here the owner's car is ACTIVELY NAVIGATING at the moment of accept. Its nav
// reading is one second old — as fresh as a reading can be — and says two
// minutes, because the owner is two minutes from the shops. navFresh passes.
// Only the dispatch instant can tell that those two minutes are somebody else's.
func TestNavRelevance_FreshErrandReadingIsStillNotOurs(t *testing.T) {
	dispatch := fixedNow
	// One second old at the moment of accept, and PREDATING the dispatch by
	// exactly that second.
	errandReading := fixedNow.Add(-time.Second)

	rc := navSeqContext(2, &errandReading, &dispatch)
	if !navFresh(rc, fixedNow) {
		t.Fatal("precondition: the errand reading must be FRESH, or this test is " +
			"proving the freshness gate rather than the relevance gate")
	}
	if got := alertPhaseFor(rc, fixedNow); got != AlertPhaseEnroute {
		t.Fatalf("phase = %d, want %d — a one-second-old reading about the owner's "+
			"errand burnt the rider's Arriving rung", got, AlertPhaseEnroute)
	}
}

// TestNavRelevance_RealArrivingStillFires is the control, and it is the more
// important half of this file.
//
// A gate that refuses everything passes every test above and destroys the
// feature: the rider is never told their car is close. The sequence here is the
// happy path — dispatch, then the car's own navigation streaming back, then the
// threshold crossing — and Arriving must be reached at the crossing and not
// before.
func TestNavRelevance_RealArrivingStillFires(t *testing.T) {
	dispatch := fixedNow

	steps := []struct {
		name        string
		after       time.Duration
		eta         int
		readingLag  time.Duration
		want        AlertPhase
		explanation string
	}{
		{
			"accept: no post-dispatch reading yet", 0, 12, 0, AlertPhaseEnroute,
			"nothing has come back from the car",
		},
		{
			"the car's route streams back", 20 * time.Second, 12, 2 * time.Second, AlertPhaseEnroute,
			"a real reading, ours, but twelve minutes out",
		},
		{
			"ten minutes of driving", 10 * time.Minute, ArrivingWithinMinutes + 1, time.Second, AlertPhaseEnroute,
			"closing, one minute above the threshold",
		},
		{
			"the threshold crossing", 11 * time.Minute, ArrivingWithinMinutes, time.Second, AlertPhaseArriving,
			"THE ONE THAT MUST FIRE",
		},
		{
			"and it stays true for the rest of the leg", 12 * time.Minute, 1, time.Second, AlertPhaseArriving,
			"the once-per-Activity guarantee is the ladder's job, not this gate's",
		},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			now := fixedNow.Add(st.after)
			reading := now.Add(-st.readingLag)
			rc := navSeqContext(st.eta, &reading, &dispatch)
			if got := alertPhaseFor(rc, now); got != st.want {
				t.Fatalf("phase = %d, want %d (%s)", got, st.want, st.explanation)
			}
		})
	}
}

// TestNavRelevance_DoesNotFlickerAcrossTicks is the failure mode MYR-485 names
// as the thing a client gate must never be shown.
//
// A gate that oscillates is worse than one that is wrong in a fixed direction:
// the rider watches the card change its mind. Both inputs move monotonically
// forward during a leg — the nav stamp is written GREATEST(new, stored), and a
// dispatch instant is stamped once and never re-stamped — so once relevance is
// true it must STAY true for the rest of the leg, including across the 30-second
// nav resends a car in stopped traffic emits.
func TestNavRelevance_DoesNotFlickerAcrossTicks(t *testing.T) {
	dispatch := fixedNow
	// Twenty ticks at the ticker's ~75s cadence. The car is in traffic: the
	// reading advances by the 30s resend, not smoothly, and the ETA wobbles.
	reading := dispatch.Add(5 * time.Second)
	etas := []int{2, 2, 1, 2, 1, 0, 1, 0, 1, 2, 1, 1, 0, 2, 1, 1, 1, 0, 1, 1}

	for i, eta := range etas {
		now := dispatch.Add(time.Duration(i+1) * 75 * time.Second)
		// The resend cadence: the stamp advances in 30s steps and never back.
		for !reading.Add(30 * time.Second).After(now) {
			reading = reading.Add(30 * time.Second)
		}
		rc := navSeqContext(eta, &reading, &dispatch)
		if !navReadingIsOurs(rc) {
			t.Fatalf("tick %d: relevance went FALSE after being true — the gate flickers, "+
				"and a client keyed on it would blank a live ETA mid-ride", i)
		}
		if got := alertPhaseFor(rc, now); got != AlertPhaseArriving {
			t.Fatalf("tick %d: phase = %d, want %d — eta %d is inside the threshold and the "+
				"reading is ours", i, got, AlertPhaseArriving, eta)
		}
	}
}

// TestNavRelevance_Boundaries pins the edges, because an off-by-one in either
// direction is a lie of exactly the kind this issue is about.
func TestNavRelevance_Boundaries(t *testing.T) {
	dispatch := fixedNow
	atDispatch := dispatch
	justBefore := dispatch.Add(-time.Nanosecond)
	justAfter := dispatch.Add(time.Nanosecond)

	tests := []struct {
		name     string
		reading  *time.Time
		dispatch *time.Time
		want     bool
	}{
		{
			// Equal counts as ours. A reading taken in the same instant as the
			// dispatch cannot be proven to predate it, and the tie-break that
			// matters is the one that does not silently withhold a real ETA.
			"reading exactly at dispatch is ours", &atDispatch, &dispatch, true,
		},
		{"one nanosecond before dispatch is not ours", &justBefore, &dispatch, false},
		{"one nanosecond after dispatch is ours", &justAfter, &dispatch, true},
		{
			// Never dispatched: failed, skipped, or a reservation still asleep.
			// The car was never sent to this rider at all.
			"no dispatch instant is not ours", &justAfter, nil, false,
		},
		{
			// No navigation frame has ever been seen for this car.
			"no reading is not ours", nil, &dispatch, false,
		},
		{"neither is not ours", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := RideContext{NavUpdatedAt: tt.reading, PickupDispatchedAt: tt.dispatch}
			if got := navReadingIsOurs(rc); got != tt.want {
				t.Errorf("navReadingIsOurs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNavRelevance_StaleAndIrrelevantAreIndependent proves the two gates are not
// each other in disguise. Each must be able to refuse on its own, or one of them
// is dead code that a later reader will delete.
func TestNavRelevance_StaleAndIrrelevantAreIndependent(t *testing.T) {
	dispatch := fixedNow.Add(-30 * time.Minute)
	freshAndOurs := fixedNow.Add(-10 * time.Second)
	staleButOurs := fixedNow.Add(-ProgressFreshFor - time.Second)
	freshButTheirs := fixedNow.Add(-10 * time.Second)
	laterDispatch := fixedNow.Add(-time.Second)

	tests := []struct {
		name         string
		reading      time.Time
		dispatch     time.Time
		wantFresh    bool
		wantOurs     bool
		wantArriving bool
	}{
		{"fresh and ours", freshAndOurs, dispatch, true, true, true},
		{"stale but ours", staleButOurs, dispatch, false, true, false},
		{"fresh but theirs", freshButTheirs, laterDispatch, true, false, false},
		{"stale and theirs", staleButOurs, laterDispatch, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := navSeqContext(1, &tt.reading, &tt.dispatch)
			if got := navFresh(rc, fixedNow); got != tt.wantFresh {
				t.Errorf("navFresh() = %v, want %v", got, tt.wantFresh)
			}
			if got := navReadingIsOurs(rc); got != tt.wantOurs {
				t.Errorf("navReadingIsOurs() = %v, want %v", got, tt.wantOurs)
			}
			want := AlertPhaseEnroute
			if tt.wantArriving {
				want = AlertPhaseArriving
			}
			if got := alertPhaseFor(rc, fixedNow); got != want {
				t.Errorf("alertPhaseFor() = %d, want %d", got, want)
			}
		})
	}
}
