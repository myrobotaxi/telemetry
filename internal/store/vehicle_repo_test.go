package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// catalogFields bundles the seven DB-backed columns that MYR-24 promoted
// out of spec-only status. Kept adjacent to the repo tests so the test
// cases stay close to the exact shape we're asserting against.
type catalogFields struct {
	model              string
	year               int
	color              string
	locationName       string
	locationAddress    string
	fsdMilesSinceReset float64
	destinationAddress *string
}

func TestVehicleRepo_GetByVIN(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_001", "5YJ3E1EA1NF000001")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name    string
		vin     string
		wantID  string
		wantErr error
	}{
		{
			name:   "existing vehicle",
			vin:    "5YJ3E1EA1NF000001",
			wantID: "veh_001",
		},
		{
			name:    "missing vehicle",
			vin:     "NONEXISTENT",
			wantErr: store.ErrVehicleNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := repo.GetByVIN(ctx, tt.vin)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", v.ID, tt.wantID)
			}
			if v.VIN != tt.vin {
				t.Errorf("VIN = %q, want %q", v.VIN, tt.vin)
			}
			if v.Status != store.VehicleStatusParked {
				t.Errorf("Status = %q, want %q", v.Status, store.VehicleStatusParked)
			}
		})
	}
}

func TestVehicleRepo_GetByID(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_002", "5YJ3E1EA1NF000002")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name    string
		id      string
		wantVIN string
		wantErr error
	}{
		{
			name:    "existing vehicle",
			id:      "veh_002",
			wantVIN: "5YJ3E1EA1NF000002",
		},
		{
			name:    "missing vehicle",
			id:      "nonexistent",
			wantErr: store.ErrVehicleNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := repo.GetByID(ctx, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.VIN != tt.wantVIN {
				t.Errorf("VIN = %q, want %q", v.VIN, tt.wantVIN)
			}
		})
	}
}

func TestVehicleRepo_GetIDsByVIN(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_ids_001", "5YJ3E1EA1NF000IDS")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name       string
		vin        string
		wantID     string
		wantUserID string
		wantErr    error
	}{
		{
			name:       "existing vehicle returns id and userId",
			vin:        "5YJ3E1EA1NF000IDS",
			wantID:     "veh_ids_001",
			wantUserID: "user_001", // seedVehicle hardcodes this owner
		},
		{
			name:    "missing vehicle returns ErrVehicleNotFound",
			vin:     "NONEXISTENT",
			wantErr: store.ErrVehicleNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, userID, err := repo.GetIDsByVIN(ctx, tt.vin)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			if userID != tt.wantUserID {
				t.Errorf("userID = %q, want %q", userID, tt.wantUserID)
			}
		})
	}
}

