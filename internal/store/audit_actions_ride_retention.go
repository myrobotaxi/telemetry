// Audit enum values for the ride retention sweep (MYR-447).
//
// They live in their own file rather than in audit_repo.go for one reason: that
// file carries the cross-repo coupling contract with the Next.js app's Prisma
// AuditLog model and is owned by a separate workstream. Adding an action and a
// targetType to the enums is additive — it changes no column, no nullability,
// no classification tier — so it does not touch the CG-DL-8 parity surface and
// does not need to sit inside the coupled file. Read the note at the top of
// audit_repo.go before changing anything about the TABLE.
//
// Both actions below are still governed by everything that file states:
// insert-only, append-only, and P0-only metadata (CG-DL-5).

package store

const (
	// AuditActionRidesPruned records a batch of terminal ride requests deleted
	// by the MYR-447 retention sweep. Written INSIDE the same transaction as
	// the DELETE and BEFORE it (CG-DL-3), so a failure to record the deletion
	// prevents the deletion rather than hiding it.
	AuditActionRidesPruned AuditAction = "rides_pruned"

	// AuditActionRidePassengersScrubbed records a batch of surviving terminal
	// rides whose deprecated booked-for-passenger columns were NULLed at
	// terminal + 30 days. A separate action from rides_pruned because it is a
	// separate promise: one says a record was destroyed, the other says a
	// record survives with two fields removed, and collapsing them would make
	// the log unable to answer which happened.
	AuditActionRidePassengersScrubbed AuditAction = "ride_passengers_scrubbed"
)

const (
	// auditTargetTypeRideRequest marks ride-request records as the affected
	// entity — used by the rides_pruned and ride_passengers_scrubbed rows
	// (MYR-447).
	//
	// The targetId paired with it is the VEHICLE id, not a ride id, exactly as
	// auditTargetTypeDrive's is: the row records a BATCH of rides grouped by the
	// car they were booked against, and for rides_pruned the ride ids it covers
	// no longer exist by the time anyone reads it.
	auditTargetTypeRideRequest = "ride_request"
)
