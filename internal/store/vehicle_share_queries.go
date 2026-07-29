package store

// SQL for the MYR-184 vehicle-sharing surface (go_vehicle_shares, migration
// 0020). Kept in one file so the whole authorization-relevant statement set can
// be read at once — every WHERE clause here is an access-control decision.
//
// Two invariants hold across every statement below:
//
//  1. Owner-scoped mutations carry `owner_user_id = $n` IN THE STATEMENT. The
//     handler layer's ownership check is a courtesy that produces a good error
//     message; this predicate is what actually prevents one person mutating
//     another's invite, and it is enforced by the database on the same row it
//     writes, so there is no check-then-write window.
//
//  2. The redeem statement is a single conditional UPDATE guarded on
//     `status = 'pending' AND expires_at > NOW()`, with RETURNING. Losers of a
//     concurrent redemption update zero rows and are answered 404 — they never
//     observe a half-granted state.

// shareColumns is the full row projection, with `code` suppressed for any row
// that is not pending. The suppression is IN THE SQL, not in Go, so no read
// path can accidentally carry an accepted grant's code out of the database:
// ShareInvite.code is contractually present only while status is 'pending'.
const shareColumns = `
	id, vehicle_id, owner_user_id, label, permission,
	CASE WHEN status = 'pending' THEN code ELSE '' END AS code,
	status, created_at, expires_at, accepted_at,
	COALESCE(accepted_by_user_id, '') AS accepted_by_user_id, revoked_at`

// queryShareOwnedVehicleIDs filters a requested vehicle-id set down to the ones
// the caller actually owns. READ-ONLY against the sibling-owned vehicle
// relation (CG-DL-9 permits reads; it forbids writes and forbids naming the
// relation in migration SQL). The create path compares the returned count with
// the requested count and refuses the whole invite on any mismatch, so a set
// containing one foreign car mints nothing at all.
const queryShareOwnedVehicleIDs = `
SELECT "id" FROM "Vehicle" WHERE "id" = ANY($1) AND "userId" = $2`

// queryShareCodeInUse reports whether a candidate code is already backing a
// live pending invite. Codes are not unique in the schema (a multi-vehicle
// invite deliberately shares one across N rows), so this pre-check is what
// keeps a fresh mint from colliding with an outstanding one. See
// mintUnusedShareCode for the residual race and why it is acceptable.
const queryShareCodeInUse = `
SELECT EXISTS (SELECT 1 FROM go_vehicle_shares WHERE code = $1 AND status = 'pending')`

// shareInviteTTLInterval is the 7-day code lifetime, expressed in SQL so the
// TTL is measured against the DATABASE clock — the same clock the redeem
// statement's `expires_at > NOW()` predicate reads. Computing it in Go instead
// would make "expired" depend on two clocks agreeing.
const shareInviteTTLInterval = `INTERVAL '7 days'`

// queryInsertShare creates one pending invite row. Called once per vehicle in a
// multi-vehicle create, all rows sharing the code passed as $6.
//
// RETURNING hands back the timestamps the database actually wrote, so the row
// the caller returns to the client is the row on disk — not a Go-side
// approximation that drifts from it by however long the round trip took.
const queryInsertShare = `
INSERT INTO go_vehicle_shares
	(id, vehicle_id, owner_user_id, label, permission, code, status, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, 'pending', NOW(), NOW() + ` + shareInviteTTLInterval + `)
RETURNING created_at, expires_at`

// queryListSharesByVehicle is the owner's sharing screen: pending invites and
// accepted grants for one car, newest first. Revoked tombstones are filtered
// out here — they are audit state, and the wire status enum has no member for
// them. Scoped to owner_user_id so the listing cannot be used to read another
// owner's invites even with a guessed vehicle id.
const queryListSharesByVehicle = `
SELECT` + shareColumns + `
FROM go_vehicle_shares
WHERE vehicle_id = $1 AND owner_user_id = $2 AND status <> 'revoked'
ORDER BY created_at DESC, id DESC`

// queryRevokeShare is the tombstone flip. Guarded on owner_user_id (nobody
// revokes another owner's grant) and on `status <> 'revoked'` so a repeat is a
// no-op rather than a second revoked_at stamp. Zero rows affected means either
// "already revoked" or "not yours / does not exist"; the caller disambiguates
// with queryShareExistsForOwner, because those two must produce DIFFERENT
// statuses (204 idempotent vs 404) while remaining indistinguishable from the
// outside for a row the caller has no business seeing.
//
// RETURNING carries the accepted_by_user_id (empty for a pending row) so the
// caller can bust that viewer's cached access set immediately — otherwise a
// revoked viewer keeps resolving the vehicle for up to the cache TTL.
const queryRevokeShare = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND status <> 'revoked'
RETURNING COALESCE(accepted_by_user_id, '')`

// queryShareExistsForOwner probes whether a row exists AND belongs to the
// caller. Used only to disambiguate a zero-row conditional update.
const queryShareExistsForOwner = `
SELECT status FROM go_vehicle_shares WHERE id = $1 AND owner_user_id = $2`

// queryLockResendSiblings selects and LOCKS every pending row that shares the
// TARGET ROW'S CURRENT CODE, for the same owner — that set is the invite, and
// the row named in the path is only one member of it.
//
// The subquery is what makes the set the right one. Keying on the code (not on
// the owner, and not on the id) is deliberate in both directions: an owner may
// hold several unrelated pending invites, which a resend must not disturb, and
// one invite may span N vehicles, every one of which must be re-minted or the
// old code stays live on the rest. A NULL subquery result — no such pending row
// for this owner — matches nothing, which is how "not yours / not pending" falls
// through to the explain probe.
//
// FOR UPDATE serializes this against a concurrent redemption of the same code:
// whichever transaction locks first wins outright, and the loser re-reads rows
// that no longer qualify rather than acting on a stale code.
const queryLockResendSiblings = `
SELECT id FROM go_vehicle_shares
WHERE owner_user_id = $2 AND status = 'pending'
  AND code = (
    SELECT code FROM go_vehicle_shares
    WHERE id = $1 AND owner_user_id = $2 AND status = 'pending'
  )
