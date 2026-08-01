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
	"strings"

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

// startPoll registers and launches a poller for one vehicle, or — if the car is
// already being polled — records that one more ride now depends on it.
//
// Idempotent by VEHICLE, and that is why the ride id is ADDED to the handle's
// set rather than dropped on the floor. The poll itself is per-vehicle (same
// VIN, same cadence, so a second loop would buy nothing), but the STOP is
// per-ride, and the two have to agree about who is depending on this poller.
// When they did not, a car carrying two open rides lost its poller the moment
// the FIRST of them ended — while the second rider was watching the map — and
// got it back only when the next reconcile ran, up to five minutes of exactly
// the frozen-marker defect this feature exists to fix.
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
		handle := p.active[vehicleID]
		joined := rideRequestID != "" && !handle.hasRide(rideRequestID)
		if joined {
			handle.rides[rideRequestID] = struct{}{}
			// A newly live ride is fresh justification, so the ceiling starts
			// again from here rather than from whenever the first ride began.
			handle.deadline = p.now().Add(p.cfg.MaxLifetime)
		}
		p.mu.Unlock()
		if joined {
			p.logger.Info("ride position poll: second ride joined an existing poller",
				slog.String("ride_request_id", rideRequestID),
				slog.String("reason", reason))
		}
		return
	case len(p.active) >= p.cfg.MaxActive:
		p.mu.Unlock()
		p.logger.Warn("ride position poll: at capacity, target deferred to reconcile",
			slog.Int("max_active", p.cfg.MaxActive),
			slog.String("ride_request_id", rideRequestID))
		return
	}

	// The cancel func is not called here: it is the handle's STOP LEVER, held
	// in the registry and invoked by stopPoll, and run defers it so the child
	// is released on every exit path including a parent cancellation.
	// #nosec G118 -- cancel is stored on the handle and invoked by stopPoll / run's defer, not dropped.
	ctx, cancel := context.WithCancel(p.ctx)
	handle := &activePoll{
		rides:    map[string]struct{}{rideRequestID: {}},
		deadline: p.now().Add(p.cfg.MaxLifetime),
		cancel:   cancel,
	}
	p.active[vehicleID] = handle
	p.wg.Add(1)
	p.mu.Unlock()

	go p.run(ctx, vehicleID, handle)

	p.logger.Info("ride position poll started",
		slog.String("ride_request_id", rideRequestID),
		slog.String("reason", reason),
		slog.Duration("interval", p.cfg.Interval),
		slog.Duration("max_lifetime", p.cfg.MaxLifetime))
}

// stopPoll removes one ride from a vehicle's poller, and cancels the poller
// only when no ride is left to justify it. An empty rideRequestID reaps
// unconditionally — that is the reconcile's orphan path, where the whole
// vehicle is by definition no longer wanted.
//
// The per-ride removal is what makes the teardown symmetric with startPoll's
// per-ride join: a terminal event for ride A must not cancel the poller that
// live ride B on the same car is depending on, and a late or duplicated
// terminal event for a ride that already ended must not cancel anything at all.
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
	if rideRequestID != "" {
		if !handle.hasRide(rideRequestID) {
			remaining := handle.rideIDs()
			p.mu.Unlock()
			p.logger.Debug("ride position poll: terminal event for a ride we are not polling",
				slog.String("ride_request_id", rideRequestID),
				slog.String("polling_ride_request_ids", strings.Join(remaining, ",")))
			return
		}
		delete(handle.rides, rideRequestID)
		if len(handle.rides) > 0 {
			remaining := handle.rideIDs()
			p.mu.Unlock()
			p.logger.Info("ride position poll: ride ended, vehicle still carrying another",
				slog.String("ride_request_id", rideRequestID),
				slog.String("polling_ride_request_ids", strings.Join(remaining, ",")),
				slog.String("reason", reason))
			return
		}
	}
	stopped := handle.rideIDs()
	delete(p.active, vehicleID)
	p.mu.Unlock()

	handle.cancel()
	p.logger.Info("ride position poll stopped",
		slog.String("ride_request_id", rideRequestID),
		slog.String("remaining_ride_request_ids", strings.Join(stopped, ",")),
		slog.String("reason", reason))
}

// syncRides replaces a handle's ride set with the database's view of it, used
// by the reconcile when the two overlap but do not match: the poller is still
// justified, so cancelling and re-adopting it would be a needless gap, but the
// set it holds must not keep naming a ride that has ended.
func (p *RidePositionPoller) syncRides(vehicleID string, wanted map[string]struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	handle, ok := p.active[vehicleID]
	if !ok || len(wanted) == 0 {
		return
	}
	rides := make(map[string]struct{}, len(wanted))
	for id := range wanted {
		rides[id] = struct{}{}
	}
	handle.rides = rides
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
