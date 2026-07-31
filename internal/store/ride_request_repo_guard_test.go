package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// Guarded-transition tests (MYR-174/175 check-then-write race fix). The
// guard is a single UPDATE with `WHERE id = $1 AND status = ANY($3)`, so
// concurrent conflicting transitions must serialize in Postgres: exactly one
// wins; every loser gets ErrRideRequestConflict.

func TestRideRequestRepo_UpdateStatusFrom(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	t.Run("legal transition succeeds and stamps accepted_at", func(t *testing.T) {
		// Scheduled so these subtests, which share one repo, don't collide
		// under the one-active-instant-ride guard (MYR-230).
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if err != nil {
			t.Fatalf("UpdateStatusFrom: %v", err)
		}
		if got.Status != store.RideRequestStatusAccepted {
			t.Errorf("status: %q", got.Status)
		}
		if got.AcceptedAt == nil {
			t.Error("accepted_at not stamped")
		}
	})

	t.Run("illegal from-state returns ErrRideRequestConflict", func(t *testing.T) {
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusCancelled); err != nil {
			t.Fatalf("first cancel: %v", err)
		}
		_, err = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if !errors.Is(err, store.ErrRideRequestConflict) {
			t.Fatalf("expected ErrRideRequestConflict, got %v", err)
		}
		if errors.Is(err, sdk.ErrNotFound) {
			t.Error("conflict must not wrap sdk.ErrNotFound")
		}
	})

	t.Run("unknown id returns ErrRideRequestNotFound", func(t *testing.T) {
		_, err := repo.UpdateStatusFrom(ctx, "crr-does-not-exist",
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound, got %v", err)
		}
	})
}

// TestRideRequestRepo_UpdateStatusFrom_ConcurrentDoubleAccept is the
// double-tap / two-devices scenario: two accepts race on the same
// `requested` row. Exactly one must win — the dispatch seam (ride.accepted)
// is published per winning transition, so a second winner would mean a
// double Tesla navigation_request push under MYR-176.
func TestRideRequestRepo_UpdateStatusFrom_ConcurrentDoubleAccept(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repo.UpdateStatusFrom(ctx, created.ID,
				[]store.RideRequestStatus{store.RideRequestStatusRequested},
				store.RideRequestStatusAccepted)
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideRequestConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one winner and one conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	final, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if final.Status != store.RideRequestStatusAccepted || final.AcceptedAt == nil {
		t.Errorf("final row: status=%q acceptedAt=%v", final.Status, final.AcceptedAt)
	}
}

// --- One active instant ride per rider (MYR-230, partial unique index) ---

// TestRideRequestRepo_Create_OneActiveInstant covers the guard's sequential
// behavior: a rider may hold at most one OPEN instant ride, terminal states
// free the slot, and scheduled rides are exempt.
func TestRideRequestRepo_Create_OneActiveInstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	t.Run("second open instant is rejected with ErrRideRequestActive", func(t *testing.T) {
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		_, err := repo.Create(ctx, minimalRideRequest())
		if !errors.Is(err, store.ErrRideRequestActive) {
			t.Fatalf("expected ErrRideRequestActive, got %v", err)
		}
		if errors.Is(err, sdk.ErrNotFound) {
			t.Error("active-ride conflict must not wrap sdk.ErrNotFound")
		}
	})

	t.Run("GetActiveInstantByRider returns the open instant ride", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		created, err := repo.Create(ctx, minimalRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ID != created.ID {
			t.Errorf("active id: got %q want %q", got.ID, created.ID)
		}
	})

	t.Run("no open instant ride returns ErrRideRequestNotFound", func(t *testing.T) {
		_, err := repo.GetActiveInstantByRider(ctx, "clnobody00000000000000")
		if !errors.Is(err, store.ErrRideRequestNotFound) || !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound (wrapping sdk.ErrNotFound), got %v", err)
		}
	})

	t.Run("terminal state frees the slot for a new instant ride", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		first, err := repo.Create(ctx, minimalRideRequest())
		if err != nil {
			t.Fatalf("first Create: %v", err)
		}
		// Cancel (terminal) removes it from the guard's partial index.
		if _, err := repo.UpdateStatus(ctx, first.ID, store.RideRequestStatusCancelled); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("second Create after terminal should succeed: %v", err)
		}
		// And GetActiveInstantByRider now returns the NEW open ride, not the
		// cancelled one.
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ID == first.ID {
			t.Error("cancelled ride must not be reported as active")
		}
	})

	t.Run("scheduled rides are exempt and never block", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		// Distinct instants: the per-RIDER guard under test is indifferent to
		// them, but the MYR-383 per-VEHICLE window gate is not, and stacking
		// three reservations on one car at one instant would now (correctly) be
		// refused by a rule this test is not about.
		mkScheduled := func() store.RideRequestRecord {
			return scheduledRideRequest()
		}
		// Many scheduled rides for one rider: all allowed.
		for i := 0; i < 3; i++ {
			if _, err := repo.Create(ctx, mkScheduled()); err != nil {
				t.Fatalf("scheduled Create %d: %v", i, err)
			}
		}
		// An open instant ride coexists with the scheduled ones.
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("instant Create alongside scheduled: %v", err)
		}
		// GetActiveInstantByRider returns the INSTANT one, ignoring scheduled.
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ScheduledFor != nil {
			t.Errorf("active-instant lookup returned a scheduled ride: %+v", got.ScheduledFor)
		}
	})
}

