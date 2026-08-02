package push

import (
	"context"
	"errors"
	"sync"
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

// TestHeldEndIsNotSingleShot is the retry rule, and it is the one that keeps
// the crash-safety argument true for the second half of a pass as well as the
// first.
//
// The tombstone is the receipt for a DELIVERED end, not for having tried. A row
// ended after a failed push drops out of the due list forever — the ride is
// terminal, so nothing else will ever look at it — and because this path is a
// BATCH, one 30-second APNs blip overlapping a pass would do that to every card
// in the pass at once.
func TestHeldEndIsNotSingleShot(t *testing.T) {
	tests := []struct {
		name string
		fail func(sender *FakeActivitySender, store *fakeActivityStore)
		heal func(sender *FakeActivitySender, store *fakeActivityStore)
	}{
		{
			// The ride-context read is the first thing endRide does, and it
			// used to tombstone on failure with the comment "the ride is over
			// whatever the read said" — true, and irrelevant: nothing had been
			// sent.
			name: "the ride-context read fails",
			fail: func(_ *FakeActivitySender, store *fakeActivityStore) {
				store.contextErr = errors.New("pool exhausted")
			},
			heal: func(_ *FakeActivitySender, store *fakeActivityStore) {
				store.contextErr = nil
			},
		},
		{
			// An APNs 5xx or a timeout. fanOut is void no longer: it reports
			// the rows still owed the push, and endRide refuses to tombstone
			// while any remain.
			name: "the end push fails",
			fail: func(sender *FakeActivitySender, _ *fakeActivityStore) {
				sender.Err = errors.New("apns 503")
			},
			heal: func(sender *FakeActivitySender, _ *fakeActivityStore) {
				sender.Err = nil
			},
		},
		{
			// The registry read inside fanOut. The rows exist and were never
			// reached, so "nothing owed" would be a lie told by a failed read.
			name: "the registry read fails",
			fail: func(_ *FakeActivitySender, store *fakeActivityStore) {
				store.listErr = errors.New("db blip")
			},
			heal: func(_ *FakeActivitySender, store *fakeActivityStore) {
				store.listErr = nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			completeAndAnnounce(t, n, store)

			tt.fail(sender, store)
			holdExpires(t, n, store)

			if store.endCount() != 0 {
				t.Fatal("the rows were tombstoned after a failed end; the card is stranded and " +
					"nothing will ever look at this ride again")
			}
			// The spy records refused attempts too, so the baseline is taken
			// after the failure rather than before it.
			attempted := len(sender.Sent())

			// The next pass retries and succeeds. The end push is idempotent,
			// so a retry costs nothing even if the first attempt did land.
			tt.heal(sender, store)
			holdExpires(t, n, store)

			sent := sender.Sent()
			if len(sent) != attempted+1 {
				t.Fatalf("sends after the retry = %d, want %d — the overdue row was not re-listed",
					len(sent), attempted+1)
			}
			if sent[len(sent)-1].Event != ActivityEventEnd {
				t.Errorf("last send event = %q, want end", sent[len(sent)-1].Event)
			}
			if store.endCount() != 1 {
				t.Error("the successful retry did not tombstone")
			}
		})
	}
}

// TestHeldEndTombstonesWhenNobodyIsOwedAPush guards the other side of the retry
// rule, which is the one that would leak if it were written as "tombstone only
// on a delivery".
//
// A rider who muted ride updates is a NORMAL, PERMANENT state: no push is sent,
// and there is nothing to retry. Counted as a failure, that ride would be
// re-listed and re-attempted every 75 seconds for as long as the row survived.
func TestHeldEndTombstonesWhenNobodyIsOwedAPush(t *testing.T) {
	prefs := newFakePrefStore()
	muted := DefaultPrefs()
	muted.RideLifecycle = false
	prefs.byUser[testRiderID] = muted

	n, sender, store := newTestActivityNotifier(t, prefs)
	store.completeRide(activityRideID, fixedNow)
	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()
	holdExpires(t, n, store)

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d pushes to a muted rider, want 0", got)
	}
	if store.endCount() != 1 {
		t.Error("a muted rider's completed ride was not tombstoned; it would be retried forever")
	}
}

