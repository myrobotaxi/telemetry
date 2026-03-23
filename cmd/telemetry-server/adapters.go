package main

import (
	"context"
	"fmt"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// vinResolverAdapter adapts store.VehicleRepo (returns Vehicle) to the
// ws.VINResolver interface (returns vehicleID string).
type vinResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return v.ID, nil
}

// vehicleOwnerAdapter adapts store.VehicleRepo to the
// telemetry.VehicleOwnerLookup interface (returns owning user ID).
type vehicleOwnerAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleOwnerAdapter) GetVehicleOwner(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve vehicle owner: %w", err)
	}
	return v.UserID, nil
}
