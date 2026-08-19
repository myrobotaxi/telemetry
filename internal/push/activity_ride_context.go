// The two value types the send path passes around: WHAT is being pushed to
// (Activity, one registered Live Activity) and WHAT IT SAYS (RideContext, the
// state a content-state is built from).
//
// Split out of activity_notifier.go, which owns the lifecycle consumer and had
// grown past the 300-line cap around them. One type per file for major domain
// types (CLAUDE.md "File Rules"); the notifier keeps the behaviour.

package push

import "time"

// Activity is one running Live Activity, the consumer-site view of a
// go_live_activities row.
type Activity struct {
	RideRequestID string
	UserID        string
	// Token is the ActivityKit update token. P1 — never log in full.
	Token   string
	Sandbox bool
	// Progress is this Activity's leg-progress anchor as last DELIVERED to the
	// phone (MYR-398). Per-Activity rather than per-ride because the
	// monotonicity rule it enforces is a promise made to one client about the
	// sequence of values that client has seen.
	Progress ProgressAnchor
	// AlertedPhase is the highest phase this Activity's island has already been
	// expanded for (MYR-398, migration 0029). Per-Activity for the same reason
	// Progress is: it records what one client has been shown, and the "at most
	// once" promise is made to that client. See activity_alert.go.
	AlertedPhase AlertPhase
}

// RideContext is everything a content-state needs that is not the token.
type RideContext struct {
	// Status is the ride's lifecycle status.
	Status string
	// VehicleName is the car's nickname, "" when it has none.
	VehicleName string
	// Destination is the dropoff's short label — the trip's END. P1.
	//
	// It is no longer necessarily what the card says (MYR-587): on a multi-stop
	// trip mid-journey the wire `destination` names the stop the car is driving
	// to instead. See legDestination.
	Destination string
	// Stops is the trip's intermediate stops in travel order, empty on a
	// two-endpoint ride. Server-owned statuses; P1 labels. See legDestination.
	Stops []TripStop
	// ETAMinutes is the car's carried navigation ETA in whole minutes, nil when
	// the car has no active route.
	ETAMinutes *int
	// TripMilesRemaining is the car's carried remaining distance on that route,
	// in miles (Tesla's tripDistanceRemaining), nil when it reports none. The
	// preferred progress input — see activity_progress.go.
	TripMilesRemaining *float64
	// NavUpdatedAt is when the car last reported NAVIGATION — the age of the two
	// readings above, not of the row that holds them (MYR-409). Nil when no
	// navigation frame has ever been seen for this car, which every gate reads
	// as not fresh. See navFresh / readingStalled; `eta` keeps its existing
	// ungated behaviour.
	NavUpdatedAt *time.Time
	// PickupDispatchedAt is when the server pushed the RIDER'S PICKUP to the car
	// as a navigation destination — go_ride_requests.dispatched_at, nil when
	// leg 1 has not been dispatched (a sleeping reservation, or a dispatch that
	// failed or was skipped).
	//
	// It is the second half of MYR-409, and the half a per-reading stamp cannot
	// supply. Freshness alone answers "is this reading recent"; it cannot answer
	// "is this reading ABOUT US". A car running its owner's errand at the moment
	// an instant ride is accepted is streaming genuinely fresh navigation — for
	// a route to the shops. Only the dispatch instant separates a reading that
	// describes the rider's pickup from one that describes whatever the car was
	// already doing. Used by arrivingSoon; see there for the residual bound.
	PickupDispatchedAt *time.Time
	// DispatchUnderway is MYR-376's reservation-dormancy predicate as evaluated
	// by the store: false while a reservation sleeps between accept and the
	// earlier of its dispatch and its due instant. It gates the PICKUP leg's
	// track — see legUnderway for what happens without it.
	DispatchUnderway bool
}
