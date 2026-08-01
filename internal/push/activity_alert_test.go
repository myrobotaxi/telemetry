package push

import (
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
// the two things a reader would otherwise have to take on trust: that Arriving
// outranks Enroute on the same status, and that the unhappy endings are outside
// the six.
func TestAlertPhaseForMapsTheDesignsSixPhases(t *testing.T) {
	tests := []struct {
		name string
		rc   RideContext
		want AlertPhase
	}{
		{"dispatch", RideContext{Status: statusRequested}, AlertPhaseDispatch},
		{
			"enroute, car far away",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(9), DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			"enroute, no eta at all",
			RideContext{Status: statusAccepted, DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			"arriving at the threshold",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(ArrivingWithinMinutes), DispatchUnderway: true},
			AlertPhaseArriving,
		},
		{
			"arriving under the threshold",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(1), DispatchUnderway: true},
			AlertPhaseArriving,
		},
		{
			"one minute above the threshold is still Enroute",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(ArrivingWithinMinutes + 1), DispatchUnderway: true},
			AlertPhaseEnroute,
		},
		{
			// THE DEFECT THE GATE CLOSES. A reservation accepted for tomorrow
			// reads `accepted` today, and the car the projection reads is the
			// OWNER'S, mid-errand. Without DispatchUnderway an owner parking
			// two minutes from home would tell the rider their ride was almost
			// here, a day early, with a notification attached.
			"dormant reservation never reaches Arriving",
			RideContext{Status: statusAccepted, ETAMinutes: intPtr(1), DispatchUnderway: false},
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
			if got := alertPhaseFor(tc.rc); got != tc.want {
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
			Status:           statusAccepted,
			VehicleName:      "Blue Whale",
			Destination:      "Home",
			ETAMinutes:       intPtr(2),
			NavUpdatedAt:     &fresh,
			DispatchUnderway: true,
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
			Status:           statusAccepted,
			VehicleName:      "Blue Whale",
			Destination:      "Home",
			ETAMinutes:       intPtr(1),
			NavUpdatedAt:     &fresh,
			DispatchUnderway: true,
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
func TestStaleFlipNeverAlerts(t *testing.T) {
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
			ETAMinutes:         intPtr(9),
			TripMilesRemaining: miles(4),
			NavUpdatedAt:       &fresh,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())

	// The car goes quiet; the next pass is well past the horizon and holds.
	n.now = func() time.Time { return fixedNow.Add(30 * time.Minute) }
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	if sent[1].Alert != nil {
		t.Errorf("the stale pass alerted (%+v); the design forbids it by name", *sent[1].Alert)
	}
	// Sanity: the pass really did go stale, so the assertion above is about
	// the case it claims to be about.
	if a, b := sent[0].ContentState.AsOf, sent[1].ContentState.AsOf; a == nil || b == nil || *a != *b {
		t.Errorf("asOf moved (%s → %s); this pass was meant to be a held one", fmtPtr(a), fmtPtr(b))
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

// TestCompletedAlertsOnTheEndPushAndPersistsNothing covers the sixth rung,
// which is the only one that rides an `end`.
func TestCompletedAlertsOnTheEndPushAndPersistsNothing(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseOnTrip,
	}}
	rc := store.context[activityRideID]
	rc.Status = statusCompleted
	store.context[activityRideID] = rc

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].Event != ActivityEventEnd {
		t.Errorf("event = %s, want %s", sent[0].Event, ActivityEventEnd)
	}
	if sent[0].Alert == nil || sent[0].Alert.Body != alertBodies[AlertPhaseCompleted] {
		t.Fatalf("alert = %v, want the Completed copy", sent[0].Alert)
	}
	// The dismissal-date is MYR-406's five minutes, unchanged by any of this.
	if sent[0].DismissalDate == nil || !sent[0].DismissalDate.Equal(fixedNow.Add(DismissAfter)) {
		t.Errorf("dismissal-date = %v, want %v", sent[0].DismissalDate, fixedNow.Add(DismissAfter))
	}
	// Nothing is persisted: the rows are tombstoned immediately after, there
	// is no phase seven, and the write would race that tombstone to become a
	// guaranteed no-op.
	if got := store.savedAlertsFor(activityRideID, testRiderID); len(got) != 0 {
		t.Errorf("persisted alert phases on an end push = %v, want none", got)
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
