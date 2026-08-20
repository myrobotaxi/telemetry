// SQL for the account-deletion IDENTITY TRANSACTION (MYR-355) — step 10 of the
// data-lifecycle.md §3.1 sequence, plus the convergence walk (MYR-452) that
// resolves the scope every earlier step is keyed on.
//
// Split out of account_deletion_queries.go, which holds the per-step
// user-scoped statements (steps 1 and 4-9). The seam is the same one
// account_deletion.go / account_deletion_identity.go already draw: those are
// statements each run on their own, over one id, outside any transaction; these
// run together, over the whole DeletionScope, inside the single transaction
// that takes the row locks and writes the audit row.

package store

// The three identity-existence probes, each taking a ROW LOCK.
//
// They answer two questions at once: whether ANYTHING is left to delete (which
// decides whether an audit row is written at all, so a re-run cannot duplicate
// it) and whether a Prisma "User" row exists (dual-source identity — an
// Apple-native user has none).
//
// FOR UPDATE is what makes the "write the audit row only if something was
// there" decision race-safe rather than merely atomic, and it is load-bearing
// precisely BECAUSE the client is told to retry: a user tapping Delete twice,
// or retrying while the first request is still in flight, would otherwise have
// both transactions read "rows exist" and both write an `account_deleted` row
// for one account. With the locks, the second transaction blocks until the
// first commits and then — at READ COMMITTED, where a locking read re-evaluates
// after the lock is released — finds no row and takes the already-gone arm.
// `TestAccountDeleter_DeleteIdentity_ConcurrentCallsWriteOneAuditRow` fails
// with 4 deleted / 0 already-gone if these three clauses are dropped.
//
// Three statements rather than one `EXISTS(...)` projection because `FOR
// UPDATE` cannot be applied to a subquery's rows: the locks have to be taken by
// the selects that actually touch the rows. `go_identity_apple` is read as a
// row set rather than a count for the same reason (an aggregate forbids FOR
// UPDATE) — and correctly so, since one person may hold several bindings.
//
// Each probe takes the whole DeletionScope (MYR-452), not a single id: after an
// identity convergence the caller's JWT subject and the id holding the rows are
// different values, and a probe that saw only the subject would report
// "already gone" over an account that is entirely intact.
const (
	queryLockPrismaUser = `
SELECT 1 FROM "User" WHERE "id" = ANY($1) FOR UPDATE`

	queryLockGoUser = `
SELECT 1 FROM go_users WHERE id = ANY($1) FOR UPDATE`

	queryLockAppleIdentities = `
SELECT apple_sub FROM go_identity_apple WHERE user_id = ANY($1) FOR UPDATE`
)

// queryConvergenceClosure walks go_identity_convergence out from the caller's
// subject, treating the graph as UNDIRECTED (MYR-452).
//
// Both directions matter, for different reasons. Forward finds the id the
// account was moved to, when the caller presents an abandoned subject. Reverse
// finds the abandoned subjects, when the caller signed in again after
// converging and so presents the right id while the old ones still carry rows.
//
// `UNION` (not UNION ALL) is what terminates the recursion: it deduplicates
// against the rows already produced, so a 2-cycle — reachable by re-linking the
// same Tesla under the new canonical id — closes instead of spinning. The CASE
// is what lets ONE recursive self-reference cover both directions, which
// Postgres requires (it permits only a single reference to the recursive term).
//
// $2 bounds the result one above the scope cap, so an implausibly large closure
// is detected and refused rather than silently truncated to a set that would
// delete some of a person's rows and leave the rest.
const queryConvergenceClosure = `
WITH RECURSIVE closure(id) AS (
    SELECT $1::text
  UNION
    SELECT CASE WHEN c.from_user_id = cl.id THEN c.to_user_id ELSE c.from_user_id END
    FROM go_identity_convergence c
    JOIN closure cl ON cl.id = c.from_user_id OR cl.id = c.to_user_id
)
SELECT id FROM closure LIMIT $2`

