package push

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The island auto-expand ladder (MYR-398, the v3 card).
//
// The design's failure mode is not a missed expansion, it is an island that
// opens every 75 seconds — "do not alert on ETA ticks or the stale flip, or the
// island strobes". So most of what is asserted here is that alerts DON'T fire.

// TestAlertPhaseForMapsTheDesignsSixPhases pins the ladder itself, including
// arrivingDispatch and acceptDispatch are the two dispatch instants the MYR-409
// relevance gate is asked about (push.navReadingIsOurs).
//
// A genuine Arriving needs a nav reading dated AFTER the server sent the car to
// the rider, so arrivingDispatch sits an hour before fixedNow — comfortably
// before every "fresh" reading in this file, including the ones in tests that
// wind `now` forward mid-test.
//
// acceptDispatch is fixedNow itself: on the accept push the dispatch has only
// just happened, so any reading already in hand necessarily predates it. That is
// the production shape of the burnt-rung defect, and stating it as a value keeps
// the ladder test honest about which arm is doing the refusing.
var (
	arrivingDispatch = fixedNow.Add(-time.Hour)
	acceptDispatch   = fixedNow
)

// the two things a reader would otherwise have to take on trust: that Arriving
// outranks Enroute on the same status, and that the unhappy endings are outside
// the six.
func TestAlertPhaseForMapsTheDesignsSixPhases(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	stale := fixedNow.Add(-ProgressFreshFor - time.Second)
	// dispatched precedes `fresh`, so a reading taken at `fresh` is about THIS
	// ride; afterFresh follows it, so the same reading is about the previous one.
	dispatched := fixedNow.Add(-2 * time.Minute)
	afterFresh := fixedNow.Add(-10 * time.Second)

	tests := []struct {
		name string
		rc   RideContext
		want AlertPhase
	}{
		{"dispatch", RideContext{Status: statusRequested}, AlertPhaseDispatch},
		{
			"enroute, car far away",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(9), NavUpdatedAt: &fresh, DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			"enroute, no eta at all",
			RideContext{Status: statusAccepted, NavUpdatedAt: &fresh, DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			"arriving at the threshold",
			RideContext{
				Status: statusAccepted, ETAMinutes: intPtr(ArrivingWithinMinutes),
				NavUpdatedAt: &fresh, PickupDispatchedAt: &dispatched, DispatchUnderway: true,
			},
			AlertPhaseArriving,
		},
		{
			"arriving under the threshold",
			RideContext{
				Status: statusAccepted, ETAMinutes: intPtr(1),
				NavUpdatedAt: &fresh, PickupDispatchedAt: &dispatched, DispatchUnderway: true,
			},
			AlertPhaseArriving,
		},
		{
			// THE DEFECT MYR-409 CLOSES, and the one a freshness gate alone
			// cannot. This reading is thirty seconds old — genuinely fresh —
			// but it PREDATES the dispatch, so it describes wherever the
			// owner's car was already going. Recency is not relevance.
			"a fresh reading that predates the dispatch never reaches Arriving",
			RideContext{
				Status: statusAccepted, ETAMinutes: intPtr(1),
				NavUpdatedAt: &fresh, PickupDispatchedAt: &afterFresh, DispatchUnderway: true,
			},
			AlertPhaseEnroute,
		},
		{
			// Dispatch failed or was skipped: the car was never sent to this
			// rider, so nothing it is navigating can be theirs. DispatchUnderway
			// is true (an instant ride has no scheduled_for), so this gate is
			// the only one that can refuse.
			"an undispatched leg never reaches Arriving",
			RideContext{
				Status: statusAccepted, ETAMinutes: intPtr(1),
				NavUpdatedAt: &fresh, DispatchUnderway: true,
			},
			AlertPhaseEnroute,
		},
		{
			"one minute above the threshold is still Enroute",
			RideContext{
				Status: statusAccepted, ETAMinutes: intPtr(ArrivingWithinMinutes + 1),
				NavUpdatedAt: &fresh, DispatchUnderway: true,
			},
			AlertPhaseEnroute,
		},
		{
			// THE DEFECT THE GATE CLOSES. A reservation accepted for tomorrow
			// reads `accepted` today, and the car the projection reads is the
			// OWNER'S, mid-errand. Without DispatchUnderway an owner parking
			// two minutes from home would tell the rider their ride was almost
			// here, a day early, with a notification attached.
			"dormant reservation never reaches Arriving",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(1), NavUpdatedAt: &fresh, DispatchUnderway: false},
			AlertPhaseEnroute,
		},
		{
			// THE DEFECT THE FRESHNESS GATE CLOSES, and note that
			// DispatchUnderway is TRUE here: for an instant ride
			// `scheduled_for IS NULL` makes it unconditionally true, so the
			// dormancy gate contributes nothing on this path. The reading is a
			// leftover from the car's previous trip.
			"a stale reading never reaches Arriving",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(1), NavUpdatedAt: &stale, DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			// A car we have never heard from is exactly the case the guard is
			// for; an absent stamp is not fresh.
			"an undated reading never reaches Arriving",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(1), DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{"arrived", RideContext{Status: statusArrived}, AlertPhaseArrived},
		{"on trip", RideContext{Status: statusEnroute}, AlertPhaseOnTrip},
		{"completed", RideContext{Status: statusCompleted}, AlertPhaseCompleted},
		{"declined is outside the six", RideContext{Status: statusDeclined}, AlertPhaseNone},
		{"cancelled is outside the six", RideContext{Status: "cancelled"}, AlertPhaseNone},
		{"reservation_expired is outside the six", RideContext{Status: "reservation_expired"}, AlertPhaseNone},
		{"an unrecognised status alerts nothing", RideContext{Status: "teleported"}, AlertPhaseNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := alertPhaseFor(tc.rc, fixedNow); got != tc.want {
				t.Errorf("alertPhaseFor(%+v) = %d, want %d", tc.rc, got, tc.want)
			}
		})
	}
}

