package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_FillVehicleIdentity covers the MYR-507 write carve-out.
//
// The two rules worth a test are the ones that are not obvious from the
// signature: the fill is PER-COLUMN (a car may have a model and no year), and
// once there is nothing left to fill the statement must match ZERO rows — not
// "match and write the same value again". That second one is load-bearing
// rather than tidy: the caller runs on every connectivity edge for every car in
// the fleet, so a statement that always matched would bump `updatedAt` on every
// "Vehicle" row forever and stream a pointless change to the Prisma side.
func TestVehicleRepo_FillVehicleIdentity(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("fills both placeholders on a never-enriched car", func(t *testing.T) {
		// Exactly as the pre-MYR-507 provisioning INSERT left it, which is how
		// the reporting vehicle actually sat in production.
		seedVehicleSummaryRow(t, "veh_id_001", "user_001", "7SAXCDE2NTF000001", "Tesla",
			"", 0, "UltraRed", store.VehicleStatusOffline, 0, 0)

		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "7SAXCDE2NTF000001", "Model X", 2026)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if !filled {
			t.Fatal("filled = false, want true — both columns held placeholders")
		}
		assertVehicleIdentity(t, "veh_id_001", "Model X", 2026)
	})

	t.Run("a second pass is a genuine no-op", func(t *testing.T) {
		before := vehicleUpdatedAt(t, "veh_id_001")

		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "7SAXCDE2NTF000001", "Model X", 2026)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if filled {
			t.Error("filled = true on a fully-populated row — the WHERE clause must " +
				"match ZERO rows once there is nothing left to fill, or every edge " +
				"pass touches every vehicle in the fleet")
		}
		if after := vehicleUpdatedAt(t, "veh_id_001"); !after.Equal(before) {
			t.Errorf("updatedAt moved (%v → %v) on a no-op fill", before, after)
		}
	})

	t.Run("NEVER overwrites a richer model the Prisma flow wrote", func(t *testing.T) {
		// The Prisma web-link flow records trims a VIN position cannot encode.
		// A blind overwrite would downgrade this row to "Model 3" — trading the
		// bug MYR-507 fixes for a subtler one.
		seedVehicleSummaryRow(t, "veh_id_002", "user_001", "5YJ3E1EA7TF000003", "Pearl",
			"Model 3 Performance", 2025, "Pearl White", store.VehicleStatusParked, 60, 200)

		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "5YJ3E1EA7TF000003", "Model 3", 2026)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if filled {
			t.Error("filled = true, want false — both columns were already populated")
		}
		assertVehicleIdentity(t, "veh_id_002", "Model 3 Performance", 2025)
	})

	t.Run("fills the empty column and leaves the populated one alone", func(t *testing.T) {
		// A car with a model and no year. The two CASE arms are independent, so
		// this must fill exactly one of them.
		seedVehicleSummaryRow(t, "veh_id_003", "user_001", "5YJ3E1EA7NF000004", "Halfway",
			"Model 3 Long Range", 0, "Red", store.VehicleStatusParked, 50, 150)

		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "5YJ3E1EA7NF000004", "Model 3", 2022)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if !filled {
			t.Fatal("filled = false, want true — `year` was still 0")
		}
		assertVehicleIdentity(t, "veh_id_003", "Model 3 Long Range", 2022)
	})

	t.Run("another owner's car is never touched", func(t *testing.T) {
		seedVehicleSummaryRow(t, "veh_id_004", "user_002", "7SAYGDEF9TF000002", "Theirs",
			"", 0, "Blue", store.VehicleStatusParked, 40, 120)

		// Ownership is a WHERE predicate, not a caller precondition.
		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "7SAYGDEF9TF000002", "Model Y", 2026)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if filled {
			t.Error("filled = true — a mismatched owner must update ZERO rows")
		}
		assertVehicleIdentity(t, "veh_id_004", "", 0)
	})

	t.Run("an undecodable VIN writes nothing and issues no statement", func(t *testing.T) {
		seedVehicleSummaryRow(t, "veh_id_005", "user_001", "7SAQCDE2QTF000005", "Unknown",
			"", 0, "Grey", store.VehicleStatusParked, 40, 120)

		// What `internal/vin` hands back for a code it does not recognise. It
		// must read as "nothing to say", never as a clear.
		filled, err := repo.FillVehicleIdentity(ctx, "user_001", "7SAQCDE2QTF000005", "", 0)
		if err != nil {
			t.Fatalf("FillVehicleIdentity: %v", err)
		}
		if filled {
			t.Error("filled = true for an empty model and zero year")
		}
		assertVehicleIdentity(t, "veh_id_005", "", 0)
	})
}

