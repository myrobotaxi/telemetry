package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-172 review fixes, against a real database.
//
// Every assertion here is about a predicate that lives in SQL rather than in
// Go, so a fake would prove nothing: the whole point of moving the register
// guard into the statement was that a read-then-write in the handler cannot
// hold under a race.

// setRideStatus moves a seeded fixture ride to a new lifecycle status.
func setRideStatus(t *testing.T, rideID, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET status = $2 WHERE id = $1`, rideID, status); err != nil {
		t.Fatalf("set ride %s status=%s: %v", rideID, status, err)
	}
}

// expireReservation records the ride exactly as the reservation sweeper does:
// a dispatch failure with the reservation_expired code, and the status LEFT AT
// `accepted`. That combination is the whole subtlety — nothing about the ride's
// own status says its Activity is over.
func expireReservation(t *testing.T, rideID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests
		    SET dispatch_status = 'failed',
		        dispatch_error  = 'reservation_expired',
		        dispatched_at   = NOW()
		  WHERE id = $1`, rideID); err != nil {
		t.Fatalf("expire reservation on %s: %v", rideID, err)
	}
}

func setDispatchFailure(t *testing.T, rideID, code string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests
		    SET dispatch_status = 'failed',
		        dispatch_error  = $2,
		        dispatched_at   = NOW()
		  WHERE id = $1`, rideID, code); err != nil {
		t.Fatalf("set dispatch failure on %s: %v", rideID, err)
	}
}

// TestLiveActivityRepo_RegisterRefusesAnExpiredReservation is the headline fix.
//
// The sweeper ends and tombstones the Activities on a late reservation but
// leaves the row at `accepted`, and it latches dispatched_at so a second expiry
// is impossible. The upsert clears ended_at on conflict — correct for a token
// rotation, catastrophic here — so a single rotation after the expiry
// resurrected the row, the ETA ticker picked it up, and the rider watched a
// countdown to a pickup nobody was making, with nothing able to end it again.
func TestLiveActivityRepo_RegisterRefusesAnExpiredReservation(t *testing.T) {
	const ride = "cride0025g1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	// The ordinary life of the Activity, up to the expiry.
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	expireReservation(t, ride)
	if _, err := repo.EndActivitiesForRide(ctx, ride); err != nil {
		t.Fatalf("EndActivitiesForRide: %v", err)
	}

	// ActivityKit rotates the token. This is the POST that used to resurrect
	// the row.
	err := repo.RegisterActivity(ctx, ride, "rider-1", "token-rotated", false)
	if !errors.Is(err, store.ErrLiveActivityRideClosed) {
		t.Fatalf("RegisterActivity after expiry = %v, want ErrLiveActivityRideClosed", err)
	}

	live, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("%d Activities live again after an expired reservation; the tombstone was cleared", len(live))
	}

	// The ticker must not see it either — that is the harm, stated directly.
	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	for _, leg := range legs {
		if leg.RideRequestID == ride {
			t.Error("the ETA ticker picked the expired reservation back up — the phantom countdown is live again")
		}
	}
}

// TestLiveActivityRepo_RegisterAllowsARescuedExpiredReservation is MYR-461, and
// it is the counterweight to the test above in exactly the way
// RegisterAllowsOrdinaryDispatchFailures is the counterweight to the guard as a
// whole.
//
// `reservation_expired` is a verdict on the DISPATCH — the car did not drive
// itself to the pickup inside the lateness ceiling — and the sweeper leaves the
// ride at `accepted` because nobody declined and nobody cancelled. The humans
// can still rescue that ride by hand, and in external beta they did: the owner
// tapped Picked up, the rider tapped Start ride, and the trip ran for another
// hour. The unscoped guard answered 409 to every registration for the whole of
// it, so the rider's lock screen stayed dark through a ride that was visibly
// happening — the card appeared once at request time, the expiry prompt-ended
// it, and nothing could ever bring it back.
//
// Once the ride advances past `accepted` the expiry verdict has been falsified
// by events, and registration must be allowed again.
func TestLiveActivityRepo_RegisterAllowsARescuedExpiredReservation(t *testing.T) {
	for _, status := range []string{"arrived", "enroute"} {
		t.Run(status, func(t *testing.T) {
			ride := "cride0025r" + status[:1]
			repo := setupLiveActivities(t, ride)
			ctx := context.Background()

			// The ride's whole history: registered, expired, ended by the
			// reservation seam — and then rescued by hand.
			if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
				t.Fatalf("RegisterActivity: %v", err)
			}
			expireReservation(t, ride)
			if _, err := repo.EndActivitiesForRide(ctx, ride); err != nil {
				t.Fatalf("EndActivitiesForRide: %v", err)
			}

			// While it is still nothing but an expired reservation, the refusal
			// stands — that is MYR-172 and it must survive this fix.
			if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-early", false); !errors.Is(err, store.ErrLiveActivityRideClosed) {
				t.Fatalf("RegisterActivity on an unrescued expired reservation = %v, want ErrLiveActivityRideClosed", err)
			}

			// The humans drive it anyway.
			setRideStatus(t, ride, status)

			if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-rescued", false); err != nil {
				t.Fatalf("RegisterActivity on a %s ride whose reservation expired: %v — "+
					"the rider is in the car and the lock screen is locked out", status, err)
			}

			live, err := repo.ActivitiesForRide(ctx, ride)
			if err != nil {
				t.Fatalf("ActivitiesForRide: %v", err)
			}
			if len(live) != 1 || live[0].ActivityPushToken != "token-rescued" {
				t.Fatalf("live rows = %+v, want one row holding the rescued token", live)
			}

			// The re-seed path is exactly what this fix makes newly reachable,
			// so pin it. Registering on a TOMBSTONED row re-seeds the phase mark
			// from the ride's status, and it must land on the rung the status
			// names — `arrived` is 4, `enroute` is 5. Too low and the next push
			// would re-announce a phase the app has just drawn locally,
			// expanding an island over a card the rider is already looking at.
			wantPhase := map[string]int16{"arrived": 4, "enroute": 5}[status]
			if live[0].AlertedPhase != wantPhase {
				t.Errorf("alerted_phase = %d after rescuing at %s, want %d",
					live[0].AlertedPhase, status, wantPhase)
			}

			// And the ticker has to pick it back up, or the card is registered
			// and still never updated — the starvation this issue was filed for.
			legs, err := repo.ListActiveLegActivities(ctx, 50)
			if err != nil {
				t.Fatalf("ListActiveLegActivities: %v", err)
			}
			var seen bool
			for _, leg := range legs {
				if leg.RideRequestID == ride {
					seen = true
				}
			}
			if !seen {
				t.Errorf("the ETA ticker does not see the rescued %s ride; the Activity would "+
					"be registered and never refreshed", status)
			}
		})
	}
}

// TestLiveActivityRepo_RegisterRefusesTerminalRides closes the race the handler
// check cannot: the handler reads the status, then writes, and a POST arriving
// mid-transition passes the read and lands after the terminal tombstone.
func TestLiveActivityRepo_RegisterRefusesTerminalRides(t *testing.T) {
	for _, status := range []string{"completed", "declined", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			ride := "cride0025t" + status[:1]
			repo := setupLiveActivities(t, ride)
			ctx := context.Background()

			setRideStatus(t, ride, status)

			err := repo.RegisterActivity(ctx, ride, "rider-1", "token-late", false)
			if !errors.Is(err, store.ErrLiveActivityRideClosed) {
				t.Fatalf("RegisterActivity on a %s ride = %v, want ErrLiveActivityRideClosed", status, err)
			}
			if live, _ := repo.ActivitiesForRide(ctx, ride); len(live) != 0 {
				t.Errorf("%d Activities registered against a %s ride", len(live), status)
			}
		})
	}
}

// TestLiveActivityRepo_RegisterAllowsOrdinaryDispatchFailures is the
// counterweight, and it is why the guard reads BOTH dispatch columns.
//
// `failed` alone is any nav push that did not land — the car was asleep, the
// proxy was down. That ride is still genuinely happening and the owner can
// drive it manually, so its Activity must keep working, rotations included.
// Refusing every failure would trade one broken lock screen for another.
func TestLiveActivityRepo_RegisterAllowsOrdinaryDispatchFailures(t *testing.T) {
	const ride = "cride0025g2"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	setDispatchFailure(t, ride, "vehicle_asleep")

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity after an ordinary dispatch failure: %v", err)
	}
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-rotated", false); err != nil {
		t.Fatalf("token rotation after an ordinary dispatch failure: %v", err)
	}

	live, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(live) != 1 || live[0].ActivityPushToken != "token-rotated" {
		t.Errorf("live rows = %+v, want one row holding the rotated token", live)
	}
}

// TestLiveActivityRepo_RegisterStillClearsTheTombstoneOnALiveRide guards the
// behaviour the guard must NOT have broken. Re-registering on a ride that is
// still happening is a client saying "I have a live Activity again", and it has
// to clear the tombstone — that path is deliberate, and the fix scopes the
// refusal to the endings rather than to re-registration in general.
func TestLiveActivityRepo_RegisterStillClearsTheTombstoneOnALiveRide(t *testing.T) {
	const ride = "cride0025g3"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	if _, err := repo.EndActivity(ctx, ride, "rider-1"); err != nil {
		t.Fatalf("EndActivity: %v", err)
	}
	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-b", false); err != nil {
		t.Fatalf("re-register on a live ride: %v", err)
	}

	live, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live rows = %d after re-registering on an accepted ride, want 1", len(live))
	}
}

// TestLiveActivityRepo_RegisterRefusesAnUnknownRide — the guard's SELECT finds
// no ride at all, which is what a registration racing an owner teardown looks
// like. It must be the same refusal, not a foreign-key panic.
func TestLiveActivityRepo_RegisterRefusesAnUnknownRide(t *testing.T) {
	const ride = "cride0025g4"
	repo := setupLiveActivities(t, ride)

	err := repo.RegisterActivity(context.Background(), "cride0025nonexistent", "rider-1", "token-a", false)
	if !errors.Is(err, store.ErrLiveActivityRideClosed) {
		t.Fatalf("RegisterActivity on an unknown ride = %v, want ErrLiveActivityRideClosed", err)
	}
}

// TestLiveActivityRepo_MarkPushedRotatesTheTickerOrder is the anti-starvation
// fix, asserted through the query that actually orders the passes.
//
// ListActiveLegActivities orders by `updated_at ASC` and its comment calls that
// anti-starvation. Nothing moved updated_at after registration, so the order was
// a fixed permutation: with a cap of one, the second ride's Activity would never
// have been refreshed again for the whole length of its ride.
func TestLiveActivityRepo_MarkPushedRotatesTheTickerOrder(t *testing.T) {
	const rideA = "cride0025m1"
	const rideB = "cride0025m2"
	repo := setupLiveActivities(t, rideA)
	seedActivityRide(t, rideB)
	ctx := context.Background()

	for _, ride := range []string{rideA, rideB} {
		if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-"+ride, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", ride, err)
		}
	}

	// Pass one at cap=1.
	first, err := repo.ListActiveLegActivities(ctx, 1)
	if err != nil {
		t.Fatalf("first ListActiveLegActivities: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first pass returned %d rows at limit 1, want 1", len(first))
	}
	if _, err := repo.MarkActivitiesPushed(ctx, []store.LiveActivityKey{
		{RideRequestID: first[0].RideRequestID, UserID: first[0].UserID},
	}); err != nil {
		t.Fatalf("MarkActivitiesPushed: %v", err)
	}

	// Pass two must reach the OTHER one.
	second, err := repo.ListActiveLegActivities(ctx, 1)
	if err != nil {
		t.Fatalf("second ListActiveLegActivities: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second pass returned %d rows at limit 1, want 1", len(second))
	}
	if second[0].RideRequestID == first[0].RideRequestID {
		t.Errorf("both capped passes selected %s; the cap is starving the other Activity",
			first[0].RideRequestID)
	}
}

// TestLiveActivityRepo_MarkPushedSkipsEndedRows — a send that raced the ride's
// terminal end must not un-stale a tombstoned row and hold it back from the
// 24-hour sweep.
func TestLiveActivityRepo_MarkPushedSkipsEndedRows(t *testing.T) {
	const ride = "cride0025m3"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-a", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	if _, err := repo.EndActivity(ctx, ride, "rider-1"); err != nil {
		t.Fatalf("EndActivity: %v", err)
	}

	n, err := repo.MarkActivitiesPushed(ctx, []store.LiveActivityKey{
		{RideRequestID: ride, UserID: "rider-1"},
	})
	if err != nil {
		t.Fatalf("MarkActivitiesPushed: %v", err)
	}
	if n != 0 {
		t.Errorf("MarkActivitiesPushed stamped %d tombstoned rows, want 0", n)
	}
}

// TestLiveActivityRepo_MarkPushedIsEmptySafe — a pass that delivered nothing
// must not issue a statement at all.
func TestLiveActivityRepo_MarkPushedIsEmptySafe(t *testing.T) {
	const ride = "cride0025m4"
	repo := setupLiveActivities(t, ride)

	n, err := repo.MarkActivitiesPushed(context.Background(), nil)
	if err != nil || n != 0 {
		t.Errorf("MarkActivitiesPushed(nil) = %d, %v; want 0, nil", n, err)
	}
}

// TestLiveActivityRepo_RideIDsForVehicle backs the owner-teardown end push: the
// teardown needs the set of rides it is about to delete that still have a Live
// Activity, because after the delete the FK cascade has taken both the rides and
// the tokens and there is nobody left to tell.
func TestLiveActivityRepo_RideIDsForVehicle(t *testing.T) {
	const rideA = "cride0025v1"
	const rideB = "cride0025v2"
	repo := setupLiveActivities(t, rideA)
	seedActivityRide(t, rideB)
	ctx := context.Background()

	// seedActivityRide derives vehicle_id as 'veh_' || rideID, so put both
	// rides on ONE car — which is what a teardown actually faces.
	//
	// rideB becomes a SCHEDULED ride, and it has to: migration 0013's partial
	// unique index allows only one active INSTANT ride per vehicle, so "one car
	// with a ride in flight and a reservation booked" is the only shape this
	// pair can legally take. It is also the realistic one.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET vehicle_id = 'veh_shared_0025' WHERE id = $1`, rideA); err != nil {
		t.Fatalf("move ride A onto the shared vehicle: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests
		    SET vehicle_id = 'veh_shared_0025', scheduled_for = NOW() + INTERVAL '2 hours'
		  WHERE id = $1`, rideB); err != nil {
		t.Fatalf("move ride B onto the shared vehicle as a reservation: %v", err)
	}

	for _, ride := range []string{rideA, rideB} {
		if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-"+ride, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", ride, err)
		}
	}
	// One of them has already ended: it needs no push and must not appear.
	if _, err := repo.EndActivity(ctx, rideB, "rider-1"); err != nil {
		t.Fatalf("EndActivity: %v", err)
	}

	ids, err := repo.ActivityRideIDsForVehicle(ctx, "veh_shared_0025")
	if err != nil {
		t.Fatalf("ActivityRideIDsForVehicle: %v", err)
	}
	if len(ids) != 1 || ids[0].RideRequestID != rideA {
		t.Errorf("ride ids = %v, want exactly [%s] (the ended one needs no end push)", ids, rideA)
	}
	// The STATUS travels with the id (MYR-421): the teardown ends a completed
	// ride as completed and everything else as cancelled, and it cannot make
	// that distinction from an id alone.
	if len(ids) == 1 && ids[0].Status != store.RideRequestStatusAccepted {
		t.Errorf("status = %q, want %q — the teardown decides the ending from it",
			ids[0].Status, store.RideRequestStatusAccepted)
	}

	// A car with nothing in flight — the ordinary teardown — reads empty.
	empty, err := repo.ActivityRideIDsForVehicle(ctx, "veh_with_no_rides")
	if err != nil {
		t.Fatalf("ActivityRideIDsForVehicle(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ride ids for an unrelated vehicle = %v, want none", empty)
	}
}
