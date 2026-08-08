package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Ride retention pruner tests (MYR-447, data-lifecycle.md §5).
//
// Boundary cases are expressed in terms of store.RideRetentionWindow and
// store.RidePassengerScrubWindow rather than literal day counts, so that
// changing a contract constant makes these tests FOLLOW it instead of failing —
// the constants are the single source of truth for the tests too.

// ridePrunerPassengerName / Phone are the P1 plaintext values whose removal is
// half the point of the feature. They exist so "scrubbed" is a claim about real
// content rather than about a column that was already NULL.
const (
	ridePrunerPassengerName  = "Maya Chen"
	ridePrunerPassengerPhone = "(415) 555-0142"
)

func setupRidePruner(t *testing.T) *store.RidePruner {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping ride pruner integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ensureAuditSchema(t)
	// "AuditLog" is intentionally NOT cleaned — it is append-only, so a DELETE
	// raises. Every audit assertion below filters by a per-test-unique userId,
	// which is what isolates the tests from each other.
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	return store.NewRidePruner(testPool, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

// seedRide inserts one ride with the given status, aged `age` old by
// updated_at — the column the sweep's boundary is keyed off. passenger controls
// whether the two deprecated columns carry values.
func seedRide(t *testing.T, id, ownerID, vehicleID, status string, age time.Duration, passenger bool) {
	t.Helper()
	var name, phone *string
	if passenger {
		n, p := ridePrunerPassengerName, ridePrunerPassengerPhone
		name, phone = &n, &p
	}
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_ride_requests
		 (id, rider_id, owner_id, vehicle_id,
		  pickup_lat_enc, pickup_lng_enc, pickup_label, pickup_address,
		  dropoff_lat_enc, dropoff_lng_enc, dropoff_label, dropoff_address,
		  status, passenger_name, passenger_phone, created_at, updated_at)
		 VALUES ($1, $2, $3, $4,
		  'v1:enc-lat', 'v1:enc-lng', 'Home', '221 Folsom St',
		  'v1:enc-lat2', 'v1:enc-lng2', 'Caltrain', '700 4th St',
		  $5, $6, $7,
		  NOW() - make_interval(secs => $8), NOW() - make_interval(secs => $8))`,
		id, "rider-"+id, ownerID, vehicleID, status, name, phone, age.Seconds())
	if err != nil {
		t.Fatalf("seed ride %s: %v", id, err)
	}
}

func rideExists(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM go_ride_requests WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("ride exists check: %v", err)
	}
	return ok
}

// ridePassenger reads both deprecated columns back. Nil means NULL.
func ridePassenger(t *testing.T, id string) (*string, *string) {
	t.Helper()
	var name, phone *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT passenger_name, passenger_phone FROM go_ride_requests WHERE id = $1`, id).
		Scan(&name, &phone); err != nil {
		t.Fatalf("read passenger columns for %s: %v", id, err)
	}
	return name, phone
}

// rideUpdatedAt reads the age column the retention boundary keys off.
func rideUpdatedAt(t *testing.T, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT updated_at FROM go_ride_requests WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("read updated_at for %s: %v", id, err)
	}
	return ts
}

// TestRidePruneBatchRetentionBoundary is the headline test: the day either side
// of the window, for each terminal status, and the whole row going with it.
func TestRidePruneBatchRetentionBoundary(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-prune-boundary"
	const vehicle = "veh-ride-prune-boundary"
	day := 24 * time.Hour

	// All three terminal statuses, one just inside the window and one just
	// outside. Enumerated rather than sampled: the sweep's predicate lists them
	// individually, so a typo in one of the three would be invisible otherwise.
	for _, status := range []string{"completed", "declined", "cancelled"} {
		seedRide(t, "ride-in-"+status, owner, vehicle, status, store.RideRetentionWindow-day, false)
		seedRide(t, "ride-out-"+status, owner, vehicle, status, store.RideRetentionWindow+day, false)
	}

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 3 {
		t.Errorf("RidesDeleted = %d, want 3 (one per terminal status, the outside-window ones)", res.RidesDeleted)
	}

	for _, status := range []string{"completed", "declined", "cancelled"} {
		t.Run("just inside the window survives: "+status, func(t *testing.T) {
			if !rideExists(t, "ride-in-"+status) {
				t.Errorf("ride-in-%s was deleted a day early", status)
			}
		})
		t.Run("just outside the window dies: "+status, func(t *testing.T) {
			if rideExists(t, "ride-out-"+status) {
				t.Errorf("ride-out-%s survived a day past the window", status)
			}
		})
	}
}

