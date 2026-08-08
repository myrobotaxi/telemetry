package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestDriveRepo_ListMissingAddresses covers the MYR-240 backfill
// projection: which rows are eligible, and that each carries the
// routePoints the caller extracts coordinates from.
//
// MYR-433: the repo must be encryption-enabled, and the SAME encryptor
// has to serve the seeding Create and the ListMissingAddresses read.
// routePoints is sealed in routePointsEnc now, so a keyless repo seeds
// nothing decryptable and every row comes back with an empty trail —
// which is also the honest production behaviour: `ops geocode backfill`
// without a key geocodes nothing rather than reading plaintext.
func TestDriveRepo_ListMissingAddresses(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_020", "5YJ3E1EA1NF000020")

	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{},
		newTestEncryptor(t), silentRouteBlobLogger())
	ctx := context.Background()

	routePoints := json.RawMessage(`[
		{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"},
		{"lat":33.1032,"lng":-96.8236,"speed":40,"heading":90,"timestamp":"2026-07-17T10:20:00Z"}
	]`)

	seeds := []store.DriveRecord{
		{
			// Closed drive, only startAddress missing.
			ID: "drv_missing_start", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T10:00:00Z", EndTime: "2026-07-17T10:20:00Z", RoutePoints: routePoints,
			StartAddress: "", EndAddress: "789 Elm St, Frisco, TX",
		},
		{
			// Closed drive, only endAddress missing — the ordinary
			// backfill-eligible case.
			ID: "drv_missing_end", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T11:00:00Z", EndTime: "2026-07-17T11:20:00Z", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "",
		},
		{
			// Both addresses already populated — never eligible.
			ID: "drv_fully_addressed", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T12:00:00Z", EndTime: "2026-07-17T12:20:00Z", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "789 Elm St, Frisco, TX",
		},
		{
			// MYR-240 adversarial-review fix: an OPEN drive (empty
			// endTime) with an empty endAddress — the state every Drive
			// row starts in per mapDriveStarted. Must be excluded from
			// the endAddress side of the sweep: routePoints' last entry
			// is the car's current, still-changing position, and writing
			// it as endAddress on an open drive would be wrong.
			ID: "drv_open_missing_end", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T13:00:00Z", EndTime: "", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "",
		},
		{
			// Open drive that's ALSO missing startAddress — the start
			// side must still be picked up (start is known and stable
			// the moment the drive begins; only the end side is
			// mid-drive-unstable).
			ID: "drv_open_missing_start", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T14:00:00Z", EndTime: "", RoutePoints: routePoints,
			StartAddress: "", EndAddress: "",
		},
	}
	for _, d := range seeds {
		if err := repo.Create(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	rows, err := repo.ListMissingAddresses(ctx)
	if err != nil {
		t.Fatalf("ListMissingAddresses: %v", err)
	}

	got := make(map[string]store.DriveBackfillRow, len(rows))
	for _, r := range rows {
		got[r.ID] = r
	}

	if _, ok := got["drv_fully_addressed"]; ok {
		t.Errorf("drv_fully_addressed should not be returned; both addresses are already populated")
	}
	if _, ok := got["drv_missing_start"]; !ok {
		t.Errorf("drv_missing_start should be returned (startAddress empty)")
	}
	if _, ok := got["drv_missing_end"]; !ok {
		t.Errorf("drv_missing_end should be returned (endAddress empty, drive is closed)")
	}
	if _, ok := got["drv_open_missing_end"]; ok {
		t.Errorf("drv_open_missing_end must NOT be returned — the drive is still open, its endAddress='' is expected, not backfill-eligible")
	}
	if _, ok := got["drv_open_missing_start"]; !ok {
		t.Errorf("drv_open_missing_start should still be returned for its start side even though the drive is open")
	}

	row := got["drv_missing_start"]
	if row.EndAddress != "789 Elm St, Frisco, TX" {
		t.Errorf("EndAddress = %q, want the already-populated value preserved", row.EndAddress)
	}
	if row.EndTime == "" {
		t.Errorf("EndTime should be surfaced and non-empty for a closed drive")
	}
	if len(row.RoutePoints) == 0 {
		t.Error("RoutePoints should be populated so the caller can extract coordinates")
	}

	openRow := got["drv_open_missing_start"]
	if openRow.EndTime != "" {
		t.Errorf("EndTime = %q, want empty for an open drive (belt-and-braces signal to the caller)", openRow.EndTime)
	}

	// MYR-447: the projection carries the audit subject too. seedVehicle
	// owns every vehicle it creates with 'user_001'.
	if row.VehicleID != "veh_020" {
		t.Errorf("VehicleID = %q, want %q", row.VehicleID, "veh_020")
	}
	if row.UserID != "user_001" {
		t.Errorf("UserID = %q, want %q", row.UserID, "user_001")
	}
}

// TestDriveRepo_ListMissingAddresses_OwnerAndVehicle covers the MYR-447
// audit projection: `ops geocode backfill` decrypts every eligible drive
// in the FLEET, so each row has to name the vehicle it belongs to and that
// vehicle's owner — the data subject the operator_decrypt row is keyed on.
//
// It also pins the join itself. queryDriveMissingAddresses grew a
// `JOIN "Vehicle"`, and an inner join is only safe here because
// Drive.vehicleId is NOT NULL with an FK to Vehicle.id. A row silently
// dropped by that join is a drive that never gets an address — the exact
// failure mode the MYR-447 predicate rewrite already had to fix once — so
// the eligible set is asserted exactly, not by containment.
func TestDriveRepo_ListMissingAddresses_OwnerAndVehicle(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicleForOwner(t, testPool, "veh_030", "5YJ3E1EA1NF000030", "user_alpha")
	seedVehicleForOwner(t, testPool, "veh_031", "5YJ3E1EA1NF000031", "user_beta")

	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{},
		newTestEncryptor(t), silentRouteBlobLogger())
	ctx := context.Background()

	routePoints := json.RawMessage(`[
		{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-08-07T10:00:00Z"},
		{"lat":33.1032,"lng":-96.8236,"speed":40,"heading":90,"timestamp":"2026-08-07T10:20:00Z"}
	]`)

	seeds := []store.DriveRecord{
		{
			ID: "drv_alpha_1", VehicleID: "veh_030", Date: "2026-08-07",
			StartTime: "2026-08-07T10:00:00Z", EndTime: "2026-08-07T10:20:00Z", RoutePoints: routePoints,
			StartAddress: "", EndAddress: "789 Elm St, Frisco, TX",
		},
		{
			// Second eligible drive on the SAME car: the caller groups by
			// (owner, vehicle), so both rows must come back carrying the
			// same pair rather than one collapsing into the other.
			ID: "drv_alpha_2", VehicleID: "veh_030", Date: "2026-08-07",
			StartTime: "2026-08-07T11:00:00Z", EndTime: "2026-08-07T11:20:00Z", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "",
		},
		{
			// A different owner entirely — the fan-out the audit exists for.
			ID: "drv_beta_1", VehicleID: "veh_031", Date: "2026-08-07",
			StartTime: "2026-08-07T12:00:00Z", EndTime: "2026-08-07T12:20:00Z", RoutePoints: routePoints,
			StartAddress: "", EndAddress: "",
		},
		{
			// Ineligible: both addresses already sealed. Present so the
			// exact-set assertion below proves the join did not widen the
			// result set either.
			ID: "drv_beta_addressed", VehicleID: "veh_031", Date: "2026-08-07",
			StartTime: "2026-08-07T13:00:00Z", EndTime: "2026-08-07T13:20:00Z", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "789 Elm St, Frisco, TX",
		},
	}
	for _, d := range seeds {
		if err := repo.Create(ctx, d); err != nil {
			t.Fatalf("seed drive %s: %v", d.ID, err)
		}
	}

	rows, err := repo.ListMissingAddresses(ctx)
	if err != nil {
		t.Fatalf("ListMissingAddresses: %v", err)
	}

	type ownership struct{ vehicleID, userID string }
	want := map[string]ownership{
		"drv_alpha_1": {"veh_030", "user_alpha"},
		"drv_alpha_2": {"veh_030", "user_alpha"},
		"drv_beta_1":  {"veh_031", "user_beta"},
	}

	got := make(map[string]ownership, len(rows))
	for _, r := range rows {
		got[r.ID] = ownership{r.VehicleID, r.UserID}
	}

	if len(got) != len(want) {
		t.Errorf("returned %d row(s) %v, want exactly %d — the JOIN must neither drop nor duplicate an eligible drive",
			len(got), got, len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("%s missing from the result set — the JOIN dropped an eligible drive", id)
			continue
		}
		if g.vehicleID != w.vehicleID {
			t.Errorf("%s VehicleID = %q, want %q", id, g.vehicleID, w.vehicleID)
		}
		if g.userID != w.userID {
			t.Errorf("%s UserID = %q, want %q (the audit row's data subject)", id, g.userID, w.userID)
		}
	}
}

