package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A deliberately LEAN vehicle-nickname read (MYR-186).
//
// VehicleRepo.GetByID is the wide snapshot read — ~70 columns plus a join onto
// go_vehicle_control_state — because the sheet that calls it needs all of them.
// The push notifier needs exactly one string to build copy like "Blue Whale
// can't take this ride", several times per ride, so it gets its own one-column
// statement instead of paying for the snapshot. Per AGENTS.md, wide selects
// belong to detail/edit handlers.
//
// The "Vehicle" table is Prisma-owned and READ-ONLY here (CG-DL-9): this is a
// SELECT and nothing else.

// queryVehicleName reads a vehicle's owner-chosen nickname by cuid.
const queryVehicleName = `SELECT COALESCE("name", '') FROM "Vehicle" WHERE "id" = $1`

// VehicleNameRepo resolves a vehicle cuid to its display nickname.
type VehicleNameRepo struct {
	pool *pgxpool.Pool
}

// NewVehicleNameRepo builds the resolver over the given pool.
func NewVehicleNameRepo(pool *pgxpool.Pool) *VehicleNameRepo {
	return &VehicleNameRepo{pool: pool}
}

// VehicleName returns the vehicle's nickname, or "" when the car has no name
// set. A vehicle that does not exist yields ErrVehicleNotFound; every caller so
// far treats that as "use the generic label" rather than as a failure, but the
// distinction is preserved so a future caller can tell the two apart.
func (r *VehicleNameRepo) VehicleName(ctx context.Context, vehicleID string) (string, error) {
	if strings.TrimSpace(vehicleID) == "" {
		return "", fmt.Errorf("store.VehicleName: empty vehicle id")
	}

	var name string
	if err := r.pool.QueryRow(ctx, queryVehicleName, vehicleID).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("store.VehicleName(%s): %w", vehicleID, ErrVehicleNotFound)
		}
		return "", fmt.Errorf("store.VehicleName(%s): %w", vehicleID, err)
	}
	return name, nil
}
