package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

func TestDriveRepo_Create(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_010", "5YJ3E1EA1NF000010")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	tests := []struct {
		name    string
		drive   store.DriveRecord
		wantErr bool
	}{
		{
			name: "valid drive",
			drive: store.DriveRecord{
				ID:               "drv_001",
				VehicleID:        "veh_010",
				Date:             "2026-03-17",
				StartTime:        "2026-03-17T10:00:00Z",
				EndTime:          "",
				StartLocation:    "33.0975,-96.8214",
				StartAddress:     "123 Main St, Plano, TX",
				StartChargeLevel: 85,
				RoutePoints:      json.RawMessage("[]"),
			},
			wantErr: false,
		},
		{
			name: "nil route points defaults to empty array",
			drive: store.DriveRecord{
				ID:        "drv_002",
				VehicleID: "veh_010",
				Date:      "2026-03-17",
				StartTime: "2026-03-17T11:00:00Z",
			},
			wantErr: false,
		},
		{
			name: "duplicate ID fails",
			drive: store.DriveRecord{
				ID:        "drv_001",
				VehicleID: "veh_010",
				Date:      "2026-03-17",
				StartTime: "2026-03-17T12:00:00Z",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.Create(ctx, tt.drive)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDriveRepo_GetByID(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_011", "5YJ3E1EA1NF000011")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Seed a drive.
	drive := store.DriveRecord{
		ID:               "drv_010",
		VehicleID:        "veh_011",
		Date:             "2026-03-17",
		StartTime:        "2026-03-17T10:00:00Z",
		StartLocation:    "33.0975,-96.8214",
		StartAddress:     "123 Main St",
		StartChargeLevel: 80,
		RoutePoints:      json.RawMessage("[]"),
	}
	if err := repo.Create(ctx, drive); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	tests := []struct {
		name          string
		id            string
		wantVehicleID string
		wantErr       error
	}{
		{
			name:          "existing drive",
			id:            "drv_010",
			wantVehicleID: "veh_011",
		},
		{
			name:    "missing drive",
			id:      "nonexistent",
			wantErr: store.ErrDriveNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := repo.GetByID(ctx, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.VehicleID != tt.wantVehicleID {
				t.Errorf("VehicleID = %q, want %q", d.VehicleID, tt.wantVehicleID)
			}
		})
	}
}

func TestDriveRepo_AppendRoutePoints(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_012", "5YJ3E1EA1NF000012")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Seed a drive.
	drive := store.DriveRecord{
		ID:        "drv_020",
		VehicleID: "veh_012",
		Date:      "2026-03-17",
		StartTime: "2026-03-17T10:00:00Z",
	}
	if err := repo.Create(ctx, drive); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	tests := []struct {
		name    string
		driveID string
		points  []store.RoutePointRecord
		wantErr error
	}{
		{
			name:    "append single point",
			driveID: "drv_020",
			points: []store.RoutePointRecord{
				{Latitude: 33.0975, Longitude: -96.8214, Speed: 65.0, Heading: 245, Timestamp: "2026-03-17T10:01:00Z"},
			},
		},
		{
			name:    "append multiple points",
			driveID: "drv_020",
			points: []store.RoutePointRecord{
				{Latitude: 33.0980, Longitude: -96.8220, Speed: 70.0, Heading: 250, Timestamp: "2026-03-17T10:02:00Z"},
				{Latitude: 33.0985, Longitude: -96.8226, Speed: 68.0, Heading: 248, Timestamp: "2026-03-17T10:03:00Z"},
			},
		},
		{
			name:    "empty points is no-op",
			driveID: "drv_020",
			points:  nil,
		},
		{
			name:    "missing drive",
			driveID: "nonexistent",
			points: []store.RoutePointRecord{
				{Latitude: 33.0, Longitude: -96.0, Timestamp: "2026-03-17T10:04:00Z"},
			},
			wantErr: store.ErrDriveNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.AppendRoutePoints(ctx, tt.driveID, tt.points)
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

	// Verify all 3 points were appended.
	d, err := repo.GetByID(ctx, "drv_020")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	var points []store.RoutePointRecord
	if err := json.Unmarshal(d.RoutePoints, &points); err != nil {
		t.Fatalf("unmarshal route points: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("route points count = %d, want 3", len(points))
	}
	if points[0].Speed != 65.0 {
		t.Errorf("first point speed = %f, want 65.0", points[0].Speed)
	}
	if points[2].Speed != 68.0 {
		t.Errorf("third point speed = %f, want 68.0", points[2].Speed)
	}
}

func TestDriveRepo_Complete(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_013", "5YJ3E1EA1NF000013")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Seed a drive.
	drive := store.DriveRecord{
		ID:               "drv_030",
		VehicleID:        "veh_013",
		Date:             "2026-03-17",
		StartTime:        "2026-03-17T10:00:00Z",
		StartLocation:    "33.0975,-96.8214",
		StartAddress:     "123 Main St",
		StartChargeLevel: 85,
	}
	if err := repo.Create(ctx, drive); err != nil {
		t.Fatalf("seed drive: %v", err)
	}

	tests := []struct {
		name    string
		driveID string
		stats   store.DriveCompletion
		wantErr error
	}{
		{
			name:    "complete drive",
			driveID: "drv_030",
			stats: store.DriveCompletion{
				EndTime:         "2026-03-17T10:45:00Z",
				EndLocation:     "33.1500,-96.8800",
				EndAddress:      "456 Oak Ave, Dallas, TX",
				DistanceMiles:   12.5,
				DurationMinutes: 45,
				AvgSpeedMph:     35.2,
				MaxSpeedMph:     72.0,
				EnergyUsedKwh:   4.2,
				EndChargeLevel:  78,
				FsdMiles:        10.0,
				FsdPercentage:   80.0,
				Interventions:   1,
			},
		},
		{
			name:    "missing drive",
			driveID: "nonexistent",
			stats: store.DriveCompletion{
				EndTime: "2026-03-17T11:00:00Z",
			},
			wantErr: store.ErrDriveNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := repo.Complete(ctx, tt.driveID, tt.stats)
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

	// Verify completion was applied.
	d, err := repo.GetByID(ctx, "drv_030")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if d.EndTime != "2026-03-17T10:45:00Z" {
		t.Errorf("EndTime = %q, want %q", d.EndTime, "2026-03-17T10:45:00Z")
	}
	if d.DistanceMiles != 12.5 {
		t.Errorf("DistanceMiles = %f, want 12.5", d.DistanceMiles)
	}
	if d.DurationMinutes != 45 {
		t.Errorf("DurationMinutes = %d, want 45", d.DurationMinutes)
	}
	if d.EndChargeLevel != 78 {
		t.Errorf("EndChargeLevel = %d, want 78", d.EndChargeLevel)
	}
	if d.Interventions != 1 {
		t.Errorf("Interventions = %d, want 1", d.Interventions)
	}
}
