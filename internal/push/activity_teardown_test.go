package push

import (
	"context"
	"errors"
	"testing"
)

// Owner-teardown end pushes (MYR-172 review).
//
// Owner "Remove this car" runs `DELETE FROM go_ride_requests WHERE vehicle_id`
// and migration 0025's FK cascades the Activity registrations away with it. The
// delete publishes NO event, so the lifecycle subscription never hears about it,
// and afterwards there is no row left to push from — the tokens are gone with
// the rides. Every rider with a Live Activity on one of those rides was left
// looking at "your car is on its way" until ActivityKit's own multi-hour ceiling
// reaped it.
//
// The migration header claimed the cascade "cannot strand" anything. It cannot
// strand a ROW. It strands the thing the row points at.

const teardownVehicleID = "veh_teardown_1"

// newTeardownNotifier wires a notifier over two rides on one vehicle, each with
// a live Activity.
func newTeardownNotifier(t *testing.T) (*ActivityNotifier, *FakeActivitySender, *fakeActivityStore) {
	t.Helper()
	n, sender, store := newTestActivityNotifier(t, nil)

	const otherRide = "ride_live_2"
	store.byRide[otherRide] = []Activity{{
		RideRequestID: otherRide,
		UserID:        "user_rider_2",
		Token:         "second-activity-token",
	}}
	store.context[otherRide] = RideContext{Status: "enroute", VehicleName: "Blue Whale", Destination: "Work"}
	store.ridesByVehicle[teardownVehicleID] = []string{activityRideID, otherRide}

	return n, sender, store
}

// TestEndForVehicleTeardownEndsEveryRidersActivity is the fix: both riders get a
// real `end` push before their rides are deleted.
func TestEndForVehicleTeardownEndsEveryRidersActivity(t *testing.T) {
	n, sender, store := newTeardownNotifier(t)

	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d pushes for a two-ride teardown, want 2", len(sent))
	}
	for _, s := range sent {
		if s.Event != ActivityEventEnd {
			t.Errorf("event = %q, want end — an update would leave the card on the lock screen", s.Event)
		}
		if s.DismissalDate == nil {
			t.Error("no dismissal-date on a teardown end; the Activity would never leave the lock screen")
		}
		// `cancelled` is what this is from the rider's side: the ride is not
		// happening and nobody is coming.
		if got := s.ContentState.Status; got != "cancelled" {
			t.Errorf("content-state status = %q, want cancelled", got)
		}
	}

	// Both rides tombstoned, so a ticker that races the delete finds nothing.
	if got := store.ended[activityRideID]; got != 1 {
		t.Errorf("ride %s tombstoned %d times, want 1", activityRideID, got)
	}
	if got := store.ended["ride_live_2"]; got != 1 {
		t.Errorf("ride ride_live_2 tombstoned %d times, want 1", got)
	}
}

// TestEndForVehicleTeardownDismissesPromptly pins the linger. A car being
// removed is an unhappy ending, so it gets the 30-second glance rather than the
// 15-minute look reserved for an arrival.
func TestEndForVehicleTeardownDismissesPromptly(t *testing.T) {
	n, sender, _ := newTeardownNotifier(t)

	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	for _, s := range sender.Sent() {
		if want := fixedNow.Add(DismissPromptly); !s.DismissalDate.Equal(want) {
			t.Errorf("dismissal-date = %s, want %s", s.DismissalDate, want)
		}
	}
}

// TestEndForVehicleTeardownIsQuietWithNothingToEnd — most teardowns remove a car
// with no ride in flight, and that must cost one read and no pushes.
func TestEndForVehicleTeardownIsQuietWithNothingToEnd(t *testing.T) {
	n, sender, _ := newTeardownNotifier(t)

	n.EndForVehicleTeardown(context.Background(), "veh_with_no_rides")

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d pushes for a vehicle with no live Activities, want 0", got)
	}
}

// TestEndForVehicleTeardownSurvivesALookupFailure — the teardown is
// authoritative and must proceed whatever the push layer does. A read failure
// here is logged and swallowed, exactly like the Tesla-side steps beside it.
func TestEndForVehicleTeardownSurvivesALookupFailure(t *testing.T) {
	n, sender, store := newTeardownNotifier(t)
	store.vehicleErr = errors.New("db unavailable")

	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d pushes after the lookup failed, want 0", got)
	}
}

// TestEndForVehicleTeardownIsANoopWhenPushIsOff keeps the keyless deployment
// honest: with no APNs key wired there is nothing to send and nothing to
// tombstone, and the teardown must not care.
func TestEndForVehicleTeardownIsANoopWhenPushIsOff(t *testing.T) {
	store := newFakeActivityStore()
	store.ridesByVehicle[teardownVehicleID] = []string{activityRideID}

	n := NewActivityNotifier(nil, store, nil, Config{Enabled: true}, discardLogger())
	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	if got := len(store.ended); got != 0 {
		t.Errorf("tombstoned %d rides with no sender wired, want 0", got)
	}
}