// TestRidePruneBatchNeverTouchesOpenRides is the test that matters most, because
// the sweep runs against a table holding LIVE rides. Every open status, aged far
// past the window, must survive.
//
// The `accepted` case carries the specific trap: a reservation whose dispatch
// failed with dispatch_error = 'reservation_expired' READS like an expired,
// finished ride, and an earlier reading of this ticket treated `expired` as a
// terminal status. There is no such status. reservation_expired is a DISPATCH
// ERROR CODE on a ride that is still `accepted` — rest-api.md §7.8 promises the
// owner and rider may still cancel or proceed manually — so it is an OPEN ride
// and deleting it would destroy a booking someone can still take.
func TestRidePruneBatchNeverTouchesOpenRides(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-prune-open"
	// Far past the window: age is not what protects these rows, status is.
	age := store.RideRetentionWindow + 400*24*time.Hour

	// One vehicle per ride. Migrations 0004 and 0013 install partial UNIQUE
	// guards allowing only ONE active instant ride per rider and per vehicle, so
	// five concurrently-open rides cannot share a car.
	for _, status := range []string{"requested", "accepted", "enroute", "arrived"} {
		seedRide(t, "ride-open-"+status, owner, "veh-open-"+status, status, age, true)
	}

	// The reservation_expired ride: still `accepted`, with the dispatch outcome
	// that makes it look finished.
	seedRide(t, "ride-reservation-expired", owner, "veh-open-expired", "accepted", age, true)
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests
		 SET dispatch_status = 'failed', dispatch_error = 'reservation_expired',
		     scheduled_for = NOW() - make_interval(days => 400)
		 WHERE id = 'ride-reservation-expired'`); err != nil {
		t.Fatalf("mark reservation expired: %v", err)
	}
	// Re-age updated_at, which the UPDATE above did not touch but which a future
	// trigger might: the point of the test is status, so pin the age explicitly.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET updated_at = NOW() - make_interval(secs => $1)
		 WHERE id = 'ride-reservation-expired'`, age.Seconds()); err != nil {
		t.Fatalf("re-age reservation expired ride: %v", err)
	}

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 0 {
		t.Errorf("RidesDeleted = %d, want 0 — no open ride may ever be swept", res.RidesDeleted)
	}

	for _, status := range []string{"requested", "accepted", "enroute", "arrived"} {
		t.Run("open ride survives: "+status, func(t *testing.T) {
			if !rideExists(t, "ride-open-"+status) {
				t.Errorf("open ride in status %q was deleted", status)
			}
		})
	}

	t.Run("a reservation_expired ride is still accepted, so it is not swept", func(t *testing.T) {
		if !rideExists(t, "ride-reservation-expired") {
			t.Fatal("a ride with dispatch_error='reservation_expired' was DELETED — " +
				"it is still `accepted`, i.e. a live ride the owner may still proceed with")
		}
		// Nor scrubbed: the passenger scrub carries the same terminal-status
		// guard, so an open ride keeps its columns however old it is.
		name, phone := ridePassenger(t, "ride-reservation-expired")
		if name == nil || phone == nil {
			t.Errorf("an open ride's passenger columns were scrubbed: name=%v phone=%v", name, phone)
		}
	})
	if res.PassengersScrubbed != 0 {
		t.Errorf("PassengersScrubbed = %d, want 0 — every seeded ride here is open", res.PassengersScrubbed)
	}
}