// TestHeldEndPassStopsOnShutdown — SIGTERM mid-batch.
//
// The tombstone deliberately runs on a context.WithoutCancel, so a loop that
// ploughed on through a cancelled context would keep tombstoning rides whose
// pushes were failing precisely because the context was cancelled. The check is
// at the TOP of each iteration rather than after the send, so a process on its
// way out does not spend a doomed APNs round-trip per remaining ride.
func TestHeldEndPassStopsOnShutdown(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	for i := range 3 {
		ride := string(rune('a'+i)) + "_done"
		store.byRide[ride] = []Activity{{RideRequestID: ride, UserID: testRiderID, Token: ride + "_token"}}
		store.context[ride] = RideContext{Status: statusCompleted, VehicleName: "Blue Whale"}
		store.completeRide(ride, fixedNow)
	}
	store.advanceClock(DismissAfter + time.Minute)
	n.now = func() time.Time { return store.clock() }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).RunPass(ctx)

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d pushes on a cancelled context, want 0", got)
	}
	if got := len(store.ended); got != 0 {
		t.Errorf("tombstoned %d rides during shutdown, want 0 — the next process must find them", got)
	}
}

// TestHeldEndIsCoveredByTheDrain is MYR-421's shutdown hole.
//
// endHeldCompletion runs on the TICKER's goroutine and never passes through
// async, so until it was counted on the same inflight/Cond drain, Wait()
// returned while its pushes — and the progress and phase-mark writes behind
// them — were still going. The symptom at shutdown is a dropped end and a
// half-written row; under `-race` it is a reported race.
//
// Asserted by blocking the sender inside the pass and proving Wait() does not
// return until the send is released.
func TestHeldEndIsCoveredByTheDrain(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)
	store.advanceClock(DismissAfter)
	n.now = func() time.Time { return store.clock() }

	release := make(chan struct{})
	entered := make(chan struct{})
	sender.Before = func() {
		close(entered)
		<-release
	}

	passDone := make(chan struct{})
	go func() {
		NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).
			RunPass(context.Background())
		close(passDone)
	}()
	<-entered

	waitReturned := make(chan struct{})
	go func() {
		n.Wait()
		close(waitReturned)
	}()

	select {
	case <-waitReturned:
		t.Fatal("Wait() returned while a held-end push was still in flight; the drain does not " +
			"cover the ticker's sweep and shutdown can drop the end")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after the send completed")
	}
	<-passDone
}

// seedCompletedRide adds one already-announced completed ride to the fixture.
func seedCompletedRide(store *fakeActivityStore, ride string, at time.Time) {
	store.byRide[ride] = []Activity{{
		RideRequestID: ride, UserID: testRiderID, Token: ride + "_token",
		AlertedPhase: AlertPhaseCompleted,
	}}
	store.context[ride] = RideContext{Status: statusCompleted, VehicleName: "Blue Whale", Destination: "Home"}
	store.completeRide(ride, at)
}

