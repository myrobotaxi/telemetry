package store

import (
	"strings"
	"testing"
)

// Query-SHAPE guards for the MYR-179 sweep (no database needed). The
// integration tests prove what the queries return; these pin the properties a
// future edit could silently drop — the ones whose loss is invisible in a
// functional test but expensive or unsafe in production.

// TestQueryRideRequestListDue_IsLean pins the lean projection. The sweep runs
// on every replica every 30 seconds forever; dragging requesterIdentitySelect's
// eight correlated identity subselects (and the dropoff decrypts) through it
// would re-pay that cost for every candidate on every tick, including the ones
// the pass merely holds — and would pull P1 rider PII into a background loop
// that has no use for it.
func TestQueryRideRequestListDue_IsLean(t *testing.T) {
	for _, banned := range []string{
		`"User"`, "go_identity_apple", "go_users", // requester identity (P1 PII)
		"dropoff_lat_enc", "dropoff_lng_enc", // the sweeper pushes the PICKUP
		"passenger_name", "passenger_phone", // P1 contact details
	} {
		if strings.Contains(queryRideRequestListDue, banned) {
			t.Errorf("the due query references %s — the sweep projection must stay lean "+
				"(see rideRequestDueColumns)", banned)
		}
	}
}

// TestQueryRideRequestListDue_MatchesTheIndexPredicate keeps the query and
// migration 0016's partial index in lockstep. If a conjunct here stops matching
// the index predicate, Postgres silently falls back to a sequential scan of the
// whole ride table on every tick — a latent, log-free path to total
// reservation-dispatch outage.
func TestQueryRideRequestListDue_MatchesTheIndexPredicate(t *testing.T) {
	for _, conjunct := range []string{
		"d.scheduled_for IS NOT NULL",
		"d.status = 'accepted'",
		"d.dispatched_at IS NULL",
		"d.scheduled_for <= $1",
		"ORDER BY d.scheduled_for ASC",
	} {
		if !strings.Contains(queryRideRequestListDue, conjunct) {
			t.Errorf("the due query lost %q — idx_go_ride_requests_reservation_due "+
				"(migration 0016) can no longer serve it", conjunct)
		}
	}
	// The anti-starvation clause: a held row leaves the LIMIT window, but a
	// past-ceiling row comes back so it can always be resolved.
	if !strings.Contains(queryRideRequestListDue, "d.scheduled_for <= $2") ||
		!strings.Contains(queryRideRequestListDue, "NOT EXISTS") {
		t.Error("the due query lost its anti-starvation clause (expiry OR vehicle-free)")
	}
	if !strings.Contains(queryRideRequestListDue, activeInstantRidePredicate) {
		t.Error("the due query's busy filter drifted from activeInstantRidePredicate — " +
			"it must stay character-for-character the migration 0013 index predicate")
	}
}

// TestQueryRideRequestClaimReservationDispatch_ReValidates is the cancelled-
// mid-sweep guard, pinned at the SQL. Losing either conjunct re-opens a real
// failure: without the status check the car is dialed to a pickup the rider
// cancelled; without the scheduled_for check the sweeper could latch an instant
// ride out from under the accept path.
func TestQueryRideRequestClaimReservationDispatch_ReValidates(t *testing.T) {
	for _, conjunct := range []string{
		"dispatched_at IS NULL",
		"status = 'accepted'",
		"scheduled_for IS NOT NULL",
		"RETURNING id",
	} {
		if !strings.Contains(queryRideRequestClaimReservationDispatch, conjunct) {
			t.Errorf("the reservation claim lost %q", conjunct)
		}
	}
	// It must stamp the SAME latch the instant path and the startup reconciler
	// use — a separate column would make an interrupted reservation dispatch
	// invisible to reconciliation.
	if !strings.Contains(queryRideRequestClaimReservationDispatch, "dispatched_at = NOW()") {
		t.Error("the reservation claim must stamp the shared dispatched_at latch")
	}
}

