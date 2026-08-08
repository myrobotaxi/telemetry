package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/passengerscrub"
)

// One-time legacy passenger-field scrub tests (MYR-447).
//
// They live in package store_test rather than beside the package under test so
// they can reuse the shared testcontainers TestMain in db_test.go: the scrub
// runs raw SQL against go_ride_requests and there is nothing to learn from
// asserting it against a mock.

func setupPassengerScrub(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping passenger scrub integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
}

func scrubLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

// TestPassengerScrubClearsBothColumnsEverywhere is the headline test: the scrub
// is deliberately BROADER than the sweeper — no age window, no status filter —
// and it always takes both columns.
func TestPassengerScrubClearsBothColumnsEverywhere(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	const owner = "user-legacy-scrub"
	const vehicle = "veh-legacy-scrub"
	// Deliberately spanning open AND terminal statuses, and fresh AND old rows.
	// The sweeper would reach none of these today; this binary reaches all of
	// them, which is the whole reason it exists.
	seedRide(t, "legacy-requested", owner, vehicle, "requested", time.Minute, true)
	seedRide(t, "legacy-accepted", owner, vehicle, "accepted", time.Hour, true)
	seedRide(t, "legacy-completed", owner, vehicle, "completed", time.Hour, true)
	seedRide(t, "legacy-cancelled", owner, vehicle, "cancelled", 400*24*time.Hour, true)
	// A row that never had a passenger: it must not be counted as work.
	seedRide(t, "legacy-none", owner, vehicle, "completed", time.Hour, false)

	res, err := passengerscrub.New(testPool, scrubLogger(), false).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsScanned != 4 {
		t.Errorf("RowsScanned = %d, want 4 (the rows carrying passenger data)", res.RowsScanned)
	}
	if res.RowsScrubbed != 4 {
		t.Errorf("RowsScrubbed = %d, want 4", res.RowsScrubbed)
	}
	if res.UpdateErrors != 0 {
		t.Errorf("UpdateErrors = %d, want 0", res.UpdateErrors)
	}
	if res.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0 — the column must be empty after a clean run", res.Remaining)
	}

	for _, id := range []string{"legacy-requested", "legacy-accepted", "legacy-completed", "legacy-cancelled"} {
		t.Run("both columns NULL: "+id, func(t *testing.T) {
			name, phone := ridePassenger(t, id)
			// Asserted as a pair. A phone-only scrub would leave a
			// name-without-phone row, which the iOS passenger block renders as a
			// leading blank rather than as no passenger at all.
			if name != nil || phone != nil {
				t.Errorf("passenger columns = %v / %v, want BOTH NULL", name, phone)
			}
		})
	}

	t.Run("the rows otherwise survive", func(t *testing.T) {
		for _, id := range []string{"legacy-requested", "legacy-accepted", "legacy-completed", "legacy-cancelled"} {
			if !rideExists(t, id) {
				t.Errorf("%s was deleted; the scrub empties two columns, it does not remove rows", id)
			}
		}
	})
}

// TestPassengerScrubIsIdempotent: a second run selects nothing and writes
// nothing. The selection predicate is what guarantees it.
func TestPassengerScrubIsIdempotent(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	seedRide(t, "idem-a", "user-scrub-idem", "veh-scrub-idem", "completed", time.Hour, true)
	seedRide(t, "idem-b", "user-scrub-idem", "veh-scrub-idem", "arrived", time.Hour, true)

	first, err := passengerscrub.New(testPool, scrubLogger(), false).Run(ctx)
	if err != nil {
		t.Fatalf("Run (first): %v", err)
	}
	if first.RowsScrubbed != 2 {
		t.Fatalf("first run scrubbed %d rows, want 2", first.RowsScrubbed)
	}

	second, err := passengerscrub.New(testPool, scrubLogger(), false).Run(ctx)
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if second.RowsScanned != 0 {
		t.Errorf("second run scanned %d rows, want 0 — the predicate must exclude scrubbed rows", second.RowsScanned)
	}
	if second.RowsScrubbed != 0 {
		t.Errorf("second run scrubbed %d rows, want 0", second.RowsScrubbed)
	}
	if second.Remaining != 0 {
		t.Errorf("second run Remaining = %d, want 0", second.Remaining)
	}
}

// TestPassengerScrubDryRunWritesNothing pins the rehearsal. The scrub is
// irreversible, so the flag that lets an operator see the blast radius first has
// to actually not write.
func TestPassengerScrubDryRunWritesNothing(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	seedRide(t, "dry-a", "user-scrub-dry", "veh-scrub-dry", "completed", time.Hour, true)
	seedRide(t, "dry-b", "user-scrub-dry", "veh-scrub-dry", "cancelled", time.Hour, true)

	res, err := passengerscrub.New(testPool, scrubLogger(), true).Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.DryRun {
		t.Error("Result.DryRun = false on a dry run")
	}
	if res.RowsScanned != 2 {
		t.Errorf("RowsScanned = %d, want 2 — a dry run still reports the blast radius", res.RowsScanned)
	}
	if res.RowsScrubbed != 0 {
		t.Errorf("RowsScrubbed = %d, want 0 on a dry run", res.RowsScrubbed)
	}
	if res.Remaining != 2 {
		t.Errorf("Remaining = %d, want 2 — nothing was written", res.Remaining)
	}

	for _, id := range []string{"dry-a", "dry-b"} {
		name, phone := ridePassenger(t, id)
		if name == nil || phone == nil {
			t.Errorf("%s lost passenger data on a DRY RUN: name=%v phone=%v", id, name, phone)
		}
	}
}

// TestPassengerScrubDoesNotBumpUpdatedAt guards the interaction with the
// retention sweeper: updated_at is the age boundary the 365-day DELETE keys off,
// so a scrub that touched it would defer every scrubbed ride's deletion by
// another full year.
func TestPassengerScrubDoesNotBumpUpdatedAt(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	seedRide(t, "age-keep", "user-scrub-age", "veh-scrub-age", "completed", 300*24*time.Hour, true)
	before := rideUpdatedAt(t, "age-keep")

	if _, err := passengerscrub.New(testPool, scrubLogger(), false).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if after := rideUpdatedAt(t, "age-keep"); !after.Equal(before) {
		t.Errorf("updated_at moved from %v to %v; the 365-day retention boundary keys off this column",
			before, after)
	}
}

// TestPassengerScrubCountRemaining covers the operator's verification path.
func TestPassengerScrubCountRemaining(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	if n, err := passengerscrub.CountRemaining(ctx, testPool); err != nil || n != 0 {
		t.Fatalf("CountRemaining on an empty table = %d, %v; want 0, nil", n, err)
	}

	seedRide(t, "count-a", "user-scrub-count", "veh-scrub-count", "completed", time.Hour, true)
	seedRide(t, "count-b", "user-scrub-count", "veh-scrub-count", "completed", time.Hour, false)

	n, err := passengerscrub.CountRemaining(ctx, testPool)
	if err != nil {
		t.Fatalf("CountRemaining: %v", err)
	}
	if n != 1 {
		t.Errorf("CountRemaining = %d, want 1", n)
	}
}
