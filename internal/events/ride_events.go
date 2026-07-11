package events

import "time"

// Ride-hailing domain events (P10, MYR-174/175). Published by the
// ride-request HTTP handlers (internal/telemetry) and consumed by the WS
// broadcaster (summary frames to the two parties) and — for accept — the
// dispatch pipeline (MYR-176).
//
// The two summary events (created / status-changed) deliberately carry
// only ids + status + the two party user ids, matching the summary-only
// WS payloads in schemas/ws-messages.schema.json: pickup/dropoff labels
// and passenger PII are P1 and never travel on the broadcast path (clients
// refetch REST detail). The dispatch event (RideAcceptedEvent) is the one
// exception — it carries the pickup/dropoff places because MYR-176 needs
// them to build the Tesla navigation_request; it is internal-only and
// never broadcast.

// RidePlace is a pickup or drop-off point carried on RideAcceptedEvent for
// the dispatch pipeline. Coordinates are P1 GPS data — internal-only, never
// logged.
type RidePlace struct {
	Latitude  float64
	Longitude float64
	Label     string
	Address   string // empty when the place carried label only
}

// RideRequestCreatedEvent announces a newly created ride request. Delivered
// by the WS broadcaster to the rider and owner connections as a summary
// `ride_request_created` frame. ScheduledFor is nil for an on-demand ("Now")
// ride.
type RideRequestCreatedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Status        string
	// RequesterName is the requester's resolved display name (MYR-229), so
	// the owner's client can label the incoming request without a detail
	// fetch. Nil when the rider has no identity row (the frame omits it) —
	// carried as a pointer, consistent with the adjacent optional fields.
	// P1 PII — never logged.
	RequesterName *string
	ScheduledFor  *time.Time
	CreatedAt     time.Time
}

// EventTopic returns TopicRideRequestCreated.
func (RideRequestCreatedEvent) EventTopic() Topic { return TopicRideRequestCreated }

// RideStatusChangedEvent announces a mutation of an existing ride request
// (main-lifecycle transition or reschedule sub-state change). Delivered by
// the WS broadcaster to the rider and owner connections as a summary
// `ride_status_changed` frame. RescheduleStatus is nil when the ride has no
// reschedule history.
type RideStatusChangedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Status        string
	// RequesterName is the requester's resolved display name (MYR-229),
	// carried so the owner's client keeps a stable requester label across
	// lifecycle transitions. Nil when the rider has no identity row (the
	// frame omits it) — carried as a pointer, consistent with the adjacent
	// optional fields. P1 PII — never logged.
	RequesterName    *string
	RescheduleStatus *string
	UpdatedAt        time.Time
}

// EventTopic returns TopicRideStatusChanged.
func (RideStatusChangedEvent) EventTopic() Topic { return TopicRideStatusChanged }

// RideAcceptedEvent is the dispatch seam: published once when an owner
// accepts a request, carrying everything MYR-176 needs to push a Tesla
// navigation_request (pickup/dropoff places + booked-for passenger). No
// subscriber exists until MYR-176; the bus tolerates zero subscribers.
// Internal-only — never broadcast to WS clients.
type RideAcceptedEvent struct {
	BasePayload
	RideRequestID  string
	VehicleID      string
	RiderID        string
	OwnerID        string
	Pickup         RidePlace
	Dropoff        RidePlace
	PassengerName  string // empty when the rider is riding themselves
	PassengerPhone string
	ScheduledFor   *time.Time
	AcceptedAt     time.Time
}

// EventTopic returns TopicRideAccepted.
func (RideAcceptedEvent) EventTopic() Topic { return TopicRideAccepted }
