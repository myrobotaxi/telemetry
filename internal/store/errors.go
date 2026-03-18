package store

import "errors"

var (
	// ErrVehicleNotFound is returned when a vehicle lookup finds no matching row.
	ErrVehicleNotFound = errors.New("vehicle not found")

	// ErrDriveNotFound is returned when a drive lookup finds no matching row.
	ErrDriveNotFound = errors.New("drive not found")

	// ErrDatabaseClosed is returned when an operation is attempted on a
	// closed database connection pool.
	ErrDatabaseClosed = errors.New("database connection closed")
)

// redactVIN returns a VIN with only the last 4 characters visible.
// Used in error messages to avoid leaking full VINs into logs.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}