// TestAlertLadderIsStrictlyAscending guards the one property every other
// guarantee here rests on. The ordinals are persisted, so a renumbering that
// left the ride's real order unsorted would silently swallow an expansion (a
// phase below the mark) or re-open the island on a phase already shown.
func TestAlertLadderIsStrictlyAscending(t *testing.T) {
	ladder := []AlertPhase{
		AlertPhaseNone,
		AlertPhaseDispatch,
		AlertPhaseEnroute,
		AlertPhaseArriving,
		AlertPhaseArrived,
		AlertPhaseOnTrip,
		AlertPhaseCompleted,
	}
	for i := 1; i < len(ladder); i++ {
		if ladder[i] <= ladder[i-1] {
			t.Fatalf("ladder position %d (%d) does not exceed its predecessor (%d)",
				i, ladder[i], ladder[i-1])
		}
	}
	if AlertPhaseNone != 0 {
		t.Errorf("AlertPhaseNone = %d, want 0 — it is the zero value a never-alerted row carries",
			AlertPhaseNone)
	}
	// Every real phase has copy, and AlertPhaseNone deliberately has none.
	if _, ok := alertBodies[AlertPhaseNone]; ok {
		t.Error("AlertPhaseNone has alert copy; nothing may alert on it")
	}
	for _, p := range ladder[1:] {
		if body := alertBodies[p]; body == "" {
			t.Errorf("phase %d has no alert body", p)
		}
	}

	// THE TOP OF THE LADDER IS A STRUCTURAL ASSUMPTION SINCE MYR-418, not
	// merely a fact about today's table. Completed is the only rung that
	// coincides with the Activity ending, and endRide handles it by sending its
	// expansion as an UPDATE and then the alert-free `end` — after which the
	// rows are tombstoned and no push can ever follow. A seventh rung above
	// Completed would therefore be unreachable by construction, silently: there
	// is no send path left to carry it. If this line fails, the new phase needs
	// a home before it needs copy.
	for phase := range alertBodies {
		if phase > AlertPhaseCompleted {
			t.Errorf("phase %d sits above Completed(%d); nothing pushes after the `end`, so it can "+
				"never be delivered — see endRide", phase, AlertPhaseCompleted)
		}
	}
	// The ladder above must enumerate every phase that has copy, or a rung
	// could be added and skipped by both this guard and the ascending check.
	if len(alertBodies) != len(ladder)-1 {
		t.Errorf("alertBodies has %d phases and the ladder enumerates %d; the drift guard is "+
			"only as good as the list it walks", len(alertBodies), len(ladder)-1)
	}
}

// TestAlertCopyNamesNoPlaceOrPerson is the payload-policy assertion. An alert
// dictionary renders as an ordinary banner on a watch and on a device with no
// Dynamic Island — a locked screen, to whoever is holding it — so copy.go's
// rules bind it, not the narrow content-state exception that lets `destination`
// through.
func TestAlertCopyNamesNoPlaceOrPerson(t *testing.T) {
	// The values the rest of this package's fixtures use for the P1 label and
	// the vehicle nickname. Neither may appear in any alert string.
	forbidden := []string{"Home", "Blue Whale", "Mission", "Duarte"}

	for phase, body := range alertBodies {
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("phase %d body %q contains %q; the alert is a lock-screen banner "+
					"and may name no place, person or vehicle", phase, body, bad)
			}
		}
		// The v3 copy rules retire "Arriving" from rider-facing text — it is
		// an engineering phase name, and it was the word the field report was
		// about.
		if strings.Contains(strings.ToLower(body), "arriving") {
			t.Errorf("phase %d body %q says \"arriving\"; that word is a phase name, never copy", phase, body)
		}
	}
	if alertTitle != "Your ride" {
		t.Errorf("alertTitle = %q, want the generic subject line", alertTitle)
	}
}

// TestAlertForRefusesAPhaseAlreadyShown is the once-per-phase rule in
// isolation, including the out-of-order case a `>` buys and an `==` would not.
func TestAlertForRefusesAPhaseAlreadyShown(t *testing.T) {
	tests := []struct {
		name          string
		phase, marked AlertPhase
		wantAlert     bool
	}{
		{"first phase above the mark alerts", AlertPhaseEnroute, AlertPhaseDispatch, true},
		{"the same phase again does not", AlertPhaseEnroute, AlertPhaseEnroute, false},
		{"a phase below the mark does not", AlertPhaseEnroute, AlertPhaseArrived, false},
		{"a skipped phase still alerts on arrival", AlertPhaseArriving, AlertPhaseDispatch, true},
		{"no phase never alerts", AlertPhaseNone, AlertPhaseNone, false},
		{"a fresh row alerts on its first real phase", AlertPhaseDispatch, AlertPhaseNone, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := alertFor(tc.phase, tc.marked)
			if (got != nil) != tc.wantAlert {
				t.Fatalf("alertFor(%d, %d) alert = %v, want alert = %v",
					tc.phase, tc.marked, got != nil, tc.wantAlert)
			}
			if got != nil && (got.Title == "" || got.Body == "") {
				t.Errorf("alert = %+v, want both title and body populated", *got)
			}
		})
	}
}

