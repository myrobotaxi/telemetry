package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-409 — migration 0034 and the nav-freshness read/write round trip.

// navStampColumnMeta reads the live type and nullability of nav_reading_at.
func navStampColumnMeta(t *testing.T) (dataType, nullable string, present bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_name = 'go_vehicle_control_state'
		    AND column_name = 'nav_reading_at'`).Scan(&dataType, &nullable)
	if err != nil {
		return "", "", false
	}
	return dataType, nullable, true
}

// TestMigration0034_AddsNullableNavReadingAt pins the shape.
//
// NULLABLE IS THE CONTRACT, not an oversight. NULL means "no navigation frame
// has ever been seen for this car", which every gate downstream reads as not
// fresh — the fail-closed direction, and the only honest answer for a car we
// have never heard navigate. A NOT NULL DEFAULT NOW() would have manufactured a
// fresh stamp for every car in the fleet on deploy day and re-opened the exact
// defect the column closes.
func TestMigration0034_AddsNullableNavReadingAt(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	dataType, nullable, present := navStampColumnMeta(t)
	if !present {
		t.Fatal("nav_reading_at missing after migrate up")
	}
	if dataType != "timestamp with time zone" {
		t.Errorf("data_type = %q, want timestamp with time zone", dataType)
	}
	if nullable != "YES" {
		t.Errorf("is_nullable = %q, want YES — NULL is how a car with no navigation "+
			"history says so, and every gate reads it as not fresh", nullable)
	}
}

// TestSetNavReadingAt_IsMonotoneForward is the write invariant, and the reason
// the stamp does not ride the shared COALESCE upsert.
//
// Two replicas write this row and the writer flushes from a ticker, so "the last
// write" is decided by scheduling rather than by which observation is newer. A
// stamp that could regress would report a genuinely fresh reading as stale and
// flicker a client gate off and back on.
func TestSetNavReadingAt_IsMonotoneForward(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	const vehicleID = "veh_nav_stamp_0034"

	early := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	late := time.Now().UTC().Truncate(time.Millisecond)

	readBack := func() *time.Time {
		var got *time.Time
		if err := testPool.QueryRow(ctx,
			`SELECT nav_reading_at FROM go_vehicle_control_state WHERE vehicle_id = $1`,
			vehicleID).Scan(&got); err != nil {
			t.Fatalf("read back: %v", err)
		}
		return got
	}

	// The INSERT arm: a car whose only side-table interaction is navigation has
	// no row yet.
	if err := repo.SetNavReadingAt(ctx, vehicleID, late); err != nil {
		t.Fatalf("SetNavReadingAt(late): %v", err)
	}
	if got := readBack(); got == nil || !got.Equal(late) {
		t.Fatalf("after first write = %v, want %v", got, late)
	}

	// The regression a naive last-writer-wins upsert would allow.
	if err := repo.SetNavReadingAt(ctx, vehicleID, early); err != nil {
		t.Fatalf("SetNavReadingAt(early): %v", err)
	}
	if got := readBack(); got == nil || !got.Equal(late) {
		t.Fatalf("an OLDER observation moved the stamp back to %v; GREATEST is not "+
			"holding and a fresh reading will report stale", got)
	}

	// Forward still works, or the column would freeze at its first value.
	later := late.Add(time.Minute)
	if err := repo.SetNavReadingAt(ctx, vehicleID, later); err != nil {
		t.Fatalf("SetNavReadingAt(later): %v", err)
	}
	if got := readBack(); got == nil || !got.Equal(later) {
		t.Fatalf("after forward write = %v, want %v", got, later)
	}

	// A zero instant is "no observation", not a write.
	if err := repo.SetNavReadingAt(ctx, vehicleID, time.Time{}); err != nil {
		t.Fatalf("SetNavReadingAt(zero): %v", err)
	}
	if got := readBack(); got == nil || !got.Equal(later) {
		t.Fatalf("a zero instant disturbed the stamp: %v", got)
	}
}

// TestActivityProjections_ReadTheNavStampNotTheRowStamp is the read half, and it
// is the assertion that would have caught the original defect.
//
// Both projections that feed the Live Activity sender — the ticker's list and
// the lifecycle fan-out's single-ride read — must source NavUpdatedAt from
// nav_reading_at. A car with a freshly written row and NO nav stamp must come
// back nil, because that is precisely the state the bug lived in.
func TestActivityProjections_ReadTheNavStampNotTheRowStamp(t *testing.T) {
	const ride = "cride0409n1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	const rider = "crider0409a"
	if err := repo.RegisterActivity(ctx, ride, rider, "token-0409", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	// setupLiveActivities seeds vehicle_id as 'veh_' || rideID.
	vehicleID := "veh_" + ride

	// STEP 1: no nav stamp at all. The side-table row may not even exist. This
	// is a car that has never reported navigation, and both reads must say so.
	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}
	if legs[0].NavUpdatedAt != nil {
		t.Errorf("NavUpdatedAt = %v with no nav stamp recorded, want nil — the read is "+
			"falling back to a row stamp and MYR-409 is re-opened", legs[0].NavUpdatedAt)
	}
	rc, err := repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if rc.NavUpdatedAt != nil {
		t.Errorf("context NavUpdatedAt = %v with no nav stamp, want nil", rc.NavUpdatedAt)
	}

	// STEP 2: the car reports navigation. Both reads must now carry that exact
	// instant — not the car row's own timestamp, and not NOW().
	stamp := time.Now().UTC().Add(-45 * time.Second).Truncate(time.Millisecond)
	vehicles := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	if err := vehicles.SetNavReadingAt(ctx, vehicleID, stamp); err != nil {
		t.Fatalf("SetNavReadingAt: %v", err)
	}

	legs, err = repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities after stamp: %v", err)
	}
	if len(legs) != 1 || legs[0].NavUpdatedAt == nil || !legs[0].NavUpdatedAt.Equal(stamp) {
		t.Fatalf("leg NavUpdatedAt = %v, want %v", legs[0].NavUpdatedAt, stamp)
	}
	rc, err = repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide after stamp: %v", err)
	}
	if rc.NavUpdatedAt == nil || !rc.NavUpdatedAt.Equal(stamp) {
		t.Fatalf("context NavUpdatedAt = %v, want %v", rc.NavUpdatedAt, stamp)
	}
}

// TestActivityProjections_CarryTheDispatchInstant proves the relevance half
// reaches the sender. Without it push.navReadingIsOurs refuses every ride, which
// would suppress the Arriving rung for the whole fleet — a failure that looks
// like nothing at all in production, so it is pinned here.
func TestActivityProjections_CarryTheDispatchInstant(t *testing.T) {
	const ride = "cride0409n2"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "crider0409b", "token-0409b", false); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Undispatched: the seeded ride has no dispatched_at.
	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(legs))
	}
	if legs[0].PickupDispatchedAt != nil {
		t.Errorf("PickupDispatchedAt = %v on an undispatched ride, want nil",
			legs[0].PickupDispatchedAt)
	}

	dispatched := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Millisecond)
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET dispatched_at = $2, dispatch_status = 'sent' WHERE id = $1`,
		ride, dispatched); err != nil {
		t.Fatalf("stamp dispatch: %v", err)
	}

	legs, err = repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities after dispatch: %v", err)
	}
	if len(legs) != 1 || legs[0].PickupDispatchedAt == nil || !legs[0].PickupDispatchedAt.Equal(dispatched) {
		t.Fatalf("leg PickupDispatchedAt = %v, want %v", legs[0].PickupDispatchedAt, dispatched)
	}
	rc, err := repo.ActivityContextForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivityContextForRide: %v", err)
	}
	if rc.PickupDispatchedAt == nil || !rc.PickupDispatchedAt.Equal(dispatched) {
		t.Fatalf("context PickupDispatchedAt = %v, want %v", rc.PickupDispatchedAt, dispatched)
	}
}
