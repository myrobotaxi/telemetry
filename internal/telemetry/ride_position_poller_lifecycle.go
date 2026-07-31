package telemetry

// MYR-394 — the ride seams that open and close a poller, and the registry
// operations behind them.
//
// WHY THREE SEAMS AND NOT ONE. `ride.status.changed` fires on every guarded
// lifecycle write and could in principle drive all of this on its own. It
// cannot, for one reason: a RESERVATION that comes due does not change status.
// The sweeper dispatches its pickup nav and the row stays `accepted`, so the
// only announcement that the car is now genuinely under way is `ride.due`.
// Each seam therefore owns exactly one transition and no two overlap:
//
//	ride.accepted        → an INSTANT ride was just accepted; leg 1 begins now.
//	ride.due             → a RESERVATION's pickup nav was delivered; leg 1 begins now.
//	ride.status.changed  → arrived / enroute (adopt), terminal (stop).
//
// `accepted` is handled ONLY on ride.accepted even though status.changed also
// carries it, so there is one owner per transition rather than two racing to be
// idempotent.

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// ride lifecycle statuses this component reacts to. They mirror
// store.RideRequestStatus, restated as local consts because internal/telemetry
// must not import internal/store.
const (
	ridePollStatusAccepted  = "accepted"
	ridePollStatusEnroute   = "enroute"
	ridePollStatusArrived   = "arrived"
	ridePollStatusCompleted = "completed"
	ridePollStatusDeclined  = "declined"
	ridePollStatusCancelled = "cancelled"
)

// handleAccepted starts a poller for an INSTANT ride the owner just accepted.
//
// A SCHEDULED ride is deliberately ignored here: `accepted` on a reservation
// means "booked", possibly for three days' time, and polling a car that is
// parked in a driveway with nobody watching is precisely the Fleet API waste
// this feature must not introduce. Its moment arrives on ride.due.
func (p *RidePositionPoller) handleAccepted(event events.Event) {
	evt, ok := event.Payload.(events.RideAcceptedEvent)
	if !ok {
		return
	}
	if evt.ScheduledFor != nil {
		p.logger.Debug("ride position poll: reservation accepted, not yet live",
			slog.String("ride_request_id", evt.RideRequestID))
		return
	}
	p.startPoll(evt.VehicleID, evt.RideRequestID, "ride.accepted")
}

// handleDue starts a poller for a reservation whose pickup nav has just been
// DELIVERED to the car (the sweeper publishes ride.due only on outcome `sent`).
// The row is still `accepted`, but the car is now driving to the pickup.
func (p *RidePositionPoller) handleDue(event events.Event) {
	evt, ok := event.Payload.(events.RideDueEvent)
	if !ok {
		return
	}
	p.startPoll(evt.VehicleID, evt.RideRequestID, "ride.due")
}

// handleStatusChanged adopts a ride that reached arrived/enroute and stops the
// poller on any terminal transition.
//
// The stop is guarded on the ride IDENTITY, not just the vehicle: a late or
// duplicated terminal event for a ride that already ended must not tear down
// the poller belonging to the NEXT ride on the same car. That window is
// narrow but real — the accept guard permits a new instant ride the moment the
// previous one goes terminal.
func (p *RidePositionPoller) handleStatusChanged(event events.Event) {
	evt, ok := event.Payload.(events.RideStatusChangedEvent)
	if !ok {
		return
	}
	switch evt.Status {
	case ridePollStatusArrived, ridePollStatusEnroute:
		// Adoption, not a duplicate start: the instant case is already running
		// from ride.accepted and startPoll is a no-op for it. What this
		// genuinely catches is a reservation that reached leg 2, and any ride
		// whose opening event this process never saw.
		p.startPoll(evt.VehicleID, evt.RideRequestID, "ride."+evt.Status)
	case ridePollStatusCompleted, ridePollStatusDeclined, ridePollStatusCancelled:
		p.stopPoll(evt.VehicleID, evt.RideRequestID, "ride."+evt.Status)
	case ridePollStatusAccepted:
		// Owned by ride.accepted / ride.due. Ignored here on purpose.
	}
}

// startPoll registers and launches a poller for one vehicle, unless one is
// already running for it, the component is stopping, or the cap is reached.
//
// Idempotent by VEHICLE. If a poller is already running for the car under a
// DIFFERENT ride, the existing one is kept: it is polling the same VIN on the
// same cadence, so replacing it would buy nothing and would open a window in
// which the car is polled twice or not at all.
func (p *RidePositionPoller) startPoll(vehicleID, rideRequestID, reason string) {
	if vehicleID == "" {
		return
	}

	p.mu.Lock()
	switch {
	case p.stopped || p.ctx == nil:
		// Shutting down, or the kill-switch left Start a no-op. Refusing here
		// is what stops a bus handler from spawning a goroutine into a context
		// that is already cancelled (the MYR-176 dying-context trap).
		p.mu.Unlock()
		return
	case p.active[vehicleID] != nil:
		p.mu.Unlock()
		return
	case len(p.active) >= p.cfg.MaxActive:
		p.mu.Unlock()
		p.logger.Warn("ride position poll: at capacity, target deferred to reconcile",
			slog.Int("max_active", p.cfg.MaxActive),
			slog.String("ride_request_id", rideRequestID))
		return
	}

	ctx, cancel := context.WithCancel(p.ctx)
	handle := &activePoll{rideRequestID: rideRequestID, cancel: cancel}
	p.active[vehicleID] = handle
	p.wg.Add(1)
	p.mu.Unlock()

	go p.run(ctx, vehicleID, handle)

	p.logger.Info("ride position poll started",
		slog.String("ride_request_id", rideRequestID),
		slog.String("reason", reason),
		slog.Duration("interval", p.cfg.Interval))
}

// stopPoll cancels the poller for a vehicle, but only if it belongs to the ride
// named. An empty rideRequestID matches unconditionally — that is the
// reconcile's orphan-reaping path, where the ride is by definition gone.
//
// It does NOT wait for the goroutine: this runs on a bus handler, and the bus
// delivers serially per subscription, so blocking here would stall every later
// ride event behind one in-flight Tesla request (head-of-line loss). The
// goroutine removes its own registry entry as it exits.
func (p *RidePositionPoller) stopPoll(vehicleID, rideRequestID, reason string) {
	p.mu.Lock()
	handle, ok := p.active[vehicleID]
	if !ok {
		p.mu.Unlock()
		return
	}
	if rideRequestID != "" && handle.rideRequestID != rideRequestID {
		p.mu.Unlock()
		p.logger.Debug("ride position poll: terminal event for a ride we are not polling",
			slog.String("ride_request_id", rideRequestID),
			slog.String("polling_ride_request_id", handle.rideRequestID))
		return
	}
	delete(p.active, vehicleID)
	p.mu.Unlock()

	handle.cancel()
	p.logger.Info("ride position poll stopped",
		slog.String("ride_request_id", handle.rideRequestID),
		slog.String("reason", reason))
}

// release removes a vehicle's registry entry on the way out of run, but ONLY if
// the entry is still THIS poller's — compared by handle identity, not by ride
// id, so the check cannot be fooled by a repeated id.
//
// This closes the classic orphan window: stopPoll may already have deleted the
// entry and a new ride may already have registered its own poller for the same
// car. Deleting unconditionally would silently unregister the live one while
// leaving its goroutine running — a poller nothing can stop, which is exactly
// what "a poller must not survive its ride" forbids.
func (p *RidePositionPoller) release(vehicleID string, handle *activePoll) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.active[vehicleID]; ok && h == handle {
		delete(p.active, vehicleID)
	}
}
