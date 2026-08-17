// SQL for the Go-owned go_ride_requests table (MYR-173). Unlike the
// Prisma-owned tables, columns are snake_case and unquoted — see
// migrations/0002_ride_requests.up.sql for the schema and the CG-DL-9
// naming rationale.

package store

// requesterIdentitySelect resolves the requester's display identity (MYR-229)
// INLINE, in the SAME statement as the ride row, via correlated subselects
// keyed on the row's rider_id. Appended to every ride-request projection (plain
// SELECTs, list scans, and every UPDATE/INSERT ... RETURNING) so there is never
// a separate lookup — no after-commit window, no extra round trip, no
// independent outage mode.
//
// The rider's CUID resolves to one of THREE identity sources (MYR-264 — an
// Apple-native rider has no Prisma "User" row, so a User-only lookup omitted
// the name and owners saw a placeholder). Precedence mirrors
// identity.GetUserProfile: Prisma "User" first (legacy/web riders), then the
// Apple first-consent name in go_identity_apple (the real name for Apple-native
// riders), then go_users. `NULLIF(TRIM(...), ”)` drops empty AND whitespace-only
// strings so COALESCE falls through to the next REAL source.
//
// THE `TRIM` IS THE MYR-581 UNIFICATION, and it fixed a latent precedence bug
// here rather than tidying anything: `NULLIF(x, ”)` alone treats "   " as a
// PRESENT value, so a top rung holding whitespace won the COALESCE outright and
// the rungs below it were never consulted — the Go-side reduction then collapsed
// it to "" and the fallback chain fell through to the email local-part, skipping a
// perfectly good real name one rung down. All of the platform's name ladders now
// spell this identically; owner_name.go holds the canonical one and the note on
// why they cannot yet be a single constant (each keys on a different column, and
// the statements embedding them are `const`).
//
// ── MYR-583: THIS LADDER IS DELIBERATELY *NOT* CONFIRMATION-GATED ────────────
//
// MYR-583 made an unconfirmed name read as ABSENT on the two surfaces that name a
// person to a counterparty ahead of time — `VehicleSummary.ownerFirstName` and
// `ShareInvite.acceptedByName` — and to the offerability gate. `requesterName`
// keeps resolving an unconfirmed name, on purpose, for two reasons:
//
//   - THERE IS NO UNCONFIRMED POPULATION HERE GOING FORWARD. A rider on a build
//     carrying the mandatory name prompt has already confirmed on-device before
//     they can create a ride at all, so gating this ladder would change nothing
//     for anybody the feature is about.
//   - FOR THE LEGACY POPULATION IT WOULD *REMOVE* INFORMATION THE OWNER ALREADY
//     RELIES ON. This field is on a card the owner reads while deciding whether to
//     let a stranger into their car, and it has carried a name there since MYR-229.
//     Blanking it would replace "James is requesting a ride" with the make-name
//     fallback — reintroducing MYR-532 item 4's "Tesla wants a ride" on the exact
//     surface that report came from — in service of a consent nicety about whose
//     spelling the owner has no opinion. Withholding a name from a decision-maker
//     mid-decision is a worse outcome than showing one its subject never typed.
//
// The asymmetry is therefore about DIRECTION, not about tiers: MYR-583 governs
// what the platform PUBLISHES about a person before they have approved it, and
// declines to strip a fact the receiving party is already acting on.
//
// All three are READ-ONLY here (CG-DL-9): these SELECT name/email, never write.
// Every column is nullable, so name/email scan into pointers.
// requester_exists (EXISTS across all three tables) distinguishes a deleted
// rider (no row anywhere → requesterName OMITTED) from a row that has neither
// name nor email (→ the "Rider" literal). The resolved value is P1 PII — NEVER
// logged, delivered only on party-scoped surfaces. A NULL/absent identity NEVER
// fails the surrounding ride operation.
const requesterIdentitySelect = `,
	COALESCE(
		NULLIF(TRIM((SELECT u."name" FROM "User" u WHERE u."id" = rider_id)), ''),
		NULLIF(TRIM((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = rider_id ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."name" FROM go_users g WHERE g.id = rider_id)), '')
	) AS requester_name,
	COALESCE(
		NULLIF(TRIM((SELECT u."email" FROM "User" u WHERE u."id" = rider_id)), ''),
		NULLIF(TRIM((SELECT a."email" FROM go_identity_apple a WHERE a.user_id = rider_id ORDER BY a.last_login_at DESC LIMIT 1)), ''),
		NULLIF(TRIM((SELECT g."email" FROM go_users g WHERE g.id = rider_id)), '')
	) AS requester_email,
	(
		EXISTS (SELECT 1 FROM "User" u WHERE u."id" = rider_id)
		OR EXISTS (SELECT 1 FROM go_identity_apple a WHERE a.user_id = rider_id)
		OR EXISTS (SELECT 1 FROM go_users g WHERE g.id = rider_id)
	) AS requester_exists`