// TestArrivingAlertsExactlyOnceAcrossManyTicks is the test this whole
// mechanism exists for.
//
// Arriving is a THRESHOLD, not a transition: once the pickup ETA is under two
// minutes it stays under two minutes for every remaining pass of the leg. Six
// ticker passes therefore describe six pushes and exactly ONE expansion — and
// the ETA is walked back UP above the threshold and down again in the middle,
// because traffic does that and a re-crossing must not buy a second island.
func TestArrivingAlertsExactlyOnceAcrossManyTicks(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			// Registered while the car was on its way, so Enroute has already
			// been shown — the ordinary state when the threshold is crossed.
			AlertedPhase: AlertPhaseEnroute,
		},
		RideContext: RideContext{
			Status:             statusAccepted,
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			ETAMinutes:         intPtr(2),
			NavUpdatedAt:       &fresh,
			PickupDispatchedAt: &arrivingDispatch,
			DispatchUnderway:   true,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())

	// 2 → 1 → 3 (traffic) → 2 → 1 → 1. Six passes, one crossing that matters.
	for _, eta := range []int{2, 1, 3, 2, 1, 1} {
		store.legs[0].ETAMinutes = intPtr(eta)
		ticker.RunPass(t.Context())
	}

	sent := sender.Sent()
	if len(sent) != 6 {
		t.Fatalf("sends = %d, want 6", len(sent))
	}

	var alerting []int
	for i := range sent {
		if sent[i].Alert != nil {
			alerting = append(alerting, i)
		}
	}
	if len(alerting) != 1 || alerting[0] != 0 {
		t.Fatalf("alerting passes = %v, want exactly [0] — Arriving is true on every remaining "+
			"tick of the leg, and an island that opens on each of them is the failure the "+
			"design forbids by name", alerting)
	}
	if got, want := sent[0].Alert.Body, alertBodies[AlertPhaseArriving]; got != want {
		t.Errorf("alert body = %q, want %q", got, want)
	}

	// The mark was persisted once, at the right rung.
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 1 || got[0] != AlertPhaseArriving {
		t.Errorf("persisted alert phases = %v, want exactly [%d]", got, AlertPhaseArriving)
	}
}

// TestArrivingAlertRidesPriorityTen catches the interaction between two rules
// that were written for different reasons.
//
// The ticker sends at apns-priority 5 so a routine refresh never competes with
// "your car is here" for Apple's per-Activity budget. Arriving is computed by
// the ticker — so without the override, the one push in this feature whose
// entire purpose is to be delivered promptly would be the one marked
// droppable.
func TestArrivingAlertRidesPriorityTen(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			AlertedPhase: AlertPhaseEnroute,
		},
		RideContext: RideContext{
			Status:             statusAccepted,
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			ETAMinutes:         intPtr(1),
			NavUpdatedAt:       &fresh,
			PickupDispatchedAt: &arrivingDispatch,
			DispatchUnderway:   true,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())
	// The alert is spent; the next pass is an ordinary conserving refresh.
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	if sent[0].Alert == nil {
		t.Fatal("first pass carried no alert")
	}
	if got := sent[0].priority(); got != priorityImmediate {
		t.Errorf("alerting tick priority = %s, want %s", got, priorityImmediate)
	}
	// LowPriority is still SET on it — the override lives in priority(), so a
	// future reader can see that the ticker did ask for conserving and was
	// overruled rather than that the flag was quietly dropped.
	if !sent[0].LowPriority {
		t.Error("the alerting tick cleared LowPriority; the override belongs in priority(), " +
			"so the caller's intent stays visible")
	}
	if sent[1].Alert != nil {
		t.Fatalf("second pass alerted again: %+v", *sent[1].Alert)
	}
	if got := sent[1].priority(); got != priorityConserving {
		t.Errorf("routine tick priority = %s, want %s", got, priorityConserving)
	}
}

// TestOrdinaryTicksNeverAlert is the plain statement of the design's
// prohibition: a mid-leg Activity whose phase has already been shown gets no
// expansion however many times the ETA moves.
func TestOrdinaryTicksNeverAlert(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			AlertedPhase: AlertPhaseOnTrip,
		},
		RideContext: RideContext{
			Status:             statusEnroute,
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			ETAMinutes:         intPtr(14),
			TripMilesRemaining: miles(6),
			NavUpdatedAt:       &fresh,
			PickupDispatchedAt: &arrivingDispatch,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	for i := 0; i < 4; i++ {
		store.legs[0].ETAMinutes = intPtr(14 - i*3)
		ticker.RunPass(t.Context())
	}

	for i, n := range sender.Sent() {
		if n.Alert != nil {
			t.Errorf("tick %d alerted (%+v); ETA ticks must never expand the island", i, *n.Alert)
		}
	}
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 0 {
		t.Errorf("persisted alert phases = %v, want none", got)
	}
}

// TestStaleFlipNeverAlerts is the design's other named prohibition. A car going
// quiet changes what the card SAYS — the subline flips to "Last updated" — but
// it is not a phase change, so the island must stay shut.
//
// RUN ON A RUNG WHERE AN ALERT COULD ACTUALLY FIRE, which is the whole
// difference between this test and a tautology. Its previous form sat on
// `enroute` at a mark of On trip, where alertFor(5, 5) is nil whatever the
// staleness does — it asserted that nothing happened in a situation where
// nothing could. Here the ride is mid-PICKUP at a mark of Enroute, so rung 3 is
// live and unspent, and the car's last streamed reading is UNDER the two-minute
// threshold: an implementation that read the ETA without dating it would alert
// Arriving on this pass. The second half then shows the same reading, freshly
// stamped, doing exactly that — so the first half is about staleness and not
// about some other reason the island stayed shut.
func TestStaleFlipNeverAlerts(t *testing.T) {
	// The car's last word was "90 seconds out", said half an hour ago and never
	// revised — the shape a car leaves behind when it parks and goes quiet.
	quiet := fixedNow.Add(-30 * time.Minute)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			AlertedPhase: AlertPhaseEnroute,
		},
		RideContext: RideContext{
			Status:           statusAccepted,
			VehicleName:      "Blue Whale",
			Destination:      "Home",
			ETAMinutes:       intPtr(1),
			NavUpdatedAt:     &quiet,
			DispatchUnderway: true,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].Alert != nil {
		t.Errorf("the stale pass alerted (%+v); the design forbids it by name, and the reading it "+
			"alerted on is half an hour old", *sent[0].Alert)
	}
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 0 {
		t.Errorf("persisted alert phases = %v, want none — a stale reading may not burn a rung", got)
	}

	// THE CONTROL. The same 1 minute, now freshly stamped AND postdating the
	// dispatch, is the real thing and must alert — otherwise the assertion above
	// would pass for the wrong reason.
	fresh := fixedNow.Add(-30 * time.Second)
	store.legs[0].NavUpdatedAt = &fresh
	store.legs[0].PickupDispatchedAt = &arrivingDispatch
	ticker.RunPass(t.Context())

	sent = sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	if sent[1].Alert == nil {
		t.Fatal("a fresh sub-threshold reading did not alert; the gate is refusing the real Arriving too")
	}
	if got, want := sent[1].Alert.Body, alertBodies[AlertPhaseArriving]; got != want {
		t.Errorf("alert body = %q, want %q", got, want)
	}
}

