package push

import "time"

// Island auto-expand alerts (MYR-398, the v3 card).
//
// A Live Activity update carrying an `aps.alert` dictionary makes iOS expand
// the Dynamic Island for about three seconds and then collapse it again. That
// expansion is the whole point: the compact island is a figure or a glyph, and
// a phase changing underneath a figure that happens to look the same is a state
// change the rider never sees. The v3 design asks for the expansion on SIX
// phase changes and forbids it everywhere else, in as many words — "do not
// alert on ETA ticks or the stale flip, or the island strobes".
//
// EVERYTHING IN THIS FILE EXISTS TO MAKE "EXACTLY ONCE" TRUE, because the
// failure mode is not a missed expansion, it is an island that opens every 75
// seconds for the length of a ride. Two properties get us there and neither is
// sufficient alone:
//
//   - The phases form a LADDER, and a push may only alert on a phase strictly
//     ABOVE the highest one this Activity has already been alerted at. A
//     monotone comparison, rather than an equality test against the last phase,
//     is what makes a duplicate or out-of-order `ride.status.changed` event
//     harmless — replaying `accepted` after `arrived` re-alerts nothing.
//   - The high-water mark is PERSISTED per Activity (migration 0029), for the
//     same two reasons the progress anchor is: the ticker is not sharded, so an
//     in-memory mark would let every replica alert once for the same phase, and
//     a deploy mid-ride would forget it entirely.
//
// The five status-driven phases would very nearly hold without any of this,
// since the lifecycle notifier fires once per transition. Arriving would not:
// it is a threshold the ETA ticker re-evaluates every pass, so it is true for
// every remaining tick of the pickup leg once it is true at all.

// AlertPhase is a position on the v3 design's phase ladder — Uber's five, plus
// the end.
//
// The ORDINALS ARE THE CONTRACT, not the names: the whole once-per-ride
// guarantee is a `>` between two of these, and they are persisted as smallints
// (`go_live_activities.alerted_phase`), so a value may never be renumbered and
// a new phase may only be inserted where its number keeps the ride's real order
// ascending. The order here is exactly the order a ride passes through them.
type AlertPhase int16

const (
	// AlertPhaseNone is "no phase worth expanding the island for" — and the
	// zero value, so an Activity that has never been alerted starts below every
	// real phase. The unhappy endings map here: `declined`, `cancelled` and
	// `reservation_expired` are outside the design's six, and an island opening
	// to deliver bad news the card is already showing is not a kindness.
	AlertPhaseNone AlertPhase = 0
	// AlertPhaseDispatch is `requested` — the v3 card's "Finding your ride",
	// which exists because the design starts the Activity at REQUEST.
	AlertPhaseDispatch AlertPhase = 1
	// AlertPhaseEnroute is `accepted` — a car is coming.
	AlertPhaseEnroute AlertPhase = 2
	// AlertPhaseArriving is the pickup leg's ETA crossing under
	// ArrivingWithinMinutes. The one phase that is not a ride status.
	AlertPhaseArriving AlertPhase = 3
	// AlertPhaseArrived is `arrived` — the car is at the kerb.
	AlertPhaseArrived AlertPhase = 4
	// AlertPhaseOnTrip is `enroute` — leg two, the rider is in the car.
	AlertPhaseOnTrip AlertPhase = 5
	// AlertPhaseCompleted is `completed`. Its expansion rides an alerting
	// UPDATE sent immediately before the alert-free `end`, NOT the `end` itself
	// (MYR-418): an `aps.alert` on an `end` is undocumented by Apple, accepted
	// by APNs and honoured by nothing, so the completion is a pair — see
	// endRide for the two sends, and buildActivityPayload for the renderer's
	// refusal to write the key on an `end` at all.
	AlertPhaseCompleted AlertPhase = 6
)

// ArrivingWithinMinutes is the pickup-leg ETA at or below which the ride enters
// the Arriving phase.
//
// Two minutes is the design's threshold and it is stated in MINUTES because
// that is the only resolution the input has: Tesla reports minutesToArrival as
// a whole number, so a duration-typed constant would imply a precision the car
// never sends.
//
// It is a threshold on a value that MOVES IN BOTH DIRECTIONS — traffic
// re-estimates an ETA — which is exactly why the ladder is monotone rather than
// a state test. A car that reads 2, then 3, then 1 crosses the threshold twice
// and must alert once.
const ArrivingWithinMinutes = 2

