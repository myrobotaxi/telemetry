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
	// ScheduledFor is the ride's reservation instant, nil for an on-demand
	// ("Now") ride — the same optional carried on RideRequestCreatedEvent, and
	// carried here for the same reason: the rider's push copy must be able to
	// tell a RESERVATION from an instant request (MYR-360). It is NOT projected
	// onto the `ride_status_changed` WS frame, which stays summary-only.
	ScheduledFor *time.Time
	// TripVersion is the ride's trip-shape version after this event (MYR-541),
	// projected onto the WS frame so a client holding a lower version
	// refetches the record. 0 for pre-MYR-541 publishers and never-edited rows.
	TripVersion int
	// TripEdit marks an event published for a TRIP-SHAPE change rather than a
	// lifecycle transition (MYR-541): Status is the ride's UNCHANGED current
	// status, carried so the WS frame keeps its required field, and the push
	// notifier must NOT re-fire the status copy for it — the trip-changed
	// seam carries its own. False for every pre-MYR-541 publisher.
	TripEdit bool
	// PreviousStatus is the status the ride held when the transition was
	// requested — the handler's pre-check read, so under a lost race it may
	// name a status one step behind the one the guarded write actually left.
	// Carried for the push notifier's audience fork (MYR-537: a rider cancel
	// wakes the OWNER only once the owner had committed — accepted or later —
	// and a requested-cancel stays silent as it always was). Best-effort by
	// that construction; NOT projected onto the WS frame. Empty on events
	// published before MYR-537, which reads as the silent arm.
	PreviousStatus string
	// CancelledBy names the party that initiated a cancellation — "rider" |
	// "owner" (MYR-522), read off the updated row so it is nil on every other
	// transition. Carried so the rider's push copy can tell an OWNER's cancel
	// (worth waking a phone for) from the rider's own (noise). Not projected
	// onto the WS frame, which stays summary-only — the detail read carries it.
	CancelledBy *string
	UpdatedAt   time.Time
}

// EventTopic returns TopicRideStatusChanged.
func (RideStatusChangedEvent) EventTopic() Topic { return TopicRideStatusChanged }

// RideAcceptedEvent is the dispatch seam: published once when an owner
// accepts a request, carrying everything MYR-176 needs to push a Tesla
// navigation_request (pickup/dropoff places). Internal-only — never broadcast
// to WS clients.
//
// IT NO LONGER CARRIES THE BOOKED-FOR PASSENGER (MYR-447). PassengerName and
// PassengerPhone were added here in MYR-173 for a nav-dispatch consumer that
// was never written: the fields were populated on every accept and read by
// NOTHING — not internal/dispatch, not internal/push, not internal/ws. That
// made this struct the one place the internal event bus carried P1 PII with no
// purpose, so every publish put a passenger's name and phone number into an
// in-process fan-out for no reader to use. The feature that produced the data
// was itself removed from the app in MYR-382. Removing the fields is the whole
// fix; there is no replacement seam, because there was never a consumer.
type RideAcceptedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Pickup        RidePlace
	Dropoff       RidePlace
	ScheduledFor  *time.Time
	AcceptedAt    time.Time
}

// EventTopic returns TopicRideAccepted.
func (RideAcceptedEvent) EventTopic() Topic { return TopicRideAccepted }

// RideStartedEvent is the leg-2 dispatch seam (MYR-270, was RideBoardedEvent in
// MYR-265): published once when the RIDER starts the ride — the guarded
// arrived→enroute transition (POST /api/ride-requests/{id}/start). It carries
// the DROPOFF place the nav-dispatch pipeline pushes as the car's new Tesla
// navigation destination for leg 2 (car en route to dropoff). Coordinates are
// P1 GPS data — internal-only, never logged. Like RideAcceptedEvent it never
// reaches the WS broadcast path; the client-visible start signal is the summary
// `ride_status_changed` (status `enroute`).
type RideStartedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	OwnerID       string
	Dropoff       RidePlace
}

// EventTopic returns TopicRideStarted.
func (RideStartedEvent) EventTopic() Topic { return TopicRideStarted }

