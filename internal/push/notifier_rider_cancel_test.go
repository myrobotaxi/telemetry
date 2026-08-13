package push

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-537 — the OWNER's push when the RIDER cancels a ride the owner had
// committed to. The client's directive widened the rider's cancel window to
// every live status; the push is the other half, because a mid-ride cancel
// cannot clear the car's dash nav (Tesla has no cancel-navigation API) — the
// owner being TOLD is the stand-down.

func riderCancelEvent(previous string, cancelledBy *string, requester *string) events.Event {
	return events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID:  testRideID,
		VehicleID:      testVehicleID,
		RiderID:        testRiderID,
		OwnerID:        testOwnerID,
		Status:         "cancelled",
		PreviousStatus: previous,
		CancelledBy:    cancelledBy,
		RequesterName:  requester,
		UpdatedAt:      time.Now(),
	})
}

// The audience matrix: a rider cancel wakes the OWNER exactly when the owner
// had committed — accepted, arrived, enroute — and stays silent from
// `requested` (the owner never accepted; paging them for a rider changing
// their mind in the booking flow is noise). An event with NO previous status
// (published by a pre-MYR-537 server) reads as the silent arm.
func TestNotifierRiderCancelWakesTheCommittedOwner(t *testing.T) {
	rider := "rider"
	tests := []struct {
		name     string
		previous string
		want     int
	}{
		{name: "from accepted", previous: "accepted", want: 1},
		{name: "from arrived", previous: "arrived", want: 1},
		{name: "from enroute (aboard)", previous: "enroute", want: 1},
		{name: "from requested stays silent", previous: "requested", want: 0},
		{name: "no previous status stays silent", previous: "", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			n.handleStatusChanged(riderCancelEvent(tt.previous, &rider, strptr("Sam")))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != tt.want {
				t.Fatalf("sent %d notifications, want %d", len(sent), tt.want)
			}
			if tt.want == 1 {
				if sent[0].DeviceToken != ownerDevice {
					t.Errorf("device = %q, want the OWNER's device", sent[0].DeviceToken)
				}
				if sent[0].Title != "Sam cancelled the ride" {
					t.Errorf("title = %q", sent[0].Title)
				}
				if sent[0].Body != "No need to continue — the ride has ended." {
					t.Errorf("body = %q", sent[0].Body)
				}
			}
		})
	}
}

// A nameless rider gets the anonymous title, never a blank interpolation.
func TestNotifierRiderCancelFallsBackAnonymously(t *testing.T) {
	rider := "rider"
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{})

	n.handleStatusChanged(riderCancelEvent("enroute", &rider, nil))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d, want 1", len(sent))
	}
	if sent[0].Title != "Your rider cancelled the ride" {
		t.Errorf("title = %q", sent[0].Title)
	}
}

// The OWNER's own cancel must not trigger the rider-cancel arm on top of its
// existing rider push — MYR-522's fork and MYR-537's are keyed on the same
// stamp and must stay disjoint.
func TestNotifierOwnerCancelDoesNotTriggerTheRiderCancelArm(t *testing.T) {
	owner := "owner"
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(riderCancelEvent("accepted", &owner, strptr("Sam")))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d, want 1 — the rider's 'had to cancel' alone", len(sent))
	}
	if sent[0].DeviceToken != riderDevice {
		t.Errorf("device = %q, want the RIDER's device", sent[0].DeviceToken)
	}
}

// A SELF-RIDE cancel (owner == rider) wakes nobody: the standing suppression
// — a phone must not buzz to report its own thumb — covers the new arm too.
func TestNotifierSelfRideCancelStaysSilent(t *testing.T) {
	rider := "rider"
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	evt := events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID:  testRideID,
		VehicleID:      testVehicleID,
		RiderID:        testRiderID,
		OwnerID:        testRiderID, // one account in both roles
		Status:         "cancelled",
		PreviousStatus: "enroute",
		CancelledBy:    &rider,
		UpdatedAt:      time.Now(),
	})
	n.handleStatusChanged(evt)
	n.Wait()

	if got := sender.Sent(); len(got) != 0 {
		t.Errorf("sent %d notifications on a self-ride cancel, want 0: %+v", len(got), got)
	}
}
