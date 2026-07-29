package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// go_push_devices (migration 0019, MYR-186) is the APNs device-token registry:
// one row per installed app instance, keyed naturally by the device token. The
// ride-lifecycle senders in internal/push read it to resolve a ride party's
// cuid into the set of phones to alert.
//
// Device tokens are P1 (data-classification.md §1.14). This repository never
// logs one — the caller logs a short prefix via push.TokenPrefix when it must
// log anything at all — and never returns one in an error string.

// PushDevice is one registered installation.
type PushDevice struct {
	// UserID is the owning person's cuid.
	UserID string
	// DeviceToken is the raw APNs token. P1 — never log in full.
	DeviceToken string
	// Sandbox is true when the token was minted by a development or TestFlight
	// build and is therefore only valid against the APNs sandbox gateway.
	Sandbox bool
}

// queryUpsertPushDevice registers a token, or refreshes it if already known.
//
// The conflict target is device_token, NOT (user_id, device_token): a token
// identifies a physical installation, and when a second person signs in on the
// same phone the row must RE-PARENT to them. Keying the upsert on the pair
// would instead leave two rows claiming one device and keep pushing the
// previous occupant's rides to the new occupant's phone. sandbox is refreshed
// too, because a device can move between a TestFlight and an App Store build.
const queryUpsertPushDevice = `
INSERT INTO go_push_devices (id, user_id, device_token, platform, sandbox, created_at, last_seen_at)
VALUES ($1, $2, $3, 'ios', $4, NOW(), NOW())
ON CONFLICT (device_token) DO UPDATE
SET user_id      = EXCLUDED.user_id,
    sandbox      = EXCLUDED.sandbox,
    last_seen_at = NOW()`

// queryDeletePushDevice unregisters a token on sign-out. Scoped to the caller
// so one person can never unregister another's device, even though the token
// alone would identify the row.
const queryDeletePushDevice = `
DELETE FROM go_push_devices
WHERE user_id = $1 AND device_token = $2`

// queryDeletePushDeviceAnyOwner unregisters a token the APNs gateway has
// permanently rejected (410 Unregistered / 400 BadDeviceToken). Deliberately
// NOT caller-scoped: Apple's verdict is about the installation, not the person,
// and the row must go regardless of who currently owns it.
const queryDeletePushDeviceAnyOwner = `
DELETE FROM go_push_devices
WHERE device_token = $1`

// queryListPushDevicesByUser is the audience fan-out for one ride party.
const queryListPushDevicesByUser = `
SELECT user_id, device_token, sandbox
FROM go_push_devices
WHERE user_id = $1
ORDER BY last_seen_at DESC`

// PushDeviceRepo is the go_push_devices repository.
type PushDeviceRepo struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewPushDeviceRepo builds the registry over the given pool.
func NewPushDeviceRepo(pool *pgxpool.Pool, logger *slog.Logger) *PushDeviceRepo {
	if logger == nil {
		logger = slog.Default()
	}
	return &PushDeviceRepo{pool: pool, logger: logger}
}

// RegisterDevice upserts a device token for the caller, re-parenting the row if
// the token was previously registered to somebody else and stamping
// last_seen_at either way.
func (r *PushDeviceRepo) RegisterDevice(ctx context.Context, userID, deviceToken string, sandbox bool) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("store.RegisterDevice: empty user id")
	}
	if strings.TrimSpace(deviceToken) == "" {
		// The token is P1: report its absence, never its value.
		return fmt.Errorf("store.RegisterDevice(user=%s): empty device token", userID)
	}

	if _, err := r.pool.Exec(ctx, queryUpsertPushDevice,
		newProvisionID(), userID, deviceToken, sandbox,
	); err != nil {
		return fmt.Errorf("store.RegisterDevice(user=%s): %w", userID, err)
	}
	return nil
}

// UnregisterDevice removes the caller's registration for a token, reporting
// whether a row matched. A miss is not an error — sign-out is idempotent, and a
// token the caller does not own must look identical to one that does not exist
// so the endpoint cannot be used to probe other people's registrations.
func (r *PushDeviceRepo) UnregisterDevice(ctx context.Context, userID, deviceToken string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, fmt.Errorf("store.UnregisterDevice: empty user id")
	}
	if strings.TrimSpace(deviceToken) == "" {
		return false, fmt.Errorf("store.UnregisterDevice(user=%s): empty device token", userID)
	}

	tag, err := r.pool.Exec(ctx, queryDeletePushDevice, userID, deviceToken)
	if err != nil {
		return false, fmt.Errorf("store.UnregisterDevice(user=%s): %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteDeviceToken removes a token APNs has permanently rejected, whoever owns
// it. Called from the push sender's 410/400 handling, never from a request.
func (r *PushDeviceRepo) DeleteDeviceToken(ctx context.Context, deviceToken string) error {
	if strings.TrimSpace(deviceToken) == "" {
		return fmt.Errorf("store.DeleteDeviceToken: empty device token")
	}
	if _, err := r.pool.Exec(ctx, queryDeletePushDeviceAnyOwner, deviceToken); err != nil {
		return fmt.Errorf("store.DeleteDeviceToken: %w", err)
	}
	return nil
}

// DevicesForUser returns every device registered to a person, most recently
// seen first. An unknown person yields an empty slice, not an error: having no
// phone registered is the ordinary state, not a failure.
func (r *PushDeviceRepo) DevicesForUser(ctx context.Context, userID string) ([]PushDevice, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("store.DevicesForUser: empty user id")
	}

	rows, err := r.pool.Query(ctx, queryListPushDevicesByUser, userID)
	if err != nil {
		return nil, fmt.Errorf("store.DevicesForUser(user=%s): %w", userID, err)
	}
	defer rows.Close()

	var out []PushDevice
	for rows.Next() {
		var d PushDevice
		if err := rows.Scan(&d.UserID, &d.DeviceToken, &d.Sandbox); err != nil {
			return nil, fmt.Errorf("store.DevicesForUser(user=%s): scan: %w", userID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.DevicesForUser(user=%s): iterate: %w", userID, err)
	}
	return out, nil
}
