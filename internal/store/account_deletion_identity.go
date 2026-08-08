package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AccountIdentityResult is the log-safe (P0) outcome of DeleteIdentity.
type AccountIdentityResult struct {
	// Deleted is true when this call removed identity rows and wrote the
	// account_deleted audit row.
	Deleted bool
	// AlreadyGone is true when no identity row existed for the user — the
	// idempotent no-op arm, which writes NO audit row. It is how a re-run of
	// a completed deletion avoids duplicating the FR-10.2 entry.
	AlreadyGone bool
	// HadPrismaUser records whether a sibling-schema "User" row was present.
	HadPrismaUser bool
	// AuditLogID is the cuid of the account_deleted row, empty on the no-op
	// arm.
	AuditLogID string
}

// DeleteIdentity is the FINAL, transactional step of an account deletion: the
// audit row plus the three identity sources that make the cuid resolvable at
// all. After it commits the user no longer exists anywhere, and their
// still-valid access token stops validating as soon as the existence cache
// evicts (auth.JWTAuthenticator).
//
// One transaction, in this order (CG-DL-3 requires the audit BEFORE the
// destructive delete):
//
//  1. Probe the three identity sources. Nothing there → commit the empty
//     transaction and report AlreadyGone. This is the idempotency hinge for
//     the whole endpoint: a re-run after a completed deletion writes no second
//     audit row.
//  2. INSERT the user-initiated `account_deleted` AuditLog row, metadata =
//     the P0 counts the handler accumulated (CG-DL-5).
//  3. DELETE go_identity_apple (every Apple sub bound to the user), then the
//     stored OAuth grants and Settings, then go_users, then the Prisma "User"
//     row — whose cascades take Invite / any residual Vehicle
//     (data-lifecycle.md §3.2).
//
// It is deliberately LAST in the sequence: the caller authenticates with a
// token that resolves through these rows, so deleting them first would leave a
// half-deleted account no one could finish deleting.
//
// It takes the whole DeletionScope rather than a single id (MYR-452). The
// subject a converged owner authenticates with is not the id their rows are
// filed under, and every statement here — the probes included — would otherwise
// miss the account entirely while still reporting success.
func (a *AccountDeleter) DeleteIdentity(ctx context.Context, scope DeletionScope, counts AccountDeletionCounts) (AccountIdentityResult, error) {
	userID := strings.TrimSpace(scope.CanonicalID)
	if userID == "" || strings.TrimSpace(scope.CallerID) == "" || len(scope.IDs) == 0 {
		return AccountIdentityResult{}, fmt.Errorf("store.DeleteIdentity: incomplete deletion scope")
	}
	// A canonical id outside its own closure would delete an id the sequence
	// never ran a single data step against.
	if !containsID(scope.IDs, userID) {
		return AccountIdentityResult{}, fmt.Errorf(
			"store.DeleteIdentity(user=%s): canonical id is not a member of its own scope %v", userID, scope.IDs)
	}

	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return AccountIdentityResult{}, fmt.Errorf("store.DeleteIdentity(user=%s): begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	// Re-walk the convergence closure under this transaction. The scope handed
	// in was resolved at step 0, before ten other steps ran and on another
	// snapshot; a link that converged in that window would otherwise leave this
	// transaction deleting the wrong id and reporting success.
	scope, err = revalidateScopeWithin(ctx, tx, scope)
	if err != nil {
		return AccountIdentityResult{}, err
	}
	userID = scope.CanonicalID

	prismaUser, goUser, appleIdentity, err := probeAccountIdentity(ctx, tx, scope)
	if err != nil {
		return AccountIdentityResult{}, err
	}

	if !prismaUser && !goUser && !appleIdentity {
		// Nothing left to delete: either the deletion already completed, or
		// the token outlived its user. Commit the empty transaction and
		// report the no-op — no audit row, so a re-run cannot duplicate the
		// FR-10.2 entry.
		if err := tx.Commit(ctx); err != nil {
			return AccountIdentityResult{}, fmt.Errorf("store.DeleteIdentity(user=%s): commit no-op: %w", userID, err)
		}
		return AccountIdentityResult{AlreadyGone: true}, nil
	}

	counts.HadPrismaUser = prismaUser
	auditID, err := a.insertAccountAudit(ctx, tx, userID, counts)
	if err != nil {
		return AccountIdentityResult{}, err
	}

	if err := deleteIdentityRows(ctx, tx, scope); err != nil {
		return AccountIdentityResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return AccountIdentityResult{}, fmt.Errorf("store.DeleteIdentity(user=%s): commit: %w", userID, err)
	}

	// Audit line: P0 only — an opaque cuid and counts. NEVER the name, the
	// email, the Apple sub, or any ride detail.
	a.logger.Info("account_deleted",
		slog.String("event", "account_deleted"),
		slog.String("user_id", userID),
		slog.Bool("converged_identity", scope.Converged()),
		slog.Int("scope_size", len(scope.IDs)),
		slog.String("audit_log_id", auditID),
		slog.Int("vehicle_count", counts.VehicleCount),
		slog.Int("rides_cancelled", counts.RidesCancelled),
		slog.Int("shares_revoked", counts.SharesRevoked),
		slog.Bool("had_prisma_user", prismaUser),
	)

	return AccountIdentityResult{
		Deleted:       true,
		HadPrismaUser: prismaUser,
		AuditLogID:    auditID,
	}, nil
}

// probeAccountIdentity reports which of the three identity sources still hold a
// row anywhere in the scope and ROW-LOCKS every row it finds, inside the open
// transaction. The locks are what serialize two concurrent deletions of the same
// account — see the queries' doc comment; without them a double-tapped Delete
// could write two audit rows for one account.
func probeAccountIdentity(ctx context.Context, tx pgx.Tx, scope DeletionScope) (prismaUser, goUser, appleIdentity bool, err error) {
	if prismaUser, err = lockIdentityRow(ctx, tx, scope, `"User"`, queryLockPrismaUser); err != nil {
		return false, false, false, err
	}
	if goUser, err = lockIdentityRow(ctx, tx, scope, "go_users", queryLockGoUser); err != nil {
		return false, false, false, err
	}
	if appleIdentity, err = lockIdentityRow(ctx, tx, scope, "go_identity_apple", queryLockAppleIdentities); err != nil {
		return false, false, false, err
	}
	return prismaUser, goUser, appleIdentity, nil
}

// lockIdentityRow runs one FOR UPDATE probe over the scope and reports whether
// it matched. pgx.ErrNoRows is the "nothing here" answer, not a failure — it is
// the whole Apple-native / legacy-web distinction, and on a re-run it is every
// table.
func lockIdentityRow(ctx context.Context, tx pgx.Tx, scope DeletionScope, table, query string) (bool, error) {
	var scratch string
	err := tx.QueryRow(ctx, query, scope.IDs).Scan(&scratch)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("store.DeleteIdentity(user=%s): lock %s: %w", scope.CanonicalID, table, err)
	}
}

