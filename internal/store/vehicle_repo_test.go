package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

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