// TestMigration0004_DedupsOpenInstantRides proves migration 0004 is
// self-cleaning on databases that predate the guard (MYR-230 review fix):
// production accumulated riders holding several concurrently-open instant
// rides, on which a bare CREATE UNIQUE INDEX would abort and fail the
// deploy. The migration must first cancel every OLDER open instant ride per
// rider (keeping the most recent by created_at DESC, id DESC), leave
// scheduled/terminal/other-rider rows untouched, and then land the index.
func TestMigration0004_DedupsOpenInstantRides(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping migration integration test")
	}
	ctx := context.Background()

	// Ensure the schema is fully migrated, then rewind to pre-0004: drop the
	// guard index and point golang-migrate's version at 0003 so RunMigrations
	// re-applies 0004 against the seeded pre-guard state. The pre-guard debris
	// (several open instant rides for one rider on one car) violates BOTH the
	// per-rider (0004) and the per-vehicle (0013, MYR-266) guards, so drop BOTH
	// indexes to seed that historical state; re-applying from v3 re-runs both
	// dedups + recreates both indexes.
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations (initial): %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DROP INDEX IF EXISTS uq_go_ride_requests_active_instant_rider`); err != nil {
		t.Fatalf("drop rider guard index: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DROP INDEX IF EXISTS uq_go_ride_requests_active_instant_vehicle`); err != nil {
		t.Fatalf("drop vehicle guard index: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE schema_migrations SET version = 3, dirty = false`); err != nil {
		t.Fatalf("rewind schema_migrations: %v", err)
	}

	// Seed the pre-guard debris directly (raw SQL so created_at is exact and
	// distinct; the *_enc columns are opaque TEXT, dummy ciphertext is fine
	// since assertions read status/updated_at and never decrypt coordinates).
	const riderA, riderB = "clriderdupA0000000000000", "clriderdupB0000000000000"
	seed := func(id, rider, status string, createdAt time.Time, scheduled *time.Time) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `INSERT INTO go_ride_requests (
			id, rider_id, owner_id, vehicle_id,
			pickup_lat_enc, pickup_lng_enc, pickup_label,
			dropoff_lat_enc, dropoff_lng_enc, dropoff_label,
			status, scheduled_for, created_at, updated_at
		) VALUES ($1, $2, 'clownerdup000000000000', 'clvehicledup0000000000',
			'enc', 'enc', 'A', 'enc', 'enc', 'B', $3, $4, $5, $5)`,
			id, rider, status, scheduled, createdAt); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	base := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	sched := base.Add(48 * time.Hour)
	// Rider A: three open instant rides (the production debris shape) — only
	// the newest may survive open. Plus one scheduled and one terminal row,
	// both of which the dedup must not touch.
	seed("crrdupA1oldest", riderA, "requested", base, nil)
	seed("crrdupA2middle", riderA, "accepted", base.Add(time.Hour), nil)
	seed("crrdupA3newest", riderA, "accepted", base.Add(2*time.Hour), nil)
	seed("crrdupA4sched", riderA, "requested", base.Add(3*time.Hour), &sched)
	seed("crrdupA5done", riderA, "completed", base.Add(4*time.Hour), nil)
	// Rider B: a single open instant ride — must pass through untouched.
	seed("crrdupB1only", riderB, "requested", base, nil)

	// Re-apply 0004 (with the dedup) via the production entry point.
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations (re-apply 0004 over duplicates): %v", err)
	}

	wantStatus := map[string]string{
		"crrdupA1oldest": "cancelled", // older open instant → cancelled
		"crrdupA2middle": "cancelled", // older open instant → cancelled
		"crrdupA3newest": "accepted",  // newest open instant survives as-is
		"crrdupA4sched":  "requested", // scheduled: exempt, untouched
		"crrdupA5done":   "completed", // terminal: untouched
		"crrdupB1only":   "requested", // other rider's single open: untouched
	}
	for id, want := range wantStatus {
		var status string
		var updatedAt, createdAt time.Time
		if err := testPool.QueryRow(ctx,
			`SELECT status, updated_at, created_at FROM go_ride_requests WHERE id = $1`, id,
		).Scan(&status, &updatedAt, &createdAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != want {
			t.Errorf("%s: status = %q, want %q", id, status, want)
		}
		bumped := updatedAt.After(createdAt)
		if want == "cancelled" && !bumped {
			t.Errorf("%s: updated_at not bumped by the dedup transition", id)
		}
		if want != "cancelled" && bumped {
			t.Errorf("%s: updated_at moved on an untouched row", id)
		}
	}

	// The guard index landed and is live: a second open instant for rider B
	// is now rejected through the repo.
	var indexed bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_go_ride_requests_active_instant_rider')`,
	).Scan(&indexed); err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if !indexed {
		t.Fatal("guard index missing after migration")
	}
	rec := minimalRideRequest()
	rec.RiderID = riderB
	repo := store.NewRideRequestRepo(testPool, store.NoopMetrics{}, newTestEncryptor(t))
	if _, err := repo.Create(ctx, rec); !errors.Is(err, store.ErrRideRequestActive) {
		t.Fatalf("expected ErrRideRequestActive after migration, got %v", err)
	}
}