// TestRidePruneBatchScrubsPassengersTogether covers the going-forward half: the
// 30-day boundary, both columns dying together, and idempotence.
func TestRidePruneBatchScrubsPassengersTogether(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-scrub"
	const vehicle = "veh-ride-scrub"
	day := 24 * time.Hour

	seedRide(t, "ride-scrub-fresh", owner, vehicle, "completed", store.RidePassengerScrubWindow-day, true)
	seedRide(t, "ride-scrub-due", owner, vehicle, "completed", store.RidePassengerScrubWindow+day, true)
	// A ride with no passenger at all: it must not be claimed, or the sweep
	// would rewrite every terminal ride ever taken on every pass.
	seedRide(t, "ride-scrub-none", owner, vehicle, "cancelled", store.RidePassengerScrubWindow+day, false)

	before := rideUpdatedAt(t, "ride-scrub-due")

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.PassengersScrubbed != 1 {
		t.Errorf("PassengersScrubbed = %d, want 1 (only the past-window ride with data)", res.PassengersScrubbed)
	}
	if res.RidesDeleted != 0 {
		t.Errorf("RidesDeleted = %d, want 0 — nothing here is a year old", res.RidesDeleted)
	}

	t.Run("inside the scrub window keeps both columns", func(t *testing.T) {
		name, phone := ridePassenger(t, "ride-scrub-fresh")
		if name == nil || *name != ridePrunerPassengerName {
			t.Errorf("passenger_name = %v, want it intact", name)
		}
		if phone == nil || *phone != ridePrunerPassengerPhone {
			t.Errorf("passenger_phone = %v, want it intact", phone)
		}
	})

	t.Run("past the scrub window BOTH columns are NULL, never one", func(t *testing.T) {
		name, phone := ridePassenger(t, "ride-scrub-due")
		// Asserted as a pair on purpose. A phone-only scrub leaves a
		// name-without-phone row, which the iOS passenger block renders as a
		// leading blank rather than as no passenger at all
		// (ScheduledRideSheet.swift:541 / RideRequestBackend.swift:126-129).
		if name != nil || phone != nil {
			t.Errorf("passenger columns = %v / %v, want BOTH NULL — a half-scrubbed row renders broken",
				name, phone)
		}
	})

	t.Run("the scrub does not bump updated_at", func(t *testing.T) {
		// If it did, the scrubbed row's apparent age would reset to zero and its
		// 365-day deletion would be deferred by another full year — a privacy
		// sweep defeating a privacy sweep.
		if after := rideUpdatedAt(t, "ride-scrub-due"); !after.Equal(before) {
			t.Errorf("updated_at moved from %v to %v; the 365-day boundary keys off this column", before, after)
		}
	})

	t.Run("the row otherwise survives whole", func(t *testing.T) {
		if !rideExists(t, "ride-scrub-due") {
			t.Fatal("the scrub deleted the row instead of emptying two columns")
		}
		var label, addr string
		if err := testPool.QueryRow(ctx,
			`SELECT pickup_label, pickup_address FROM go_ride_requests WHERE id = 'ride-scrub-due'`).
			Scan(&label, &addr); err != nil {
			t.Fatalf("read surviving ride: %v", err)
		}
		if label == "" || addr == "" {
			t.Error("the scrub took more than the two passenger columns")
		}
	})

	t.Run("a second pass claims nothing — the selection is idempotent", func(t *testing.T) {
		again, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
		if err != nil {
			t.Fatalf("PruneBatch (second): %v", err)
		}
		if again.PassengersScrubbed != 0 {
			t.Errorf("second pass scrubbed %d rows, want 0", again.PassengersScrubbed)
		}
		if !again.Exhausted {
			t.Error("second pass did not report Exhausted with nothing left to do")
		}
	})
}