// rideRequestColumns is every column read into RideRequestRecord, in scan
// order. Coordinates travel as *_enc ciphertext; the repo decrypts them
// into RidePlace.Latitude/Longitude after scanning. The trailing
// requesterIdentitySelect resolves RequesterName in the same statement.
const rideRequestColumns = `id, rider_id, owner_id, vehicle_id,
	pickup_lat_enc, pickup_lng_enc, pickup_label, pickup_address,
	dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address,
	status, passenger_name, passenger_phone,
	scheduled_for, reschedule_proposed_for, reschedule_status,
	accepted_at, completed_at, created_at, updated_at,
	dispatch_status, dispatched_at, dispatch_error, cancelled_by, trip_version,
	group_ride, join_code, join_code_expires_at` + requesterIdentitySelect

const queryRideRequestInsert = `INSERT INTO go_ride_requests (
	id, rider_id, owner_id, vehicle_id,
	pickup_lat_enc, pickup_lng_enc, pickup_label, pickup_address,
	dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address,
	status, passenger_name, passenger_phone, scheduled_for, group_ride
) VALUES (
	$1, $2, $3, $4,
	$5, $6, $7, $8,
	$9, $10, $11, $12,
	$13, $14, $15, $16, $17
)
RETURNING created_at, updated_at` + requesterIdentitySelect

const queryRideRequestByID = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE id = $1`

// constraintRideActiveInstant is the partial UNIQUE index name from
// migration 0004. A 23505 (unique_violation) carrying this constraint on
// INSERT means the rider already holds an OPEN instant ride — the repo maps
// it to ErrRideRequestActive (MYR-230). Matching on the constraint name (not
// just the SQLSTATE) keeps the mapping precise: a future unique constraint on
// this table would not be misread as an active-ride conflict.
const constraintRideActiveInstant = "uq_go_ride_requests_active_instant_rider"

// constraintVehicleRideActive is the partial UNIQUE index name from migration
// 0013. A 23505 (unique_violation) carrying this constraint on the guarded
// requested->accepted UPDATE means the target VEHICLE is already committed to
// another active instant ride — the repo maps it to ErrVehicleRideActive
// (MYR-266). Matching on the constraint name (not just the SQLSTATE) keeps it
// distinct from the per-rider active-ride conflict on the same table.
const constraintVehicleRideActive = "uq_go_ride_requests_active_instant_vehicle"

// queryRideRequestActiveInstantByRider fetches the rider's single OPEN
// instant request, if any — the one row the partial unique index (0004)
// permits. OPEN = the non-terminal lifecycle states; instant = scheduled_for
// IS NULL. Returned so the 409 `ride_active` body can carry the existing
// request for the client to adopt (MYR-230). LIMIT 1 is belt-and-suspenders:
// the unique index already guarantees at most one match.
const queryRideRequestActiveInstantByRider = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE rider_id = $1
  AND scheduled_for IS NULL
  AND status IN ('requested', 'accepted', 'enroute', 'arrived')
ORDER BY created_at DESC, id DESC
LIMIT 1`

// rideRiderScope is the RIDER LIST's membership predicate (MYR-540). "My rides"
// means the rides I booked PLUS the group rides I joined: a member holds the
// full rider view of a ride they are a party to, so a list that showed them only
// their own bookings would hide the ride they are sitting in.
//
// Written once and shared by the un-anchored and cursor variants so the two
// cannot answer the same question differently — a member seeing a ride on page 1
// and not on page 2 would be worse than not seeing it at all.
//
// EXISTS rather than a join: a ride has at most one membership row per person,
// so a join could not duplicate a row today, but EXISTS says the thing that is
// actually meant ("am I on this ride") and cannot start duplicating if the key
// ever widens. idx_go_ride_members_user serves the sub-probe.
const rideRiderScope = `(rider_id = $1 OR EXISTS (
		SELECT 1 FROM go_ride_members m WHERE m.ride_id = go_ride_requests.id AND m.user_id = $1
	))`

