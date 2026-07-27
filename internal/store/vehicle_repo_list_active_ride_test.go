package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_ListSummariesByUser_HasActiveRide pins the MYR-233
// `hasActiveRide` derivation on the catalog read path.
//
// The flag MUST mirror the accept guard exactly: TRUE iff the vehicle
// holds a `go_ride_requests` row with
// `scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')`
// — the predicate of the partial unique index
// `uq_go_ride_requests_active_instant_vehicle` (migration 0013,
// MYR-266). Anything else — `requested`, any scheduled ride, any
// terminal state, no ride at all — is FALSE.
//
// Every case gets its own vehicle and all of them are fetched in ONE
// ListSummariesByUser call, so the test doubles as the "multiple
// vehicles in one list are flagged independently" case: a correlated
// EXISTS that leaked across rows (or collapsed to a single whole-list
// boolean) fails here immediately.
func TestVehicleRepo_ListSummariesByUser_HasActiveRide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanRideRequests(t)

	scheduled := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// vehicleID doubles as the map key for the assertion pass.
		vehicleID string
		// rides seeded against this vehicle before the single list call.
		rides []rideSeed
		want  bool
	}{
		{
			name:      "instant accepted ride makes the vehicle busy",
			vehicleID: "veh_ar_accepted",
			rides:     []rideSeed{{status: "accepted"}},
			want:      true,
		},
		{
			name:      "instant arrived ride makes the vehicle busy",
			vehicleID: "veh_ar_arrived",
			rides:     []rideSeed{{status: "arrived"}},
			want:      true,
		},
		{
			name:      "instant enroute ride makes the vehicle busy",
			vehicleID: "veh_ar_enroute",
			rides:     []rideSeed{{status: "enroute"}},
			want:      true,
		},
		{
			// Several riders may hold pending requests against one idle
			// car — the owner has not committed it to anyone yet, so the
			// car is NOT busy. Two rows prove it is not a count-based
			// derivation.
			name:      "requested-only rides leave the vehicle free",
			vehicleID: "veh_ar_requested",
			rides: []rideSeed{
				{status: "requested"},
				{status: "requested"},
			},
			want: false,
		},
		{
			// Scheduled rides are EXEMPT at every status: a future
			// reservation never makes the car busy. `accepted` here is
			// the case that would be true if the scheduled_for guard
			// were dropped from the predicate.
			name:      "scheduled accepted ride leaves the vehicle free",
			vehicleID: "veh_ar_sched_accepted",
			rides:     []rideSeed{{status: "accepted", scheduledFor: &scheduled}},
			want:      false,
		},
		{
			name:      "scheduled enroute ride leaves the vehicle free",
			vehicleID: "veh_ar_sched_enroute",
			rides:     []rideSeed{{status: "enroute", scheduledFor: &scheduled}},
			want:      false,
		},
		{
			name:      "completed ride frees the vehicle",
			vehicleID: "veh_ar_completed",
			rides:     []rideSeed{{status: "completed"}},
			want:      false,
		},
		{
			name:      "cancelled ride frees the vehicle",
			vehicleID: "veh_ar_cancelled",
			rides:     []rideSeed{{status: "cancelled"}},
			want:      false,
		},
		{
			name:      "declined ride frees the vehicle",
			vehicleID: "veh_ar_declined",
			rides:     []rideSeed{{status: "declined"}},
			want:      false,
		},
		{
			// Terminal history must not resurrect the flag.
			name:      "history of terminal rides leaves the vehicle free",
			vehicleID: "veh_ar_history",
			rides: []rideSeed{
				{status: "completed"},
				{status: "cancelled"},
				{status: "declined"},
			},
			want: false,
		},
		{
			name:      "vehicle with no rides at all is free",
			vehicleID: "veh_ar_norides",
			rides:     nil,
			want:      false,
		},
	}

	const ownerID = "user_active_ride"
	for i, tc := range cases {
		// VINs must be unique (Vehicle."vin" is UNIQUE); names only
		// affect ORDER BY, which this test does not assert on.
		seedVehicleSummaryRow(t, tc.vehicleID, ownerID,
			vinForIndex(i), tc.vehicleID, "Model 3", 2024, "Midnight Silver Metallic",
			store.VehicleStatusParked, 70, 210)
		for j, r := range tc.rides {
			seedRideRequestRow(t, rideIDForIndex(i, j), ownerID, tc.vehicleID, r)
		}
	}

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListSummariesByUser(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("ListSummariesByUser: %v", err)
	}
	if len(rows) != len(cases) {
		t.Fatalf("rows = %d, want %d", len(rows), len(cases))
	}

	byID := make(map[string]store.VehicleSummary, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := byID[tc.vehicleID]
			if !ok {
				t.Fatalf("vehicle %s missing from list", tc.vehicleID)
			}
			if got.HasActiveRide != tc.want {
				t.Errorf("HasActiveRide = %v, want %v (vehicle %s)",
					got.HasActiveRide, tc.want, tc.vehicleID)
			}
		})
	}
}