// TestStaleETAAtAcceptDoesNotBurnTheLadder is the ordinary instant ride, and
// the durable defect it pins is the reason the freshness gate exists.
//
// The owner's car has just finished a previous trip, so `minutesToArrival` still
// reads 1 — the column keeps the last value a route produced. The rider requests
// an instant ride and the owner accepts. `ride.status.changed` fires at once and
// the fan-out reads the ride BEFORE the dispatched route has streamed back, so
// the ETA in hand belongs to a journey that is over. DispatchUnderway is no help
// whatsoever here: `scheduled_for IS NULL` on an instant ride makes it
// unconditionally true.
//
// Ungated, the accept push expands the island with "your ride is almost here"
// for a car twenty minutes away AND persists the mark at Arriving(3) — spending
// rungs 2 and 3 together, so the rider is never told a car is on its way and
// never gets the real Arriving when the car genuinely is two minutes out. Unlike
// a wrong `eta`, which the next reading corrects, that is in the database for
// the life of the ride. So the accept push must yield Enroute, and Arriving must
// wait for a reading dated after the dispatch.
func TestStaleETAAtAcceptDoesNotBurnTheLadder(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	// A v3 registration made at `requested`, seeded at Dispatch.
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseDispatch,
	}}
	// The leftover: one minute, last streamed before the accept and never since.
	leftover := fixedNow.Add(-ProgressFreshFor - time.Minute)
	store.context[activityRideID] = RideContext{
		Status:             statusAccepted,
		VehicleName:        "Blue Whale",
		Destination:        "Home",
		ETAMinutes:         intPtr(1),
		NavUpdatedAt:       &leftover,
		PickupDispatchedAt: &acceptDispatch,
		DispatchUnderway:   true,
	}

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusAccepted,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends on accept = %d, want 1", len(sent))
	}
	if sent[0].Alert == nil {
		t.Fatal("the accept push carried no alert; a car being on its way is a phase change")
	}
	if got, want := sent[0].Alert.Body, alertBodies[AlertPhaseEnroute]; got != want {
		t.Errorf("accept alert body = %q, want %q — the ETA in hand at accept describes the trip "+
			"the car just finished, not this one", got, want)
	}
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 1 || got[0] != AlertPhaseEnroute {
		t.Fatalf("persisted alert phases after accept = %v, want exactly [%d]; a mark of %d here "+
			"spends the real Arriving as well as Enroute",
			got, AlertPhaseEnroute, AlertPhaseArriving)
	}

	// THE CAR IS DISPATCHED AND DRIVES THE REAL PICKUP. Twenty minutes later it
	// is genuinely ninety seconds out, and this reading is its own.
	arrivingAt := fixedNow.Add(20 * time.Minute)
	n.now = func() time.Time { return arrivingAt }
	dispatched := arrivingAt.Add(-20 * time.Second)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			// The mark the accept push left behind.
			AlertedPhase: AlertPhaseEnroute,
		},
		RideContext: RideContext{
			Status:             statusAccepted,
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			ETAMinutes:         intPtr(1),
			NavUpdatedAt:       &dispatched,
			PickupDispatchedAt: &arrivingDispatch,
			DispatchUnderway:   true,
		},
	}}

	sender.Reset()
	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())
	// And again, because Arriving is a threshold rather than a transition: it is
	// true on every remaining pass of the leg and must be sent on exactly one.
	ticker.RunPass(t.Context())

	ticks := sender.Sent()
	if len(ticks) != 2 {
		t.Fatalf("ticker sends = %d, want 2", len(ticks))
	}
	if ticks[0].Alert == nil {
		t.Fatal("the first post-dispatch pass did not alert; the rider never learns the car is close")
	}
	if got, want := ticks[0].Alert.Body, alertBodies[AlertPhaseArriving]; got != want {
		t.Errorf("post-dispatch alert body = %q, want %q", got, want)
	}
	if ticks[1].Alert != nil {
		t.Errorf("the second pass alerted again (%+v); Arriving is once per Activity", *ticks[1].Alert)
	}

	want := []AlertPhase{AlertPhaseEnroute, AlertPhaseArriving}
	got := store.savedAlertsFor(activityRideID, testRiderID)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("persisted alert phases = %v, want %v — the ladder climbed 2 then 3, in that order "+
			"and once each", got, want)
	}
}

