// The nav-apply close-loop (MYR-527). `dispatch_status = sent` records that
// Tesla's share endpoint answered success — which it does whether or not the
// car ever applies the destination. The gap's first live casualty (2026-08-12):
// a rider aboard a 3-hour trip whose dash still navigated to the pickup, the
// app faithfully rendering the wrong nav ("4 min away") because the rider
// surface reads the car's own telemetry by design (MYR-485).
//
// The confirmation signal is the car's OWN reporting: DestinationLocation
// streams at 1s with a 30s resend and persists to the vehicle row, so a share
// the car applied shows up there within seconds. The verifier watches for the
// reported destination to converge on the shared target; if it never does, it
// re-shares ONCE through the same sequencer-gated pipeline, and if it still
// never does, it publishes `ride.nav_unapplied` — whose consumer is the
// owner's "check the dash" push, the human fallback the incident actually
// needed.
//
// BEST-EFFORT BY DESIGN, and the asymmetries are deliberate:
//   - Only the process that won the leg's claim verifies, so there is at most
//     one verifier per leg. A process that dies mid-verification simply stops
//     — the outcome row is already honest (`sent` is true: Tesla accepted it)
//     and a lost verification costs a warning, never a dispatch.
//   - A verification that cannot decide (status read failed, ride moved on,
//     leg superseded) abandons SILENTLY. The alert exists for the one case
//     that is both diagnosable and actionable: a live leg whose target never
//     appeared in the car's own reporting.
//   - The re-share re-enters the navSequencer with the leg's own order, so it
//     can never walk the car's destination backwards past a later leg
//     (MYR-526's guarantee holds for re-shares by construction), and it does
//     NOT touch the claim or the outcome row — the first push's `sent` is the
//     true statement about what happened.

package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// NavVerifyStore is the verifier's store seam, consumer-site per convention.
type NavVerifyStore interface {
	// VehicleNavDestination is the destination the CAR ITSELF reports it is
	// navigating to (decrypted), or a nil pair when it reports none. The
	// (0, 0) sentinel may come back as a non-nil pair and is treated as
	// "no destination" here, per the standing §2.3 rule.
	VehicleNavDestination(ctx context.Context, vehicleID string) (lat, lng *float64, err error)
	// RideStatus is the ride's bare lifecycle status — the verifier's "is
	// this leg still the ride's current leg?" probe.
	RideStatus(ctx context.Context, rideID string) (string, error)
}

const (
	// navVerifyWindow is how long the car gets to report the shared
	// destination before the verifier acts. Applied shares show up in
	// seconds (1s stream + 30s resend); 90s tolerates a car mid-wake.
	navVerifyWindow = 90 * time.Second
	// navVerifyPoll is the destination-read cadence — a lean two-column
	// decrypt per poll, only while a leg is under verification.
	navVerifyPoll = 10 * time.Second
	// navVerifyToleranceMiles accepts the car's reported destination as "the
	// shared target" within ~250 m: the car re-geocodes the share onto its
	// own map, so parking-lot-scale disagreement is ordinary and street-scale
	// disagreement is not.
	navVerifyToleranceMiles = 0.16
)

// navVerify holds the optional close-loop wiring. Nil store = feature off —
// the ordinary state for every test and for a deploy that wires nothing.
type navVerify struct {
	store NavVerifyStore
	bus   events.Bus
	// ctx is the SERVER's lifetime context, not any per-event context: a
	// verification outlives the dispatch that armed it by design, and dies
	// with the process rather than stalling shutdown (it is deliberately NOT
	// tracked by the drain group).
	ctx context.Context
	// window/poll are the constants, injectable for tests.
	window, poll time.Duration
}

// WithNavVerify wires the close-loop. ctx should be the server's root
// context; bus may be nil (the failure seam is then log-only).
func (d *Dispatcher) WithNavVerify(ctx context.Context, store NavVerifyStore, bus events.Bus) *Dispatcher {
	d.verify = &navVerify{store: store, bus: bus, ctx: ctx, window: navVerifyWindow, poll: navVerifyPoll}
	return d
}

// legCurrentStatus is the ride status during which a leg's target IS the
// car's rightful destination. Outside it the verification is moot: a pickup
// leg whose ride reached `arrived` was either applied or overtaken by the
// arrival itself, and a dropoff leg whose ride completed needs nothing.
func legCurrentStatus(order legOrder) string {
	if order == legOrderDropoff {
		return "enroute"
	}
	return "accepted"
}

// armNavVerify starts the close-loop for a leg whose push just resolved
// `sent`. No-op when unwired.
func (d *Dispatcher) armNavVerify(leg dispatchLeg) {
	v := d.verify
	if v == nil {
		return
	}
	go d.runNavVerify(v, leg)
}

// runNavVerify is the whole loop: observe → (re-share → observe) → report.
func (d *Dispatcher) runNavVerify(v *navVerify, leg dispatchLeg) {
	// Bounded independently of any per-event context: two observation
	// windows, one re-share (up to OverallTimeout), plus slack.
	ctx, cancel := context.WithTimeout(v.ctx, 2*v.window+d.cfg.OverallTimeout+30*time.Second)
	defer cancel()

	if d.observeNavApplied(ctx, v, leg, "") {
		return
	}
	if !d.legStillCurrent(ctx, v, leg) {
		return
	}
	switch d.reshareNav(ctx, leg) {
	case reshareAbandoned:
		// Superseded, cancelled, or unresolvable — someone else owns the
		// situation now, and an alert here would be about a leg that is no
		// longer the car's rightful destination.
		return
	case reshareSent:
		if d.observeNavApplied(ctx, v, leg, " after re-share") {
			return
		}
	case reshareFailed:
		// The re-share itself could not reach the car. Nothing further to
		// wait for — the alert is the honest next step.
	}
	d.reportNavUnapplied(ctx, v, leg)
}