// TestHeldEndPassIsCapped bounds the backlog one pass can work through, and
// pins the ordering the cap sheds by.
//
// NEWEST COMPLETION FIRST, which is a reversal of what this shipped with.
// Oldest-first was sound while the list drained on every pass and became a
// starvation bug the moment a failed end started leaving its row in place: a
// ride that cannot be delivered to sits at the head of every subsequent pass
// and consumes a slot forever. Newest-first sheds the rides whose end matters
// least — the oldest, whose card is stalest and which the horizon is about to
// retire anyway — and a healthy backlog drains in the same number of passes
// either way, because the same number leaves per pass regardless of order.
func TestHeldEndPassIsCapped(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	for i := range 3 {
		seedCompletedRide(store, string(rune('a'+i))+"_done", fixedNow.Add(time.Duration(i)*time.Second))
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
	if got := sender.Sent()[0].ActivityToken; got != "c_done_token" {
		t.Errorf("first end went to %s, want the NEWEST completion — a capped pass must not let "+
			"the oldest rides hold the head of the queue", got)
	}

	ticker.RunPass(context.Background())
	if got := len(sender.Sent()); got != 3 {
		t.Errorf("sends after the second pass = %d, want 3 — the shed ride was not re-listed", got)
	}
}

// TestHeldEndGivesUpAtTheHorizon is the retry's TERMINATOR, and without it the
// retry rule is a permanent resident rather than a retry.
//
// Only 410 and 400 take a row out of the due list. A 403 TopicDisallowed from a
// misconfigured APNS_TOPIC, a 404, a timeout — all come back retryable, so an
// uncapped retry re-attempts the same row every 75 seconds until the 24-hour
// registration sweep DELETEs it: ~1,150 doomed pushes and no `end` ever sent.
// Past the horizon the card is abandoned honestly instead: no push, a tombstone,
// and a loud log.
func TestHeldEndGivesUpAtTheHorizon(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	completeAndAnnounce(t, n, store)
	sender.Err = errors.New("apns 403 TopicDisallowed")

	// Every pass inside the window retries and leaves the row live.
	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger())
	for elapsed := DismissAfter; elapsed < heldEndGiveUpAfter; elapsed += 5 * time.Minute {
		store.advanceClock(5 * time.Minute)
		n.now = func() time.Time { return store.clock() }
		ticker.RunPass(context.Background())
		if store.endCount() != 0 {
			t.Fatalf("%s after completion: the row was retired early", elapsed)
		}
	}
	attempted := len(sender.Sent())
	if attempted < 2 {
		t.Fatalf("only %d attempts across the whole window; the retry is not retrying", attempted)
	}

	// Past it, the bulk terminator retires the card — and sends nothing.
	store.advanceClock(5 * time.Minute)
	n.now = func() time.Time { return store.clock() }
	ticker.RunPass(context.Background())

	if got := store.abandonedRides(); len(got) != 1 || got[0] != activityRideID {
		t.Errorf("abandoned = %v, want [%s]", got, activityRideID)
	}
	if store.endCount() != 1 {
		t.Error("the expired card was not tombstoned; the 24h sweep is still the terminator")
	}
	if got := len(sender.Sent()); got != attempted {
		t.Errorf("sends = %d, want %d — giving up must push nothing", got, attempted)
	}

	// And it is gone for good: a further pass finds nothing at all.
	store.advanceClock(5 * time.Minute)
	n.now = func() time.Time { return store.clock() }
	ticker.RunPass(context.Background())
	if got := len(sender.Sent()); got != attempted {
		t.Errorf("sends after the horizon = %d, want %d — the loop did not terminate", got, attempted)
	}
}

// TestHeldEndHorizonBracketsTheHold pins the two constants against each other.
//
// A horizon at or below the hold is an empty window, which returns nothing
// forever and reads in production as "the held end simply stopped working".
// Literal numbers rather than the constants, so moving either one has to be
// deliberate.
func TestHeldEndHorizonBracketsTheHold(t *testing.T) {
	if DismissAfter != 5*time.Minute {
		t.Errorf("hold = %s, want 5m", DismissAfter)
	}
	if heldEndGiveUpAfter != 30*time.Minute {
		t.Errorf("give-up horizon = %s, want 30m", heldEndGiveUpAfter)
	}
	if heldEndGiveUpAfter <= DismissAfter {
		t.Fatal("the horizon does not exceed the hold; the retry window is empty")
	}
}

// TestStuckRidesDoNotBlockANewCompletion is the head-of-line guarantee, and it
// is the failure the retry rule would otherwise have introduced.
//
// A full cap's worth of undeliverable rides — trivial to produce with one bad
// APNS_TOPIC — used to sit at the head of every pass under oldest-first
// ordering, so a ride that completed a moment ago would not be reached until
// the poison aged out. Here the poison is exactly MaxPerPass rides, all inside
// their horizon, all failing; a newly completed ride must still get its end.
func TestStuckRidesDoNotBlockANewCompletion(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	const stuck = 4
	for i := range stuck {
		seedCompletedRide(store, string(rune('a'+i))+"_stuck", fixedNow)
	}
	store.advanceClock(DismissAfter + time.Minute)
	n.now = func() time.Time { return store.clock() }

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger())
	ticker.cfg.MaxPerPass = stuck

	// A pass in which every send fails: the rows stay live, as designed.
	sender.Err = errors.New("apns 503")
	ticker.RunPass(context.Background())
	if len(store.ended) != 0 {
		t.Fatalf("stuck rides were retired: %v", store.ended)
	}

	// A ride completes now and comes due five minutes later. Apple is healthy
	// again for it — but the four stuck rides are still there and still fill
	// the cap.
	sender.Err = nil
	seedCompletedRide(store, "fresh_ride", store.clock())
	store.advanceClock(DismissAfter)
	n.now = func() time.Time { return store.clock() }
	sender.Reset()

	ticker.RunPass(context.Background())

	var reached bool
	for _, s := range sender.Sent() {
		if s.ActivityToken == "fresh_ride_token" && s.Event == ActivityEventEnd {
			reached = true
		}
	}
	if !reached {
		t.Errorf("the newly completed ride was not reached in a pass full of stuck ones "+
			"(sent %d, cap %d) — the stuck rides are holding the head of the queue",
			len(sender.Sent()), stuck)
	}
}

