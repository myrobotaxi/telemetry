package push

import (
	"strings"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Notification copy for the ride lifecycle (MYR-186).
//
// PAYLOAD POLICY — the rule the whole package exists to keep:
// a notification body may name a REQUESTER'S FIRST NAME and a VEHICLE
// NICKNAME. Nothing else about the ride is allowed out. No pickup or dropoff
// label, no street address, no coordinates, no passenger phone. Those are P1
// (data-classification.md §1.9) and a push notification is the one surface
// that renders on a LOCKED screen, to whoever is holding the phone. A client
// that needs the places refetches the ride over authenticated REST.
//
// The strings below are therefore all constants or built from exactly two
// interpolations, both of which are enforced elsewhere: firstName is already
// first-name-only when it reaches us (the store's MYR-229 fallback chain), and
// vehicleName comes from the vehicle's nickname column.

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

// cancelledByOwner is the `cancelled_by` stamp that makes a cancellation the
// OWNER's (MYR-522), matching the handler's rideCancelledByOwner. Absence —
// the rider's own cancel, or a row cancelled before the stamp existed — sends
// nothing, which is also the safe reading of an un-upgraded event.
const cancelledByOwner = "owner"

// alert is the title/body pair for one notification.
type alert struct {
	title string
	body  string
}

// createdAlert is the OWNER's copy for a new ride request.
//
// Scheduled rides deliberately DO NOT name the time. The server holds the
// reservation instant in UTC and knows nothing about the rider's or the
// owner's time zone, so any absolute rendering here would be either wrong
// ("5:30 PM" in the wrong zone) or unreadable ("Jul 31, 5:30 PM UTC"). The
// notification says only that the request is scheduled; the correct local
// rendering belongs to the client, which knows the device's zone and already
// refetches the ride to show it.
func createdAlert(ev events.RideRequestCreatedEvent) alert {
	scheduled := ev.ScheduledFor != nil
	name := displayName(ev.RequesterName)

	switch {
	case name == "" && scheduled:
		return alert{title: fallbackScheduled, body: bodyReviewRequest}
	case name == "":
		return alert{title: fallbackRequester, body: bodyReviewRequest}
	case scheduled:
		return alert{title: name + " requested a scheduled ride", body: bodyReviewRequest}
	default:
		return alert{title: name + " wants a ride", body: bodyReviewRequest}
	}
}

// statusAlert is the RIDER's copy for a lifecycle transition, or ok=false when
// the transition is not worth a notification. `scheduled` reports whether the
// ride is a RESERVATION (its `scheduledFor` is set).
//
// Only `declined` forks on `scheduled`, and MYR-360 is why. Until then an owner
// could only decline a ride the rider had just asked for, so "can't take this
// ride" always landed as a reply to a request still on the rider's screen. An
// owner may now decline an ACCEPTED reservation — days after it was confirmed,
// typically because they paused ride sharing — and that same sentence on a lock
// screen gives the rider no way to tell it is about a booking they thought was
// settled. The scheduled variant names the scheduled ride and nothing else.
//
// It deliberately does NOT name the TIME, the same standing rule createdAlert
// follows: the server holds `scheduledFor` in UTC and knows no client time
// zone, so an absolute rendering would be either wrong ("5:30 PM" in the wrong
// zone) or unreadable ("Aug 1, 5:30 PM UTC"). Correct local rendering belongs
// to the client, which refetches the ride anyway.
// ownerCancelled reports whether this event is an OWNER's cancellation — the
// one `cancelled` transition worth waking the rider for (MYR-522).
func ownerCancelled(status string, cancelledBy *string) bool {
	return status == statusCancelled && cancelledBy != nil && *cancelledBy == cancelledByOwner
}

func statusAlert(status, vehicleName string, scheduled, byOwnerCancel bool) (alert, bool) {
	switch status {
	case statusAccepted:
		return alert{title: "Your ride is confirmed", body: bodySeeDetails}, true
	case statusDeclined:
		if scheduled {
			return alert{
				title: vehicleLabel(vehicleName) + " can't make your scheduled ride",
				body:  "Try booking another car.",
			}, true
		}
		return alert{
			title: vehicleLabel(vehicleName) + " can't take this ride",
			body:  "Try booking another car.",
		}, true
	case statusArrived:
		return alert{
			title: "Your car is here — your turn to start",
			body:  "Open MyRoboTaxi to start the ride.",
		}, true
	case statusCancelled:
		// MYR-522: only an OWNER's cancel speaks — the rider's own cancel is
		// their own action. The grammar is declined's, because it is the same
		// news one status later: the car is not coming, try another one. The
		// vehicle nickname is the only interpolation, per the payload policy —
		// the owner's NAME never travels on a lock-screen surface.
		if !byOwnerCancel {
			return alert{}, false
		}
		if scheduled {
			return alert{
				title: vehicleLabel(vehicleName) + " had to cancel your scheduled ride",
				body:  "Try booking another car.",
			}, true
		}
		return alert{
			title: vehicleLabel(vehicleName) + " had to cancel your ride",
			body:  "Try booking another car.",
		}, true
	default:
		return alert{}, false
	}
}

// ownerStatusAlert is the OWNER's copy for a lifecycle transition, or ok=false
// when the transition is not the owner's business (MYR-462).
//
// It exists because statusAlert answers a different question. That function is
// the RIDER's side of the ladder, and its silence on `enroute` is correct there
// — the rider is the person who just pressed Start ride, and a phone that
// buzzes to report your own tap is noise. But the same transition is the single
// most consequential piece of news the OWNER gets all ride: their car has left
// the pickup with a passenger aboard. Until this function existed no send site
// anywhere in the service had the owner as a recipient except the new-request
// push, so an owner who lent their car out learned the trip had started only by
// opening the app and waiting for a refresh to land. In external beta that read
// as a 66-minute lag between the rider starting and the owner's banner moving.
//
// It obeys the same payload policy as its rider-side twin: a requester's FIRST
// NAME and nothing else about the ride. No pickup, no dropoff, no address —
// this lands on a locked screen, and the owner refetches over authenticated
// REST for the places.
// riderCancelKind classifies a RIDER's cancellation by how far the ride had
// got — which is the only thing that changes what the OWNER needs to hear.
type riderCancelKind int

const (
	// riderCancelNone — not a rider cancellation, or one carrying too little
	// provenance to classify. Sends nothing.
	riderCancelNone riderCancelKind = iota
	// riderCancelRequest — the rider withdrew a request the owner had not yet
	// decided (MYR-585). The owner's ride is unaffected; their INBOX is not.
	riderCancelRequest
	// riderCancelCommitted — the rider ended a ride the owner had already
	// committed to (MYR-537): accepted, at the kerb, or mid-trip.
	riderCancelCommitted
)

// riderCancelled classifies a `cancelled` transition stamped "rider".
//
// MYR-537 made the COMMITTED arm speak and deliberately left `requested`
// silent, reasoning that pushing every withdrawn request would page owners for
// riders changing their minds in the booking flow. MYR-585 reverses that on the
// client's direction, and the field report is the argument: a rider created a
// scheduled request, the owner's phone buzzed with it, the rider withdrew it 13
// seconds later, and the owner tapped a push into an empty screen with nothing
// anywhere to say why. The silence did not save the owner a notification — they
// had already been woken by the request itself — it only stranded the one they
// got. A cancellation is not a new interruption; it is the retraction of an
// interruption already spent.
//
// The two arms stay distinct rather than collapsing into one because the copy
// is not interchangeable: "no need to continue" is a stand-down for a car
// already moving, and it is nonsense addressed to an owner who never accepted.
//
// An event with no PreviousStatus (published pre-MYR-537) still reads as the
// silent arm, the same safe default the cancelledBy absence rule uses.
func riderCancelled(status string, cancelledBy *string, previousStatus string) riderCancelKind {
	if status != statusCancelled || cancelledBy == nil || *cancelledBy != rideCancelledByRiderStamp {
		return riderCancelNone
	}
	switch previousStatus {
	case statusAccepted, statusArrived, statusEnroute:
		return riderCancelCommitted
	case statusRequested:
		return riderCancelRequest
	}
	return riderCancelNone
}

// rideCancelledByRiderStamp mirrors the handler's rideCancelledByRider.
const rideCancelledByRiderStamp = "rider"

func ownerStatusAlert(status string, requesterName *string, cancel riderCancelKind) (alert, bool) {
	switch cancel {
	case riderCancelRequest:
		// MYR-585: the rider withdrew a request the owner had not decided yet.
		// The news is not about a car — nothing was promised and nothing has to
		// stand down — it is about the REQUEST PUSH the owner is still holding.
		// The body therefore closes that loop explicitly rather than describing
		// the ride, because "I tapped the notification and there was nothing
		// there" is the exact confusion this send exists to prevent.
		if name := displayName(requesterName); name != "" {
			return alert{
				title: name + " cancelled their ride request",
				body:  bodyRequestWithdrawn,
			}, true
		}
		// The anonymous voice every owner-facing fallback in this file uses.
		// The incoming-request card's own fallback ("New ride request") is a
		// NOUN — it names a thing that arrived, and cannot be the subject of a
		// sentence about that thing being taken back.
		return alert{
			title: "Your rider cancelled their ride request",
			body:  bodyRequestWithdrawn,
		}, true
	case riderCancelCommitted:
		// MYR-537: the rider ended a ride the owner had committed to — en
		// route to the pickup, at the kerb, or mid-trip with themselves
		// aboard. The car's dash nav cannot be cleared remotely (Tesla has no
		// cancel-navigation API), so this push IS the stand-down: the owner
		// is the only one who can stop the car going where nobody is bound.
		if name := displayName(requesterName); name != "" {
			return alert{
				title: name + " cancelled the ride",
				body:  bodyRideEnded,
			}, true
		}
		return alert{
			title: "Your rider cancelled the ride",
			body:  bodyRideEnded,
		}, true
	}
	if status != statusEnroute {
		return alert{}, false
	}
	if name := displayName(requesterName); name != "" {
		return alert{
			title: name + " started the ride",
			body:  bodyFollowAlong,
		}, true
	}
	return alert{
		title: "Your rider started the ride",
		body:  bodyFollowAlong,
	}, true
}

// dueAlert is the RIDER's copy for a scheduled ride whose pickup nav has
// reached the car. Since MYR-535 dispatch fires EARLY — the car leaves in
// time to ARRIVE at the reserved instant — so the body states the fact that
// is true at send time (the pickup is coming up) rather than the old
// "starting now", which would land up to half an hour before the pickup.
// No absolute time, per the standing rule: the server holds the instant in
// UTC and knows no client time zone.
func dueAlert(vehicleName string) alert {
	return alert{
		title: vehicleLabel(vehicleName) + " is heading your way",
		body:  "Your scheduled pickup is coming up.",
	}
}

// ownerDueAlert is the OWNER's copy for the same moment (MYR-535, client
// decision 2026-08-12): the route just landed on the car's dash, and the
// owner is the person who has to walk out and drive it. Payload policy as
// everywhere: the requester's FIRST NAME is the only interpolation — no
// pickup, no address, no time.
func ownerDueAlert(requesterName string) alert {
	if name := displayName(&requesterName); name != "" {
		return alert{
			title: name + "'s pickup is coming up",
			body:  "Your car has the route — time to head out.",
		}
	}
	return alert{
		title: "Your rider's pickup is coming up",
		body:  "Your car has the route — time to head out.",
	}
}

// navUnappliedAlert is the OWNER's copy for a nav share the car never applied
// (MYR-527): the ride is live, the dash may not have the route, and the owner
// is the only person who can fix it in under a minute. Payload policy holds —
// the vehicle nickname is the only interpolation; the destination itself (P1)
// never travels, so the copy says WHERE TO LOOK rather than where to go.
func navUnappliedAlert(vehicleName string) alert {
	return alert{
		title: vehicleLabel(vehicleName) + " may not have the route",
		body:  "Check the dash and set the destination manually if needed.",
	}
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

// riderTripChangedAlert is the RIDER's copy when the OWNER edits the trip
// (MYR-541). The edited PART is named, the place never is (P1 on a locked
// screen); the client refetches for the rest.
func riderTripChangedAlert(part string) alert {
	// "stops" is the one plural part, and "Your stops was changed" is the kind
	// of sentence that makes a product feel unattended.
	verb := " was changed"
	if part == tripPartStops {
		verb = " were changed"
	}
	return alert{
		title: "Your " + part + verb,
		body:  bodySeeDetails,
	}
}

// ownerTripChangedAlert is the OWNER's copy when the RIDER edits the trip.
func ownerTripChangedAlert(requesterName *string, part string) alert {
	if name := displayName(requesterName); name != "" {
		return alert{title: name + " changed the " + part, body: bodySeeDetails}
	}
	return alert{title: "Your rider changed the " + part, body: bodySeeDetails}
}

// Shared bodies.
const (
	bodyReviewRequest = "Open MyRoboTaxi to accept or decline."
	bodySeeDetails    = "Open MyRoboTaxi to see the details."
	bodyFollowAlong   = "Open MyRoboTaxi to follow the trip."
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
