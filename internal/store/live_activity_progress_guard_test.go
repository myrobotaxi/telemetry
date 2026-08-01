package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The two things the anchor's SQL has to get right that no unit test on the
// arithmetic can see: the floor cannot be lowered by a losing replica, and the
// ticker's read can tell a dispatched pickup from a sleeping reservation
// (MYR-398 review).

// TestLiveActivityRepo_ProgressFloorRefusesToGoBackwards is the unsharded
// ticker's race, written down.
//
// Two replicas list the same Activity seconds apart and both read the same
// anchor. Whichever APNs round trip finishes LAST writes last — which can be
// the replica that computed the LOWER fraction, because it read the car a
// moment earlier. A last-write-wins UPDATE would then record a floor beneath
// what the phone has already been shown, and the next push where the clamp is
// load-bearing would deliver a decrease. The guard in the statement's WHERE is
// what makes the losing write a silent no-op instead.
func TestLiveActivityRepo_ProgressFloorRefusesToGoBackwards(t *testing.T) {
	const ride = "cride0025q4"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	const rider = "crider0398d"
	if err := repo.RegisterActivity(ctx, ride, rider, "token-floor", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	key := store.LiveActivityKey{RideRequestID: ride, UserID: rider}

	// Replica B's faster round trip lands first, delivering the higher value.
	high := store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 10, Value: 0.66}
	if err := repo.SaveActivityProgress(ctx, key, high); err != nil {
		t.Fatalf("SaveActivityProgress(high): %v", err)
	}

	// Replica A's slower round trip returns two seconds later with the value it
	// computed BEFORE B's, and must not win.
	low := store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 10, Value: 0.65}
	if err := repo.SaveActivityProgress(ctx, key, low); err != nil {
		t.Fatalf("SaveActivityProgress(low): %v", err)
	}

	if got := readAnchor(t, ride); got.Value != 0.66 {
		t.Errorf("floor = %v after a slower replica wrote 0.65 over 0.66, want 0.66 — "+
			"the floor must record what the phone HAS SEEN", got.Value)
	}

	// Equal is not a regression: two replicas agreeing must both be able to
	// record the reading pair that goes with the fraction.
	same := store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 10, Value: 0.66}
	if err := repo.SaveActivityProgress(ctx, key, same); err != nil {
		t.Fatalf("SaveActivityProgress(same): %v", err)
	}

	// A NEW LEG is a different measurement, not a lower fraction: it must land
	// unconditionally or leg two would inherit leg one's floor forever.
	legTwo := store.LiveActivityProgress{Leg: "dropoff", Source: "nav_distance", Baseline: 30, Value: 0}
	if err := repo.SaveActivityProgress(ctx, key, legTwo); err != nil {
		t.Fatalf("SaveActivityProgress(leg two): %v", err)
	}
	got := readAnchor(t, ride)
	if got.Leg != "dropoff" || got.Value != 0 {
		t.Errorf("anchor = %+v after a leg flip, want the dropoff leg opening at 0", got)
	}

	// And a CLEAR must land whatever the stored fraction is.
	if err := repo.SaveActivityProgress(ctx, key, store.LiveActivityProgress{}); err != nil {
		t.Fatalf("SaveActivityProgress(clear): %v", err)
	}
	if got := readAnchor(t, ride); got.Leg != "" {
		t.Errorf("anchor = %+v after a clear, want it erased", got)
	}
}

// readAnchor reads the four anchor columns straight from the row, so the
// assertion cannot be satisfied by the repo's own scan logic.
func readAnchor(t *testing.T, ride string) store.LiveActivityProgress {
	t.Helper()
	var leg, source *string
	var baseline, value *float64
	if err := testPool.QueryRow(context.Background(),
		`SELECT progress_leg, progress_source, progress_baseline, progress_value
		 FROM go_live_activities WHERE ride_request_id = $1`, ride,
	).Scan(&leg, &source, &baseline, &value); err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	var out store.LiveActivityProgress
	if leg != nil {
		out.Leg = *leg
	}
	if source != nil {
		out.Source = *source
	}
	if baseline != nil {
		out.Baseline = *baseline
	}
	if value != nil {
		out.Value = *value
	}
	return out
}

