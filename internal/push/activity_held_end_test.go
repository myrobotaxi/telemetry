package push

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The held completion `end` (MYR-421).
//
// The client's measurement is the premise of every case here: an `.ended`
// Activity LEAVES THE DYNAMIC ISLAND about 1.4 seconds after the end lands.
// MYR-418's pair sent the end one second after the alerting update, so the
// sixth expansion's check was on the island for roughly two seconds and the
// rider could not find it. The end is therefore held for the linger and sent by
// a ticker pass instead.

// holdExpires walks both clocks past the hold and runs ONE ticker pass — the
// two things that have to happen for a held `end` to go out.
//
// The pass is run with `Enabled: false` on purpose rather than incidentally: the
// held end is the second half of a lifecycle transition, not an ETA refresh, and
// wiring it under the ETA kill-switch would mean an operator who turned the
// ticker off stranded every completed card on every rider's lock screen. Every
// caller of this helper is therefore also asserting that.
func holdExpires(t *testing.T, n *ActivityNotifier, store *fakeActivityStore) {
	t.Helper()
	store.advanceClock(DismissAfter)
	at := store.clock()
	n.now = func() time.Time { return at }
	NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).RunPass(context.Background())
}

// completeAndAnnounce drives a ride to `completed` and drains the announcement,
// leaving the fixture in the state the hold window starts from.
func completeAndAnnounce(t *testing.T, n *ActivityNotifier, store *fakeActivityStore) {
	t.Helper()
	store.completeRide(activityRideID, fixedNow)
	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()
}

// TestHeldEndWaitsTheWholeLinger walks the window minute by minute. Nothing may
// go out early: the gap between the announcement and the end IS the time the
// rider has to notice the check on the island.
func TestHeldEndWaitsTheWholeLinger(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	for elapsed := time.Minute; elapsed < DismissAfter; elapsed += time.Minute {
		store.advanceClock(time.Minute)
		ticker.RunPass(context.Background())
		if got := len(sender.Sent()); got != 1 {
			t.Fatalf("%s after completion: sends = %d, want 1 — the end escaped its hold", elapsed, got)
		}
	}

	store.advanceClock(time.Minute)
	ticker.RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends at the hold horizon = %d, want 2", len(sent))
	}
	if sent[1].Event != ActivityEventEnd {
		t.Errorf("second send event = %q, want end", sent[1].Event)
	}
}

// TestHeldEndHorizonIsTheDismissalConstant pins the "one number" requirement.
//
// The hold and the dismissal-date are the same five minutes — the ones the
// client also counts down locally (MYR-405) — and a second constant is how one
// of them drifts from the other without a test saying so. Asserted as a LITERAL
// for the reason TestActivityNotifierTerminalDismissal gives: written as
// DismissAfter it would only prove the ticker passes some constant along.
func TestHeldEndHorizonIsTheDismissalConstant(t *testing.T) {
	n, _, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)

	NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger()).RunPass(context.Background())

	if store.heldFor != 5*time.Minute {
		t.Errorf("held-end horizon = %s, want 5m — the client's own completed linger", store.heldFor)
	}
	if DismissAfter != 5*time.Minute {
		t.Errorf("DismissAfter = %s, want 5m; the hold and the dismissal-date are one number", DismissAfter)
	}
}

// TestHeldEndDismissalIsMeasuredFromItsOwnInstant is the arithmetic the rider
// experiences.
//
// The dismissal-date is computed from the END's send instant, not from the
// completion five minutes earlier. Measured from completion it would already be
// in the past when the end landed, which tells iOS to remove the Activity AT
// ONCE — the lock-screen card would vanish the moment the island did, and the
// linger this whole change exists to protect would be gone from both surfaces.
func TestHeldEndDismissalIsMeasuredFromItsOwnInstant(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)
	holdExpires(t, n, store)

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	end := sent[1]
	if end.DismissalDate == nil {
		t.Fatal("the held end carries no dismissal-date")
	}
	if want := fixedNow.Add(DismissAfter); !end.Timestamp.Equal(want) {
		t.Errorf("end timestamp = %s, want %s (completion + the hold)", end.Timestamp, want)
	}
	if want := end.Timestamp.Add(5 * time.Minute); !end.DismissalDate.Equal(want) {
		t.Errorf("dismissal-date = %s, want %s — five minutes past the END's instant, not past completion",
			end.DismissalDate, want)
	}
	if !end.DismissalDate.After(end.Timestamp) {
		t.Error("the dismissal-date is not in the future at send time; iOS would remove the card at once")
	}
}

