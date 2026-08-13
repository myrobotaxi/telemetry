package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCollapseIDValue pins the id's SHAPE — the thing every other property in
// this file depends on. It is the notification's intent (`{rideID}:{topic}`)
// and nothing per-attempt.
func TestCollapseIDValue(t *testing.T) {
	tests := []struct {
		name  string
		ride  string
		topic string
		want  string
	}{
		{
			name:  "ride and topic",
			ride:  "ride_abc123",
			topic: "ride.due",
			want:  "ride_abc123:ride.due",
		},
		{
			name:  "distinct per topic for one ride",
			ride:  "ride_abc123",
			topic: "ride.status.changed",
			want:  "ride_abc123:ride.status.changed",
		},
		{
			name:  "empty ride id yields no header at all",
			ride:  "",
			topic: "ride.due",
			want:  "",
		},
		{
			name:  "empty topic still keys on the ride",
			ride:  "ride_abc123",
			topic: "",
			want:  "ride_abc123:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := collapseID(tt.ride, tt.topic); got != tt.want {
				t.Errorf("collapseID(%q, %q) = %q, want %q", tt.ride, tt.topic, got, tt.want)
			}
		})
	}
}

// TestCollapseIDRespectsAppleCap pins the 64-byte limit from the APNs provider
// API. Over-length is the one failure mode that would SILENCE the push (400
// BadCollapseId), so the truncation matters more than the collision it can in
// principle cause.
func TestCollapseIDRespectsAppleCap(t *testing.T) {
	tests := []struct {
		name  string
		ride  string
		topic string
	}{
		{name: "long ride id", ride: strings.Repeat("r", 200), topic: "ride.due"},
		{name: "long topic", ride: "ride_abc123", topic: strings.Repeat("t", 200)},
		{name: "multibyte tail", ride: strings.Repeat("é", 40), topic: "ride.due"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseID(tt.ride, tt.topic)
			if len(got) > maxCollapseIDBytes {
				t.Errorf("collapseID() = %d bytes, want <= %d", len(got), maxCollapseIDBytes)
			}
			// A truncated id must still be valid UTF-8 — a half-encoded rune in
			// a header is a 400 by another route.
			for _, r := range got {
				if r == '�' {
					t.Errorf("collapseID() = %q, want no partial rune", got)
				}
			}
		})
	}
}

