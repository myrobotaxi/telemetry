package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-398 — the leg-progress anchor's read and write paths.

// TestLiveActivityRepo_ProgressRoundTripsAndClears is the whole write contract:
// what the sender stored is what the next push reads, and an empty anchor
// erases the row's memory rather than leaving half a measurement behind.
func TestLiveActivityRepo_ProgressRoundTripsAndClears(t *testing.T) {
	const ride = "cride0025q1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	const rider = "crider0398a"
	if err := repo.RegisterActivity(ctx, ride, rider, "token-anchor", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	key := store.LiveActivityKey{RideRequestID: ride, UserID: rider}

	readingAt := time.Now().UTC().Truncate(time.Millisecond)
	want := store.LiveActivityProgress{
		Leg: "dropoff", Source: "eta", Baseline: 18, Value: 0.375,
		Reading: 11, ReadingAt: readingAt,
	}
	if err := repo.SaveActivityProgress(ctx, key, want); err != nil {
		t.Fatalf("SaveActivityProgress: %v", err)
	}
	rows, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("activities = %d, want 1", len(rows))
	}
	// Compared field by field because ReadingAt is a TIMESTAMPTZ round trip:
	// `==` on time.Time compares representations, not instants.
	got := rows[0].Progress
	if got.Leg != want.Leg || got.Source != want.Source ||
		got.Baseline != want.Baseline || got.Value != want.Value ||
		got.Reading != want.Reading || !got.ReadingAt.Equal(want.ReadingAt) {
		t.Fatalf("anchor = %+v, want %+v", got, want)
	}

	// A leg boundary clears it. Reading back a PARTIAL anchor would be worse
	// than reading back none: a baseline with no unit is un-interpretable.
	if err := repo.SaveActivityProgress(ctx, key, store.LiveActivityProgress{}); err != nil {
		t.Fatalf("SaveActivityProgress(clear): %v", err)
	}
	rows, err = repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide after clear: %v", err)
	}
	if len(rows) != 1 || rows[0].Progress != (store.LiveActivityProgress{}) {
		t.Errorf("anchor = %+v after a clear, want the zero value", rows[0].Progress)
	}
}

// TestLiveActivityRepo_ProgressWriteSkipsEndedRows keeps rendering state off a
// tombstoned Activity: the send and the ride's ending race by design, and a
// write that landed after the tombstone would resurrect nothing useful.
func TestLiveActivityRepo_ProgressWriteSkipsEndedRows(t *testing.T) {
	const ride = "cride0025q2"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	const rider = "crider0398b"
	if err := repo.RegisterActivity(ctx, ride, rider, "token-ended", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := repo.EndActivitiesForRide(ctx, ride); err != nil {
		t.Fatalf("end: %v", err)
	}

	key := store.LiveActivityKey{RideRequestID: ride, UserID: rider}
	// Not an error — the race is ordinary, and the caller has nothing to fix.
	if err := repo.SaveActivityProgress(ctx, key,
		store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 4, Value: 0.2}); err != nil {
		t.Fatalf("SaveActivityProgress on an ended row: %v", err)
	}

	var leg *string
	if err := testPool.QueryRow(ctx,
		`SELECT progress_leg FROM go_live_activities WHERE ride_request_id = $1`, ride,
	).Scan(&leg); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if leg != nil {
		t.Errorf("progress_leg = %q on a tombstoned Activity, want NULL", *leg)
	}
}

// TestLiveActivityRepo_ProgressRejectsEmptyIdentifiers mirrors the guard every
// other method on this repo carries.
func TestLiveActivityRepo_ProgressRejectsEmptyIdentifiers(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := newLiveActivityRepo(t)

	if err := repo.SaveActivityProgress(ctx,
		store.LiveActivityKey{UserID: "rider"}, store.LiveActivityProgress{}); err == nil {
		t.Error("an empty ride id was accepted")
	}
	if err := repo.SaveActivityProgress(ctx,
		store.LiveActivityKey{RideRequestID: "ride"}, store.LiveActivityProgress{}); err == nil {
		t.Error("an empty user id was accepted")
	}
}

