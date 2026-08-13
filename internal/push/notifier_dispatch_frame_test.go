package push

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-555 gave the reservation sweeper a second seam: a summary
// `ride_status_changed` re-carrying the ride's UNCHANGED `accepted` status, so
// an app that is already open finds out the car left. This package subscribes
// to that topic too, and the frame must be SILENT here — `ride.due` is already
// telling both parties the news, and the status copy for `accepted` is "Your
// ride is confirmed", which would land hours after the accept it describes.

// dispatchFrameEvent is the sweeper's publish, shaped exactly as
// publishDispatched builds it.
func dispatchFrameEvent() events.Event {
	at := time.Now()
	return events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID:  testRideID,
		VehicleID:      testVehicleID,
		RiderID:        testRiderID,
		OwnerID:        testOwnerID,
		Status:         "accepted",
		PreviousStatus: "accepted",
		ScheduledFor:   &at,
		TripVersion:    5,
		DispatchEdit:   true,
		UpdatedAt:      at,
	})
}

// TestNotifierDispatchFrameIsSilent is the whole rule. Without the DispatchEdit
// arm this event reaches statusAlert("accepted", …) and wakes the rider's phone
// with a confirmation they already have.
func TestNotifierDispatchFrameIsSilent(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(dispatchFrameEvent())
	n.Wait()

	if sent := sender.Sent(); len(sent) != 0 {
		t.Errorf("sent %d notifications for a dispatch frame, want 0: %+v", len(sent), sent)
	}
}

// TestNotifierRealAcceptStillSpeaks is the control, and it is the assertion
// that stops the suppression being written too wide. An ORDINARY accept — same
// status, no marker — must still wake the rider.
func TestNotifierRealAcceptStillSpeaks(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications for a real accept, want 1", len(sent))
	}
	if sent[0].DeviceToken != riderDevice {
		t.Errorf("device = %q, want the RIDER's", sent[0].DeviceToken)
	}
}

// TestNotifierDispatchFrameCostsNoWorker pins that the suppression happens on
// the CHEAP path — before a worker slot is spent and before any device lookup —
// exactly where the TripEdit arm sits. The sweeper publishes one of these per
// dispatch, so paying a fan-out for each would be a standing cost for nothing.
func TestNotifierDispatchFrameCostsNoWorker(t *testing.T) {
	devices := newFakeDeviceStore()
	devices.byUser[testRiderID] = []Device{{Token: riderDevice}}
	n := NewNotifier(NewFakeSender(), devices, nil, nil,
		&fakeVehicleNamer{name: "Blue Whale"}, Config{Enabled: true}, discardLogger())

	n.handleStatusChanged(dispatchFrameEvent())
	n.Wait()

	devices.mu.Lock()
	defer devices.mu.Unlock()
	if devices.listCalls != 0 {
		t.Errorf("device lookups = %d, want 0 — the frame must be dropped before the fan-out", devices.listCalls)
	}
}