// TestRidePruneBatchWritesGroupedAudit covers CG-DL-3 and the P0-only metadata
// rule, for both actions.
func TestRidePruneBatchWritesGroupedAudit(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-prune-audit"
	deleteAge := store.RideRetentionWindow + 48*time.Hour
	scrubAge := store.RidePassengerScrubWindow + 48*time.Hour

	// Two cars' worth of deletable rides, plus one scrubbable ride on the first
	// car: three audit rows total (two rides_pruned, one
	// ride_passengers_scrubbed), not four and not one.
	seedRide(t, "ride-audit-a1", owner, "veh-audit-ride-a", "completed", deleteAge, false)
	seedRide(t, "ride-audit-a2", owner, "veh-audit-ride-a", "cancelled", deleteAge+time.Hour, false)
	seedRide(t, "ride-audit-b1", owner, "veh-audit-ride-b", "declined", deleteAge, false)
	seedRide(t, "ride-audit-scrub", owner, "veh-audit-ride-a", "completed", scrubAge, true)

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 3 {
		t.Errorf("RidesDeleted = %d, want 3", res.RidesDeleted)
	}
	if res.PassengersScrubbed != 1 {
		t.Errorf("PassengersScrubbed = %d, want 1", res.PassengersScrubbed)
	}
	if res.AuditRows != 3 {
		t.Errorf("AuditRows = %d, want 3 (two pruned groups + one scrub group)", res.AuditRows)
	}

	rows, err := testPool.Query(ctx,
		`SELECT "action","targetType","targetId","initiator","metadata"::text
		 FROM "AuditLog" WHERE "userId" = $1 ORDER BY "action", "targetId"`, owner)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()

	type auditRow struct{ action, targetType, targetID, initiator, metadata string }
	var got []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.action, &a.targetType, &a.targetID, &a.initiator, &a.metadata); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("audit rows = %d, want 3", len(got))
	}

	// Ordered by action then targetId: ride_passengers_scrubbed(a),
	// rides_pruned(a), rides_pruned(b).
	wantActions := []string{"ride_passengers_scrubbed", "rides_pruned", "rides_pruned"}
	wantTargets := []string{"veh-audit-ride-a", "veh-audit-ride-a", "veh-audit-ride-b"}
	for i, a := range got {
		if a.action != wantActions[i] {
			t.Errorf("row %d action = %q, want %q", i, a.action, wantActions[i])
		}
		if a.targetID != wantTargets[i] {
			t.Errorf("row %d targetId = %q, want %q (the owner's car, not a ride id)", i, a.targetID, wantTargets[i])
		}
		if a.targetType != "ride_request" {
			t.Errorf("row %d targetType = %q, want ride_request", i, a.targetType)
		}
		if a.initiator != "system_pruner" {
			t.Errorf("row %d initiator = %q, want system_pruner", i, a.initiator)
		}

		// CG-DL-5: P0 only. Counts and an opaque window — and nothing picked up
		// from the P1 columns the batch destroyed.
		var meta map[string]any
		if err := json.Unmarshal([]byte(a.metadata), &meta); err != nil {
			t.Fatalf("row %d metadata is not JSON: %v", i, err)
		}
		for _, key := range []string{"rideCount", "oldestRideDate", "newestRideDate"} {
			if _, ok := meta[key]; !ok {
				t.Errorf("row %d metadata missing %q: %s", i, key, a.metadata)
			}
		}
		if len(meta) != 3 {
			t.Errorf("row %d metadata has %d keys, want exactly 3 (P0 only): %s", i, len(meta), a.metadata)
		}
		for _, forbidden := range []string{
			ridePrunerPassengerName, ridePrunerPassengerPhone,
			"Home", "Caltrain", "221 Folsom St", "700 4th St", "enc-lat",
		} {
			if strings.Contains(a.metadata, forbidden) {
				t.Errorf("row %d metadata leaked P1 content %q: %s", i, forbidden, a.metadata)
			}
		}
	}

	// The grouped rides_pruned row for car A covers both of its rides.
	var metaA map[string]any
	if err := json.Unmarshal([]byte(got[1].metadata), &metaA); err != nil {
		t.Fatalf("unmarshal rides_pruned metadata for car A: %v", err)
	}
	if count, _ := metaA["rideCount"].(float64); count != 2 {
		t.Errorf("car A rideCount = %v, want 2", metaA["rideCount"])
	}
}