// reshareResult is what one re-share attempt amounted to.
type reshareResult int

const (
	// reshareAbandoned: the leg was superseded or the context ended — no
	// re-share happened and none should. Silent.
	reshareAbandoned reshareResult = iota
	// reshareSent: the re-share reached Tesla; observe again.
	reshareSent
	// reshareFailed: the re-share was attempted and did not deliver (resolve
	// or command failure). The car will not converge on its own — report.
	reshareFailed
)

// observeNavApplied polls the car's reported destination for one window.
// True the moment it converges on the leg's target.
func (d *Dispatcher) observeNavApplied(ctx context.Context, v *navVerify, leg dispatchLeg, phase string) bool {
	deadline := time.Now().Add(v.window)
	for {
		if d.navDestinationMatches(ctx, v, leg) {
			d.logger.Info("dispatch: nav share confirmed applied"+phase,
				slog.String("leg", leg.name),
				slog.String("ride_id", leg.rideID),
				slog.String("vehicle_id", leg.vehicleID),
			)
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return true // shutdown: abandon silently, never report
		case <-time.After(v.poll):
		}
	}
}

// navDestinationMatches reads the car's reported destination and compares it
// to the leg's target. Any failure to read is "not yet" — the loop's deadline
// decides, never one read.
func (d *Dispatcher) navDestinationMatches(ctx context.Context, v *navVerify, leg dispatchLeg) bool {
	lat, lng, err := v.store.VehicleNavDestination(ctx, leg.vehicleID)
	if err != nil || lat == nil || lng == nil {
		return false
	}
	if *lat == 0 && *lng == 0 {
		return false
	}
	return haversineMiles(*lat, *lng, leg.coord.Latitude, leg.coord.Longitude) <= navVerifyToleranceMiles
}

// legStillCurrent reports whether the leg's target is still the ride's
// rightful destination. An unreadable status abandons (false): the re-share
// and the alert both assert things about a ride we then cannot see.
func (d *Dispatcher) legStillCurrent(ctx context.Context, v *navVerify, leg dispatchLeg) bool {
	status, err := v.store.RideStatus(ctx, leg.rideID)
	if err != nil {
		d.logger.Debug("dispatch: nav verify abandoned, ride status unreadable",
			slog.String("leg", leg.name),
			slog.String("ride_id", leg.rideID),
			slog.String("error", err.Error()),
		)
		return false
	}
	return status == legCurrentStatus(leg.order)
}

// reshareNav re-runs the leg's Tesla push through the sequencer gate —
// reacquire admits the leg's OWN order, a later leg still wins — WITHOUT
// touching the claim or the outcome row: the first push's `sent` remains the
// true statement about what happened.
func (d *Dispatcher) reshareNav(ctx context.Context, leg dispatchLeg) reshareResult {
	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hold, err := d.nav.reacquire(legCtx, leg.vehicleID, leg.rideID, leg.order, cancel)
	if err != nil {
		if !errors.Is(err, errNavSuperseded) {
			d.logger.Debug("dispatch: nav re-share abandoned at the gate",
				slog.String("leg", leg.name),
				slog.String("ride_id", leg.rideID),
				slog.String("error", err.Error()),
			)
		}
		return reshareAbandoned
	}
	defer hold.Release()

	vin, code := d.resolveVIN(legCtx, leg.vehicleID)
	if code != nil {
		return reshareFailed
	}
	token, code := d.resolveToken(legCtx, leg.ownerID)
	if code != nil {
		return reshareFailed
	}
	outcome, _, _ := d.executeWithRetry(legCtx, vin, token, leg.coord)
	d.logger.Info("dispatch: nav share re-shared",
		slog.String("leg", leg.name),
		slog.String("ride_id", leg.rideID),
		slog.String("vehicle_id", leg.vehicleID),
		slog.String("outcome", string(outcome)),
	)
	if hold.Superseded() {
		return reshareAbandoned
	}
	if outcome == OutcomeSent {
		return reshareSent
	}
	return reshareFailed
}

// reportNavUnapplied is the loop's end state: a live leg the car never took,
// through the window AND a re-share. One ERROR line, one internal event.
func (d *Dispatcher) reportNavUnapplied(ctx context.Context, v *navVerify, leg dispatchLeg) {
	// The alert must not fire about a ride that moved on during the second
	// window — the same silence rule as the first check.
	if !d.legStillCurrent(ctx, v, leg) {
		return
	}
	d.logger.Error("dispatch: nav share never applied by the car",
		slog.String("leg", leg.name),
		slog.String("ride_id", leg.rideID),
		slog.String("vehicle_id", leg.vehicleID),
	)
	if v.bus == nil {
		return
	}
	evt := events.NewEvent(events.RideNavUnappliedEvent{
		RideRequestID: leg.rideID,
		VehicleID:     leg.vehicleID,
		OwnerID:       leg.ownerID,
		Leg:           leg.name,
	})
	if err := v.bus.Publish(ctx, evt); err != nil {
		d.logger.Warn("dispatch: publish ride.nav_unapplied failed",
			slog.String("ride_id", leg.rideID),
			slog.String("error", err.Error()),
		)
	}
}
