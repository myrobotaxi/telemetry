// SQL for the Go-owned go_ride_requests table (MYR-173). Unlike the
// Prisma-owned tables, columns are snake_case and unquoted — see
// migrations/0002_ride_requests.up.sql for the schema and the CG-DL-9
// naming rationale.

package store

// rideRequestColumns is every column read into RideRequestRecord, in scan
// order. Coordinates travel as *_enc ciphertext; the repo decrypts them
// into RidePlace.Latitude/Longitude after scanning.
const rideRequestColumns = `id, rider_id, owner_id, vehicle_id,
	pickup_lat_enc, pickup_lng_enc, pickup_label, pickup_address,
	dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address,
	status, passenger_name, passenger_phone,
	scheduled_for, reschedule_proposed_for, reschedule_status,
	accepted_at, completed_at, created_at, updated_at`

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
RETURNING created_at, updated_at`

const queryRideRequestByID = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE id = $1`

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