// RideDueEvent is the reservation-due seam (MYR-179): published once when a
// SCHEDULED ride reaches its ScheduledFor instant and its pickup nav push has
// been DELIVERED to the car (outcome `sent`). Deliberately SUMMARY-ONLY (ids + the two instants): unlike
// RideAcceptedEvent it carries no places and no passenger contact, because its
// intended consumer is a rider push notification, not a Tesla command — a
// future consumer that needs the pickup refetches the ride.
//
// Consumed by the push notifier, which fans out to BOTH parties (MYR-186 the
// rider, MYR-535 the owner). The publish is fire-and-forget and drop-safe.
// Internal-only — never broadcast to WS clients.
type RideDueEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	// ScheduledFor is the reservation instant that came due.
	ScheduledFor time.Time
	// DueAt is the sweeper-clock instant the reservation was picked up for
	// dispatch (read in the worker, just before the claim — the push itself
	// then takes up to the dispatcher's OverallTimeout). Since MYR-535 it
	// ordinarily PRECEDES ScheduledFor by the computed dispatch lead
	// (ScheduledFor is the pickup ARRIVAL instant; the car is sent out early
	// enough to make it), and it is bounded above by ScheduledFor + the
	// lateness ceiling: past that the reservation is failed instead and no
	// RideDueEvent is ever published. So a consumer can tell an on-lead
	// dispatch from a held-then-released one without re-reading the row.
	DueAt time.Time
}

// EventTopic returns TopicRideDue.
func (RideDueEvent) EventTopic() Topic { return TopicRideDue }

// RideNavUnappliedEvent is the nav-share close-loop's failure report
// (MYR-527): a leg's `navigation_request` was accepted by Tesla (`sent`) but
// the car's own telemetry never reported the shared destination — through the
// verification window and one re-share. First live casualty 2026-08-12: a
// rider aboard a 3-hour trip whose dash still navigated to the pickup, with
// the app faithfully rendering the wrong nav ("4 min away").
//
// SUMMARY-ONLY like RideDueEvent: ids and a leg label, no places — the
// consumer is the owner's "check the dash" push, and the payload policy keeps
// P1 places off that path entirely. Internal-only — never broadcast to WS
// clients. Fire-and-forget and drop-safe: a bus failure is logged and the
// dispatch outcome stands.
type RideNavUnappliedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	OwnerID       string
	// Leg is "pickup" or "dropoff" — the audit label of the leg that never
	// took, for the log line and any future per-leg copy fork.
	Leg string
}

// EventTopic returns TopicRideNavUnapplied.
func (RideNavUnappliedEvent) EventTopic() Topic { return TopicRideNavUnapplied }

// RideTripChangedEvent is the trip-edit seam (MYR-541): published once per
// accepted PATCH of the ride's pickup/drop-off, alongside a TripEdit-marked
// RideStatusChangedEvent (which carries the WS refetch signal). Consumers:
// the DISPATCHER (re-shares the car's nav when the edited endpoint is the
// current leg's target, then verifies per MYR-527) and the PUSH NOTIFIER
// (tells the other participants). Carries the edited PLACES because the
// dispatcher needs the coordinate to share — exactly as RideAcceptedEvent
// does — and is internal-only, never broadcast to WS clients.
type RideTripChangedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	// EditorUserID is who made the edit; the notifier excludes them from the
	// fan-out and forks the copy on which party they are.
	EditorUserID string
	// Status is the ride's (unchanged) lifecycle status at edit time — what
	// the dispatcher's re-share decision reads.
	Status string
	// PickupDispatched: leg-1 resolved `sent` (the pickup nav is on the car),
	// so an edited pickup must be re-shared.
	PickupDispatched bool
	// NewPickup / NewDropoff are the edited endpoints; nil = not edited.
	// P1 coordinates — never logged, never pushed, never on a WS frame.
	NewPickup  *RidePlace
	NewDropoff *RidePlace
	// RequesterName, as on RideStatusChangedEvent (P1, never logged).
	RequesterName *string
	UpdatedAt     time.Time
}

// EventTopic returns TopicRideTripChanged.
func (RideTripChangedEvent) EventTopic() Topic { return TopicRideTripChanged }
