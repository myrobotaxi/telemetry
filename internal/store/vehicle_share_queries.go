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
	id, vehicle_id, owner_user_id, label, permission, allow_rides, suspended_at,
	CASE WHEN status = 'pending' THEN code ELSE '' END AS code,
	status, created_at, expires_at, accepted_at,
	COALESCE(accepted_by_user_id, '') AS accepted_by_user_id, revoked_at,
	` + acceptedByNameExpr

// `acceptedByNameExpr` — the ACCEPTING account's resolved display name (MYR-581)
// — lives in vehicle_share_accepted_name.go. It is a bigger fragment than any
// other member of this projection and it carries its own argument about how it
// differs from `label`, so it is split out under the 300-line rule; nothing
// about its position in the column order changes (it is LAST, and
// `scanShare` reads it last).

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
// $7 is the preset's allow_rides projection, seeded at INSERT rather than left
// to the column default so a pending row and the grant it becomes never
// disagree. It is inert until redemption, which re-asserts it anyway.
const queryInsertShare = `
INSERT INTO go_vehicle_shares
	(id, vehicle_id, owner_user_id, label, permission, code, allow_rides, status, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', NOW(), NOW() + ` + shareInviteTTLInterval + `)
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
//
// It also carries vehicle_id, which is what lets the caller tear down the
// revoked viewer's LIVE WebSocket for that one car rather than every session
// they hold (MYR-373, websocket-protocol.md §10 DV-09). Read in the same
// statement that did the revoking, so a concurrent edit cannot make the id and
// the revocation disagree.
const queryRevokeShare = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND status <> 'revoked'
RETURNING COALESCE(accepted_by_user_id, ''), vehicle_id`

// MYR-469 — the RIDER's own way out of a share. Tombstones every ACCEPTED
// grant the caller redeemed on this vehicle — the same tombstone the owner's
// revoke writes (status → revoked, revoked_at stamped; never a hard delete,
// for the same audit reason) — and refuses ATOMICALLY while the caller has a
// live ride on the car: the ride's telemetry access rides the grant, so a
// leave mid-ride is the MYR-449 dark stream self-inflicted. The NOT EXISTS is
// inside the statement so a ride created between a check and the write cannot
// slip through the gap.
const queryLeaveVehicleShares = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status = 'accepted'
  AND NOT EXISTS (
    SELECT 1 FROM go_ride_requests r
    WHERE r.vehicle_id = $1 AND r.rider_id = $2
      AND r.status IN ('requested', 'accepted', 'arrived', 'enroute'))`

// queryViewerLeaveRefused disambiguates a zero-row leave: it answers a row
// exactly when an accepted grant EXISTS and a live ride held it in place —
// i.e. the guard fired. BOTH conditions, deliberately: a caller with a live
// ride and no grant at all (an owner self-riding a never-shared car) has
// nothing to leave, and answering 409 there would be a refusal about a share
// they do not hold. No row → nothing to leave → idempotent success.
const queryViewerLeaveRefused = `
SELECT 1 FROM go_vehicle_shares s
WHERE s.vehicle_id = $1 AND s.accepted_by_user_id = $2 AND s.status = 'accepted'
  AND EXISTS (
    SELECT 1 FROM go_ride_requests r
    WHERE r.vehicle_id = $1 AND r.rider_id = $2
      AND r.status IN ('requested', 'accepted', 'arrived', 'enroute'))
LIMIT 1`

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
//
// TIER-AT-REDEEM (MYR-369). `allow_rides` is computed FROM THE ROW'S OWN
// `permission` inside this UPDATE rather than passed in by the caller. That is
// what makes a pending invite minted before this change redeem to exactly the
// capabilities its preset always implied — including one minted at the retired
// live_history, which maps to false along with 'live'. Doing it in SQL keeps the
// mapping atomic with the accept: there is no window in which a row is accepted
// but not yet capability-stamped, and no Go-side value that could be computed
// from a stale read of `permission`.
//
// `suspended_at = NULL` is asserted too. A pending row cannot be suspended, so
// this is belt and braces against a row that somehow carried the column — a
// freshly accepted grant must never be born paused, because nobody would know
// to un-pause it.
// `updated_at` IS STAMPED HERE TOO (MYR-451), and it has to be: this statement
// MOVES A CAPABILITY. `allow_rides` goes false → true for a `rides` preset, and
// redemption is the only mutation other than the owner's patch that changes it.
// Leaving it unstamped would let a grant that acquired the ride capability at
// redemption still report the invite's creation instant — so the one question
// the column exists to answer ("when did this grant gain or lose rides, before
// or after the ride they took?") would be answered with a date that can be days
// early. The lifecycle transitions that DON'T touch a capability — revoke,
// resend — keep their own dedicated timestamps and deliberately do not stamp
// this one.
const queryAcceptSharesByID = `
UPDATE go_vehicle_shares
SET status = 'accepted', accepted_at = NOW(), accepted_by_user_id = $2,
    allow_rides = (permission = 'rides'), suspended_at = NULL,
    updated_at = NOW()
WHERE id = ANY($1) AND status = 'pending' AND expires_at > NOW()
RETURNING vehicle_id, owner_user_id, allow_rides`

// queryAcceptedSharesByCodeAndUser is the IDEMPOTENT re-redeem lookup: rows
// this same person already accepted under this same code. A retried request
// after a dropped response lands here and returns 200 with the same grants
// instead of 404 or a duplicate row.
//
// SUSPENDED ROWS ARE EXCLUDED (MYR-369), which makes re-redeeming a code whose
// grant the owner has since suspended answer 404 — the same answer an unknown
// or expired code gets. That is the suspension invariant holding on one more
// surface: a suspended grant conveys nothing, so it must not be re-servable as a
// successful join, and the 404 keeps it indistinguishable from every other dead
// code rather than announcing "you were suspended" to somebody the owner chose
// to cut off. The row is untouched and un-suspending restores the re-redeem
// along with everything else.
const queryAcceptedSharesByCodeAndUser = `
SELECT vehicle_id, owner_user_id, allow_rides
FROM go_vehicle_shares
WHERE code = $1 AND status = 'accepted' AND accepted_by_user_id = $2
  AND suspended_at IS NULL`

// queryOwnerFirstNameSources resolves the sharing owner's display identity from
// the three identity sources, in the same precedence order as
// requesterIdentitySelect (MYR-264): a sibling-schema row first, then the
// Apple first-consent name, then go_users. All READ-ONLY.
//
// Only the FIRST NAME reaches the redeemer (P1 policy: first names only, same
// as the push payloads and RideRequest.requesterName) — the reduction to a first
// name happens in Go so the ladder stays one statement.
//
// MYR-581 added `TRIM` to every rung, and it FIXED A LATENT PRECEDENCE BUG rather
// than tidying anything. `NULLIF(x, ”)` alone treats a whitespace-only name as a
// PRESENT value, so a top rung holding "   " won the COALESCE outright and the
// rungs below it were never consulted — the Go-side reduction then collapsed it to
// "" and the caller fell through to the EMAIL local-part, skipping a perfectly
// good real name one rung down. With TRIM, whitespace-only is NULL at every rung
// and the ladder falls through as intended.
//
// MYR-583 LEFT THIS LADDER UNGATED BY CONFIRMATION, and the reason is the FALLBACK
// two lines down rather than any argument about consent. Every other reader of a
// display name renders nothing when the ladder resolves to nothing; this one falls
// through to the EMAIL LOCAL-PART (see vehicle_name.go). So gating it would not
// turn an unconfirmed name into an honest absence — it would turn "Amruth" into
// "amruth.kelkar", disclosing MORE about the owner to the person redeeming their
// invite than the unconfirmed first name did. A gate that makes the leak larger is
// not the gate the ruling asked for. If this surface is revisited, the fallback
// has to be revisited with it.
//
// It also makes this expression agree character-for-character with
// `ownerNameResolvedExpr` (owner_name.go). The two cannot yet be ONE constant — this
// one keys on `$1` and that one on `"Vehicle"."userId"`, and the statements that
// embed them are `const`, so a key-parameterized helper would have to make them
// all `var` — but they must at least AGREE, because both answer "what is this
// person called?" for the same P1 policy.
const queryOwnerFirstNameSources = `
SELECT
	COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = $1)), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = $1 ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = $1)), '')
	) AS owner_name,
	COALESCE(
		NULLIF(TRIM((SELECT u."email" FROM "User" u WHERE u."id" = $1)), ''),
		NULLIF(TRIM((SELECT a."email" FROM go_identity_apple a WHERE a.user_id = $1 ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."email" FROM go_users g WHERE g.id = $1)), '')
	) AS owner_email`

// queryRevokeSharesForVehicle tombstones every live grant on a vehicle. Called
// from the owner-offboarding path: a car that has left the fleet must not keep
// appearing in its viewers' lists.
const queryRevokeSharesForVehicle = `
UPDATE go_vehicle_shares
SET status = 'revoked', revoked_at = NOW()
WHERE vehicle_id = $1 AND status <> 'revoked'`
