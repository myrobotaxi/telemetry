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
