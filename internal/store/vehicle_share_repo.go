package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VehicleShareRepo is the go_vehicle_shares repository (MYR-184) — the owner
// side of vehicle sharing: mint an invite, list a car's invites and viewers,
// revoke a grant, resend a code. The rider side (redeem) lives in
// vehicle_share_redeem.go on the same type.
//
// NOTHING in this file logs a `label` or a `code`: both are P1
// (data-classification.md §1.15) and rows are identified in logs and errors by
// their id.
type VehicleShareRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewVehicleShareRepo builds the sharing repository over the given pool.
func NewVehicleShareRepo(pool *pgxpool.Pool, logger *slog.Logger) *VehicleShareRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &VehicleShareRepo{pool: pool, logger: logger}
}

// maxShareCodeMintAttempts bounds the collision-retry loop. Each attempt has a
// ~1-in-2.2-billion chance of colliding with a live pending code, so exceeding
// three attempts means something is wrong with the entropy source, not that we
// got unlucky.
const maxShareCodeMintAttempts = 3

// CreateInvite mints ONE code and creates one pending row per vehicle, all
// sharing it, then returns the row for in.PathVehicleID.
//
// The whole thing is one transaction: an invite that granted three of four
// requested cars would be a silent partial share, and the redeemer would be
// told they had access to a car nobody granted. Ownership of EVERY requested
// vehicle is verified inside that transaction against the authoritative
// relation — the handler's own check of the path vehicle is not trusted to
// cover the rest of the set.
func (r *VehicleShareRepo) CreateInvite(ctx context.Context, in CreateShareInviteInput) (VehicleShare, error) {
	if err := validateCreateInput(in); err != nil {
		return VehicleShare{}, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): begin: %w", in.OwnerUserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := verifyOwnsAll(ctx, tx, in.OwnerUserID, in.VehicleIDs); err != nil {
		return VehicleShare{}, err
	}

	code, err := mintUnusedShareCode(ctx, tx)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): %w", in.OwnerUserID, err)
	}

	var pathRow VehicleShare
	for _, vehicleID := range in.VehicleIDs {
		id := newProvisionID()
		var createdAt, expiresAt time.Time
		if err := tx.QueryRow(ctx, queryInsertShare,
			id, vehicleID, in.OwnerUserID, in.Label, in.Permission, code,
		).Scan(&createdAt, &expiresAt); err != nil {
			return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s, vehicle=%s): insert: %w",
				in.OwnerUserID, vehicleID, err)
		}
		if vehicleID == in.PathVehicleID {
			pathRow = VehicleShare{
				ID: id, VehicleID: vehicleID, OwnerUserID: in.OwnerUserID,
				Label: in.Label, Permission: in.Permission, Code: code,
				Status: ShareStatusPending, CreatedAt: createdAt, ExpiresAt: expiresAt,
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return VehicleShare{}, fmt.Errorf("store.CreateInvite(owner=%s): commit: %w", in.OwnerUserID, err)
	}
	return pathRow, nil
}

// validateCreateInput rejects a malformed create before it reaches the
// database. The handler validates the same things to produce a good 400; this
// is the repository refusing to write a row it cannot justify.
func validateCreateInput(in CreateShareInviteInput) error {
	switch {
	case strings.TrimSpace(in.OwnerUserID) == "":
		return errors.New("store.CreateInvite: empty owner id")
	case strings.TrimSpace(in.Label) == "":
		return errors.New("store.CreateInvite: empty label")
	case !ValidSharePermission(in.Permission):
		return fmt.Errorf("store.CreateInvite: invalid permission %q", in.Permission)
	case len(in.VehicleIDs) == 0:
		return errors.New("store.CreateInvite: empty vehicle set")
	}
	for _, id := range in.VehicleIDs {
		if id == in.PathVehicleID {
			return nil
		}
	}
	return fmt.Errorf("store.CreateInvite: vehicle set omits path vehicle %s", in.PathVehicleID)
}

