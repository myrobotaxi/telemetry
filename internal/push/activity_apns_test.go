package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Built at runtime rather than as a literal, for the same gosec G101 reason as
// testDeviceValue: a hex constant assigned to a field named *Token is exactly
// the shape the hardcoded-credential rule flags, and it is not one.
var testActivityValue = strings.Repeat("11223344", 4)

// fixedNow is the pinned clock for every wire assertion in this file. Instants
// ARE the contract on this surface — a stale-date three minutes off is the
// difference between honest staleness and a confident lie — so nothing here
// tolerates a real clock.
var fixedNow = time.Date(2026, 7, 30, 18, 4, 5, 0, time.UTC)

func testActivityNotification() ActivityNotification {
	eta := fixedNow.Add(7 * time.Minute).Unix()
	// asOf lags the send instant by ninety seconds — the ordinary shape of a
	// push whose nav reading is a minute and a half old, and a value no test
	// here can confuse with `aps.timestamp` by accident (MYR-398).
	asOf := fixedNow.Add(-90 * time.Second).Unix()
	return ActivityNotification{
		ActivityToken: testActivityValue,
		Event:         ActivityEventUpdate,
		Timestamp:     fixedNow,
		ContentState: ActivityContentState{
			Version:     ActivityContentStateVersion,
			Status:      "enroute",
			ETA:         &eta,
			VehicleName: "Blue Whale",
			Destination: "Home",
			AsOf:        &asOf,
		},
	}
}

type capturedActivity struct {
	path   string
	header http.Header
	body   []byte
}

// captureActivity runs one SendActivity against an httptest server and returns
// what reached the wire.
func captureActivity(t *testing.T, n ActivityNotification) capturedActivity {
	t.Helper()
	var got capturedActivity
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = capturedActivity{path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).SendActivity(context.Background(), n); err != nil {
		t.Fatalf("SendActivity() error = %v", err)
	}
	return got
}

// TestActivityRequestHeaders pins the three headers that make this a Live
// Activity rather than an alert.
//
// The topic and the push type are asserted together deliberately: Apple rejects
// the pair being half-right with TopicDisallowed, a 403 that reads like a
// credential failure and sends you looking at the signing key for an hour.
func TestActivityRequestHeaders(t *testing.T) {
	n := testActivityNotification()
	got := captureActivity(t, n)

	if want := "/3/device/" + testActivityValue; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}

	for header, want := range map[string]string{
		"Apns-Topic":      "app.myrobotaxi.ios.push-type.liveactivity",
		"Apns-Push-Type":  "liveactivity",
		"Apns-Priority":   "10",
		"Content-Type":    "application/json",
		"Apns-Expiration": strconv.FormatInt(fixedNow.Add(StaleAfter).Unix(), 10),
	} {
		if v := got.header.Get(header); v != want {
			t.Errorf("header %s = %q, want %q", header, v, want)
		}
	}
}

// TestActivityTickUsesConservingPriority pins the header MAPPING for the
// retreat path: no production caller sets LowPriority since MYR-573, and this
// is what keeps the one-line retreat honest — if the flag comes back, it must
// still render apns-priority 5.
func TestActivityTickUsesConservingPriority(t *testing.T) {
	n := testActivityNotification()
	n.LowPriority = true

	if got := captureActivity(t, n).header.Get("Apns-Priority"); got != "5" {
		t.Errorf("Apns-Priority = %q, want 5 for an ETA tick", got)
	}
}

