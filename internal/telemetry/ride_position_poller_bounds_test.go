package telemetry

// MYR-394 — the three bounds that stop a poller from outliving its usefulness:
// a lifetime ceiling, a cap that cannot evict live rides, and a per-vehicle
// ride SET so one car's two rides do not tear down each other's poller.
//
// All three are about the same underlying fact: NOTHING IN THIS SYSTEM
// TERMINATES A RIDE. There is no automatic terminal transition anywhere in the
// repo, so "the ride row is still open" is not a promise that anything will
// ever stop the loop, and every bound below is what turns it into one.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// fakeClock is a manually-advanced clock for the lifetime ceiling, so a
// six-hour bound can be tested in milliseconds.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestRidePoller_StopsAtLifetimeCeiling — a ride that never terminates must not
// be polled forever.
//
// The concrete case is a rider no-show: the row sits at `arrived`, satisfies
// the poll predicate indefinitely, and is faithfully re-adopted after every
// restart. At one GET per 25s that is ~3,500 Fleet API requests a day for a
// ride that ended in everyone's mind hours ago, with nothing in the system that
// would ever stop it.
func TestRidePoller_StopsAtLifetimeCeiling(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{
		Interval:    5 * time.Millisecond,
		MaxLifetime: 6 * time.Hour,
	})
	clock := &fakeClock{now: time.Now()}
	h.poller.setNow(clock.Now)

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)
	if !h.awaitPoll(t) {
		t.Fatal("no first read after accept")
	}

	// Well inside the ceiling: the poller keeps working.
	clock.advance(5 * time.Hour)
	if !h.awaitPoll(t) {
		t.Fatal("poller stopped five hours in; the ceiling is six")
	}

	// Past it: the poller stops itself and deregisters.
	clock.advance(2 * time.Hour)
	h.awaitActive(t, 0)

	settled := h.backfill.callCount()
	time.Sleep(50 * time.Millisecond) // ten intervals' worth
	if got := h.backfill.callCount(); got != settled {
		t.Fatalf("Tesla reads after the lifetime ceiling: %d -> %d, want no further reads", settled, got)
	}
}

// TestRidePoller_LifetimeCeilingExtendsForANewRide — the ceiling bounds a
// poller's justification, not the vehicle. When a second, genuinely new ride
// joins the car, the clock starts again: refusing to poll a fresh ride because
// an older one on the same car is nearly six hours old would be the bound
// misfiring on exactly the rider it exists to protect.
func TestRidePoller_LifetimeCeilingExtendsForANewRide(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{
		Interval:    5 * time.Millisecond,
		MaxLifetime: 6 * time.Hour,
	})
	clock := &fakeClock{now: time.Now()}
	h.poller.setNow(clock.Now)

	h.publish(t, acceptedEvt(nil))
	awaitPollingRides(t, h.poller, ridePollVehicle, ridePollRide)

	clock.advance(5*time.Hour + 55*time.Minute)

	second := acceptedEvt(nil)
	second.RideRequestID = "ride-fresh"
	h.publish(t, second)
	awaitPollingRides(t, h.poller, ridePollVehicle, ridePollRide, "ride-fresh")

	// The original ceiling would have fired here; the new ride moved it.
	clock.advance(30 * time.Minute)
	if !h.awaitPoll(t) {
		t.Fatal("poller stopped despite a ride that became live 30 minutes ago")
	}
	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d, want 1", n)
	}
}

// TestRidePoller_SecondRideOnOneVehicleKeepsThePoller is the teardown gap.
//
// Two open rides on one car are reachable: activeInstantRidePredicate
// deliberately does not count a dispatched-but-unstarted reservation as making
// the car busy, and the MYR-383 window gate only refuses rides whose instants
// fall within ±W. When the registry stored ONE ride id per vehicle, the second
// ride got no handle of its own — and then the FIRST ride's terminal event
// cancelled the poller the second one depended on, leaving a live rider's map
// frozen until the next reconcile, up to five minutes later.
func TestRidePoller_SecondRideOnOneVehicleKeepsThePoller(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	// R1: a reservation whose pickup nav has just been delivered. The row is
	// still `accepted`; the car is driving to the pickup.
	h.publish(t, events.RideDueEvent{
		RideRequestID: "ride-reservation",
		VehicleID:     ridePollVehicle,
		ScheduledFor:  time.Now(),
		DueAt:         time.Now(),
	})
	awaitPollingRides(t, h.poller, ridePollVehicle, "ride-reservation")

	// R2: an instant ride accepted on the same car.
	instant := acceptedEvt(nil)
	instant.RideRequestID = "ride-instant"
	h.publish(t, instant)
	awaitPollingRides(t, h.poller, ridePollVehicle, "ride-instant", "ride-reservation")

	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d, want 1 — two rides on one car share one poll stream", n)
	}

	// R1 ends. R2's rider is watching the map RIGHT NOW.
	done := statusEvt(ridePollStatusCompleted)
	done.RideRequestID = "ride-reservation"
	h.publish(t, done)
	awaitPollingRides(t, h.poller, ridePollVehicle, "ride-instant")

	// R2 ends. Nothing justifies the poller, so it stops.
	last := statusEvt(ridePollStatusCancelled)
	last.RideRequestID = "ride-instant"
	h.publish(t, last)
	h.awaitActive(t, 0)
}

