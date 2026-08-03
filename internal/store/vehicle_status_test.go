package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_GetStatus exercises the MYR-372 single-column status read
// against a real Postgres. It backs the reservation sweeper's availability
// re-check, so the two properties that matter are that it reports the column
// FAITHFULLY — the sweeper's whole job is to notice a car that entered service
// after the accept — and that a vehicle it cannot find is an ERROR rather than
// a fabricated status the sweeper would happily dispatch against.
func TestVehicleRepo_GetStatus(t *testing.T) {
	cleanTables(t, testPool)

	const (
		ownerID = "user_status_owner"
		parked  = "veh_status_001"
		inSvc   = "veh_status_002"
	)
	seedVehicleSummaryRow(t, parked, ownerID, "5YJ3E1EA1NF000T01", "Ready", "Model 3", 2024, "White",
		store.VehicleStatusParked, 61, 190)
	seedVehicleSummaryRow(t, inSvc, ownerID, "5YJ3E1EA1NF000T02", "AtShop", "Model 3", 2024, "Blue",
		store.VehicleStatusInService, 44, 150)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("reports the persisted status", func(t *testing.T) {
		tests := []struct {
			vehicle string
			want    store.VehicleStatus
		}{
			{vehicle: parked, want: store.VehicleStatusParked},
			{vehicle: inSvc, want: store.VehicleStatusInService},
		}
		for _, tt := range tests {
			got, err := repo.GetStatus(ctx, tt.vehicle)
			if err != nil {
				t.Fatalf("GetStatus(%s): %v", tt.vehicle, err)
			}
			if got != tt.want {
				t.Errorf("GetStatus(%s) = %q, want %q", tt.vehicle, got, tt.want)
			}
		}
	})

	t.Run("a status flip is visible to the next read", func(t *testing.T) {
		// The sweeper's entire reason for existing: the car was dispatchable
		// when the reservation was accepted and is not any more. A cached or
		// stale answer here would reinstate the bug MYR-372 closes.
		if _, err := testPool.Exec(ctx,
			`UPDATE "Vehicle" SET "status" = $2 WHERE "id" = $1`, parked, store.VehicleStatusInService); err != nil {
			t.Fatalf("flip status: %v", err)
		}

		got, err := repo.GetStatus(ctx, parked)
		if err != nil {
			t.Fatalf("GetStatus after flip: %v", err)
		}
		if got != store.VehicleStatusInService {
			t.Errorf("GetStatus after flip = %q, want %q", got, store.VehicleStatusInService)
		}
	})

	t.Run("an unknown vehicle is ErrVehicleNotFound, never a status", func(t *testing.T) {
		got, err := repo.GetStatus(ctx, "veh_does_not_exist")
		if !errors.Is(err, store.ErrVehicleNotFound) {
			t.Fatalf("GetStatus(unknown) error = %v, want ErrVehicleNotFound", err)
		}
		if got != "" {
			t.Errorf("GetStatus(unknown) returned status %q; absence must never be dressed as a value", got)
		}
	})
}
