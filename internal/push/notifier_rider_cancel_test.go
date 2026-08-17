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

// The audience matrix: EVERY rider cancel wakes the OWNER, and the pre-check
// status picks which sentence they get. `requested` is the MYR-585 arm — the
// owner is holding a request push that now leads nowhere, and until this issue
// it was the one cancellation nobody was told about. Committed statuses keep
// MYR-537's stand-down wording. An event with NO previous status (published by
// a pre-MYR-537 server) still reads as the silent arm — it cannot be classified,
// and guessing would put the wrong sentence on a lock screen.
func TestNotifierRiderCancelWakesTheOwner(t *testing.T) {
	rider := "rider"
	tests := []struct {
		name      string
		previous  string
		want      int
		wantTitle string
		wantBody  string
	}{
		{
			name: "from requested — the withdrawn request (MYR-585)", previous: "requested", want: 1,
			wantTitle: "Sam cancelled their ride request",
			wantBody:  "Nothing to do — the request was withdrawn.",
		},
		{
			name: "from accepted", previous: "accepted", want: 1,
			wantTitle: "Sam cancelled the ride",
			wantBody:  "No need to continue — the ride has ended.",
		},
		{
			name: "from arrived", previous: "arrived", want: 1,
			wantTitle: "Sam cancelled the ride",
			wantBody:  "No need to continue — the ride has ended.",
		},
		{
			name: "from enroute (aboard)", previous: "enroute", want: 1,
			wantTitle: "Sam cancelled the ride",
			wantBody:  "No need to continue — the ride has ended.",
		},
		{name: "no previous status stays silent", previous: "", want: 0},
		{name: "an already-terminal previous status stays silent", previous: "completed", want: 0},
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
			if tt.want == 0 {
				return
			}
			if sent[0].DeviceToken != ownerDevice {
				t.Errorf("device = %q, want the OWNER's device", sent[0].DeviceToken)
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
			if sent[0].Body != tt.wantBody {
				t.Errorf("body = %q, want %q", sent[0].Body, tt.wantBody)
			}
		})
	}
}

// A nameless rider gets the anonymous title on BOTH arms, never a blank
// interpolation. The requester name is resolved from a person's account, and a
// deleted or never-named account resolves to nothing (data-classification.md
// §1.15) — which is a live production shape, not a theoretical one.
func TestNotifierRiderCancelFallsBackAnonymously(t *testing.T) {
	rider := "rider"
	tests := []struct {
		name      string
		previous  string
		requester *string
		wantTitle string
	}{
		{name: "nil name, committed ride", previous: "enroute", requester: nil, wantTitle: "Your rider cancelled the ride"},
		{name: "nil name, withdrawn request", previous: "requested", requester: nil, wantTitle: "Your rider cancelled their ride request"},
		{name: "blank name, withdrawn request", previous: "requested", requester: strptr("   "), wantTitle: "Your rider cancelled their ride request"},
		{name: "blank name, committed ride", previous: "accepted", requester: strptr(""), wantTitle: "Your rider cancelled the ride"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{})

			n.handleStatusChanged(riderCancelEvent(tt.previous, &rider, tt.requester))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d, want 1", len(sent))
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
		})
	}
}

// The withdrawn-request push is a RIDE_LIFECYCLE notification like every other
// one here — MYR-585 adds no sixth §7.19 category. An owner who silenced the
// class hears nothing, including this.
func TestNotifierRequestCancelObeysTheLifecyclePreference(t *testing.T) {
	rider := "rider"
	prefs := newFakePrefStore()
	silenced := DefaultPrefs()
	silenced.RideLifecycle = false
	prefs.byUser[testOwnerID] = silenced
	n, sender := notifierWithPrefs(t, prefs)

	n.handleStatusChanged(riderCancelEvent("requested", &rider, strptr("Sam")))
	n.Wait()

	if got := sender.Sent(); len(got) != 0 {
		t.Errorf("sent %d notification(s) to an owner who silenced ride_lifecycle, want 0: %+v", len(got), got)
	}
}

// SELF-SUPPRESSION, the standing rule (MYR-548): the canceller never gets their
// own push. A self-ride owner cancelling their own REQUEST is the MYR-585 shape
// of it — the new arm inherits the suppression rather than re-deriving it.
func TestNotifierSelfRideRequestCancelStaysSilent(t *testing.T) {
	rider := "rider"
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{})

	evt := events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID:  testRideID,
		VehicleID:      testVehicleID,
		RiderID:        testRiderID,
		OwnerID:        testRiderID, // one account in both roles
		Status:         "cancelled",
		PreviousStatus: "requested",
		CancelledBy:    &rider,
		RequesterName:  strptr("Sam"),
		UpdatedAt:      time.Now(),
	})
	n.handleStatusChanged(evt)
	n.Wait()

	if got := sender.Sent(); len(got) != 0 {
		t.Errorf("sent %d notification(s) on a self-ride request cancel, want 0: %+v", len(got), got)
	}
}

// The RIDER is never told about their own cancellation, in either direction:
// the rider-side copy function has no `cancelled` arm that a rider-stamped
// event can reach. This is the negative that keeps the two forks disjoint — a
// regression here would buzz the phone whose thumb did it.
func TestNotifierRiderCancelNeverReachesTheRider(t *testing.T) {
	rider := "rider"
	for _, previous := range []string{"requested", "accepted", "arrived", "enroute"} {
		t.Run("from "+previous, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			n.handleStatusChanged(riderCancelEvent(previous, &rider, strptr("Sam")))
			n.Wait()

			for _, s := range sender.Sent() {
				if s.DeviceToken == riderDevice {
					t.Errorf("the cancelling RIDER got their own push: %+v", s)
				}
			}
		})
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
