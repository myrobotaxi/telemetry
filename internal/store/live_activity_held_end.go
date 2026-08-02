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
// push has come due AND is still worth attempting.
//
// GROUPED BY RIDE, because the `end` is sent per ride: endRide fans out over
// every Activity registered against one ride and tombstones them together, so a
// ride with two parties watching must appear here once, not twice.
//
// A WINDOW, NOT A FLOOR. The lower bound is the hold; the upper bound is the
// give-up horizon, and it is here rather than only in the caller because a row
// past the horizon must not consume one of the LIMIT's slots. Rows above the
// ceiling are retired in bulk by queryAbandonExpiredHeldEnds below, which runs
// first on every pass.
//
// NEWEST COMPLETION FIRST, and this is a REVERSAL of the ordering this query
// shipped with. Oldest-first was chosen so that a capped pass shed the least
// overdue — sound while the list drained on every pass, and wrong once a failed
// end began leaving its row in place. A ride that cannot be delivered to sits at
// the HEAD of every subsequent pass and consumes a slot forever, so a few
// hundred poisoned rows (one bad APNS_TOPIC will do it) would push every newly
// completed ride out of reach until the poison aged out. Newest-first makes the
// cap shed the rides whose end matters least — the oldest, whose card is
// stalest and which are closest to the horizon that will retire them anyway —
// and the total drain time for a healthy backlog is identical either way, since
// the same number leaves per pass regardless of order.
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
  AND COALESCE(r.completed_at, r.updated_at) >  NOW() - make_interval(secs => $2)
GROUP BY a.ride_request_id
ORDER BY MIN(COALESCE(r.completed_at, r.updated_at)) DESC
LIMIT $3`

// queryAbandonExpiredHeldEnds retires completed rides whose `end` was never
// delivered inside the give-up horizon.
//
// THIS IS THE LOOP'S TERMINATOR, and it is a single unbounded statement rather
// than a per-ride decision on purpose. Retrying is what keeps a transient
// failure from stranding a card, but a retry with no horizon is not a retry, it
// is a permanent resident: only 410 and 400 take a row out of the due list, so
// a 403 TopicDisallowed or a 404 would be re-attempted every 75 seconds until
// the 24-hour sweep DELETEd the registration — about 1,150 doomed pushes, and
// an end that was never sent.
//
// It cannot be starved, which is the property the per-ride path cannot offer.
// The due list is LIMITed and ordered newest-first, so the oldest rows — exactly
// the ones at the horizon — are the last a capped pass would reach and might
// never be reached at all. A bulk UPDATE with no LIMIT is immune to both.
//
// No push goes out. There is nothing honest left to send: the card has been
// showing a completed ride for half an hour, the client's own five-minute
// reaper has long since removed it on any phone whose app is alive, and the
// alternative to giving up is pretending indefinitely. The caller logs the count
// loudly — an abandoned card is a real, if bounded, failure.
const queryAbandonExpiredHeldEnds = `
UPDATE go_live_activities a
SET ended_at = NOW(), updated_at = NOW()
FROM go_ride_requests r
WHERE r.id = a.ride_request_id
  AND a.ended_at IS NULL
  AND r.status = 'completed'
  AND COALESCE(r.completed_at, r.updated_at) <= NOW() - make_interval(secs => $1)`

// AbandonExpiredHeldEnds tombstones completed rides whose held `end` was never
// delivered within giveUpAfter of completion, and reports how many Activities
// it retired. Sends nothing — see the query header.
func (r *LiveActivityRepo) AbandonExpiredHeldEnds(ctx context.Context, giveUpAfter time.Duration) (int64, error) {
	if giveUpAfter <= 0 {
		return 0, fmt.Errorf("store.AbandonExpiredHeldEnds: non-positive horizon %s", giveUpAfter)
	}

	tag, err := r.pool.Exec(ctx, queryAbandonExpiredHeldEnds, giveUpAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store.AbandonExpiredHeldEnds(after=%s): %w", giveUpAfter, err)
	}
	return tag.RowsAffected(), nil
}

// ListRidesAwaitingActivityEnd returns up to limit rides that completed between
// heldFor and giveUpAfter ago and still have a live Activity registered against
// them — the set whose `end` push is due and still worth attempting. Newest
// completion first.
//
// An empty result is the ordinary state: most of the time no ride completed in
// the last five minutes. It is not an error.
func (r *LiveActivityRepo) ListRidesAwaitingActivityEnd(ctx context.Context, heldFor, giveUpAfter time.Duration, limit int) ([]string, error) {
	if heldFor < 0 {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: negative hold %s", heldFor)
	}
	if giveUpAfter <= heldFor {
		// An empty window would silently return nothing forever, which reads in
		// production as "the held end simply stopped working".
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: horizon %s does not exceed the hold %s",
			giveUpAfter, heldFor)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("store.ListRidesAwaitingActivityEnd: non-positive limit %d", limit)
	}

	rows, err := r.pool.Query(ctx, queryListRidesAwaitingActivityEnd,
		heldFor.Seconds(), giveUpAfter.Seconds(), limit)
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