// TestVehicleRepo_ListSummariesByUser_ActiveRideIsPerVehicle proves the
// flag is scoped to the row's own vehicle: a busy car belonging to the
// SAME owner must not bleed onto that owner's other cars, and a busy car
// belonging to ANOTHER owner must not bleed across users either.
func TestVehicleRepo_ListSummariesByUser_ActiveRideIsPerVehicle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanRideRequests(t)

	const ownerA, ownerB = "user_bleed_a", "user_bleed_b"

	seedVehicleSummaryRow(t, "veh_bleed_busy", ownerA, "5YJ3E1EA1NF00BL01", "Busy",
		"Model 3", 2024, "Red", store.VehicleStatusDriving, 60, 180)
	seedVehicleSummaryRow(t, "veh_bleed_free", ownerA, "5YJ3E1EA1NF00BL02", "Free",
		"Model Y", 2023, "White", store.VehicleStatusParked, 90, 300)
	seedVehicleSummaryRow(t, "veh_bleed_other", ownerB, "5YJ3E1EA1NF00BL03", "Other",
		"Model S", 2022, "Black", store.VehicleStatusParked, 50, 250)

	seedRideRequestRow(t, "ride_bleed_1", ownerA, "veh_bleed_busy", rideSeed{status: "enroute"})
	seedRideRequestRow(t, "ride_bleed_2", ownerB, "veh_bleed_other", rideSeed{status: "accepted"})

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	rows, err := repo.ListSummariesByUser(context.Background(), ownerA)
	if err != nil {
		t.Fatalf("ListSummariesByUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (ownerA's vehicles only)", len(rows))
	}

	want := map[string]bool{"veh_bleed_busy": true, "veh_bleed_free": false}
	for _, r := range rows {
		w, ok := want[r.ID]
		if !ok {
			t.Fatalf("unexpected vehicle %s in ownerA's list", r.ID)
		}
		if r.HasActiveRide != w {
			t.Errorf("%s.HasActiveRide = %v, want %v", r.ID, r.HasActiveRide, w)
		}
	}
}

// rideSeed describes one go_ride_requests row for the fixtures above.
// scheduledFor nil means an INSTANT ride — the only kind the flag
// considers.
type rideSeed struct {
	status       string
	scheduledFor *time.Time
}

// seedRideRequestRow inserts a single go_ride_requests row. Coordinates
// are the encrypted-at-rest columns, so opaque placeholder ciphertext is
// enough — the flag derivation never decrypts anything.
//
// Every row gets its OWN rider, derived from the ride id: migration
// 0004's per-RIDER partial unique index
// (`uq_go_ride_requests_active_instant_rider`) permits one open instant
// ride per rider, so reusing a rider across two open rows would 23505 in
// the fixture rather than in the code under test.
func seedRideRequestRow(t *testing.T, id, ownerID, vehicleID string, r rideSeed) {
	t.Helper()
	riderID := "rider_" + id
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_ride_requests
		 (id, rider_id, owner_id, vehicle_id,
		  pickup_lat_enc, pickup_lng_enc, pickup_label,
		  dropoff_lat_enc, dropoff_lng_enc, dropoff_label,
		  status, scheduled_for)
		 VALUES ($1,$2,$3,$4,'enc','enc','Pickup','enc','enc','Dropoff',$5,$6)`,
		id, riderID, ownerID, vehicleID, r.status, r.scheduledFor)
	if err != nil {
		t.Fatalf("seed ride request %s (%s): %v", id, r.status, err)
	}
}

// cleanRideRequests truncates the Go-owned ride table. cleanTables only
// covers the Prisma-owned Vehicle/Drive tables, and go_ride_requests has
// no FK to Vehicle, so stale rows would otherwise survive a vehicle
// wipe and flip a later assertion.
func cleanRideRequests(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM go_ride_requests`); err != nil {
			t.Errorf("clean go_ride_requests (cleanup): %v", err)
		}
	})
}

// mustApplyGoMigrations applies the embedded Go-owned migrations to the
// shared test container. TestMain's createSchema only builds the
// Prisma-owned tables; the catalog list query now correlates against
// go_ride_requests (migration 0002, index 0013), so any test that
// exercises ListSummariesByUser needs the Go namespace present. Safely
// idempotent — RunMigrations returns cleanly on an already-migrated DB.
func mustApplyGoMigrations(t *testing.T) {
	t.Helper()
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
}

// vinForIndex builds a unique 17-character VIN per case index —
// Vehicle."vin" is UNIQUE, so the fixtures must not collide.
func vinForIndex(i int) string {
	return fmt.Sprintf("5YJ3E1EA1NF00A%03d", i)
}

func rideIDForIndex(i, j int) string {
	return fmt.Sprintf("ride_ar_%d_%d", i, j)
}
