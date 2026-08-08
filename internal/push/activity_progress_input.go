package push

import (
	"math"
	"time"
)

// Choosing, gating and dating the reading a progress fraction is derived from
// (MYR-398). computeProgress next door owns the arithmetic; this file owns the
// question of whether there is anything honest to do arithmetic ON.

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

// legUnderway reports whether the leg the status names has actually STARTED.
//
// THE DEFECT THIS CLOSES. `accepted` is not one state, it is two: a car
// dispatched and driving to the rider, and a reservation accepted for tomorrow
// that is DORMANT until the earlier of its dispatch and its due instant
// (MYR-376). The ride's status is `accepted` for both, the ticker pushes to
// both every ~75 seconds from the moment of accept, and the car it reads nav
// from is the OWNER'S CAR — which, between accepting a reservation and serving
// it, is being driven to the shops with navigation on.
//
// Without this gate the rider's lock screen anchors a pickup track on that
// errand and advances it: a baseline of five miles, then 0.2, then 0.6, and
// when the owner parks, `tripDistanceRemaining` hits zero and the all-but-
// arrived branch persists 0.99 as leg one's floor. The leg never flips (the
// status was `accepted` throughout), so when the car is finally dispatched and
// drives the real twenty minutes to the kerb, the monotone clamp pins the track
// at 0.99 for the entire actual pickup — a full track for a car that has not
// left. That is the single failure mode the field exists to prevent, aimed at
// exactly the rider it exists for, and it is DURABLE: the floor is in the
// database and survives the deploy that would otherwise clear it.
//
// The dropoff leg needs no equivalent. `enroute` is the rider having started
// leg two in person, from inside the car; there is no dormant version of it.
//
// The predicate itself is not restated here — it is
// store.rideNotDormantPredicate, evaluated in SQL beside the ride row and
// delivered as RideContext.DispatchUnderway, so this gate and MYR-376's pickup
// gate cannot drift apart. A stronger test — matching the car's active nav
// DESTINATION against the ride's pickup coordinates — is not cheaply available:
// `pickup_lat_enc`/`pickup_lng_enc` are app-encrypted (migration 0002), which
// is why dormancy is the right minimal gate.
//
// `eta` is deliberately not gated the same way and misreports on this same
// path; unlike the fraction it is self-correcting on the next reading, and
// gating it is out of this issue's scope (see ProgressFreshFor).
func legUnderway(leg ProgressLeg, rc RideContext) bool {
	if leg != ProgressLegPickup {
		return true
	}
	return rc.DispatchUnderway
}

// progressInput picks the reading to measure this leg with, preferring the
// car's remaining DISTANCE and falling back to its remaining minutes, and
// returns ProgressSourceNone when neither can be trusted.
func progressInput(rc RideContext, anchor ProgressAnchor, now time.Time) (float64, ProgressSource) {
	if !navFresh(rc, now) {
		return 0, ProgressSourceNone
	}
	value, source := navReading(rc)
	if source == ProgressSourceNone || readingStalled(anchor, value, source, now) {
		return 0, ProgressSourceNone
	}
	return value, source
}

// navReading returns the car's carried nav reading and the unit it is in.
func navReading(rc RideContext) (float64, ProgressSource) {
	if v := rc.TripMilesRemaining; v != nil && isFinite(*v) && *v >= 0 {
		return *v, ProgressSourceNavDistance
	}
	if v := rc.ETAMinutes; v != nil && *v >= 0 {
		return float64(*v), ProgressSourceETA
	}
	return 0, ProgressSourceNone
}