// TestLifecycleTransitionsAlertOnceEach walks a whole ride through the
// lifecycle notifier and asserts one expansion per phase change.
func TestLifecycleTransitionsAlertOnceEach(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	// A fresh registration on a `requested` ride: seeded at Dispatch, because
	// the app has just drawn that state itself.
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseDispatch,
	}}

	steps := []struct {
		status string
		want   AlertPhase
	}{
		{statusAccepted, AlertPhaseEnroute},
		{statusArrived, AlertPhaseArrived},
		{statusEnroute, AlertPhaseOnTrip},
	}

	for _, step := range steps {
		rc := store.context[activityRideID]
		rc.Status = step.status
		// Far enough out that `accepted` is Enroute rather than Arriving.
		rc.ETAMinutes, rc.DispatchUnderway = intPtr(11), true
		store.context[activityRideID] = rc

		sender.Reset()
		n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
			RideRequestID: activityRideID, RiderID: testRiderID, Status: step.status,
		}))
		n.Wait()

		sent := sender.Sent()
		if len(sent) != 1 {
			t.Fatalf("%s: sends = %d, want 1", step.status, len(sent))
		}
		if sent[0].Alert == nil {
			t.Fatalf("%s: no alert; every phase change expands the island", step.status)
		}
		if got, want := sent[0].Alert.Body, alertBodies[step.want]; got != want {
			t.Errorf("%s: alert body = %q, want %q", step.status, got, want)
		}
		// A lifecycle push is priority 10 already, alert or no alert.
		if got := sent[0].priority(); got != priorityImmediate {
			t.Errorf("%s: priority = %s, want %s", step.status, got, priorityImmediate)
		}

		// REPLAY THE SAME EVENT. A bus that delivers twice, or a handler that
		// re-publishes, must not buy a second expansion.
		sender.Reset()
		n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
			RideRequestID: activityRideID, RiderID: testRiderID, Status: step.status,
		}))
		n.Wait()
		if replayed := sender.Sent(); len(replayed) != 1 || replayed[0].Alert != nil {
			t.Errorf("%s: replay produced %d sends with alert=%v, want 1 send and no alert",
				step.status, len(replayed), len(replayed) > 0 && replayed[0].Alert != nil)
		}
	}

	// Every phase persisted exactly once, in order.
	want := []AlertPhase{AlertPhaseEnroute, AlertPhaseArrived, AlertPhaseOnTrip}
	got := store.savedAlertsFor(activityRideID, testRiderID)
	if len(got) != len(want) {
		t.Fatalf("persisted alert phases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("persisted alert phases = %v, want %v", got, want)
		}
	}
}

// TestOutOfOrderTransitionDoesNotReopenTheIsland is the property the monotone
// comparison buys and an equality test against "the last phase" would not.
func TestOutOfOrderTransitionDoesNotReopenTheIsland(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseArrived,
	}}
	rc := store.context[activityRideID]
	rc.Status, rc.DispatchUnderway = statusAccepted, true
	rc.ETAMinutes = intPtr(11)
	store.context[activityRideID] = rc

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusAccepted,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].Alert != nil {
		t.Errorf("a replayed `accepted` after `arrived` alerted (%+v); 2 > 4 is false and the "+
			"ladder must swallow it", *sent[0].Alert)
	}
}

// TestCompletedSendsAnAlertingUpdateThenTheEnd is MYR-418, and it is the whole
// issue in one function.
//
// WHAT THE OLD SHAPE DID, because the regression this guards is invisible from
// the server side. The sixth rung used to ride the `end` push itself: one
// notification, `event: "end"`, carrying the final content-state AND an
// `aps.alert`. APNs accepted every one of them — the client's ride alerted
// through phase 5 and then tombstoned at phase 5 with the end delivered — and
// no island ever opened. Apple's ActivityKit push documentation introduces the
// alert dictionary under `start` and `update`, and says of `end` only that it
// should carry the final content state; an alert there is undocumented, not
// rejected, and not honoured. There is no failure signal for it anywhere, which
// is why the shape is pinned here rather than trusted.
//
// SO THE PAIR: an alerting UPDATE carrying the terminal state, then the `end`
// carrying the same state, its dismissal-date, and no alert.
//
// AND THE PAIR IS NOW FIVE MINUTES APART (MYR-421), which is the other half of
// what this pins. The end does not merely end the Activity — it removes it from
// the Dynamic Island about 1.4 seconds later — so a pair one second apart put
// the sixth expansion's check on the island for two seconds total. The
// announcement goes out on the transition and the end is HELD for the linger,
// sent by the ticker's held-end pass.
func TestCompletedSendsAnAlertingUpdateThenTheEnd(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseOnTrip,
	}}
	store.completeRide(activityRideID, fixedNow)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()

	if got := len(sender.Sent()); got != 1 {
		t.Fatalf("sends at completion = %d, want 1 — the announcement alone; the end is held", got)
	}
	holdExpires(t, n, store)

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2 — an alerting update, then the held end", len(sent))
	}
	update, end := sent[0], sent[1]

	// THE ALERT IS ON THE UPDATE, which is the shape Apple documents it on.
	if update.Event != ActivityEventUpdate {
		t.Errorf("first send event = %s, want %s", update.Event, ActivityEventUpdate)
	}
	if update.Alert == nil || update.Alert.Body != alertBodies[AlertPhaseCompleted] {
		t.Fatalf("first send alert = %v, want the Completed copy", update.Alert)
	}
	// It is not an `end`, so it must not carry a dismissal-date: on an update
	// the key is meaningless, and its presence would be the first symptom of
	// the two shapes being built from one branch again.
	if update.DismissalDate != nil {
		t.Errorf("alerting update carries dismissal-date %v; that key belongs to the end alone",
			update.DismissalDate)
	}
	// An expansion is never a refresh. Priority 10 whatever else is true.
	if got := update.priority(); got != priorityImmediate {
		t.Errorf("alerting update priority = %q, want %q", got, priorityImmediate)
	}

	// THE END IS ALERT-FREE and carries the same final state.
	if end.Event != ActivityEventEnd {
		t.Errorf("second send event = %s, want %s", end.Event, ActivityEventEnd)
	}
	if end.Alert != nil {
		t.Errorf("the end push carries an alert (%+v); Apple does not document one there, "+
			"and MYR-418 is what believing it did cost", *end.Alert)
	}
	if end.DismissalDate == nil || !end.DismissalDate.Equal(end.Timestamp.Add(DismissAfter)) {
		t.Errorf("dismissal-date = %v, want %v (MYR-406's five minutes past the end's own instant)",
			end.DismissalDate, end.Timestamp.Add(DismissAfter))
	}

	// BOTH CARRY THE FINAL CONTENT-STATE. The end's is what the card is left
	// rendering after the Activity ends, and the update's is what the island
	// expands to show — a wrong or empty one on either is a rider who watches
	// the expansion happen and sees the old state inside it.
	for i, n := range []ActivityNotification{update, end} {
		if n.ContentState.Status != statusCompleted {
			t.Errorf("send %d status = %q, want %q", i, n.ContentState.Status, statusCompleted)
		}
		if n.ContentState.Progress == nil || *n.ContentState.Progress != 1 {
			t.Errorf("send %d progress = %v, want a full track — `completed` is asserted by the "+
				"ride record, not estimated", i, n.ContentState.Progress)
		}
	}

	// THE ORDERING DEFENCE. ActivityKit discards a push older than the one it
	// is showing; `aps.timestamp` is whole seconds, so two pushes off one clock
	// read would collide. Asserted as a strict inequality ON THE RENDERED
	// SECOND rather than on the time.Time, because the wire is where the
	// collision would happen. The hold makes the gap five minutes rather than
	// the one second endAfterAlertGap used to buy, which subsumes the defence
	// rather than replacing it — the constant still guards every ending that is
	// not held.
	if end.Timestamp.Unix() <= update.Timestamp.Unix() {
		t.Errorf("end aps.timestamp %d does not exceed the update's %d — the end is the push that "+
			"must never be discarded", end.Timestamp.Unix(), update.Timestamp.Unix())
	}
	// THE HOLD ITSELF. The island keeps the check for the whole linger instead
	// of ~2 seconds, which is the defect MYR-421 is about.
	if got := end.Timestamp.Sub(update.Timestamp); got < DismissAfter {
		t.Errorf("the end followed the announcement after %s, want at least %s — an `.ended` "+
			"Activity leaves the Dynamic Island ~1.4s later, so this gap IS the time the "+
			"rider has to see the check", got, DismissAfter)
	}

	// THE LADDER IS MARKED, on the push that carried the alert. It could not be
	// before: the mark's guarded UPDATE is scoped to live rows and the end's
	// tombstone was already racing it, so a completed ride's row tombstoned at
	// 5 no matter what happened. That tombstone-at-5 was the prod evidence.
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 1 || got[0] != AlertPhaseCompleted {
		t.Errorf("persisted alert phases = %v, want exactly [%d]", got, AlertPhaseCompleted)
	}
	if store.endCount() != 1 {
		t.Error("the pair did not tombstone the registry rows")
	}
}

