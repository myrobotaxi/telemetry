package ridecomplete

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

type fakeResolver struct {
	id  string
	err error
}

func (f *fakeResolver) ResolveID(context.Context, string) (string, error) {
	return f.id, f.err
}

type fakeStore struct {
	mu     sync.Mutex
	rides  []CompletedRide
	err    error
	gotVeh string
	calls  int
}

func (f *fakeStore) CompleteEnrouteByVehicle(_ context.Context, vehicleID string) ([]CompletedRide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotVeh = vehicleID
	if f.err != nil {
		return nil, f.err
	}
	return f.rides, nil
}

type fakePublisher struct {
	mu     sync.Mutex
	events []events.Event
	err    error
}

func (f *fakePublisher) Publish(_ context.Context, ev events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakePublisher) statusChanges() []events.RideStatusChangedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []events.RideStatusChangedEvent
	for _, e := range f.events {
		if sc, ok := e.Payload.(events.RideStatusChangedEvent); ok {
			out = append(out, sc)
		}
	}
	return out
}

func newCompleter(res *fakeResolver, st *fakeStore, pub *fakePublisher) *Completer {
	c := New(res, st, pub, nil)
	c.timeout = time.Second
	return c
}

// TestComplete_EnrouteRide_Completes proves a drive-end for a vehicle with an
// in-flight enroute ride transitions it to completed and publishes a
// ride_status_changed to the two parties.
func TestComplete_EnrouteRide_Completes(t *testing.T) {
	ride := CompletedRide{
		RideRequestID: "cride1", VehicleID: "cveh1", RiderID: "crider1", OwnerID: "cowner1",
		Status: "completed", UpdatedAt: time.Now(),
	}
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{rides: []CompletedRide{ride}}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.complete(context.Background(), "5YJ3E1EA7KF000000", "cdrive1")

	if st.gotVeh != "cveh1" {
		t.Errorf("store queried vehicle %q, want the resolved cuid cveh1", st.gotVeh)
	}
	scs := pub.statusChanges()
	if len(scs) != 1 {
		t.Fatalf("published %d status changes, want 1", len(scs))
	}
	if scs[0].RideRequestID != "cride1" || scs[0].Status != "completed" ||
		scs[0].RiderID != "crider1" || scs[0].OwnerID != "cowner1" {
		t.Errorf("status change payload: %+v", scs[0])
	}
}

// TestComplete_NoActiveRide_NoOp proves a drive-end for a vehicle with no
// enroute ride (store returns zero rows) publishes nothing.
func TestComplete_NoActiveRide_NoOp(t *testing.T) {
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{rides: nil} // no enroute ride
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.complete(context.Background(), "5YJ3E1EA7KF000000", "cdrive1")

	if st.calls != 1 {
		t.Errorf("store calls=%d, want 1 (guarded no-op still queries)", st.calls)
	}
	if len(pub.events) != 0 {
		t.Errorf("no-op drive-end must publish nothing, got %d", len(pub.events))
	}
}

// TestComplete_VINUnresolved_NoStoreCall proves an unresolvable VIN short-circuits
// before touching the store.
func TestComplete_VINUnresolved_NoStoreCall(t *testing.T) {
	res := &fakeResolver{err: errors.New("unknown vin")}
	st := &fakeStore{}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.complete(context.Background(), "5YJ3E1EA7KF000000", "cdrive1")

	if st.calls != 0 {
		t.Errorf("store must not be called when VIN is unresolved, got %d calls", st.calls)
	}
	if len(pub.events) != 0 {
		t.Errorf("published %d events on unresolved VIN, want 0", len(pub.events))
	}
}

// TestComplete_EmptyVIN_NoOp proves an empty VIN is ignored.
func TestComplete_EmptyVIN_NoOp(t *testing.T) {
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.complete(context.Background(), "", "cdrive1")
	if st.calls != 0 || len(pub.events) != 0 {
		t.Errorf("empty VIN must be a no-op: store calls=%d events=%d", st.calls, len(pub.events))
	}
}

// TestComplete_MultipleRides_PublishesEach proves the defensive multi-row case
// (more than one enroute ride for a vehicle) publishes one frame per ride.
func TestComplete_MultipleRides_PublishesEach(t *testing.T) {
	rides := []CompletedRide{
		{RideRequestID: "cride1", VehicleID: "cveh1", RiderID: "r1", OwnerID: "o1", Status: "completed"},
		{RideRequestID: "cride2", VehicleID: "cveh1", RiderID: "r2", OwnerID: "o1", Status: "completed"},
	}
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{rides: rides}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.complete(context.Background(), "5YJ3E1EA7KF000000", "cdrive1")
	if got := len(pub.statusChanges()); got != 2 {
		t.Errorf("published %d status changes, want 2 (one per completed ride)", got)
	}
}

// TestHandle_WrongPayload_NoOp proves a non-drive.ended payload is ignored.
func TestHandle_WrongPayload_NoOp(t *testing.T) {
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.handle(events.Event{ID: "x", Payload: events.RideStatusChangedEvent{}})
	if st.calls != 0 || len(pub.events) != 0 {
		t.Errorf("wrong payload must be a no-op: store calls=%d events=%d", st.calls, len(pub.events))
	}
}

// TestHandle_DriveEnded_DrivesCompletion proves the bus handler path type-asserts
// DriveEndedEvent and runs completion end to end.
func TestHandle_DriveEnded_DrivesCompletion(t *testing.T) {
	ride := CompletedRide{RideRequestID: "cride1", VehicleID: "cveh1", RiderID: "r1", OwnerID: "o1", Status: "completed"}
	res := &fakeResolver{id: "cveh1"}
	st := &fakeStore{rides: []CompletedRide{ride}}
	pub := &fakePublisher{}
	c := newCompleter(res, st, pub)

	c.handle(events.Event{ID: "e1", Payload: events.DriveEndedEvent{VIN: "5YJ3E1EA7KF000000", DriveID: "cdrive1"}})

	if len(pub.statusChanges()) != 1 {
		t.Errorf("drive.ended did not drive completion: %d status changes", len(pub.statusChanges()))
	}
}
