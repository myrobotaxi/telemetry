package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// serviceWindowFixture pins the two instants used across these tests. Distinct
// values so a transposed column shows up as a wrong value, not a coincidence.
var (
	teslaETC = time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	ownerEnd = time.Date(2026, 8, 3, 9, 30, 0, 0, time.UTC)
)

// TestVehicleRepo_ServiceWindowRoundTrips exercises the MYR-316 writers against
// a real Postgres. The properties that matter are all about NULL and about
// INDEPENDENCE, because those are precisely what the shared COALESCE
// control-state upsert cannot express and why these writers exist:
//
//  1. each source round-trips through its own column;
//  2. each source can be set BACK to NULL individually (a clear, not a no-op);
//  3. writing one source NEVER disturbs the other — a late Tesla estimate must
//     not erase what the owner typed, and vice versa;
//  4. ClearServiceWindow drops both at once and reports whether it did
//     anything, so the monitor can call it on every non-in-service edge without
//     churning updated_at for the overwhelming majority of cars;
//  5. a vehicle with no side-table row yet still accepts a write (the INSERT
//     arm) — a car whose only side-table interaction is a service visit is
//     ordinary.
func TestVehicleRepo_ServiceWindowRoundTrips(t *testing.T) {
	// GetByID LEFT JOINs the Go-owned side table, which TestMain's Prisma-only
	// createSchema does not create. Idempotent.
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_sw_owner"
		vehicle = "veh_sw_001"
	)
	seedVehicleSummaryRow(t, vehicle, ownerID, "5YJ3E1EA1NF000S01", "Serviced", "Model 3", 2024, "Blue", store.VehicleStatusInService, 44, 150)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	read := func(t *testing.T) (etc, owner *time.Time) {
		t.Helper()
		err := testPool.QueryRow(ctx,
			`SELECT service_etc, service_expected_end_at
			 FROM go_vehicle_control_state WHERE vehicle_id = $1`, vehicle).
			Scan(&etc, &owner)
		if err != nil {
			t.Fatalf("read service window: %v", err)
		}
		return etc, owner
	}

	t.Run("tesla estimate round-trips via the INSERT arm", func(t *testing.T) {
		if err := repo.SetServiceETC(ctx, vehicle, &teslaETC); err != nil {
			t.Fatalf("SetServiceETC: %v", err)
		}
		etc, owner := read(t)
		if etc == nil || !etc.Equal(teslaETC) {
			t.Errorf("service_etc = %v, want %v", etc, teslaETC)
		}
		if owner != nil {
			t.Errorf("service_expected_end_at = %v, want nil — the Tesla write must not touch it", owner)
		}
	})

	t.Run("owner value lands in its own column without disturbing Tesla's", func(t *testing.T) {
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, &ownerEnd); err != nil {
			t.Fatalf("SetServiceExpectedEndAt: %v", err)
		}
		etc, owner := read(t)
		if etc == nil || !etc.Equal(teslaETC) {
			t.Errorf("service_etc = %v, want it preserved at %v", etc, teslaETC)
		}
		if owner == nil || !owner.Equal(ownerEnd) {
			t.Errorf("service_expected_end_at = %v, want %v", owner, ownerEnd)
		}
	})

	t.Run("tesla can withdraw its estimate without erasing the owner fallback", func(t *testing.T) {
		if err := repo.SetServiceETC(ctx, vehicle, nil); err != nil {
			t.Fatalf("SetServiceETC(nil): %v", err)
		}
		etc, owner := read(t)
		if etc != nil {
			t.Errorf("service_etc = %v, want nil — a nil MUST clear, not be treated as 'leave alone'", etc)
		}
		if owner == nil || !owner.Equal(ownerEnd) {
			t.Errorf("service_expected_end_at = %v, want it preserved at %v", owner, ownerEnd)
		}
	})

	t.Run("owner can retract their own value", func(t *testing.T) {
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, nil); err != nil {
			t.Fatalf("SetServiceExpectedEndAt(nil): %v", err)
		}
		_, owner := read(t)
		if owner != nil {
			t.Errorf("service_expected_end_at = %v, want nil", owner)
		}
	})

	t.Run("clear drops both and reports whether it did anything", func(t *testing.T) {
		if err := repo.SetServiceETC(ctx, vehicle, &teslaETC); err != nil {
			t.Fatalf("SetServiceETC: %v", err)
		}
		if err := repo.SetServiceExpectedEndAt(ctx, vehicle, &ownerEnd); err != nil {
			t.Fatalf("SetServiceExpectedEndAt: %v", err)
		}

		cleared, err := repo.ClearServiceWindow(ctx, vehicle)
		if err != nil {
			t.Fatalf("ClearServiceWindow: %v", err)
		}
		if !cleared {
			t.Error("cleared = false, want true — both columns held a value")
		}
		etc, owner := read(t)
		if etc != nil || owner != nil {
			t.Errorf("after clear: etc=%v owner=%v, want both nil", etc, owner)
		}

		// Second clear is the common case in production: the monitor calls it on
		// every non-in-service edge, and almost every car has nothing to clear.
		cleared, err = repo.ClearServiceWindow(ctx, vehicle)
		if err != nil {
			t.Fatalf("ClearServiceWindow (idempotent): %v", err)
		}
		if cleared {
			t.Error("cleared = true on an already-clear row, want false — a no-op must not churn updated_at")
		}
	})
}

