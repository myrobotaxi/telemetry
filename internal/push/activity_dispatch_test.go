package push

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The Dispatch phase (MYR-398, the v3 card).
//
// The design starts the Activity at REQUEST rather than at accept, so
// `requested` — a status the content-state has always been able to carry —
// becomes a state the server actually pushes. What is asserted here is the one
// new RULE that came with it: no ETA before a car has been assigned.

// TestRequestedCardCarriesNoETA is the fabricated-ETA gate.
//
// The projection reads navigation from the ride's vehicle. Before the owner
// accepts, that car belongs to this ride in name only — it may be driving the
// owner to the shops with navigation on — so an arrival time computed from it
// is not a degraded ETA, it is an invented one under a headline that says
// "Finding your ride".
func TestRequestedCardCarriesNoETA(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	rc := RideContext{
		Status:      statusRequested,
		VehicleName: "Blue Whale",
		Destination: "Home",
		// The owner's own errand, four minutes from wherever they are going.
		ETAMinutes:         intPtr(4),
		TripMilesRemaining: miles(2),
		NavUpdatedAt:       &fresh,
		DispatchUnderway:   true,
	}

	state, anchor := contentState(rc, ProgressAnchor{}, fixedNow)

	if state.ETA != nil {
		t.Errorf("eta = %s on a `requested` ride; no car has been assigned to it yet",
			fmtPtr(state.ETA))
	}
	// The track needs no new rule — a status with no leg has never produced a
	// fraction — but the absence is worth pinning beside the ETA, because they
	// are the same claim about the same car.
	if state.Progress != nil {
		t.Errorf("progress = %s on a `requested` ride; there is no leg", fmtPtr(state.Progress))
	}
	if anchor != (ProgressAnchor{}) {
		t.Errorf("anchor = %+v, want none stored — the owner's errand must not anchor anything",
			anchor)
	}
	// And the card still says WHEN it was computed, so the Dispatch state can
	// go stale honestly like any other.
	if state.AsOf == nil || *state.AsOf != fixedNow.Unix() {
		t.Errorf("asOf = %s, want %d", fmtPtr(state.AsOf), fixedNow.Unix())
	}
}

// TestAcceptRestoresTheETA is the other half of the gate: it is scoped to
// `requested` and nothing else. The instant the owner accepts, the same car and
// the same reading produce an arrival time.
func TestAcceptRestoresTheETA(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	rc := RideContext{
		Status:           statusAccepted,
		VehicleName:      "Blue Whale",
		Destination:      "Home",
		ETAMinutes:       intPtr(4),
		NavUpdatedAt:     &fresh,
		DispatchUnderway: true,
	}

	state, _ := contentState(rc, ProgressAnchor{}, fixedNow)

	want := fixedNow.Add(4 * time.Minute).Unix()
	if state.ETA == nil || *state.ETA != want {
		t.Errorf("eta = %s, want %d", fmtPtr(state.ETA), want)
	}
}

// TestDispatchCardIsTheFourKeysPlusAsOf is the raw-key shape of the Dispatch
// state as it reaches the wire.
func TestDispatchCardIsTheFourKeysPlusAsOf(t *testing.T) {
	state, _ := contentState(RideContext{
		Status:      statusRequested,
		VehicleName: "Blue Whale",
		Destination: "Home",
		ETAMinutes:  intPtr(4),
	}, ProgressAnchor{}, fixedNow)

	n := testActivityNotification()
	n.ContentState = state
	got := captureActivity(t, n)

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	cs := payload["aps"].(map[string]any)["content-state"].(map[string]any)

	wantKeys := []string{"v", "status", "vehicleName", "destination", "asOf"}
	if got := keysOf(cs); !sameSet(got, wantKeys) {
		t.Errorf("content-state keys = %v, want %v", got, wantKeys)
	}
	if got := cs["status"]; got != statusRequested {
		t.Errorf("status = %v, want requested", got)
	}
}

// TestDispatchTickKeepsTheCardFresh. A `requested` ride has no ETA and no track
// to refresh, and it ticks anyway — because what a tick moves is the timestamp
// and the stale-date. Without it, "Finding your ride" would slide into
// ActivityKit's own "as of X min ago" rendering three minutes in, accusing
// itself of a fault while the search is genuinely still running.
func TestDispatchTickKeepsTheCardFresh(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			AlertedPhase: AlertPhaseDispatch,
		},
		RideContext: RideContext{
			Status:      statusRequested,
			VehicleName: "Blue Whale",
			Destination: "Home",
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())

	later := fixedNow.Add(2 * time.Minute)
	n.now = func() time.Time { return later }
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2 — a Dispatch card must keep being refreshed", len(sent))
	}
	if !sent[1].StaleDate().After(sent[0].StaleDate()) {
		t.Error("the second pass did not push the stale-date out; that is the only thing a " +
			"Dispatch tick is for")
	}
	// Neither pass alerts: the mark was seeded at Dispatch when the app
	// registered, because the app had just drawn that state itself.
	for i := range sent {
		if sent[i].Alert != nil {
			t.Errorf("pass %d expanded the island for a phase the rider was already looking at", i)
		}
	}
}

// TestAcceptOfARequestedRideAlertsOnce is the first genuine expansion of a v3
// ride: the app drew Dispatch itself, and Enroute is the first thing it did not
// already know.
func TestAcceptOfARequestedRideAlertsOnce(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
		AlertedPhase: AlertPhaseDispatch,
	}}
	rc := store.context[activityRideID]
	rc.Status, rc.DispatchUnderway = statusAccepted, true
	rc.ETAMinutes = intPtr(9)
	store.context[activityRideID] = rc

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusAccepted,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if sent[0].Alert == nil || sent[0].Alert.Body != alertBodies[AlertPhaseEnroute] {
		t.Fatalf("alert = %v, want the Enroute copy", sent[0].Alert)
	}
	// The ETA is back, now that a car is genuinely coming.
	if sent[0].ContentState.ETA == nil {
		t.Error("eta absent on the accept push; the gate is scoped to `requested`")
	}
}

// TestRequestedRideEndingsStillPush covers the funnel: both ways a `requested`
// ride can end reach the Activity as a terminal `end` with a dismissal-date,
// and neither expands the island.
func TestRequestedRideEndingsStillPush(t *testing.T) {
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
				t.Fatalf("sends = %d, want 1", len(sent))
			}
			if sent[0].Event != ActivityEventEnd {
				t.Errorf("event = %s, want %s — a Dispatch card that is never answered must be "+
					"ended, not abandoned", sent[0].Event, ActivityEventEnd)
			}
			if sent[0].DismissalDate == nil ||
				!sent[0].DismissalDate.Equal(fixedNow.Add(DismissPromptly)) {
				t.Errorf("dismissal-date = %v, want %v", sent[0].DismissalDate,
					fixedNow.Add(DismissPromptly))
			}
			if sent[0].Alert != nil {
				t.Errorf("alerted on %s; it is outside the design's six", status)
			}
			if store.ended[activityRideID] != 1 {
				t.Errorf("tombstoned %d times, want 1", store.ended[activityRideID])
			}
		})
	}
}
