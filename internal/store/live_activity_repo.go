package store

import (
	"context"
	"errors"
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
	// Progress is this Activity's leg-progress anchor (MYR-398, migration
	// 0027). Zero value means no anchor yet.
	Progress LiveActivityProgress
	// AlertedPhase is the highest island-expand phase this Activity has already
	// been alerted at (MYR-398, migration 0029). 0 means none yet, which is
	// what a row registered before the ride's first transition carries.
	AlertedPhase int16
}

// LiveActivityKey is the natural key of one registration — the pair the table
// is UNIQUE on. Carried as a type rather than two loose strings because
// MarkActivitiesPushed takes a slice of them and a []string pair would be two
// slices the caller could silently misalign.
type LiveActivityKey struct {
	RideRequestID string
	UserID        string
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
//
// THE WRITE IS GUARDED IN SQL, and that is the point of the INSERT … SELECT
// shape (which otherwise buys nothing over a VALUES list).
//
// Clearing the tombstone is the right behaviour for the case it was written
// for — an ordinary token rotation on a ride that is still happening — and
// exactly the wrong one for a ride whose Activity was ended for good. There are
// two such endings and they fail differently:
//
//   - A TERMINAL RIDE STATUS. The handler checks this before calling, but the
//     check is a read and this is the write, so a POST that arrives while the
//     ride is transitioning would re-register after the terminal `event: "end"`
//     had already tombstoned the row. Narrowing that window is not a fix;
//     making the guard part of the write is.
//   - A RESERVATION THAT EXPIRED. The sweeper ends and tombstones the
//     Activities but leaves the ride at `accepted` (it records the give-up in
//     the dispatch columns instead), and dispatched_at is latched, so nothing
//     can ever expire the ride a second time. Without this predicate a single
//     token rotation after the expiry would resurrect the row and the ETA
//     ticker would resume a countdown to a pickup that is never coming.
//
// Both predicates are COALESCEd because NULL is the ordinary state of
// dispatch_status: `NOT (NULL = 'failed' AND …)` is NULL, not TRUE, which would
// silently reject every registration on a ride that has not dispatched yet.
//
// A guard miss affects zero rows, which RegisterActivity reports as
// ErrLiveActivityRideClosed. Note this deliberately does NOT refuse every
// dispatch failure: a nav push that failed for any other reason leaves a ride
// that is still genuinely happening (the owner can drive it manually), and its
// Activity must keep rotating tokens.
//
// A FRESH REGISTRATION IS SEEDED AT THE RIDE'S CURRENT PHASE, not at zero
// (MYR-398, migration 0029). The app starts the Activity locally and draws the
// current state itself, so the island has just been in front of the rider —
// expanding it to announce the phase they are already looking at would be an
// unearned interruption at the very start of every ride. Seeding says "that one
// has happened"; the first genuine transition after it is what earns the first
// expansion.
//
// Seeded from the STATUS ALONE, so it cannot express Arriving (which needs the
// car's ETA). That is the right way round: a rider who registers while the car
// is already ninety seconds out has genuinely not seen an Arriving expansion,
// and gets it on the next tick.
//
// The CONFLICT branch deliberately leaves the mark alone, exactly as it leaves
// the progress anchor alone and for the same reason: a token rotation is the
// same Activity mid-ride, and re-seeding it would be harmless while re-zeroing
// it would run the whole ladder again.
const queryUpsertLiveActivity = `
INSERT INTO go_live_activities
    (id, ride_request_id, user_id, activity_push_token, sandbox, alerted_phase, created_at, updated_at)
SELECT $1, r.id, $3, $4, $5,
       CASE r.status
           WHEN 'requested' THEN 1
           WHEN 'accepted'  THEN 2
           WHEN 'arrived'   THEN 4
           WHEN 'enroute'   THEN 5
           ELSE 0
       END,
       NOW(), NOW()
FROM go_ride_requests r
WHERE r.id = $2
  AND r.status NOT IN ('completed', 'declined', 'cancelled')
  AND NOT (COALESCE(r.dispatch_status, '') = 'failed'
           AND COALESCE(r.dispatch_error, '') = 'reservation_expired')
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
//
// Aliased `a` only so it can share progressColumns with the ticker's read
// (live_activity_progress.go): one spelling of the anchor projection, so the
// two send paths cannot drift into disagreeing about what a track means.
const queryListLiveActivitiesForRide = `
SELECT a.ride_request_id, a.user_id, a.activity_push_token, a.sandbox,
       ` + progressColumns + `
FROM go_live_activities a
WHERE a.ride_request_id = $1 AND a.ended_at IS NULL`

// queryDeleteRejectedActivity removes a token APNs has permanently rejected
// (410 Unregistered / 400 BadDeviceToken). Deliberately NOT caller-scoped, for
// the same reason as the device-registry twin: Apple's verdict is about the
// Activity, not about the person who registered it.
const queryDeleteRejectedActivity = `
DELETE FROM go_live_activities
WHERE activity_push_token = $1`

// querySweepLiveActivities reaps rows whose last TOUCH is older than the cutoff.
//
// The predicate is updated_at, NOT ended_at, and that is the point: the rows
// most worth reaping are the ones that NEVER ended — the Activity died on the
// phone, or the app was deleted mid-ride — and those keep ended_at IS NULL
// forever. An ended-only sweep would leak exactly the rows it exists to clean.
//
// updated_at means "last registration, end, OR successful push" (see
// queryMarkLiveActivitiesPushed), which is what makes 24 hours the right
// horizon: a row still being pushed to is by definition not stale, so a ride
// that outlives the horizon is never swept out from under a live Activity.
const querySweepLiveActivities = `
DELETE FROM go_live_activities
WHERE updated_at < NOW() - make_interval(secs => $1)`

// queryMarkLiveActivitiesPushed stamps updated_at on the rows a ticker pass
// actually delivered to.
//
// This is what makes the ticker's `ORDER BY updated_at ASC LIMIT n` an
// anti-starvation order rather than a decoration. Without it updated_at is
// frozen at registration time, the ordering is a fixed permutation, and a pass
// truncated by MaxPerPass sheds the SAME tail on every tick forever — the rows
// most in need of a refresh are precisely the ones that never get one.
//
// One statement per pass rather than one per send: a pass can deliver
// MaxPerPass (200) updates every 60–90s, and 200 single-row UPDATEs to move one
// timestamp is write amplification bought for nothing.
//
// Scoped to live rows so a send that raced the ride's terminal end cannot
// un-stale a tombstoned row and hold it back from the sweep.
//
// The pairs arrive as two parallel text arrays rather than one array of
// composites: pgx encodes text[] natively, whereas a record[] needs a
// registered composite type, and `unnest` of two arrays is the standard
// Postgres spelling for "these tuples".
const queryMarkLiveActivitiesPushed = `
UPDATE go_live_activities
SET updated_at = NOW()
WHERE ended_at IS NULL
  AND (ride_request_id, user_id) IN (
      SELECT * FROM unnest($1::text[], $2::text[])
  )`

// queryLiveActivityRideIDsForVehicle lists the rides of one vehicle that still
// have a live Activity registered against them.
//
// Used by owner teardown (MYR-258): the teardown hard-DELETEs go_ride_requests
// by vehicle and the FK cascade takes go_live_activities with it, silently. The
// rows go away; the Live Activities on the riders' phones do not. This read
// gives the teardown the set it must end-push BEFORE the delete.
const queryLiveActivityRideIDsForVehicle = `
SELECT DISTINCT a.ride_request_id
FROM go_live_activities a
JOIN go_ride_requests r ON r.id = a.ride_request_id
WHERE r.vehicle_id = $1 AND a.ended_at IS NULL`

// ErrLiveActivityRideClosed is returned by RegisterActivity when the SQL guard
// refuses the write: the ride has reached a terminal status, or its reservation
// expired, and its Live Activity has been ended for good.
//
// Deliberately does NOT wrap sdk.ErrNotFound. The ride exists — the caller was
// proven to be its rider before we got here — it is simply past the point where
// a registration means anything. The HTTP layer maps it to 409 (rest-api.md
// §7.21.1), which is an instruction to the client to end its Activity locally.
var ErrLiveActivityRideClosed = errors.New("live activity: ride is closed to registration")

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
// Returns ErrLiveActivityRideClosed when the ride is past registration — a
// terminal status, or a reservation that expired. That decision is made by the
// statement itself rather than by a read before it: the handler's own check is
// a read-then-write and a POST can arrive in the middle of the terminal
// transition, so the guard that actually holds is the one inside the write.
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

	tag, err := r.pool.Exec(ctx, queryUpsertLiveActivity,
		newProvisionID(), rideRequestID, userID, token, sandbox,
	)
	if err != nil {
		return fmt.Errorf("store.RegisterActivity(ride=%s, user=%s): %w", rideRequestID, userID, err)
	}
	if tag.RowsAffected() == 0 {
		// The guard refused, or the ride vanished under us (owner teardown
		// hard-deletes rides). Both mean the same thing to the caller: there is
		// no Activity to keep alive here.
		return fmt.Errorf("store.RegisterActivity(ride=%s, user=%s): %w",
			rideRequestID, userID, ErrLiveActivityRideClosed)
	}
	return nil
}

// MarkActivitiesPushed stamps updated_at on the (ride, party) rows a ticker
// pass delivered to, and reports how many it moved.
//
// Called after the sends rather than instead of them: an Activity Apple
// refused is not "recently pushed", and stamping it would let a permanently
// failing row hold the front of the queue while healthy ones starve.
func (r *LiveActivityRepo) MarkActivitiesPushed(ctx context.Context, keys []LiveActivityKey) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}

	rideIDs := make([]string, 0, len(keys))
	userIDs := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(k.RideRequestID) == "" || strings.TrimSpace(k.UserID) == "" {
			return 0, fmt.Errorf("store.MarkActivitiesPushed: empty ride request id or user id")
		}
		rideIDs = append(rideIDs, k.RideRequestID)
		userIDs = append(userIDs, k.UserID)
	}

	tag, err := r.pool.Exec(ctx, queryMarkLiveActivitiesPushed, rideIDs, userIDs)
	if err != nil {
		return 0, fmt.Errorf("store.MarkActivitiesPushed(n=%d): %w", len(keys), err)
	}
	return tag.RowsAffected(), nil
}

// ActivityRideIDsForVehicle lists the rides of one vehicle that still have a
// live Activity, so a caller about to hard-delete those rides can end their
// Activities first. An empty result is the ordinary case.
func (r *LiveActivityRepo) ActivityRideIDsForVehicle(ctx context.Context, vehicleID string) ([]string, error) {
	if strings.TrimSpace(vehicleID) == "" {
		return nil, fmt.Errorf("store.ActivityRideIDsForVehicle: empty vehicle id")
	}

	rows, err := r.pool.Query(ctx, queryLiveActivityRideIDsForVehicle, vehicleID)
	if err != nil {
		return nil, fmt.Errorf("store.ActivityRideIDsForVehicle(vehicle=%s): %w", vehicleID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ActivityRideIDsForVehicle(vehicle=%s): scan: %w", vehicleID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActivityRideIDsForVehicle(vehicle=%s): iterate: %w", vehicleID, err)
	}
	return out, nil
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
		var leg, source *string
		var baseline, value, reading *float64
		var readingAt *time.Time
		if err := rows.Scan(&a.RideRequestID, &a.UserID, &a.ActivityPushToken, &a.Sandbox,
			&leg, &source, &baseline, &value, &reading, &readingAt, &a.AlertedPhase); err != nil {
			return nil, fmt.Errorf("store.ActivitiesForRide(ride=%s): scan: %w", rideRequestID, err)
		}
		a.Progress = scanProgress(leg, source, baseline, value, reading, readingAt)
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
	if _, err := r.pool.Exec(ctx, queryDeleteRejectedActivity, token); err != nil {
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