// TestRideRequestRepo_Create_ConcurrentInstant is the race the guard exists
// for: two instant creates for the same rider fire simultaneously. The
// partial unique index serializes them in Postgres — exactly one INSERT
// wins; the loser's 23505 surfaces as ErrRideRequestActive. This is the
// create-side analogue of the UpdateStatusFrom double-accept race.
func TestRideRequestRepo_Create_ConcurrentInstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repo.Create(ctx, minimalRideRequest())
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideRequestActive):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one winner and one conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	// Exactly one open instant row persisted.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_ride_requests WHERE rider_id = $1 AND scheduled_for IS NULL`,
		minimalRideRequest().RiderID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one persisted instant ride, got %d", n)
	}
}

// --- One active ride per VEHICLE (MYR-266, partial unique index 0013) ---

// rideForRiderVehicle builds an instant ride for a specific rider + vehicle so
// the per-vehicle guard tests can put TWO different riders on ONE car (the
// per-rider guard would otherwise block two instant rides for one rider).
func rideForRiderVehicle(rider, vehicle string) store.RideRequestRecord {
	r := minimalRideRequest()
	r.RiderID = rider
	r.VehicleID = vehicle
	return r
}

// TestRideRequestRepo_UpdateStatusFrom_OneActiveVehicle covers the per-vehicle
// guard's sequential behavior: a car serves at most one active instant ride.
// Two riders may hold PENDING (`requested`) requests to the same car — that is
// the owner's incoming feed — but the SECOND accept is rejected with
// ErrVehicleRideActive, and a terminal state on the first ride frees the car.
func TestRideRequestRepo_UpdateStatusFrom_OneActiveVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const veh = "clsharedvehicle0000000"
	const riderA, riderB = "clriderA00000000000000", "clriderB00000000000000"

	// Both riders request the SAME car — `requested` is EXCLUDED from the
	// per-vehicle guard, so both pending requests coexist (the incoming feed).
	rideA, err := repo.Create(ctx, rideForRiderVehicle(riderA, veh))
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	rideB, err := repo.Create(ctx, rideForRiderVehicle(riderB, veh))
	if err != nil {
		t.Fatalf("Create B (second pending request for same car must be allowed): %v", err)
	}

	// Owner accepts A → the car is now committed.
	if _, err := repo.UpdateStatusFrom(ctx, rideA.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept A: %v", err)
	}

	// Accepting B for the same car is now rejected by the guard.
	_, err = repo.UpdateStatusFrom(ctx, rideB.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted)
	if !errors.Is(err, store.ErrVehicleRideActive) {
		t.Fatalf("accept B: expected ErrVehicleRideActive, got %v", err)
	}
	if errors.Is(err, sdk.ErrNotFound) {
		t.Error("vehicle-active conflict must not wrap sdk.ErrNotFound")
	}
	if errors.Is(err, store.ErrRideRequestConflict) {
		t.Error("vehicle-active conflict must be distinct from ErrRideRequestConflict")
	}

	// The same car advancing its OWN committed ride through the active states
	// must NOT self-violate the guard (same row keeps the single index entry).
	for _, to := range []store.RideRequestStatus{store.RideRequestStatusArrived, store.RideRequestStatusEnroute} {
		from := store.RideRequestStatusAccepted
		if to == store.RideRequestStatusEnroute {
			from = store.RideRequestStatusArrived
		}
		if _, err := repo.UpdateStatusFrom(ctx, rideA.ID,
			[]store.RideRequestStatus{from}, to); err != nil {
			t.Fatalf("advance A %s->%s must not self-violate: %v", from, to, err)
		}
	}

	// A terminal state on A frees the car: B can now be accepted.
	if _, err := repo.UpdateStatus(ctx, rideA.ID, store.RideRequestStatusCompleted); err != nil {
		t.Fatalf("complete A: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, rideB.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept B after A completed should succeed: %v", err)
	}
}

