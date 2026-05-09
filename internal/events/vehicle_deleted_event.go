package events

// VehicleDeletedEvent is published when a Vehicle row is deleted from
// the Prisma-owned "Vehicle" table. It is the Go-side wakeup signal that
// drives the §3.5 cleanup contract in docs/contracts/data-lifecycle.md:
// the WS hub closes any client subscribed to VehicleID with code 4002,
// the telemetry receiver closes any active inbound mTLS stream for VIN,
// and the auth layer invalidates any cached user-existence record for
// UserID.
//
// Source: a Postgres LISTEN/NOTIFY channel populated by an AFTER DELETE
// trigger on the Vehicle table. The trigger is owned by the Next.js
// repo (prisma/migrations/.../vehicle_deleted_notify_trigger). The
// telemetry server holds a dedicated long-lived pgx connection on the
// channel via internal/store/notify_listener.go.
//
// Fields are populated from the trigger payload {vehicleId, userId, vin}.
// VIN may be empty for early-setup vehicles that were deleted before
// pairing completed; consumers MUST tolerate the empty VIN (skip the
// inbound-mTLS-close path in that case — there is no stream to close).
type VehicleDeletedEvent struct {
	BasePayload
	VehicleID string
	UserID    string
	VIN       string // may be empty when the deleted vehicle never paired
}

// EventTopic returns TopicVehicleDeleted.
func (VehicleDeletedEvent) EventTopic() Topic { return TopicVehicleDeleted }
