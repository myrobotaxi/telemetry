package events

// The waypoint seam (MYR-538). Split from ride_events.go, which is at the
// file-size cap, and kept apart from it on merit too: every event in that file
// reports something a PERSON did to a ride — created it, accepted it, cancelled
// it, edited its trip. This one reports something the CAR did, observed from
// telemetry rather than from a request.

// Waypoint labels carried on RideWaypointArrivedEvent. Only the pickup is
// detected in v1; the drop-off is deliberately left to the owner's
// "Dropped off" tap, because ending a ride has consequences (billing, history,
// the Live Activity's dismissal) that a false positive cannot be walked back
// from, whereas an early "your car is here" costs a rider a short wait.
const (
	// WaypointPickup is leg 1's endpoint — the rider's pickup place.
	WaypointPickup = "pickup"
)

// RideWaypointArrivedEvent announces that a vehicle carrying out a ride has
// been OBSERVED to reach one of that ride's waypoints: it is parked inside the
// arrival radius of the place and has stayed there for the dwell window (see
// internal/arrival). It is published exactly once per (ride, waypoint).
//
// INTERNAL-ONLY, and deliberately NOT the lifecycle signal. The ride's status
// move (accepted → arrived) travels on the usual RideStatusChangedEvent, which
// is what the WS broadcaster, the push notifier and the Live Activity already
// consume — auto-arrival changes WHO writes that transition, never its shape.
// This event carries the additional fact that the STATUS alone cannot express:
// "the car itself is here, right now, and we know it from telemetry". Its first
// consumer is MYR-542's light flash (the car greeting its rider at the kerb),
// which must fire only for a physically-present car and must NOT fire for an
// owner's manual tap made from a kitchen table.
//
// SUMMARY-ONLY, like every other internal ride seam: ids and a waypoint label,
// no coordinates. A consumer that needs the place refetches the ride.
// Fire-and-forget and drop-safe — the status transition has already been
// committed by the time this is published, and a publish failure must never
// unwind it.
type RideWaypointArrivedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	// Waypoint is the leg endpoint the car reached — WaypointPickup in v1.
	Waypoint string
}

// EventTopic returns TopicRideWaypointArrived.
func (RideWaypointArrivedEvent) EventTopic() Topic { return TopicRideWaypointArrived }
