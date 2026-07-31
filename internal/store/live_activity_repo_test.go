package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"

	"errors"
)

// MYR-172 — go_live_activities repository behaviour against a real database.

func newLiveActivityRepo(t *testing.T) *store.LiveActivityRepo {
	t.Helper()
	return store.NewLiveActivityRepo(testPool, testLogger())
}

// setupLiveActivities prepares a clean table plus one seeded ride.
func setupLiveActivities(t *testing.T, rideID string) *store.LiveActivityRepo {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, rideID)
	return newLiveActivityRepo(t)
}

// TestLiveActivityRepo_RegisterIsAnUpsertThatRotatesTheToken is the central
// behaviour: ActivityKit hands the app a NEW update token partway through a
// single Activity and expects the server to switch to it, so re-registering
// must replace the value in place rather than add a row.
func TestLiveActivityRepo_RegisterIsAnUpsertThatRotatesTheToken(t *testing.T) {
	const ride = "cride0025r1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-first", false); err != nil {
		t.Fatalf("first RegisterActivity: %v", err)
	}
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-rotated", true); err != nil {
		t.Fatalf("rotating RegisterActivity: %v", err)
	}

	got, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows after a rotation, want 1 — the token is not the key", len(got))
	}
	if got[0].ActivityPushToken != "token-rotated" {
		t.Errorf("token = %q, want the rotated value", got[0].ActivityPushToken)
	}
	// sandbox is refreshed too: a device can move between a TestFlight and an
	// App Store build, and sending to the wrong gateway is a hard rejection.
	if !got[0].Sandbox {
		t.Error("sandbox was not refreshed by the rotation")
	}
}

// TestLiveActivityRepo_RegisterClearsTheEndTombstone — a client that
// re-registers is telling us it has a live Activity again. Leaving the
// tombstone would silently exclude the row from every send path, and the rider
// would watch a frozen lock screen with nothing in the logs to explain it.
func TestLiveActivityRepo_RegisterClearsTheEndTombstone(t *testing.T) {
	const ride = "cride0025r2"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	ended, err := repo.EndActivity(ctx, ride, "rider-1")
	if err != nil || !ended {
		t.Fatalf("EndActivity = %v, %v; want true, nil", ended, err)
	}
	if got, _ := repo.ActivitiesForRide(ctx, ride); len(got) != 0 {
		t.Fatalf("an ended Activity is still listed as live (%d rows)", len(got))
	}

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-b", false); err != nil {
		t.Fatalf("re-RegisterActivity: %v", err)
	}
	got, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("re-registration did not revive the row (%d live rows)", len(got))
	}
	if got[0].ActivityPushToken != "token-b" {
		t.Errorf("token = %q, want token-b", got[0].ActivityPushToken)
	}
}

// TestLiveActivityRepo_EndIsScopedAndIdempotent.
func TestLiveActivityRepo_EndIsScopedAndIdempotent(t *testing.T) {
	const ride = "cride0025r3"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}

	// Another party's end must not touch this row.
	ended, err := repo.EndActivity(ctx, ride, "someone-else")
	if err != nil {
		t.Fatalf("EndActivity(other): %v", err)
	}
	if ended {
		t.Error("ending as a different party closed somebody else's Activity")
	}

	if ended, err = repo.EndActivity(ctx, ride, "rider-1"); err != nil || !ended {
		t.Fatalf("EndActivity(owner) = %v, %v; want true, nil", ended, err)
	}
	// Second end: idempotent, reports false rather than failing.
	if ended, err = repo.EndActivity(ctx, ride, "rider-1"); err != nil || ended {
		t.Errorf("second EndActivity = %v, %v; want false, nil", ended, err)
	}
}

