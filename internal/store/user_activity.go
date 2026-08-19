// The WRITE half of owner last-seen tracking (MYR-592): one throttled upsert
// against the Go-owned go_user_activity table (migration 0043).
//
// ── WHAT THIS ANSWERS ────────────────────────────────────────────────────────
//
// "When did this account last authenticate?" — the only input the owner
// inactivity sweeper (rest-api.md §7.27) has, and a fact nothing in the schema
// carried before. Tesla bills Fleet Telemetry PER VEHICLE, so a car whose owner
// stopped opening the app costs money every day for frames nobody reads; the
// client's ruling is to warn on day four of owner silence and stop streaming on
// day five. Both arms are measured from this column.
//
// ── THE THROTTLE IS THE WHOLE DESIGN ─────────────────────────────────────────
//
// The caller is the bearer-token validator, which runs on EVERY authenticated
// REST request and every WebSocket handshake. An unthrottled stamp would be the
// busiest write in the database — and it would be spent maintaining
// sub-second precision on a value compared against a FOUR-DAY threshold by an
// HOURLY worker. So the statement carries its own guard: a row moves at most
// once per `throttle`, and a request arriving inside that window matches zero
// rows and writes nothing.
//
// The guard is in SQL rather than in a read-then-write pair, and that is not
// only about round trips. Two concurrent requests from the same account —
// ordinary for a client holding a WebSocket while issuing REST calls — would
// both read a stale timestamp and both decide to write. `ON CONFLICT … DO
// UPDATE … WHERE` collapses that to one statement whose outcome cannot depend
// on the interleaving: the second writer's predicate is evaluated against the
// first writer's committed row.
//
// A SECOND, in-process gate sits in front of this (internal/auth/user_activity.go)
// so the statement is usually not issued at all. This one is the durable
// backstop: it survives a restart, and it is what keeps N server replicas from
// each contributing their own hourly write.
//
// ── THE LAG IS DELIBERATE AND IT IS SAFE IN ONE DIRECTION ────────────────────
//
// A stored value may be up to one throttle window behind reality. Against a
// threshold measured in days that is immaterial, and the error is always
// CONSERVATIVE: a stale row makes a present owner look slightly less recent than
// they are. The sweeper's response to "less recent" is to warn somebody who is
// in fact around — a notification, recoverable by opening the app — never to
// suspend somebody who is. The window can therefore be widened for cost without
// re-arguing correctness; it cannot be narrowed into a hot write path by
// accident, because narrowing is what this file exists to prevent.
//
// ── SCOPE ────────────────────────────────────────────────────────────────────
//
// EVERY authenticated account stamps, owner or rider. The writer deliberately
// does not ask whether the caller owns a car: the question costs a join on the
// hot path and its answer changes over an account's life. Only OWNERS' rows are
// ever read, by the sweeper's candidate query, which reaches this table through
// a vehicle.
//
// P1 (data-classification.md §1.21) — a behavioural observation about a person,
// a step up from the P0 its opaque-cuid-and-timestamp neighbours carry. Never
// logged, never on any wire, never in a URL. The only thing a consumer ever sees
// is the consequence: VehicleSummary.telemetrySuspendedAt.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryTouchUserActivity stamps last_seen_at unless the stored value is already
// newer than the caller's cutoff.
//
// $1 = user id (JWT `sub`), $2 = now, $3 = the throttle cutoff (now - window).
//
// THE INSERT ARM IS UNCONDITIONAL, and that is correct rather than an oversight
// in the guard: a person with no row has never been observed, so there is
// nothing to throttle against and the first sighting must land. Only the UPDATE
// arm is rationed.
//
// THE PREDICATE COMPARES THE STORED VALUE, not EXCLUDED — `go_user_activity.
// last_seen_at < $3` reads the row already committed. Writing it against
// EXCLUDED would compare the new value with the cutoff derived from it and be
// unconditionally false, i.e. a row that never moved again.
//
// NO `updated_at`, NO RETURNING beyond the tag. The caller wants one bit — did
// this write land — which pgx already reports as the command tag's row count.
const queryTouchUserActivity = `
INSERT INTO go_user_activity (user_id, last_seen_at)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE
   SET last_seen_at = EXCLUDED.last_seen_at
 WHERE go_user_activity.last_seen_at < $3`

// queryUserLastSeen reads one account's last-seen instant. Used by the reconnect
// endpoint's tests and by operator tooling; the sweeper does NOT use it (it
// joins, so that it can never examine an account that owns no car).
const queryUserLastSeen = `SELECT last_seen_at FROM go_user_activity WHERE user_id = $1`

// queryDeleteUserActivity removes one account's row. Step 8c of the
// rest-api.md §7.6 account-deletion sequence.
const queryDeleteUserActivity = `DELETE FROM go_user_activity WHERE user_id = $1`

// UserActivityRepo owns go_user_activity.
//
// It holds no encryptor and no logger: there is nothing here to decrypt, and the
// one caller (the auth-side stamper) owns the decision to swallow-and-log a
// failure, because only it knows that a last-seen write must never fail an
// authentication.
type UserActivityRepo struct {
	pool *pgxpool.Pool
}

// NewUserActivityRepo wires the repo to a pool.
func NewUserActivityRepo(pool *pgxpool.Pool) *UserActivityRepo {
	return &UserActivityRepo{pool: pool}
}

// Touch stamps last_seen_at for userID at `now`, unless the stored value is
// already within `throttle` of it. Returns true when a row was actually written.
//
// The boolean is for tests and for the caller's in-process gate — nothing in
// production branches on it, and nothing should: "we skipped the write" and "we
// made the write" are the same outcome from the sweeper's point of view.
//
// A non-positive throttle means "always write", which is what the tests use to
// assert the statement itself. Production always passes a real window.
func (r *UserActivityRepo) Touch(ctx context.Context, userID string, now time.Time, throttle time.Duration) (bool, error) {
	if userID == "" {
		return false, nil
	}
	cutoff := now.Add(-throttle)
	tag, err := r.pool.Exec(ctx, queryTouchUserActivity, userID, now, cutoff)
	if err != nil {
		return false, fmt.Errorf("store.UserActivityRepo.Touch(%s): %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// LastSeen returns the stored instant for userID. The second return is false
// when no row exists — an account we have never observed, which is materially
// different from one we have observed to be idle and which the sweeper must
// never confuse with it.
func (r *UserActivityRepo) LastSeen(ctx context.Context, userID string) (time.Time, bool, error) {
	var seen time.Time
	err := r.pool.QueryRow(ctx, queryUserLastSeen, userID).Scan(&seen)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("store.UserActivityRepo.LastSeen(%s): %w", userID, err)
	}
	return seen, true, nil
}

// DeleteForUser removes the account's row. Idempotent; a missing row is not an
// error, because §7.6 step 8c may run for an account that never authenticated
// after this table shipped.
func (r *UserActivityRepo) DeleteForUser(ctx context.Context, userID string) error {
	if _, err := r.pool.Exec(ctx, queryDeleteUserActivity, userID); err != nil {
		return fmt.Errorf("store.UserActivityRepo.DeleteForUser(%s): %w", userID, err)
	}
	return nil
}