// TestCompletedHoldsTheRowsLiveUntilTheEndGoesOut is MYR-421's tombstone
// semantics, which are the part of this change that reaches beyond the push.
//
// The row must SURVIVE the hold. Tombstoning at completion — where it used to
// happen — would exclude the row from its own `end` five minutes later, which
// is the very defect the send-then-tombstone ordering exists to prevent, moved
// in time rather than avoided.
func TestCompletedHoldsTheRowsLiveUntilTheEndGoesOut(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.completeRide(activityRideID, fixedNow)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()

	if store.endCount() != 0 {
		t.Fatal("completion tombstoned the rows; the held end would then have nothing to push to")
	}
	if got := len(sender.Sent()); got != 1 || sender.Sent()[0].Event != ActivityEventUpdate {
		t.Fatalf("sends at completion = %+v, want one update", sender.Sent())
	}

	// A pass BEFORE the hold expires must send nothing: the whole point is that
	// the island keeps the check for the five minutes.
	NewActivityTicker(n, store, TickerConfig{Enabled: false}, discardLogger()).RunPass(context.Background())
	if got := len(sender.Sent()); got != 1 {
		t.Fatalf("sends after an early pass = %d, want 1 — the end went out before its hold expired", got)
	}
	if store.endCount() != 0 {
		t.Error("an early pass tombstoned the rows")
	}

	holdExpires(t, n, store)

	sent := sender.Sent()
	if len(sent) != 2 || sent[1].Event != ActivityEventEnd {
		t.Fatalf("sends after the hold = %+v, want the end", sent)
	}
	if store.endCount() != 1 {
		t.Error("the held end did not tombstone the registry rows")
	}

	// And it is idempotent: a second pass finds no live row and pushes nothing.
	holdExpires(t, n, store)
	if got := len(sender.Sent()); got != 2 {
		t.Errorf("sends after a second pass = %d, want 2 — the tombstone must retire the ride", got)
	}
}

// TestCompletedDoesNotReAlertWhenTheSixthRungIsAlreadySpent keeps the pair
// inside the once-per-phase promise the other five rungs make.
//
// The mark is raised by the pair's own first push, so the second one finds
// 6 > 6 false and attaches nothing — which is the mechanism, not a special
// case. A row that somehow arrives already at 6 gets the same treatment.
func TestCompletedDoesNotReAlertWhenTheSixthRungIsAlreadySpent(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseCompleted,
	}}
	store.completeRide(activityRideID, fixedNow)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()
	holdExpires(t, n, store)

	for i, s := range sender.Sent() {
		if s.Alert != nil {
			t.Errorf("send %d (%s) alerted at a rung already spent: %+v", i, s.Event, *s.Alert)
		}
	}
	// Still ended, and still tombstoned: a spent rung suppresses the expansion,
	// never the ending.
	sent := sender.Sent()
	if len(sent) == 0 || sent[len(sent)-1].Event != ActivityEventEnd {
		t.Fatalf("sends = %+v, want the end push regardless", sent)
	}
	if store.endCount() != 1 {
		t.Error("the ride was not tombstoned")
	}
}

