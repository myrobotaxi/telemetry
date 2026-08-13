package push

import (
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-541 — trip-edit pushes: the EDITOR hears nothing, everyone else does,
// and the copy names the edited PART, never the place.

func tripChangedEvent(editor string, pickup, dropoff bool) events.Event {
	place := events.RidePlace{Latitude: 1, Longitude: 2, Label: "Secret Place"}
	ev := events.RideTripChangedEvent{
		RideRequestID: testRideID, VehicleID: testVehicleID,
		RiderID: testRiderID, OwnerID: testOwnerID,
		EditorUserID: editor, Status: "enroute",
		RequesterName: strptr("Sam"), UpdatedAt: time.Now(),
	}
	if pickup {
		ev.NewPickup = &place
	}
	if dropoff {
		ev.NewDropoff = &place
	}
	return events.NewEvent(ev)
}

func TestNotifierTripChangedAudienceAndCopy(t *testing.T) {
	tests := []struct {
		name       string
		editor     string
		wantDevice string
		wantTitle  string
	}{
		{name: "rider edited: owner told", editor: testRiderID, wantDevice: ownerDevice, wantTitle: "Sam changed the drop-off"},
		{name: "owner edited: rider told", editor: testOwnerID, wantDevice: riderDevice, wantTitle: "Your drop-off was changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			n.handleTripChanged(tripChangedEvent(tt.editor, false, true))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d, want 1 (the non-editor alone)", len(sent))
			}
			if sent[0].DeviceToken != tt.wantDevice || sent[0].Title != tt.wantTitle {
				t.Errorf("got %q to %q", sent[0].Title, sent[0].DeviceToken)
			}
			if sent[0].Body != "Open MyRoboTaxi to see the details." {
				t.Errorf("body = %q", sent[0].Body)
			}
			for _, s := range sent {
				if contains := s.Title + s.Body; len(contains) > 0 && (strings.Contains(contains, "Secret") || strings.Contains(contains, "1.0")) {
					t.Errorf("payload leaks the place: %q", contains)
				}
			}
		})
	}
}

// A TripEdit-marked status event must NOT re-fire the status copy — no
// spurious "Your ride is confirmed" when somebody moves the drop-off.
func TestNotifierTripEditStatusFrameStaysSilent(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: testRideID, VehicleID: testVehicleID,
		RiderID: testRiderID, OwnerID: testOwnerID,
		Status: "accepted", TripEdit: true, TripVersion: 1,
		UpdatedAt: time.Now(),
	}))
	n.Wait()

	if got := sender.Sent(); len(got) != 0 {
		t.Errorf("a trip-edit status frame sent %d pushes, want 0: %+v", len(got), got)
	}
}

// A self-ride editor is both parties: nobody is pushed.
func TestNotifierTripChangedSelfRideStaysSilent(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{})

	ev := events.RideTripChangedEvent{
		RideRequestID: testRideID, VehicleID: testVehicleID,
		RiderID: testRiderID, OwnerID: testRiderID,
		EditorUserID: testRiderID, Status: "enroute",
		UpdatedAt: time.Now(),
	}
	place := events.RidePlace{Latitude: 1, Longitude: 2, Label: "X"}
	ev.NewDropoff = &place
	n.handleTripChanged(events.NewEvent(ev))
	n.Wait()

	if got := sender.Sent(); len(got) != 0 {
		t.Errorf("self-ride edit sent %d pushes, want 0", len(got))
	}
}
