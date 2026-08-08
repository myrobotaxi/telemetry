// The four statements the ride retention sweep runs (MYR-447). Split out of
// ride_pruner.go so the SQL and its rationale sit together and the orchestration
// file stays readable — the same split ride_request_queries.go /
// ride_request_status_queries.go already uses for this table.

package store

import "fmt"

// rideTerminalStatusPredicate is the ONLY definition of "this ride is over",
// written once and pasted into all four statements below.
//
// THE THREE STATUSES ARE ENUMERATED POSITIVELY, never as NOT IN (open). That is
// the convention ride_request_queries.go already states for the open-ride
// predicate, and here it is load-bearing rather than stylistic: this predicate
// scopes a DELETE. Expressed negatively, adding a lifecycle state to the CHECK
// constraint in migration 0002 would silently enrol it in a destructive sweep;
// expressed positively, the new state is simply not swept until someone edits
// this line on purpose.
//
// WHAT IS NOT TERMINAL, because it reads like it should be: a ride whose
// dispatch failed with dispatch_error = 'reservation_expired'. There is no
// `expired` ride status — `reservation_expired` is a DISPATCH ERROR CODE, and
// internal/dispatch/reservation_worker.go is explicit that it introduces "NO NEW
// WIRE STATUS, deliberately". Such a ride is still `accepted`: rest-api.md §7.8
// promises the owner and rider may still cancel or proceed manually, so it is a
// LIVE ride and the sweep must never see it. `accepted` is absent from the list
// below, which is what enforces that.
const rideTerminalStatusPredicate = `status IN ('completed', 'declined', 'cancelled')`

// rideAgePredicate is the age boundary, rendered with a day count.
//
// WHY updated_at AND NOT A PER-ROW TERMINAL TIMESTAMP. There isn't one.
// completed_at is stamped only on entry to `completed`; `declined` and
// `cancelled` stamp no timestamp of their own (see rideRequestStatusStamp in
// ride_request_status_queries.go), so two of the three terminal states have
// nothing to key off. What that shared stamp clause DOES do on every transition
// is `updated_at = NOW()`.
//
// That makes updated_at a LOWER BOUND on age-since-terminal: the row's last
// write was at updated_at, the transition into a terminal state was at or before
// it, and nothing moves a terminal ride afterwards. So
// `updated_at < NOW() - 365d` IMPLIES the ride went terminal at least 365 days
// ago. The error is one-directional — a row edited after going terminal is
// retained LONGER than the window, never deleted sooner. Over-retention is a
// tolerable miss; over-deletion is not, and this predicate cannot produce it.
//
// A terminated_at column was considered and rejected: it would need a
// backfill with no honest value to backfill FROM, and it would put a second
// authority on "when did this end" beside a stamp clause that is already the
// single writer. COALESCE(completed_at, updated_at) was also rejected — it is
// STRICTLY WORSE, because on the completed arm it drops back to the true
// terminal instant and so deletes SOONER than updated_at would, giving up the
// conservative direction on exactly the arm that has real P1 content, while
// mixing two column semantics in one boundary and leaving no single clean index
// to serve it.
func rideAgePredicate(days int) string {
	return fmt.Sprintf(`updated_at < NOW() - make_interval(days => %d)`, days)
}

// queryPrunableRideBatch claims one batch of expired terminal rides.
//
// Four things about this statement are load-bearing:
//
//   - NO JOIN. go_ride_requests carries owner_id, rider_id and vehicle_id on the
//     row itself, so the audit grouping needs nothing from "Vehicle" — one fewer
//     table in the lock footprint than the drive pruner's equivalent.
//   - BOTH guards are present: the age boundary AND the terminal-status list. A
//     claim missing the second would lock live rides.
//   - ORDER BY updated_at ASC makes the sweep oldest-first, so a backlog drains
//     in the order the data aged and a budget-capped pass always makes progress
//     against the worst offenders. It is also exactly the order migration 0030's
//     partial index stores, so the LIMIT short-circuits the sort.
//   - FOR UPDATE SKIP LOCKED replaces §5.8's "leader-elected" requirement with
//     something that needs no coordinator. Two replicas that both wake at 03:00
//     UTC claim DISJOINT batches; a replica that dies mid-batch releases its
//     claim on rollback. Correct here precisely because the work set is
//     unordered with respect to correctness — any expired terminal ride may be
//     deleted by anyone.
//
// It selects no P1 column: not the pickup/dropoff ciphertext, not the labels or
// addresses, not the passenger fields. The pruner never reads what it destroys.
var queryPrunableRideBatch = fmt.Sprintf(`
SELECT id, vehicle_id, owner_id, updated_at
FROM go_ride_requests
WHERE %s
  AND %s
ORDER BY updated_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED`, rideAgePredicate(RideRetentionDays), rideTerminalStatusPredicate)

