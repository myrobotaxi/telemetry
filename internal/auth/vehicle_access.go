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

// queryShareGrant resolves the CAPABILITY FLAGS one person holds over one
// vehicle through an accepted share (go_vehicle_shares, migrations 0020 +
// 0024). No row means no access — there is deliberately no default grant to
// fall back to.
//
// `suspended_at IS NULL` IS AN ACCESS-CONTROL PREDICATE, not a filter for
// tidiness. A suspended grant must be indistinguishable from no grant on every
// viewer-facing path (MYR-369), so it is excluded HERE, in the statement, next
// to the status check it belongs with — rather than fetched and then discarded
// in Go, which would leave a window for some future caller to read the row and
// act on its flags.
//
// `permission` is deliberately NOT selected. It is the invite-time preset and
// derived output; no gate reads it, and selecting it here would put it within
// reach of one.
const queryShareGrant = `
SELECT allow_rides FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2
  AND status = 'accepted' AND suspended_at IS NULL
LIMIT 1`

// shareLookup is the consumer-site interface ResolveVehicleAccess uses to read
// an accepted share grant. Defined here so tests can swap the DB-backed
// implementation for a stub, mirroring vehicleOwnerLookup.
type shareLookup interface {
	// GetShareGrant returns the capability set userID holds over vehicleID.
	// Returns ErrNoVehicleAccess when there is no accepted grant OR when the
	// grant exists but is suspended — the two are deliberately the same
	// answer.
	GetShareGrant(ctx context.Context, userID, vehicleID string) (ShareGrant, error)
}

// ResolveVehicleAccess resolves the caller's role for a vehicle AND, for a
// viewer, the capability set that role carries.
//
// Three outcomes, and only three:
//
//   - owner  — (RoleOwner, ShareGrant{}, nil). An owner holds no grant; they
//     hold everything. The returned grant is the ZERO VALUE, which
//     is the most restrictive one, so a caller that forgets to
//     branch on the role denies rather than over-grants. Branch on
//     the role first.
//   - viewer — (RoleViewer, grant, nil) when a live accepted share exists.
//   - denied — (Role(""), ShareGrant{}, ErrNoVehicleAccess).
//
// A SUSPENDED GRANT IS THE DENIED CASE, not a viewer with an empty capability
// set (MYR-369). The distinction matters: a suspended grant must be
// indistinguishable from no grant at all on every viewer-facing surface, and
// returning RoleViewer for one would put the caller inside the viewer branch of
// every handler — masked, but present.
//
// A vehicle that does not exist surfaces as an error from the owner lookup, NOT
// as a denial, so the handler layer can answer 404 rather than 403 — the
// existing rule that an unknown vehicle must never be distinguishable from one
// the caller merely cannot see is enforced at that layer, where the correct
// status for each endpoint is known.
func (a *JWTAuthenticator) ResolveVehicleAccess(ctx context.Context, userID, vehicleID string) (Role, ShareGrant, error) {
	ownerID, err := a.ownerLookup.GetVehicleOwnerByID(ctx, vehicleID)
	if err != nil {
		return Role(""), ShareGrant{}, fmt.Errorf("auth.ResolveVehicleAccess: vehicle %s: %w", vehicleID, err)
	}
	if ownerID == userID {
		return RoleOwner, ShareGrant{}, nil
	}

	if a.shares == nil {
		// No share lookup configured — fail closed. The pre-MYR-184
		// behaviour of returning RoleViewer here is exactly the hole this
		// change exists to close.
		return Role(""), ShareGrant{}, ErrNoVehicleAccess
	}

	grant, err := a.shares.GetShareGrant(ctx, userID, vehicleID)
	if err != nil {
		if errors.Is(err, ErrNoVehicleAccess) {
			return Role(""), ShareGrant{}, ErrNoVehicleAccess
		}
		return Role(""), ShareGrant{}, fmt.Errorf("auth.ResolveVehicleAccess(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	// SECOND, INDEPENDENT SUSPENSION GATE. The statement already excludes
	// suspended rows, so this cannot fire through the DB-backed lookup — it
	// fires for a stub, a future lookup implementation, or a statement
	// somebody edits. An access-control invariant that holds only because
	// one WHERE clause is correct is one edit from not holding.
	if !grant.Active() {
		return Role(""), ShareGrant{}, ErrNoVehicleAccess
	}
	return RoleViewer, grant, nil
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
//   - PATCH busts the GRANTEE's entry (MYR-369), for the same reason as revoke
//     and with the same urgency. A suspension REMOVES the vehicle from that
//     person's access set, so it is a revoke in every respect that matters to
//     this cache; leaving the entry warm would let a suspended viewer keep
//     resolving the car for up to the TTL, which is the exact exposure the
//     revoke bust exists to prevent. The un-suspend and the allow_rides
//     directions are busted by the same call — not because they are dangerous
//     (they widen or narrow one capability, and the ride gates read the row
//     directly rather than the cache) but because ONE unconditional bust on
//     mutation is a rule that cannot be got wrong, and a bust conditional on
//     which field moved is a rule that can.
//
// What this does NOT do is make the cache authoritative across processes. The
// entry is per-instance, so on a multi-machine deployment a bust only clears
// the machine that served the mutation and the others still lapse on TTL. The
// app runs a single Fly machine today (fly.toml declares one [[vm]] with no
// scale-out), so the bust is complete in practice; if that changes, revocation
// needs a cross-instance signal (the LISTEN/NOTIFY channel the user-deletion
// path already uses is the obvious carrier).
func (a *JWTAuthenticator) InvalidateVehicles(userID string) {
	// Nil-receiver tolerant: dev mode has no JWTAuthenticator, and a
	// cache-bust that silently does nothing is strictly better than a panic
	// on a path whose whole job is housekeeping.
	if a == nil || a.cache == nil || userID == "" {
		return
	}
	a.cache.invalidate(userID)
}

// GetShareGrant reads a LIVE accepted share grant from go_vehicle_shares.
// Returns ErrNoVehicleAccess when there is none — which covers "no grant",
// "revoked" and "suspended" alike, because the statement's WHERE clause makes
// all three produce zero rows.
//
// Suspended is therefore always false on a grant this returns. It is set on the
// returned struct anyway (rather than left implicitly zero) nowhere at all: the
// row simply does not come back, which is the stronger property.
func (q *pgVehicleQuerier) GetShareGrant(ctx context.Context, userID, vehicleID string) (ShareGrant, error) {
	var allowRides bool
	err := q.pool.QueryRow(ctx, queryShareGrant, vehicleID, userID).Scan(&allowRides)
	switch {
	case err == nil:
		return ShareGrant{AllowRides: allowRides}, nil
	case errors.Is(err, pgx.ErrNoRows):
		return ShareGrant{}, ErrNoVehicleAccess
	default:
		return ShareGrant{}, fmt.Errorf("pgVehicleQuerier.GetShareGrant(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}
