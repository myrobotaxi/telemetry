package push

// Naming the place the ETA is about (MYR-587).
//
// THE DEFECT, from a real multi-stop ride on 2026-08-18 03:02Z: the rider's
// lock screen read "10:39 PM dropoff · Heading to Bell Southstone Yards" while
// the car was driving to TARGET, the first stop. 10:39 PM was the wire `eta`,
// and the wire `eta` is ALWAYS about the leg the car is currently driving
// (MYR-485: every displayed value originates in the car's own navigation, and
// the car navigates ONE leg at a time). So the card paired the next stop's
// arrival time with the trip's final place name — two true values making one
// false sentence, on a surface whose entire job is to be read in a glance.
//
// The fix is the rule MYR-564 already applied in-app: NAME THE PLACE THE ETA IS
// ABOUT. The server is the party that can — it holds the stops and their
// server-owned statuses — and it states it rather than leaving the client to
// guess from a list it cannot see change while backgrounded.
//
// This is a COPY-SHAPED change with no prose on the wire, which is the rule
// §7.21.3 sets for this surface: the card gets the label it should print and a
// BOOLEAN saying what kind of place it is, and the client still owns every word
// around them ("stop" versus "dropoff" in the headline).

// TripStop is one intermediate stop as the content-state needs it: the
// server-owned status that says whether the car has passed it, and the label
// the card would print. Slice order IS travel order.
//
// P1 on the label, exactly like RideContext.Destination and for the same
// reason — it is a place the rider named.
type TripStop struct {
	// Status mirrors the contracts RideStop.status enum: `upcoming`,
	// `current`, `completed`. Carried as a string rather than a typed enum
	// because internal/push must not import internal/store, and because the
	// rule below deliberately treats an UNRECOGNISED member as meaningful
	// rather than as an error.
	Status string
	// Label is the stop's short name, e.g. "Original ChopShop".
	Label string
}

// The two stop statuses this rule reads by name. `upcoming` is deliberately not
// among them: the ladder asks what a stop is NOT (completed), so a member added
// to the enum tomorrow needs no edit here. See stopAhead.
const (
	stopStatusCurrent   = "current"
	stopStatusCompleted = "completed"
)

// legDestination returns the label the card should print and whether that label
// is an intermediate STOP rather than the trip's final drop-off.
//
// SCOPED TO `enroute`, and the scope is the substance rather than an
// optimisation. `enroute` is the one status on which the car is driving the
// rider's own itinerary, so it is the only one where the ETA can be about a
// stop. On `accepted` the car is driving to the PICKUP — the ETA is about a
// place that is deliberately not on this wire at all (§7.21.3: the app already
// holds it) and the card's headline says "Pick up in N min" — and on
// `requested` there is no car and no ETA. On a terminal status the ride is over
// and there is nothing to head to: naming a stop on a ride that ENDED EARLY at
// stop 2 (MYR-547 leaves the rest unreached) would put the card's last word on
// a place the car never went. All of those keep the drop-off, byte-identical to
// what they sent before this existed.
//
// A TWO-ENDPOINT RIDE IS BYTE-IDENTICAL TOO, by construction rather than by a
// special case: with no stops there is nothing ahead, so the drop-off IS the
// current target and the boolean is false.
func legDestination(rc RideContext) (label string, isStop bool) {
	if rc.Status != statusEnroute {
		return rc.Destination, false
	}
	if stop, ok := stopAhead(rc.Stops); ok {
		return stop.Label, true
	}
	return rc.Destination, false
}

// stopAhead picks the stop the car is driving to, by the exact ladder the
// client's own currentTargetLabel walks (MYR-564):
//
//  1. the stop the SERVER has marked `current` — the arrival detector advances
//     it (MYR-538), so it is the only positive statement about where the car is
//     going, and it wins outright even over an earlier stop still reading
//     `upcoming`;
//  2. otherwise the FIRST stop that is not `completed`, which covers the window
//     between departing one stop and being detected at the next;
//  3. otherwise nothing — every stop is behind the car, and the caller falls
//     through to the drop-off.
//
// ANYTHING THAT IS NOT `completed` COUNTS AS AHEAD, and that is MYR-547's
// fail-safe rather than loose parsing. `completed` is the only status that
// asserts the car REACHED a place; every other value — including one this build
// has never heard of, since the contract's enums are append-only and a client
// must tolerate an unrecognised member — is a stop it did not reach yet. The
// two failure directions are not symmetric: skipping a stop we cannot classify
// walks the card back to the drop-off and reinstates exactly the wrong sentence
// this rule exists to delete, while counting it as ahead names a place that is
// genuinely still on the itinerary.
//
// An EMPTY label is returned as it is, not skipped. A stop the rider left
// unnamed is still the place the ETA is about, and the pairing stays honest;
// `destination` has no minimum length on this wire today (a drop-off can be
// blank in exactly the same way), so the client's own fallback for a nameless
// place is the right place to answer it — reaching past it to the drop-off
// would name the wrong place, which is strictly worse than naming none.
func stopAhead(stops []TripStop) (TripStop, bool) {
	firstAhead := -1
	for i := range stops {
		if stops[i].Status == stopStatusCompleted {
			continue
		}
		if stops[i].Status == stopStatusCurrent {
			return stops[i], true
		}
		if firstAhead < 0 {
			firstAhead = i
		}
	}
	if firstAhead < 0 {
		return TripStop{}, false
	}
	return stops[firstAhead], true
}
