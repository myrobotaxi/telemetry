package push

import (
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-522 — an OWNER's cancel is the one `cancelled` transition that speaks,
// and it speaks to the RIDER. The rider's own cancel (and a legacy event with
// no initiator at all) stays silent, which is also what keeps every event from
// an un-upgraded publisher behaving exactly as before this issue.

// cancelledStatusEvent is statusEvent for a cancellation carrying its
// initiator stamp; by="" models a legacy event with no stamp.
func cancelledStatusEvent(by string, scheduledFor *time.Time) events.Event {
	ev := events.RideStatusChangedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		Status:        "cancelled",
		ScheduledFor:  scheduledFor,
	}
	if by != "" {
		ev.CancelledBy = &by
	}
	return events.NewEvent(ev)
}

func TestNotifierOwnerCancelReachesTheRider(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(cancelledStatusEvent("owner", nil))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1", len(sent))
	}
	if sent[0].DeviceToken != riderDevice {
		t.Errorf("device = %q, want the rider's %q", sent[0].DeviceToken, riderDevice)
	}
	if sent[0].Title != "Blue Whale had to cancel your ride" {
		t.Errorf("title = %q", sent[0].Title)
	}
	if sent[0].Body != "Try booking another car." {
		t.Errorf("body = %q", sent[0].Body)
	}
}

// A cancelled RESERVATION names the scheduled ride, MYR-360's declined rule:
// days may have passed since the booking was confirmed, and "your ride" on a
// lock screen gives the rider no way to tell it is about that booking.
func TestNotifierOwnerCancelOfAReservationNamesTheScheduledRide(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	at := time.Date(2026, 8, 15, 17, 30, 0, 0, time.UTC)
	n.handleStatusChanged(cancelledStatusEvent("owner", &at))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1", len(sent))
	}
	if sent[0].Title != "Blue Whale had to cancel your scheduled ride" {
		t.Errorf("title = %q", sent[0].Title)
	}
	// The standing rule: a scheduled push never renders the time.
	copyText := sent[0].Title + " " + sent[0].Body
	for _, forbidden := range []string{"17:30", "5:30", "UTC", "Aug", "15", "2026"} {
		if strings.Contains(copyText, forbidden) {
			t.Errorf("copy %q contains %q — scheduled pushes must not render a time", copyText, forbidden)
		}
	}
}

// The rider's own cancel, and a legacy event with no stamp, wake nobody.
func TestNotifierRiderAndUnstampedCancelsStaySilent(t *testing.T) {
	for _, by := range []string{"rider", ""} {
		sender := NewFakeSender()
		n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

		n.handleStatusChanged(cancelledStatusEvent(by, nil))
		n.Wait()

		if sent := sender.Sent(); len(sent) != 0 {
			t.Errorf("cancelledBy=%q sent %d notifications, want silence", by, len(sent))
		}
	}
}