// Newest first, id as the tie-break — matches the contracts
// RideRequestsListResponse ordering (createdAt DESC, id DESC) and the
// idx_go_ride_requests_rider index.
const queryRideRequestsByRider = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE ` + rideRiderScope + `
ORDER BY created_at DESC, id DESC
LIMIT $2`

const queryRideRequestsByOwner = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const queryRideRequestsByOwnerAndStatus = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1 AND status = $2
ORDER BY created_at DESC, id DESC
LIMIT $3`

// queryOpenRideRequestsByRider selects every NON-TERMINAL ride the rider
// holds, instant and scheduled alike, oldest first — the account-deletion
// sweep (MYR-355). The status list is the same OPEN set the per-rider
// active-instant index uses; it is spelled out rather than expressed as "NOT
// IN (terminal)" so that adding a lifecycle state is a deliberate edit here
// rather than a silent widening of what a deletion cancels.
//
// Unbounded on purpose: this is not a user-facing page, and leaving even one
// open request behind would strand an owner with a request from a person who
// no longer exists.
//
// LEAN PROJECTION, deliberately: `id, status` and nothing else. The full
// rideRequestColumns set would drag four AES-256-GCM decryptions of P1
// pickup/dropoff coordinates through a sweep that only needs to know which
// rides to transition — and the guarded UPDATE's own RETURNING already
// resolves the whole record for the rides that actually change. Decrypting
// location data with no use for it is a cost paid in the wrong currency.
const queryOpenRideRequestsByRider = `SELECT id, status
FROM go_ride_requests
WHERE rider_id = $1
  AND status IN ('requested', 'accepted', 'enroute', 'arrived')
ORDER BY created_at ASC, id ASC`

// Cursor (keyset) variants for the paginated HTTP surface (MYR-174/175).
// The (created_at, id) row-value comparison resumes strictly after the
// prior page's last row under the same (created_at DESC, id DESC) ordering,
// so pagination is stable across concurrent inserts (contracts
// RideRequestsListResponse.nextCursor). The HTTP layer over-fetches limit+1
// to drive hasMore without a COUNT — mirrors the drives-list cursor.
const queryRideRequestsByRiderCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE ` + rideRiderScope + ` AND (created_at, id) < ($2, $3)
ORDER BY created_at DESC, id DESC
LIMIT $4`

const queryRideRequestsByOwnerCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1 AND (created_at, id) < ($2, $3)
ORDER BY created_at DESC, id DESC
LIMIT $4`

const queryRideRequestsByOwnerAndStatusCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE owner_id = $1 AND status = $2 AND (created_at, id) < ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $5`

// The lifecycle-transition statements (plain, guarded, and dormancy-guarded)
// live in ride_request_status_queries.go — they share one stamping clause and
// differ only in their WHERE.

// queryRideRequestProposeReschedule opens (or replaces) a reschedule
// negotiation: records the rider's proposed pickup time and flips the
// sub-state to 'requested' (shared-screens.jsx ScheduledRideSheet — the
// owner is asked to re-confirm). The main status column is untouched.
const queryRideRequestProposeReschedule = `UPDATE go_ride_requests SET
	reschedule_proposed_for = $2,
	reschedule_status = 'requested',
	updated_at = NOW()
WHERE id = $1
RETURNING ` + rideRequestColumns

// queryRideRequestResolveReschedule closes a pending negotiation.
// Confirmed ($2 = true): scheduled_for adopts the proposed time and the
// sub-state becomes 'confirmed'. Declined: the sub-state becomes
// 'declined' and scheduled_for keeps the original reservation. The
// proposed time is retained either way for audit (contracts
// rescheduleProposedFor). Only rows with an open 'requested' negotiation
// match — resolving twice (or resolving a ride that was never asked to
// move) affects zero rows and surfaces as ErrRideRequestNotFound.
const queryRideRequestResolveReschedule = `UPDATE go_ride_requests SET
	scheduled_for = CASE WHEN $2 THEN reschedule_proposed_for ELSE scheduled_for END,
	reschedule_status = CASE WHEN $2 THEN 'confirmed' ELSE 'declined' END,
	updated_at = NOW()