// TestNoEndPushEverCarriesAnAlert is the invariant MYR-418 turns on, stated
// once across every ending this surface has.
//
// It is deliberately broader than the completed case above: the rule is not
// "completed moved its alert", it is "an `end` is a state delivery and never an
// interruption", and that is what a future phase seven must not quietly undo.
func TestNoEndPushEverCarriesAnAlert(t *testing.T) {
	for _, status := range []string{statusCompleted, statusDeclined, "cancelled"} {
		t.Run(status, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.byRide[activityRideID] = []Activity{{
				RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
				AlertedPhase: AlertPhaseDispatch,
			}}
			rc := store.context[activityRideID]
			rc.Status = status
			store.context[activityRideID] = rc
			if status == statusCompleted {
				store.completeRide(activityRideID, fixedNow)
			}

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID, RiderID: testRiderID, Status: status,
			}))
			n.Wait()
			// The completed end is held; the unhappy ones already went out and
			// a pass over them finds nothing.
			holdExpires(t, n, store)

			var ends int
			for _, s := range sender.Sent() {
				if s.Event != ActivityEventEnd {
					continue
				}
				ends++
				if s.Alert != nil {
					t.Errorf("%s: the end push carries an alert (%+v)", status, *s.Alert)
				}
			}
			if ends != 1 {
				t.Errorf("%s: end pushes = %d, want exactly 1", status, ends)
			}
		})
	}
}

// TestTerminalEndsCarryTheirFinalContentState is the other half of MYR-418's
// question, asked of the endings that do NOT alert.
//
// They have no expansion to lose, but they do have a card: the client renders
// each terminal status's own copy, and an `end` that carried a stale or empty
// state would leave a cancelled ride reading "your car is on its way" for the
// thirty seconds before it dismisses. Apple asks for the final content state on
// an end in as many words, and this is the assertion that we send it.
//
// `reservation_expired` is here and is not a ride status: the sweeper resolves
// it into the dispatch columns and leaves the ride at `accepted`, so
// EndForReservationExpiry passes the ending in and endRide's `rc.Status =
// status` is the only reason the card ever hears the word. That override is
// exactly what this case guards.
func TestTerminalEndsCarryTheirFinalContentState(t *testing.T) {
	tests := []struct {
		name   string
		status string
		end    func(n *ActivityNotifier)
	}{
		{"declined", statusDeclined, nil},
		{"cancelled", "cancelled", nil},
		{
			"reservation_expired overrides a ride row still reading accepted",
			"reservation_expired",
			func(n *ActivityNotifier) {
				n.EndForReservationExpiry(context.Background(), activityRideID)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			if tc.end == nil {
				rc := store.context[activityRideID]
				rc.Status = tc.status
				store.context[activityRideID] = rc
				n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
					RideRequestID: activityRideID, RiderID: testRiderID, Status: tc.status,
				}))
				n.Wait()
			} else {
				// The store still says `accepted`, as the real row does.
				tc.end(n)
			}

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sends = %d, want 1 — no pre-end alert outside the six", len(sent))
			}
			end := sent[0]
			if end.Event != ActivityEventEnd {
				t.Fatalf("event = %s, want %s", end.Event, ActivityEventEnd)
			}
			if end.ContentState.Status != tc.status {
				t.Errorf("content-state.status = %q, want %q — the card renders the ending's own copy",
					end.ContentState.Status, tc.status)
			}
			if end.ContentState.Version != ActivityContentStateVersion {
				t.Errorf("content-state.v = %d, want %d", end.ContentState.Version, ActivityContentStateVersion)
			}
			// No track on a journey that did not happen: a partial arc over bad
			// news is noise on top of it.
			if end.ContentState.Progress != nil {
				t.Errorf("content-state.progress = %v on %s, want absent",
					*end.ContentState.Progress, tc.status)
			}
			if end.ContentState.AsOf == nil {
				t.Error("content-state.asOf absent; every content-state carries one")
			}
		})
	}
}

// TestUnhappyEndingsNeverAlert. Their content-state still lands — a lock
// screen that keeps saying "on its way" after a cancellation is the whole
// reason this surface pushes on every transition — but an island opening to
// deliver bad news the card already carries is not a kindness.
func TestUnhappyEndingsNeverAlert(t *testing.T) {
	for _, status := range []string{statusDeclined, "cancelled"} {
		t.Run(status, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.byRide[activityRideID] = []Activity{{
				RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
				AlertedPhase: AlertPhaseDispatch,
			}}
			rc := store.context[activityRideID]
			rc.Status = status
			store.context[activityRideID] = rc

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID, RiderID: testRiderID, Status: status,
			}))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sends = %d, want 1 — the ending must still reach the card", len(sent))
			}
			if sent[0].Event != ActivityEventEnd {
				t.Errorf("event = %s, want %s", sent[0].Event, ActivityEventEnd)
			}
			if sent[0].Alert != nil {
				t.Errorf("alerted on %s (%+v); it is outside the design's six", status, *sent[0].Alert)
			}
		})
	}
}