// TestRidePruneBatchConvergesOverBatches proves the LIMIT loop: a backlog larger
// than one batch drains in successive bounded batches, and the last one reports
// Exhausted so the pass loop terminates.
func TestRidePruneBatchConvergesOverBatches(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-batching"
	const vehicle = "veh-ride-batching"
	const batchSize = 10
	const seeded = 25 // deliberately not a multiple of the batch size

	age := store.RideRetentionWindow + time.Hour
	for i := range seeded {
		seedRide(t, fmt.Sprintf("ride-batch-%02d", i), owner, vehicle, "completed",
			age+time.Duration(i)*time.Minute, false)
	}
	// One in-window ride, to prove the loop stops at the boundary rather than at
	// "no rows left".
	seedRide(t, "ride-batch-keep", owner, vehicle, "completed", store.RideRetentionWindow-time.Hour, false)

	var totalDeleted, batches int
	for {
		res, err := pruner.PruneBatch(ctx, batchSize)
		if err != nil {
			t.Fatalf("PruneBatch (batch %d): %v", batches, err)
		}
		batches++
		totalDeleted += res.RidesDeleted
		if res.RidesDeleted > batchSize {
			t.Fatalf("batch %d deleted %d rows, exceeding the %d cap", batches, res.RidesDeleted, batchSize)
		}
		if res.Exhausted {
			break
		}
		if batches > 10 {
			t.Fatal("prune loop did not converge")
		}
	}

	if totalDeleted != seeded {
		t.Errorf("total deleted = %d, want %d", totalDeleted, seeded)
	}
	// 25 rows at 10 per batch: 10, 10, 5, then a FOURTH call that claims nothing
	// and reports Exhausted. The short third batch deliberately does NOT
	// terminate the loop — under SKIP LOCKED "short" means "no rows I can
	// claim", which a peer holding the rest also satisfies.
	if batches != 4 {
		t.Errorf("batches = %d, want 4 (three deleting batches + the confirming empty claim)", batches)
	}
	if !rideExists(t, "ride-batch-keep") {
		t.Error("in-window ride was deleted by the batch loop")
	}
}

