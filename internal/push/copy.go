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
// to them, so it sends nothing. `cancelled` forks on WHO cancelled (MYR-522):
// the rider's own cancel is their own action and stays silent, an OWNER's
// cancel is exactly the news a rider is waiting on a locked phone for.
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
// riderCancelled reports whether this event is a RIDER's cancellation of a
// ride its owner had already committed to (MYR-537): stamped "rider", and the
// pre-check status was accepted or later. A `requested` cancel stays silent —
// the owner never committed, and pushing every cancel of a request that was
// never accepted would page owners for riders changing their minds in the
// booking flow. An event with no PreviousStatus (published pre-MYR-537) reads
// as the silent arm, the same safe default the cancelledBy absence rule uses.
func riderCancelled(status string, cancelledBy *string, previousStatus string) bool {
	if status != statusCancelled || cancelledBy == nil || *cancelledBy != rideCancelledByRiderStamp {
		return false
	}
	switch previousStatus {
	case statusAccepted, statusArrived, statusEnroute:
		return true
	}
	return false
}

// rideCancelledByRiderStamp mirrors the handler's rideCancelledByRider.
const rideCancelledByRiderStamp = "rider"

func ownerStatusAlert(status string, requesterName *string, byRiderCancel bool) (alert, bool) {
	if byRiderCancel {
		// MYR-537: the rider ended a ride the owner had committed to — en
		// route to the pickup, at the kerb, or mid-trip with themselves
		// aboard. The car's dash nav cannot be cleared remotely (Tesla has no
		// cancel-navigation API), so this push IS the stand-down: the owner
		// is the only one who can stop the car going where nobody is bound.
		if name := displayName(requesterName); name != "" {
			return alert{
				title: name + " cancelled the ride",
				body:  "No need to continue — the ride has ended.",
			}, true
		}
		return alert{
			title: "Your rider cancelled the ride",
			body:  "No need to continue — the ride has ended.",
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
// "drop-off", or "trip" when one edit moved both.
func tripEditedPart(pickup, dropoff bool) string {
	switch {
	case pickup && dropoff:
		return "trip"
	case pickup:
		return "pickup"
	default:
		return "drop-off"
	}
}

// riderTripChangedAlert is the RIDER's copy when the OWNER edits the trip
// (MYR-541). The edited PART is named, the place never is (P1 on a locked
// screen); the client refetches for the rest.
func riderTripChangedAlert(part string) alert {
	return alert{
		title: "Your " + part + " was changed",
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