// dormancyFixtureRide is the one ride the dormancy case moves between the
// predicate's arms.
const dormancyFixtureRide = "cride0025q5"

// TestLiveActivityRepo_ActiveLegsReportsDispatchDormancy is the ticker's half of
// the dispatch gate: the read has to CARRY the fact, because the sender cannot
// derive it — the ride's status is `accepted` on both sides of the line.
//
// The predicate is MYR-376's, evaluated in SQL, and the NULL trap is the point
// of the COALESCE: an undispatched reservation has `dispatch_status` NULL, so
// the bare OR chain yields NULL rather than false.
func TestLiveActivityRepo_ActiveLegsReportsDispatchDormancy(t *testing.T) {
	repo := setupLiveActivities(t, dormancyFixtureRide)
	ctx := context.Background()

	const rider = "crider0398e"
	if err := repo.RegisterActivity(ctx, dormancyFixtureRide, rider, "token-dormant", false); err != nil {
		t.Fatalf("register: %v", err)
	}

	// An instant ride — no scheduled_for — is underway the moment it is
	// accepted, and must never be gated.
	if got := dispatchUnderwayFor(t, repo); !got {
		t.Error("an instant ride reads as dormant; its pickup track would never open")
	}

	// Tomorrow's reservation, accepted but never dispatched: DORMANT.
	setSchedule(t, time.Now().Add(20*time.Hour), nil)
	if got := dispatchUnderwayFor(t, repo); got {
		t.Error("an undispatched future reservation reads as underway — " +
			"the owner's own driving would anchor and advance the rider's pickup track")
	}

	// A dispatch that FAILED is still dormant before the due instant: §7.8's
	// recovery path is manual, and nothing about it says the car has set off.
	failed := "failed"
	setSchedule(t, time.Now().Add(20*time.Hour), &failed)
	if got := dispatchUnderwayFor(t, repo); got {
		t.Error("a failed pre-due dispatch reads as underway")
	}

	// The sweeper sends the leg-1 nav: the leg has started.
	sent := "sent"
	setSchedule(t, time.Now().Add(20*time.Hour), &sent)
	if got := dispatchUnderwayFor(t, repo); !got {
		t.Error("a dispatched reservation still reads as dormant; the track would never open")
	}

	// Past its due instant a reservation is underway however the dispatch went
	// — dormancy ends at the due instant, not at a successful dispatch.
	setSchedule(t, time.Now().Add(-time.Minute), nil)
	if got := dispatchUnderwayFor(t, repo); !got {
		t.Error("an overdue reservation reads as dormant; manual proceed is the documented path")
	}
}

// dispatchUnderwayFor reads the flag through BOTH send paths and fails if they
// disagree — the ticker and the lifecycle fan-out must gate identically or the
// track would appear on a status change and vanish on the next tick.
func dispatchUnderwayFor(t *testing.T, repo *store.LiveActivityRepo) bool {
	t.Helper()
	ctx := context.Background()

	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}

	rc, err := repo.ActivityContextForRide(ctx, dormancyFixtureRide)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if legs[0].DispatchUnderway != rc.DispatchUnderway {
		t.Fatalf("the ticker read %v and the lifecycle read %v; one gate, two answers",
			legs[0].DispatchUnderway, rc.DispatchUnderway)
	}
	return rc.DispatchUnderway
}

// setSchedule moves a fixture ride between the dormancy predicate's arms.
func setSchedule(t *testing.T, at time.Time, dispatchStatus *string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET scheduled_for = $2, dispatch_status = $3 WHERE id = $1`,
		dormancyFixtureRide, at, dispatchStatus); err != nil {
		t.Fatalf("set schedule on %s: %v", dormancyFixtureRide, err)
	}
}
