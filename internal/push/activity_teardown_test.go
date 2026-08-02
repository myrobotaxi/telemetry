package push

import (
	"context"
	"errors"
	"testing"
	"time"
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
	store.ridesByVehicle[teardownVehicleID] = []VehicleRide{
		{RideRequestID: activityRideID, Status: statusEnroute},
		{RideRequestID: otherRide, Status: statusEnroute},
	}

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
// five-minute look reserved for an arrival.
//
// The 30 seconds is a literal here rather than DismissPromptly, so that folding
// the completed linger (MYR-406) and this one into one constant cannot move
// this ending without a test saying so.
func TestEndForVehicleTeardownDismissesPromptly(t *testing.T) {
	n, sender, _ := newTeardownNotifier(t)

	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	for _, s := range sender.Sent() {
		if want := fixedNow.Add(30 * time.Second); !s.DismissalDate.Equal(want) {
			t.Errorf("dismissal-date = %s, want %s", s.DismissalDate, want)
		}
	}
}

// TestEndForVehicleTeardownDoesNotCancelACompletedRide is the MYR-421
// correction, and it is a defect the hold CREATED.
//
// The teardown read's predicate is `ended_at IS NULL`. Before the held `end`
// that could never match a completed ride — the tombstone went out with the
// transition — so every id this read returned was a ride genuinely in flight
// and `cancelled` was always the truth. Now a completed ride's row stays live
// for five minutes, so a rider who finished their trip and whose owner then
// unlinked the car had their completion check OVERWRITTEN with a cancellation
// and dismissed in thirty seconds.
//
// Skipping completed rides here would be worse, not better: the teardown's
// cascade destroys the rows moments later, so the held-end pass would never
// fire and the card would sit on its arrival state until ActivityKit's own
// ceiling. This is the last chance to end it, so it is ended honestly.
func TestEndForVehicleTeardownDoesNotCancelACompletedRide(t *testing.T) {
	n, sender, store := newTeardownNotifier(t)
	// activityRideID completed two minutes ago and is inside its hold; the
	// other ride is still in flight.
	store.ridesByVehicle[teardownVehicleID] = []VehicleRide{
		{RideRequestID: activityRideID, Status: statusCompleted},
		{RideRequestID: "ride_live_2", Status: statusEnroute},
	}
	store.completeRide(activityRideID, fixedNow.Add(-2*time.Minute))

	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	byRide := map[string]ActivityNotification{}
	for _, s := range sender.Sent() {
		byRide[s.ContentState.Status] = s
	}
	if len(sender.Sent()) != 2 {
		t.Fatalf("sent %d pushes, want one per ride", len(sender.Sent()))
	}

	done, ok := byRide[statusCompleted]
	if !ok {
		t.Fatalf("no push carried the completed state; the rider's finished ride was reported as %v",
			byRide)
	}
	if done.Event != ActivityEventEnd {
		t.Errorf("completed ride event = %q, want end", done.Event)
	}
	if done.Alert != nil {
		t.Errorf("the teardown end carries an alert (%+v); rung six was already spent", *done.Alert)
	}
	// The honest linger, not the 30-second brush-off an unhappy ending gets.
	// Literal, for the reason TestEndForVehicleTeardownDismissesPromptly gives.
	if done.DismissalDate == nil || !done.DismissalDate.Equal(done.Timestamp.Add(5*time.Minute)) {
		t.Errorf("completed dismissal-date = %v, want 5m past the end", done.DismissalDate)
	}

	// The ride that was genuinely in flight is still cancelled, promptly.
	live, ok := byRide["cancelled"]
	if !ok {
		t.Fatal("the in-flight ride was not cancelled")
	}
	if live.DismissalDate == nil || !live.DismissalDate.Equal(live.Timestamp.Add(30*time.Second)) {
		t.Errorf("cancelled dismissal-date = %v, want 30s past the end", live.DismissalDate)
	}

	// BOTH are tombstoned here and now. The cascade is about to delete the
	// rows, so leaving the completed one live for the held-end pass would mean
	// nothing ever ends it.
	if store.ended[activityRideID] != 1 || store.ended["ride_live_2"] != 1 {
		t.Errorf("tombstones = %v, want one per ride", store.ended)
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
	store.ridesByVehicle[teardownVehicleID] = []VehicleRide{
		{RideRequestID: activityRideID, Status: statusEnroute},
	}

	n := NewActivityNotifier(nil, store, nil, Config{Enabled: true}, discardLogger())
	n.EndForVehicleTeardown(context.Background(), teardownVehicleID)

	if got := len(store.ended); got != 0 {
		t.Errorf("tombstoned %d rides with no sender wired, want 0", got)
	}
}