// TestVehicleRepo_CatalogCarriesTrimLabel covers the MYR-507 read half on BOTH
// catalog queries — the owner list and the shared/viewer list.
//
// Both must carry it, and the VIEWER one is the reason the field exists: a rider
// never fetches a /snapshot, so if the shared query drops the column the
// descriptor regresses to the colour token with every owner-side test still
// green.
func TestVehicleRepo_CatalogCarriesTrimLabel(t *testing.T) {
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	seedVehicleSummaryRow(t, "veh_trim_001", "user_001", "7SAXCDE2NTF000001", "Amruth's X Plaid",
		"Model X", 2026, "UltraRed", store.VehicleStatusParked, 31, 96)
	seedVehicleSummaryRow(t, "veh_trim_002", "user_001", "5YJ3E1EA7TF000003", "Never read",
		"Model 3", 2026, "", store.VehicleStatusParked, 60, 200)

	// Only the first car has been read from Tesla.
	mustSetTrimLabel(t, "veh_trim_001", "Plaid")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("owner rows carry it, and an unread trim stays NULL", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, "user_001")
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		byName := map[string]store.VehicleSummary{}
		for _, r := range rows {
			byName[r.Name] = r
		}

		got := byName["Amruth's X Plaid"]
		if got.TrimLabel == nil || *got.TrimLabel != "Plaid" {
			t.Errorf("TrimLabel = %v, want \"Plaid\"", got.TrimLabel)
		}
		if unread := byName["Never read"]; unread.TrimLabel != nil {
			t.Errorf("TrimLabel = %v, want nil — a car with no control-state row has "+
				"no trim, and that is NOT the empty string", *unread.TrimLabel)
		}
	})

	t.Run("shared/viewer rows carry it too", func(t *testing.T) {
		seedShareGrant(t, "share_trim_001", "veh_trim_001", "user_001", "user_002", "accepted")

		rows, err := repo.ListSharedSummariesByUser(ctx, "user_002")
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].TrimLabel == nil || *rows[0].TrimLabel != "Plaid" {
			t.Errorf("viewer TrimLabel = %v, want \"Plaid\". The viewer is the ONE role "+
				"with no /snapshot to fall back on", rows[0].TrimLabel)
		}
		// The identity siblings must survive the same query.
		if rows[0].Model != "Model X" || rows[0].Year != 2026 || rows[0].Color != "UltraRed" {
			t.Errorf("viewer identity = (%q, %d, %q), want (\"Model X\", 2026, \"UltraRed\")",
				rows[0].Model, rows[0].Year, rows[0].Color)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func assertVehicleIdentity(t *testing.T, vehicleID, wantModel string, wantYear int) {
	t.Helper()
	var (
		gotModel string
		gotYear  int
	)
	err := testPool.QueryRow(context.Background(),
		`SELECT "model", "year" FROM "Vehicle" WHERE "id" = $1`, vehicleID,
	).Scan(&gotModel, &gotYear)
	if err != nil {
		t.Fatalf("read back identity for %s: %v", vehicleID, err)
	}
	if gotModel != wantModel {
		t.Errorf("%s model = %q, want %q", vehicleID, gotModel, wantModel)
	}
	if gotYear != wantYear {
		t.Errorf("%s year = %d, want %d", vehicleID, gotYear, wantYear)
	}
}

func vehicleUpdatedAt(t *testing.T, vehicleID string) time.Time {
	t.Helper()
	var at time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT "updatedAt" FROM "Vehicle" WHERE "id" = $1`, vehicleID,
	).Scan(&at); err != nil {
		t.Fatalf("read updatedAt for %s: %v", vehicleID, err)
	}
	return at
}

func mustSetTrimLabel(t *testing.T, vehicleID, label string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_control_state (vehicle_id, trim_label)
		 VALUES ($1, $2)
		 ON CONFLICT (vehicle_id) DO UPDATE SET trim_label = EXCLUDED.trim_label`,
		vehicleID, label)
	if err != nil {
		t.Fatalf("seed trim_label for %s: %v", vehicleID, err)
	}
}