// TestHeldEndSendsNoSecondAlert keeps the pair inside the once-per-phase promise
// across the five minutes between its halves.
//
// The mark was raised to 6 by the announcement, so the end finds 6 > 6 false —
// but the end must also not carry a redundant, alert-free UPDATE in front of it,
// which is what re-running the announcing path would produce.
func TestHeldEndSendsNoSecondAlert(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)
	holdExpires(t, n, store)

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want exactly 2 — one announcement and one end", len(sent))
	}
	if sent[1].Alert != nil {
		t.Errorf("the held end carries an alert (%+v)", *sent[1].Alert)
	}
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 1 {
		t.Errorf("persisted alert phases = %v, want exactly one (the announcement's)", got)
	}
}

// TestHeldEndSurvivesARestart is the crash-safety requirement, and it is why
// this is a sweep rather than a timer.
//
// A `time.AfterFunc` scheduled at completion is lost by every deploy — and the
// ride it was holding is TERMINAL, so no transition will ever fire for it again
// and the ETA ticker does not list completed rides. Nothing would ever end the
// card. The predicate here is `go_ride_requests.completed_at`, stamped by the
// status write itself, so a brand-new process recomputes the overdue set from
// the database and ends it on its first pass.
func TestHeldEndSurvivesARestart(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)

	// The process dies here. A NEW notifier and a NEW ticker come up over the
	// same rows, sharing nothing with the ones above.
	store.advanceClock(DismissAfter + time.Minute)
	restarted := NewActivityNotifier(sender, store, nil, Config{Enabled: true}, discardLogger())
	restarted.now = func() time.Time { return store.clock() }

	NewActivityTicker(restarted, store, TickerConfig{Enabled: true}, discardLogger()).
		RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends after the restart = %d, want 2 — the overdue card was stranded", len(sent))
	}
	if sent[1].Event != ActivityEventEnd {
		t.Errorf("second send event = %q, want end", sent[1].Event)
	}
	if store.endCount() != 1 {
		t.Error("the restarted process did not tombstone the rows")
	}
}

// TestHeldEndRunsWithTheEtaTickerOff is the kill-switch boundary.
//
// LIVE_ACTIVITY_TICKER_ENABLED is documented (§7.21.5) as stopping "only the
// periodic ETA refresh", with lifecycle transitions still updating the Activity.
// The held end is the second half of a lifecycle transition, so leaving it
// inside that switch would have quietly made the documented sentence false — and
// turned an operator's throttling mitigation into every completed card stranded
// until ActivityKit's multi-hour ceiling.
func TestHeldEndRunsWithTheEtaTickerOff(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	// One live leg on ANOTHER ride, so a pass that did refresh would be visible.
	store.legs = []ActivityLeg{{
		Activity:    Activity{RideRequestID: "ride_other", UserID: testRiderID, Token: "other-token"},
		RideContext: RideContext{Status: statusEnroute, ETAMinutes: intPtr(4)},
	}}
	completeAndAnnounce(t, n, store)

	store.advanceClock(DismissAfter)
	n.now = func() time.Time { return store.clock() }
	NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).
		RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends with the ETA ticker off = %d, want 2 (the announcement and the held end)", len(sent))
	}
	if sent[1].Event != ActivityEventEnd {
		t.Errorf("second send event = %q, want end — the held end must not ride the ETA kill-switch", sent[1].Event)
	}
	for _, s := range sent {
		if s.ActivityToken == "other-token" {
			t.Error("a disabled ticker refreshed an active leg; only the held end may run")
		}
	}
}

// TestCompletedRidesGetNoEtaTicksDuringTheHold pins the other half of the
// tombstone audit.
//
// The row is live for five minutes after the ride ends, and the ONLY reason
// that does not produce ETA ticks at a finished ride is the active-leg query's
// status filter — `completed` is not in ActiveLegStatuses. A tick during the
// window would re-push a non-alerting update over the arrival state and reset
// the card's staleness clock on a ride that is over.
func TestCompletedRidesGetNoEtaTicksDuringTheHold(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)

	// The active-leg list is what the ticker reads, and the completed ride is
	// absent from it exactly as the SQL leaves it absent.
	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	store.advanceClock(time.Minute)
	ticker.RunPass(context.Background())
	store.advanceClock(time.Minute)
	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 1 {
		t.Errorf("sends across two mid-window passes = %d, want 1 — a completed ride is being ticked", got)
	}
}

