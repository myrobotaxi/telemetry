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
const (
	queryLockPrismaUser = `
SELECT 1 FROM "User" WHERE "id" = $1 FOR UPDATE`

	queryLockGoUser = `
SELECT 1 FROM go_users WHERE id = $1 FOR UPDATE`

	queryLockAppleIdentities = `
SELECT apple_sub FROM go_identity_apple WHERE user_id = $1 FOR UPDATE`
)

// queryDeleteAppleIdentity removes every Apple sub bound to the user. Plural
// by construction: the schema indexes user_id precisely because one person may
// hold more than one binding.
const queryDeleteAppleIdentity = `
DELETE FROM go_identity_apple WHERE user_id = $1`

// queryDeleteGoUser removes the Apple-native user row. A legacy web user has
// no row here (their cuid lives in "User"), so this affects zero rows for them.
const queryDeleteGoUser = `
DELETE FROM go_users WHERE id = $1`

// queryDeletePrismaUser removes the sibling-schema "User" row, whose Prisma
// cascades take Account / Settings / Invite / any residual Vehicle with it
// (data-lifecycle.md §3.2). By the time this runs the owner's vehicles have
// already been torn down one at a time by store.OwnerTeardown, so the cascade
// normally has nothing left to do — it is the backstop, not the mechanism.
//
// Apple-native users have no row here at all and this affects zero rows.
const queryDeletePrismaUser = `
DELETE FROM "User" WHERE "id" = $1`

// queryCountUserDrives counts the drives still attached to the user's cars, for
// the audit metadata. Read BEFORE the destructive steps by the handler; zero by
// the time the final transaction runs, which is exactly why it is not read
// there.
const queryCountUserDrives = `
SELECT count(*) FROM "Drive" d
JOIN "Vehicle" v ON v."id" = d."vehicleId"
WHERE v."userId" = $1`
