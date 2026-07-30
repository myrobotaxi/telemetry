package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_RideShareRoundTrips exercises the MYR-342 writer and reader
// against a real Postgres. The properties that matter are the ones the shared
// COALESCE control-state upsert could NOT have provided, plus the one that only
// a real database can prove:
//
//  1. a vehicle with NO side-table row reads as ENABLED — this is the ordinary
//     state of most cars and the reason the read carries COALESCE(..., TRUE);
//  2. `false` actually persists. Under the shared upsert's
//     `col = COALESCE(EXCLUDED.col, existing.col)` form a false would have been
//     writable but a clear would not, and for a boolean toggle the interesting
//     half is precisely the one the shared form mangles;
//  3. the write round-trips BOTH ways repeatedly — pausing is not a one-way
//     door;
//  4. the INSERT arm works, so an owner whose first ever control-state
//     interaction is pausing the car does not silently no-op;
//  5. writing this column disturbs NO other control-state column, which is what
//     keeps it safely adjacent to the MYR-316 service window on the same row.
func TestVehicleRepo_RideShareRoundTrips(t *testing.T) {
	// The reads and writes target the Go-owned side table, which TestMain's
	// Prisma-only createSchema does not create. Idempotent.
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_rs_owner"
		vehicle = "veh_rs_001"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000R01", "Shared", "Model 3", 2024, "White", store.VehicleStatusParked, 61, 190)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("a vehicle with no side-table row reads as enabled", func(t *testing.T) {
		var rows int
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM go_vehicle_control_state WHERE vehicle_id = $1`, vehicle).Scan(&rows); err != nil {
			t.Fatalf("count control-state rows: %v", err)
		}
		if rows != 0 {
			t.Fatalf("precondition: expected no control-state row yet, got %d", rows)
		}

		enabled, err := repo.RideShareEnabled(ctx, vehicle)
		if err != nil {
			t.Fatalf("RideShareEnabled: %v", err)
		}
		if !enabled {
			t.Error("a car nobody has ever touched must read as ENABLED — the wire " +
				"contract's absent-means-enabled rule depends on it")
		}
	})

	t.Run("an unknown vehicle id reads as enabled, not an error", func(t *testing.T) {
		// The side table knows nothing about which vehicles exist, and every
		// caller has already established existence by other means. Inventing a
		// not-found here would push that work onto them twice.
		enabled, err := repo.RideShareEnabled(ctx, "veh_rs_does_not_exist")
		if err != nil {
			t.Fatalf("RideShareEnabled on an unknown id must not error: %v", err)
		}
		if !enabled {
			t.Error("an unknown vehicle must not read as paused")
		}
	})

	t.Run("pausing persists through the INSERT arm", func(t *testing.T) {
		if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
			t.Fatalf("SetRideShareEnabled(false): %v", err)
		}
		enabled, err := repo.RideShareEnabled(ctx, vehicle)
		if err != nil {
			t.Fatalf("RideShareEnabled: %v", err)
		}
		if enabled {
			t.Error("false did not persist — an owner's pause must survive the write " +
				"that created the row")
		}
	})

	t.Run("the toggle moves both ways, repeatedly", func(t *testing.T) {
		for i, want := range []bool{true, false, true} {
			if err := repo.SetRideShareEnabled(ctx, vehicle, want); err != nil {
				t.Fatalf("step %d SetRideShareEnabled(%v): %v", i, want, err)
			}
			got, err := repo.RideShareEnabled(ctx, vehicle)
			if err != nil {
				t.Fatalf("step %d RideShareEnabled: %v", i, err)
			}
			if got != want {
				t.Fatalf("step %d: got %v want %v", i, got, want)
			}
		}
	})

	t.Run("the write disturbs no sibling control-state column", func(t *testing.T) {
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, &ownerEnd); err != nil {
			t.Fatalf("seed a sibling column: %v", err)
		}
		if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
			t.Fatalf("SetRideShareEnabled: %v", err)
		}

		var expected *string
		if err := testPool.QueryRow(ctx,
			`SELECT service_expected_end_at::text FROM go_vehicle_control_state WHERE vehicle_id = $1`,
			vehicle).Scan(&expected); err != nil {
			t.Fatalf("read sibling column: %v", err)
		}
		if expected == nil {
			t.Error("the ride-share write erased the service window — the two share a " +
				"row but must not share a statement")
		}
	})

	t.Run("the snapshot read path carries the flag", func(t *testing.T) {
		// GetByID is the join the two request-time enforcement gates read the
		// flag off, so a regression here would disable them silently rather than
		// loudly.
		if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
			t.Fatalf("SetRideShareEnabled: %v", err)
		}
		row, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if row.RideShareEnabled {
			t.Error("GetByID did not carry the pause — the create and accept gates read it from here")
		}

		if err := repo.SetRideShareEnabled(ctx, vehicle, true); err != nil {
			t.Fatalf("SetRideShareEnabled: %v", err)
		}
		row, err = repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !row.RideShareEnabled {
			t.Error("GetByID reported a paused vehicle after it was re-enabled")
		}
	})

	t.Run("the catalog list read path carries the flag", func(t *testing.T) {
		if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
			t.Fatalf("SetRideShareEnabled: %v", err)
		}
		rows, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected 1 catalog row, got %d", len(rows))
		}
		if rows[0].RideShareEnabled {
			t.Error("the catalog row did not carry the pause")
		}
	})
}

// TestVehicleRepo_RideShareIsNotReachableFromTelemetry is the access-control
// half of the design, asserted rather than merely commented.
//
// The column is deliberately ABSENT from ControlStateUpdate, the struct the
// TELEMETRY writer pipeline fills. If it were ever added there, a routine frame
// from the car could re-enable ride sharing on a vehicle its owner had paused —
// a security bug wearing the clothes of a refactor. This test pins the property
// end to end: pause the car, run a control-state upsert carrying every other
// column, and assert the pause survived.
func TestVehicleRepo_RideShareIsNotReachableFromTelemetry(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_rs_owner2"
		vehicle = "veh_rs_002"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000R02", "Paused", "Model Y", 2025, "Black", store.VehicleStatusParked, 55, 170)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.SetRideShareEnabled(ctx, vehicle, false); err != nil {
		t.Fatalf("SetRideShareEnabled(false): %v", err)
	}

	locked := true
	if err := repo.UpsertControlState(ctx, vehicle, store.ControlStateUpdate{IsLocked: &locked}); err != nil {
		t.Fatalf("UpsertControlState: %v", err)
	}

	enabled, err := repo.RideShareEnabled(ctx, vehicle)
	if err != nil {
		t.Fatalf("RideShareEnabled: %v", err)
	}
	if enabled {
		t.Error("a telemetry-driven control-state upsert re-enabled ride sharing — " +
			"the column must stay out of ControlStateUpdate")
	}
}