// TestLiveActivityRepo_EndActivitiesForRideClosesEveryParty is what a terminal
// status uses: the ride is over for everybody at once.
func TestLiveActivityRepo_EndActivitiesForRideClosesEveryParty(t *testing.T) {
	const ride = "cride0025r4"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	for _, user := range []string{"rider-1", "owner-1"} {
		if err := repo.RegisterActivity(ctx, ride, user, "token-"+user, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", user, err)
		}
	}

	n, err := repo.EndActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("EndActivitiesForRide: %v", err)
	}
	if n != 2 {
		t.Errorf("closed %d Activities, want 2", n)
	}
	if got, _ := repo.ActivitiesForRide(ctx, ride); len(got) != 0 {
		t.Errorf("%d Activities still live after the ride ended", len(got))
	}
}

// TestLiveActivityRepo_DeleteActivityTokenIsNotCallerScoped — an APNs 410 is a
// verdict about the Activity, not about the person who registered it.
func TestLiveActivityRepo_DeleteActivityTokenIsNotCallerScoped(t *testing.T) {
	const ride = "cride0025r5"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-rejected", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	if err := repo.DeleteActivityToken(ctx, "token-rejected"); err != nil {
		t.Fatalf("DeleteActivityToken: %v", err)
	}
	if got, _ := repo.ActivitiesForRide(ctx, ride); len(got) != 0 {
		t.Errorf("%d rows survived the rejected-token delete", len(got))
	}
}

// TestLiveActivityRepo_ActiveLegsSelectsOnlyLiveMidRideActivities.
func TestLiveActivityRepo_ActiveLegsSelectsOnlyLiveMidRideActivities(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	ctx := context.Background()
	repo := newLiveActivityRepo(t)

	// One ride per active-leg status, plus a completed one and an ended row.
	cases := []struct {
		ride   string
		status string
		want   bool
	}{
		{"cride0025L1", "accepted", true},
		{"cride0025L2", "arrived", true},
		{"cride0025L3", "enroute", true},
		{"cride0025L4", "completed", false},
		{"cride0025L5", "requested", false},
	}
	for _, c := range cases {
		seedActivityRide(t, c.ride)
		if _, err := testPool.Exec(ctx,
			`UPDATE go_ride_requests SET status = $2 WHERE id = $1`, c.ride, c.status); err != nil {
			t.Fatalf("set status %s: %v", c.status, err)
		}
		if err := repo.RegisterActivity(ctx, c.ride, "rider-"+c.ride, "token-"+c.ride, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", c.ride, err)
		}
	}
	// An ended Activity on an otherwise-active ride must not be listed.
	seedActivityRide(t, "cride0025L6")
	if err := repo.RegisterActivity(ctx, "cride0025L6", "rider-6", "token-6", false); err != nil {
		t.Fatalf("RegisterActivity(L6): %v", err)
	}
	if _, err := repo.EndActivity(ctx, "cride0025L6", "rider-6"); err != nil {
		t.Fatalf("EndActivity(L6): %v", err)
	}

	legs, err := repo.ListActiveLegActivities(ctx, 100)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}

	seen := map[string]bool{}
	for _, leg := range legs {
		seen[leg.RideRequestID] = true
	}
	for _, c := range cases {
		if seen[c.ride] != c.want {
			t.Errorf("ride %s (status %s) listed = %v, want %v", c.ride, c.status, seen[c.ride], c.want)
		}
	}
	if seen["cride0025L6"] {
		t.Error("an ENDED Activity was listed as an active leg")
	}

	// The projection must carry the destination label the content-state needs.
	for _, leg := range legs {
		if leg.DropoffLabel != "Home" {
			t.Errorf("ride %s dropoff label = %q, want Home", leg.RideRequestID, leg.DropoffLabel)
		}
		// No "Vehicle" row exists for these fixtures, so the LEFT JOIN must
		// degrade rather than drop the leg: the ride is still the rider's
		// reality and the Activity still needs its timestamp refreshed.
		if leg.ETAMinutes != nil {
			t.Errorf("ride %s carried an ETA with no vehicle row: %v", leg.RideRequestID, *leg.ETAMinutes)
		}
	}
}

