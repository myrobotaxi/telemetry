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

// RequesterFirstName resolves a rider's FIRST NAME for the owner's
// "time to head out" push (MYR-535). Same repo as the nickname read because it
// serves the same consumer the same way: the push notifier resolving one
// display string at delivery time, keeping the dispatch pipeline's events
// summary-only (RideDueEvent deliberately carries ids and instants, no PII).
//
// The identity sources are the MYR-229 three-source ladder, reused verbatim
// (queryOwnerFirstNameSources — the query resolves ANY user id; "owner" is its
// first caller's role, not a constraint). The terminal differs on purpose:
// name token → email local-part → "" — the push copy has its own anonymous
// fallback title, and handing it a literal like "Rider" would render
// "Rider's pickup is coming up". The resolved value is P1 and is never logged.
func (r *VehicleNameRepo) RequesterFirstName(ctx context.Context, userID string) (string, error) {
	if strings.TrimSpace(userID) == "" {
		return "", fmt.Errorf("store.RequesterFirstName: empty user id")
	}
	var name, email *string
	err := r.pool.QueryRow(ctx, queryOwnerFirstNameSources, userID).Scan(&name, &email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store.RequesterFirstName(%s): %w", userID, err)
	}
	return requesterFirstNameToken(name, email), nil
}

// requesterFirstNameToken is the pure ladder: first whitespace-separated token
// of the display name → email local-part → "". Pure — unit-tested without a
// database.
func requesterFirstNameToken(name, email *string) string {
	if name != nil {
		if token := firstNameToken(*name); token != "" {
			return token
		}
	}
	if email != nil {
		if local := emailLocalPart(*email); local != "" {
			return local
		}
	}
	return ""
}
