package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The MYR-369 owner edit of one ACCEPTED grant — PATCH /api/invites/{inviteId}.
//
// This is the endpoint that replaces the pre-MYR-369 rule that a grant's access
// was fixed for its life. It is ACCESS CONTROL, so the shape of the write
// matters as much as its effect:
//
//   - ONE STATEMENT. No read to decide what to write, no read to check
//     ownership, no read to check status. All three predicates ride the UPDATE
//     itself, on the row it changes, so there is no check-then-write window for
//     a concurrent revoke or a concurrent patch to slip through. Two owners
//     cannot exist for one row, but a revoke racing a patch can — and the loser
//     of that race must write nothing, not write to a tombstone.
//
//   - PARTIAL WITHOUT DYNAMIC SQL. The (present, value) parameter pairs in
//     queryPatchShare express "leave this alone" as an assignment of the column
//     to itself, so the statement is fixed text with a fixed plan. Assembling a
//     SET clause from whichever fields were present would be the obvious
//     alternative and is strictly worse on every axis that matters here.
//
//   - ZERO ROWS IS AMBIGUOUS ON PURPOSE, and the ambiguity is resolved with the
//     SAME probe RevokeInvite uses, so the two endpoints cannot drift into
//     telling a caller different things about the same row.

// PatchInvite applies an owner's edit to one accepted grant and returns the row
// as it now stands.
//
// Returns:
//   - ErrShareNotFound — the row does not exist, belongs to somebody else, or
//     is a revoked tombstone. The three are DELIBERATELY INDISTINGUISHABLE:
//     distinguishing them would turn this endpoint into an oracle for other
//     people's invite ids, which is exactly what DELETE and resend already
//     refuse to be.
//   - ErrShareNotAccepted — the row is the caller's and exists, but is still
//     PENDING. A pending invite has no grant to edit; its access is decided at
//     redemption from its preset. Told plainly because the caller demonstrably
//     owns the row, so there is nothing left to hide from them.
//
// The returned row always reflects the DATABASE's post-write state, never a
// Go-side reconstruction of what was asked for: RETURNING is what the caller
// echoes to the client, so a partially-applied or concurrently-modified row
// reports itself honestly.
func (r *VehicleShareRepo) PatchInvite(ctx context.Context, in PatchShareInviteInput) (VehicleShare, error) {
	if strings.TrimSpace(in.InviteID) == "" {
		return VehicleShare{}, errors.New("store.PatchInvite: empty invite id")
	}
	if strings.TrimSpace(in.OwnerUserID) == "" {
		return VehicleShare{}, errors.New("store.PatchInvite: empty owner id")
	}
	// An empty patch is refused rather than run. The statement would be a
	// harmless self-assignment, but succeeding on a request that asks for
	// nothing lets a client bug look like an applied edit.
	if !in.HasChange() {
		return VehicleShare{}, errors.New("store.PatchInvite: no fields to change")
	}

	setRides, rides := derefBool(in.AllowRides)
	setSuspended, suspended := derefBool(in.Suspended)

	row, err := scanShare(r.pool.QueryRow(ctx, queryPatchShare,
		in.InviteID, in.OwnerUserID, setRides, rides, setSuspended, suspended))
	switch {
	case err == nil:
		return row, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return VehicleShare{}, fmt.Errorf("store.PatchInvite(invite=%s): %w", in.InviteID, err)
	}

	// Zero rows: not ours / gone, or ours but not accepted. Same probe, same
	// statement, same answers as RevokeInvite.
	return VehicleShare{}, r.explainPatchMiss(ctx, in.InviteID, in.OwnerUserID)
}

// explainPatchMiss turns a zero-row patch into the right sentinel.
//
// A row the caller owns can only have missed the `status = 'accepted'` predicate
// in two ways, and they get different answers: still pending is
// ErrShareNotAccepted (a real, fixable complaint), while a revoked tombstone is
// ErrShareNotFound — a revoked grant is gone as far as every surface is
// concerned, and saying "it exists but is revoked" would leak the tombstone the
// wire deliberately never shows.
func (r *VehicleShareRepo) explainPatchMiss(ctx context.Context, inviteID, ownerUserID string) error {
	var status string
	switch err := r.pool.QueryRow(ctx, queryShareExistsForOwner, inviteID, ownerUserID).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrShareNotFound
	case err != nil:
		return fmt.Errorf("store.PatchInvite(invite=%s): probe: %w", inviteID, err)
	case status == ShareStatusPending:
		return ErrShareNotAccepted
	default:
		return ErrShareNotFound
	}
}

// derefBool splits an optional bool into the (present, value) pair
// queryPatchShare's CASE arms consume. A nil pointer yields (false, false); the
// value half is then never read, because the present half selects the
// column-to-itself arm.
func derefBool(p *bool) (present, value bool) {
	if p == nil {
		return false, false
	}
	return true, *p
}