// verifyOwnsAll fails unless every requested vehicle belongs to the owner. It
// compares COUNTS of distinct ids rather than checking membership one by one:
// a duplicate id in the request must not let a foreign id slip through the
// tally.
func verifyOwnsAll(ctx context.Context, tx pgx.Tx, ownerID string, vehicleIDs []string) error {
	want := make(map[string]struct{}, len(vehicleIDs))
	for _, id := range vehicleIDs {
		want[id] = struct{}{}
	}

	rows, err := tx.Query(ctx, queryShareOwnedVehicleIDs, vehicleIDs, ownerID)
	if err != nil {
		return fmt.Errorf("store.CreateInvite(owner=%s): ownership check: %w", ownerID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store.CreateInvite(owner=%s): ownership scan: %w", ownerID, err)
		}
		delete(want, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store.CreateInvite(owner=%s): ownership iterate: %w", ownerID, err)
	}
	if len(want) > 0 {
		return ErrShareVehicleNotOwned
	}
	return nil
}

// mintUnusedShareCode draws codes until one is not already backing a live
// pending invite.
//
// The check and the insert are not atomic against a concurrent create that
// happens to draw the SAME code in the same instant — there is no unique
// constraint to lean on, because a multi-vehicle invite legitimately shares one
// code across N rows. The residual race is a ~1-in-2.2-billion draw colliding
// inside a few milliseconds, and the redeem path refuses outright (never
// guesses) if it ever does resolve a code to two owners. See
// ErrShareCodeCollision.
func mintUnusedShareCode(ctx context.Context, tx pgx.Tx) (string, error) {
	for attempt := 0; attempt < maxShareCodeMintAttempts; attempt++ {
		code, err := newShareCode()
		if err != nil {
			return "", err
		}
		var inUse bool
		if err := tx.QueryRow(ctx, queryShareCodeInUse, code).Scan(&inUse); err != nil {
			return "", fmt.Errorf("code-in-use probe: %w", err)
		}
		if !inUse {
			return code, nil
		}
	}
	// The value is never reported — only the failure to find a free one.
	return "", errors.New("could not mint an unused invite code")
}

// ListInvitesForVehicle returns the owner's pending invites and accepted
// grants for one vehicle, newest first. Revoked tombstones are excluded by the
// query. Scoped to ownerUserID: a caller who does not own the vehicle gets an
// empty list, never another owner's rows.
func (r *VehicleShareRepo) ListInvitesForVehicle(ctx context.Context, vehicleID, ownerUserID string) ([]VehicleShare, error) {
	rows, err := r.pool.Query(ctx, queryListSharesByVehicle, vehicleID, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()

	out := make([]VehicleShare, 0, 8)
	for rows.Next() {
		share, err := scanShare(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): %w", vehicleID, err)
		}
		out = append(out, share)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListInvitesForVehicle(vehicle=%s): iterate: %w", vehicleID, err)
	}
	return out, nil
}

// RevokeInvite tombstones an invite (pending → revoked) or an accepted grant
// (accepted → revoked). IDEMPOTENT: revoking an already-revoked row that
// belongs to the caller succeeds silently, so a retried DELETE is safe.
//
// Returns ErrShareNotFound when the row does not exist OR belongs to somebody
// else — the two are deliberately indistinguishable, so the endpoint cannot be
// used to probe for the existence of other people's invites.
//
// The returned string is the user id whose access this revocation removed
// (empty when the row was still pending, so nobody held access). The caller
// uses it to bust that person's cached access set: without it a revoked viewer
// keeps resolving the vehicle until the cache TTL lapses.
func (r *VehicleShareRepo) RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (string, error) {
	var revokedViewerID string
	switch err := r.pool.QueryRow(ctx, queryRevokeShare, inviteID, ownerUserID).Scan(&revokedViewerID); {
	case err == nil:
		return revokedViewerID, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("store.RevokeInvite(invite=%s): %w", inviteID, err)
	}

	// Zero rows: either already revoked (idempotent success) or not ours.
	var status string
	switch err := r.pool.QueryRow(ctx, queryShareExistsForOwner, inviteID, ownerUserID).Scan(&status); {
	case err == nil:
		return "", nil // status is necessarily 'revoked' — the UPDATE covered the rest
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrShareNotFound
	default:
		return "", fmt.Errorf("store.RevokeInvite(invite=%s): probe: %w", inviteID, err)
	}
}

// scanShare reads one full row in the shareColumns order. rowScanner (declared
// in vehicle_repo_scan.go) is the shared pgx.Row / pgx.Rows surface, so this
// serves both the single-row RETURNING path and the list iteration.
func scanShare(row rowScanner) (VehicleShare, error) {
	var s VehicleShare
	err := row.Scan(
		&s.ID, &s.VehicleID, &s.OwnerUserID, &s.Label, &s.Permission,
		&s.Code, &s.Status, &s.CreatedAt, &s.ExpiresAt, &s.AcceptedAt,
		&s.AcceptedByUserID, &s.RevokedAt,
	)
	if err != nil {
		return VehicleShare{}, fmt.Errorf("scan vehicle share: %w", err)
	}
	return s, nil
}
