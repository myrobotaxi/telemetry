package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_UpdateVehicleColor exercises the MYR-320 owner-scoped write
// carve-out against a real Postgres (data-lifecycle.md §1.4). The properties
// that matter are all about SCOPE and about the ONE value the writer refuses:
//
//  1. an owner's car gets Tesla's colour name written;
//  2. an EMPTY colour is a NO-OP, never a clear — Tesla omitting exterior_color
//     on a partial payload must not blank a colour an earlier read got right.
//     This is the inverse of the MYR-286 plate writer, where an empty string IS
//     a deliberate clear, and the difference is not cosmetic: nobody ever asks
//     to erase their car's colour, so an empty value can only ever be a gap in
//     Tesla's answer;
//  3. a non-owner's write matches ZERO rows and changes NOTHING — the ownership
//     predicate is in the WHERE clause, not a caller precondition;
//  4. an unknown VIN is likewise a zero-row no-op;
//  5. the write touches ONLY the colour, so it can never race the streaming
//     pipeline.
func TestVehicleRepo_UpdateVehicleColor(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	// GetByID LEFT JOINs the Go-owned side table, which TestMain's Prisma-only
	// createSchema does not create. Idempotent.
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID  = "user_color_owner"
		otherID  = "user_color_other"
		vehicle  = "veh_color_001"
		otherVeh = "veh_color_002"
		ownerVIN = "5YJ3E1EA1NF000C01"
		otherVIN = "5YJ3E1EA1NF000C02"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, ownerVIN, "Mine", "Model Y", 2026, "", store.VehicleStatusParked, 55, 190)
	seedVehicleSummaryRow(t, otherVeh, otherID, otherVIN, "Theirs", "Model 3", 2023, "White", store.VehicleStatusParked, 60, 200)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	readColor := func(t *testing.T, id string) string {
		t.Helper()
		var color string
		if err := testPool.QueryRow(ctx, `SELECT "color" FROM "Vehicle" WHERE "id" = $1`, id).Scan(&color); err != nil {
			t.Fatalf("read color(%s): %v", id, err)
		}
		return color
	}

	t.Run("owner's car is populated from the empty placeholder", func(t *testing.T) {
		// The starting state for every real car: the MYR-257 provisioning INSERT
		// seeds '' and nothing has ever filled it in.
		if got := readColor(t, vehicle); got != "" {
			t.Fatalf("precondition: color = %q, want the empty placeholder", got)
		}

		updated, err := repo.UpdateVehicleColor(ctx, ownerID, ownerVIN, "Quicksilver")
		if err != nil {
			t.Fatalf("UpdateVehicleColor: %v", err)
		}
		if !updated {
			t.Fatal("updated = false, want true")
		}
		if got := readColor(t, vehicle); got != "Quicksilver" {
			t.Errorf("stored color = %q, want %q", got, "Quicksilver")
		}
	})

	t.Run("EMPTY colour never overwrites a known one", func(t *testing.T) {
		updated, err := repo.UpdateVehicleColor(ctx, ownerID, ownerVIN, "")
		if err != nil {
			t.Fatalf("UpdateVehicleColor(empty): %v", err)
		}
		if updated {
			t.Error("updated = true for an empty colour, want false — an empty read " +
				"means 'we learned nothing', never 'the car has no colour'")
		}
		if got := readColor(t, vehicle); got != "Quicksilver" {
			t.Errorf("color = %q, want the previous %q to survive", got, "Quicksilver")
		}
	})

	t.Run("a non-empty colour DOES overwrite a known one", func(t *testing.T) {
		// A repaint, or Tesla correcting itself. Only the empty case is refused.
		updated, err := repo.UpdateVehicleColor(ctx, ownerID, ownerVIN, "Deep Blue Metallic")
		if err != nil {
			t.Fatalf("UpdateVehicleColor: %v", err)
		}
		if !updated {
			t.Fatal("updated = false, want true")
		}
		if got := readColor(t, vehicle); got != "Deep Blue Metallic" {
			t.Errorf("stored color = %q, want %q", got, "Deep Blue Metallic")
		}
	})

	t.Run("cross-user write matches zero rows and changes nothing", func(t *testing.T) {
		updated, err := repo.UpdateVehicleColor(ctx, ownerID, otherVIN, "Quicksilver")
		if err != nil {
			t.Fatalf("UpdateVehicleColor(cross-user): %v", err)
		}
		if updated {
			t.Error("updated = true for another owner's VIN, want false")
		}
		if got := readColor(t, otherVeh); got != "White" {
			t.Errorf("other owner's color = %q, want the untouched %q", got, "White")
		}
	})

	t.Run("unknown VIN is a zero-row no-op", func(t *testing.T) {
		updated, err := repo.UpdateVehicleColor(ctx, ownerID, "5YJ3E1EA1NF000XXX", "Quicksilver")
		if err != nil {
			t.Fatalf("UpdateVehicleColor(unknown vin): %v", err)
		}
		if updated {
			t.Error("updated = true for an unknown VIN, want false")
		}
	})

	t.Run("no telemetry column is touched", func(t *testing.T) {
		var name, model string
		var year, chargeLevel, estimatedRange int
		err := testPool.QueryRow(ctx,
			`SELECT "name", "model", "year", "chargeLevel", "estimatedRange"
			 FROM "Vehicle" WHERE "id" = $1`, vehicle,
		).Scan(&name, &model, &year, &chargeLevel, &estimatedRange)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		if name != "Mine" || model != "Model Y" || year != 2026 || chargeLevel != 55 || estimatedRange != 190 {
			t.Errorf("colour write clobbered a sibling column: name=%q model=%q year=%d charge=%d range=%d",
				name, model, year, chargeLevel, estimatedRange)
		}
	})
}