// TestRideRequestRepo_UpdateStatusFrom_ScheduledVehicleExempt proves the
// per-vehicle guard is instant-only: an ACCEPTED scheduled ride for a car does
// not block an instant accept for the same car (mirrors the per-rider guard's
// scheduled exemption).
func TestRideRequestRepo_UpdateStatusFrom_ScheduledVehicleExempt(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const veh = "clschedvehicle00000000"
	const riderA, riderB = "clriderS00000000000000", "clriderT00000000000000"

	sched := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	schedRide := rideForRiderVehicle(riderA, veh)
	schedRide.ScheduledFor = &sched
	sRec, err := repo.Create(ctx, schedRide)
	if err != nil {
		t.Fatalf("Create scheduled: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, sRec.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept scheduled: %v", err)
	}

	// An instant ride for the SAME car can still be accepted — scheduled rows
	// are exempt (scheduled_for IS NULL is the guard predicate).
	instant, err := repo.Create(ctx, rideForRiderVehicle(riderB, veh))
	if err != nil {
		t.Fatalf("Create instant: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, instant.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept instant alongside accepted scheduled must succeed: %v", err)
	}
}

// TestRideRequestRepo_UpdateStatusFrom_ConcurrentAcceptSameVehicle is the race
// the per-vehicle guard exists for: two owners' devices (or a double-tap across
// two pending riders) accept two different rides for the SAME car at once. The
// partial unique index serializes them in Postgres — exactly one accept wins;
// the loser's 23505 surfaces as ErrVehicleRideActive. This is the per-vehicle
// analogue of the per-rider concurrent-create race.
func TestRideRequestRepo_UpdateStatusFrom_ConcurrentAcceptSameVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const veh = "clracevehicle000000000"
	rideA, err := repo.Create(ctx, rideForRiderVehicle("clriderR100000000000000", veh))
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	rideB, err := repo.Create(ctx, rideForRiderVehicle("clriderR200000000000000", veh))
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}

	results := make([]error, 2)
	ids := []string{rideA.ID, rideB.ID}
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repo.UpdateStatusFrom(ctx, ids[i],
				[]store.RideRequestStatus{store.RideRequestStatusRequested},
				store.RideRequestStatusAccepted)
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrVehicleRideActive):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one winner and one vehicle-active conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	// Exactly one committed (accepted) instant row persisted for the car.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_ride_requests
		   WHERE vehicle_id = $1 AND scheduled_for IS NULL
		     AND status IN ('accepted','enroute','arrived')`, veh).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one committed instant ride for the car, got %d", n)
	}
}

// TestMigration0013_DedupsActiveVehicleRides proves migration 0013 is
// self-cleaning on a database that predates the guard (mirrors the 0004 dedup
// test): a car double-booked with several active instant rides must have all
// but the newest cancelled before the unique index lands, or the deploy aborts.
func TestMigration0013_DedupsActiveVehicleRides(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping migration integration test")
	}
	ctx := context.Background()

	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations (initial): %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	// Rewind to pre-0013: drop the guard index and point golang-migrate at 0012
	// so RunMigrations re-applies 0013 against the seeded double-booked state.
	if _, err := testPool.Exec(ctx, `DROP INDEX IF EXISTS uq_go_ride_requests_active_instant_vehicle`); err != nil {
		t.Fatalf("drop guard index: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE schema_migrations SET version = 12, dirty = false`); err != nil {
		t.Fatalf("rewind schema_migrations: %v", err)
	}

	const vehX, vehY = "clvehdupX0000000000000", "clvehdupY0000000000000"
	seed := func(id, vehicle, rider, status string, createdAt time.Time, scheduled *time.Time) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `INSERT INTO go_ride_requests (
			id, rider_id, owner_id, vehicle_id,
			pickup_lat_enc, pickup_lng_enc, pickup_label,
			dropoff_lat_enc, dropoff_lng_enc, dropoff_label,
			status, scheduled_for, created_at, updated_at
		) VALUES ($1, $2, 'clownerdup000000000000', $3,
			'enc', 'enc', 'A', 'enc', 'enc', 'B', $4, $5, $6, $6)`,
			id, rider, vehicle, status, scheduled, createdAt); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	base := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	sched := base.Add(48 * time.Hour)
	// Vehicle X: three active instant rides (double-booked debris) — only the
	// newest survives. Plus a scheduled and a terminal row that must be left be.
	seed("crrvehX1oldest", vehX, "clr1", "accepted", base, nil)
	seed("crrvehX2middle", vehX, "clr2", "enroute", base.Add(time.Hour), nil)
	seed("crrvehX3newest", vehX, "clr3", "arrived", base.Add(2*time.Hour), nil)
	seed("crrvehX4sched", vehX, "clr4", "accepted", base.Add(3*time.Hour), &sched)
	seed("crrvehX5done", vehX, "clr5", "completed", base.Add(4*time.Hour), nil)
	// Vehicle Y: a single active instant ride — must pass through untouched.
	seed("crrvehY1only", vehY, "clr6", "accepted", base, nil)

	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations (re-apply 0013 over duplicates): %v", err)
	}

	wantStatus := map[string]string{
		"crrvehX1oldest": "cancelled", // older active instant → cancelled
		"crrvehX2middle": "cancelled", // older active instant → cancelled
		"crrvehX3newest": "arrived",   // newest active instant survives as-is
		"crrvehX4sched":  "accepted",  // scheduled: exempt, untouched
		"crrvehX5done":   "completed", // terminal: untouched
		"crrvehY1only":   "accepted",  // other car's single active: untouched
	}
	for id, want := range wantStatus {
		var status string
		var updatedAt, createdAt time.Time
		if err := testPool.QueryRow(ctx,
			`SELECT status, updated_at, created_at FROM go_ride_requests WHERE id = $1`, id,
		).Scan(&status, &updatedAt, &createdAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if status != want {
			t.Errorf("%s: status = %q, want %q", id, status, want)
		}
		bumped := updatedAt.After(createdAt)
		if want == "cancelled" && !bumped {
			t.Errorf("%s: updated_at not bumped by the dedup transition", id)
		}
		if want != "cancelled" && bumped {
			t.Errorf("%s: updated_at moved on an untouched row", id)
		}
	}

	// The guard index landed and is live: a second active instant ride for
	// vehicle Y is now rejected by the guarded accept through the repo.
	var indexed bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uq_go_ride_requests_active_instant_vehicle')`,
	).Scan(&indexed); err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	if !indexed {
		t.Fatal("guard index missing after migration")
	}
	repo := store.NewRideRequestRepo(testPool, store.NoopMetrics{}, newTestEncryptor(t))
	second := rideForRiderVehicle("clriderZ00000000000000", vehY)
	created, err := repo.Create(ctx, second) // `requested` is allowed (not committed yet)
	if err != nil {
		t.Fatalf("Create second pending for vehicle Y: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); !errors.Is(err, store.ErrVehicleRideActive) {
		t.Fatalf("expected ErrVehicleRideActive after migration, got %v", err)
	}
}

// TestRideRequestRepo_UpdateStatusFrom_CancelVsDecline races the rider's
// cancel against the owner's decline on the same `requested` row. Exactly
// one transition lands; the loser observes the conflict instead of silently
// overwriting a terminal state.
func TestRideRequestRepo_UpdateStatusFrom_CancelVsDecline(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		wg         sync.WaitGroup
		cancelErr  error
		declineErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, cancelErr = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested, store.RideRequestStatusAccepted},
			store.RideRequestStatusCancelled)
	}()
	go func() {
		defer wg.Done()
		_, declineErr = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusDeclined)
	}()
	wg.Wait()

	winners := 0
	for _, err := range []error{cancelErr, declineErr} {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrRideRequestConflict):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winning transition, got %d (cancel=%v decline=%v)", winners, cancelErr, declineErr)
	}

	final, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantStatus := store.RideRequestStatusCancelled
	if cancelErr != nil {
		wantStatus = store.RideRequestStatusDeclined
	}
	if final.Status != wantStatus {
		t.Errorf("final status %q does not match the winning transition %q", final.Status, wantStatus)
	}
}
