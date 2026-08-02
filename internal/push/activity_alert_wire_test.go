package push

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The exact-layout guard on an alerting Live Activity update (MYR-413).
//
// WHY A BYTE-FOR-BYTE TEST WHEN activity_alert_test.go ALREADY DECODES THE
// KEYS. The keyed test proves the dictionary contains what we meant; it cannot
// prove the ENVELOPE around it is the one Apple documents, because a decoded
// map is blind to a key that moved, a sibling that was added, and a value that
// changed type. An ActivityKit alert that Apple cannot parse is discarded
// SILENTLY — no 400, no reason string, no metric, just an island that never
// opens — so this surface has no runtime failure signal at all and the only
// place the shape can be defended is here.
//
// THE REFERENCE. Apple, "Starting and updating Live Activities with ActivityKit
// push notifications" (fetched 2026-08-02) requires, for an alerting update:
// `apns-push-type: liveactivity`; `apns-topic: <bundleID>.push-type.liveactivity`;
// `apns-priority` of 5 or 10 (10 for an update that must arrive now); and an
// `aps` dictionary carrying `timestamp`, `event: "update"`, `content-state`,
// and an optional `alert`. `stale-date` and `dismissal-date` are the documented
// optional siblings.
//
// TWO PLACES WE DIVERGE FROM APPLE'S PRINTED EXAMPLE, both deliberate and both
// permitted by the prose rather than by inference:
//
//   - `alert.title` / `alert.body` are STRINGS, where the example prints
//     `loc-key`/`loc-args` dictionaries. The prose says "consider localizing
//     both strings", which is a recommendation over a requirement, and §7.21.4
//     records the reason we decline it: a key the installed build's string
//     table lacks renders as the RAW KEY on a lock screen, and a server that
//     ships ahead of the app is this project's normal state.
//   - No `sound`, where the example prints one. Apple's DTS answer on the
//     purpose of the alert field (developer.apple.com/forums/thread/799195)
//     states that content matters because an alert with NEITHER text NOR sound
//     is a silent notification whose behaviour with Live Activities is
//     undefined. Text is present, so the alert is well-defined; the design asks
//     for an expansion rather than six beeps a ride.

// TestAlertedUpdateWireLayout pins the whole rendered body of an alerting
// update, key order included.
//
// Key ORDER is not something Apple requires and is asserted anyway: it is the
// cheapest possible tripwire on the struct, since any field added, removed or
// retyped in activityAPS moves these bytes and lands the diff in front of a
// reviewer with Apple's requirements written directly above it.
func TestAlertedUpdateWireLayout(t *testing.T) {
	n := testActivityNotification()
	n.ContentState.Status = "arrived"
	n.Alert = &ActivityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseArrived]}

	got := captureActivity(t, n)

	// Built from the same pinned clock the notification is, so the expectation
	// states the RELATIONSHIP between the instants (stale-date is timestamp + 3
	// minutes; eta and asOf are the content's own) rather than restating three
	// unexplained integers.
	want := `{"aps":{` +
		`"timestamp":` + unixOf(fixedNow) + `,` +
		`"event":"update",` +
		`"content-state":{` +
		`"v":1,` +
		`"status":"arrived",` +
		`"eta":` + unixOf(fixedNow.Add(7*time.Minute)) + `,` +
		`"vehicleName":"Blue Whale",` +
		`"destination":"Home",` +
		`"asOf":` + unixOf(fixedNow.Add(-90*time.Second)) + `},` +
		`"stale-date":` + unixOf(fixedNow.Add(StaleAfter)) + `,` +
		`"alert":{"title":"Your ride","body":"Your ride is here"}` +
		`}}`

	if string(got.body) != want {
		t.Errorf("alerting update body mismatch.\n got: %s\nwant: %s", got.body, want)
	}

	// The headers are half the contract: a perfect body under `apns-push-type:
	// alert` is not a Live Activity update at all, and Apple answers the topic
	// and the push type disagreeing with TopicDisallowed — a 403 that reads
	// like a credential failure.
	for header, wantValue := range map[string]string{
		"Apns-Push-Type": "liveactivity",
		"Apns-Topic":     "app.myrobotaxi.ios.push-type.liveactivity",
		"Apns-Priority":  "10",
		"Content-Type":   "application/json",
	} {
		if v := got.header.Get(header); v != wantValue {
			t.Errorf("header %s = %q, want %q on an alerting update", header, v, wantValue)
		}
	}
}

