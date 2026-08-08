package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// maxDeletionScopeIDs caps how many user ids one deletion may claim.
//
// A row in go_identity_convergence is an unconditional grant of DELETE
// authority over another id, so the size of the closure is the blast radius of
// a single Delete tap. A real person accumulates one convergence, maybe two;
// anything approaching this number means the graph has been corrupted, and the
// safe response to "I am not sure whose account this is" is to refuse rather
// than to delete them all. Exceeding it fails the deletion with a 500, which is
// recoverable — deleting a stranger's account is not.
const maxDeletionScopeIDs = 8

// DeletionScope is the set of user ids that constitute ONE human, expanded from
// the subject the caller authenticated with (MYR-452).
//
// WHY A SET AND NOT A STRING. The identity model has two sources — go_users for
// Apple-native accounts, the Prisma "User" table for legacy web ones — and one
// sanctioned operation that MOVES a person between them:
// OwnerProvisioner.rebindApple, which fires when a Tesla link proves the caller
// and an existing owner are the same person. It re-points the Apple binding onto
// the canonical id and abandons the caller's original one.
//
// Nothing re-issues the caller's tokens when that happens. Their JWT keeps
// naming the pre-convergence id for the life of its refresh family, so the
// subject arriving at DELETE /api/users/me is routinely NOT the id that owns the
// account's rows. Keyed on the subject alone, the teardown revoked nothing,
// deleted nothing, wrote its audit row and answered 204 — and the surviving
// binding signed the person back in as Owner on their next Sign in with Apple.
type DeletionScope struct {
	// CallerID is the JWT subject as presented — always a member of IDs.
	CallerID string
	// CanonicalID is the id the account actually lives under: whichever member
	// of the closure holds the Apple binding, falling back to the caller when
	// no binding exists (an un-converged account, or a re-run after the binding
	// is already gone). The audit row is filed against it.
	//
	// Deriving it from the binding rather than from the direction of the
	// convergence edges is what keeps it correct when the graph contains a
	// cycle, where "whatever the arrows point at" is not well defined.
	CanonicalID string
	// IDs is every id in the closure, CanonicalID first and the remainder
	// sorted. Deduplicated, never empty. In the overwhelmingly common
	// un-converged case it is exactly [CallerID].
	IDs []string
}

// Converged reports whether the caller authenticated under an id other than the
// one that owns the account, or trails abandoned aliases. P0-safe to log.
func (s DeletionScope) Converged() bool {
	return s.CanonicalID != s.CallerID || len(s.IDs) > 1
}

// rowQuerier is the read surface the closure walk needs, satisfied by both
// *pgxpool.Pool and pgx.Tx. It exists so the walk can be re-run INSIDE the
// identity transaction, against the same snapshot as the deletes.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ResolveDeletionScope expands the caller's JWT subject into the closure of ids
// reachable through go_identity_convergence, in BOTH directions:
//
//   - FORWARD: the caller is an abandoned id that was re-pointed away. The edge
//     names the id the account actually lives under.
//   - REVERSE: the caller already IS the canonical id (they signed in again
//     after converging, so their token carries the right subject) but the
//     abandoned ids still exist and still carry rows keyed to them.
//
// The walk is transitive and cycle-safe, because neither property is guaranteed
// by the writer: a chain forms when a person converges twice, and a 2-cycle
// forms when re-linking the same Tesla under the new canonical id converges
// back the other way.
//
// Resolution is read-only. It never fails a deletion for a missing row — an id
// with no convergence edge at all is the ordinary shape — but it DOES fail for
// an implausibly large closure, on the reasoning at maxDeletionScopeIDs.
func (a *AccountDeleter) ResolveDeletionScope(ctx context.Context, callerID string) (DeletionScope, error) {
	return resolveDeletionScope(ctx, a.pool, callerID)
}

func resolveDeletionScope(ctx context.Context, q rowQuerier, callerID string) (DeletionScope, error) {
	callerID = strings.TrimSpace(callerID)
	if callerID == "" {
		return DeletionScope{}, fmt.Errorf("store.ResolveDeletionScope: empty user id")
	}

	ids, err := convergenceClosure(ctx, q, callerID)
	if err != nil {
		return DeletionScope{}, err
	}
	canonical, err := canonicalIDForScope(ctx, q, callerID, ids)
	if err != nil {
		return DeletionScope{}, err
	}
	return DeletionScope{
		CallerID:    callerID,
		CanonicalID: canonical,
		IDs:         orderScopeIDs(canonical, ids),
	}, nil
}

