// Package store — DriveRepo stores a drive's GPS trail as ciphertext
// only (NFR-3.23, MYR-433). The trail is the single most sensitive
// non-credential thing in this database: it is a minute-by-minute record
// of where a person drove. The client's acceptance bar for MYR-433 was
// that an operator with database access cannot read it.
//
// Read path: GetByID decrypts `routePointsEnc`. The plaintext
// `routePoints` JSONB column is not selected and is never consulted; on
// any decrypt failure the trail reads as empty rather than falling back.
//
// Write path: Create seeds `routePoints` with the literal `[]` (the
// column is NOT NULL on the Prisma schema and must be given something)
// and puts any real seed trail in `routePointsEnc`.
//
// AppendRoutePoints is the interesting one. The pre-MYR-433 version
// appended in SQL with `jsonb ||`, which was atomic for free, then
// re-encrypted the RETURNING value into the shadow fail-open. Ciphertext
// cannot be concatenated in the database, so the append is now a
// read-modify-write in Go, wrapped in a transaction with SELECT … FOR
// UPDATE. Two properties are deliberate:
//
//   - The row lock replaces the atomicity the SQL concat gave us. Without
//     it, two concurrent flushes for one drive would both decrypt the same
//     trail and the second commit would silently drop the first's points.
//   - It is fail-CLOSED. A decrypt or encrypt failure aborts the append
//     and returns an error. The old fail-open behaviour was correct when a
//     plaintext copy was still landing; now, writing anyway would overwrite
//     an unreadable-but-intact trail with a fragment, destroying history.
//     An error lets the caller retry with the buffer still in hand.
//
// The Encryptor is opt-in via NewDriveRepoWithEncryption. A repo built
// with the legacy NewDriveRepo constructor cannot persist or read a
// trail at all — AppendRoutePoints returns ErrEncryptionRequired.

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

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
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
// MYR-433: the seed trail is written to routePointsEnc only; the
// plaintext column receives the literal `[]` from the INSERT itself. The
// seed is typically empty anyway, in which case
// routeblob.EncryptJSONBytes returns the empty sentinel and the shadow
// is left NULL until the first AppendRoutePoints call.
func (r *DriveRepo) Create(ctx context.Context, drive DriveRecord) error {
	routePoints := drive.RoutePoints
	if routePoints == nil {
		routePoints = json.RawMessage("[]")
	}

	encShadow, err := r.encryptRoutePointsRaw(routePoints)
	if err != nil {
		return fmt.Errorf("DriveRepo.Create(%s): %w", drive.ID, err)
	}

	labels, err := r.sealDriveLabels(
		drive.StartLocation, drive.StartAddress, drive.EndLocation, drive.EndAddress)
	if err != nil {
		return fmt.Errorf("DriveRepo.Create(%s): %w", drive.ID, err)
	}

	start := time.Now()
	_, err = r.pool.Exec(ctx, queryDriveInsert,
		drive.ID, drive.VehicleID, drive.Date, drive.StartTime, drive.EndTime,
		labels[0], labels[1], labels[2], labels[3],
		drive.DistanceMiles, drive.DurationMinutes, drive.AvgSpeedMph, drive.MaxSpeedMph,
		drive.EnergyUsedKwh, drive.StartChargeLevel, drive.EndChargeLevel,
		drive.FsdMiles, drive.FsdPercentage, drive.Interventions,
		encShadow, // nil-string when encryptor is absent or shadow is empty
	)
	r.metrics.ObserveQueryDuration("drive.create", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.create")
		return fmt.Errorf("DriveRepo.Create(%s): %w", drive.ID, err)
	}
	return nil
}

// Delete removes a Drive row outright. Called by the writer when the
// drive detector discards a micro-drive (MYR-160) — the row created on
// drive.started must not linger as a stuck-open drive. Deleting a row
// that was never created (e.g. the VIN lookup failed on drive.started)
// affects zero rows and is intentionally not an error.
func (r *DriveRepo) Delete(ctx context.Context, driveID string) error {
	start := time.Now()
	_, err := r.pool.Exec(ctx, queryDriveDelete, driveID)
	r.metrics.ObserveQueryDuration("drive.delete", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.delete")
		return fmt.Errorf("DriveRepo.Delete(%s): %w", driveID, err)
	}
	return nil
}

