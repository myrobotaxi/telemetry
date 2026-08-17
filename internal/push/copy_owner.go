package push

import "github.com/myrobotaxi/telemetry/internal/events"

// The OWNER's side of the notification ladder. Split out of copy.go by MYR-585
// alongside copy_rider.go; see that file's header for the division.
//
// The owner's audience has NO member fan-out and never will: a member is a
// passenger, and a ride has exactly one owner.
//
// Every string here obeys the payload policy in copy.go — a requester's FIRST
// NAME is the only interpolation about a person, and nothing else about the
// ride travels.

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
// The cancel fork is keyed on the CLASSIFICATION rather than on a second
// reading of the status string, so the two cannot disagree about which
// sentence a given event deserves.
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
		// The anonymous voice every owner-facing fallback here uses. The
		// incoming-request card's own fallback ("New ride request") is a NOUN —
		// it names a thing that arrived, and cannot be the subject of a sentence
		// about that thing being taken back.
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

// ownerDueAlert is the OWNER's copy for a dispatched reservation (MYR-535,
// client decision 2026-08-12): the route just landed on the car's dash, and the
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

// ownerTripChangedAlert is the OWNER's copy when the RIDER edits the trip
// (MYR-541).
func ownerTripChangedAlert(requesterName *string, part string) alert {
	if name := displayName(requesterName); name != "" {
		return alert{title: name + " changed the " + part, body: bodySeeDetails}
	}
	return alert{title: "Your rider changed the " + part, body: bodySeeDetails}
}