// TestHeldEndPassYieldsOnItsTimeBudget — a saturated batch must not starve the
// ETA refresh.
//
// RunPass runs the refresh and then the held ends serially on one goroutine, so
// a batch that runs long does not delay the refresh it already ran: it delays
// every subsequent one, by keeping the loop away from its timer. At MaxPerPass
// with each send bounded by heldEndSendTimeout that is tens of minutes with no
// ETA refresh for any live ride.
//
// The clock is advanced by the SENDER rather than per call, so the arithmetic is
// exact: budget is half of a 60s interval (30s), each send costs 20s, so rides
// one and two start inside the budget and the third finds it spent.
func TestHeldEndPassYieldsOnItsTimeBudget(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	for i := range 3 {
		seedCompletedRide(store, string(rune('a'+i))+"_slow", fixedNow.Add(time.Duration(i)*time.Second))
	}
	store.advanceClock(DismissAfter + time.Minute)

	clock := store.clock()
	var mu sync.Mutex
	n.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	sender.Before = func() {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(20 * time.Second)
	}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger())
	ticker.cfg.Interval = time.Minute // budget = 30s
	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 2 {
		t.Fatalf("sends in one pass = %d, want 2 — the pass spent %s of a 30s budget",
			got, time.Duration(got)*20*time.Second)
	}
	// The deferred ride is not lost, only deferred.
	sender.Before = nil
	ticker.RunPass(context.Background())
	if got := len(sender.Sent()); got != 3 {
		t.Errorf("sends after the next pass = %d, want 3 — the deferred ride was dropped", got)
	}
}

// deadlineSpySender records how much time each send was actually given.
//
// Every send on this surface already runs under SOME deadline — async gives a
// fan-out the notifier's 30-second Timeout — so the question is not whether a
// deadline exists but how tight it is. The held-end pass must impose a much
// shorter one of its own.
type deadlineSpySender struct {
	mu      sync.Mutex
	budgets []time.Duration
}

func (s *deadlineSpySender) SendActivity(ctx context.Context, _ ActivityNotification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		s.budgets = append(s.budgets, time.Until(deadline))
	} else {
		s.budgets = append(s.budgets, 0)
	}
	return nil
}

func (s *deadlineSpySender) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.budgets...)
}

// TestHeldEndSendCarriesItsOwnDeadline — one unreachable ride may not own a
// quarter of the tick.
//
// The APNs client allows 10s per attempt and retries once, so an unbounded ride
// holds the loop for ~20s by itself; the deadline cuts the inline retry short
// because the PASS is already the retry and a pass retry costs the loop nothing.
// Safe only because both mark writes and the tombstone run on detached
// contexts — asserted here by the tombstone landing despite the deadline.
func TestHeldEndSendCarriesItsOwnDeadline(t *testing.T) {
	_, _, store := newTestActivityNotifier(t, nil)
	spy := &deadlineSpySender{}
	n := NewActivityNotifier(spy, store, nil, Config{Enabled: true}, discardLogger())
	n.now = func() time.Time { return fixedNow }

	completeAndAnnounce(t, n, store)

	store.advanceClock(DismissAfter)
	n.now = func() time.Time { return store.clock() }
	NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).
		RunPass(context.Background())

	budgets := spy.recorded()
	if len(budgets) != 2 {
		t.Fatalf("sends = %d, want 2 (the announcement and the held end)", len(budgets))
	}
	// The announcement runs on the fan-out worker's own 30-second timeout.
	if budgets[0] <= heldEndSendTimeout {
		t.Errorf("the announcement was given %s, want more than the sweep's %s — the per-ride "+
			"deadline must be the SWEEP's, not a new bound on every lifecycle push",
			budgets[0], heldEndSendTimeout)
	}
	// The held end is capped, so one unreachable ride cannot own a quarter of
	// the tick by exhausting the APNs client's inline retry.
	if budgets[1] <= 0 || budgets[1] > heldEndSendTimeout {
		t.Errorf("the held end was given %s, want (0, %s]", budgets[1], heldEndSendTimeout)
	}
	// The tombstone still lands: it runs on a context detached from this
	// deadline, which is what makes the deadline safe rather than merely tight.
	if store.endCount() != 1 {
		t.Error("the tombstone did not land; the per-ride deadline is reaching writes it must not")
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
