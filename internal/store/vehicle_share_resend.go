package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The RESEND path (MYR-184): re-mint an invite's code and push its expiry out
// without disturbing the row's identity. Split out of vehicle_share_repo.go
// (300-line file cap).
//
// The invariant worth stating in one place: a resend touches ONLY `code` and
// `expires_at`, and it touches them on EVERY ROW OF THE INVITE. The invite ids
// stay valid for a client holding them, `created_at` still records the original
// send, and — because every statement is guarded on status = 'pending' — an
// accepted grant can never be quietly reopened for somebody else to redeem.
//
// The sibling scope is the security-relevant half. CreateInvite mints ONE code
// backing one row per vehicle, so an invite is a SET of rows, not a row. A
// resend that updated only the row named in the path would leave the old code
// live and pending on the siblings for the rest of its 7-day TTL — which
// defeats the one thing resend exists for (invalidating a leaked code) and
// splits the invite, so the new code grants one car and the old code grants the
// rest. Hence: one transaction, all siblings, one new code.

// ResendInvite mints a NEW code across EVERY pending row of the invite the given
// row belongs to, and pushes expires_at out by the full TTL on each, atomically.
// The previous code is invalidated everywhere it was live. Invite ids and
// created_at are unchanged, so a client holding an id keeps working.
//
// Returns the PATH row — the invite the caller named — so the response shape is
// the single ShareInvite the endpoint documents.
//
// Returns ErrShareNotPending when the row is an accepted grant (409 — changing
// an accepted grant is a revoke plus a fresh invite), and ErrShareNotFound when
// the row is revoked, missing, or somebody else's.
func (r *VehicleShareRepo) ResendInvite(ctx context.Context, inviteID, ownerUserID string) (VehicleShare, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): begin: %w", inviteID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids, err := lockResendSiblings(ctx, tx, inviteID, ownerUserID)
	if err != nil {
		return VehicleShare{}, err
	}
	if len(ids) == 0 {
		return VehicleShare{}, r.explainResendMiss(ctx, inviteID, ownerUserID)
	}

	code, err := mintUnusedShareCode(ctx, tx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): %w", inviteID, err)
	}

	pathRow, err := remintLockedRows(ctx, tx, ids, code, inviteID, ownerUserID)
	if err != nil {
		return VehicleShare{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): commit: %w", inviteID, err)
	}
	return pathRow, nil
}

// lockResendSiblings locks every pending row sharing the target row's code and
// returns their ids. An empty result means the target is not a pending row of
// the caller's — the caller disambiguates with explainResendMiss.
func lockResendSiblings(ctx context.Context, tx pgx.Tx, inviteID, ownerUserID string) ([]string, error) {
	rows, err := tx.Query(ctx, queryLockResendSiblings, inviteID, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("store.ResendInvite(invite=%s): lock siblings: %w", inviteID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ResendInvite(invite=%s): scan sibling: %w", inviteID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ResendInvite(invite=%s): iterate siblings: %w", inviteID, err)
	}
	return ids, nil
}

// remintLockedRows writes the new code and expiry onto the locked set and
// returns the path row out of what came back.
//
// A missing path row is treated as an internal error rather than a not-found:
// the row was locked and pending one statement ago, so its absence from the
// RETURNING set means the update did not do what the lock guaranteed, and
// answering 404 would tell the owner their invite vanished when it did not.
func remintLockedRows(ctx context.Context, tx pgx.Tx, ids []string, code, inviteID, ownerUserID string) (VehicleShare, error) {
	rows, err := tx.Query(ctx, queryResendShare, ids, code, ownerUserID)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): re-mint: %w", inviteID, err)
	}
	defer rows.Close()

	var pathRow VehicleShare
	var found bool
	for rows.Next() {
		share, scanErr := scanShare(rows)
		if scanErr != nil {
			return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): %w", inviteID, scanErr)
		}
		if share.ID == inviteID {
			pathRow, found = share, true
		}
	}
	if err := rows.Err(); err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): iterate re-mint: %w", inviteID, err)
	}
	if !found {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): re-mint did not return the path row", inviteID)
	}
	return pathRow, nil
}

// explainResendMiss turns a zero-row resend into the right typed error: an
// accepted row is a conflict, anything else is not-found.
//
// Deliberately run OUTSIDE the resend transaction (which has already been
// abandoned by the time this is called): it is a read-only probe whose only job
// is to pick an error code, and it must not hold locks while doing it.
func (r *VehicleShareRepo) explainResendMiss(ctx context.Context, inviteID, ownerUserID string) error {
	var status string
	switch err := r.pool.QueryRow(ctx, queryShareExistsForOwner, inviteID, ownerUserID).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrShareNotFound
	case err != nil:
		return fmt.Errorf("store.ResendInvite(invite=%s): probe: %w", inviteID, err)
	case status == ShareStatusAccepted:
		return ErrShareNotPending
	default:
		return ErrShareNotFound // revoked tombstones are not resurrectable
	}
}