// navFresh is the FIRST HALF of the freshness gate: it rejects a car that has
// not reported navigation inside the horizon at all.
//
// An unknown timestamp is not fresh — a car whose navigation we have never
// heard is exactly the case this guard exists for. A timestamp in the FUTURE is
// treated as fresh: it means the database clock and ours disagree, which is a
// fact about our infrastructure and not a reason to blank a rider's track.
//
// SINCE MYR-409 THIS IS A STATEMENT ABOUT THE READINGS, NOT ABOUT THE ROW.
// `NavUpdatedAt` was `Vehicle."lastUpdated"` — a row stamp moved by every
// telemetry write of any column, so a car streaming nothing but speed and
// position held this gate open indefinitely over an ETA of arbitrary age. It is
// now `go_vehicle_control_state.nav_reading_at` (migration 0034), stamped only
// by a frame that actually carried a navigation-group member.
//
// readingStalled next door is still the other half, and still earns its keep:
// this gate dates the last frame that MENTIONED navigation, while that one
// catches a car dutifully re-sending the SAME reading — a stopped car's 30s
// nav resend keeps this stamp current, and only the value's own staleness
// distinguishes it from movement.
func navFresh(rc RideContext, now time.Time) bool {
	return rc.NavUpdatedAt != nil && now.Sub(*rc.NavUpdatedAt) <= ProgressFreshFor
}

// readingStalled is the HONEST HALF of the freshness gate: it rejects a reading
// that is byte-identical to the one already recorded and older, as an
// observation, than the horizon.
//
// This is what survives a writer that keeps the car's row fresh without
// touching its navigation — MYR-394's active-ride position polling refreshes
// `lastUpdated` every ~25 seconds while writing only position, speed and
// heading, which would otherwise hold navFresh open forever over a
// `tripDistanceRemaining` of arbitrary age. Keyed on the reading rather than on
// the row, the horizon keeps meaning what §7.21.3 says it means.
//
// A car genuinely stopped in traffic trips this too, and that costs nothing: an
// unchanged reading against an unchanged baseline yields the fraction the phone
// already has, so holding and deriving agree. What differs is the case that
// matters — a reading that is stale and would OPEN or ADVANCE a track.
func readingStalled(anchor ProgressAnchor, value float64, source ProgressSource, now time.Time) bool {
	if anchor.Source != source || anchor.ReadingAt.IsZero() {
		return false
	}
	if value != anchor.Reading {
		return false
	}
	return now.Sub(anchor.ReadingAt) > ProgressFreshFor
}

// noteReading dates the observation the next push will compare against.
//
// The stamp moves only when the reading CHANGES, which is the whole point: it
// records when the car last told us something new about this leg, not when we
// last looked.
func noteReading(anchor ProgressAnchor, value float64, source ProgressSource, now time.Time) ProgressAnchor {
	if anchor.Source != source || anchor.ReadingAt.IsZero() || anchor.Reading != value {
		anchor.Reading, anchor.ReadingAt = value, now
	}
	return anchor
}

// rerouted reports whether a reading longer than the leg's baseline means the
// ROUTE got longer — which is true of a distance and false of an ETA.
//
// Distance to a fixed point falls monotonically as the car drives, so a
// distance ABOVE the baseline can only be a reroute or a changed destination,
// and re-scaling the baseline is the honest response. Minutes are not a
// distance: they are a traffic re-estimate that moves in both directions for
// reasons the car did not move, and §7.21.3's degradation table says in as many
// words that traffic pushing the ETA out must leave the fraction HELD. Treating
// that as a reroute would silently redefine the leg as longer, and because the
// inflation is monotone-legal nothing downstream would ever catch it: a
// 12-minute leg that spikes to 20 and settles back to 6 would render 0.85 for a
// car that is half way. The clamp absorbs it instead — raw goes negative and
// the floor holds the delivered fraction.
//
// THE TRADE-OFF, STATED: on the ETA source a genuine DESTINATION CHANGE now
// freezes the track until the ETA falls back below the original baseline, since
// minutes cannot distinguish a longer route from slower traffic. That is the
// choice the contract already made — its traffic row demands the hold, and its
// reroute row is written in distance terms.
func rerouted(source ProgressSource, value, baseline float64) bool {
	return source == ProgressSourceNavDistance && value > baseline
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
