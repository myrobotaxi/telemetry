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

// TestActivityTickUsesConservingPriority pins MYR-194 decision 3: lifecycle
// events take priority over ETA ticks. A tick that shipped at priority 10 would
// spend the same per-Activity budget as an arrival, and Apple would start
// dropping the arrivals.
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
	wantKeys := []string{"v", "status", "eta", "vehicleName", "destination"}
	if got := keysOf(state); !sameSet(got, wantKeys) {
		t.Errorf("content-state keys = %v, want %v", got, wantKeys)
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
