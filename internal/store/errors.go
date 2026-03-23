package store

import (
	"errors"
	"fmt"

	"github.com/tnando/my-robo-taxi-telemetry/pkg/sdk"
)

var (
	// ErrVehicleNotFound is returned when a vehicle lookup finds no matching row.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrVehicleNotFound = fmt.Errorf("vehicle %w", sdk.ErrNotFound)

	// ErrDriveNotFound is returned when a drive lookup finds no matching row.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrDriveNotFound = fmt.Errorf("drive %w", sdk.ErrNotFound)

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
