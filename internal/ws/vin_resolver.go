package ws

import "context"

// VINResolver maps a vehicle's VIN to its database identifier (cuid).
// All client-facing messages use the database vehicleID, never the raw VIN.
// Defined at the consumer site; implemented by store.VehicleRepo or a
// caching layer.
type VINResolver interface {
	GetByVIN(ctx context.Context, vin string) (vehicleID string, err error)
}
