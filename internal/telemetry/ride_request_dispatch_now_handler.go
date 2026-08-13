// POST /api/ride-requests/{id}/dispatch-now — START EARLY (MYR-556).
//
// A reservation's car leaves at a computed leave-time (MYR-535): `scheduledFor`
// minus the estimated drive to the pickup plus a buffer, clamped to
// [5 min, 30 min]. That is the right default and the wrong one for the case
// this endpoint serves — the owner is ready, the rider is ready, and everybody
// is standing about waiting for a clock. This is the manual override, and it is
// the OWNER's alone: the car is theirs, it is a physical object in the street,
// and moving it is not a decision a passenger takes.
//
// It changes no status, adds no lifecycle state and mints no new response type.
// The answer is the ride, refreshed — the same RideRequest every other verb on
// this resource returns — because the only thing that changed is the dispatch
// columns the projection already carries.

package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// DispatchNowRide is the reservation this handler asks to be dispatched. It is
// the handler's OWN shape, not internal/dispatch's: this package does not
// import that one, and the cmd-side adapter is what joins them (the
// consumer-site interface rule, exactly as RideActivityEnder and
// RideMemberReader are wired).
type DispatchNowRide struct {
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	// Pickup is where the car is sent. P1 coordinates — never logged.
	Pickup RidePlaceData
	// ScheduledFor is the reservation instant the ride is being dispatched
	// AHEAD of. Present by construction: the handler refuses an on-demand ride
	// before it ever builds one of these.
	ScheduledFor time.Time
	// TripVersion rides along so the MYR-555 dispatch frame states the ride's
	// CURRENT version rather than the wire's zero.
	TripVersion int
}

// DispatchNowOutcome is the coarse answer the dispatch path gives back. It is
// deliberately not the nav push's own outcome: whether Tesla accepted the share
// is recorded on the ride row and travels back in `dispatchStatus`, so the
// endpoint answers the refreshed record instead of re-interpreting it.
type DispatchNowOutcome int

const (
	// DispatchNowDispatched: the claim was won and the push pipeline ran.
	DispatchNowDispatched DispatchNowOutcome = iota
	// DispatchNowVehicleBusy: the car is mid-ride on another, INSTANT ride.
	DispatchNowVehicleBusy
	// DispatchNowAlreadyDispatched: the leg-1 latch was already held — the
	// sweeper reached the reservation first, or this is a repeated tap.
	DispatchNowAlreadyDispatched
)

// ReservationDispatcher runs the reservation sweeper's CLAIMED DISPATCH PATH on
// demand. Defined at the consumer site and satisfied by a cmd-side adapter over
// *dispatch.ReservationSweeper, so the HTTP layer never learns what a nav
// sequencer is.
//
// The seam is deliberately narrow — one verb, one coarse outcome — because
// everything behind it is the sweeper's, unchanged and unforked. The refusals
// a human needs a sentence for are decided HERE, against the row this handler
// already loaded; the one refusal that cannot be (is the car busy right now?)
// is the one the dispatcher answers.
type ReservationDispatcher interface {
	DispatchNow(ctx context.Context, ride DispatchNowRide) (DispatchNowOutcome, error)
}

// WithReservationDispatcher wires the dispatch-now seam (MYR-556). Not wiring
// it leaves the endpoint answering 500 — a deployment error, not a runtime
// state, exactly the reading `activities` and `bookedWindowsMax` get. It is
// deliberately NOT a fail-open: there is no safe "pretend it worked" for a
// request whose whole point is to move a car.
func WithReservationDispatcher(d ReservationDispatcher) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.dispatchNow = d
	}
}