// TestVehicleRepo_ServiceWindowSurfacesOnReads proves the two columns reach
// BOTH read surfaces — the wide GetByID snapshot read (which LEFT JOINs the
// side table) and the lean ListSummariesByUser catalog read (which MYR-316
// taught to LEFT JOIN it for the first time). A column that persists but never
// surfaces is the failure mode worth pinning: the list query's join is new, and
// a mis-ordered scan destination there would silently transpose values.
func TestVehicleRepo_ServiceWindowSurfacesOnReads(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID  = "user_sw_read"
		inSvc    = "veh_sw_read_1"
		neverSvc = "veh_sw_read_2"
	)
	seedVehicleSummaryRow(t, inSvc, ownerID, "5YJ3E1EA1NF000S11", "AtShop", "Model 3", 2024, "Blue", store.VehicleStatusInService, 44, 150)
	seedVehicleSummaryRow(t, neverSvc, ownerID, "5YJ3E1EA1NF000S12", "Fine", "Model Y", 2023, "Grey", store.VehicleStatusParked, 80, 250)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.SetServiceETC(ctx, inSvc, &teslaETC); err != nil {
		t.Fatalf("SetServiceETC: %v", err)
	}
	if err := repo.SetServiceExpectedEndAt(ctx, inSvc, &ownerEnd); err != nil {
		t.Fatalf("SetServiceExpectedEndAt: %v", err)
	}

	t.Run("snapshot read carries both raw sources", func(t *testing.T) {
		v, err := repo.GetByID(ctx, inSvc)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if v.ServiceETC == nil || !v.ServiceETC.Equal(teslaETC) {
			t.Errorf("ServiceETC = %v, want %v", v.ServiceETC, teslaETC)
		}
		if v.ServiceExpectedEndAt == nil || !v.ServiceExpectedEndAt.Equal(ownerEnd) {
			t.Errorf("ServiceExpectedEndAt = %v, want %v", v.ServiceExpectedEndAt, ownerEnd)
		}
	})

	t.Run("catalog list read carries both raw sources", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}

		byID := make(map[string]store.VehicleSummary, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
		}

		got := byID[inSvc]
		if got.ServiceETC == nil || !got.ServiceETC.Equal(teslaETC) {
			t.Errorf("in-service row ServiceETC = %v, want %v", got.ServiceETC, teslaETC)
		}
		if got.ServiceExpectedEndAt == nil || !got.ServiceExpectedEndAt.Equal(ownerEnd) {
			t.Errorf("in-service row ServiceExpectedEndAt = %v, want %v", got.ServiceExpectedEndAt, ownerEnd)
		}

		// A car with no side-table row at all must LEFT JOIN to nils, not fail.
		other := byID[neverSvc]
		if other.ServiceETC != nil || other.ServiceExpectedEndAt != nil {
			t.Errorf("never-serviced row carried a window: etc=%v owner=%v",
				other.ServiceETC, other.ServiceExpectedEndAt)
		}
	})
}
