package store

import (
	"context"
	"fmt"
	"time"
)

// The held completion `end`'s read path (MYR-421).
//
// A completed ride's `end` push is no longer sent beside its alerting update.
// An `.ended` Activity LEAVES THE DYNAMIC ISLAND about 1.4 seconds later
// (measured on device), so the MYR-418 pair — alerting update, then the end one
// second behind it — put the sixth expansion's check on the island for roughly
// two seconds before the Activity vanished from it altogether. The lock-screen
// card still lingered for five minutes via its dismissal-date; the ISLAND
// presence died with the end, and the check was unfindable in practice.
//
// So the end is HELD for the linger horizon and this query is what finds it
// again. It lives in its own file rather than in live_activity_repo.go for the
// same reason live_activity_presence.go does: that file is already past this
// project's 300-line cap.
//
// THE COMPLETION INSTANT IS THE RIDE'S, NOT THE ACTIVITY'S, and that is what
// makes the hold crash-safe without a new column. `go_ride_requests.completed_at`
// is stamped by the status write itself (ride_request_status_queries.go), in the
// same transaction that made the ride terminal, so it survives a restart, a
// deploy, and a process that died between the update and the end. A duration
// counted from anything the push path wrote would not: the push path is
// best-effort by design and its writes are exactly the ones a crash loses.
//
// COALESCE onto `updated_at` is a floor, not a fallback anybody expects to
// use. Every path that sets `status = 'completed'` also stamps `completed_at`,
// but the column is nullable and predates this read; a row that somehow reached
// `completed` without one would otherwise be invisible here FOREVER — the ride
// is terminal so no transition will ever fire again, and the ticker does not
// list completed rides. `updated_at` is NOT NULL and can only be at or after the
// transition, so the worst case degrades to "held slightly longer", never to
// "never ended".

// queryListRidesAwaitingActivityEnd finds completed rides whose held-back `end`
// push has come due.
//
// GROUPED BY RIDE, because the `end` is sent per ride: endRide fans out over
// every Activity registered against one ride and tombstones them together, so a
// ride with two parties watching must appear here once, not twice.
//
// Ordered by the completion instant so the most overdue card is ended first.
// That ordering only bites when the LIMIT does — after a restart that left a
// backlog, which is precisely when "oldest first" is the ordering worth having.
//
// The status literal is spelled inline, as it is in the registration guard next
// door: this predicate is about the one terminal status whose end is held, and
// the other two (`declined`, `cancelled`) are deliberately absent because their
// end went out immediately.
const queryListRidesAwaitingActivityEnd = `
SELECT a.ride_request_id
FROM go_live_activities a
JOIN go_ride_requests r ON r.id = a.ride_request_id
WHERE a.ended_at IS NULL
  AND r.status = 'completed'
  AND COALESCE(r.completed_at, r.updated_at) <= NOW() - make_interval(secs => $1)
GROUP BY a.ride_request_id
ORDER BY MIN(COALESCE(r.completed_at, r.updated_at)) ASC
LIMIT $2`

// ListRidesAwaitingActivityEnd returns up to limit rides that completed at
// least heldFor ago and still have a live Activity registered against them —
// the set whose `end` push is due.
//
// An empty result is the ordinary state: most of the time no ride completed in
// the last five minutes. It is not an error.
func (r *LiveActivityRepo) ListRidesAwaitingActivityEnd(ctx context.Context, heldFor time.Duration, limit int) ([]string, error) {
	if heldFor < 0 {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: negative hold %s", heldFor)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: non-positive limit %d", limit)
	}

	rows, err := r.pool.Query(ctx, queryListRidesAwaitingActivityEnd, heldFor.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd(hold=%s): %w", heldFor, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: iterate: %w", err)
	}
	return out, nil
}
