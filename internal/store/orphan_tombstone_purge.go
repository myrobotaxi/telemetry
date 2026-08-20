package store

// MYR-596: the legacy half of "remove everything".
//
// Step 8e of the account-deletion sequence takes a person's go_removed_vehicles
// tombstones with their account from now on. It does nothing for the accounts
// deleted BEFORE it shipped: their tombstones are still sitting there, a
// (user_id, VIN) pair each, filed against cuids that resolve to nothing. That
// backlog is finite and it never grows again, so this is a one-shot cleanup
// rather than a background job — invoked by cmd/sweep-orphan-fleet-configs
// under -purge-orphan-tombstones, which already owns the dry-run/apply
// semantics and already runs against production by hand.
//
// WHY IT LIVES BESIDE THE SWEEP'S OTHER READS. The sweep's source A is exactly
// this table, so the tool that knows the most about what these rows are worth
// is the one that reads them. It is also the tool that must WARN about the
// consequence, which is real: a purged tombstone is a VIN the sweep can no
// longer name at all. See the binary's header.
//
// EVERY STATEMENT AGAINST "User" IS A READ; the only write is the DELETE
// against go_removed_vehicles, a Go-owned table — the same posture as
// fleet_config_orphans.go, and inside data-lifecycle.md §1.4 without a
// carve-out (CG-DL-9 constrains Prisma-table WRITES).

import (
	"context"
	"fmt"
)

// orphanedTombstonePredicate is the definition of "orphaned", written ONCE and
// shared by the count and the delete so a dry run cannot describe a different
// row set from the apply that follows it.
//
// A tombstone is orphaned when its user_id resolves to no identity ANYWHERE.
// All four probes are needed and none is redundant:
//
//   - "User" — the legacy web account's identity row.
//   - go_users — the Apple-native account's, which has no "User" row at all.
//   - go_identity_apple — the Apple binding. An account can hold one under an
//     id that has neither of the two rows above.
//   - go_identity_convergence — THE GUARD, and the one whose absence would be a
//     bug rather than an omission. After an identity convergence a LIVE person's
//     abandoned cuid may hold tombstones while carrying no identity row of its
//     own (MYR-452, §3.1.1); deleting those would let that person's next Tesla
//     sync resurrect a car they deliberately removed — reinstating precisely the
//     MYR-261 bug. Any id that appears at either end of a convergence edge is
//     therefore left alone. The deletion sequence removes an account's edges in
//     the same pass as its identity rows, so a genuinely deleted account has no
//     edge to hide behind and is still matched here.
//
// The predicate is a compile-time constant with no interpolation: the two
// statements below are string literals concatenated by the compiler, not SQL
// built at runtime.
const orphanedTombstonePredicate = `
WHERE NOT EXISTS (SELECT 1 FROM "User" u              WHERE u."id"       = rv.user_id)
  AND NOT EXISTS (SELECT 1 FROM go_users gu           WHERE gu.id        = rv.user_id)
  AND NOT EXISTS (SELECT 1 FROM go_identity_apple ia  WHERE ia.user_id   = rv.user_id)
  AND NOT EXISTS (SELECT 1 FROM go_identity_convergence c
                  WHERE c.from_user_id = rv.user_id OR c.to_user_id = rv.user_id)`

// queryCountOrphanedTombstones is the dry run's whole answer: how many rows the
// apply would take. A COUNT and not a listing, deliberately — the operator has
// no action to perform per row (there is no token, so nothing at Tesla can be
// reached), and the VINs are P1. The sweep's source-A pass already names the
// ones that still matter, in the same report.
const queryCountOrphanedTombstones = `
SELECT count(*) FROM go_removed_vehicles rv` + orphanedTombstonePredicate

// queryPurgeOrphanedTombstones removes them.
//
// Unbounded on purpose, like queryDeleteOrphanFleetConfigAttempts: the set is a
// backlog that closes and never reopens, and a LIMIT would make "the purge
// cleared everything" untrue in a way the operator could not see.
const queryPurgeOrphanedTombstones = `
DELETE FROM go_removed_vehicles rv` + orphanedTombstonePredicate

// CountOrphanedTombstones reports how many go_removed_vehicles rows name a user
// id that no identity source knows — the pre-MYR-596 deletion backlog. Pure
// read; this is what a dry run calls.
func (r *FleetConfigOrphanRepo) CountOrphanedTombstones(ctx context.Context) (int64, error) {
	var n int64
	if err := r.pool.QueryRow(ctx, queryCountOrphanedTombstones).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountOrphanedTombstones: %w", err)
	}
	return n, nil
}

// PurgeOrphanedTombstones deletes those rows and returns how many went. Called
// only under -apply; it re-proves the orphan predicate inside the DELETE, so a
// convergence or a re-provision landing between the count and the write costs
// nothing. Idempotent — a second run finds none.
func (r *FleetConfigOrphanRepo) PurgeOrphanedTombstones(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, queryPurgeOrphanedTombstones)
	if err != nil {
		return 0, fmt.Errorf("store.PurgeOrphanedTombstones: %w", err)
	}
	return tag.RowsAffected(), nil
}
