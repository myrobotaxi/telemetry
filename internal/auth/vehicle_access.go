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

// rideMembershipLookup is the consumer-site interface ResolveVehicleAccess uses
// to read the MYR-540 group-ride membership source. Separate from shareLookup
// because it is a different KIND of access — ride-scoped and self-expiring
// rather than a standing grant — and keeping the two apart is what stops a
// future edit granting a member a share's capabilities by accident.
type rideMembershipLookup interface {
	// IsRidingVehicle reports whether userID holds a LIVE group-ride membership
	// on a ride being served by vehicleID. Errors are lookup failures, never
	// denials: "no" is (false, nil).
	IsRidingVehicle(ctx context.Context, userID, vehicleID string) (bool, error)
}

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
		return a.resolveRidingMember(ctx, userID, vehicleID, ErrNoVehicleAccess)
	}

	grant, err := a.shares.GetShareGrant(ctx, userID, vehicleID)
	if err != nil {
		if errors.Is(err, ErrNoVehicleAccess) {
			// MYR-540: a share is not the only way to be a non-owner with
			// business here. Before denying, ask whether the caller is RIDING in
			// this car right now on a group ride they joined.
			return a.resolveRidingMember(ctx, userID, vehicleID, ErrNoVehicleAccess)
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

// resolveRidingMember is the MYR-540 GROUP-RIDE MEMBERSHIP source of viewer
// access, consulted only after ownership and shares have both declined.
//
// It returns the ZERO-VALUE ShareGrant, and that is the whole tier decision: a
// member gets the base capability every live viewer has — the catalog row, the
// snapshot, the WebSocket subscription, all under the MYR-435 viewer mask — and
// nothing more. In particular GrantsRides() is false, so riding along in a car
// never becomes permission to summon it.
//
// WHY IT IS LAST. Ownership and an accepted share are STANDING relationships and
// membership is a transient one, so resolving in that order means the person
// who both owns a share on the car and happens to be riding in it keeps the
// grant they actually hold — flags and all — instead of being downgraded to the
// bare membership tier for the length of one ride.
//
// FAILS CLOSED, and unlike the share lookup above it does so by returning the
// caller's own denial rather than a lookup error: this probe runs on a path that
// was ALREADY going to deny, so a database blip here must not convert a denial
// into a 500 on a request that has no access anyway. A genuine member whose
// probe failed is denied for this request and admitted on the retry, which is
// the same bound the 5-minute access-set cache already imposes.
func (a *JWTAuthenticator) resolveRidingMember(
	ctx context.Context, userID, vehicleID string, denial error,
) (Role, ShareGrant, error) {
	if a.rides == nil {
		return Role(""), ShareGrant{}, denial
	}
	riding, err := a.rides.IsRidingVehicle(ctx, userID, vehicleID)
	if err != nil || !riding {
		return Role(""), ShareGrant{}, denial
	}
	return RoleViewer, ShareGrant{}, nil
}

// IsRidingVehicle reports whether userID is a member of a LIVE group ride being
// served by vehicleID (MYR-540). A terminal ride matches nothing, which is how
// membership access ends without anything having to revoke it.
func (q *pgVehicleQuerier) IsRidingVehicle(ctx context.Context, userID, vehicleID string) (bool, error) {
	var riding bool
	if err := q.pool.QueryRow(ctx, queryRideMembershipOnVehicle, userID, vehicleID).Scan(&riding); err != nil {
		return false, fmt.Errorf("pgVehicleQuerier.IsRidingVehicle(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return riding, nil
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
