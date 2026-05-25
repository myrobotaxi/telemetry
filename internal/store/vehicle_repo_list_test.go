package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_ListSummariesByUser exercises the MYR-122 lean
// projection. The slim path MUST:
//
//  1. return every owned vehicle, ordered by name then VIN
//  2. populate every catalog column the wire response emits
//  3. NEVER pull GPS, climate, or navRouteCoordinates — those columns
//     could be populated with arbitrary heavy data (encrypted shadows,
//     100KB+ polyline blobs) and the slim path must not even SELECT
//     them. We assert the absence implicitly: the row type
//     `VehicleSummary` has no field for them, so a regression that
//     re-widens the projection wouldn't compile.
//
// Sibling to TestVehicleRepo_CatalogFields, which exercises the wide
// `ListByUser` path that detail/edit consumers still use.
func TestVehicleRepo_ListSummariesByUser(t *testing.T) {
	cleanTables(t, testPool)

	// Seed three vehicles for user_001 with a deliberate insertion
	// order (carlos, alpha, bravo) so ORDER BY name, vin actually
	// re-sorts.
	seedVehicleSummaryRow(t, "veh_sum_001", "user_001", "5YJ3E1EA1NF000C01", "Carlos", "Model Y", 2024, "Pearl White", store.VehicleStatusDriving, 72, 218)
	seedVehicleSummaryRow(t, "veh_sum_002", "user_001", "5YJ3E1EA1NF000A02", "Alpha", "Model 3", 2023, "Midnight Silver Metallic", store.VehicleStatusParked, 35, 122)
	seedVehicleSummaryRow(t, "veh_sum_003", "user_001", "5YJ3E1EA1NF000B03", "Bravo", "Model S", 2022, "Solid Black", store.VehicleStatusCharging, 88, 410)
	// Vehicle owned by another user — must NOT appear in user_001's
	// results.
	seedVehicleSummaryRow(t, "veh_sum_other", "user_002", "5YJ3E1EA1NF000O99", "Other", "Model 3", 2024, "Red", store.VehicleStatusParked, 50, 200)
	// Heavy detail columns the slim path MUST NOT pull. We populate
	// them so a regression that switches the projection back to the
	// wide SELECT would surface as a scan failure (or, worse, decrypt
	// noise) — both are loud signals.
	mustUpdateHeavyDetailColumns(t, "veh_sum_001")
	mustUpdateHeavyDetailColumns(t, "veh_sum_002")
	mustUpdateHeavyDetailColumns(t, "veh_sum_003")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("returns owned vehicles in name, vin order", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, "user_001")
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3 (owned by user_001)", len(rows))
		}
		wantOrder := []string{"Alpha", "Bravo", "Carlos"}
		for i, want := range wantOrder {
			if rows[i].Name != want {
				t.Errorf("rows[%d].Name = %q, want %q", i, rows[i].Name, want)
			}
		}
	})

	t.Run("populates every catalog column the wire response emits", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, "user_001")
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		byName := map[string]store.VehicleSummary{}
		for _, r := range rows {
			byName[r.Name] = r
		}

		carlos := byName["Carlos"]
		if carlos.ID != "veh_sum_001" {
			t.Errorf("Carlos.ID = %q, want veh_sum_001", carlos.ID)
		}
		if carlos.VIN != "5YJ3E1EA1NF000C01" {
			t.Errorf("Carlos.VIN = %q, want 5YJ3E1EA1NF000C01", carlos.VIN)
		}
		if carlos.UserID != "user_001" {
			t.Errorf("Carlos.UserID = %q, want user_001", carlos.UserID)
		}
		if carlos.Model != "Model Y" {
			t.Errorf("Carlos.Model = %q, want Model Y", carlos.Model)
		}
		if carlos.Year != 2024 {
			t.Errorf("Carlos.Year = %d, want 2024", carlos.Year)
		}
		if carlos.Color != "Pearl White" {
			t.Errorf("Carlos.Color = %q, want Pearl White", carlos.Color)
		}
		if carlos.Status != store.VehicleStatusDriving {
			t.Errorf("Carlos.Status = %q, want driving", carlos.Status)
		}
		if carlos.ChargeLevel != 72 {
			t.Errorf("Carlos.ChargeLevel = %d, want 72", carlos.ChargeLevel)
		}
		if carlos.EstimatedRange != 218 {
			t.Errorf("Carlos.EstimatedRange = %d, want 218", carlos.EstimatedRange)
		}
		if carlos.LastUpdated.IsZero() {
			t.Errorf("Carlos.LastUpdated is zero")
		}
	})

	t.Run("empty result for user with no vehicles", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, "user_with_nothing")
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %d, want 0", len(rows))
		}
	})

	t.Run("excludes vehicles owned by other users", func(t *testing.T) {
		rows, err := repo.ListSummariesByUser(ctx, "user_001")
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		for _, r := range rows {
			if r.ID == "veh_sum_other" {
				t.Errorf("user_001 leaked vehicle owned by user_002: %+v", r)
			}
		}
	})
}

// seedVehicleSummaryRow inserts a single Vehicle row populated with
// only the catalog columns the lean projection cares about. Keeps the
// test data free of the heavy detail columns (GPS, navRouteCoordinates)
// so the assertions in the test stay focused on the slim shape.
func seedVehicleSummaryRow(
	t *testing.T,
	id, userID, vin, name, model string,
	year int,
	color string,
	status store.VehicleStatus,
	chargeLevel, estimatedRange int,
) {
	t.Helper()
	ctx := context.Background()
	_, err := testPool.Exec(ctx,
		`INSERT INTO "Vehicle" (
			"id", "userId", "vin", "name",
			"model", "year", "color", "status",
			"chargeLevel", "estimatedRange"
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8::"VehicleStatus",
			$9, $10
		)`,
		id, userID, vin, name,
		model, year, color, string(status),
		chargeLevel, estimatedRange,
	)
	if err != nil {
		t.Fatalf("seedVehicleSummaryRow(%s): %v", id, err)
	}
}

// mustUpdateHeavyDetailColumns populates the plaintext GPS columns and
// the navRouteCoordinates JSON blob on a Vehicle row. The slim
// projection MUST NOT touch these columns — if a regression re-widens
// the SELECT, a 50KB polyline blob on every list call surfaces in the
// duration log immediately. The values themselves don't matter; only
// their presence does.
func mustUpdateHeavyDetailColumns(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	// 200-point polyline ≈ a realistic short-route blob. The exact
	// shape isn't load-bearing for this test — populating non-NULL
	// JSON is the point.
	type pt struct {
		Lng float64 `json:"lng"`
		Lat float64 `json:"lat"`
	}
	pts := make([]pt, 200)
	for i := range pts {
		pts[i] = pt{Lng: -122.0 + float64(i)*0.0001, Lat: 37.7 + float64(i)*0.0001}
	}
	blob, err := json.Marshal(pts)
	if err != nil {
		t.Fatalf("marshal nav route: %v", err)
	}
	_, err = testPool.Exec(ctx,
		`UPDATE "Vehicle"
		   SET "latitude" = 37.7749,
		       "longitude" = -122.4194,
		       "navRouteCoordinates" = $2::jsonb
		 WHERE "id" = $1`,
		id, string(blob),
	)
	if err != nil {
		t.Fatalf("populate heavy detail columns(%s): %v", id, err)
	}
}