func TestVehicleRepo_UpdateTelemetry(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_003", "5YJ3E1EA1NF000003")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	speed := 65
	lat := 33.0975
	lng := -96.8214
	heading := 245
	gear := "D"

	tests := []struct {
		name    string
		vin     string
		update  store.VehicleUpdate
		wantErr error
	}{
		{
			name: "partial update with speed and location",
			vin:  "5YJ3E1EA1NF000003",
			update: store.VehicleUpdate{
				Speed:       &speed,
				Latitude:    &lat,
				Longitude:   &lng,
				Heading:     &heading,
				LastUpdated: time.Now(),
			},
		},
		{
			name: "update with gear position",
			vin:  "5YJ3E1EA1NF000003",
			update: store.VehicleUpdate{
				GearPosition: &gear,
				LastUpdated:  time.Now(),
			},
		},
		{
			name: "empty update is no-op",
			vin:  "5YJ3E1EA1NF000003",
			update: store.VehicleUpdate{
				LastUpdated: time.Now(),
			},
		},
		{
			name: "missing vehicle",
			vin:  "NONEXISTENT",
			update: store.VehicleUpdate{
				Speed:       &speed,
				LastUpdated: time.Now(),
			},
			wantErr: store.ErrVehicleNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.UpdateTelemetry(ctx, tt.vin, tt.update)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// Verify the updates were applied.
	v, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF000003")
	if err != nil {
		t.Fatalf("GetByVIN after update: %v", err)
	}
	if v.Speed != speed {
		t.Errorf("Speed = %d, want %d", v.Speed, speed)
	}
	if v.Heading != heading {
		t.Errorf("Heading = %d, want %d", v.Heading, heading)
	}
	if v.GearPosition == nil || *v.GearPosition != gear {
		t.Errorf("GearPosition = %v, want %q", v.GearPosition, gear)
	}
}

// TestVehicleRepo_CatalogFields verifies that the seven catalog columns
// promoted by MYR-24 (model, year, color, fsdMilesSinceReset, locationName,
// locationAddress, destinationAddress) are loaded on GetByVIN, GetByID, and
// ListByUser. Before MYR-24 none of these columns were scanned; the test
// is the regression guard for the drift that §7.2 of
// docs/contracts/vehicle-state-schema.md used to track.
func TestVehicleRepo_CatalogFields(t *testing.T) {
	cleanTables(t, testPool)

	seedVehicleWithCatalog(t, testPool, "veh_cat_001", "5YJ3E1EA1NF000C01", catalogFields{
		model:              "Model 3",
		year:               2024,
		color:              "Midnight Silver Metallic",
		locationName:       "Home",
		locationAddress:    "123 Market St, San Francisco, CA",
		fsdMilesSinceReset: 412.7,
		destinationAddress: strPtr("2001 Market St, San Francisco, CA 94114"),
	})
	// Second vehicle: minimal catalog (defaults) + null destinationAddress to
	// exercise the nullable branch of the scan.
	seedVehicleWithCatalog(t, testPool, "veh_cat_002", "5YJ3E1EA1NF000C02", catalogFields{
		model:              "Model Y",
		year:               2023,
		color:              "Pearl White",
		locationName:       "",
		locationAddress:    "",
		fsdMilesSinceReset: 0,
		destinationAddress: nil,
	})

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name string
		vin  string
		want catalogFields
	}{
		{
			name: "full catalog populated",
			vin:  "5YJ3E1EA1NF000C01",
			want: catalogFields{
				model:              "Model 3",
				year:               2024,
				color:              "Midnight Silver Metallic",
				locationName:       "Home",
				locationAddress:    "123 Market St, San Francisco, CA",
				fsdMilesSinceReset: 412.7,
				destinationAddress: strPtr("2001 Market St, San Francisco, CA 94114"),
			},
		},
		{
			name: "nullable destinationAddress with empty location strings",
			vin:  "5YJ3E1EA1NF000C02",
			want: catalogFields{
				model:              "Model Y",
				year:               2023,
				color:              "Pearl White",
				locationName:       "",
				locationAddress:    "",
				fsdMilesSinceReset: 0,
				destinationAddress: nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := repo.GetByVIN(ctx, tt.vin)
			if err != nil {
				t.Fatalf("GetByVIN(%s): %v", tt.vin, err)
			}
			assertCatalog(t, v, tt.want)

			byID, err := repo.GetByID(ctx, v.ID)
			if err != nil {
				t.Fatalf("GetByID(%s): %v", v.ID, err)
			}
			assertCatalog(t, byID, tt.want)
		})
	}

	t.Run("ListByUser loads catalog fields", func(t *testing.T) {
		vehicles, err := repo.ListByUser(ctx, "user_001")
		if err != nil {
			t.Fatalf("ListByUser: %v", err)
		}
		if len(vehicles) != 2 {
			t.Fatalf("ListByUser returned %d vehicles, want 2", len(vehicles))
		}
		byVIN := map[string]store.Vehicle{}
		for _, v := range vehicles {
			byVIN[v.VIN] = v
		}
		assertCatalog(t, byVIN["5YJ3E1EA1NF000C01"], catalogFields{
			model:              "Model 3",
			year:               2024,
			color:              "Midnight Silver Metallic",
			locationName:       "Home",
			locationAddress:    "123 Market St, San Francisco, CA",
			fsdMilesSinceReset: 412.7,
			destinationAddress: strPtr("2001 Market St, San Francisco, CA 94114"),
		})
		assertCatalog(t, byVIN["5YJ3E1EA1NF000C02"], catalogFields{
			model:              "Model Y",
			year:               2023,
			color:              "Pearl White",
			locationName:       "",
			locationAddress:    "",
			fsdMilesSinceReset: 0,
			destinationAddress: nil,
		})
	})
}

func assertCatalog(t *testing.T, v store.Vehicle, want catalogFields) {
	t.Helper()
	if v.Model != want.model {
		t.Errorf("Model = %q, want %q", v.Model, want.model)
	}
	if v.Year != want.year {
		t.Errorf("Year = %d, want %d", v.Year, want.year)
	}
	if v.Color != want.color {
		t.Errorf("Color = %q, want %q", v.Color, want.color)
	}
	if v.LocationName != want.locationName {
		t.Errorf("LocationName = %q, want %q", v.LocationName, want.locationName)
	}
	if v.LocationAddress != want.locationAddress {
		t.Errorf("LocationAddress = %q, want %q", v.LocationAddress, want.locationAddress)
	}
	if v.FsdMilesSinceReset != want.fsdMilesSinceReset {
		t.Errorf("FsdMilesSinceReset = %v, want %v", v.FsdMilesSinceReset, want.fsdMilesSinceReset)
	}
	switch {
	case want.destinationAddress == nil && v.DestinationAddress != nil:
		t.Errorf("DestinationAddress = %q, want nil", *v.DestinationAddress)
	case want.destinationAddress != nil && v.DestinationAddress == nil:
		t.Errorf("DestinationAddress = nil, want %q", *want.destinationAddress)
	case want.destinationAddress != nil && *v.DestinationAddress != *want.destinationAddress:
		t.Errorf("DestinationAddress = %q, want %q", *v.DestinationAddress, *want.destinationAddress)
	}
}

func strPtr(s string) *string { return &s }

// TestVehicleRepo_ChargeFieldsRoundTrip exercises the DB-persistence path
// for the v1 charge atomic group members chargeState (proto 179, enum) and
// timeToFull (proto 43, hours decimal) added by MYR-41. Confirms a vehicle
// seeded without charge fields starts as null on both, an UpdateTelemetry
// writes them, and a subsequent GetByVIN reads them back exactly.
func TestVehicleRepo_ChargeFieldsRoundTrip(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_charge_001", "5YJ3E1EA1NF00CR01")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Initial state: both columns NULL (seedVehicle does not populate them).
	v, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00CR01")
	if err != nil {
		t.Fatalf("GetByVIN initial: %v", err)
	}
	if v.ChargeState != nil {
		t.Errorf("initial ChargeState = %q, want nil", *v.ChargeState)
	}
	if v.TimeToFull != nil {
		t.Errorf("initial TimeToFull = %v, want nil", *v.TimeToFull)
	}

	// UpdateTelemetry with both charge fields.
	chargeState := "Charging"
	timeToFull := 1.5
	err = repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00CR01", store.VehicleUpdate{
		ChargeState: &chargeState,
		TimeToFull:  &timeToFull,
		LastUpdated: time.Now(),
	})
	if err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	// GetByVIN reads the persisted values.
	v, err = repo.GetByVIN(ctx, "5YJ3E1EA1NF00CR01")
	if err != nil {
		t.Fatalf("GetByVIN after update: %v", err)
	}
	if v.ChargeState == nil || *v.ChargeState != "Charging" {
		t.Errorf("ChargeState = %v, want Charging", v.ChargeState)
	}
	if v.TimeToFull == nil || *v.TimeToFull != 1.5 {
		t.Errorf("TimeToFull = %v, want 1.5", v.TimeToFull)
	}
}

func TestVehicleRepo_UpdateStatus(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_004", "5YJ3E1EA1NF000004")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name    string
		vin     string
		status  store.VehicleStatus
		wantErr error
	}{
		{
			name:   "set driving",
			vin:    "5YJ3E1EA1NF000004",
			status: store.VehicleStatusDriving,
		},
		{
			name:   "set charging",
			vin:    "5YJ3E1EA1NF000004",
			status: store.VehicleStatusCharging,
		},
		{
			name:    "missing vehicle",
			vin:     "NONEXISTENT",
			status:  store.VehicleStatusOffline,
			wantErr: store.ErrVehicleNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.UpdateStatus(ctx, tt.vin, tt.status)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify status was updated.
			v, err := repo.GetByVIN(ctx, tt.vin)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if v.Status != tt.status {
				t.Errorf("Status = %q, want %q", v.Status, tt.status)
			}
		})
	}
}
