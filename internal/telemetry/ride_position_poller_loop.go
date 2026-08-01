package telemetry

// MYR-394 — one vehicle's poll loop, and the single cycle it repeats.
//
// The loop reuses the MYR-260 backfill entry point (RefreshFromVehicleData)
// verbatim. It adds a TRIGGER and nothing else, exactly as the MYR-320 periodic
// in-service pass does: no second REST path, no second set of failure rules, no
// second place where a Tesla read can go wrong.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// run polls one vehicle until its context is cancelled — by the ride ending, by
// the reconcile reaping it, or by server shutdown.
//
// The FIRST cycle fires immediately rather than after one interval. That is the
// whole point of the feature: the rider opens the tracking screen the moment
// the ride is accepted, and making them look at a stale marker for 25 more
// seconds would leave the reported defect on screen for the exact window it is
// most looked at.
func (p *RidePositionPoller) run(ctx context.Context, vehicleID string, handle *activePoll) {
	defer p.wg.Done()
	defer p.release(vehicleID, handle)
	// Release the child context on EVERY exit path. stopPoll already cancels on
	// the ordinary route, but a shutdown cancels the PARENT instead and this
	// child would otherwise never have its own cancel called.
	defer handle.cancel()

	p.pollOnce(ctx, vehicleID, handle)

	for {
		timer := time.NewTimer(jitterDuration(p.cfg.Interval, p.cfg.JitterFraction))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if p.lifetimeExceeded(handle, p.clock()) {
			p.logger.Warn("ride position poll: lifetime ceiling reached, stopping (ride never terminated)",
				slog.String("ride_request_id", p.rideLabel(handle)),
				slog.Duration("max_lifetime", p.cfg.MaxLifetime))
			return
		}
		p.pollOnce(ctx, vehicleID, handle)
	}
}

// pollOnce runs one cycle: yield if the car is streaming, otherwise resolve the
// VIN and token and run the existing backfill.
//
// Every failure is non-fatal and never stops the loop. A car that cannot be
// read this cycle — asleep, offline, token expired, Tesla having a bad minute —
// may well be readable the next one, and abandoning the ride's tracking on the
// first bad response is a worse outcome than a gap.
func (p *RidePositionPoller) pollOnce(ctx context.Context, vehicleID string, handle *activePoll) {
	if ctx.Err() != nil {
		return
	}

	rides := p.rideLabel(handle)

	vin, ok := p.resolveVIN(ctx, vehicleID, rides, handle)
	if !ok {
		return
	}

	// LAYER 1 of the stream yield: the car is talking to us right now, so
	// Tesla's cached REST snapshot has nothing to add and the request is not
	// worth making. Layer 2 lives inside RefreshFromVehicleData, where the
	// MYR-300 gate drops stream-sourceable fields even if we did ask — so
	// correctness does not depend on this check, only the request rate does.
	if p.pollStreamFresh(vin) {
		p.logger.Debug("ride position poll: yielding to live stream",
			slog.String("vin", redactVIN(vin)),
			slog.String("ride_request_id", rides))
		return
	}

	token, ok := p.resolveToken(ctx, vin)
	if !ok {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	defer cancel()

	if err := p.deps.Backfill.RefreshPositionFromVehicleData(callCtx, vin, token); err != nil {
		p.logPollFailure(vin, rides, err)
		return
	}

	p.logger.Debug("ride position poll: position refreshed",
		slog.String("vin", redactVIN(vin)),
		slog.String("ride_request_id", rides))
}

// resolveVIN maps the ride's vehicle to its VIN, caching the answer on the
// handle. The mapping is immutable for the lifetime of a vehicle row, so this
// costs one lookup per ride rather than one per cycle.
//
// The handle's vin field is only ever touched by its own goroutine — startPoll
// publishes the handle to the registry before this runs, but every other reader
// uses only the (lock-guarded) ride set and cancel — so no lock is needed here.
func (p *RidePositionPoller) resolveVIN(
	ctx context.Context, vehicleID, rides string, handle *activePoll,
) (string, bool) {
	if handle.vin != "" {
		return handle.vin, true
	}
	vin, err := p.deps.Vehicles.ResolveVIN(ctx, vehicleID)
	if err != nil {
		p.logger.Warn("ride position poll: cannot resolve VIN (non-fatal)",
			slog.String("ride_request_id", rides),
			slog.String("error", err.Error()))
		return "", false
	}
	if vin == "" {
		// A provisioning placeholder row (MYR-257) with no VIN yet. Nothing to
		// read from Tesla; try again next cycle in case it gets one.
		return "", false
	}
	handle.vin = vin
	return vin, true
}

// resolveToken finds the owner's current Tesla access token. Re-resolved every
// cycle rather than cached alongside the VIN: tokens expire and get refreshed
// out from under us, and the resolver is the component that knows about that.
func (p *RidePositionPoller) resolveToken(ctx context.Context, vin string) (string, bool) {
	userID, err := p.deps.Owners.GetVehicleOwner(ctx, vin)
	if err != nil {
		p.logger.Warn("ride position poll: cannot resolve owner (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()))
		return "", false
	}
	tok, err := p.deps.Tokens.Resolve(ctx, userID)
	if err != nil {
		// Overwhelmingly this is an owner who has not linked Tesla, or whose
		// link was revoked. Debug, not Warn: it is a steady state, not an
		// incident, and one such ride would otherwise emit a warning every 25s.
		p.logger.Debug("ride position poll: no Tesla token for owner (skipping cycle)",
			slog.String("vin", redactVIN(vin)))
		return "", false
	}
	if tok.AccessToken == "" {
		return "", false
	}
	return tok.AccessToken, true
}

// logPollFailure classifies a failed cycle so that the ordinary case — an
// asleep car — does not read as an incident in the logs.
//
// THE NEVER-WAKE GUARANTEE lives here by omission, and it is structural rather
// than conditional: there is no branch below that could call wake_up, because
// this component holds no waker and RefreshFromVehicleData reaches none. Tesla
// answers 408 (or 503 while a vehicle is transitioning) for a car that is
// asleep or offline; we note it and wait for the next cycle. The car may well
// wake on its own when the driver gets in, and if it does not, a rider seeing
// the last known position is a far better outcome than the server prodding a
// sleeping car awake every 25 seconds and flattening its battery.
func (p *RidePositionPoller) logPollFailure(vin, rideRequestID string, err error) {
	if isVehicleUnreachable(err) {
		p.logger.Debug("ride position poll: vehicle asleep or offline (skipped, no wake)",
			slog.String("vin", redactVIN(vin)),
			slog.String("ride_request_id", rideRequestID))
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Shutdown, or a read that outran CallTimeout. Neither is news.
		p.logger.Debug("ride position poll: cycle cut short",
			slog.String("vin", redactVIN(vin)),
			slog.String("ride_request_id", rideRequestID))
		return
	}
	p.logger.Warn("ride position poll: refresh failed (non-fatal)",
		slog.String("vin", redactVIN(vin)),
		slog.String("ride_request_id", rideRequestID),
		slog.String("error", err.Error()))
}

// isVehicleUnreachable reports whether err is Tesla saying "the car is not
// answering" rather than something being wrong.
//
// 408 is the documented asleep/offline response for vehicle_data. 503 is what
// Tesla returns while a vehicle is transitioning between states — the
// `vehicle_asleep`-shaped answer the MYR-394 brief calls out — and is treated
// identically: skip the cycle, keep the schedule, never wake.
func isVehicleUnreachable(err error) bool {
	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusRequestTimeout ||
		apiErr.StatusCode == http.StatusServiceUnavailable
}
