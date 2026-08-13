package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-527 — the nav-apply close-loop. `sent` records that Tesla ACCEPTED the
// share; these tests pin the machinery that notices when the CAR never applied
// it: observe the car's own reported destination, re-share once through the
// sequencer, and raise the owner's "check the dash" seam only for a live leg
// that never took. The first live casualty (2026-08-12, a rider aboard a
// 3-hour trip whose dash still navigated to the pickup) is the shape every
// failure-arm test below reproduces.

// fakeNavVerifyStore scripts the two lean reads.
type fakeNavVerifyStore struct {
	mu        sync.Mutex
	lat, lng  *float64
	destErr   error
	status    string
	statusErr error
	destReads int
}

func (f *fakeNavVerifyStore) VehicleNavDestination(context.Context, string) (*float64, *float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destReads++
	return f.lat, f.lng, f.destErr
}

func (f *fakeNavVerifyStore) RideStatus(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.statusErr
}

func (f *fakeNavVerifyStore) setDest(lat, lng float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lat, f.lng = &lat, &lng
}

// verifyHarness builds a dispatcher with the close-loop wired on SHORT windows
// and a synchronous executor, plus the leg under verification.
func verifyHarness(fs *fakeNavVerifyStore, bus events.Bus) (*Dispatcher, *fakeExecutor, *navVerify, dispatchLeg) {
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{token: "tok"},
		exec,
		&fakeStore{},
		Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
		nil,
	)
	d = d.WithNavVerify(context.Background(), fs, bus)
	d.verify.window = 40 * time.Millisecond
	d.verify.poll = 5 * time.Millisecond
	leg := dispatchLeg{
		name:      "dropoff",
		rideID:    "cride1234567890",
		vehicleID: "cveh1234567890",
		ownerID:   "cowner1234567890",
		coord:     events.RidePlace{Latitude: 37.7955, Longitude: -122.3937, Label: "Dropoff"},
		order:     legOrderDropoff,
	}
	return d, exec, d.verify, leg
}

func publishedNavUnapplied(bus *fakeBus) []events.RideNavUnappliedEvent {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	var out []events.RideNavUnappliedEvent
	for _, evt := range bus.published {
		if p, ok := evt.Payload.(events.RideNavUnappliedEvent); ok {
			out = append(out, p)
		}
	}
	return out
}

// The happy path: the car reports the shared destination inside the first
// window — no re-share, no event, and the loop's only cost is a few lean reads.
func TestNavVerify_AppliedShareIsConfirmedQuietly(t *testing.T) {
	fs := &fakeNavVerifyStore{status: "enroute"}
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)
	fs.setDest(leg.coord.Latitude+0.0005, leg.coord.Longitude) // ~55 m off: applied

	d.runNavVerify(v, leg)

	if len(exec.calls) != 0 {
		t.Errorf("re-shares = %d, want 0 for an applied share", len(exec.calls))
	}
	if got := publishedNavUnapplied(bus); len(got) != 0 {
		t.Errorf("events = %d, want 0", len(got))
	}
}

// THE INCIDENT ARM: a live leg the car never took gets exactly ONE re-share,
// and when that re-share still does not take, the owner's seam fires once.
func TestNavVerify_UnappliedLiveLegResharesOnceThenReports(t *testing.T) {
	fs := &fakeNavVerifyStore{status: "enroute"} // dropoff's current status
	// The car reports the PICKUP forever — the incident's exact shape.
	pickupLat, pickupLng := 33.0839, -96.8353
	fs.lat, fs.lng = &pickupLat, &pickupLng
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)

	d.runNavVerify(v, leg)

	if len(exec.calls) != 1 {
		t.Fatalf("re-shares = %d, want exactly 1", len(exec.calls))
	}
	got := publishedNavUnapplied(bus)
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Leg != "dropoff" || got[0].OwnerID != leg.ownerID || got[0].RideRequestID != leg.rideID {
		t.Errorf("event = %+v, want the leg's own identifiers", got[0])
	}
}

// A ride that moved on is not this verifier's business: no re-share, no event.
// (A pickup leg's ride reaching `arrived` means the car GOT there.)
func TestNavVerify_RideThatMovedOnAbandonsSilently(t *testing.T) {
	fs := &fakeNavVerifyStore{status: "completed"}
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)

	d.runNavVerify(v, leg)

	if len(exec.calls) != 0 || len(publishedNavUnapplied(bus)) != 0 {
		t.Errorf("re-shares = %d, events = %d — want silence for a ride that moved on",
			len(exec.calls), len(publishedNavUnapplied(bus)))
	}
}

// An unreadable status abandons too — the re-share and the alert both assert
// things about a ride the verifier then cannot see.
func TestNavVerify_UnreadableStatusAbandons(t *testing.T) {
	fs := &fakeNavVerifyStore{statusErr: errors.New("db down")}
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)

	d.runNavVerify(v, leg)

	if len(exec.calls) != 0 || len(publishedNavUnapplied(bus)) != 0 {
		t.Error("an unreadable ride must produce no re-share and no event")
	}
}