// deleteIdentityRows removes the Apple bindings, the Apple-native user row and
// the Prisma "User" row, in that order, inside the open transaction. Each is
// unconditional: a source that holds no row simply affects zero rows, which is
// exactly the Apple-native-only case (no "User") and the legacy-web case (no
// go_users).
func deleteIdentityRows(ctx context.Context, tx pgx.Tx, scope DeletionScope) error {
	steps := []struct {
		name  string
		query string
	}{
		{"delete apple identity", queryDeleteAppleIdentity},
		{"delete provider accounts", queryDeleteProviderAccounts},
		{"delete settings", queryDeleteSettings},
		{"delete convergence edges", queryDeleteConvergenceEdges},
		{"delete go user", queryDeleteGoUser},
		{"delete prisma user", queryDeletePrismaUser},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.query, scope.IDs); err != nil {
			return fmt.Errorf("store.DeleteIdentity(user=%s): %s: %w", scope.CanonicalID, s.name, err)
		}
	}
	return nil
}

// insertAccountAudit writes the user-initiated `account_deleted` AuditLog row
// within the deletion transaction, reusing the same-package queryAuditInsert
// column list (single source of truth shared with AuditRepo — keeps CG-DL-8
// column parity automatic). targetType='user' and targetId=userId per
// data-lifecycle.md §3.1. Metadata is P0-only per CG-DL-5.
func (a *AccountDeleter) insertAccountAudit(ctx context.Context, tx pgx.Tx, userID string, counts AccountDeletionCounts) (string, error) {
	meta, err := json.Marshal(counts)
	if err != nil {
		return "", fmt.Errorf("store.DeleteIdentity(user=%s): marshal audit metadata: %w", userID, err)
	}
	id := newProvisionID()
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, queryAuditInsert,
		id,                                // id (cuid)
		userID,                            // userId (the account being deleted)
		now,                               // timestamp
		string(AuditActionAccountDeleted), // action
		auditTargetTypeUser,               // targetType
		userID,                            // targetId (self)
		auditInitiatorUser,                // initiator
		meta,                              // metadata (P0 counts only)
		now,                               // createdAt
	); err != nil {
		return "", fmt.Errorf("store.DeleteIdentity(user=%s): insert audit: %w", userID, err)
	}
	return id, nil
}