WHERE id = $1 AND reschedule_status = 'requested'
RETURNING ` + rideRequestColumns

// queryRideRequestClaimDispatch is the MYR-176 exactly-once dispatch latch:
// it stamps dispatched_at only when it is still NULL, so a re-delivered
// ride.accepted event (or a concurrent second delivery) affects zero rows and
// the dispatcher skips the duplicate nav push. RETURNING id lets the caller
// distinguish "won the claim" (one row) from "already dispatched" (no rows).
const queryRideRequestClaimDispatch = `UPDATE go_ride_requests SET
	dispatched_at = NOW(),
	updated_at = NOW()
WHERE id = $1 AND dispatched_at IS NULL
RETURNING id`

// queryRideRequestRecordDispatch persists the resolved dispatch outcome
// (status + opaque error code) after the claim. dispatch_error is NULL on
// success and on a kill-switch skip; it is set for every 'failed' outcome and
// for the one non-kill-switch skip, 'nav_superseded' (MYR-526). The row is
// already claimed (dispatched_at set), so this is an unconditional update by
// id.
const queryRideRequestRecordDispatch = `UPDATE go_ride_requests SET
	dispatch_status = $2,
	dispatch_error = $3,
	updated_at = NOW()
WHERE id = $1`

// queryRideRequestListInterrupted finds rides claimed for dispatch
// (dispatched_at set) whose outcome never resolved (dispatch_status NULL) and
// whose claim is older than $1 seconds — the orphan signature of a process
// that died between ClaimDispatch and RecordDispatchOutcome (MYR-176 startup
// reconciler). The age floor keeps a live in-flight dispatch from matching.
const queryRideRequestListInterrupted = `SELECT id
	FROM go_ride_requests
	WHERE dispatched_at IS NOT NULL
	  AND dispatch_status IS NULL
	  AND dispatched_at < NOW() - make_interval(secs => $1)`

// --- Leg-2 (dropoff) nav-dispatch (MYR-265; trigger moved to the rider
// start endpoint by MYR-270) ---

// queryRideRequestClaimDropoffDispatch is the leg-2 exactly-once dispatch latch
// (mirror of queryRideRequestClaimDispatch): it stamps dropoff_dispatched_at
// only when still NULL, so a re-delivered ride.started event affects zero rows
// and the dispatcher skips the duplicate dropoff nav push. RETURNING id lets
// the caller distinguish "won the claim" (one row) from "already dispatched"
// (no rows). Independent of the leg-1 dispatched_at latch — both legs claim and
// record on their own columns so neither clobbers the other's history.
const queryRideRequestClaimDropoffDispatch = `UPDATE go_ride_requests SET
	dropoff_dispatched_at = NOW(),
	updated_at = NOW()
WHERE id = $1 AND dropoff_dispatched_at IS NULL
RETURNING id`

// queryRideRequestRecordDropoffDispatch persists the resolved leg-2 outcome
// (status + opaque error code) after the claim. dropoff_dispatch_error follows
// the same rule as dispatch_error: NULL on success and on a kill-switch skip,
// set for every 'failed' outcome and for the 'nav_superseded' skip (MYR-526).
// The row is already claimed (dropoff_dispatched_at set), so this is an
// unconditional update by id.
const queryRideRequestRecordDropoffDispatch = `UPDATE go_ride_requests SET
	dropoff_dispatch_status = $2,
	dropoff_dispatch_error = $3,
	updated_at = NOW()
WHERE id = $1`

// queryRideRequestListInterruptedDropoff is the leg-2 (dropoff) analogue of
// queryRideRequestListInterrupted (MYR-266): it finds rides claimed for the
// DROPOFF push (dropoff_dispatched_at set) whose outcome never resolved
// (dropoff_dispatch_status NULL) and whose claim is older than $1 seconds — the
// orphan signature of a process that died between ClaimDropoffDispatch and
// RecordDropoffDispatchOutcome. A dropoff that RESOLVED (status 'sent'/'failed'/
// 'skipped') has a non-NULL status and is excluded, so the startup reconciler
// never touches a car that already received its dropoff nav. The age floor
// keeps a live in-flight dropoff from matching.
const queryRideRequestListInterruptedDropoff = `SELECT id
	FROM go_ride_requests
	WHERE dropoff_dispatched_at IS NOT NULL
	  AND dropoff_dispatch_status IS NULL
	  AND dropoff_dispatched_at < NOW() - make_interval(secs => $1)`
