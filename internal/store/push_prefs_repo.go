package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// go_push_prefs (migration 0022, MYR-349) holds one row per person: which
// categories of notification they want their phones woken for. The §7.19
// endpoints read and write it; the internal/push notifier reads it before every
// fan-out.
//
// All columns are P0 (data-classification.md §1.16) — an opaque cuid and five
// booleans about delivery. Nothing here is identifying and nothing is a
// capability, so unlike the sibling device registry there is no redaction rule
// to keep: these values may be logged in full.

// PushPrefs is one person's answers, in the order the wire renders them.
//
// Every field is a plain bool rather than a pointer BECAUSE THE ABSENCE OF A
// ROW IS NOT AN UNKNOWN. A person who has never opened Settings is receiving
// everything, so the zero value of this struct would be the exact opposite of
// the truth — always construct it via DefaultPushPrefs or a read, never with
// `PushPrefs{}`.
type PushPrefs struct {
	RideLifecycle    bool
	DriveStarted     bool
	DriveCompleted   bool
	ChargingComplete bool
	ViewerJoined     bool
}

// DefaultPushPrefs is what a person with no stored row gets: everything on.
// This is the pre-MYR-349 behaviour, expressed once so the missing-row read,
// the omitted-key upsert and the notifier's failure path cannot disagree.
func DefaultPushPrefs() PushPrefs {
	return PushPrefs{
		RideLifecycle:    true,
		DriveStarted:     true,
		DriveCompleted:   true,
		ChargingComplete: true,
		ViewerJoined:     true,
	}
}

// PushPrefsUpdate is a PARTIAL write: a nil field means "leave this category
// alone", never "set it to false". §7.19's PUT carries only the switches the
// client actually moved, so two phones changing two different categories cannot
// clobber each other — which for this surface is the difference between a stale
// toggle and a notification somebody explicitly silenced arriving anyway.
type PushPrefsUpdate struct {
	RideLifecycle    *bool
	DriveStarted     *bool
	DriveCompleted   *bool
	ChargingComplete *bool
	ViewerJoined     *bool
}

// queryGetPushPrefs is the point read behind both the endpoint's GET and the
// notifier's per-send gate.
const queryGetPushPrefs = `
SELECT ride_lifecycle, drive_started, drive_completed, charging_complete, viewer_joined
FROM go_push_prefs
WHERE user_id = $1`

// queryUpsertPushPrefs applies a partial update and returns the WHOLE row as it
// now stands — the echo §7.19 answers with, which is why it is RETURNING rather
// than a write followed by a second read (a concurrent write between the two
// would make the client adopt a value that was never stored).
//
// The two halves treat an omitted key differently, and both are correct:
//
//   - INSERT (no row yet) → COALESCE($n, TRUE), i.e. an omitted category takes
//     the same all-on default the missing row itself would have produced;
//   - UPDATE (row exists) → COALESCE($n, go_push_prefs.col), i.e. an omitted
//     category keeps whatever the person last chose.
//
// Note the UPDATE reads the parameter directly rather than EXCLUDED: the
// INSERT's VALUES have already COALESCEd nil to TRUE, so EXCLUDED can never be
// NULL and using it would turn every omitted key into "switch this back on".
const queryUpsertPushPrefs = `
INSERT INTO go_push_prefs (
    user_id, ride_lifecycle, drive_started, drive_completed, charging_complete, viewer_joined,
    created_at, updated_at
)
VALUES (
    $1,
    COALESCE($2::boolean, TRUE),
    COALESCE($3::boolean, TRUE),
    COALESCE($4::boolean, TRUE),
    COALESCE($5::boolean, TRUE),
    COALESCE($6::boolean, TRUE),
    NOW(), NOW()
)
ON CONFLICT (user_id) DO UPDATE
SET ride_lifecycle    = COALESCE($2::boolean, go_push_prefs.ride_lifecycle),
    drive_started     = COALESCE($3::boolean, go_push_prefs.drive_started),
    drive_completed   = COALESCE($4::boolean, go_push_prefs.drive_completed),
    charging_complete = COALESCE($5::boolean, go_push_prefs.charging_complete),
    viewer_joined     = COALESCE($6::boolean, go_push_prefs.viewer_joined),
    updated_at        = NOW()
RETURNING ride_lifecycle, drive_started, drive_completed, charging_complete, viewer_joined`

// PushPrefsRepo is the go_push_prefs repository.
type PushPrefsRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPushPrefsRepo builds the repository over the given pool.
func NewPushPrefsRepo(pool *pgxpool.Pool, logger *slog.Logger) *PushPrefsRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &PushPrefsRepo{pool: pool, logger: logger}
}

// PrefsForUser returns a person's stored preferences.
//
// A MISSING ROW IS NOT AN ERROR and not an empty struct — it is
// DefaultPushPrefs, everything on. That is the state every account is in until
// somebody moves a switch, so the no-rows branch is the COMMON path, not an
// edge case, and returning the zero value here would silence every notification
// on the platform.
func (r *PushPrefsRepo) PrefsForUser(ctx context.Context, userID string) (PushPrefs, error) {
	if strings.TrimSpace(userID) == "" {
		return PushPrefs{}, fmt.Errorf("store.PrefsForUser: empty user id")
	}

	prefs := DefaultPushPrefs()
	err := r.pool.QueryRow(ctx, queryGetPushPrefs, userID).Scan(
		&prefs.RideLifecycle,
		&prefs.DriveStarted,
		&prefs.DriveCompleted,
		&prefs.ChargingComplete,
		&prefs.ViewerJoined,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPushPrefs(), nil
	}
	if err != nil {
		return PushPrefs{}, fmt.Errorf("store.PrefsForUser(user=%s): %w", userID, err)
	}
	return prefs, nil
}

// UpdatePrefs applies a partial update and returns the whole row as stored.
// Creating the row on first write is part of the same statement, so a person
// who has never had a row and switches one category off ends up with that
// category off and the other four on.
func (r *PushPrefsRepo) UpdatePrefs(ctx context.Context, userID string, update PushPrefsUpdate) (PushPrefs, error) {
	if strings.TrimSpace(userID) == "" {
		return PushPrefs{}, fmt.Errorf("store.UpdatePrefs: empty user id")
	}

	var prefs PushPrefs
	err := r.pool.QueryRow(ctx, queryUpsertPushPrefs,
		userID,
		update.RideLifecycle,
		update.DriveStarted,
		update.DriveCompleted,
		update.ChargingComplete,
		update.ViewerJoined,
	).Scan(
		&prefs.RideLifecycle,
		&prefs.DriveStarted,
		&prefs.DriveCompleted,
		&prefs.ChargingComplete,
		&prefs.ViewerJoined,
	)
	if err != nil {
		return PushPrefs{}, fmt.Errorf("store.UpdatePrefs(user=%s): %w", userID, err)
	}
	return prefs, nil
}
