// Single-column vehicle status read for the reservation sweeper (MYR-372).

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryVehicleStatus reads one vehicle's persisted status.
//
// A READ of the Prisma-owned "Vehicle" table on its primary key, which needs
// no carve-out — the data-lifecycle.md §1.4 rules constrain WRITES.
//
// It exists rather than reusing queryVehicleByID because the sweeper wants one
// column and that statement returns thirty-one across a LEFT JOIN of the
// control-state side table. The same argument queryRideShareEnabled makes:
// the list and snapshot paths already carry what they need, and this is for the
// caller holding a vehicle id and one question.
//
// Deliberately NOT `SELECT ... WHERE status NOT IN ('in_service','offline')`.
// The set of undispatchable statuses is a POLICY, spelled once in
// internal/telemetry's vehicleAvailability; encoding it as a SQL literal here
// would make this the second definition and the one nobody thinks to update.
// This statement answers what the column says, nothing more.
const queryVehicleStatus = `SELECT "status" FROM "Vehicle" WHERE "id" = $1`

// GetStatus returns one vehicle's persisted status.
//
// Unknown vehicle ids are ErrVehicleNotFound, NOT a fabricated status. This is
// the opposite reading from RideShareEnabled, and deliberately so: that method
// probes a Go-owned side table which knows nothing about which vehicles exist,
// whereas this one reads the vehicle row itself, where absence is a real and
// reportable fact. The sweeper treats the error as "unknown, therefore hold",
// which is the recoverable answer either way.
func (r *VehicleRepo) GetStatus(ctx context.Context, vehicleID string) (VehicleStatus, error) {
	start := time.Now()
	var status VehicleStatus
	err := r.pool.QueryRow(ctx, queryVehicleStatus, vehicleID).Scan(&status)
	r.metrics.ObserveQueryDuration("vehicle.get_status", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError("vehicle.get_status")
		return "", fmt.Errorf("VehicleRepo.GetStatus(%s): %w", vehicleID, ErrVehicleNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_status")
		return "", fmt.Errorf("VehicleRepo.GetStatus(%s): %w", vehicleID, err)
	}
	return status, nil
}
