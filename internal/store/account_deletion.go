package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountDeleter is the store-side half of user-initiated account deletion
// (MYR-355, FR-10.1/FR-10.2, docs/contracts/rest-api.md §7.6). It owns the
// account-scoped steps that are pure SQL; the ORDER of the whole deletion —
// and the two steps that must run through existing application machinery
// (per-vehicle teardown, guarded ride cancellation) — lives in
// telemetry.AccountDeletionHandler, because those steps publish events and
// only the handler holds those seams.
//
// WHY THIS IS NOT ONE TRANSACTION. The per-vehicle teardown
// (store.OwnerTeardown) is already its own transaction, by design: it takes
// FOR UPDATE locks over the owner's vehicle set to make the last-vehicle
// decision race-safe, and it fires a `vehicle_deleted` NOTIFY whose consumers
// must not observe an uncommitted delete. Wrapping N teardowns plus a ride
// cancellation that publishes push notifications inside one outer transaction
// would either deadlock against those locks or emit notifications for work
// that a later rollback undid. So the deletion is a SEQUENCE of independently
// atomic steps, and the property we buy instead of one-shot atomicity is that
// **every step is idempotent and the sequence is RE-RUNNABLE**: a mid-failure
// leaves the account partially deleted, the endpoint answers 500, and calling
// it again resumes from wherever it stopped. The steps are ordered so that
// resuming is always possible — the identity rows the caller authenticates
// with are deleted LAST, so a failure never locks the user out of finishing
// their own deletion.
//
// Contract position (data-lifecycle.md §1.4 / §3): this extends the MYR-258
// owner-teardown carve-out from "one vehicle" to "the account", and moves
// ownership of the FR-10.1 deletion transaction from the Next.js app to the Go
// server, because the native iOS client (P9) never talks to the Next.js app at
// all. The `account_deleted` AuditLog row that §3.1 assigns to Prisma is now
// written here, in the same transaction as the identity delete (CG-DL-3).
type AccountDeleter struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewAccountDeleter builds the account-deletion writer over the given pool.
func NewAccountDeleter(pool *pgxpool.Pool, logger *slog.Logger) *AccountDeleter {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountDeleter{pool: pool, logger: logger}
}

// AccountDeletionCounts is the P0-only tally the handler accumulates across
// the sequence and hands to DeleteIdentity for the audit metadata. Every field
// is an opaque count or a bool — never PII, never a name, never a coordinate
// (CG-DL-5).
type AccountDeletionCounts struct {
	// VehicleCount is the number of owned vehicles torn down.
	VehicleCount int `json:"vehicleCount"`
	// DriveCount is the number of drives that went with those vehicles,
	// counted BEFORE the teardowns ran.
	DriveCount int `json:"driveCount"`
	// RidesCancelled is the number of open ride requests cancelled with the
	// user as RIDER.
	RidesCancelled int `json:"ridesCancelled"`
	// SharesRevoked is the number of accepted grants the user had REDEEMED
	// that were tombstoned.
	SharesRevoked int `json:"sharesRevoked"`
	// PushDevicesDeleted is the number of APNs registrations removed.
	PushDevicesDeleted int `json:"pushDevicesDeleted"`
	// RefreshTokensRevoked is the number of live refresh tokens revoked.
	RefreshTokensRevoked int `json:"refreshTokensRevoked"`
	// HadPrismaUser records whether a sibling-schema "User" row existed —
	// the dual-source identity fact, and the one thing that distinguishes an
	// Apple-native account from a legacy web one in the audit trail.
	HadPrismaUser bool `json:"hadPrismaUser"`
}

// CountUserDrives counts the drives attached to the user's vehicles. Called
// once, before the teardowns, purely so the audit metadata can carry the
// number; a failure here is NOT fatal to the deletion (the handler logs and
// proceeds with zero) because a missing statistic must never block a user's
// right to erasure.
func (a *AccountDeleter) CountUserDrives(ctx context.Context, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("store.CountUserDrives: empty user id")
	}
	var n int
	if err := a.pool.QueryRow(ctx, queryCountUserDrives, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountUserDrives(user=%s): %w", userID, err)
	}
	return n, nil
}

// RevokeSharesReceived tombstones every live grant the user redeemed — the
// shares they RECEIVED, as opposed to the shares on their own cars, which the
// per-vehicle teardown already revokes (MYR-184). Returns the number of rows
// revoked. Idempotent: a re-run matches nothing.
func (a *AccountDeleter) RevokeSharesReceived(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "RevokeSharesReceived", queryRevokeSharesReceived, userID)
}

// DeletePushDevices removes every APNs registration owned by the user, so no
// device keeps receiving ride notifications addressed to a deleted account.
// Returns the number of rows deleted. Idempotent.
func (a *AccountDeleter) DeletePushDevices(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeletePushDevices", queryDeletePushDevicesForUser, userID)
}

// RevokeRefreshTokens revokes every live refresh token in the user's name, so
// no stored session can mint a new access token after the identity rows go.
// Returns the number of rows revoked. Idempotent.
//
// Ordering note: this runs BEFORE the identity delete but AFTER every other
// destructive step, and it deliberately does NOT invalidate the caller's
// current ACCESS token — that token is what authenticates the re-run if the
// identity transaction then fails.
func (a *AccountDeleter) RevokeRefreshTokens(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "RevokeRefreshTokens", queryRevokeRefreshTokensForUser, userID)
}

// execCount runs a one-parameter mutating statement and returns the affected
// row count, wrapping the error with the operation name and the user id (P0).
func (a *AccountDeleter) execCount(ctx context.Context, op, query, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("store.%s: empty user id", op)
	}
	tag, err := a.pool.Exec(ctx, query, userID)
	if err != nil {
		return 0, fmt.Errorf("store.%s(user=%s): %w", op, userID, err)
	}
	return int(tag.RowsAffected()), nil
}
