package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// go_live_activities (migration 0025, MYR-172) is the ActivityKit push-token
// registry: one row per (ride, party) whose Live Activity is running on a
// phone. The activity-update sender in internal/push reads it to resolve a ride
// into the set of Activities to push a new content-state to.
//
// This is the sibling of go_push_devices, not a copy of it. A device token
// addresses an INSTALLATION for as long as the app is installed; an activity
// token addresses ONE RUNNING ACTIVITY for the length of one ride, and
// ActivityKit rotates it underneath us while the Activity is alive. That is why
// the natural key here is (ride_request_id, user_id) and registration REPLACES
// the token in place — see the migration header for the full argument.
//
// Activity tokens are P1 (data-classification.md §1.18). This repository never
// logs one and never returns one in an error string; callers that must log
// anything log an 8-character prefix via push.TokenPrefix.

// LiveActivity is one running Live Activity that can be pushed to.
type LiveActivity struct {
	// RideRequestID is the ride whose Activity this is.
	RideRequestID string
	// UserID is the party running it. v1 only ever registers the rider.
	UserID string
	// ActivityPushToken is the raw ActivityKit update token. P1 — never log in
	// full, never echo into a response or an error.
	ActivityPushToken string
	// Sandbox is true when the token was minted by a development or TestFlight
	// build and is therefore only valid against the APNs sandbox gateway.
	Sandbox bool
}

// queryUpsertLiveActivity registers an Activity's update token, or replaces the
// token if one is already registered for this (ride, party).
//
// The conflict target is the (ride_request_id, user_id) pair, NOT the token.
// ActivityKit rotates the update token during the life of a single Activity, so
// the token is not a stable identity the way a device token is: keying on it
// would accumulate one row per rotation and leave the sender guessing which is
// live. Keying on the pair makes a rotation an ordinary re-registration.
//
// ended_at is reset to NULL on conflict. A client that re-registers is telling
// us it has a live Activity again; leaving a stale tombstone in place would
// silently exclude the row from every send path and the rider would watch a
// frozen lock screen with nothing in the logs to explain it.
const queryUpsertLiveActivity = `
INSERT INTO go_live_activities
    (id, ride_request_id, user_id, activity_push_token, sandbox, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
ON CONFLICT (ride_request_id, user_id) DO UPDATE
SET activity_push_token = EXCLUDED.activity_push_token,
    sandbox             = EXCLUDED.sandbox,
    updated_at          = NOW(),
    ended_at            = NULL`

// queryEndLiveActivity tombstones one party's Activity for one ride.
//
// Scoped to the caller so one person can never end another's Activity, and
// idempotent: ending an already-ended row reports no rows affected rather than
// failing, because a terminal-state send and the client's own end-of-Activity
// call race by design and both are correct.
const queryEndLiveActivity = `
UPDATE go_live_activities
SET ended_at = NOW(), updated_at = NOW()
WHERE ride_request_id = $1 AND user_id = $2 AND ended_at IS NULL`

// queryEndLiveActivitiesForRide tombstones every party's Activity for a ride.
// Used by the terminal-state sender after the final `event: "end"` push, where
// the ride is over for everyone regardless of who registered.
const queryEndLiveActivitiesForRide = `
UPDATE go_live_activities
SET ended_at = NOW(), updated_at = NOW()
WHERE ride_request_id = $1 AND ended_at IS NULL`

// queryListLiveActivitiesForRide is the fan-out for one ride's lifecycle push.
const queryListLiveActivitiesForRide = `
SELECT ride_request_id, user_id, activity_push_token, sandbox
FROM go_live_activities
WHERE ride_request_id = $1 AND ended_at IS NULL`

// queryDeleteLiveActivityToken removes a token APNs has permanently rejected
// (410 Unregistered / 400 BadDeviceToken). Deliberately NOT caller-scoped, for
// the same reason as the device-registry twin: Apple's verdict is about the
// Activity, not about the person who registered it.
const queryDeleteLiveActivityToken = `
DELETE FROM go_live_activities
WHERE activity_push_token = $1`

// querySweepLiveActivities reaps rows whose last write is older than the cutoff.
//
// The predicate is updated_at, NOT ended_at, and that is the point: the rows
// most worth reaping are the ones that NEVER ended — the Activity died on the
// phone, or the app was deleted mid-ride — and those keep ended_at IS NULL
// forever. An ended-only sweep would leak exactly the rows it exists to clean.
const querySweepLiveActivities = `
DELETE FROM go_live_activities
WHERE updated_at < NOW() - make_interval(secs => $1)`

// LiveActivityRepo is the go_live_activities repository.
type LiveActivityRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewLiveActivityRepo builds the registry over the given pool.
func NewLiveActivityRepo(pool *pgxpool.Pool, logger *slog.Logger) *LiveActivityRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &LiveActivityRepo{pool: pool, logger: logger}
}

