package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The rider side of MYR-184 vehicle sharing: redeeming a code, and the two
// lookups the authorization layer needs afterwards (the viewer access set and
// the per-vehicle tier).
//
// The code being redeemed is P1 and is NEVER logged and NEVER echoed into an
// error string, including on failure — an error message naming a rejected code
// would hand an enumerating caller a confirmation oracle.

// RedeemCode accepts EVERY pending, unexpired row backing a code, atomically,
// on behalf of redeemerID.
//
// The whole redemption is one transaction with the candidate rows held under
// FOR UPDATE, which is what makes the partial states unreachable:
//
//   - Two people redeeming the same code concurrently: the second blocks on the
//     lock, re-reads no pending rows, and gets ErrShareNotFound (404). It never
//     sees a subset granted.
//   - The caller owning one of the target vehicles: nothing is written at all —
//     the transaction rolls back before the UPDATE, so a multi-vehicle code
//     that includes one of your own cars grants you none of the others rather
//     than a confusing partial set.
//   - A retried request from the SAME person after a dropped response: no
//     pending rows remain, so the accepted-by-me lookup answers instead and the
//     same grants come back with a 200.
//
// Invalid, expired, and consumed-by-someone-else codes all produce
// ErrShareNotFound so the caller cannot tell them apart.
func (r *VehicleShareRepo) RedeemCode(ctx context.Context, code, redeemerID string) ([]ShareGrant, error) {
	if strings.TrimSpace(redeemerID) == "" {
		return nil, errors.New("store.RedeemCode: empty redeemer id")
	}
	if !ValidShareCodeFormat(code) {
		// Never report the value — only that it did not match the shape.
		return nil, ErrShareNotFound
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids, owner, err := lockPendingRows(ctx, tx, code)
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		// Nothing redeemable. Either this person already redeemed it (200,
		// idempotent) or the code is dead in one of the three
		// indistinguishable ways (404).
		return r.alreadyRedeemed(ctx, tx, code, redeemerID)
	}
	if owner == redeemerID {
		return nil, ErrShareSelfRedeem
	}

	grants, err := acceptLockedRows(ctx, tx, ids, redeemerID)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		// The rows were locked and pending a statement ago; zero updated
		// means the expiry boundary was crossed mid-transaction.
		return nil, ErrShareNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store.RedeemCode: commit: %w", err)
	}
	return grants, nil
}

// lockPendingRows selects and locks the redeemable rows for a code, returning
// their ids and the single owner they all belong to.
//
// A code resolving to more than one owner can only happen through an
// astronomically unlikely mint collision, and granting it would hand the
// redeemer access to two unrelated people's cars. It is refused, not guessed.
func lockPendingRows(ctx context.Context, tx pgx.Tx, code string) (ids []string, owner string, err error) {
	rows, err := tx.Query(ctx, queryLockPendingByCode, code)
	if err != nil {
		return nil, "", fmt.Errorf("store.RedeemCode: lock candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, vehicleID, ownerID, permission string
		if err := rows.Scan(&id, &vehicleID, &ownerID, &permission); err != nil {
			return nil, "", fmt.Errorf("store.RedeemCode: scan candidate: %w", err)
		}
		if owner != "" && ownerID != owner {
			return nil, "", ErrShareCodeCollision
		}
		owner = ownerID
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store.RedeemCode: iterate candidates: %w", err)
	}
	return ids, owner, nil
}

// acceptLockedRows flips the locked ids to accepted and returns what was
// granted. A unique violation here is the partial-unique accepted-grant index
// refusing a SECOND grant of the same vehicle to the same person through a
// different invite — a conflict, not a failure.
func acceptLockedRows(ctx context.Context, tx pgx.Tx, ids []string, redeemerID string) ([]ShareGrant, error) {
	rows, err := tx.Query(ctx, queryAcceptSharesByID, ids, redeemerID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrShareAlreadyGranted
		}
		return nil, fmt.Errorf("store.RedeemCode: accept: %w", err)
	}
	defer rows.Close()

	grants, err := scanGrants(rows)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrShareAlreadyGranted
		}
		return nil, fmt.Errorf("store.RedeemCode: accept: %w", err)
	}
	return grants, nil
}

// isUniqueViolation reports whether err is a Postgres 23505.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// alreadyRedeemed serves the idempotent re-redeem: rows this same person
// already accepted under this same code. Returns ErrShareNotFound when there
// are none, which is the same answer an unknown or expired code gets.
func (r *VehicleShareRepo) alreadyRedeemed(ctx context.Context, tx pgx.Tx, code, redeemerID string) ([]ShareGrant, error) {
	rows, err := tx.Query(ctx, queryAcceptedSharesByCodeAndUser, code, redeemerID)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: idempotency lookup: %w", err)
	}
	defer rows.Close()

	grants, err := scanGrants(rows)
	if err != nil {
		return nil, fmt.Errorf("store.RedeemCode: idempotency lookup: %w", err)
	}
	if len(grants) == 0 {
		return nil, ErrShareNotFound
	}
	return grants, nil
}

