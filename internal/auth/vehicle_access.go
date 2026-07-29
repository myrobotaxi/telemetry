package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNoVehicleAccess reports that a caller is neither the owner of a vehicle
// nor the holder of an accepted share on it.
//
// It is a DENIAL, not a lookup failure: handlers map it to
// 403 vehicle_not_owned (or 404, where the endpoint's rule is to keep the
// vehicle's existence hidden), never to a viewer projection and never to a 500.
var ErrNoVehicleAccess = errors.New("no access to vehicle")

// querySharePermission resolves the tier one person holds over one vehicle
// through an accepted share (go_vehicle_shares, migration 0020). No row means
// no access — there is deliberately no default tier to fall back to.
const querySharePermission = `
SELECT permission FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status = 'accepted'
LIMIT 1`

// shareLookup is the consumer-site interface ResolveVehicleAccess uses to read
// an accepted share grant. Defined here so tests can swap the DB-backed
// implementation for a stub, mirroring vehicleOwnerLookup.
type shareLookup interface {
	// GetSharePermission returns the tier userID holds over vehicleID.
	// Returns ErrNoVehicleAccess when there is no accepted grant.
	GetSharePermission(ctx context.Context, userID, vehicleID string) (SharePermission, error)
}

// ResolveVehicleAccess resolves the caller's role for a vehicle AND, for a
// viewer, the share tier that role carries.
//
// Three outcomes, and only three:
//
//   - owner  — (RoleOwner, "", nil). An owner is not on a tier; they hold
//     everything, so the returned permission is empty and callers
//     MUST NOT feed it to AtLeast. Branch on the role first.
//   - viewer — (RoleViewer, tier, nil) when an accepted share exists.
//   - denied — (Role(""), "", ErrNoVehicleAccess).
//
// A vehicle that does not exist surfaces as an error from the owner lookup, NOT
// as a denial, so the handler layer can answer 404 rather than 403 — the
// existing rule that an unknown vehicle must never be distinguishable from one
// the caller merely cannot see is enforced at that layer, where the correct
// status for each endpoint is known.
func (a *JWTAuthenticator) ResolveVehicleAccess(ctx context.Context, userID, vehicleID string) (Role, SharePermission, error) {
	ownerID, err := a.ownerLookup.GetVehicleOwnerByID(ctx, vehicleID)
	if err != nil {
		return Role(""), SharePermission(""), fmt.Errorf("auth.ResolveVehicleAccess: vehicle %s: %w", vehicleID, err)
	}
	if ownerID == userID {
		return RoleOwner, SharePermission(""), nil
	}

	if a.shares == nil {
		// No share lookup configured — fail closed. The pre-MYR-184
		// behaviour of returning RoleViewer here is exactly the hole this
		// change exists to close.
		return Role(""), SharePermission(""), ErrNoVehicleAccess
	}

	permission, err := a.shares.GetSharePermission(ctx, userID, vehicleID)
	if err != nil {
		if errors.Is(err, ErrNoVehicleAccess) {
			return Role(""), SharePermission(""), ErrNoVehicleAccess
		}
		return Role(""), SharePermission(""), fmt.Errorf("auth.ResolveVehicleAccess(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return RoleViewer, permission, nil
}

// InvalidateVehicles drops the cached access set for userID so the next
// GetUserVehicles refetches from the database.
//
// CACHE POLICY (MYR-184). The access set is cached for vehicleCacheTTL, so a
// share that appears or disappears would otherwise take up to that long to be
// visible. The two transitions are treated differently, deliberately:
//
//   - REDEEM busts the REDEEMER's entry, so the car they just joined shows up
//     in their list on the very next request rather than minutes later. A
//     "you're in!" screen followed by an empty garage is the feature failing.
//   - REVOKE busts the REVOKED VIEWER's entry, so access ends promptly. This
//     one is a security property, not a niceness: a bounded window where a
//     revoked viewer still resolves the vehicle is a real (if brief) exposure.
//
// What this does NOT do is make the cache authoritative across processes. The
// entry is per-instance, so on a multi-machine deployment a bust only clears
// the machine that served the mutation and the others still lapse on TTL. The
// app runs a single Fly machine today (fly.toml declares one [[vm]] with no
// scale-out), so the bust is complete in practice; if that changes, revocation
// needs a cross-instance signal (the LISTEN/NOTIFY channel the user-deletion
// path already uses is the obvious carrier).
func (a *JWTAuthenticator) InvalidateVehicles(userID string) {
	if a.cache == nil || userID == "" {
		return
	}
	a.cache.invalidate(userID)
}

// GetSharePermission reads an accepted share grant from go_vehicle_shares.
// Returns ErrNoVehicleAccess when there is none.
func (q *pgVehicleQuerier) GetSharePermission(ctx context.Context, userID, vehicleID string) (SharePermission, error) {
	var permission string
	err := q.pool.QueryRow(ctx, querySharePermission, vehicleID, userID).Scan(&permission)
	switch {
	case err == nil:
		return SharePermission(permission), nil
	case errors.Is(err, pgx.ErrNoRows):
		return SharePermission(""), ErrNoVehicleAccess
	default:
		return SharePermission(""), fmt.Errorf("pgVehicleQuerier.GetSharePermission(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}