// TestRidePruneBatchEmptyIsCleanNoop covers the steady state: most nights there
// is nothing to do, and that must cost two claims and no audit row.
func TestRidePruneBatchEmptyIsCleanNoop(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-noop"
	seedRide(t, "ride-noop-fresh", owner, "veh-ride-noop", "completed", time.Hour, true)

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 0 || res.PassengersScrubbed != 0 || res.AuditRows != 0 {
		t.Errorf("empty prune deleted %d / scrubbed %d / wrote %d audit rows, want 0/0/0",
			res.RidesDeleted, res.PassengersScrubbed, res.AuditRows)
	}
	if !res.Exhausted {
		t.Error("empty prune did not report Exhausted")
	}
	if n := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "userId" = $1`, owner); n != 0 {
		t.Errorf("audit rows = %d, want 0 — a no-op prune must not write one", n)
	}
}

// TestRidePruneBatchRejectsNonPositiveBatchSize guards the programming-error path.
func TestRidePruneBatchRejectsNonPositiveBatchSize(t *testing.T) {
	pruner := setupRidePruner(t)
	for _, size := range []int{0, -1} {
		if _, err := pruner.PruneBatch(context.Background(), size); err == nil {
			t.Errorf("PruneBatch(%d) returned nil error, want a rejection", size)
		}
	}
}

// TestRidePruneBatchDeleteBeatsScrub pins the within-transaction ordering: a
// ride old enough for BOTH jobs is DELETED, not scrubbed-then-deleted and not
// scrubbed-and-kept. A scrub-first ordering would rewrite rows about to be
// destroyed and emit a ride_passengers_scrubbed audit row about data that no
// longer exists.
func TestRidePruneBatchDeleteBeatsScrub(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-order"
	seedRide(t, "ride-order-old", owner, "veh-ride-order", "completed",
		store.RideRetentionWindow+time.Hour, true)

	res, err := pruner.PruneBatch(ctx, store.DefaultRidePruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 1 {
		t.Errorf("RidesDeleted = %d, want 1", res.RidesDeleted)
	}
	if res.PassengersScrubbed != 0 {
		t.Errorf("PassengersScrubbed = %d, want 0 — the row was deleted, so there was nothing to scrub",
			res.PassengersScrubbed)
	}
	if res.AuditRows != 1 {
		t.Errorf("AuditRows = %d, want 1 (rides_pruned only)", res.AuditRows)
	}
	if rideExists(t, "ride-order-old") {
		t.Error("the ride survived the delete")
	}
}

// TestRidePruneBatchScrubNeverTouchesDeletableRides is the CROSS-BATCH half
// of the ordering above, and the case that made the upper age bound
// necessary.
//
// Running the delete first is only sufficient WITHIN one batch. With more
// expired rides than fit in a batch, the delete takes the oldest N and a
// tail-only scrub claim — ordered identically — would take the NEXT N,
// which are also past the retention window and are simply waiting for the
// following batch to destroy them. The sweep would rewrite rows it is about
// to delete, and emit a `ride_passengers_scrubbed` audit row asserting that
// a SURVIVING record had two fields removed, minutes before that record
// stopped existing. The two actions make different promises, so an audit
// trail that confuses them is worse than one that omits the scrub.
//
// A batch size of 1 reproduces it deterministically without seeding
// hundreds of rows.
func TestRidePruneBatchScrubNeverTouchesDeletableRides(t *testing.T) {
	pruner := setupRidePruner(t)
	ctx := context.Background()

	const owner = "user-ride-band"
	// Both are past the 365-day deletion boundary and both carry passenger
	// data. With batchSize 1 the delete can only claim the older one.
	seedRide(t, "ride-band-older", owner, "veh-ride-band", "completed",
		store.RideRetentionWindow+48*time.Hour, true)
	seedRide(t, "ride-band-newer", owner, "veh-ride-band", "completed",
		store.RideRetentionWindow+time.Hour, true)

	res, err := pruner.PruneBatch(ctx, 1)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.RidesDeleted != 1 {
		t.Fatalf("RidesDeleted = %d, want 1", res.RidesDeleted)
	}
	if res.PassengersScrubbed != 0 {
		t.Errorf("PassengersScrubbed = %d, want 0 — the row the scrub could reach "+
			"is past the retention window and belongs to the DELETE job; scrubbing it "+
			"would audit a survival that is about to stop being true", res.PassengersScrubbed)
	}
	if !rideExists(t, "ride-band-newer") {
		t.Fatal("the second ride should still be waiting for the next batch")
	}
	// It must still carry its passenger data: untouched, not scrubbed.
	if name, _ := ridePassenger(t, "ride-band-newer"); name == nil {
		t.Error("the surviving ride was scrubbed by the tail claim the upper bound exists to prevent")
	}

	// The next pass deletes it — never having scrubbed it first.
	res2, err := pruner.PruneBatch(ctx, 1)
	if err != nil {
		t.Fatalf("second PruneBatch: %v", err)
	}
	if res2.RidesDeleted != 1 || res2.PassengersScrubbed != 0 {
		t.Errorf("second batch = %d deleted / %d scrubbed, want 1 / 0",
			res2.RidesDeleted, res2.PassengersScrubbed)
	}
}