// TestAlertedUpdateIsPriorityTenEvenWhenTheCallerAskedToConserve is the header
// half of ActivityNotification.priority, asserted on the WIRE rather than on
// the method.
//
// Arriving is the one rung of the ladder that does not ride a lifecycle
// transition — the ETA ticker evaluates it — so the single alert in the design
// most likely to be dropped or coalesced originates on exactly the path that
// sets LowPriority. A unit test of priority() would pass with the header
// unwired; this one would not.
func TestAlertedUpdateIsPriorityTenEvenWhenTheCallerAskedToConserve(t *testing.T) {
	n := testActivityNotification()
	n.LowPriority = true
	n.Alert = &ActivityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseArriving]}

	if got := captureActivity(t, n).header.Get("Apns-Priority"); got != priorityImmediate {
		t.Errorf("Apns-Priority = %q on an alerting tick, want %q — an island that "+
			"opens three seconds late has not opened", got, priorityImmediate)
	}
}

// TestAlertKeysAreApplesNotGos guards the one class of bug a Go struct cannot
// catch: Apple decodes the literal strings, so a field rename that forgets its
// tag ships an alert Apple ignores in silence.
//
// Asserted by SUBSTRING on the raw bytes rather than through a decode, because
// a decode is exactly the step that would paper over the defect being tested.
func TestAlertKeysAreApplesNotGos(t *testing.T) {
	n := testActivityNotification()
	n.Alert = &ActivityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseEnroute]}
	body := string(captureActivity(t, n).body)

	for _, literal := range []string{`"alert":{`, `"title":`, `"body":`} {
		if !strings.Contains(body, literal) {
			t.Errorf("rendered body is missing the literal %s — Apple reads the "+
				"JSON key, not the Go field name.\nbody: %s", literal, body)
		}
	}
	// Go's exported field names, which must never reach the wire.
	for _, leaked := range []string{`"Title"`, `"Body"`, `"Alert"`} {
		if strings.Contains(body, leaked) {
			t.Errorf("rendered body contains the Go field name %s", leaked)
		}
	}
}

