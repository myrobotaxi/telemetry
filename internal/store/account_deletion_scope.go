package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DeletionScope is the set of user ids that constitute ONE human, expanded from
// the subject the caller authenticated with (MYR-452).
//
// WHY A SET AND NOT A STRING. The identity model has two sources — go_users for
// Apple-native accounts, the Prisma "User" table for legacy web ones — and one
// sanctioned operation that MOVES a person between them:
// OwnerProvisioner.rebindApple, which fires when a Tesla link proves the caller
// and an existing owner are the same person. It re-points the Apple binding onto
// the canonical id and leaves the caller's original go_users id behind.
//
// Nothing re-issues the caller's tokens when that happens. Their JWT keeps
// naming the pre-convergence id for the life of its refresh family, so the
// subject arriving at DELETE /api/users/me is routinely NOT the id that owns the
// account's rows. Keyed on the subject alone, the teardown revoked nothing,
// deleted nothing, wrote its audit row and answered 204 — and the surviving
// binding signed the person back in as Owner on their next Sign in with Apple.
//
// The scope is the fix: resolve the subject to every id it can legitimately
// stand for, then run every step over all of them.
type DeletionScope struct {
	// CallerID is the JWT subject as presented — always a member of IDs.
	CallerID string
	// CanonicalID is the id that owns the identity rows: the convergence
	// target when the caller was re-pointed, otherwise the caller itself. The
	// audit row is filed against this id.
	CanonicalID string
	// IDs is every id in the closure, CanonicalID first. Deduplicated, never
	// empty. In the overwhelmingly common un-converged case it is exactly
	// [CallerID] and the whole sequence behaves as it always has.
	IDs []string
}

// Converged reports whether the caller authenticated under an id other than the
// one that owns the account. P0-safe to log as a bool.
func (s DeletionScope) Converged() bool {
	return s.CanonicalID != s.CallerID || len(s.IDs) > 1
}

// ResolveDeletionScope expands the caller's JWT subject into the full identity
// closure, following go_users.converged_to in BOTH directions:
//
//   - FORWARD: the caller is a pre-convergence id that was re-pointed away. Its
//     row names the canonical target, which is where the account actually lives.
//   - REVERSE: the caller already IS the canonical id (they signed in again after
//     converging, so their token carries the right subject) but the abandoned
//     alias rows still exist and still hold the person's email. They are part of
//     the account and must go with it.
//
// Resolution is read-only and non-destructive: it never fails a deletion for a
// missing row, because an id with no go_users row at all is the ordinary
// legacy-web shape.
func (a *AccountDeleter) ResolveDeletionScope(ctx context.Context, callerID string) (DeletionScope, error) {
	if strings.TrimSpace(callerID) == "" {
		return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope: empty user id")
	}

	canonical := callerID
	var target string
	err := a.pool.QueryRow(ctx, queryConvergenceTarget, callerID).Scan(&target)
	switch {
	case err == nil:
		if t := strings.TrimSpace(target); t != "" {
			canonical = t
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Not a converged alias — the ordinary case.
	default:
		return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope(user=%s): read convergence target: %w", callerID, err)
	}

	scope := DeletionScope{CallerID: callerID, CanonicalID: canonical, IDs: []string{canonical}}
	if callerID != canonical {
		scope.IDs = append(scope.IDs, callerID)
	}

	rows, err := a.pool.Query(ctx, queryConvergenceAliases, canonical)
	if err != nil {
		return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope(user=%s): list convergence aliases: %w", callerID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope(user=%s): scan alias: %w", callerID, err)
		}
		if alias = strings.TrimSpace(alias); alias != "" && !containsID(scope.IDs, alias) {
			scope.IDs = append(scope.IDs, alias)
		}
	}
	if err := rows.Err(); err != nil {
		return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope(user=%s): iterate aliases: %w", callerID, err)
	}

	return scope, nil
}

// containsID is a linear membership test. The closure is a handful of ids at
// the very most — one person, one convergence in practice — so a map would cost
// more than it saves.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