// queryConvergenceCanonical finds which member of the closure the Apple binding
// is filed under — the id that IS the account. It returns no rows for an
// account whose binding is already gone (a re-run), where the caller stands for
// itself.
// ORDER BY keeps the answer deterministic: without it the row order is
// whatever the plan emits, and the audit row's target could differ between two
// runs over the same data.
const queryConvergenceCanonical = `
SELECT DISTINCT user_id FROM go_identity_apple WHERE user_id = ANY($1) ORDER BY user_id`

// queryDeleteConvergenceEdges removes the account's convergence edges. They are
// pure identity plumbing: once the ids they connect are gone, an edge is a
// dangling grant of delete authority over a cuid that resolves to nothing.
const queryDeleteConvergenceEdges = `
DELETE FROM go_identity_convergence
WHERE from_user_id = ANY($1) OR to_user_id = ANY($1)`

// queryDeleteAppleIdentity removes every Apple sub bound to the user. Plural
// by construction: the schema indexes user_id precisely because one person may
// hold more than one binding.
// It is keyed on the whole scope: the binding of a converged owner is filed
// under the canonical id, NOT under the subject their token presents, and
// leaving it standing is precisely what let a deleted account be recognised and
// signed back into on the next Sign in with Apple (MYR-452).
const queryDeleteAppleIdentity = `
DELETE FROM go_identity_apple WHERE user_id = ANY($1)`

// queryDeleteGoUser removes the Apple-native user row, and — because it is
// keyed on the whole scope — every abandoned alias row the person accumulated
// through identity convergence. Those aliases hold the person's email and are
// referenced by nothing; before MYR-452 they outlived the account.
//
// A legacy web user has no row here at all (their cuid lives in "User"), so
// this affects zero rows for them.
const queryDeleteGoUser = `
DELETE FROM go_users WHERE id = ANY($1)`

// queryDeleteProviderAccounts removes every stored OAuth grant in the account's
// name — all providers, not just Tesla.
//
// This is deliberately EXPLICIT rather than left to the Prisma
// "User"→"Account" ON DELETE CASCADE, which does exist and does work. The
// cascade is defined in the Next.js app's schema, in ANOTHER REPOSITORY, and a
// deletion guarantee about live fleet-control credentials should not be
// enforced only by a constraint this server neither owns, migrates, nor tests —
// a schema change there would silently turn our promise off, and the failure
// would be invisible until someone audited a database dump.
//
// It is redundant today and that is the point: the statement makes the
// guarantee local and testable.
//
// It runs inside the identity transaction, long after the best-effort revoke at
// Tesla (MYR-366) has had its turn with the refresh token.
// #nosec G101 -- column/predicate SQL, not a credential (gosec greps the
// literal 'Account' + 'token' shapes and misflags it).
const queryDeleteProviderAccounts = `
DELETE FROM "Account" WHERE "userId" = ANY($1)`

// queryDeleteSettings removes the account's Settings row. The per-vehicle
// teardown only RESETS the link/pairing flags (it upserts them to FALSE,
// because the owner is still there); when the owner themselves is going, the
// row goes.
const queryDeleteSettings = `
DELETE FROM "Settings" WHERE "userId" = ANY($1)`

// queryDeletePrismaUser removes the sibling-schema "User" row, whose Prisma
// cascades take Invite / any residual Vehicle with it (data-lifecycle.md §3.2).
// By the time this runs the owner's vehicles have already been torn down one at
// a time by store.OwnerTeardown, so the cascade normally has nothing left to do
// — it is the backstop, not the mechanism. Account and Settings are no longer
// left to it at all; see the two statements above.
//
// Apple-native users have no row here at all and this affects zero rows.
const queryDeletePrismaUser = `
DELETE FROM "User" WHERE "id" = ANY($1)`

// queryCountUserDrives counts the drives still attached to the user's cars, for
// the audit metadata. Read BEFORE the destructive steps by the handler; zero by
// the time the final transaction runs, which is exactly why it is not read
// there.
const queryCountUserDrives = `
SELECT count(*) FROM "Drive" d
JOIN "Vehicle" v ON v."id" = d."vehicleId"
WHERE v."userId" = $1`