// unixOf renders an instant as the unix SECOND encoding/json emits for the
// int64 it becomes, so the expectation above reads as a relationship between
// instants rather than as a row of magic numbers. (A float-typed field would
// render as `1.7540352e+09`, which is the other half of why the layout is
// pinned as bytes.)
func unixOf(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// TestCompletedPairWireLayout pins BOTH bytes of MYR-418's completion pair, and
// it is driven through the notifier rather than from a hand-built notification.
//
// WHY THE LOOP IS CLOSED HERE. The keyed test next door proves the notifier
// decides on two pushes with the right fields; TestAlertedUpdateWireLayout
// proves the renderer turns a notification into Apple's JSON. Neither would
// catch the pair being assembled correctly and then rendered into a shape iOS
// ignores — which is precisely the class of defect MYR-418 was: a payload APNs
// accepted, a device did nothing with, and no test looked at. So this one takes
// what the notifier actually produced and puts it on a real socket.
//
// THE REFERENCE, re-verified against Apple's ActivityKit push documentation
// (fetched 2026-08-02). Of an `end` the prose says only: "To end a Live
// Activity, set the value for the `event` key to `end`. If you end a Live
// Activity, include the final content state to make sure the Live Activity
// displays the latest data after it ends." The `alert` dictionary is introduced
// under `start` and `update` and appears in neither the `end` prose nor the
// `end` example. That absence is the whole reason the sixth expansion moved.
func TestCompletedPairWireLayout(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID, UserID: testRiderID, Token: testActivityValue,
		AlertedPhase: AlertPhaseOnTrip,
	}}
	rc := store.context[activityRideID]
	rc.Status = statusCompleted
	// No route: the car has arrived, so there is no arrival time to show and
	// `eta` is an ABSENT KEY rather than a zero. This is the shape §7.21.4
	// prints for a completed ride.
	rc.ETAMinutes = nil
	store.context[activityRideID] = rc

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: statusCompleted,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	endAt := fixedNow.Add(endAfterAlertGap)

	// The alerting update. `progress` is exactly 1 and renders as `1`, not
	// `1.0` — a float that reached the wire as `1e+00` would decode into the
	// Swift Double fine and read as garbage in a packet capture, which is half
	// of why this is asserted as bytes.
	wantUpdate := `{"aps":{` +
		`"timestamp":` + unixOf(fixedNow) + `,` +
		`"event":"update",` +
		`"content-state":{` +
		`"v":1,` +
		`"status":"completed",` +
		`"vehicleName":"Blue Whale",` +
		`"destination":"Home",` +
		`"progress":1,` +
		`"asOf":` + unixOf(fixedNow) + `},` +
		`"stale-date":` + unixOf(fixedNow.Add(StaleAfter)) + `,` +
		`"alert":{"title":"Your ride","body":"You've arrived"}` +
		`}}`

	// The end. Same content-state one second on, a dismissal-date, and NO
	// `alert` key at all — its presence is the regression.
	wantEnd := `{"aps":{` +
		`"timestamp":` + unixOf(endAt) + `,` +
		`"event":"end",` +
		`"content-state":{` +
		`"v":1,` +
		`"status":"completed",` +
		`"vehicleName":"Blue Whale",` +
		`"destination":"Home",` +
		`"progress":1,` +
		`"asOf":` + unixOf(endAt) + `},` +
		`"stale-date":` + unixOf(endAt.Add(StaleAfter)) + `,` +
		`"dismissal-date":` + unixOf(endAt.Add(DismissAfter)) +
		`}}`

	for _, tc := range []struct {
		name string
		n    ActivityNotification
		want string
	}{
		{"alerting update", sent[0], wantUpdate},
		{"end", sent[1], wantEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := captureActivity(t, tc.n)
			if string(got.body) != tc.want {
				t.Errorf("%s body mismatch.\n got: %s\nwant: %s", tc.name, got.body, tc.want)
			}
			// THE 24-HOUR RETENTION FLOOR, ON BOTH HALVES. The end has always
			// had it; the alerting update takes it too (MYR-413), and MYR-418
			// makes that load-bearing rather than incidental — the update is
			// now the sole carrier of the sixth expansion, so a rider in a
			// tunnel at dropoff must still get it when they surface. Left at
			// the three-minute stale-date it would be discarded by Apple.
			wantExp := strconv.FormatInt(tc.n.Timestamp.Add(24*time.Hour).Unix(), 10)
			if got := got.header.Get("Apns-Expiration"); got != wantExp {
				t.Errorf("%s Apns-Expiration = %q, want %q (the day's floor)", tc.name, got, wantExp)
			}
			for header, wantValue := range map[string]string{
				"Apns-Push-Type": "liveactivity",
				"Apns-Topic":     "app.myrobotaxi.ios.push-type.liveactivity",
				"Apns-Priority":  "10",
			} {
				if v := got.header.Get(header); v != wantValue {
					t.Errorf("%s header %s = %q, want %q", tc.name, header, v, wantValue)
				}
			}
		})
	}
}

// TestEndPushRendersNoAlertKey is the byte-level twin of the notifier-level
// invariant: no `end` this surface builds may render an `alert`.
//
// It sets the field by hand — something no production path now does — so that
// the renderer's own behaviour is pinned independently of the caller. If a
// future change reintroduces an alert on an end anywhere, this is the test that
// explains why it will not work.
func TestEndPushRendersNoAlertKey(t *testing.T) {
	dismissAt := fixedNow.Add(DismissAfter)
	n := testActivityNotification()
	n.Event = ActivityEventEnd
	n.DismissalDate = &dismissAt
	n.Alert = &ActivityAlert{Title: alertTitle, Body: alertBodies[AlertPhaseCompleted]}

	body := string(captureActivity(t, n).body)
	if strings.Contains(body, `"alert"`) {
		t.Errorf("an end push rendered an `alert` key. Apple documents the dictionary on `start` "+
			"and `update` only, and a `completed` end that carried one expanded nothing on a real "+
			"device (MYR-418) — the sixth expansion must ride the update that precedes the end.\n"+
			"body: %s", body)
	}
}
