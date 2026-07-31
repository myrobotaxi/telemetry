package push

import (
	"math"
	"time"
)

// Leg progress for the Live Activity's track (MYR-398).
//
// The redesigned card draws the car's position on a track: toward the pickup
// while the ride is `accepted`, toward the dropoff while it is `enroute`. What
// the server pushes for that is one scalar — a 0..1 fraction of the CURRENT
// leg — and everything in this file exists to make sure that scalar is either
// true or absent.
//
// The governing rule, stated once: AN ABSENT PROGRESS RENDERS A TRACKLESS CARD,
// A WRONG ONE RENDERS A LIE. The rider can read the headline and the meet-at
// line without a track. They cannot tell that an arrow four fifths of the way
// along is describing a car that has barely left. So every branch below that
// cannot justify a number returns nil, and the two that CAN return 1.0 do it on
// the authority of the ride record rather than of an estimate.

// ProgressLeg names which leg of the ride an anchor was cut for.
//
// It is not on the wire. The client derives the leg from `status` (rest-api.md
// §7.21.3) — the server would only be sending the same fact twice — but the
// SENDER must remember it, because comparing the leg it is pushing against the
// leg the anchor describes is what makes the flip detectable. Without it, leg
// two would be measured against leg one's distance and the track would open at
// whatever fraction leg one happened to end on.
type ProgressLeg string

const (
	// ProgressLegNone means no leg is running, or none that has a track.
	ProgressLegNone ProgressLeg = ""
	// ProgressLegPickup is leg 1 — the car driving to the rider.
	ProgressLegPickup ProgressLeg = "pickup"
	// ProgressLegDropoff is leg 2 — the car driving the rider to the dropoff.
	ProgressLegDropoff ProgressLeg = "dropoff"
)

// ProgressSource names the quantity a baseline is measured in.
type ProgressSource string

const (
	// ProgressSourceNone means nothing trustworthy was available.
	ProgressSourceNone ProgressSource = ""
	// ProgressSourceNavDistance is Tesla's tripDistanceRemaining, in miles.
	// Preferred: a track depicts DISTANCE, and distance to a fixed point falls
	// as the car drives instead of being re-estimated in both directions.
	ProgressSourceNavDistance ProgressSource = "nav_distance"
	// ProgressSourceETA is Tesla's minutesToArrival, in whole minutes. The
	// fallback, and a worse one — minutes are a traffic estimate standing in
	// for a distance, so they move backwards for reasons the car never moved.
	ProgressSourceETA ProgressSource = "eta"
)

// ProgressAnchor is one Activity's memory of one leg: enough to turn "how far
// is left" into "how far along". Zero value means no anchor, which is the
// ordinary state of most rows most of the time.
type ProgressAnchor struct {
	// Leg is the leg this anchor describes.
	Leg ProgressLeg
	// Source is the quantity Baseline is measured in.
	Source ProgressSource
	// Baseline is the reading that corresponds to progress 0.
	Baseline float64
	// Value is the last fraction actually DELIVERED to this Activity, and the
	// floor the next push clamps to. Persisted only after APNs accepted the
	// push, so it records what the phone has seen rather than what we meant it
	// to see.
	Value float64
}

// The two ride statuses this file needs that copy.go's alert-worthy set does
// not carry. Spelled out here rather than imported from internal/store, which
// internal/push must not depend on; `statusAccepted` and `statusArrived` are
// reused from copy.go for the same reason one spelling is better than two.
const (
	statusEnroute   = "enroute"
	statusCompleted = "completed"
)

// MaxInFlightProgress is the highest fraction the server will send for a leg
// that is still running.
//
// A full track means the leg is OVER, and only the ride record is allowed to
// say that: `arrived` is the owner confirming the car is at the kerb and
// `completed` is the owner confirming the dropoff. An estimate that reached 1.0
// on its own would park the arrow at the end while the car is still moving —
// the rider would watch a finished-looking track for four more minutes and
// conclude the widget is broken, which for that rider it is.
const MaxInFlightProgress = 0.99

// ProgressFreshFor is how old the car's navigation reading may be and still
// move the arrow. Deliberately the same horizon as StaleAfter: past it
// ActivityKit is already telling the rider the card is out of date, and a
// server that kept advancing the track on data the phone has been told not to
// trust would be contradicting its own honesty mechanism.
//
// It gates PROGRESS ONLY, not `eta`, and the asymmetry is deliberate. `eta` is
// an absolute instant: a stale one visibly slides into the past, and the rider
// can see that for themselves. A stale fraction looks exactly like a fresh one.
// (Applying the gate to `eta` as well would change what every installed build
// renders today, which is not an additive change and not this issue's.)
const ProgressFreshFor = StaleAfter

// progressPrecision rounds the emitted fraction to three decimals. Enough to
// place a pixel on any track a phone can draw, short enough that the JSON
// carries "0.734" rather than the fourteen digits float64 division produces,
// and — because the ROUNDED value is what gets persisted as the floor — it
// keeps the monotonicity comparison working on exactly the numbers the client
// saw.
const progressPrecision = 1000

// legForStatus maps a ride status onto the leg whose track is running.
func legForStatus(status string) ProgressLeg {
	switch status {
	case statusAccepted:
		return ProgressLegPickup
	case statusEnroute:
		return ProgressLegDropoff
	default:
		// `arrived` and every terminal status are handled before this is
		// reached; `requested` and anything unrecognised genuinely have no leg.
		return ProgressLegNone
	}
}