// TestLiveActivityRepo_ActiveLegsRespectsTheLimit.
func TestLiveActivityRepo_ActiveLegsRespectsTheLimit(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	ctx := context.Background()
	repo := newLiveActivityRepo(t)

	for _, ride := range []string{"cride0025M1", "cride0025M2", "cride0025M3"} {
		seedActivityRide(t, ride)
		if err := repo.RegisterActivity(ctx, ride, "rider-"+ride, "token-"+ride, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", ride, err)
		}
	}

	legs, err := repo.ListActiveLegActivities(ctx, 2)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 2 {
		t.Errorf("got %d legs with limit 2, want 2", len(legs))
	}
	if _, err := repo.ListActiveLegActivities(ctx, 0); err == nil {
		t.Error("a non-positive limit was accepted")
	}
}

// TestLiveActivityRepo_SweepKeysOffUpdatedAtNotEndedAt is the cleanup's whole
// point: the rows most worth reaping are the ones that NEVER ended, because the
// Activity died on the phone. An ended-only sweep would leak exactly those.
func TestLiveActivityRepo_SweepKeysOffUpdatedAtNotEndedAt(t *testing.T) {
	const ride = "cride0025s1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-stale", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	// Age the row past the horizon WITHOUT ending it.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_live_activities SET updated_at = NOW() - INTERVAL '48 hours' WHERE ride_request_id = $1`,
		ride); err != nil {
		t.Fatalf("age row: %v", err)
	}

	removed, err := repo.SweepStaleActivities(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleActivities: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d rows, want 1 — a never-ended row past the horizon must be reaped", removed)
	}

	// A fresh row survives.
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-fresh", false); err != nil {
		t.Fatalf("re-RegisterActivity: %v", err)
	}
	if removed, err = repo.SweepStaleActivities(ctx, 24*time.Hour); err != nil || removed != 0 {
		t.Errorf("sweep removed %d fresh rows (err %v), want 0", removed, err)
	}
	if _, err := repo.SweepStaleActivities(ctx, 0); err == nil {
		t.Error("a non-positive sweep age was accepted")
	}
}

// TestLiveActivityRepo_ActivityContextForRide covers the content-state inputs
// and the not-found path a terminal send races with (owner teardown
// hard-deletes rides).
func TestLiveActivityRepo_ActivityContextForRide(t *testing.T) {
	const ride = "cride0025c1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	got, err := repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if got.Status != store.RideRequestStatusAccepted {
		t.Errorf("status = %q, want accepted", got.Status)
	}
	if got.DropoffLabel != "Home" {
		t.Errorf("dropoff label = %q, want Home", got.DropoffLabel)
	}

	_, err = repo.ActivityContextForRide(ctx, "cride-does-not-exist")
	if !errors.Is(err, sdk.ErrNotFound) {
		t.Errorf("missing ride error = %v, want it to wrap sdk.ErrNotFound", err)
	}
}

// TestLiveActivityRepo_RejectsEmptyIdentifiers pins the guards, and that the
// P1 token's VALUE never reaches an error string.
func TestLiveActivityRepo_RejectsEmptyIdentifiers(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newLiveActivityRepo(t)

	if err := repo.RegisterActivity(ctx, "", "rider", "tok", false); err == nil {
		t.Error("an empty ride id was accepted")
	}
	if err := repo.RegisterActivity(ctx, "ride", "", "tok", false); err == nil {
		t.Error("an empty user id was accepted")
	}
	err := repo.RegisterActivity(ctx, "ride", "rider", "", false)
	if err == nil {
		t.Fatal("an empty token was accepted")
	}
	if _, endErr := repo.EndActivity(ctx, "", "rider"); endErr == nil {
		t.Error("an empty ride id was accepted by EndActivity")
	}
	if delErr := repo.DeleteActivityToken(ctx, ""); delErr == nil {
		t.Error("an empty token was accepted by DeleteActivityToken")
	}
}
