package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-539 — the leg advance. Reaching a stop must move the statuses exactly
// once and point the dash at whatever comes next; reaching anything else must
// do nothing at all.

// fakeStopStore records every advance and answers the scripted next target.
type fakeStopStore struct {
	mu sync.Mutex
	// calls records the (rideID, stopID) pairs the dispatcher asked to advance.
	calls []string
	// advance is returned on the FIRST call; every later call is refused, which
	// is exactly what the guarded write does for a re-delivered event.
	advance StopAdvance
	err     error
}

func (f *fakeStopStore) AdvanceStopArrival(_ context.Context, rideID, stopID string) (StopAdvance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rideID+"/"+stopID)
	if f.err != nil {
		return StopAdvance{}, f.err
	}
	if len(f.calls) > 1 {
		return StopAdvance{}, fmt.Errorf("second delivery: %w", ErrStopNotAdvanced)
	}
	return f.advance, nil
}

func (f *fakeStopStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newStopDispatcher builds a dispatcher with the leg advance wired and the
// verifier deliberately UNWIRED — armNavVerify no-ops on nil, so the executor
// call count is exactly the advance's own re-shares (the trip-change decision
// table makes the same choice, for the same reason).
func newStopDispatcher(exec CommandExecutor, stops StopStore) *Dispatcher {
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000002"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
		nil,
	)
	return d.WithStopAdvance(stops)
}

func stopArrivedEvent(stopID string) events.Event {
	return events.NewEvent(events.RideWaypointArrivedEvent{
		RideRequestID: "cride539",
		VehicleID:     "cveh539",
		RiderID:       "crider539",
		OwnerID:       "cowner539",
		Waypoint:      events.WaypointStop(stopID),
	})
}

// The happy path: the car reaches a stop, the guarded write advances the leg,
// and the NEXT stop is shared with the car.
func TestWaypointArrived_AdvancesAndResharesTheNextStop(t *testing.T) {
	next := events.RidePlace{Latitude: 37.9, Longitude: -122.5, Label: "Pharmacy"}
	stops := &fakeStopStore{advance: StopAdvance{NextStopID: "crs02", NextTarget: next}}
	exec := &fakeExecutor{}
	d := newStopDispatcher(exec, stops)

	d.handleWaypointArrived(stopArrivedEvent("crs01"))
	d.Wait()

	if stops.callCount() != 1 {
		t.Fatalf("advance calls = %d, want 1", stops.callCount())
	}
	if stops.calls[0] != "cride539/crs01" {
		t.Errorf("advanced %q, want the ride and the stop the label named", stops.calls[0])
	}
	if len(exec.calls) != 1 {
		t.Fatalf("Tesla shares = %d, want 1 (the next target)", len(exec.calls))
	}
}

// The final leg: the completed stop was the last one, so the re-share carries
// the drop-off.
func TestWaypointArrived_LastStopResharesTheDropoff(t *testing.T) {
	stops := &fakeStopStore{advance: StopAdvance{
		NextTarget: events.RidePlace{Latitude: 37.77, Longitude: -122.39, Label: "Caltrain"},
	}}
	exec := &fakeExecutor{}
	d := newStopDispatcher(exec, stops)

	d.handleWaypointArrived(stopArrivedEvent("crs09"))
	d.Wait()

	if len(exec.calls) != 1 {
		t.Fatalf("Tesla shares = %d, want 1 (the drop-off)", len(exec.calls))
	}
}

// EXACTLY-ONCE: a re-delivered event loses the guarded write and must NOT
// re-dial the car. Only the winner re-shares — that is the whole ordering of
// write-then-share.
func TestWaypointArrived_RedeliveryResharesNothing(t *testing.T) {
	stops := &fakeStopStore{advance: StopAdvance{
		NextStopID: "crs02",
		NextTarget: events.RidePlace{Latitude: 37.9, Longitude: -122.5, Label: "Pharmacy"},
	}}
	exec := &fakeExecutor{}
	d := newStopDispatcher(exec, stops)

	d.handleWaypointArrived(stopArrivedEvent("crs01"))
	d.Wait()
	d.handleWaypointArrived(stopArrivedEvent("crs01"))
	d.Wait()

	if stops.callCount() != 2 {
		t.Fatalf("advance attempts = %d, want 2 (both deliveries reach the guard)", stops.callCount())
	}
	if len(exec.calls) != 1 {
		t.Errorf("Tesla shares = %d, want exactly 1 — a re-delivery re-dialled the dash", len(exec.calls))
	}
}

