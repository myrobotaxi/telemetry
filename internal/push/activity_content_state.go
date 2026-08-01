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
	if etaKnown(rc) {
		eta := now.Add(time.Duration(*rc.ETAMinutes) * time.Minute).Unix()
		state.ETA = &eta
	}
	progress, anchor := computeProgress(rc, prev, now)
	state.Progress = progress
	stamp := asOf(anchor, now).Unix()
	state.AsOf = &stamp
	return state, anchor
}

// etaKnown reports whether this ride has an arrival time worth putting on the
// card.
//
// The nil / negative arms are the original rule: no active nav route means no
// key, because there is no route solver in this service and the alternative is
// an invented number.
//
// THE `requested` ARM IS NEW WITH THE v3 CARD and closes the same defect
// legUnderway closes for the track. The v3 design starts the Activity at
// REQUEST, so a ride awaiting the owner's accept now has a card — and the car
// the projection reads navigation from is the OWNER'S car, which at that moment
// has been assigned to nobody and may well be driving the owner somewhere else
// with navigation on. "Finding your ride" beside an arrival time computed from
// that errand is not a degraded ETA, it is a fabricated one. Nobody is on their
// way yet, so there is no time to show.
//
// It is scoped narrowly to `requested` on purpose. The wider question — whether
// a STALE `eta` should be withheld on the statuses that do have a car assigned —
// is the open one §7.21.3 names and still belongs to its own issue; this arm is
// about a leg that has no driver rather than about a reading that has no age.
func etaKnown(rc RideContext) bool {
	if rc.Status == statusRequested {
		return false
	}
	return rc.ETAMinutes != nil && *rc.ETAMinutes >= 0
}

// asOf is when the data this content-state was built from was last TRUE, which
// is not always when it was built.
//
// One rule covers every branch: if the push rests on a NAV READING, the honest
// instant is when that reading was first seen to hold; otherwise it rests on
// the ride record, which just asserted something, and the honest instant is
// now.
//
// The anchor is what distinguishes the two, and it already carries exactly the
// right stamp because noteReading moves ReadingAt only when the reading CHANGES
// (activity_progress_input.go). So:
//
//   - A HELD fraction — the car went quiet, or cleared its route, or its
//     reading stalled behind a busy row stamp — returns the anchor unchanged,
//     and its ReadingAt is the old instant. That is the case the whole field
//     exists for, and the answer is that asOf STAYS PUT while `aps.timestamp`
//     and the stale-date move on without it.
//   - A car reporting the same distance at a red light likewise keeps its
//     earlier ReadingAt, which is correct rather than merely tolerable: the
//     card is not stale, so nothing renders asOf, and the instant it would
//     render is honestly "when the car last told us something new".
//   - Every branch that clears or never builds an anchor — `arrived`,
//     `completed`, a terminal status, a dormant reservation, `requested`, a leg
//     with no telemetry at all — leaves ReadingAt zero and falls through to
//     now. Each of those is the RIDE RECORD speaking, and the ride record is
//     speaking as of this instant.
//
// The one thing it must never do is exceed `now`, and it cannot: ReadingAt is
// only ever set from a `now` of an earlier or equal pass.
func asOf(anchor ProgressAnchor, now time.Time) time.Time {
	if anchor.ReadingAt.IsZero() {
		return now
	}
	return anchor.ReadingAt
}