// TestVehicleRepo_ListInServiceVINs backs the MYR-320 periodic pass. The list IS
// the in_service filter, so the properties that matter are that it names every
// in-service car and NOTHING else — a pass that read a parked car would burn
// Fleet API budget on vehicles the edge path already covers.
func TestVehicleRepo_ListInServiceVINs(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const owner = "user_insvc"
	seedVehicleSummaryRow(t, "veh_svc_1", owner, "5YJ3E1EA1NF000S01", "InService A", "Model Y", 2026, "Red",
		store.VehicleStatusInService, 40, 150)
	seedVehicleSummaryRow(t, "veh_svc_2", owner, "5YJ3E1EA1NF000S02", "InService B", "Model 3", 2024, "Blue",
		store.VehicleStatusInService, 41, 151)
	seedVehicleSummaryRow(t, "veh_parked", owner, "5YJ3E1EA1NF000S03", "Parked", "Model 3", 2024, "White",
		store.VehicleStatusParked, 42, 152)
	seedVehicleSummaryRow(t, "veh_driving", owner, "5YJ3E1EA1NF000S04", "Driving", "Model X", 2025, "Black",
		store.VehicleStatusDriving, 43, 153)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("returns in_service VINs only", func(t *testing.T) {
		vins, err := repo.ListInServiceVINs(ctx, 100)
		if err != nil {
			t.Fatalf("ListInServiceVINs: %v", err)
		}
		got := make(map[string]bool, len(vins))
		for _, v := range vins {
			got[v] = true
		}
		if len(vins) != 2 {
			t.Fatalf("len = %d (%v), want 2", len(vins), vins)
		}
		for _, want := range []string{"5YJ3E1EA1NF000S01", "5YJ3E1EA1NF000S02"} {
			if !got[want] {
				t.Errorf("missing in-service VIN %s", want)
			}
		}
		for _, unwanted := range []string{"5YJ3E1EA1NF000S03", "5YJ3E1EA1NF000S04"} {
			if got[unwanted] {
				t.Errorf("VIN %s is not in service but was listed", unwanted)
			}
		}
	})

	t.Run("limit caps one pass", func(t *testing.T) {
		vins, err := repo.ListInServiceVINs(ctx, 1)
		if err != nil {
			t.Fatalf("ListInServiceVINs: %v", err)
		}
		if len(vins) != 1 {
			t.Errorf("len = %d, want 1 — the cap bounds one pass", len(vins))
		}
	})

	t.Run("non-positive limit refuses to scan", func(t *testing.T) {
		vins, err := repo.ListInServiceVINs(ctx, 0)
		if err != nil {
			t.Fatalf("ListInServiceVINs(0): %v", err)
		}
		if len(vins) != 0 {
			t.Errorf("len = %d, want 0 — a zero limit must not become an unbounded scan", len(vins))
		}
	})
}
