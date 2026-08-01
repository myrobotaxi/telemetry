package push

import (
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
//
// The input selection, the two gates and the arithmetic helpers live next door
// in activity_progress_input.go.

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
	// Reading is the raw observation Value was derived from, in Source's unit,
	// and ReadingAt is when that observation was first seen to hold.
	//
	// They date the NAV DATA rather than the car's row, which is the only way
	// the freshness horizon means what it says — see navFresh and
	// readingStalled. ReadingAt is the zero time when nothing has been recorded
	// yet, which disables the gate rather than failing it.
	Reading   float64
	ReadingAt time.Time
}

// sameAnchor reports whether two anchors record the same memory.
//
// Written out rather than `==` because ReadingAt is a time.Time: one side comes
// from time.Now() carrying a monotonic reading and the other from a TIMESTAMPTZ
// round trip that has none, and `==` compares those representations rather than
// the instants. The send path uses this to skip a write that would change
// nothing, so a false "different" is a wasted UPDATE per push per Activity.
func sameAnchor(a, b ProgressAnchor) bool {
	return a.Leg == b.Leg &&
		a.Source == b.Source &&
		a.Baseline == b.Baseline &&
		a.Value == b.Value &&
		a.Reading == b.Reading &&
		a.ReadingAt.Equal(b.ReadingAt)
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
// move the arrow. Deliberately the same horizon as StaleAfter: past it the
// reading is older than the window the card itself claims to be good for, and a
// server that kept advancing the track on data it has already declared past its
// own trust horizon would be contradicting itself.
//
// It gates PROGRESS ONLY, not `eta`, and the asymmetry is a SCOPE decision
// rather than a claim that a stale `eta` is harmless. Applying the gate to
// `eta` would omit a key that every installed build already renders, which is
// not an additive change and not this issue's. Be clear about what that leaves:
// `eta` is rebuilt from `now` on every push (contentState), so a car whose
// telemetry froze at "7 minutes" keeps producing a fresh arrival instant seven
// minutes out, tick after tick — the ETA does not decay into the past on its
// own. The honest version of this gate is its own issue; see the note in
// rest-api.md §7.21.3.
const ProgressFreshFor = StaleAfter

// progressPrecision rounds the emitted fraction to three decimals. Enough to
// place a pixel on any track a phone can draw, short enough that the JSON
// carries "0.734" rather than the fourteen digits float64 division produces,
// and — because the ROUNDED value is what gets persisted as the floor — it
// keeps the monotonicity comparison working on exactly the numbers the client
// saw.
const progressPrecision = 1000

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
	if !legUnderway(leg, rc) {
		// The ride exists but the leg has not started — a reservation still
		// dormant between accept and its due instant. Whatever the car is doing
		// is the OWNER'S own driving, and measuring a track against it is the
		// worst failure this feature has: see legUnderway. No key, and no
		// anchor, so the real leg starts from nothing when it starts.
		return nil, ProgressAnchor{}
	}

	// An anchor cut for the other leg is not a weaker anchor, it is a different
	// measurement. Discard it and keep only the leg.
	anchor := prev
	if anchor.Leg != leg {
		anchor = ProgressAnchor{Leg: leg}
	}

	value, source := progressInput(rc, anchor, now)
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

	// Date the observation before anything derives from it, so the freshness
	// gate above measures the NAV DATA's age on the next pass rather than the
	// car row's.
	anchor = noteReading(anchor, value, source, now)

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
	// different unit, or — on the DISTANCE source only — a route that is now
	// longer than it has ever been. The new baseline is chosen so THIS reading
	// means exactly the fraction already delivered, so a route change moves the
	// ground under the arrow without moving the arrow.
	if anchor.Source != source || anchor.Baseline <= 0 || rerouted(source, value, anchor.Baseline) {
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