// TestQueryRideRequestClaimDispatch_StaysUnguarded is the other side of that
// coin: MYR-179 deliberately left the INSTANT path's claim byte-identical to
// MYR-176 rather than adding a status conjunct to a hot path whose window is
// milliseconds. The guarded variant is reservation-only, on purpose.
func TestQueryRideRequestClaimDispatch_StaysUnguarded(t *testing.T) {
	if strings.Contains(queryRideRequestClaimDispatch, "status") {
		t.Error("the instant claim gained a status guard — MYR-179 must not change " +
			"instant-dispatch behaviour; use queryRideRequestClaimReservationDispatch")
	}
}

// TestQueryRideRequestListLapsed_MatchesTheIndexPredicate keeps the MYR-555
// second pass and migration 0041's partial index in lockstep, for the same
// reason the due query is pinned against 0016: if a conjunct stops matching,
// Postgres falls back to a sequential scan of the whole ride table on every
// tick, silently and with no log line to find it by.
func TestQueryRideRequestListLapsed_MatchesTheIndexPredicate(t *testing.T) {
	for _, conjunct := range []string{
		"scheduled_for IS NOT NULL",
		"status = 'accepted'",
		"dispatched_at IS NOT NULL",
		"COALESCE(dispatch_error, '') <> 'reservation_expired'",
	} {
		if !strings.Contains(queryRideRequestListLapsedDispatches, conjunct) {
			t.Errorf("the lapsed query dropped %q — it must match "+
				"idx_go_ride_requests_reservation_lapsed's predicate exactly", conjunct)
		}
	}
}

// TestQueryRideRequestExpireDispatched_IsItsOwnLatch pins the property that
// makes the lapsed pass self-terminating. `dispatched_at` is already stamped on
// every row in this set, so ClaimReservationDispatch can never win here and the
// one-winner guarantee has to live in this statement. Drop the dispatch_error
// conjunct and the sweeper re-ends the rider's Live Activity every 30 seconds
// for the life of the row.
func TestQueryRideRequestExpireDispatched_IsItsOwnLatch(t *testing.T) {
	if !strings.Contains(queryRideRequestExpireDispatchedReservation,
		"COALESCE(dispatch_error, '') <> 'reservation_expired'") {
		t.Error("the lapse verdict is no longer one-winner: its predicate must exclude " +
			"rows that already carry it")
	}
	// Scoped to `accepted` (MYR-461): a ride the parties rescued into `arrived`
	// must be out of reach, or its rider's card is tombstoned mid-ride.
	if !strings.Contains(queryRideRequestExpireDispatchedReservation, "status = 'accepted'") {
		t.Error("the lapse verdict is no longer scoped to `accepted` — it could land on a " +
			"ride that is genuinely happening")
	}
	// The ride row itself is never touched: expiry is a verdict on the
	// DISPATCH, and rest-api.md §7.8 promises the parties may still proceed.
	if strings.Contains(queryRideRequestExpireDispatchedReservation, "SET\n\tstatus") ||
		strings.Contains(queryRideRequestExpireDispatchedReservation, "status =\n") {
		t.Error("the lapse verdict writes the ride's status, want the row left at `accepted`")
	}
}

// TestLapsedPredicateIsTheDueQuerysComplement states the relationship in one
// assertion: the first pass sees only UNCLAIMED reservations, the second only
// CLAIMED ones. Between them no accepted reservation can fall through — which
// is precisely what happened before MYR-555 (prod c92b2c86).
func TestLapsedPredicateIsTheDueQuerysComplement(t *testing.T) {
	if !strings.Contains(queryRideRequestListDue, "d.dispatched_at IS NULL") {
		t.Fatal("the due query no longer filters on an unclaimed latch")
	}
	if !strings.Contains(lapsedDispatchPredicate, "dispatched_at IS NOT NULL") {
		t.Fatal("the lapsed predicate no longer filters on a claimed latch")
	}
}