FOR UPDATE`

// queryResendShare re-mints the code and pushes the expiry out across the WHOLE
// locked sibling set, in one statement. The invite ids a client holds stay valid
// across a resend, and created_at is deliberately untouched (the owner's
// "sent {ago}" line still refers to the original send).
//
// The 'pending' predicate AND `owner_user_id` are re-asserted even though the
// ids came from an owner-scoped, already-locked SELECT in the same transaction.
// That is invariant (1) at the top of this file holding: the write itself
// carries the ownership predicate, so no reordering, refactor, or future caller
// of this statement can produce a cross-owner mutation. Same belt-and-braces
// principle as queryAcceptSharesByID — the predicate is the contract, the lock
// is only the serialization.
//
// RETURNING hands back every re-minted row so the caller can pick out the path
// row without a second read.
const queryResendShare = `
UPDATE go_vehicle_shares
SET code = $2, expires_at = NOW() + ` + shareInviteTTLInterval + `
WHERE id = ANY($1) AND owner_user_id = $3 AND status = 'pending'
RETURNING` + shareColumns

// queryLockPendingByCode selects the candidate rows a redemption would grant
// and LOCKS them for the duration of the transaction. The expiry predicate is
// here (not in Go) so "expired" is evaluated against the database clock at the
// instant of the write.
//
// FOR UPDATE is what serializes concurrent redemptions of one code: the second
// transaction blocks here, and when it proceeds the rows it re-reads are no
// longer 'pending', so it selects nothing and is answered 404 — never a partial
// grant, never a second grant.
const queryLockPendingByCode = `
SELECT id, vehicle_id, owner_user_id, permission
FROM go_vehicle_shares
WHERE code = $1 AND status = 'pending' AND expires_at > NOW()
FOR UPDATE`

// queryAcceptSharesByID flips the locked rows to accepted in one statement,
// re-asserting the pending + unexpired predicate so the write is conditional
// even though the rows are already locked (belt and braces: the predicate, not
// the lock, is the contract). RETURNING gives the caller exactly the rows it
// actually changed.
const queryAcceptSharesByID = `
UPDATE go_vehicle_shares
SET status = 'accepted', accepted_at = NOW(), accepted_by_user_id = $2
WHERE id = ANY($1) AND status = 'pending' AND expires_at > NOW()
RETURNING vehicle_id, owner_user_id, permission`

// queryAcceptedSharesByCodeAndUser is the IDEMPOTENT re-redeem lookup: rows
// this same person already accepted under this same code. A retried request
// after a dropped response lands here and returns 200 with the same grants
// instead of 404 or a duplicate row.
const queryAcceptedSharesByCodeAndUser = `
SELECT vehicle_id, owner_user_id, permission
FROM go_vehicle_shares
WHERE code = $1 AND status = 'accepted' AND accepted_by_user_id = $2`

// queryAcceptedShareVehicleIDs is the VIEWER ACCESS SET — the vehicles a person
// may see because somebody shared them, unioned with their own cars by the
// authenticator. Served by the leading column of the partial-unique
// accepted-grant index.
const queryAcceptedShareVehicleIDs = `
SELECT vehicle_id FROM go_vehicle_shares
WHERE accepted_by_user_id = $1 AND status = 'accepted'`

// queryAcceptedSharePermission resolves the tier one person holds over one
// vehicle. Returns no row when there is no accepted grant, which the caller
// MUST treat as "no access" — never as a default tier.
const queryAcceptedSharePermission = `
SELECT permission FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status = 'accepted'
LIMIT 1`

// queryOwnerFirstNameSources resolves the sharing owner's display identity from
// the three identity sources, in the same precedence order as
// requesterIdentitySelect (MYR-264): a sibling-schema row first, then the
// Apple first-consent name, then go_users. All READ-ONLY.
//
// Only the FIRST NAME reaches the redeemer (P1 policy: first names only, same
// as the push payloads and RideRequest.requesterName) — the trimming happens in
// Go so the ladder stays one statement.
const queryOwnerFirstNameSources = `
SELECT
	COALESCE(
		NULLIF((SELECT u."name" FROM "User" u WHERE u."id" = $1), ''),
		NULLIF((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = $1 ORDER BY a.last_login_at DESC LIMIT 1), ''),
		NULLIF((SELECT g."name" FROM go_users g WHERE g.id = $1), '')
	) AS owner_name,
	COALESCE(
		NULLIF((SELECT u."email" FROM "User" u WHERE u."id" = $1), ''),
		NULLIF((SELECT a."email" FROM go_identity_apple a WHERE a.user_id = $1 ORDER BY a.last_login_at DESC LIMIT 1), ''),
		NULLIF((SELECT g."email" FROM go_users g WHERE g.id = $1), '')
	) AS owner_email`

// queryRevokeSharesForVehicle tombstones every live grant on a vehicle. Called
// from the owner-offboarding path: a car that has left the fleet must not keep
// appearing in its viewers' lists.
const queryRevokeSharesForVehicle = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE vehicle_id = $1 AND status <> 'revoked'`