// seedVehicleForOwner inserts a vehicle owned by an explicit user id.
// db_test.go's seedVehicle hardcodes 'user_001', which cannot express the
// multi-owner fan-out this file has to assert on.
func seedVehicleForOwner(t *testing.T, pool *pgxpool.Pool, id, vin, userID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "name", "status")
		 VALUES ($1, $2, $3, 'Test Model 3', 'parked')`,
		id, userID, vin)
	if err != nil {
		t.Fatalf("seed vehicle %s: %v", id, err)
	}
}

func TestDriveRepo_UpdateAddresses(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_021", "5YJ3E1EA1NF000021")

	// MYR-447: the four address columns are ciphertext-only, so this path
	// needs a key on both the seeding Create and the read-back — exactly
	// as MYR-433 already required for the route trail.
	repo := store.NewDriveRepoWithEncryption(
		testPool, store.NoopMetrics{}, newTestEncryptor(t), testLogger())
	ctx := context.Background()

	seed := store.DriveRecord{
		ID: "drv_update", VehicleID: "veh_021", Date: "2026-07-17",
		StartTime: "2026-07-17T10:00:00Z", RoutePoints: json.RawMessage("[]"),
		StartAddress: "", EndAddress: "already set, must not be clobbered",
	}
	if err := repo.Create(ctx, seed); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name                                                 string
		id                                                   string
		startLocation, startAddress, endLocation, endAddress *string
		wantErr                                              error
	}{
		{
			name:          "sets only the nil-free columns, nil columns preserved",
			id:            "drv_update",
			startLocation: strPtr("Stonebriar"),
			startAddress:  strPtr("4220 Tributary Way, Frisco, TX"),
			endLocation:   nil,
			endAddress:    nil,
		},
		{
			name:          "missing row",
			id:            "does-not-exist",
			startLocation: strPtr("X"),
			startAddress:  strPtr("Y"),
			wantErr:       store.ErrDriveNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.UpdateAddresses(ctx, tt.id, tt.startLocation, tt.startAddress, tt.endLocation, tt.endAddress)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	got, err := repo.GetByID(ctx, "drv_update")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.StartLocation != "Stonebriar" {
		t.Errorf("StartLocation = %q, want %q", got.StartLocation, "Stonebriar")
	}
	if got.StartAddress != "4220 Tributary Way, Frisco, TX" {
		t.Errorf("StartAddress = %q, want %q", got.StartAddress, "4220 Tributary Way, Frisco, TX")
	}
	// nil endLocation/endAddress args must not clobber the pre-existing value.
	if got.EndAddress != "already set, must not be clobbered" {
		t.Errorf("EndAddress = %q, want preserved original value", got.EndAddress)
	}
}
