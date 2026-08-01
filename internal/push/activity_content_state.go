package push

import (
	"time"
)

// The content-state projection (MYR-172, `progress` added by MYR-398).
//
// Split out of activity_notifier_send.go, which owns the FAN-OUT: who gets a
// push and what happens when Apple refuses it. This file owns the one question
// that has nothing to do with delivery — what the card should say — and it is
// the single function both send paths go through, the lifecycle notifier and
// the ETA ticker alike.

// contentState projects a ride into what the Activity renders, and returns the
// progress anchor to persist if the push is delivered.
//
// The ETA conversion is the simpler of the two pieces of arithmetic on this
// path: the car reports a DURATION in whole minutes (Tesla's minutesToArrival,
// persisted verbatim) and the Activity needs an INSTANT, because an instant
// survives the gap between pushes and a duration does not. A negative or absent
// value means no route, and the key is omitted rather than guessed. The other
// is computeProgress, which turns "how far is left" into "how far along" and is
// documented where it lives.
func contentState(rc RideContext, prev ProgressAnchor, now time.Time) (ActivityContentState, ProgressAnchor) {
	state := ActivityContentState{
		Version:     ActivityContentStateVersion,
		Status:      rc.Status,
		VehicleName: truncateLabel(rc.VehicleName),
		Destination: truncateLabel(rc.Destination),
	}
	if rc.ETAMinutes != nil && *rc.ETAMinutes >= 0 {
		eta := now.Add(time.Duration(*rc.ETAMinutes) * time.Minute).Unix()
		state.ETA = &eta
	}
	progress, anchor := computeProgress(rc, prev, now)
	state.Progress = progress
	return state, anchor
}
