// SQL for the Go-owned go_ride_requests table (MYR-173). Unlike the
// Prisma-owned tables, columns are snake_case and unquoted — see
// migrations/0002_ride_requests.up.sql for the schema and the CG-DL-9
// naming rationale.

package store

// requesterIdentitySelect resolves the requester's display identity (MYR-229)
// INLINE, in the SAME statement as the ride row, via correlated subselects on
// the Prisma-owned "User" table keyed on the row's rider_id. Appended to every
// ride-request projection (plain SELECTs, list scans, and every
// UPDATE/INSERT ... RETURNING) so there is never a separate lookup — no
// after-commit window, no extra round trip, no independent outage mode.
//
// "User" is READ-ONLY here (CG-DL-9): these SELECT name/email, never write.
// Both columns are nullable in the Prisma schema, so name/email scan into
// pointers. requester_exists (a boolean EXISTS) distinguishes a deleted rider
// (no row → requesterName OMITTED) from a row that has neither name nor email
// (→ the "Rider" literal). The resolved value is P1 PII — NEVER logged. A
// NULL/absent identity NEVER fails the surrounding ride operation.
const requesterIdentitySelect = `,
	(SELECT u."name" FROM "User" u WHERE u."id" = rider_id) AS requester_name,
	(SELECT u."email" FROM "User" u WHERE u."id" = rider_id) AS requester_email,
	EXISTS (SELECT 1 FROM "User" u WHERE u."id" = rider_id) AS requester_exists`

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
	dispatch_status, dispatched_at, dispatch_error` + requesterIdentitySelect

const queryRideRequestInsert = `INSERT INTO go_ride_requests (
	id, rider_id, owner_id, vehicle_id,
	pickup_lat_enc, pickup_lng_enc, pickup_label, pickup_address,
	dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address,
	status, passenger_name, passenger_phone, scheduled_for
) VALUES (
	$1, $2, $3, $4,
	$5, $6, $7, $8,
	$9, $10, $11, $12,
	$13, $14, $15, $16
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

// Newest first, id as the tie-break — matches the contracts
// RideRequestsListResponse ordering (createdAt DESC, id DESC) and the
// idx_go_ride_requests_rider index.
const queryRideRequestsByRider = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE rider_id = $1
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

// Cursor (keyset) variants for the paginated HTTP surface (MYR-174/175).
// The (created_at, id) row-value comparison resumes strictly after the
// prior page's last row under the same (created_at DESC, id DESC) ordering,
// so pagination is stable across concurrent inserts (contracts
// RideRequestsListResponse.nextCursor). The HTTP layer over-fetches limit+1
// to drive hasMore without a COUNT — mirrors the drives-list cursor.
const queryRideRequestsByRiderCursor = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE rider_id = $1 AND (created_at, id) < ($2, $3)
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

// queryRideRequestUpdateStatus persists a lifecycle transition with its
// timestamp side-effects in one statement: entering 'accepted' stamps
// accepted_at, entering 'completed' stamps completed_at (each only on
// first entry — re-updates never move an already-set stamp), and every
// transition touches updated_at.
const queryRideRequestUpdateStatus = `UPDATE go_ride_requests SET
	status = $2,
	accepted_at = CASE
		WHEN $2 = 'accepted' AND accepted_at IS NULL THEN NOW()
		ELSE accepted_at
	END,
	completed_at = CASE
		WHEN $2 = 'completed' AND completed_at IS NULL THEN NOW()
		ELSE completed_at
	END,
	updated_at = NOW()
WHERE id = $1
RETURNING ` + rideRequestColumns

// queryRideRequestUpdateStatusFrom is the GUARDED transition variant
// (MYR-174/175 check-then-write race fix): identical timestamp-stamping
// semantics to queryRideRequestUpdateStatus, but the row only matches when
// its CURRENT status is in the caller's allowed-from set ($3, text[]).
// Concurrent conflicting transitions therefore serialize in the database —
// exactly one UPDATE matches; the loser affects zero rows and the repo maps
// that to ErrRideRequestConflict (or ErrRideRequestNotFound when the id is
// unknown). This guard is what makes the ride.accepted dispatch event
// exactly-once per accept: only the winning write's caller publishes.
const queryRideRequestUpdateStatusFrom = `UPDATE go_ride_requests SET
	status = $2,
	accepted_at = CASE
		WHEN $2 = 'accepted' AND accepted_at IS NULL THEN NOW()
		ELSE accepted_at
	END,
	completed_at = CASE
		WHEN $2 = 'completed' AND completed_at IS NULL THEN NOW()
		ELSE completed_at
	END,
	updated_at = NOW()
WHERE id = $1 AND status = ANY($3)
RETURNING ` + rideRequestColumns

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
// success/skip. The row is already claimed (dispatched_at set), so this is an
// unconditional update by id.
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
