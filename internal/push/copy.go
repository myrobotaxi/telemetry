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
// (`requested`, `enroute`, `completed`, `cancelled`) is either the rider's own
// action or invisible to them, so it sends nothing.
const (
	statusAccepted = "accepted"
	statusDeclined = "declined"
	statusArrived  = "arrived"
)

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
func statusAlert(status, vehicleName string, scheduled bool) (alert, bool) {
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
	default:
		return alert{}, false
	}
}

// dueAlert is the RIDER's copy for a scheduled ride whose pickup nav has
// reached the car.
func dueAlert(vehicleName string) alert {
	return alert{
		title: vehicleLabel(vehicleName) + " is heading your way",
		body:  "Your scheduled ride is starting now.",
	}
}

// Shared bodies.
const (
	bodyReviewRequest = "Open MyRoboTaxi to accept or decline."
	bodySeeDetails    = "Open MyRoboTaxi to see the details."
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
