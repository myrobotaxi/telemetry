package events

// The waypoint seam (MYR-538). Split from ride_events.go, which is at the
// file-size cap, and kept apart from it on merit too: every event in that file
// reports something a PERSON did to a ride — created it, accepted it, cancelled
// it, edited its trip. This one reports something the CAR did, observed from
// telemetry rather than from a request.

// Waypoint labels carried on RideWaypointArrivedEvent. Each names WHICH of a
// ride's places the car was observed to reach.
//
// The drop-off is deliberately still not a lifecycle event: reaching the
// destination publishes WaypointDestination, and NOTHING completes the ride —
// that stays the owner's "Dropped off" tap, because ending a ride has
// consequences (billing, history, the Live Activity's dismissal) a false
// positive cannot be walked back from, whereas an early "your car is here"
// costs a rider a short wait. An intermediate STOP is the opposite case: its
// advance is reversible in practice (the next leg is re-shared, not billed) and
// nothing at all happens without it, so it IS acted on.
const (
	// WaypointPickup is leg 1's endpoint — the rider's pickup place.
	WaypointPickup = "pickup"
	// WaypointDestination is the trip's final drop-off (MYR-539). Published for
	// consumers that want the moment (MYR-542's flash); it advances nothing.
	WaypointDestination = "destination"
	// waypointStopPrefix prefixes an intermediate stop's label, which carries
	// the stop id: "stop:crs0123…". The id is on the label rather than in a
	// second field because the waypoint IS the identity of the place reached,
	// and a consumer that does not know about stops treats every stop label as
	// one unrecognized value rather than as a field it must learn.
	waypointStopPrefix = "stop:"
)

// WaypointStop builds the label for an intermediate stop.
func WaypointStop(stopID string) string { return waypointStopPrefix + stopID }

// StopIDFromWaypoint returns the stop id a waypoint label carries, and whether
// the label named a stop at all. The pickup and the destination answer false —
// they are the ride's own endpoints, not members of the stop list.
func StopIDFromWaypoint(waypoint string) (string, bool) {
	if len(waypoint) <= len(waypointStopPrefix) || waypoint[:len(waypointStopPrefix)] != waypointStopPrefix {
		return "", false
	}
	return waypoint[len(waypointStopPrefix):], true
}

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
	// Waypoint is the leg endpoint the car reached: WaypointPickup,
	// WaypointStop(id) for an intermediate stop, or WaypointDestination
	// (MYR-539). Consumers MUST tolerate a label they do not recognize.
	Waypoint string
}

// EventTopic returns TopicRideWaypointArrived.
func (RideWaypointArrivedEvent) EventTopic() Topic { return TopicRideWaypointArrived }