// ActivityAlert is the `aps.alert` dictionary on an alerting Activity update.
//
// THIS IS THE ONE PLACE THIS SURFACE SENDS PROSE, and it is a deliberate
// exception to §7.21.3's "the server sends the enum and never prose" rather
// than an oversight. APNs has no way to say "expand the island using copy the
// app already holds": an alert dictionary is title and body, and the localised
// alternative (`title-loc-key` / `loc-key`) renders the RAW KEY on the lock
// screen when the installed build's string table lacks it — which is precisely
// the client/server skew this project ships into.
//
// So the copy here is deliberately BLAND AND GENERIC, and deliberately not the
// card's copy. The card's headline is the client's and changes in an app
// update; this is a fallback banner that only a device without a Dynamic Island
// ever renders in full. Keeping the two visibly different is what stops the
// alert from quietly becoming a second, server-owned copy deck.
//
// P1 RULE, UNCHANGED: no pickup or dropoff label, no address, no vehicle
// nickname, no name. `destination` is carried in the CONTENT-STATE as the
// narrow exception argued in §7.21.3 — an alert body is a different surface
// with a different threat model (it renders on a locked screen and on a paired
// watch, to whoever is holding the device), and copy.go's payload policy
// applies to it in full.
type ActivityAlert struct {
	// Title is `aps.alert.title`.
	Title string
	// Body is `aps.alert.body`.
	Body string
}

// alertTitle is the same for every phase on purpose: the phase is the BODY's
// job, and a title that restated it would put the same sentence on the banner
// twice.
const alertTitle = "Your ride"

// alertBodies is the copy for each phase of the ladder.
//
// "Your ride", never a model name and never a nickname — the v3 copy rules say
// the model appears exactly once, in the card's subline, as identification.
// Note "almost here" rather than "arriving": Arriving is an ENGINEERING phase
// name and the design retires the word from rider-facing copy, because it was
// the word the field report was about.
var alertBodies = map[AlertPhase]string{
	AlertPhaseDispatch:  "Finding your ride",
	AlertPhaseEnroute:   "Your ride is on the way",
	AlertPhaseArriving:  "Your ride is almost here",
	AlertPhaseArrived:   "Your ride is here",
	AlertPhaseOnTrip:    "You're on your way",
	AlertPhaseCompleted: "You've arrived",
}

// alertPhaseFor maps the state a push is about to carry onto its phase.
//
// It reads the SAME RideContext the content-state is built from, rather than
// being handed the transition that triggered the push, and that is what makes
// the two send paths agree: the lifecycle notifier and the ETA ticker compute
// the identical phase for identical state, so which one happens to run first
// after a transition cannot change how many times the island opens.
//
// `now` is here for the freshness half of the Arriving gate; every other rung is
// a pure function of the status.
func alertPhaseFor(rc RideContext, now time.Time) AlertPhase {
	switch rc.Status {
	case statusRequested:
		return AlertPhaseDispatch
	case statusAccepted:
		if arrivingSoon(rc, now) {
			return AlertPhaseArriving
		}
		return AlertPhaseEnroute
	case statusArrived:
		return AlertPhaseArrived
	case statusEnroute:
		return AlertPhaseOnTrip
	case statusCompleted:
		return AlertPhaseCompleted
	default:
		// `declined`, `cancelled`, `reservation_expired`, and anything
		// unrecognised. Outside the design's six.
		return AlertPhaseNone
	}
}