// TestRidePollReconcile_KeepsBothRidesOnOneVehicle is the mirror image, from
// the reconcile's side: building the wanted map first-row-wins would cancel one
// of the two rides' claim on the poller and re-adopt the other, re-arming the
// same teardown on the next terminal event.
func TestRidePollReconcile_KeepsBothRidesOnOneVehicle(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	awaitPollingRides(t, h.poller, ridePollVehicle, ridePollRide)

	h.lister.set(
		ActiveRideTarget{RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN},
		ActiveRideTarget{RideRequestID: "ride-second", VehicleID: ridePollVehicle, VIN: ridePollVIN},
	)

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0 — the running poller is still justified by %s", reaped, ridePollRide)
	}
	if adopted != 0 {
		t.Fatalf("adopted = %d, want 0 — the vehicle already had a poller", adopted)
	}
	assertPollingRides(t, h.poller, ridePollVehicle, ridePollRide, "ride-second")
	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d, want 1", n)
	}
}

// TestRidePollReconcile_DoesNotReapAgainstATruncatedList is the cap-eviction
// scenario, and it is the one with real teeth.
//
// Abandoned rides never terminate, so they accumulate; because their updated_at
// is frozen they are also the rides a paged read is most likely to be full of.
// Once enough of them exist to fill MaxActive, every reconcile pass returns a
// FULL page — and reapOrphans, which runs before adoptMissing and stops every
// poller whose vehicle is absent from that page, would tear down the pollers
// the ride.accepted / ride.due seams had just started for genuinely live rides,
// with no cap headroom left to adopt them back. The live rider's marker freezes
// within five minutes of their ride starting, while the server keeps reading
// the positions of parked cars.
//
// An incomplete answer must not be read as "these rides ended" — the same rule
// the pass already applies to a failed LIST.
func TestRidePollReconcile_DoesNotReapAgainstATruncatedList(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour, MaxActive: 2})

	// A live ride, started by its bus seam the way a real one is.
	h.publish(t, acceptedEvt(nil))
	awaitPollingRides(t, h.poller, ridePollVehicle, ridePollRide)

	// More never-terminating rides than the registry can hold, none of them on
	// the live ride's car.
	h.lister.set(
		ActiveRideTarget{RideRequestID: "zombie-1", VehicleID: "veh-z1", VIN: "7SAYGDET7TA600001"},
		ActiveRideTarget{RideRequestID: "zombie-2", VehicleID: "veh-z2", VIN: "7SAYGDET7TA600002"},
		ActiveRideTarget{RideRequestID: "zombie-3", VehicleID: "veh-z3", VIN: "7SAYGDET7TA600003"},
	)

	_, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d against a truncated list, want 0 — an incomplete page is not evidence a ride ended", reaped)
	}
	assertPollingRides(t, h.poller, ridePollVehicle, ridePollRide)

	// The reconcile must ASK for one row more than it can hold; that extra row
	// is the only way it can tell a full page from a complete one.
	h.lister.mu.Lock()
	gotLimit := h.lister.lastLimit
	h.lister.mu.Unlock()
	if gotLimit != 3 {
		t.Fatalf("LIST limit = %d, want MaxActive+1 = 3", gotLimit)
	}
}

// TestRidePollReconcile_ReapsAgainstACompleteList is the control for the test
// above: the truncation guard must not become a blanket excuse to stop reaping,
// or the pass would no longer clean up the poller whose terminal event the bus
// dropped — the reason the reconcile exists at all.
func TestRidePollReconcile_ReapsAgainstACompleteList(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour, MaxActive: 2})

	h.publish(t, acceptedEvt(nil))
	awaitPollingRides(t, h.poller, ridePollVehicle, ridePollRide)

	// One row, comfortably inside the cap, and it is not our car: the ride
	// genuinely ended and its terminal event was dropped.
	h.lister.set(ActiveRideTarget{
		RideRequestID: "other-ride", VehicleID: "veh-2", VIN: "7SAYGDET7TA613796",
	})

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reaped != 1 || adopted != 1 {
		t.Fatalf("Reconcile = (adopted %d, reaped %d), want (1, 1)", adopted, reaped)
	}
	if _, ok := h.poller.PollingRides(ridePollVehicle); ok {
		t.Fatal("the orphaned poller survived a complete list")
	}
}