// The pickup and the destination advance NOTHING here. The pickup's lifecycle
// already travelled on the status event, and the destination deliberately does
// not end the ride — dropped-off stays the owner's tap.
func TestWaypointArrived_IgnoresPickupAndDestination(t *testing.T) {
	for _, waypoint := range []string{events.WaypointPickup, events.WaypointDestination, "", "stop:"} {
		t.Run("waypoint "+waypoint, func(t *testing.T) {
			stops := &fakeStopStore{}
			exec := &fakeExecutor{}
			d := newStopDispatcher(exec, stops)

			d.handleWaypointArrived(events.NewEvent(events.RideWaypointArrivedEvent{
				RideRequestID: "cride539", VehicleID: "cveh539", OwnerID: "cowner539", Waypoint: waypoint,
			}))
			d.Wait()

			if stops.callCount() != 0 {
				t.Errorf("advance calls = %d, want 0", stops.callCount())
			}
			if len(exec.calls) != 0 {
				t.Errorf("Tesla shares = %d, want 0", len(exec.calls))
			}
		})
	}
}

// A failed advance changes nothing: no re-share on a leg the database did not
// move, because the share would then be about a target no row agrees with.
func TestWaypointArrived_FailedAdvanceResharesNothing(t *testing.T) {
	stops := &fakeStopStore{err: errors.New("database unwell")}
	exec := &fakeExecutor{}
	d := newStopDispatcher(exec, stops)

	d.handleWaypointArrived(stopArrivedEvent("crs01"))
	d.Wait()

	if len(exec.calls) != 0 {
		t.Errorf("Tesla shares = %d, want 0 on a refused advance", len(exec.calls))
	}
}

// With no store wired the seam is inert — the ordinary state for a deploy that
// wires nothing, and the reason the subscription is unconditional.
func TestWaypointArrived_UnwiredIsInert(t *testing.T) {
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000003"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, Backoff: time.Millisecond},
		nil,
	)
	d.handleWaypointArrived(stopArrivedEvent("crs01"))
	d.Wait()
	if len(exec.calls) != 0 {
		t.Errorf("an unwired leg advance must dial nothing, got %d", len(exec.calls))
	}
}

// A stops edit while the ride is under way re-shares the SERVER's leg target
// and does not also fire the drop-off arm: one edit is one re-share.
func TestTripChanged_StopsEditResharesTheLegTargetOnly(t *testing.T) {
	target := events.RidePlace{Latitude: 37.81, Longitude: -122.41, Label: "Bakery"}
	dropoff := events.RidePlace{Latitude: 37.77, Longitude: -122.39, Label: "Caltrain"}
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000004"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, Backoff: time.Millisecond},
		nil,
	)

	d.processTripChanged(context.Background(), events.RideTripChangedEvent{
		RideRequestID: "cride539", VehicleID: "cveh539", OwnerID: "cowner539",
		Status: "enroute", StopsChanged: true, LegTarget: &target, NewDropoff: &dropoff,
	})
	d.Wait()

	if len(exec.calls) != 1 {
		t.Fatalf("Tesla shares = %d, want exactly 1", len(exec.calls))
	}
}

// A stops edit BEFORE the ride is under way pushes nothing: leg 2 has not
// dispatched, and the Start will dispatch the new first stop for free.
func TestTripChanged_StopsEditBeforeStartResharesNothing(t *testing.T) {
	target := events.RidePlace{Latitude: 37.81, Longitude: -122.41, Label: "Bakery"}
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000005"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, Backoff: time.Millisecond},
		nil,
	)

	for _, status := range []string{"requested", "accepted", "arrived"} {
		d.processTripChanged(context.Background(), events.RideTripChangedEvent{
			RideRequestID: "cride539", VehicleID: "cveh539", OwnerID: "cowner539",
			Status: status, StopsChanged: true, LegTarget: &target,
		})
	}
	d.Wait()

	if len(exec.calls) != 0 {
		t.Errorf("Tesla shares = %d, want 0 before the ride is under way", len(exec.calls))
	}
}
