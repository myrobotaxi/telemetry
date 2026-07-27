package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_UpdateLicensePlate exercises the MYR-286 owner-scoped write
// carve-out against a real Postgres (data-lifecycle.md §1.4). The properties
// that matter are all about SCOPE and SHAPE, not about normalization — the
// handler owns normalization and this writer is deliberately dumb:
//
//  1. an owner can set their own car's plate;
//  2. an empty plate CLEARS it, writing the empty string (the column is NOT
//     NULL, so a clear must never produce NULL);
//  3. a non-owner's write matches ZERO rows and changes NOTHING — the
//     ownership predicate is in the WHERE clause, not a caller precondition;
//  4. an unknown vehicleId is likewise a zero-row no-op;
//  5. the write touches ONLY the plate — no telemetry column is clobbered, so
//     it can never race the streaming pipeline.
func TestVehicleRepo_UpdateLicensePlate(t *testing.T) {
	// GetByID LEFT JOINs the Go-owned go_vehicle_control_state side table,
	// which TestMain's Prisma-only createSchema does not create. Idempotent.
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID  = "user_plate_owner"
		otherID  = "user_plate_other"
		vehicle  = "veh_plate_001"
		otherVeh = "veh_plate_002"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000P01", "Plated", "Model 3", 2024, "Red", store.VehicleStatusParked, 55, 190)
	seedVehicleSummaryRow(t, otherVeh, otherID, "5YJ3E1EA1NF000P02", "Theirs", "Model Y", 2023, "White", store.VehicleStatusParked, 60, 200)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	readPlate := func(t *testing.T, id string) string {
		t.Helper()
		var plate string
		if err := testPool.QueryRow(ctx, `SELECT "licensePlate" FROM "Vehicle" WHERE "id" = $1`, id).Scan(&plate); err != nil {
			t.Fatalf("read plate(%s): %v", id, err)
		}
		return plate
	}

	t.Run("owner sets their own plate", func(t *testing.T) {
		updated, err := repo.UpdateLicensePlate(ctx, ownerID, vehicle, "ABC 1234")
		if err != nil {
			t.Fatalf("UpdateLicensePlate: %v", err)
		}
		if !updated {
			t.Fatal("updated = false, want true")
		}
		if got := readPlate(t, vehicle); got != "ABC 1234" {
			t.Errorf("stored plate = %q, want %q", got, "ABC 1234")
		}
	})

	t.Run("read paths surface the stored plate", func(t *testing.T) {
		// Both read surfaces the wire exposes must carry it: the wide
		// GetByID (backs /snapshot) and the lean list projection (backs
		// GET /api/vehicles).
		v, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if v.LicensePlate != "ABC 1234" {
			t.Errorf("GetByID LicensePlate = %q, want %q", v.LicensePlate, "ABC 1234")
		}

		rows, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].LicensePlate != "ABC 1234" {
			t.Errorf("summary LicensePlate = %q, want %q", rows[0].LicensePlate, "ABC 1234")
		}
	})

	t.Run("empty plate clears to empty string, never NULL", func(t *testing.T) {
		updated, err := repo.UpdateLicensePlate(ctx, ownerID, vehicle, "")
		if err != nil {
			t.Fatalf("UpdateLicensePlate: %v", err)
		}
		if !updated {
			t.Fatal("updated = false, want true (a clear is an ordinary write)")
		}
		if got := readPlate(t, vehicle); got != "" {
			t.Errorf("stored plate = %q, want empty string", got)
		}
		var isNull bool
		if err := testPool.QueryRow(ctx,
			`SELECT "licensePlate" IS NULL FROM "Vehicle" WHERE "id" = $1`, vehicle,
		).Scan(&isNull); err != nil {
			t.Fatalf("null check: %v", err)
		}
		if isNull {
			t.Error("cleared plate is NULL; the column is NOT NULL and a clear must write the empty string")
		}
	})

	t.Run("non-owner write matches zero rows and changes nothing", func(t *testing.T) {
		if _, err := repo.UpdateLicensePlate(ctx, otherID, otherVeh, "MINE 1"); err != nil {
			t.Fatalf("seed other owner's plate: %v", err)
		}

		updated, err := repo.UpdateLicensePlate(ctx, ownerID, otherVeh, "STOLEN")
		if err != nil {
			t.Fatalf("UpdateLicensePlate: %v", err)
		}
		if updated {
			t.Error("updated = true; a non-owner write must match zero rows")
		}
		if got := readPlate(t, otherVeh); got != "MINE 1" {
			t.Errorf("other owner's plate = %q, want %q — cross-user write leaked", got, "MINE 1")
		}
	})

	t.Run("unknown vehicle is a zero-row no-op", func(t *testing.T) {
		updated, err := repo.UpdateLicensePlate(ctx, ownerID, "veh_does_not_exist", "ABC 1234")
		if err != nil {
			t.Fatalf("UpdateLicensePlate: %v", err)
		}
		if updated {
			t.Error("updated = true for an unknown vehicleId")
		}
	})

	t.Run("write touches only the plate", func(t *testing.T) {
		before, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID before: %v", err)
		}
		if _, err := repo.UpdateLicensePlate(ctx, ownerID, vehicle, "XYZ-99"); err != nil {
			t.Fatalf("UpdateLicensePlate: %v", err)
		}
		after, err := repo.GetByID(ctx, vehicle)
		if err != nil {
			t.Fatalf("GetByID after: %v", err)
		}
		if after.LicensePlate != "XYZ-99" {
			t.Fatalf("plate = %q, want %q", after.LicensePlate, "XYZ-99")
		}
		// Normalize away the field under test, then require the rest of the
		// row to be identical — a broadened UPDATE would show up here.
		// reflect.DeepEqual (not ==) because Vehicle carries a
		// json.RawMessage, which is not comparable.
		after.LicensePlate = before.LicensePlate
		if !reflect.DeepEqual(after, before) {
			t.Errorf("plate write mutated other columns:\nbefore=%+v\nafter =%+v", before, after)
		}
	})
}
