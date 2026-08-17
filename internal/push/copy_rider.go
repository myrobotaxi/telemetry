package push

// The RIDER's side of the notification ladder (MYR-186). Split out of copy.go
// by MYR-585, when the file crossed the 300-line ceiling and the audiences had
// grown far enough apart to be read separately: this file answers "what does
// the passenger need to know?", copy_owner.go answers "what does the person
// whose car it is need to know?", and copy.go holds what they share.
//
// The payload policy in copy.go governs every string here.
//
// The group-ride members receive these alerts too, verbatim (MYR-540): a member
// is a passenger, and forking the copy would mean inventing a second voice for
// one fact. That fan-out lives in notifier_members.go; nothing here knows about
// it, which is exactly why the two audiences cannot drift.

// ownerCancelled reports whether this event is an OWNER's cancellation — the
// one `cancelled` transition worth waking the RIDER for (MYR-522). The rider's
// own cancel is their own action; it speaks to the owner instead, through
// riderCancelled in copy_owner.go.
func ownerCancelled(status string, cancelledBy *string) bool {
	return status == statusCancelled && cancelledBy != nil && *cancelledBy == cancelledByOwner
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
func statusAlert(status, vehicleName string, scheduled, byOwnerCancel bool) (alert, bool) {
	switch status {
	case statusAccepted:
		return alert{title: "Your ride is confirmed", body: bodySeeDetails}, true
	case statusDeclined:
		if scheduled {
			return alert{
				title: vehicleLabel(vehicleName) + " can't make your scheduled ride",
				body:  bodyTryAnotherCar,
			}, true
		}
		return alert{
			title: vehicleLabel(vehicleName) + " can't take this ride",
			body:  bodyTryAnotherCar,
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
				body:  bodyTryAnotherCar,
			}, true
		}
		return alert{
			title: vehicleLabel(vehicleName) + " had to cancel your ride",
			body:  bodyTryAnotherCar,
		}, true
	default:
		return alert{}, false
	}
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
