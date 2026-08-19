package events

import "time"

// VehicleTelemetryWarningEvent is published by the owner-inactivity sweeper on
// the FOURTH day of an owner's silence, one day before their vehicle's
// fleet-telemetry config is removed (MYR-592, rest-api.md §7.27).
//
// It exists so the sweeper never imports internal/push, the same separation
// internal/dispatch keeps: the worker publishes what happened, the notifier
// decides what to say about it. This is the ONLY event MYR-592 publishes —
// there is deliberately no suspension-moment event, because the client asked
// for the in-app notice plus this one warning and nothing else. A push at the
// moment of suspension would arrive to tell somebody who has not opened the app
// in five days about a thing they were already warned about yesterday.
//
// PUBLISHED ONCE PER EPISODE, guarded by go_vehicle_telemetry_suspensions.
// warned_at rather than by anything here: the bus is fire-and-forget and cannot
// be the memory. If the publish fails the warning is simply lost for that
// episode — the suspension still happens on schedule, with the in-app notice as
// the backstop, which is the tolerable failure. Sending it twice is not.
//
// NOT A RIDE EVENT. It carries no ride id, and the notifier's fan-out therefore
// emits no `apns-collapse-id` (collapseID returns "" on an empty ride, which
// apns_collapse.go documents as the correct answer for exactly this case: a
// notification about a subject other than a ride must not merge with unrelated
// news on somebody's lock screen).
type VehicleTelemetryWarningEvent struct {
	BasePayload

	// OwnerID is the only recipient. Riders and viewers of a shared car hear
	// nothing: suspension follows the OWNER's inactivity and only the owner can
	// prevent it, so telling a rider would be an alarm about somebody else's
	// decision that they cannot act on.
	OwnerID string

	// VehicleID is the car. Present for the log line and for a future consumer;
	// the push copy does not use it.
	VehicleID string

	// VehicleName is the owner's own nickname for the car. May be EMPTY — a
	// car need not be named — and the copy layer supplies the "Your car"
	// fallback rather than this producer inventing one.
	VehicleName string

	// SuspendAt is when streaming will stop if nothing changes. Carried so the
	// log can state the deadline; the COPY deliberately says "tomorrow" rather
	// than a timestamp, because a lock-screen banner is not the place for an
	// instant the owner would then be tempted to count down from.
	SuspendAt time.Time
}

// EventTopic returns TopicVehicleTelemetryWarning.
func (VehicleTelemetryWarningEvent) EventTopic() Topic { return TopicVehicleTelemetryWarning }
