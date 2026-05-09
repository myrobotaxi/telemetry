// Package store — DriveRepo lives behind a dual-write contract during
// the MYR-64 cross-repo route-blob encryption rollout (NFR-3.23).
//
// Read path: GetByID prefers the encrypted shadow `routePointsEnc`
// when non-NULL and falls back to the plaintext `routePoints` JSONB
// column on decrypt/unmarshal failure. The plaintext column is
// non-nullable on the Prisma schema (defaults to `[]`), so the
// fallback always has a value to surface.
//
// Write path: Create dual-writes the seed array (typically `[]`).
// AppendRoutePoints does a plaintext-first concat in PostgreSQL via
// `jsonb_concat (||)` and uses RETURNING to read the post-append array
// back; the helper then re-encrypts that full shape into the shadow
// in a follow-up UPDATE. The two UPDATEs are not transactional — the
// plaintext write is the source of truth and a shadow re-encrypt
// failure logs at Warn rather than rolling back the plaintext (Tesla
// telemetry MUST NOT be lost over an encryption hiccup).
//
// The Encryptor is opt-in via NewDriveRepoWithEncryption. The legacy
// NewDriveRepo constructor stays plaintext-only for tests and
// migration windows.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/routeblob"
)

// DriveRepo manages drive records in the Prisma-owned "Drive" table.
type DriveRepo struct {
	pool      *pgxpool.Pool
	metrics   Metrics
	encryptor cryptox.Encryptor // nil disables the dual-write
	logger    *slog.Logger      // optional; warnings go here when non-nil
}

// NewDriveRepo creates a DriveRepo without column-level encryption.
// Retained for the migration window so existing call sites that don't
// yet have an Encryptor in scope keep compiling.
func NewDriveRepo(pool *pgxpool.Pool, metrics Metrics) *DriveRepo {
	return &DriveRepo{pool: pool, metrics: metrics}
}

// NewDriveRepoWithEncryption is the dual-write constructor: the
// Encryptor is required and used on every write to compute the
// `routePointsEnc` shadow, and on every read to prefer the shadow over
// the plaintext column.
//
// Panics on a nil Encryptor — mirrors NewVehicleRepoWithEncryption so
// the dual-write contract fails loud at construction.
func NewDriveRepoWithEncryption(pool *pgxpool.Pool, metrics Metrics, encryptor cryptox.Encryptor, logger *slog.Logger) *DriveRepo {
	if encryptor == nil {
		panic("store.NewDriveRepoWithEncryption: encryptor must not be nil")
	}
	return &DriveRepo{pool: pool, metrics: metrics, encryptor: encryptor, logger: logger}
}

// Create inserts a new drive record when a drive starts. The drive is
// created with placeholder end-time fields that will be filled in when
// the drive completes.
//
// MYR-64: when an Encryptor is wired, the seed routePoints array is
// also encrypted into routePointsEnc. The seed is typically `[]`, in
// which case routeblob.EncryptJSONBytes returns the empty sentinel and
// the shadow is left NULL — the read path will fall back to the
// plaintext `[]` until the first AppendRoutePoints call writes a real
// shadow.
func (r *DriveRepo) Create(ctx context.Context, drive DriveRecord) error {
	routePoints := drive.RoutePoints
	if routePoints == nil {
		routePoints = json.RawMessage("[]")
	}

	encShadow := r.encryptRoutePointsRaw(routePoints)

	start := time.Now()
	_, err := r.pool.Exec(ctx, queryDriveInsert,
		drive.ID, drive.VehicleID, drive.Date, drive.StartTime, drive.EndTime,
		drive.StartLocation, drive.StartAddress, drive.EndLocation, drive.EndAddress,
		drive.DistanceMiles, drive.DurationMinutes, drive.AvgSpeedMph, drive.MaxSpeedMph,
		drive.EnergyUsedKwh, drive.StartChargeLevel, drive.EndChargeLevel,
		drive.FsdMiles, drive.FsdPercentage, drive.Interventions, routePoints,
		encShadow, // nil-string when encryptor is absent or shadow is empty
	)
	r.metrics.ObserveQueryDuration("drive.create", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.create")
		return fmt.Errorf("DriveRepo.Create(%s): %w", drive.ID, err)
	}
	return nil
}

// AppendRoutePoints appends route points to the drive's routePoints
// JSON array. Uses PostgreSQL jsonb_concat (||) to avoid
// read-modify-write of the (potentially large) array.
//
// MYR-64 dual-write: the UPDATE returns the post-append array so we
// can re-encrypt the full shape into the shadow in a second statement.
// The shadow re-encrypt is fail-open — telemetry MUST NOT be lost over
// an encryption hiccup.
func (r *DriveRepo) AppendRoutePoints(ctx context.Context, driveID string, points []RoutePointRecord) error {
	if len(points) == 0 {
		return nil
	}

	pointsJSON, err := json.Marshal(points)
	if err != nil {
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): marshal: %w", driveID, err)
	}

	// Pass as json.RawMessage so pgx encodes it as JSON (not bytea).
	// Plain []byte from json.Marshal is sent as bytea by pgx, which fails
	// the ::jsonb cast with "invalid input syntax for type json".
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryDriveAppendRoutePoints, driveID, json.RawMessage(pointsJSON))
	var fullArr json.RawMessage
	scanErr := row.Scan(&fullArr)
	r.metrics.ObserveQueryDuration("drive.append_route_points", time.Since(start).Seconds())
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, ErrDriveNotFound)
	}
	if scanErr != nil {
		r.metrics.IncQueryError("drive.append_route_points")
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, scanErr)
	}

	r.refreshRoutePointsShadow(ctx, driveID, fullArr)
	return nil
}