// scanGrants reads (vehicle_id, owner_user_id, allow_rides) triples — the FLAGS
// the redemption wrote, not the preset that produced them (MYR-369).
func scanGrants(rows pgx.Rows) ([]ShareGrant, error) {
	var out []ShareGrant
	for rows.Next() {
		var g ShareGrant
		if err := rows.Scan(&g.VehicleID, &g.OwnerUserID, &g.AllowRides); err != nil {
			return nil, fmt.Errorf("scan share grant: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share grants: %w", err)
	}
	return out, nil
}

// SharedVehicleIDs returns the vehicles a person may see because somebody
// shared them — the viewer half of the access set the authenticator unions with
// the caller's own cars. An empty result is the ordinary state, not an error.
func (r *VehicleShareRepo) SharedVehicleIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, queryAcceptedShareVehicleIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("store.SharedVehicleIDs(user=%s): %w", userID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.SharedVehicleIDs(user=%s): scan: %w", userID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SharedVehicleIDs(user=%s): iterate: %w", userID, err)
	}
	return out, nil
}

// ShareGrantFor resolves the CAPABILITY FLAGS one person holds over one vehicle
// (MYR-369). Returns ErrShareNotFound when there is no accepted grant OR when
// the grant is suspended — callers MUST treat both as "no access", and the two
// are deliberately indistinguishable.
//
// The bool is the ride capability. It is only ever true for a LIVE grant, since
// the statement returns no row for a suspended one, so a caller cannot read a
// capability off a paused grant even by mistake.
func (r *VehicleShareRepo) ShareGrantFor(ctx context.Context, userID, vehicleID string) (bool, error) {
	var allowRides bool
	err := r.pool.QueryRow(ctx, queryAcceptedShareGrant, vehicleID, userID).Scan(&allowRides)
	switch {
	case err == nil:
		return allowRides, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrShareNotFound
	default:
		return false, fmt.Errorf("store.ShareGrantFor(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
}

// RiderMayRequestRides reports whether riderID may still ride in vehicleID
// (MYR-369): true when they OWN the car, or hold a LIVE accepted grant carrying
// the ride capability.
//
// The reservation sweeper's seam. Returns a real error on a transport failure
// rather than false, so the sweeper can HOLD the reservation — "unknown" and
// "not permitted" must not collapse into the same answer on a path where the
// alternative to holding is an irreversible claim.
func (r *VehicleShareRepo) RiderMayRequestRides(ctx context.Context, riderID, vehicleID string) (bool, error) {
	var permitted bool
	if err := r.pool.QueryRow(ctx, queryRiderMayRequestRides, riderID, vehicleID).Scan(&permitted); err != nil {
		return false, fmt.Errorf("store.RiderMayRequestRides(rider=%s, vehicle=%s): %w", riderID, vehicleID, err)
	}
	return permitted, nil
}

// OwnerFirstName resolves the sharing owner's FIRST NAME for the redeemer's
// success screen ("You can now ride in {owner}'s Tesla").
//
// Deliberately narrow — first name only, no surname, no email, no user id: the
// P1 first-names-only policy that governs the push payloads and
// RideRequest.requesterName. The ladder is name → email local-part → "Owner",
// so the result is never empty and the caller never needs an absent-name
// branch. The resolved value is P1 and is never logged.
func (r *VehicleShareRepo) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	var name, email *string
	err := r.pool.QueryRow(ctx, queryOwnerFirstNameSources, ownerUserID).Scan(&name, &email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store.OwnerFirstName(owner=%s): %w", ownerUserID, err)
	}
	return firstNameFrom(name, email), nil
}

// shareOwnerFallbackName is the stable literal used when the owner's row
// carries neither a usable name nor a usable email. Mirrors the "Rider"
// fallback on the ride surface.
const shareOwnerFallbackName = "Owner"

// firstNameFrom applies the name → email-local-part → literal ladder.
func firstNameFrom(name, email *string) string {
	if name != nil {
		if first := strings.Fields(*name); len(first) > 0 {
			return first[0]
		}
	}
	if email != nil {
		if local, _, ok := strings.Cut(*email, "@"); ok && local != "" {
			return local
		}
	}
	return shareOwnerFallbackName
}