// orderScopeIDs puts the canonical id first and sorts the rest, so the audit
// target, the log lines and the tests are all deterministic.
func orderScopeIDs(canonical string, ids []string) []string {
	rest := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != canonical {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append([]string{canonical}, rest...)
}

// convergenceClosure walks the undirected convergence graph out from callerID.
func convergenceClosure(ctx context.Context, q rowQuerier, callerID string) ([]string, error) {
	rows, err := q.Query(ctx, queryConvergenceClosure, callerID, maxDeletionScopeIDs+1)
	if err != nil {
		return nil, fmt.Errorf("store.ResolveDeletionScope(user=%s): walk convergence closure: %w", callerID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ResolveDeletionScope(user=%s): scan closure member: %w", callerID, err)
		}
		if id = strings.TrimSpace(id); id != "" && !containsID(ids, id) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ResolveDeletionScope(user=%s): iterate closure: %w", callerID, err)
	}

	if len(ids) > maxDeletionScopeIDs {
		return nil, fmt.Errorf(
			"store.ResolveDeletionScope(user=%s): closure of %d ids exceeds the %d-id cap; refusing to delete on a corrupt convergence graph",
			callerID, len(ids), maxDeletionScopeIDs)
	}
	if len(ids) == 0 {
		// The recursive term always yields the seed, so this is unreachable in
		// practice. Treat it as "the caller stands for itself" rather than
		// hand back an empty scope a caller could misread as "delete nothing".
		ids = []string{callerID}
	}
	return ids, nil
}

// canonicalIDForScope picks the member of the closure that holds the Apple
// binding — the id the account is actually filed under.
func canonicalIDForScope(ctx context.Context, q rowQuerier, callerID string, ids []string) (string, error) {
	rows, err := q.Query(ctx, queryConvergenceCanonical, ids)
	if err != nil {
		return "", fmt.Errorf("store.ResolveDeletionScope(user=%s): find binding owner: %w", callerID, err)
	}
	defer rows.Close()

	canonical := ""
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return "", fmt.Errorf("store.ResolveDeletionScope(user=%s): scan binding owner: %w", callerID, err)
		}
		// One person's bindings should all sit under a single id. If they do
		// not, prefer the caller's own — the audit row should name the id the
		// person actually authenticated with.
		if canonical == "" || owner == callerID {
			canonical = owner
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store.ResolveDeletionScope(user=%s): iterate binding owners: %w", callerID, err)
	}
	if canonical == "" {
		canonical = callerID
	}
	return canonical, nil
}

// unionScopeWithin re-walks the closure inside an open transaction and merges
// anything new into the scope resolved at step 0.
//
// Step 0 runs before ten other steps, on a different connection and a different
// snapshot. A Tesla link committing a convergence in that window would leave the
// sequence keyed on a stale set — the MYR-452 failure re-expressed as a race.
// Re-walking under the identity transaction closes it. The merge only ever ADDS
// ids, so a closure that has since shrunk still deletes everything the sequence
// set out to delete.
func unionScopeWithin(ctx context.Context, tx pgx.Tx, scope DeletionScope) (DeletionScope, error) {
	fresh, err := convergenceClosure(ctx, tx, scope.CallerID)
	if err != nil {
		return DeletionScope{}, err
	}
	merged := scope
	for _, id := range fresh {
		if !containsID(merged.IDs, id) {
			merged.IDs = append(merged.IDs, id)
		}
	}
	if len(merged.IDs) > maxDeletionScopeIDs {
		return DeletionScope{}, fmt.Errorf(
			"store.DeleteIdentity(user=%s): closure grew to %d ids, past the %d-id cap; refusing to delete",
			scope.CanonicalID, len(merged.IDs), maxDeletionScopeIDs)
	}
	merged.IDs = orderScopeIDs(merged.CanonicalID, merged.IDs)
	return merged, nil
}

// containsID is a linear membership test. The closure is capped at
// maxDeletionScopeIDs, so a map would cost more than it saves.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