// refreshRoutePointsShadow re-encrypts the post-append routePoints
// array into the shadow column. Fail-open: encrypt or UPDATE failures
// are logged at Warn but never returned. The plaintext column is the
// source of truth and a shadow lag is the rollout's expected steady
// state until the backfill catches up.
func (r *DriveRepo) refreshRoutePointsShadow(ctx context.Context, driveID string, fullArr json.RawMessage) {
	if r.encryptor == nil {
		return
	}
	ct, err := routeblob.EncryptJSONBytes(fullArr, r.encryptor)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("Drive routePointsEnc encrypt failed; plaintext-only append committed",
				slog.String("drive_id", driveID),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	if ct == "" {
		// Empty array — leave the shadow NULL. Read fallback applies.
		return
	}
	if _, uErr := r.pool.Exec(ctx, queryDriveSetRoutePointsEnc, driveID, ct); uErr != nil {
		if r.logger != nil {
			r.logger.Warn("Drive routePointsEnc shadow UPDATE failed; plaintext-only append committed",
				slog.String("drive_id", driveID),
				slog.String("error", uErr.Error()),
			)
		}
	}
}

// encryptRoutePointsRaw is the Create-path companion: encrypts the
// seed routePoints array into the shadow value and returns the *string
// pgx wants for the parameter slot. nil result writes NULL into the
// column. Encrypt failures are logged at Warn (when a logger is wired)
// and converted to a NULL shadow so the plaintext write still goes
// through — telemetry must never be lost over an encryption hiccup.
func (r *DriveRepo) encryptRoutePointsRaw(raw json.RawMessage) *string {
	if r.encryptor == nil {
		return nil
	}
	ct, err := routeblob.EncryptJSONBytes(raw, r.encryptor)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("Drive routePointsEnc seed encrypt failed; writing plaintext only",
				slog.String("error", err.Error()),
			)
		}
		return nil
	}
	if ct == "" {
		return nil
	}
	return &ct
}

// Complete updates a drive with its final stats when the drive ends.
func (r *DriveRepo) Complete(ctx context.Context, driveID string, stats DriveCompletion) error {
	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryDriveComplete,
		driveID, stats.EndTime, stats.EndLocation, stats.EndAddress,
		stats.DistanceMiles, stats.DurationMinutes,
		stats.AvgSpeedMph, stats.MaxSpeedMph, stats.EnergyUsedKwh,
		stats.EndChargeLevel, stats.FsdMiles, stats.FsdPercentage,
		stats.Interventions,
	)
	r.metrics.ObserveQueryDuration("drive.complete", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.complete")
		return fmt.Errorf("DriveRepo.Complete(%s): %w", driveID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DriveRepo.Complete(%s): %w", driveID, ErrDriveNotFound)
	}
	return nil
}

// GetByID returns a single drive by its ID.
// Returns ErrDriveNotFound if no drive has that ID.
//
// MYR-64: prefers the routePointsEnc shadow when the encryptor is wired
// and the column is non-NULL. Decrypt or shape-validation failures fall
// back to the plaintext column with a Warn log.
func (r *DriveRepo) GetByID(ctx context.Context, id string) (DriveRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryDriveByID, id)

	var d DriveRecord
	var routePointsEnc *string
	err := row.Scan(
		&d.ID, &d.VehicleID, &d.Date, &d.StartTime, &d.EndTime,
		&d.StartLocation, &d.StartAddress, &d.EndLocation, &d.EndAddress,
		&d.DistanceMiles, &d.DurationMinutes, &d.AvgSpeedMph, &d.MaxSpeedMph,
		&d.EnergyUsedKwh, &d.StartChargeLevel, &d.EndChargeLevel,
		&d.FsdMiles, &d.FsdPercentage, &d.Interventions, &d.RoutePoints, &d.CreatedAt,
		&routePointsEnc,
	)
	r.metrics.ObserveQueryDuration("drive.get_by_id", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError("drive.get_by_id")
		return DriveRecord{}, fmt.Errorf("DriveRepo.GetByID(%s): %w", id, ErrDriveNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("drive.get_by_id")
		return DriveRecord{}, fmt.Errorf("DriveRepo.GetByID(%s): %w", id, err)
	}
	r.applyResolvedRoutePoints(&d, routePointsEnc)
	return d, nil
}

// applyResolvedRoutePoints prefers the encrypted shadow and falls back
// to the plaintext column on decrypt/unmarshal failure. The plaintext
// column survives untouched on the legacy (no-encryptor) path.
func (r *DriveRepo) applyResolvedRoutePoints(d *DriveRecord, ct *string) {
	if r.encryptor == nil || ct == nil || *ct == "" {
		return
	}
	raw, err := routeblob.DecryptJSONBytes(*ct, r.encryptor)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("Drive routePointsEnc decrypt failed; falling back to plaintext",
				slog.String("drive_id", d.ID),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	if len(raw) == 0 {
		return
	}
	if !looksLikeJSONArray(raw) {
		if r.logger != nil {
			r.logger.Warn("Drive routePointsEnc decoded to non-array; falling back to plaintext",
				slog.String("drive_id", d.ID),
			)
		}
		return
	}
	d.RoutePoints = raw
}
