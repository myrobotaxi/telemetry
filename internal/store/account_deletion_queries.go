// SQL for the account-deletion writer (MYR-355). See account_deletion.go for
// the sequence and account_deletion_identity.go for the final transaction.

package store

// queryRevokeSharesReceived tombstones every live grant the deleted user
// REDEEMED (the mirror image of queryRevokeSharesForVehicle, which tombstones
// every grant ON a car). Revocation is a tombstone, not a delete (migration
// 0020): the owner's audit trail of who could see their car outlives the
// viewer's account. `status <> 'revoked'` makes a re-run affect zero rows
// instead of re-stamping revoked_at, so the step is idempotent.
const queryRevokeSharesReceived = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE accepted_by_user_id = $1 AND status <> 'revoked'`

// queryScrubSharesReceivedLabel erases the owner-typed label from every share
// row the deleted user had REDEEMED (MYR-447).
//
// WHAT THE LABEL IS, AND WHY IT OUTLIVING THE PERSON IS THE BUG. `label` is a
// free-text name the CAR OWNER typed for their own list — "Mira Chen", "Mom",
// "Roommate" (data-classification.md §1.15). It is P1, it is a third party's
// name, and it is the deleted user's name. Revocation alone (the step above)
// leaves it in place forever: `queryRevokeSharesReceived` moves `status` and
// stamps `revoked_at` and touches nothing else, so before MYR-447 a person
// could delete their account and have their name persist indefinitely in a
// stranger's row, keyed by a cuid that resolves to nothing.
//
// WHY THIS IS A SEPARATE STATEMENT rather than another SET clause on the
// revoke. The revoke is guarded by `status <> 'revoked'` for idempotency, which
// means it deliberately skips rows that were ALREADY revoked — by the owner, or
// by an earlier partial run of this very sequence. Those rows carry the same
// name and are exactly as stale, so folding the scrub into the revoke would
// leave the oldest and most-forgotten labels untouched. This statement is
// therefore keyed only on the person, not on the status.
//
// SCRUB VALUE. `label` is TEXT NOT NULL (migration 0020) so the scrub writes
// the empty string rather than NULL — chosen over widening the column because
// `store.CreateInvite` rejects a blank label at the door, which makes the empty
// string an unambiguous scrubbed-here sentinel that no live row can hold. The
// `label <> ”` predicate then makes a re-run affect zero rows, so the step is
// idempotent on the same terms as every other step in the sequence.
//
// NO WIRE EFFECT. A revoked grant is never serialized to any client (`status`
// has no `revoked` wire member), and the label was owner-facing only — it is
// never delivered to the invited party. So the owner loses a name on a row they
// can no longer see, and nobody's UI changes.
const queryScrubSharesReceivedLabel = `
UPDATE go_vehicle_shares
SET label = ''
WHERE accepted_by_user_id = $1 AND label <> ''`

// queryDeletePushDevicesForUser drops the whole APNs address book for one
// person. Unlike the sign-out unregister (queryDeletePushDevice) this is not
// scoped to one token: the account is going away, so every installation it
// still owns must stop receiving its ride notifications. Deleting zero rows on
// a re-run is a clean no-op.
//
// This is deliberately a DELETE rather than a tombstone: go_push_devices is an
// address book, not a ledger — a stale row is a live capability (anyone who
// resolves this cuid would push to that phone), and Apple's own 410 self-heal
// path already deletes rather than marks.
const queryDeletePushDevicesForUser = `
DELETE FROM go_push_devices WHERE user_id = $1`

// queryDeleteSavedPlacesForUser drops both saved-place slots for one person
// (MYR-321). Not scoped to a kind, unlike the §7.20 per-slot delete: the
// account is going away, so every place it saved goes with it.
//
// A DELETE rather than a tombstone, and this is the one step in the sequence
// where that choice is not merely conventional. The rows hold AES-256-GCM
// ciphertext of where this person LIVES (data-classification.md §1.17) — a
// durable home coordinate, not a one-off trip. A tombstone would keep exactly
// that ciphertext around after somebody exercised their right to erasure, and
// no counterparty is owed a record that this account once knew its owner's
// address: unlike a revoked share, which is evidence in the CAR OWNER's audit
// trail, a saved place is a person's own note to themselves. Nothing outlives
// them here.
//
// Deleting zero rows on a re-run is a clean no-op.
const queryDeleteSavedPlacesForUser = `
DELETE FROM go_saved_places WHERE user_id = $1`

// queryRevokeRefreshTokensForUser revokes every unrevoked refresh token in the
// deleted user's name. Revoke rather than delete, matching the reuse-detection
// model in migration 0003: the rotation lineage is evidence and stays. reason
// is the opaque P0 enum the column already carries.
//
// The ACCESS token cannot be revoked (it is a stateless ES256 JWT with a ~1h
// TTL) — the user-existence check in auth.JWTAuthenticator is what closes that
// window, which is why the handler invalidates the existence cache after the
// identity rows go.
// #nosec G101 -- column/predicate SQL over a hash-only table, not a credential
// (gosec greps the literal 'refresh_tokens' + 'revoked' and misflags it).
const queryRevokeRefreshTokensForUser = `
UPDATE go_refresh_tokens
SET revoked = TRUE, revoked_at = NOW(), reason = 'account_deleted'
WHERE user_id = $1 AND revoked = FALSE`

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
const queryConvergenceCanonical = `
SELECT DISTINCT user_id FROM go_identity_apple WHERE user_id = ANY($1)`

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
// "User"→"Account" ON DELETE CASCADE. Two reasons. First, the cascade is
// defined in the Next.js app's schema, in another repository, and a deletion
// guarantee about live fleet-control credentials should not be enforced only by
// a constraint this repo neither owns nor tests. Second, the cascade cannot fire
// for a row whose "User" is already gone: the Tesla grant is otherwise removed
// only by the LAST-vehicle arm of store.OwnerTeardown, so an owner who is linked
// but holds zero Vehicle rows — exactly the MYR-448 cohort — had nothing at all
// delete their tokens.
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