// TestStaleSnapshotAlertIsBoundedAndCannotLowerTheMark pins the bound the
// contract states rather than the guarantee it would be nice to have.
//
// The ticker is unsharded: every replica lists every live Activity at the top of
// its pass and raises the mark only after Apple accepts, so two replicas can
// hold the same snapshot and both attach the same alert. Worse, one replica's
// snapshot AGES across a pass of up to MaxPerPass sends, so it can send a lower
// rung's banner after another process has already sent a higher one.
//
// Neither is eliminated and this test does not pretend otherwise — it pins what
// IS true: the guarded write refuses to lower the mark, so the damage stops at
// one duplicate per replica per phase and the out-of-order case self-heals on
// the very next pass. Writing the mark BEFORE the send would close it, and is
// the worse trade: a phase burnt on a push APNs never accepted is an expansion
// the rider never gets at all. See saveAlertedPhase.
func TestStaleSnapshotAlertIsBoundedAndCannotLowerTheMark(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	leg := ActivityLeg{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			AlertedPhase: AlertPhaseEnroute,
		},
		RideContext: RideContext{
			Status:             statusAccepted,
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			ETAMinutes:         intPtr(1),
			NavUpdatedAt:       &fresh,
			PickupDispatchedAt: &arrivingDispatch,
			DispatchUnderway:   true,
		},
	}
	store.legs = []ActivityLeg{leg}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	// Replica A's pass: Arriving is unspent, so it alerts and raises the mark.
	ticker.RunPass(t.Context())
	// Replica B was holding the SAME pre-write snapshot. Restoring it is exactly
	// what a second process listing the row before A's UPDATE landed would see.
	store.legs = []ActivityLeg{leg}
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	var alerting int
	for i := range sent {
		if sent[i].Alert != nil {
			alerting++
		}
	}
	// THE HONEST NUMBER. Two replicas over one stale snapshot means two
	// expansions, and the contract says so in as many words.
	if alerting != 2 {
		t.Fatalf("alerting sends = %d, want 2 — the bound is one duplicate per replica per phase, "+
			"and a test claiming fewer would be pinning a guarantee this design does not make",
			alerting)
	}

	// THE PART THAT IS GUARANTEED. The second write cannot lower the mark, and
	// the next pass — reading the raised row rather than the stale snapshot —
	// alerts nothing. That is what makes the duplication bounded instead of
	// permanent.
	ticker.RunPass(t.Context())
	if third := sender.Sent(); len(third) != 3 || third[2].Alert != nil {
		t.Fatalf("the pass after the snapshot caught up alerted again; the duplication is unbounded")
	}

	// And the out-of-order banner: a straggler still holding Arriving reaches
	// the row after a lifecycle push has already alerted Arrived. Its write is
	// refused, so the mark stays at the higher rung.
	key := ActivityKey{RideRequestID: activityRideID, UserID: testRiderID}
	if err := store.SaveAlertedPhase(t.Context(), key, AlertPhaseArrived); err != nil {
		t.Fatalf("save Arrived: %v", err)
	}
	if err := store.SaveAlertedPhase(t.Context(), key, AlertPhaseArriving); err != nil {
		t.Fatalf("a losing write is a silent no-op, not an error: %v", err)
	}
	if got := store.legs[0].AlertedPhase; got != AlertPhaseArrived {
		t.Errorf("mark = %d after a straggler wrote %d, want the higher %d to have held",
			got, AlertPhaseArriving, AlertPhaseArrived)
	}
}

// TestAlertPhaseNotPersistedWhenAppleRefuses is the write-ordering promise,
// with the sign of the failure reversed from the progress floor's.
//
// The mark records what the phone HAS BEEN SHOWN. Burning a phase on a push
// Apple never accepted would cost the rider that expansion permanently; the
// cost of the chosen ordering is at most one extra expansion, which the next
// push re-evaluates.
func TestAlertPhaseNotPersistedWhenAppleRefuses(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseDispatch,
	}}
	rc := store.context[activityRideID]
	rc.Status, rc.DispatchUnderway = statusAccepted, true
	rc.ETAMinutes = intPtr(11)
	store.context[activityRideID] = rc
	sender.Err = errTestSendFailed

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusAccepted,
	}))
	n.Wait()

	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 0 {
		t.Errorf("persisted %v for a push Apple refused, want none — the rider would lose that "+
			"expansion for good", got)
	}
}

// TestAlertPayloadRawJSONKeys pins the `aps.alert` dictionary on the wire.
//
// Same discipline as its neighbours and the same MYR-362 lesson: Apple decodes
// the literal strings "alert", "title" and "body", so nothing here reads a
// value through a Go field name.
func TestAlertPayloadRawJSONKeys(t *testing.T) {
	n := testActivityNotification()
	n.Alert = &ActivityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseArrived]}

	got := captureActivity(t, n)

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	aps := payload["aps"].(map[string]any)

	alert, ok := aps["alert"].(map[string]any)
	if !ok {
		t.Fatalf("aps.alert is %T, want an object", aps["alert"])
	}
	wantKeys := []string{"title", "body"}
	if got := keysOf(alert); !sameSet(got, wantKeys) {
		t.Errorf("aps.alert keys = %v, want %v", got, wantKeys)
	}
	if got := alert["title"]; got != alertTitle {
		t.Errorf("aps.alert.title = %v, want %q", got, alertTitle)
	}
	if got, want := alert["body"], alertBodies[AlertPhaseArrived]; got != want {
		t.Errorf("aps.alert.body = %v, want %q", got, want)
	}
	// No sound: the design asks for an expansion, not an interruption.
	if _, present := alert["sound"]; present {
		t.Errorf("aps.alert.sound = %v; six beeps per ride would undo the feature", alert["sound"])
	}
	// The alert does not disturb the four aps keys that shipped before it.
	if _, present := aps["content-state"]; !present {
		t.Error("aps.content-state missing from an alerting push")
	}
	if got, want := aps["timestamp"], float64(fixedNow.Unix()); got != want {
		t.Errorf("aps.timestamp = %v, want %v", got, want)
	}
	// And the header agrees: an alerting push is never conserving.
	if got := got.header.Get("apns-priority"); got != priorityImmediate {
		t.Errorf("apns-priority = %s, want %s on an alerting push", got, priorityImmediate)
	}
}

// TestContractPrintedAlertExampleDecodes reads §7.21.4's alerting example back
// out of the document, so the printed dictionary and the struct cannot drift.
func TestContractPrintedAlertExampleDecodes(t *testing.T) {
	// The `alert` object printed in the alerting-update example, byte for byte.
	const printed = `{
      "title": "Your ride",
      "body": "Your ride is here"
    }`

	var got activityAlert
	dec := json.NewDecoder(strings.NewReader(printed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the contract's printed alert does not decode: %v", err)
	}
	want := activityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseArrived]}
	if got != want {
		t.Errorf("printed alert = %+v, want %+v — the document and the copy table disagree", got, want)
	}
}
