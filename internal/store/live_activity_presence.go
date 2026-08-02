package store

import (
	"context"
	"fmt"
	"strings"
)

// The "is this rider watching a card right now?" read (MYR-413).
//
// It lives in its own file rather than beside its eleven siblings in
// live_activity_repo.go because that file is already past this project's
// 300-line cap, and because this read serves a DIFFERENT caller from all of
// them: every other query here belongs to the Live Activity SEND path, while
// this one is asked by the ride-lifecycle ALERT notifier — the other push
// channel entirely — about whether it should stay quiet.

// queryLiveActivityExists reports whether one party has a RUNNING Live Activity
// on one ride.
//
// `ended_at IS NULL` is the whole predicate and the whole subtlety. A tombstoned
// row is a card the rider ended — swiped away, or ended by the app's own
// final-state fallback — and a tombstone must therefore read as ABSENT here, so
// that suppression hands the banner back the moment the card stops being able to
// deliver the news. The same is true of a row that was deleted outright after
// APNs permanently rejected its token: no row, no suppression, and the rider is
// never left dark because a surface they cannot see was assumed to be working.
//
// EXISTS rather than a count or a row fetch: the caller's question is a boolean,
// the (ride_request_id, user_id) unique index answers it with an index-only
// probe, and fetching the row would pull the P1 activity token across the wire
// to a caller that has no business holding one.
const queryLiveActivityExists = `
SELECT EXISTS (
    SELECT 1 FROM go_live_activities
    WHERE ride_request_id = $1 AND user_id = $2 AND ended_at IS NULL
)`

// HasLiveActivity reports whether userID has a live (non-tombstoned) Live
// Activity registered against rideRequestID.
//
// PER USER, NOT PER DEVICE, and the bound is real rather than theoretical. The
// table's natural key is (ride, party) because ActivityKit rotates an Activity's
// push token mid-ride, so one row is one PERSON'S card, not one phone's. A rider
// carrying two phones — the Live Activity running on one, the other merely
// signed in — presents a single row, and this read cannot tell the two apart.
// The caller suppresses the lifecycle banner on BOTH phones as a result. That is
// the deliberate trade: the alternative is a banner on every phone including the
// one already showing the card, which is precisely the duplicate MYR-413 is
// about. Documented in rest-api.md §7.21.6 rather than silently accepted.
//
// An unknown ride, an unknown user, or a ride nobody registered against all
// yield (false, nil) — "no card here" is the ordinary answer, not an error. Most
// rides are booked from a browser and never have an Activity at all.
func (r *LiveActivityRepo) HasLiveActivity(ctx context.Context, rideRequestID, userID string) (bool, error) {
	if strings.TrimSpace(rideRequestID) == "" || strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.HasLiveActivity: empty ride request id or user id")
	}

	var exists bool
	if err := r.pool.QueryRow(ctx, queryLiveActivityExists, rideRequestID, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store.HasLiveActivity(ride=%s, user=%s): %w", rideRequestID, userID, err)
	}
	return exists, nil
}