// The share applying AFTER the re-share ends the loop with no event — the
// re-share is the fix working, not a failure to report.
func TestNavVerify_AppliedAfterReshareStaysQuiet(t *testing.T) {
	fs := &fakeNavVerifyStore{status: "enroute"}
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)
	// No destination at all through window 1; the re-share "lands" it.
	exec.errs = nil
	go func() {
		time.Sleep(60 * time.Millisecond) // past window 1 + the re-share
		fs.setDest(leg.coord.Latitude, leg.coord.Longitude)
	}()

	d.runNavVerify(v, leg)

	if len(exec.calls) != 1 {
		t.Fatalf("re-shares = %d, want 1", len(exec.calls))
	}
	if len(publishedNavUnapplied(bus)) != 0 {
		t.Error("a share applied by the re-share must not raise the seam")
	}
}

// A leg SUPERSEDED by the time of the re-share abandons: the sequencer's
// reacquire admits the leg's OWN order and refuses a strictly higher
// high-water mark, so the pickup verifier of a ride the rider already started
// can neither re-share nor alert.
func TestNavVerify_SupersededLegAbandonsAtTheGate(t *testing.T) {
	fs := &fakeNavVerifyStore{status: "accepted"} // pickup's current status
	bus := &fakeBus{}
	d, exec, v, leg := verifyHarness(fs, bus)
	leg.name, leg.order = "pickup", legOrderPickup

	// The dropoff already reached the car: deliver order 2 through the real
	// sequencer so its high-water mark stands.
	ctx, cancel := context.WithCancel(context.Background())
	hold, err := d.nav.acquire(ctx, leg.vehicleID, leg.rideID, legOrderDropoff, cancel)
	if err != nil {
		t.Fatalf("seeding the dropoff mark: %v", err)
	}
	hold.Release()
	cancel()

	d.runNavVerify(v, leg)

	if len(exec.calls) != 0 {
		t.Errorf("re-shares = %d, want 0 — a superseded leg must not re-dial the pickup", len(exec.calls))
	}
	if len(publishedNavUnapplied(bus)) != 0 {
		t.Error("a superseded leg must not raise the seam")
	}
}

// The re-share path goes through reacquire, which admits the leg's OWN order:
// a delivered leg can be pushed again (MYR-527) while a strictly lower one is
// still refused (MYR-526's guarantee, untouched).
func TestNavSequencer_ReacquireAdmitsEqualOrderOnly(t *testing.T) {
	seq := newNavSequencer()
	ctx := context.Background()

	// Deliver the dropoff.
	c1, cancel1 := context.WithCancel(ctx)
	h, err := seq.acquire(c1, "veh", "ride", legOrderDropoff, cancel1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()
	cancel1()

	// A plain acquire of the same order is refused (unchanged MYR-526 rule)…
	c2, cancel2 := context.WithCancel(ctx)
	if _, err := seq.acquire(c2, "veh", "ride", legOrderDropoff, cancel2); !errors.Is(err, errNavSuperseded) {
		t.Fatalf("acquire(equal) = %v, want errNavSuperseded", err)
	}
	cancel2()

	// …a REACQUIRE of the same order is admitted…
	c3, cancel3 := context.WithCancel(ctx)
	h3, err := seq.reacquire(c3, "veh", "ride", legOrderDropoff, cancel3)
	if err != nil {
		t.Fatalf("reacquire(equal) = %v, want a hold", err)
	}
	h3.Release()
	cancel3()

	// …and a reacquire of a LOWER order is still refused.
	c4, cancel4 := context.WithCancel(ctx)
	if _, err := seq.reacquire(c4, "veh", "ride", legOrderPickup, cancel4); !errors.Is(err, errNavSuperseded) {
		t.Fatalf("reacquire(lower) = %v, want errNavSuperseded", err)
	}
	cancel4()
}

// The (0, 0) sentinel is "no destination", never a match — a car that has
// reported nothing must not satisfy a verification by accident of arithmetic.
func TestNavVerify_NoFixSentinelIsNotAMatch(t *testing.T) {
	fs := &fakeNavVerifyStore{}
	d, _, v, leg := verifyHarness(fs, &fakeBus{})
	fs.setDest(0, 0)
	leg.coord.Latitude, leg.coord.Longitude = 0.001, 0.001 // within tolerance of (0,0)

	if d.navDestinationMatches(context.Background(), v, leg) {
		t.Error("the (0,0) sentinel matched a nearby target — it must read as no destination")
	}
}

// Unwired is a no-op: the ordinary state for every pre-MYR-527 test and any
// deploy that wires nothing.
func TestNavVerify_UnwiredArmIsANoOp(t *testing.T) {
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{token: "tok"},
		&fakeExecutor{},
		&fakeStore{},
		Config{Enabled: true},
		nil,
	)
	d.armNavVerify(dispatchLeg{name: "pickup", order: legOrderPickup}) // must not panic or spawn
}