// TestHeldEndSkipsARowTheClientAlreadyEnded — the rider swiped the card away, or
// the app's own five-minute reaper (MYR-405) ended the Activity first. Either
// tombstones the row, and a tombstoned row must drop out of the due list rather
// than be pushed an end nobody is watching for.
func TestHeldEndSkipsARowTheClientAlreadyEnded(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)

	if _, err := store.EndActivitiesForRide(context.Background(), activityRideID); err != nil {
		t.Fatalf("EndActivitiesForRide: %v", err)
	}
	holdExpires(t, n, store)

	if got := len(sender.Sent()); got != 1 {
		t.Errorf("sends = %d, want 1 — the end was pushed at a row the client had already ended", got)
	}
}

// TestHeldEndPassSurvivesAListFailure — a database blip costs one pass, never
// the loop. The row is untouched, so the next pass finds it still overdue.
func TestHeldEndPassSurvivesAListFailure(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)
	store.heldEndErr = errors.New("pool exhausted")

	holdExpires(t, n, store)
	if got := len(sender.Sent()); got != 1 {
		t.Fatalf("sends after a failed list = %d, want 1", got)
	}
	if store.endCount() != 0 {
		t.Error("a failed list tombstoned rows it never pushed to")
	}

	store.heldEndErr = nil
	holdExpires(t, n, store)
	if got := len(sender.Sent()); got != 2 {
		t.Errorf("sends after the blip cleared = %d, want 2 — the overdue row was not retried", got)
	}
}

// TestHeldEndPassIsCapped bounds the backlog one pass can work through. After a
// long outage the due list is every ride that completed while the process was
// down, and the rest are re-listed on the next pass.
func TestHeldEndPassIsCapped(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	for i := range 3 {
		ride := string(rune('a'+i)) + "_done"
		store.byRide[ride] = []Activity{{RideRequestID: ride, UserID: testRiderID, Token: ride + "_token"}}
		store.context[ride] = RideContext{Status: statusCompleted, VehicleName: "Blue Whale", Destination: "Home"}
		store.completeRide(ride, fixedNow.Add(time.Duration(i)*time.Second))
	}
	// Past the hold for all three, which is the backlog shape: a process that
	// was down while several rides completed.
	store.advanceClock(DismissAfter + time.Minute)
	n.now = func() time.Time { return store.clock() }

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger())
	ticker.cfg.MaxPerPass = 2
	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 2 {
		t.Fatalf("sends in a capped pass = %d, want 2", got)
	}
	// Oldest completion first: the cap sheds the LEAST overdue.
	if got := sender.Sent()[0].ActivityToken; got != "a_done_token" {
		t.Errorf("first end went to %s, want the oldest completion", got)
	}

	ticker.RunPass(context.Background())
	if got := len(sender.Sent()); got != 3 {
		t.Errorf("sends after the second pass = %d, want 3 — the shed ride was not re-listed", got)
	}
}

// TestUnhappyEndingsAreNotHeld is the negative half of the change, stated once
// across every ending that is not a completion.
//
// They keep their IMMEDIATE end and their thirty seconds. Holding them would be
// the opposite of what they are for: the news is the ending itself, and a card
// that keeps promising a car for five minutes after the ride was cancelled is
// the defect this surface pushes on every transition to avoid.
func TestUnhappyEndingsAreNotHeld(t *testing.T) {
	for _, status := range []string{statusDeclined, "cancelled"} {
		t.Run(status, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.context[activityRideID] = RideContext{Status: status, VehicleName: "Blue Whale"}

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID, RiderID: testRiderID, Status: status,
			}))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 || sent[0].Event != ActivityEventEnd {
				t.Fatalf("sends = %+v, want one immediate end", sent)
			}
			if sent[0].DismissalDate == nil || !sent[0].DismissalDate.Equal(fixedNow.Add(30*time.Second)) {
				t.Errorf("dismissal-date = %v, want 30s past the end", sent[0].DismissalDate)
			}
			if store.endCount() != 1 {
				t.Error("an unhappy ending did not tombstone at once")
			}

			// And nothing is left for the sweep to find.
			holdExpires(t, n, store)
			if got := len(sender.Sent()); got != 1 {
				t.Errorf("sends after a pass = %d, want 1 — an unhappy ending was held", got)
			}
		})
	}
}

// TestReservationExpiryIsNotHeld covers the ending that never rides the event
// bus: the sweeper resolves it into the dispatch columns and leaves the ride at
// `accepted`, so it can never appear in the held-end query — and it must not
// need to.
func TestReservationExpiryIsNotHeld(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)

	n.EndForReservationExpiry(context.Background(), activityRideID)

	sent := sender.Sent()
	if len(sent) != 1 || sent[0].Event != ActivityEventEnd {
		t.Fatalf("sends = %+v, want one immediate end", sent)
	}
	if store.endCount() != 1 {
		t.Error("a lapsed reservation did not tombstone at once")
	}
}