// computeProgress derives the fraction to send and the anchor to persist if the
// send succeeds.
//
// A nil fraction means "send no `progress` key". The returned anchor is only
// ever written after a successful delivery — see ActivityNotifier.saveProgress.
func computeProgress(rc RideContext, prev ProgressAnchor, now time.Time) (*float64, ProgressAnchor) {
	// The two fractions the RIDE RECORD asserts rather than the car estimates.
	// Both clear the anchor: `arrived` because leg two must start from nothing,
	// `completed` because there is no leg three.
	if rc.Status == statusArrived || rc.Status == statusCompleted {
		full := 1.0
		return &full, ProgressAnchor{}
	}

	leg := legForStatus(rc.Status)
	if leg == ProgressLegNone {
		// `declined`, `cancelled`, `reservation_expired`, `requested`. A journey
		// that did not happen has no fraction of itself completed, and a partial
		// track on a cancellation card is noise on top of bad news.
		return nil, ProgressAnchor{}
	}

	// An anchor cut for the other leg is not a weaker anchor, it is a different
	// measurement. Discard it and keep only the leg.
	anchor := prev
	if anchor.Leg != leg {
		anchor = ProgressAnchor{Leg: leg}
	}

	value, source := progressInput(rc, now)
	if source == ProgressSourceNone {
		// Nothing trustworthy: the car has no route, or has gone quiet. HOLD
		// what this phone already has — the car did not teleport, and the last
		// fraction remains the best true statement about it — and say nothing at
		// all if it has never had one. Never invent, and never re-derive from a
		// reading we have just declined to believe.
		if anchor.Source == ProgressSourceNone {
			// Nothing to remember either: an anchor naming a leg and no
			// measurement is not a partial anchor, it is an un-interpretable
			// one, and storing it would only add a row state to reason about.
			return nil, ProgressAnchor{}
		}
		held := anchor.Value
		return &held, anchor
	}

	// The car is at the end of its route but the ride record has not caught up
	// (the owner has not tapped "picked up" yet). Report all-but-arrived: the
	// distance is real, the leg's end is not ours to declare.
	if value <= 0 {
		anchor.Source = source
		anchor.Value = MaxInFlightProgress
		capped := MaxInFlightProgress
		return &capped, anchor
	}

	// Re-anchor when the anchor cannot describe this reading: none yet, a
	// different unit, or a route that is now longer than it has ever been (a
	// reroute, or the car's nav destination changing under us). The new baseline
	// is chosen so THIS reading means exactly the fraction already delivered, so
	// a route change moves the ground under the arrow without moving the arrow.
	if anchor.Source != source || anchor.Baseline <= 0 || value > anchor.Baseline {
		anchor.Source = source
		anchor.Baseline = reanchor(value, anchor.Value)
	}

	raw := 1 - value/anchor.Baseline
	// The clamp, and the promise it keeps: the fraction the client sees never
	// decreases within a leg. Traffic genuinely moves an ETA backwards and a
	// reroute genuinely lengthens a route, but neither means the car has
	// un-driven the road behind it, and an arrow sliding back down the track
	// reads as a broken widget rather than as weather. `eta` is NOT clamped and
	// must not be — a later arrival time is news the rider needs. The pair is
	// coherent: most of the way there, and taking longer than it was.
	if raw < anchor.Value {
		raw = anchor.Value
	}
	anchor.Value = roundProgress(clampUnit(raw))
	sent := anchor.Value
	return &sent, anchor
}

// progressInput picks the reading to measure this leg with, preferring the
// car's remaining DISTANCE and falling back to its remaining minutes.
func progressInput(rc RideContext, now time.Time) (float64, ProgressSource) {
	if !navFresh(rc, now) {
		return 0, ProgressSourceNone
	}
	if v := rc.TripMilesRemaining; v != nil && isFinite(*v) && *v >= 0 {
		return *v, ProgressSourceNavDistance
	}
	if v := rc.ETAMinutes; v != nil && *v >= 0 {
		return float64(*v), ProgressSourceETA
	}
	return 0, ProgressSourceNone
}

// navFresh reports whether the car's navigation reading is recent enough to
// move the arrow.
//
// An unknown timestamp is not fresh — a car we have never heard from is exactly
// the case this guard exists for. A timestamp in the FUTURE is treated as
// fresh: it means the database clock and ours disagree, which is a fact about
// our infrastructure and not a reason to blank a rider's track.
func navFresh(rc RideContext, now time.Time) bool {
	return rc.NavUpdatedAt != nil && now.Sub(*rc.NavUpdatedAt) <= ProgressFreshFor
}

// reanchor returns the baseline that makes `value` mean exactly `sent`.
//
// With nothing delivered yet this is just `value` — the leg opens at zero. With
// a fraction already on the phone it is value/(1-sent), which is the arithmetic
// spelling of "keep the arrow where it is and re-scale what is left".
func reanchor(value, sent float64) float64 {
	if sent <= 0 {
		return value
	}
	if sent > MaxInFlightProgress {
		sent = MaxInFlightProgress
	}
	return value / (1 - sent)
}

// clampUnit bounds a fraction to [0, MaxInFlightProgress].
func clampUnit(v float64) float64 {
	if !isFinite(v) || v < 0 {
		return 0
	}
	if v > MaxInFlightProgress {
		return MaxInFlightProgress
	}
	return v
}

// roundProgress cuts the fraction to progressPrecision decimals.
func roundProgress(v float64) float64 {
	return math.Round(v*progressPrecision) / progressPrecision
}

// isFinite rejects the NaN and ±Inf a float column can hold. A NaN reaching
// encoding/json is not a bad number on a lock screen, it is a marshalling error
// that takes out the whole push.
func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