// AppendRoutePoints appends route points to the drive's encrypted GPS
// trail (MYR-433).
//
// Ciphertext cannot be concatenated in SQL, so this is a
// decrypt-append-reseal cycle run inside a transaction that holds a row
// lock (SELECT … FOR UPDATE) for its duration. The lock is what makes
// concurrent flushes for the same drive safe — see the package comment.
//
// Fail-closed throughout: if the existing trail cannot be decrypted, the
// append is abandoned rather than overwriting it with just the new
// points.
func (r *DriveRepo) AppendRoutePoints(ctx context.Context, driveID string, points []RoutePointRecord) error {
	if len(points) == 0 {
		return nil
	}
	if r.encryptor == nil {
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, ErrEncryptionRequired)
	}

	start := time.Now()
	err := r.appendRoutePointsTx(ctx, driveID, points)
	r.metrics.ObserveQueryDuration("drive.append_route_points", time.Since(start).Seconds())
	if err != nil {
		if !errors.Is(err, ErrDriveNotFound) {
			r.metrics.IncQueryError("drive.append_route_points")
		}
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, err)
	}
	return nil
}

// appendRoutePointsTx is the transactional body of AppendRoutePoints.
// Split out so the caller owns metrics and error decoration.
func (r *DriveRepo) appendRoutePointsTx(ctx context.Context, driveID string, points []RoutePointRecord) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	var existingCT *string
	if scanErr := tx.QueryRow(ctx, queryDriveLockRoutePointsEnc, driveID).Scan(&existingCT); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return ErrDriveNotFound
		}
		return fmt.Errorf("lock row: %w", scanErr)
	}

	merged, err := r.mergeRoutePoints(existingCT, points)
	if err != nil {
		return err
	}

	ct, err := routeblob.EncryptJSONBytes(merged, r.encryptor)
	if err != nil {
		return fmt.Errorf("encrypt trail: %w", err)
	}
	if ct == "" {
		// Unreachable: len(points) > 0 guarantees a non-empty array. Guard
		// anyway so a future change to EncryptJSONBytes can't quietly NULL
		// a live trail.
		return fmt.Errorf("encrypt trail: unexpected empty ciphertext for %d points", len(points))
	}

	if _, err := tx.Exec(ctx, queryDriveSetRoutePointsEnc, driveID, ct); err != nil {
		return fmt.Errorf("write trail: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// mergeRoutePoints decrypts the existing trail (when present) and
// appends the new points, returning the marshalled full array.
//
// Elements are carried as json.RawMessage so already-persisted points
// are spliced through byte-for-byte — no decode/re-encode round trip
// that could alter a float's representation on every single append.
func (r *DriveRepo) mergeRoutePoints(existingCT *string, points []RoutePointRecord) (json.RawMessage, error) {
	var trail []json.RawMessage

	if existingCT != nil && *existingCT != "" {
		raw, err := routeblob.DecryptJSONBytes(*existingCT, r.encryptor)
		if err != nil {
			// Fail closed. Appending here would replace a trail we simply
			// cannot read with a fragment, which is unrecoverable.
			//
			// ErrRouteTrailUnreadable marks this PERMANENT so the caller
			// drops the batch instead of retrying a decrypt that cannot
			// start succeeding — see the sentinel's doc comment.
			r.metrics.IncDecryptFailure("routePointsEnc")
			return nil, fmt.Errorf("%w: %w", ErrRouteTrailUnreadable, err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &trail); err != nil {
				// Decrypted but not a JSON array. GetByID treats this same
				// condition as "empty trail" and keeps serving; the write
				// path cannot do that, because "append to a trail I cannot
				// parse" has no safe answer — starting fresh would silently
				// destroy whatever IS in there. So the read stays lenient,
				// the write refuses, and both are permanent-and-visible.
				r.metrics.IncDecryptFailure("routePointsEnc")
				return nil, fmt.Errorf("%w: decode: %w", ErrRouteTrailUnreadable, err)
			}
		}
	}

	for i := range points {
		b, err := json.Marshal(points[i])
		if err != nil {
			return nil, fmt.Errorf("marshal point %d: %w", i, err)
		}
		trail = append(trail, b)
	}

	merged, err := json.Marshal(trail)
	if err != nil {
		return nil, fmt.Errorf("marshal trail: %w", err)
	}
	return merged, nil
}

// encryptRoutePointsRaw is the Create-path companion: encrypts the seed
// routePoints array into the shadow value and returns the *string pgx
// wants for the parameter slot. A nil result writes NULL into the column.
//
// Fail-CLOSED, matching AppendRoutePoints. The pre-MYR-433 version logged
// at Warn and returned nil "so the plaintext write still goes through" —
// but there is no plaintext write any more, so swallowing the error would
// create a drive whose seed trail was silently discarded. An error here
// aborts Create, and the drive detector will retry on the next event.
//
// An empty seed (the normal case — `[]`) is not a failure: EncryptJSONBytes
// returns the empty sentinel and the shadow stays NULL until the first
// append.
func (r *DriveRepo) encryptRoutePointsRaw(raw json.RawMessage) (*string, error) {
	if r.encryptor == nil {
		return nil, nil //nolint:nilnil // no encryptor wired: no shadow to write
	}
	ct, err := routeblob.EncryptJSONBytes(raw, r.encryptor)
	if err != nil {
		return nil, fmt.Errorf("encrypt seed trail: %w", err)
	}
	if ct == "" {
		return nil, nil //nolint:nilnil // empty seed: leave the shadow NULL
	}
	return &ct, nil
}

// Complete updates a drive with its final stats when the drive ends.
//
// MYR-447: the two end labels are sealed before they leave this method
// and land in "endLocationEnc"/"endAddressEnc". Fail-closed — an encrypt
// error aborts the completion rather than writing a drive whose endpoint
// silently lost its address; the caller logs and the row stays open for
// the startup reconciler.
func (r *DriveRepo) Complete(ctx context.Context, driveID string, stats DriveCompletion) error {
	endLoc, endAddr, err := r.sealEndLabels(stats.EndLocation, stats.EndAddress)
	if err != nil {
		return fmt.Errorf("DriveRepo.Complete(%s): %w", driveID, err)
	}

	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryDriveComplete,
		driveID, stats.EndTime, endLoc, endAddr,
		stats.DistanceMiles, stats.DurationMinutes,
		stats.AvgSpeedMph, stats.MaxSpeedMph, stats.EnergyUsedKwh,
		stats.EndChargeLevel, stats.FsdMiles, stats.FsdPercentage,
		stats.Interventions, stats.StartChargeLevel,
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
// MYR-433: the trail comes from routePointsEnc alone. Decrypt or
// shape-validation failures leave RoutePoints empty and log at Warn —
// the plaintext column is not read.
func (r *DriveRepo) GetByID(ctx context.Context, id string) (DriveRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryDriveByID, id)

	var d DriveRecord
	var routePointsEnc *string
	var startLocEnc, startAddrEnc, endLocEnc, endAddrEnc *string
	err := row.Scan(
		&d.ID, &d.VehicleID, &d.Date, &d.StartTime, &d.EndTime,
		&startLocEnc, &startAddrEnc, &endLocEnc, &endAddrEnc,
		&d.DistanceMiles, &d.DurationMinutes, &d.AvgSpeedMph, &d.MaxSpeedMph,
		&d.EnergyUsedKwh, &d.StartChargeLevel, &d.EndChargeLevel,
		&d.FsdMiles, &d.FsdPercentage, &d.Interventions, &d.CreatedAt,
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
	r.applyResolvedDriveLabels(&d, startLocEnc, startAddrEnc, endLocEnc, endAddrEnc)
	return d, nil
}

// applyResolvedRoutePoints decrypts the trail from the ciphertext
// column. On any failure RoutePoints stays empty — MYR-433 removed the
// plaintext column from the projection, so there is nothing to fall back
// to, and an empty trail is a safer answer than a stale one.
//
// A repo built without an Encryptor reads no trail at all.
func (r *DriveRepo) applyResolvedRoutePoints(d *DriveRecord, ct *string) {
	if r.encryptor == nil || ct == nil || *ct == "" {
		return
	}
	raw, err := routeblob.DecryptJSONBytes(*ct, r.encryptor)
	if err != nil {
		// ERROR, not Warn: this read is fail-soft, so a key mistake would
		// otherwise present as "this drive recorded no route".
		if r.logger != nil {
			r.logger.Error("Drive routePointsEnc decrypt failed; surfacing empty trail",
				slog.String("drive_id", d.ID),
				slog.String("error", err.Error()),
			)
		}
		r.metrics.IncDecryptFailure("routePointsEnc")
		return
	}
	if len(raw) == 0 {
		return
	}
	if !looksLikeJSONArray(raw) {
		if r.logger != nil {
			r.logger.Error("Drive routePointsEnc decoded to non-array; surfacing empty trail",
				slog.String("drive_id", d.ID),
			)
		}
		r.metrics.IncDecryptFailure("routePointsEnc")
		return
	}
	d.RoutePoints = raw
}