// RegisterActivity upserts the update token for one party's Live Activity on
// one ride, replacing a rotated token in place and clearing any previous
// end-tombstone.
//
// The caller is responsible for having established that userID is a party to
// rideRequestID — this method enforces the shape of the write, not the
// authorization behind it.
func (r *LiveActivityRepo) RegisterActivity(ctx context.Context, rideRequestID, userID, token string, sandbox bool) error {
	if strings.TrimSpace(rideRequestID) == "" {
		return fmt.Errorf("store.RegisterActivity: empty ride request id")
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.RegisterActivity(ride=%s): empty user id", rideRequestID)
	}
	if strings.TrimSpace(token) == "" {
		// The token is P1: report its absence, never its value.
		return fmt.Errorf("store.RegisterActivity(ride=%s, user=%s): empty activity token", rideRequestID, userID)
	}

	if _, err := r.pool.Exec(ctx, queryUpsertLiveActivity,
		newProvisionID(), rideRequestID, userID, token, sandbox,
	); err != nil {
		return fmt.Errorf("store.RegisterActivity(ride=%s, user=%s): %w", rideRequestID, userID, err)
	}
	return nil
}

// EndActivity tombstones the caller's Activity for a ride, reporting whether a
// live row matched. A miss is not an error: ending is idempotent, and an
// Activity somebody else registered must look identical to one that was never
// registered so the endpoint cannot be used to probe other people's rows.
func (r *LiveActivityRepo) EndActivity(ctx context.Context, rideRequestID, userID string) (bool, error) {
	if strings.TrimSpace(rideRequestID) == "" {
		return false, fmt.Errorf("store.EndActivity: empty ride request id")
	}
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.EndActivity(ride=%s): empty user id", rideRequestID)
	}

	tag, err := r.pool.Exec(ctx, queryEndLiveActivity, rideRequestID, userID)
	if err != nil {
		return false, fmt.Errorf("store.EndActivity(ride=%s, user=%s): %w", rideRequestID, userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// EndActivitiesForRide tombstones every live Activity on a ride and reports how
// many it closed. Called after a terminal-state `event: "end"` push, where the
// ride is over for every party at once.
func (r *LiveActivityRepo) EndActivitiesForRide(ctx context.Context, rideRequestID string) (int64, error) {
	if strings.TrimSpace(rideRequestID) == "" {
		return 0, fmt.Errorf("store.EndActivitiesForRide: empty ride request id")
	}

	tag, err := r.pool.Exec(ctx, queryEndLiveActivitiesForRide, rideRequestID)
	if err != nil {
		return 0, fmt.Errorf("store.EndActivitiesForRide(ride=%s): %w", rideRequestID, err)
	}
	return tag.RowsAffected(), nil
}

// ActivitiesForRide returns every still-live Activity registered against a
// ride. An unknown ride yields an empty slice, not an error: a ride with no
// Activity running is the ordinary state — most rides are booked from a
// browser, and a rider who never opened the iOS app has nothing to update.
func (r *LiveActivityRepo) ActivitiesForRide(ctx context.Context, rideRequestID string) ([]LiveActivity, error) {
	if strings.TrimSpace(rideRequestID) == "" {
		return nil, fmt.Errorf("store.ActivitiesForRide: empty ride request id")
	}

	rows, err := r.pool.Query(ctx, queryListLiveActivitiesForRide, rideRequestID)
	if err != nil {
		return nil, fmt.Errorf("store.ActivitiesForRide(ride=%s): %w", rideRequestID, err)
	}
	defer rows.Close()

	var out []LiveActivity
	for rows.Next() {
		var a LiveActivity
		if err := rows.Scan(&a.RideRequestID, &a.UserID, &a.ActivityPushToken, &a.Sandbox); err != nil {
			return nil, fmt.Errorf("store.ActivitiesForRide(ride=%s): scan: %w", rideRequestID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActivitiesForRide(ride=%s): iterate: %w", rideRequestID, err)
	}
	return out, nil
}

// DeleteActivityToken removes a token APNs has permanently rejected, whoever
// registered it. Called from the activity sender's 410/400 handling, never from
// a request.
func (r *LiveActivityRepo) DeleteActivityToken(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("store.DeleteActivityToken: empty activity token")
	}
	if _, err := r.pool.Exec(ctx, queryDeleteLiveActivityToken, token); err != nil {
		return fmt.Errorf("store.DeleteActivityToken: %w", err)
	}
	return nil
}

// SweepStaleActivities deletes rows untouched for longer than olderThan and
// reports how many it removed. Idempotent and safe to run on any cadence.
func (r *LiveActivityRepo) SweepStaleActivities(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("store.SweepStaleActivities: non-positive age %s", olderThan)
	}

	tag, err := r.pool.Exec(ctx, querySweepLiveActivities, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store.SweepStaleActivities(age=%s): %w", olderThan, err)
	}
	return tag.RowsAffected(), nil
}