// ServeDispatchNow handles POST /api/ride-requests/{id}/dispatch-now.
//
// THE LADDER, IN ORDER, AND WHY EACH RUNG IS THE CODE IT IS:
//
//	404 — a stranger. loadForParty answers it, so the endpoint is not an
//	      existence oracle for a ride the caller has no relation to.
//	403 — the RIDER (and a joined group member). They are parties and can see
//	      this ride perfectly well in their own app, so a 404 would read as a
//	      bug; they simply do not get to move somebody else's car. This is
//	      ServeCancel's reasoning applied in the other direction, and it is the
//	      same shape ServeStart uses to keep the owner out of the rider's verb.
//	409 — the ride is not an undispatched, accepted reservation. Three distinct
//	      sentences under one code, because a client cannot act differently on
//	      them but a person can.
//	200 — the refreshed ride.
//
// IDEMPOTENCE NEEDS NO RATE LIMIT AND HAS NONE. The claim is the guard: the
// second tap finds the latch held and gets a 409, because the row itself now
// says the car has been sent. There is nothing a repeated call can spend — no
// second push, no second event, no second APNs delivery — so a budget here
// would only be a second thing to get wrong.
func (h *RideRequestHandler) ServeDispatchNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return
	}
	if userID != rec.OwnerID {
		// The rider is a party; a joined member is a party since MYR-540.
		// Neither owns the car. Non-parties were already 404'd above.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"only the vehicle's owner can send the car early")
		return
	}
	if !h.dispatchNowAllowed(w, rec) {
		return
	}

	if h.dispatchNow == nil {
		h.logger.Error("ride-request: dispatch-now requested but no dispatcher is wired",
			slog.String("ride_request_id", rec.ID),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	outcome, err := h.dispatchNow.DispatchNow(ctx, DispatchNowRide{
		RideRequestID: rec.ID,
		VehicleID:     rec.VehicleID,
		RiderID:       rec.RiderID,
		OwnerID:       rec.OwnerID,
		Pickup:        rec.Pickup,
		ScheduledFor:  *rec.ScheduledFor,
		TripVersion:   rec.TripVersion,
	})
	if err != nil {
		// "We could not tell" — an unreadable busy state, or a claim that would
		// not take. Both are honest 500s: guessing a refusal would tell the
		// owner their car is busy when nobody knows whether it is.
		h.logger.Error("ride-request: dispatch-now failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	switch outcome {
	case DispatchNowVehicleBusy:
		// SPOKEN, not blamed on the ride's status. The reservation is perfectly
		// valid and will dispatch on its own the moment the car is free — the
		// sweeper's hold IS a bounded retry — so the sentence says what is true
		// and what happens next. Same code class as the accept gate's
		// one-active-ride refusal: a capability gate on the VEHICLE, not an
		// illegal transition.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable,
			"the car is on another ride right now — this one will be sent as soon as it is free")
	case DispatchNowAlreadyDispatched:
		// The sweeper reached its leave-time in the seconds since the row was
		// read, or this is the second tap. Nothing is owed either way.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"the car has already been sent for this ride")
	case DispatchNowDispatched:
		h.writeJSON(w, http.StatusOK, h.rideWire(h.refreshedRide(ctx, rec)))
	}
}

// dispatchNowAllowed is the 409 ladder. Three refusals, one code, three
// sentences — the code is what a client branches on and the sentence is what a
// person reads, and only the sentence can distinguish "not yet accepted" from
// "not a reservation at all".
//
// It is a PRE-CHECK, and it says so: the authority on already-dispatched is the
// claim inside the dispatch path, which re-validates in SQL. This rung exists so
// the ordinary repeated tap gets its sentence without spending a busy probe, not
// because it decides anything under concurrency — a row that becomes dispatched
// between here and the claim comes back through DispatchNowAlreadyDispatched
// and lands on the very same 409.
func (h *RideRequestHandler) dispatchNowAllowed(w http.ResponseWriter, rec RideRequestData) bool {
	if rec.Status != rideStatusAccepted {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"a ride can only be sent early once it is accepted, and this one is "+rec.Status)
		return false
	}
	if rec.ScheduledFor == nil {
		// An on-demand ride's car was dispatched the moment the owner accepted
		// it. There is no clock to jump.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this ride is not scheduled — its car was sent when you accepted it")
		return false
	}
	if rec.DispatchedAt != nil {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"the car has already been sent for this ride")
		return false
	}
	return true
}

// refreshedRide re-reads the ride so the response carries the dispatch columns
// the call just wrote — `dispatchedAt` and `dispatchStatus`, which are the whole
// observable result of this endpoint.
//
// A FAILED RE-READ ANSWERS THE PRE-DISPATCH RECORD RATHER THAN A 500, and the
// direction is deliberate. The dispatch COMMITTED: the latch is stamped, the car
// has its route, and a 500 would tell the owner it failed — after which their
// retry gets the already-dispatched 409 and they have no idea which of the two
// answers was true. The stale record is merely incomplete, and it is corrected
// within the same breath by MYR-555's dispatch frame, which has already gone out
// on the WebSocket and asks this client to refetch anyway. The log line is where
// the failure is not swallowed.
func (h *RideRequestHandler) refreshedRide(ctx context.Context, rec RideRequestData) RideRequestData {
	refreshed, err := h.store.GetByID(ctx, rec.ID)
	if err != nil {
		h.logger.Error("ride-request: re-reading a dispatched ride failed; answering the pre-dispatch record",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		return rec
	}
	return refreshed
}
