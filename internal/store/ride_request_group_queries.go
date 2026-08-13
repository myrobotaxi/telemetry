// SQL for the MYR-540 GROUP RIDE: the accept-time code mint, the join
// redemption, and the membership write. Kept in one file for the same reason
// vehicle_share_queries.go is — every predicate below is an access-control
// decision, and the whole set is worth reading at once.
//
// Two invariants hold across it:
//
//  1. THE MINT IS A GUARDED WRITE. `join_code IS NULL AND group_ride` is what
//     makes minting exactly-once and idempotent, not the caller's ordering. A
//     second accept (or a re-delivered one) matches zero rows and mints nothing.
//
//  2. THE REDEEM RESOLVES BY CODE AGAINST ONE COLUMN OF ONE TABLE, with the
//     expiry and the terminal-status refusal IN the predicate so both are
//     evaluated on the DATABASE clock at the instant of the read — and so the
//     three dead-code cases (unknown / expired / terminal) are indistinguishable
//     by construction rather than by the handler remembering to collapse them.

package store

// queryRideRequestMintJoinCode stamps the join code and its expiry onto a
// GROUP ride at accept.
//
// GUARDED THREE WAYS, and each conjunct earns its place:
//
//   - `group_ride` — a solo ride never gets a code. This is the only place the
//     flag gates a write, and it is what makes "shareUrl only on a group ride"
//     a database fact rather than a projection rule that could be got wrong.
//   - `join_code IS NULL` — the exactly-once latch, in the MYR-176
//     ClaimDispatch style. A repeated accept, a retried transaction, or a future
//     second caller mints nothing and the ride keeps the code it already
//     published. Without it, a re-mint would silently invalidate every link
//     already sent.
//   - `status NOT IN (terminal)` — belt and braces. The caller only reaches
//     this on the winning requested→accepted transition, inside that
//     transition's own transaction, so the status cannot have moved; asserting
//     it anyway means no future caller can mint a credential for a dead ride.
//
// The expiry is computed as `NOW() + INTERVAL` on the DATABASE clock — the same
// clock the redeem predicate reads — exactly as shareInviteTTLInterval does.
// Computing it in Go would make "expired" depend on two clocks agreeing.
//
// RETURNING hands back the values the database actually wrote, so the link the
// accept response carries is signed over the row on disk rather than a Go-side
// approximation of it.
const queryRideRequestMintJoinCode = `
UPDATE go_ride_requests
SET join_code = $2,
    join_code_expires_at = NOW() + ` + rideJoinCodeTTLInterval + `,
    updated_at = NOW()
WHERE id = $1
  AND group_ride
  AND join_code IS NULL
  AND status NOT IN ('completed', 'declined', 'cancelled')
RETURNING join_code, join_code_expires_at`

// rideJoinCodeTTLInterval is the join code's lifetime, expressed in SQL so it
// is measured against the database clock.
//
// SEVEN DAYS, the invite link's TTL (shareInviteTTLInterval) and for the same
// reason: it is the outer bound on how long a link that escaped into a group
// chat stays live, and it is deliberately far longer than any ride, because the
// RIDE's own lifecycle — not this expiry — is what ordinarily kills the code. A
// ride reaches a terminal status and the redeem predicate refuses immediately;
// the TTL exists only for the row that never reaches one (an abandoned
// `accepted` reservation nothing swept), where a code with no expiry at all
// would be a credential that outlives everything that could revoke it.
const rideJoinCodeTTLInterval = `INTERVAL '7 days'`

// queryRideRequestByJoinCode resolves a submitted code to the ride it joins.
//
// The three refusals the contract makes INDISTINGUISHABLE — unknown, expired,
// and a ride that has reached a terminal status — are all "zero rows" here.
// That is the point of putting every one of them in the WHERE rather than
// fetching the row and judging it in Go: the handler has one branch to write
// (no row → 404) and cannot accidentally grow a second, and the endpoint
// therefore cannot become an oracle that tells a caller enumerating the 36^6
// space that a guessed code is real but spent.
//
// `group_ride` is asserted even though only a group ride can hold a code. Same
// belt-and-braces reasoning as the mint: the invariant is enforced where the
// decision is made.
//
// It projects the FULL rideRequestColumns set because the 200 body is the whole
// RideRequest object — the joiner goes straight to the rider tracking surface
// with no second round trip, which is the contract's stated reason there is no
// separate response $def.
const queryRideRequestByJoinCode = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE join_code = $1
  AND group_ride
  AND join_code_expires_at > NOW()
  AND status NOT IN ('completed', 'declined', 'cancelled')`

// queryInsertRideMember records a membership.
//
// ON CONFLICT DO NOTHING against the (ride_id, user_id) primary key IS the
// idempotence the contract promises: a joiner who re-submits a code they
// already redeemed inserts nothing and is answered 200 with the same ride,
// rather than a second membership row or a 409. The database arbitrates it, so
// two concurrent redemptions by the same account resolve the same way as two
// sequential ones.
//
// RETURNING joined_at distinguishes the FIRST join (one row back) from a
// re-join (none) — read only for the event publish, never for the response,
// which is identical either way.
const queryInsertRideMember = `
INSERT INTO go_ride_members (ride_id, user_id)
VALUES ($1, $2)
ON CONFLICT (ride_id, user_id) DO NOTHING
RETURNING joined_at`

// queryRideMemberExists probes whether one person is already a member of one
// ride. It backs the ACL widening (loadForParty and its equivalents), which
// asks the question one ride at a time, and it is served by the composite
// primary key.
const queryRideMemberExists = `
SELECT EXISTS (SELECT 1 FROM go_ride_members WHERE ride_id = $1 AND user_id = $2)`

// queryDeleteRideMembershipsForUser drops every membership held by the account
// being deleted (MYR-355's sweep).
//
// The CASCADE on ride_id covers the other direction — a ride's rows die with
// the ride — but says nothing about a MEMBER's own deletion: their rows would
// otherwise survive on somebody else's live ride, keeping a deleted account in
// a `members` array and, worse, in the access set that admits a WebSocket to
// that ride's vehicle. Deleting them here makes the membership die with EITHER
// end of the relationship.
//
// A DELETE rather than a tombstone, and for the saved-places reason rather than
// the revoked-share one: a membership is not evidence in anybody's audit trail.
// The car owner's record of who rode is the RIDE, which survives; a joiner's
// row on it is this person's own participation, and nobody is owed a note that
// a deleted account once sat in a car. Deleting zero rows on a re-run is a
// clean no-op.
const queryDeleteRideMembershipsForUser = `
DELETE FROM go_ride_members WHERE user_id = $1`

// queryRideVehicleIDsForMember is the ACCESS-SET leg (MYR-540): the vehicles a
// person may see because they are riding along on a LIVE group ride.
//
// It is the third source of vehicle access, beside ownership and an accepted
// share, and it is deliberately the narrowest of the three:
//
//   - RIDE-SCOPED — one ride, not a standing grant on the car. The membership
//     row is the whole grant and it dies with the ride.
//   - LIVE ONLY — the terminal statuses are excluded, so access ENDS when the
//     ride does, with no revocation step to remember and nothing to sweep.
//   - VIEWER TIER — this statement only puts the vehicle in the set; the role
//     resolution (auth.ResolveVehicleAccess) is what decides the tier, and it
//     resolves a member to RoleViewer with the zero-value capability set.
const queryRideVehicleIDsForMember = `
SELECT r.vehicle_id
FROM go_ride_members m
JOIN go_ride_requests r ON r.id = m.ride_id
WHERE m.user_id = $1
  AND r.status NOT IN ('completed', 'declined', 'cancelled')`