// queryPruneDeleteRides deletes one claimed batch by id.
//
// BOTH GUARDS ARE REPEATED here rather than trusted from the SELECT. The ids are
// already claimed and locked, so this cannot change the outcome in normal
// operation — it exists so that no future edit to the claim query can turn this
// statement into an unbounded delete. The drive pruner repeats its age predicate
// for the same reason ("the boundary is asserted at the point of destruction");
// the stakes are higher here, because go_ride_requests holds OPEN rides too, and
// a mis-scoped delete would destroy a ride someone is currently taking rather
// than merely over-deleting history.
//
// It names no column, so the row goes whole: both encrypted coordinate pairs,
// both labels, both addresses, and the deprecated passenger name/phone.
//
// CASCADE FOOTPRINT. go_live_activities.ride_request_id is the only foreign key
// pointing at this table (migration 0025, ON DELETE CASCADE), so this DELETE
// also takes row locks in go_live_activities and destroys any Live Activity
// sidecar rows for the pruned rides. That is correct — an LA for a ride deleted
// a year after it ended is dead weight — but it does mean the batch's lock
// footprint spans two tables, which is the reason the batch stays small.
var queryPruneDeleteRides = fmt.Sprintf(`
DELETE FROM go_ride_requests
WHERE id = ANY($1)
  AND %s
  AND %s`, rideAgePredicate(RideRetentionDays), rideTerminalStatusPredicate)

// queryScrubbableRideBatch claims one batch of terminal rides still carrying
// booked-for-passenger data past the scrub window.
//
// The `passenger_name IS NOT NULL OR passenger_phone IS NOT NULL` arm is what
// makes the sweep IDEMPOTENT: an already-scrubbed row does not match, so a
// second pass over the same night's work claims nothing and the steady state is
// an empty claim rather than a rewrite of every terminal ride ever taken.
//
// It selects the row's identity and nothing else — never the passenger values
// themselves. Reading a P1 string in order to overwrite it would put it in a Go
// heap and one careless log line away from disk.
//
// THE UPPER AGE BOUND IS WHAT KEEPS THE DELETE-BEFORE-SCRUB ORDERING HONEST
// ACROSS BATCHES. Running the delete first is only sufficient WITHIN one batch:
// with more than batchSize expired rides carrying passenger data, the delete
// takes the oldest 100 and this claim — ordered the same way — would take the
// NEXT 100, which are also past 365 days and are simply waiting for the next
// batch to destroy them. The sweep would then rewrite rows it is about to
// delete and, worse, emit a `ride_passengers_scrubbed` audit row asserting that
// a surviving record had two fields removed, minutes before that record ceased
// to exist. The two actions make different promises (see data-lifecycle.md
// §4.2) and an audit trail that confuses them is worse than one that omits the
// scrub entirely.
//
// So the scrub's window is a HALF-OPEN BAND, not a tail: older than the 30-day
// scrub boundary, and younger than the 365-day deletion boundary. Rides past
// 365 days are the delete job's alone.
var queryScrubbableRideBatch = fmt.Sprintf(`
SELECT id, vehicle_id, owner_id, updated_at
FROM go_ride_requests
WHERE %s
  AND updated_at >= NOW() - make_interval(days => %d)
  AND %s
  AND (passenger_name IS NOT NULL OR passenger_phone IS NOT NULL)
ORDER BY updated_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED`, rideAgePredicate(RidePassengerScrubDays), RideRetentionDays, rideTerminalStatusPredicate)

// queryScrubRidePassengers NULLs both passenger columns on one claimed batch.
//
// ALWAYS BOTH COLUMNS, NEVER PHONE ONLY. The iOS client renders the block as
// `"\(passenger.phone) · ..."` (ScheduledRideSheet.swift:541) behind a single
// `if let passenger` whose presence test is the NAME
// (RideRequestBackend.swift:126-129 maps absent-or-empty name to "no passenger
// at all"). Scrubbing the phone but leaving the name therefore does not remove
// the passenger — it renders the block with a leading blank where the number
// was. Scrubbing both makes the whole `if let passenger` branch disappear
// cleanly, which is the only outcome the UI has a correct rendering for. Any
// future variant of this statement must keep the two SET clauses together.
//
// IT DOES NOT TOUCH updated_at, and that omission is load-bearing rather than an
// oversight. updated_at is the age boundary the 365-day DELETE keys off (see
// rideAgePredicate); bumping it here would reset every scrubbed ride's apparent
// age to zero and push its deletion out another full year — a privacy sweep
// that defeats a privacy sweep. Postgres will not touch the column on its own:
// there is no BEFORE UPDATE trigger on go_ride_requests, only the column
// default, which fires on INSERT.
//
// Both guards are repeated here for the same reason the DELETE repeats them,
// and so is the upper age bound that keeps this statement off rows the delete
// job owns.
var queryScrubRidePassengers = fmt.Sprintf(`
UPDATE go_ride_requests
SET passenger_name = NULL,
    passenger_phone = NULL
WHERE id = ANY($1)
  AND %s
  AND updated_at >= NOW() - make_interval(days => %d)
  AND %s`, rideAgePredicate(RidePassengerScrubDays), RideRetentionDays, rideTerminalStatusPredicate)