// arrivingSoon reports whether the PICKUP leg is inside the Arriving threshold.
//
// TWO GATES, and neither is defensive.
//
// DispatchUnderway, because `accepted` covers a dispatched car driving to the
// rider AND a reservation accepted for tomorrow that is dormant until its due
// instant (MYR-376), and the car this reads navigation from is the OWNER'S,
// which in between is running the owner's own errands. Without it, an owner
// parking two minutes from home would open the rider's island with "your ride is
// almost here" a day before the pickup — the same defect legUnderway closes for
// the progress track, with a notification attached to it.
//
// navFresh, because THE REST OF THIS PACKAGE ALREADY REFUSES TO TRUST AN UNDATED
// ETA and this rung was the one place that did. `ETAMinutes` is the car's last
// streamed `minutesToArrival`, a column that keeps its value after the route
// that produced it ends, so a car that has just finished a previous trip reads 0
// or 1 until it says otherwise. On an INSTANT ride `scheduled_for IS NULL` makes
// DispatchUnderway unconditionally true, and the accept push fans out before the
// dispatched route has streamed back — so ungated, the moment a rider's ride was
// accepted their island would expand with "almost here" for a car twenty minutes
// away, and the mark would persist at Arriving(3). That last part is why this is
// worth a gate rather than a shrug: a wrong `eta` self-corrects on the next
// reading, but a burnt rung is DURABLE. Rungs 2 and 3 would both be spent, so
// the rider would never be told the car was on its way and never get the real
// Arriving expansion when it genuinely was two minutes out — the next thing they
// saw would be Arrived.
//
// BOTH ARMS ARE NOW CLOSED (MYR-409). This comment used to end by naming an
// arm that was still open, and the note is kept because the shape of the fix is
// the interesting part.
//
// The arm that was always closed is the common one: a car that finished its
// errand, parked and went quiet. Its stamp ages out of the horizon and the
// accept push correctly yields Enroute.
//
// The arm that was open is a car ACTIVELY NAVIGATING SOMEWHERE ELSE at the
// instant its owner accepts an instant ride. `NavUpdatedAt` was
// `Vehicle."lastUpdated"`, a ROW stamp, so a car awake and streaming speed and
// position carried a "fresh" row over a `minutesToArrival` of any age — and the
// obvious fix, `readingStalled` next door, is keyed on an anchor this rung does
// not have at accept, since the anchor is only opened by a DELIVERED push.
//
// It is closed by dating the reading on the car rather than on the ride:
// `NavUpdatedAt` is now `go_vehicle_control_state.nav_reading_at`, stamped only
// by a frame that carried a navigation-group member. Note what this does and
// does not claim. A car navigating its owner's errand at accept is genuinely
// streaming FRESH navigation, so this gate passes — and the rung is not burnt
// anyway, because the errand's `minutesToArrival` describes a route to the
// owner's destination and only resolves Arriving when that route is itself
// nearly over. What is now impossible is the durable case the issue was filed
// for: a STALE `minutesToArrival`, frozen at 0 or 1 by a route that ended
// without a cancel, riding a row stamp kept fresh by motion.
func arrivingSoon(rc RideContext, now time.Time) bool {
	if !rc.DispatchUnderway || !navFresh(rc, now) || !navReadingIsOurs(rc) {
		return false
	}
	return rc.ETAMinutes != nil && *rc.ETAMinutes >= 0 && *rc.ETAMinutes <= ArrivingWithinMinutes
}

// navReadingIsOurs reports whether the car's navigation reading can possibly be
// about THIS RIDE'S PICKUP, by asking whether it postdates the dispatch that
// sent the car there (MYR-409).
//
// WHY FRESHNESS IS NOT ENOUGH, which is the whole reason this exists. navFresh
// now dates the reading rather than the row, and that closes the frozen-value
// arm: a `minutesToArrival` left at 1 by a route that ended without a cancel no
// longer rides a row stamp kept current by motion. It does NOT close the arm
// this issue is named for. A car navigating its owner's errand at the instant an
// instant ride is accepted is streaming perfectly fresh navigation, one second
// old, describing a route to the shops — and if the owner happens to be two
// minutes from the shops, `ETAMinutes` is 2 and the rung burns. Recency and
// relevance are different questions and only one of them is a timestamp on the
// car.
//
// The dispatch instant answers the other. A reading taken BEFORE the server told
// the car where the rider is cannot be describing the rider, whatever its age.
//
// NIL IS NOT FRESH, and that is deliberate rather than defensive. A nil stamp
// means leg 1 was never dispatched — a reservation still sleeping, or a dispatch
// that failed or was skipped — so the car was never sent to this rider at all
// and nothing it is navigating is theirs. DispatchUnderway catches the sleeping
// reservation; the failed and skipped dispatches are this predicate's alone.
//
// THE RESIDUAL, stated because it is real and this gate does not close it.
// Dispatch is recorded when the destination is PUSHED to the car, not when the
// car acts on it, so a nav frame emitted in the seconds between those two events
// postdates the dispatch while still describing the old route. Closing that
// needs the car's active destination matched against the ride's pickup
// coordinates, and `pickup_lat_enc`/`pickup_lng_enc` are app-encrypted
// (migration 0002) — the same reason legUnderway settles for dormancy next door.
// The window is now a few seconds wide rather than unbounded, and it errs the
// safe way at the moment that matters: on the accept push, dispatch is
// approximately now, so no reading yet postdates it and the rung is left unspent
// for the real Arriving.
func navReadingIsOurs(rc RideContext) bool {
	if rc.PickupDispatchedAt == nil || rc.NavUpdatedAt == nil {
		return false
	}
	return !rc.NavUpdatedAt.Before(*rc.PickupDispatchedAt)
}

// alertFor returns the alert to attach to a push carrying `phase`, or nil when
// this Activity has already been alerted at or above it.
//
// `alerted` is the high-water mark read from the Activity's row. The comparison
// is strictly greater-than, which is what makes the guarantee "at most once per
// phase per Activity" rather than "usually once".
func alertFor(phase, alerted AlertPhase) *ActivityAlert {
	if phase <= alerted {
		return nil
	}
	body, ok := alertBodies[phase]
	if !ok {
		return nil
	}
	return &ActivityAlert{Title: alertTitle, Body: body}
}
