package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

func TestDriveRepo_ListMissingAddresses(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_020", "5YJ3E1EA1NF000020")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	routePoints := json.RawMessage(`[
		{"lat":33.0860,"lng":-96.8522,"speed":0,"heading":0,"timestamp":"2026-07-17T10:00:00Z"},
		{"lat":33.1032,"lng":-96.8236,"speed":40,"heading":90,"timestamp":"2026-07-17T10:20:00Z"}
	]`)

	seeds := []store.DriveRecord{
		{
			ID: "drv_missing_start", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T10:00:00Z", RoutePoints: routePoints,
			StartAddress: "", EndAddress: "789 Elm St, Frisco, TX",
		},
		{
			ID: "drv_missing_end", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T11:00:00Z", RoutePoints: routePoints,
			StartAddress: "123 Main St, Frisco, TX", EndAddress: "",
		},
		{
			ID: "drv_fully_addressed", VehicleID: "veh_020", Date: "2026-07-17",
			StartTime: "2026-07-17T12:00:00Z", RoutePoints: routePoints,
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
		t.Errorf("drv_missing_end should be returned (endAddress empty)")
	}

	row := got["drv_missing_start"]
	if row.EndAddress != "789 Elm St, Frisco, TX" {
		t.Errorf("EndAddress = %q, want the already-populated value preserved", row.EndAddress)
	}
	if len(row.RoutePoints) == 0 {
		t.Error("RoutePoints should be populated so the caller can extract coordinates")
	}
}

func TestDriveRepo_UpdateAddresses(t *testing.T) {
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_021", "5YJ3E1EA1NF000021")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
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
