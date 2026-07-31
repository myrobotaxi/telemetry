package store

// The ACCESS-CONTROL half of the go_vehicle_shares statement set (MYR-184,
// MYR-369). Split out of vehicle_share_queries.go — which holds the invite
// LIFECYCLE statements, mint through revoke — because these are the statements
// that decide what a live grant conveys.
//
// TWO INVARIANTS govern every statement in this file:
//
//	(1) THE PREDICATE IS THE AUTHORIZATION. The check that decides whether the
//	    caller may see or change the row lives in that same statement's WHERE
//	    clause, on the same row it reads or writes — never in Go wrapped around
//	    a broader statement. That is what closes the check-then-act window and
//	    what makes a future caller unable to reach the row by skipping a guard.
//
//	(2) EVERY ACCESS-SET STATEMENT CARRIES `status = 'accepted' AND
//	    suspended_at IS NULL`. A statement that omits the suspension term is a
//	    suspended viewer keeping access.
//
// WHERE THE SUSPENSION PREDICATE ACTUALLY LIVES — read this before assuming
// this file is the whole story, because IT IS NOT. Reading these statements
// together does NOT make a missing suspension term visible, and an earlier
// version of this comment claimed it did. The predicate is REPEATED across six
// live statements in five files in two packages, and only two of them are
// here:
//
//	internal/auth/queries.go            queryUserVehicleIDs
//	    The merged access set (owned UNION shared). The WebSocket subscribed
//	    set and its cache, GET /api/vehicles, and every per-vehicle "may this
//	    caller see this car" resolve through THIS one, not through anything in
//	    this file.
//	internal/auth/vehicle_access.go     queryShareGrant
//	    Per-vehicle capability resolution behind ResolveVehicleAccess.
//	internal/store/vehicle_repo_list_shared.go  sharedSummaryJoin
//	    The viewer catalog. The predicate sits in the JOIN because the join IS
//	    the access check.
//	internal/store/vehicle_share_queries.go     queryAcceptedSharesByCodeAndUser
//	    The idempotent re-redeem lookup — what makes re-redeeming a suspended
//	    grant's code answer 404.
//	THIS FILE                           queryAcceptedShareGrant
//	    The REST snapshot gate (ShareGrantFor).
//	THIS FILE                           queryRiderMayRequestRides
//	    The reservation-dispatch probe used by the sweeper.
//
// The auth/store split is STRUCTURALLY FORCED, not sloppiness: internal/auth
// cannot import internal/store — the dependency rule runs the other way
// (internal/auth is a dependency of internal/ws, never the reverse) — so the two
// packages necessarily carry their own copies. Within internal/store the
// repetition is because each statement serves a different surface with a
// different shape (a join, an EXISTS probe, a code lookup, a flags lookup) and
// there is no one statement they could share.
//
// The honest consequence: this invariant is maintained by REPETITION, so it is
// held by convention plus tests, not by construction. That is why the list above
// exists, why it is inventoried site by site in rest-api.md §7.5.0, and why each
// site carries a DB-level test — including auth.queryUserVehicleIDs, whose term
// was untested until MYR-369's fix round (store.authAccessSet).

// queryAcceptedShareGrant resolves the CAPABILITY FLAGS one person holds over
// one vehicle. Returns no row when there is no accepted grant OR when the grant
// is suspended — the caller MUST treat both as "no access", and they are
// deliberately the same answer.
//
// `permission` is not selected: it is the invite-time preset and derived output,
// and no gate may read it (MYR-369).
const queryAcceptedShareGrant = `
SELECT allow_rides FROM go_vehicle_shares
WHERE vehicle_id = $1 AND accepted_by_user_id = $2 AND status = 'accepted'
  AND suspended_at IS NULL
LIMIT 1`

// queryRiderMayRequestRides answers, in ONE statement, whether a person may
// still ride in a vehicle (MYR-369) — used by the reservation sweeper, which
// runs outside any request and therefore has no handler-resolved vehicle row to
// lean on.
//
// Two disjuncts, in the order they are cheapest and most common: the rider OWNS
// the car (the ordinary reservation, and the reason the grant table is never
// touched for it), or they hold a LIVE accepted grant carrying `allow_rides`.
// A suspended grant fails the second disjunct exactly as a revoked or absent one
// does — the suspension invariant reaching the one enforcement layer that can
// see a reservation accepted BEFORE the owner withdrew access.
//
// One statement rather than two round trips so ownership and the grant are read
// from a single snapshot: a sweeper that read them separately could see a car
// transferred between the two and answer from a state that never existed.
const queryRiderMayRequestRides = `
SELECT EXISTS (
	SELECT 1 FROM "Vehicle" WHERE "id" = $2 AND "userId" = $1
) OR EXISTS (
	SELECT 1 FROM go_vehicle_shares
	WHERE vehicle_id = $2 AND accepted_by_user_id = $1
	  AND status = 'accepted' AND suspended_at IS NULL AND allow_rides
)`

// queryPatchShare is the MYR-369 owner edit of ONE ACCEPTED grant: a single
// conditional UPDATE with RETURNING, and no read-then-write anywhere.
//
// PARTIAL WITHOUT A QUERY BUILDER. $3/$4 and $5/$6 are (present, value) pairs:
// each column is assigned `CASE WHEN $present THEN $value ELSE <itself> END`, so
// an absent field is written back to exactly what it already held rather than
// being skipped. That is what makes an absent field different from `false`
// without composing SQL strings at runtime — one statement, one plan, no
// injection surface, and the partial semantics are visible in the statement
// instead of in the Go that assembled it.
//
// The suspension timestamp is stamped with NOW() on the transition and cleared
// to NULL on restore, so `suspended_at` records WHEN — which a boolean could
// not. Re-suspending an already-suspended grant REFRESHES the timestamp; that is
// accepted, because the interesting instant for an audit is the most recent
// decision and the endpoint is idempotent in effect either way.
//
// THE PREDICATE IS THE AUTHORIZATION. `owner_user_id = $2` is carried by the
// WRITE, on the same row it changes, so there is no check-then-write window and
// no reordering or future caller can produce a cross-owner mutation — invariant
// (1) at the top of this file. `status = 'accepted'` is likewise in the write:
// a pending or revoked row updates zero rows, and the caller disambiguates with
// queryShareExistsForOwner rather than pre-reading.
const queryPatchShare = `
UPDATE go_vehicle_shares
SET allow_rides  = CASE WHEN $3::boolean THEN $4::boolean ELSE allow_rides END,
    suspended_at = CASE
                     WHEN NOT $5::boolean THEN suspended_at
                     WHEN $6::boolean      THEN NOW()
                     ELSE NULL
                   END
WHERE id = $1 AND owner_user_id = $2 AND status = 'accepted'
RETURNING` + shareColumns