// TestLiveActivityRepo_ActiveLegsCarriesTheNavInputs asserts the ticker's read
// hands the sender everything the fraction needs: the anchor, the car's
// remaining distance, and — the one that makes the honesty gate possible — how
// OLD that distance is.
func TestLiveActivityRepo_ActiveLegsCarriesTheNavInputs(t *testing.T) {
	const ride = "cride0025q3"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	const rider = "crider0398c"
	if err := repo.RegisterActivity(ctx, ride, rider, "token-legs", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	anchor := store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 9, Value: 0.25}
	if err := repo.SaveActivityProgress(ctx,
		store.LiveActivityKey{RideRequestID: ride, UserID: rider}, anchor); err != nil {
		t.Fatalf("SaveActivityProgress: %v", err)
	}
	seedActivityVehicle(t, ride, 7, 6.75)

	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}
	got := legs[0]
	if got.Progress != anchor {
		t.Errorf("anchor = %+v, want %+v", got.Progress, anchor)
	}
	if got.TripMilesRemaining == nil || *got.TripMilesRemaining != 6.75 {
		t.Errorf("tripMilesRemaining = %v, want 6.75", got.TripMilesRemaining)
	}
	if got.NavUpdatedAt == nil {
		t.Error("navUpdatedAt is nil; without it the sender cannot tell a fresh reading from a three-hour-old one")
	}

	// The single-ride read the lifecycle fan-out uses must agree with it.
	rc, err := repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if rc.TripMilesRemaining == nil || *rc.TripMilesRemaining != 6.75 {
		t.Errorf("lifecycle read tripMilesRemaining = %v, want 6.75", rc.TripMilesRemaining)
	}
	if rc.NavUpdatedAt == nil {
		t.Error("lifecycle read navUpdatedAt is nil")
	}
}

// seedActivityVehicle inserts the Prisma-owned "Vehicle" row the ride points
// at, carrying a nav reading. The id matches seedActivityRide's derivation.
//
// IT STAMPS nav_reading_at TOO, and the two writes belong together (MYR-409).
// In production a frame carrying etaMinutes / tripDistanceRemaining is by
// definition a navigation frame, so the writer stamps the side table in the
// same flush; a helper that wrote the readings without the stamp would be
// seeding a state the server cannot produce, and every test built on it would
// be asserting against fiction. Tests that want the UNSTAMPED case — a car
// streaming motion with navigation of unknown age, which is the defect state —
// seed the car row directly and deliberately, as migration_0034_test.go does.
func seedActivityVehicle(t *testing.T, rideID string, etaMinutes int, milesRemaining float64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "name", "etaMinutes", "tripDistanceRemaining", "lastUpdated")
		 VALUES ('veh_' || $1, 'cowner0025', 'VIN' || $1, 'Blue Whale', $2, $3, NOW())
		 ON CONFLICT ("id") DO UPDATE
		 SET "etaMinutes" = EXCLUDED."etaMinutes",
		     "tripDistanceRemaining" = EXCLUDED."tripDistanceRemaining",
		     "lastUpdated" = EXCLUDED."lastUpdated"`,
		rideID, etaMinutes, milesRemaining); err != nil {
		t.Fatalf("seed vehicle for %s: %v", rideID, err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_control_state (vehicle_id, nav_reading_at)
		 VALUES ('veh_' || $1, NOW())
		 ON CONFLICT (vehicle_id) DO UPDATE
		 SET nav_reading_at = GREATEST(EXCLUDED.nav_reading_at, go_vehicle_control_state.nav_reading_at)`,
		rideID); err != nil {
		t.Fatalf("seed nav stamp for %s: %v", rideID, err)
	}
}
