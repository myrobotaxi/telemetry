package push

import (
	"errors"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-527 — the owner's "check the dash" push, the human fallback for a nav
// share the car never applied. The audience is the OWNER alone: the fix (touch
// the dash) is only theirs to take, and the rider's surface is already showing
// the honest wrong numbers the fix would correct.

func navUnappliedEvent() events.Event {
	return events.NewEvent(events.RideNavUnappliedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		OwnerID:       testOwnerID,
		Leg:           "dropoff",
	})
}

func TestNotifierNavUnappliedNotifiesOwner(t *testing.T) {
	tests := []struct {
		name        string
		vehicleName string
		namerErr    error
		wantTitle   string
	}{
		{name: "names the car", vehicleName: "Blue Whale", wantTitle: "Blue Whale may not have the route"},
		{name: "unnamed car falls back", wantTitle: "Your car may not have the route"},
		{name: "lookup failure falls back", namerErr: errors.New("db down"), wantTitle: "Your car may not have the route"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: tt.vehicleName, err: tt.namerErr})

			n.handleNavUnapplied(navUnappliedEvent())
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want 1 (the owner alone)", len(sent))
			}
			if sent[0].DeviceToken != ownerDevice {
				t.Errorf("device = %q, want the OWNER's device", sent[0].DeviceToken)
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
			if sent[0].Body != "Check the dash and set the destination manually if needed." {
				t.Errorf("body = %q", sent[0].Body)
			}
		})
	}
}

// The payload policy sweep: the alert carries the vehicle nickname and nothing
// about the ride — no leg label, no destination, no coordinates.
func TestNotifierNavUnappliedCarriesNoPlace(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleNavUnapplied(navUnappliedEvent())
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d, want 1", len(sent))
	}
	for _, forbidden := range []string{"dropoff", "pickup", "Fort", "37.", "-96."} {
		payload := sent[0].Title + " | " + sent[0].Body
		if strings.Contains(payload, forbidden) {
			t.Errorf("payload %q leaks %q", payload, forbidden)
		}
	}
}