// TestSendSetsCollapseIDOnAlerts is the wire assertion: the header is present,
// carries the intent id, and is a plain alert header — nothing else about the
// request changed.
func TestSendSetsCollapseIDOnAlerts(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("apns-collapse-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotification()
	n.EventTopic = "ride.due"
	if err := newTestClient(t, srv.URL).Send(context.Background(), n); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if want := "ride_abc123:ride.due"; got != want {
		t.Errorf("apns-collapse-id = %q, want %q", got, want)
	}
}

// TestSendOmitsCollapseIDWithoutRideID pins the safety rule: an alert with no
// ride to key on carries NO header, rather than one keyed on the topic alone —
// which would merge unrelated notifications into a single lock-screen line.
func TestSendOmitsCollapseIDWithoutRideID(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Apns-Collapse-Id"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotification()
	n.RideID = ""
	n.EventTopic = "ride.due"
	if err := newTestClient(t, srv.URL).Send(context.Background(), n); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if present {
		t.Error("apns-collapse-id header present for a ride-less alert, want absent")
	}
}

// TestSendCollapseIDIsStableAcrossTheRetry is the WHOLE POINT of MYR-554: the
// prod duplicate came from deliver()'s one retry after Apple had already
// accepted the first POST, so both attempts must present the SAME id for
// Apple's de-duplication to collapse them.
func TestSendCollapseIDIsStableAcrossTheRetry(t *testing.T) {
	var (
		mu  sync.Mutex
		ids []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("apns-collapse-id"))
		mu.Unlock()
		// 500 is retryable, so deliver() runs its second attempt.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := testNotification()
	n.EventTopic = "ride.due"
	if err := newTestClient(t, srv.URL).Send(context.Background(), n); err == nil {
		t.Fatal("Send() error = nil, want the 500 to surface")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 {
		t.Fatalf("attempts = %d, want 2 (one try plus one retry)", len(ids))
	}
	if ids[0] != ids[1] {
		t.Errorf("apns-collapse-id differed across the retry: %q then %q", ids[0], ids[1])
	}
	if want := "ride_abc123:ride.due"; ids[0] != want {
		t.Errorf("apns-collapse-id = %q, want %q", ids[0], want)
	}
}

// TestSendCollapseIDIsDistinctPerRideAndTopic pins the other half: collapsing
// must merge one notification's redelivery and NOTHING else. Two rides, and one
// ride's two different topics, are three separate ids.
func TestSendCollapseIDIsDistinctPerRideAndTopic(t *testing.T) {
	var (
		mu  sync.Mutex
		ids []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("apns-collapse-id"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	sends := []struct{ ride, topic string }{
		{ride: "ride_abc123", topic: "ride.due"},
		{ride: "ride_abc123", topic: "ride.status.changed"},
		{ride: "ride_zzz999", topic: "ride.due"},
	}
	for _, s := range sends {
		n := testNotification()
		n.RideID = s.ride
		n.EventTopic = s.topic
		if err := client.Send(context.Background(), n); err != nil {
			t.Fatalf("Send() error = %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]int{}
	for _, id := range ids {
		seen[id]++
	}
	if len(seen) != len(sends) {
		t.Errorf("distinct collapse ids = %d (%v), want %d", len(seen), seen, len(sends))
	}
}

// TestActivityPushCarriesNoCollapseID pins the untouched half. An ActivityKit
// update addresses one card through its own token, so it has nothing to
// collapse against; a header here would merge the ETA ticker's updates on
// Apple's terms rather than ActivityKit's.
func TestActivityPushCarriesNoCollapseID(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Apns-Collapse-Id"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.SendActivity(context.Background(), ActivityNotification{
		ActivityToken: testDeviceValue,
		ContentState:  ActivityContentState{Status: "accepted"},
		Event:         ActivityEventUpdate,
	})
	if err != nil {
		t.Fatalf("SendActivity() error = %v", err)
	}

	if present {
		t.Error("apns-collapse-id header present on an ActivityKit push, want absent")
	}
}

// TestFanOutCarriesTheEventTopic pins the WIRING: the collapse id is only as
// good as the topic that reaches the Notification, and every fan-out site
// resolves it from the delivery it built. Without this the header would be
// keyed on the ride alone and two unrelated alerts for one ride would merge.
func TestFanOutCarriesTheEventTopic(t *testing.T) {
	tests := []struct {
		name      string
		fire      func(*Notifier)
		wantTopic string
	}{
		{
			name:      "created",
			fire:      func(n *Notifier) { n.handleCreated(createdEvent(strptr("Ada"), nil)) },
			wantTopic: "ride.request.created",
		},
		{
			name:      "status changed",
			fire:      func(n *Notifier) { n.handleStatusChanged(statusEvent("accepted")) },
			wantTopic: "ride.status.changed",
		},
		{
			name:      "due",
			fire:      func(n *Notifier) { n.handleDue(dueEvent()) },
			wantTopic: "ride.due",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			tt.fire(n)
			n.Wait()

			sent := sender.Sent()
			if len(sent) == 0 {
				t.Fatal("sent 0 notifications, want at least 1")
			}
			for i := range sent {
				if sent[i].EventTopic != tt.wantTopic {
					t.Errorf("notification %d EventTopic = %q, want %q", i, sent[i].EventTopic, tt.wantTopic)
				}
				if got, want := collapseID(sent[i].RideID, sent[i].EventTopic), testRideID+":"+tt.wantTopic; got != want {
					t.Errorf("collapse id = %q, want %q", got, want)
				}
			}
		})
	}
}