// TestAlertPathHeadersUnchanged guards the refactor that made room for the
// Activity path. Threading an apnsMessage through the transport must not have
// moved the alert wire by a byte — the alert push is the shipped, live feature.
func TestAlertPathHeadersUnchanged(t *testing.T) {
	var got capturedActivity
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = capturedActivity{header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if got.header.Get("Apns-Push-Type") != "alert" {
		t.Errorf("alert Apns-Push-Type = %q, want alert", got.header.Get("Apns-Push-Type"))
	}
	if got.header.Get("Apns-Topic") != "app.myrobotaxi.ios" {
		t.Errorf("alert Apns-Topic = %q, want the bare bundle id", got.header.Get("Apns-Topic"))
	}
	// The alert path sets no expiration and must not have inherited one.
	if v := got.header.Get("Apns-Expiration"); v != "" {
		t.Errorf("alert Apns-Expiration = %q, want absent", v)
	}
}

// TestActivityPayloadRawJSONKeys asserts the payload by its RAW JSON KEYS.
//
// This is the MYR-362 lesson written down: the Swift ContentState decodes these
// exact hyphenated and camelCase strings, and a Go-side field rename that a
// struct-level assertion would sail through is a silent decode failure on every
// installed phone. Nothing here reads through the Go types.
func TestActivityPayloadRawJSONKeys(t *testing.T) {
	got := captureActivity(t, testActivityNotification())

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	// An Activity update carries NO userInfo: the token already addresses one
	// Activity on one ride, so a ride id outside `aps` would be an identifier
	// on the wire buying nothing.
	if len(payload) != 1 {
		t.Errorf("payload top-level keys = %v, want exactly aps", keysOf(payload))
	}

	aps, ok := payload["aps"].(map[string]any)
	if !ok {
		t.Fatalf("aps is %T, want object", payload["aps"])
	}

	if got, want := aps["timestamp"], float64(fixedNow.Unix()); got != want {
		t.Errorf("aps.timestamp = %v, want %v", got, want)
	}
	if got := aps["event"]; got != "update" {
		t.Errorf("aps.event = %v, want update", got)
	}
	if got, want := aps["stale-date"], float64(fixedNow.Add(StaleAfter).Unix()); got != want {
		t.Errorf("aps.stale-date = %v, want %v (timestamp + 3 min, MYR-194)", got, want)
	}
	if _, present := aps["dismissal-date"]; present {
		t.Error("aps.dismissal-date present on an update event; it belongs only to an end")
	}

	state, ok := aps["content-state"].(map[string]any)
	if !ok {
		t.Fatalf("aps.content-state is %T, want object", aps["content-state"])
	}
	wantKeys := []string{"v", "status", "eta", "vehicleName", "destination", "asOf"}
	if got := keysOf(state); !sameSet(got, wantKeys) {
		t.Errorf("content-state keys = %v, want %v", got, wantKeys)
	}
	// asOf and aps.timestamp are DIFFERENT INSTANTS and the payload must show
	// it. Asserting the gap rather than the value is the point: a refactor that
	// stamped asOf from the send clock would satisfy any equality test written
	// against `now` and would silently destroy the field's only job.
	if got, want := state["asOf"], float64(fixedNow.Add(-90*time.Second).Unix()); got != want {
		t.Errorf("content-state.asOf = %v, want %v (when the DATA was true)", got, want)
	}
	if state["asOf"] == aps["timestamp"] {
		t.Error("content-state.asOf equals aps.timestamp; the two must be able to differ, " +
			"or a card whose data has frozen can never say so")
	}
	// No alert on a routine update — the island stays shut (MYR-398).
	if _, present := aps["alert"]; present {
		t.Errorf("aps.alert = %v on a routine update; the island must expand only on "+
			"the six phase changes", aps["alert"])
	}
	if got, want := state["v"], float64(ActivityContentStateVersion); got != want {
		t.Errorf("content-state.v = %v, want %v", got, want)
	}
	if got := state["status"]; got != "enroute" {
		t.Errorf("content-state.status = %v, want enroute", got)
	}
	if got, want := state["eta"], float64(fixedNow.Add(7*time.Minute).Unix()); got != want {
		t.Errorf("content-state.eta = %v, want %v (an absolute instant, not a duration)", got, want)
	}
	if got := state["vehicleName"]; got != "Blue Whale" {
		t.Errorf("content-state.vehicleName = %v, want Blue Whale", got)
	}
	if got := state["destination"]; got != "Home" {
		t.Errorf("content-state.destination = %v, want Home", got)
	}
}

// TestActivityPayloadOmitsUnknownETA pins the honesty rule: no nav route means
// no `eta` key at all. There is no route solver in this service, so the only
// alternative to omitting it is inventing a number — which MYR-194 forbids in
// as many words.
func TestActivityPayloadOmitsUnknownETA(t *testing.T) {
	n := testActivityNotification()
	n.ContentState.ETA = nil

	got := captureActivity(t, n)
	var payload struct {
		APS struct {
			ContentState map[string]any `json:"content-state"`
		} `json:"aps"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, present := payload.APS.ContentState["eta"]; present {
		t.Error("content-state.eta emitted with no known ETA; the key must be absent, not null or zero")
	}
	// The other four are NOT omitempty — a missing status or vehicleName would
	// fail to decode into the Swift struct's non-optional fields.
	for _, key := range []string{"v", "status", "vehicleName", "destination"} {
		if _, present := payload.APS.ContentState[key]; !present {
			t.Errorf("content-state.%s missing; only eta is optional", key)
		}
	}
}

// TestActivityEndCarriesDismissalDate pins the end shape.
func TestActivityEndCarriesDismissalDate(t *testing.T) {
	dismissAt := fixedNow.Add(DismissAfter)
	n := testActivityNotification()
	n.Event = ActivityEventEnd
	n.DismissalDate = &dismissAt

	got := captureActivity(t, n)
	var payload struct {
		APS struct {
			Event         string `json:"event"`
			DismissalDate *int64 `json:"dismissal-date"`
		} `json:"aps"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.APS.Event != "end" {
		t.Errorf("aps.event = %q, want end", payload.APS.Event)
	}
	if payload.APS.DismissalDate == nil {
		t.Fatal("aps.dismissal-date absent on an end event")
	}
	if *payload.APS.DismissalDate != dismissAt.Unix() {
		t.Errorf("aps.dismissal-date = %d, want %d", *payload.APS.DismissalDate, dismissAt.Unix())
	}
}

// TestActivityExpirationPerShape is the MYR-172 review fix, and the assertion
// is per-SHAPE on purpose: `update` and `end` want opposite things from
// apns-expiration, and the original code gave both the stale-date.
//
// An update that arrives after its content stopped being trustworthy is worse
// than one that never arrives, so it expires with the stale-date. An `end` is
// the only push here with NO successor — the rows are tombstoned as it is sent
// and nothing ever retries — so a three-minute expiration meant a phone in a
// tunnel kept "your car is on its way" on the lock screen for hours after the
// ride was declined. It now outlives a flat battery.
//
// MYR-413 added a third shape between them: an ALERTING update. It is an
// `update`, so it used to take the stale-date, but the duplicate-banner gate
// made it the sole carrier of news that used to travel on a banner APNs stored
// and retried, so it takes the day's floor too.
func TestActivityExpirationPerShape(t *testing.T) {
	promptly := fixedNow.Add(DismissPromptly)
	linger := fixedNow.Add(DismissAfter)
	distant := fixedNow.Add(72 * time.Hour)

	tests := []struct {
		name      string
		event     ActivityEvent
		dismissAt *time.Time
		alert     *ActivityAlert
		want      time.Time
	}{
		{
			name:  "update expires at its stale-date",
			event: ActivityEventUpdate,
			want:  fixedNow.Add(StaleAfter),
		},
		{
			// MYR-413. An ORDINARY tick above keeps the three-minute
			// expiration because a late one is worthless. An ALERTING one
			// does not, because since the duplicate-banner gate it is the
			// only thing that tells a rider watching a card that their car
			// arrived — the durable banner it replaced is not sent. Late is
			// safe here: the payload carries its own stale-date and
			// aps.timestamp ordering stops it overwriting a newer tick.
			name:  "alerting update outlives the phone being offline",
			event: ActivityEventUpdate,
			alert: &ActivityAlert{Title: alertTitle, Body: "Your car is here."},
			want:  fixedNow.Add(alertingUpdateRetention),
		},
		{
			// The regression guard. DismissPromptly is 30 SECONDS, so pinning
			// the expiration to the dismissal-date would have made the most
			// important push in the feature the shortest-lived one — worse
			// than the bug being fixed.
			name:      "declined end outlives its 30s dismissal-date",
			event:     ActivityEventEnd,
			dismissAt: &promptly,
			want:      fixedNow.Add(endPushRetention),
		},
		{
			name:      "completed end outlives its 5m dismissal-date",
			event:     ActivityEventEnd,
			dismissAt: &linger,
			want:      fixedNow.Add(endPushRetention),
		},
		{
			name:  "end with no dismissal-date still gets the full day",
			event: ActivityEventEnd,
			want:  fixedNow.Add(endPushRetention),
		},
		{
			// The one case where the dismissal-date wins: it is genuinely
			// later, so delivering past it really would be pointless.
			name:      "a dismissal-date beyond the floor is honoured",
			event:     ActivityEventEnd,
			dismissAt: &distant,
			want:      distant,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := testActivityNotification()
			n.Event = tc.event
			n.DismissalDate = tc.dismissAt
			n.Alert = tc.alert

			got := captureActivity(t, n).header.Get("Apns-Expiration")
			if want := strconv.FormatInt(tc.want.Unix(), 10); got != want {
				t.Errorf("Apns-Expiration = %q, want %q", got, want)
			}
		})
	}
}

// TestActivityEndExpirationOutlivesTheStaleDate states the invariant the table
// above enforces case by case, so a future edit that changes every constant at
// once still has to keep the property.
func TestActivityEndExpirationOutlivesTheStaleDate(t *testing.T) {
	n := testActivityNotification()
	n.Event = ActivityEventEnd
	dismissAt := fixedNow.Add(DismissPromptly)
	n.DismissalDate = &dismissAt

	if got := activityExpiration(n); !got.After(n.StaleDate()) {
		t.Errorf("end expiration = %s, want later than the stale-date %s — an end push"+
			" that expires with the content is one an offline phone never receives",
			got, n.StaleDate())
	}
}

// TestActivityAlertingUpdateExpirationOutlivesTheStaleDate is the sibling
// invariant for MYR-413, stated so that a future edit which moves every
// constant at once still has to keep the property.
//
// The asymmetry it guards is the whole point: the banner the gate deletes had
// NO apns-expiration, so APNs stored and retried it for an offline phone. If
// the alerting update that inherited its job expires at the three-minute
// stale-date, a rider in a tunnel when the car reaches the kerb reconnects to
// nothing at all — neither surface ever told them. The ordinary tick beside it
// must keep the short expiration, because a late ETA refresh really is
// worthless, so the two are asserted together.
func TestActivityAlertingUpdateExpirationOutlivesTheStaleDate(t *testing.T) {
	n := testActivityNotification()
	n.Event = ActivityEventUpdate
	n.Alert = &ActivityAlert{Title: alertTitle, Body: "Your car is here."}

	if got := activityExpiration(n); !got.After(n.StaleDate()) {
		t.Errorf("alerting update expiration = %s, want later than the stale-date %s —"+
			" since the duplicate-banner gate this push is the only thing that tells a"+
			" rider watching a card that their car arrived, and an offline phone must"+
			" still get it", got, n.StaleDate())
	}

	n.Alert = nil
	if got, want := activityExpiration(n), n.StaleDate(); !got.Equal(want) {
		t.Errorf("ordinary update expiration = %s, want the stale-date %s — a late ETA"+
			" tick is worthless and must still be dropped by Apple", got, want)
	}
}

// TestActivityTopicDerivation pins the suffix rather than trusting a literal
// spread across the sender and its test.
func TestActivityTopicDerivation(t *testing.T) {
	if got, want := activityTopic("app.myrobotaxi.ios"), "app.myrobotaxi.ios.push-type.liveactivity"; got != want {
		t.Errorf("activityTopic() = %q, want %q", got, want)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	index := make(map[string]bool, len(got))
	for _, k := range got {
		index[k] = true
	}
	for _, k := range want {
		if !index[k] {
			return false
		}
	}
	return true
}
