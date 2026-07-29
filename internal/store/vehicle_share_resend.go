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
// `expires_at`. The invite id stays valid for a client holding it, `created_at`
// still records the original send, and — because the statement is guarded on
// status = 'pending' — an accepted grant can never be quietly reopened for
// somebody else to redeem.

// ResendInvite mints a NEW code on an existing PENDING row and pushes
// expires_at out by the full TTL, invalidating the previous code. The invite id
// and created_at are unchanged, so a client holding the id keeps working.
//
// Returns ErrShareNotPending when the row is an accepted grant (409 — changing
// an accepted grant is a revoke plus a fresh invite), and ErrShareNotFound when
// the row is revoked, missing, or somebody else's.
func (r *VehicleShareRepo) ResendInvite(ctx context.Context, inviteID, ownerUserID string) (VehicleShare, error) {
	code, err := r.mintCodeOutsideTx(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): %w", inviteID, err)
	}

	row := r.pool.QueryRow(ctx, queryResendShare, inviteID, ownerUserID, code)
	share, err := scanShare(row)
	if err == nil {
		return share, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return VehicleShare{}, fmt.Errorf("store.ResendInvite(invite=%s): %w", inviteID, err)
	}
	return VehicleShare{}, r.explainResendMiss(ctx, inviteID, ownerUserID)
}

// explainResendMiss turns a zero-row resend into the right typed error: an
// accepted row is a conflict, anything else is not-found.
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

// mintCodeOutsideTx is the single-statement path used by resend, which has no
// surrounding transaction to piggyback on.
func (r *VehicleShareRepo) mintCodeOutsideTx(ctx context.Context) (string, error) {
	for attempt := 0; attempt < maxShareCodeMintAttempts; attempt++ {
		code, err := newShareCode()
		if err != nil {
			return "", err
		}
		var inUse bool
		if err := r.pool.QueryRow(ctx, queryShareCodeInUse, code).Scan(&inUse); err != nil {
			return "", fmt.Errorf("code-in-use probe: %w", err)
		}
		if !inUse {
			return code, nil
		}
	}
	return "", errors.New("could not mint an unused invite code")
}
