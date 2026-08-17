package push

import "strings"

// Notification copy for the ride lifecycle (MYR-186) — the SHARED vocabulary.
// The two audiences' alert builders live beside this file, in copy_rider.go and
// copy_owner.go; MYR-585 split them out when this file crossed the 300-line
// ceiling. What stays here is what both sides must agree on: the payload
// policy, the status and stamp vocabulary, the shared bodies, and the two
// interpolation helpers that enforce the policy mechanically.
//
// PAYLOAD POLICY — the rule the whole package exists to keep:
// a notification body may name a REQUESTER'S FIRST NAME and a VEHICLE
// NICKNAME. Nothing else about the ride is allowed out. No pickup or dropoff
// label, no street address, no coordinates, no passenger phone. Those are P1
// (data-classification.md §1.9) and a push notification is the one surface
// that renders on a LOCKED screen, to whoever is holding the phone. A client
// that needs the places refetches the ride over authenticated REST.
//
// Every string in the two audience files is therefore a constant or is built
// from exactly two interpolations, both of which are enforced below: firstName
// goes through displayName (already first-name-only when it reaches us, per the
// store's MYR-229 fallback chain), and vehicleName through vehicleLabel.

// Fallback copy used when the optional interpolation is unavailable.
const (
	fallbackVehicleName = "Your car"
	fallbackRequester   = "New ride request"
	fallbackScheduled   = "New scheduled ride request"
)

// maxNameRunes caps an interpolated name. A display name is user-controlled;
// capping keeps one pathological value from crowding the alert out of the
// lock-screen banner.
const maxNameRunes = 32

// Ride statuses that are worth waking a phone for. Every other transition
// (`requested`, `completed`) is either the recipient's own action or invisible
// to them, so it sends nothing. `cancelled` forks on WHO cancelled and reaches
// the OTHER party either way: an OWNER's cancel is the news a rider is waiting
// on a locked phone for (MYR-522), and a RIDER's cancel is news to the owner
// whether they had committed to the ride (MYR-537) or were still holding the
// request (MYR-585).
//
// `enroute` is the one status that speaks to the OWNER and not the rider: the
// rider pressed Start ride themselves, so telling them is noise, but it is the
// owner's only signal that their car has left the kerb with somebody in it.
// See ownerStatusAlert (MYR-462).
const (
	statusAccepted  = "accepted"
	statusDeclined  = "declined"
	statusArrived   = "arrived"
	statusCancelled = "cancelled"
)

// The two `cancelled_by` stamps, mirroring the handler's rideCancelledByOwner /
// rideCancelledByRider. Absence — a row cancelled before the stamp existed, or
// a publisher that does not set it (the account-deletion sweep) — sends
// nothing, which is the safe reading of an un-upgraded event.
const (
	cancelledByOwner          = "owner"
	rideCancelledByRiderStamp = "rider"
)

// alert is the title/body pair for one notification.
type alert struct {
	title string
	body  string
}

// tripEditedPart names what moved, for the MYR-541 copy: "pickup",
// "drop-off", "stops" (MYR-539), or "trip" when one edit moved more than one of
// them. Naming the PART is the whole payload policy here — the place itself
// never reaches a lock screen.
func tripEditedPart(pickup, dropoff, stops bool) string {
	switch {
	case countTrue(pickup, dropoff, stops) > 1:
		return "trip"
	case pickup:
		return "pickup"
	case stops:
		return tripPartStops
	default:
		return "drop-off"
	}
}

// tripPartStops is the one edited part that is plural, and the copy has to
// agree with it in two places.
const tripPartStops = "stops"

// countTrue counts the set flags — how many parts of the trip one edit moved.
func countTrue(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// Shared bodies.
const (
	bodyReviewRequest = "Open MyRoboTaxi to accept or decline."
	bodySeeDetails    = "Open MyRoboTaxi to see the details."
	bodyFollowAlong   = "Open MyRoboTaxi to follow the trip."
	bodyTryAnotherCar = "Try booking another car."
	bodyRideEnded     = "No need to continue — the ride has ended."
	// bodyRequestWithdrawn answers the question the owner is actually holding
	// when this lands: they still have the request notification, and tapping it
	// leads nowhere (MYR-585).
	bodyRequestWithdrawn = "Nothing to do — the request was withdrawn."
)

// vehicleLabel renders a vehicle nickname, falling back to a generic label
// when the car has no name or the lookup failed. The fallback is phrased so
// every sentence it lands in still reads naturally ("Your car can't take this
// ride", "Your car is heading your way").
func vehicleLabel(name string) string {
	trimmed := truncateRunes(strings.TrimSpace(name), maxNameRunes)
	if trimmed == "" {
		return fallbackVehicleName
	}
	return trimmed
}

// displayName normalises the requester's name for interpolation. The store has
// already reduced it to a first name (MYR-229); this only trims, re-takes the
// first token defensively, and caps the length. Returns "" when there is
// nothing usable, which routes the caller to the anonymous fallback title.
func displayName(name *string) string {
	if name == nil {
		return ""
	}
	fields := strings.Fields(*name)
	if len(fields) == 0 {
		return ""
	}
	return truncateRunes(fields[0], maxNameRunes)
}

// truncateRunes caps s at n runes (never splitting a multi-byte character).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
