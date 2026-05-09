// Package store — VehicleRepo lives behind a dual-write contract during
// the MYR-63 cross-repo Vehicle-GPS encryption rollout (NFR-3.23,
// NFR-3.25).
//
// Read path: every (lat, lng) pair (`latitude`, `destinationLatitude`,
// `originLatitude` and their longitude mates) prefers the encrypted
// shadow `*Enc` ciphertext when both halves are present. A half-pair
// `*Enc` row (one column populated, the other NULL) is corrupt and
// forces a plaintext fallback for the entire pair — see
// vehicle_gps_encryption.go for the rationale and the byte-compatible
// TS counterpart.
//
// Write path: every UPDATE encrypts the new pair and dual-writes BOTH
// the plaintext Float column AND the `*Enc` TEXT column in one
// statement. Half-pair input (one half nil) skips the *Enc write
// entirely and logs a warning, preserving the atomic-pair invariant.
//
// The Encryptor MUST be injected via constructor. The composition root
// owns the loaded KeySet for the entire process — never call
// cryptox.MustLoad() / LoadKeySetFromEnv() from inside this package.

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
)

// VehicleRepo reads and writes vehicle records in the Prisma-owned
// "Vehicle" table. It never creates or deletes vehicles -- that is
// the Next.js app's responsibility.
//
// During the MYR-63 dual-write rollout window the repo encrypts on
// write and prefers ciphertext on read for the six GPS columns. See
// the package comment above.
type VehicleRepo struct {
	pool      *pgxpool.Pool
	metrics   Metrics
	encryptor cryptox.Encryptor // nil disables the dual-write (legacy callers)
	logger    *slog.Logger      // optional; warnings go here when non-nil
}

// NewVehicleRepo creates a VehicleRepo without column-level encryption.
// Retained for the migration window so existing call sites that don't
// yet have an Encryptor in scope keep compiling. New call sites should
// prefer NewVehicleRepoWithEncryption.
//
// The repo built here returns plaintext rows on read and writes
// plaintext-only on update. Once every caller is migrated this
// constructor can be retired in a follow-up.
func NewVehicleRepo(pool *pgxpool.Pool, metrics Metrics) *VehicleRepo {
	return &VehicleRepo{pool: pool, metrics: metrics}
}

// NewVehicleRepoWithEncryption is the dual-write constructor: the
// Encryptor is required and used on every read (preferring `*Enc`) and
// every write (encrypting + dual-writing). The logger is optional but
// recommended — half-pair `*Enc` reads and decrypt failures are logged
// at Warn so the rollout's edge cases surface in operator dashboards.
func NewVehicleRepoWithEncryption(pool *pgxpool.Pool, metrics Metrics, encryptor cryptox.Encryptor, logger *slog.Logger) *VehicleRepo {
	if encryptor == nil {
		// Defensive: a nil Encryptor would silently produce empty *Enc
		// columns, which the read path would then fall back to plaintext
		// for, masking the rollout regression. Fail loudly so the
		// composition root catches this at startup.
		panic("store.NewVehicleRepoWithEncryption: encryptor must not be nil")
	}
	return &VehicleRepo{pool: pool, metrics: metrics, encryptor: encryptor, logger: logger}
}

// GetByVIN returns the vehicle with the given VIN.
// Returns ErrVehicleNotFound if no vehicle has that VIN.
func (r *VehicleRepo) GetByVIN(ctx context.Context, vin string) (Vehicle, error) {
	start := time.Now()
	v, err := r.scanVehicle(ctx, queryVehicleByVIN, vin)
	r.metrics.ObserveQueryDuration("vehicle.get_by_vin", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_by_vin")
		return Vehicle{}, fmt.Errorf("VehicleRepo.GetByVIN(%s): %w", redactVIN(vin), err)
	}
	return v, nil
}

// GetIDsByVIN returns just the (vehicleID, userID) pair for the given VIN.
// Both values are immutable for the lifetime of a vehicle row, which makes
// this safe to cache indefinitely. Use this in hot paths that only need
// to map a VIN to its identifiers — it avoids pulling the heavy
// navRouteCoordinates JSON and other telemetry columns that GetByVIN reads.
// Returns ErrVehicleNotFound if no vehicle has that VIN.
func (r *VehicleRepo) GetIDsByVIN(ctx context.Context, vin string) (id, userID string, err error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryVehicleIDsByVIN, vin)
	scanErr := row.Scan(&id, &userID)
	r.metrics.ObserveQueryDuration("vehicle.get_ids_by_vin", time.Since(start).Seconds())
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("VehicleRepo.GetIDsByVIN(%s): %w", redactVIN(vin), ErrVehicleNotFound)
	}
	if scanErr != nil {
		r.metrics.IncQueryError("vehicle.get_ids_by_vin")
		return "", "", fmt.Errorf("VehicleRepo.GetIDsByVIN(%s): %w", redactVIN(vin), scanErr)
	}
	return id, userID, nil
}

// ListByUser returns every vehicle owned by the given user, ordered by
// name and VIN. Returns an empty slice (and nil error) when the user has
// no linked vehicles.
func (r *VehicleRepo) ListByUser(ctx context.Context, userID string) ([]Vehicle, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryVehiclesByUser, userID)
	r.metrics.ObserveQueryDuration("vehicle.list_by_user", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_by_user")
		return nil, fmt.Errorf("VehicleRepo.ListByUser(%s): %w", userID, err)
	}
	defer rows.Close()

	var out []Vehicle
	for rows.Next() {
		v, err := r.scanVehicleRow(rows)
		if err != nil {
			return nil, fmt.Errorf("VehicleRepo.ListByUser(%s): %w", userID, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("VehicleRepo.ListByUser(%s): rows: %w", userID, err)
	}
	return out, nil
}

// GetByID returns the vehicle with the given Prisma cuid.
// Returns ErrVehicleNotFound if no vehicle has that ID.
func (r *VehicleRepo) GetByID(ctx context.Context, id string) (Vehicle, error) {
	start := time.Now()
	v, err := r.scanVehicle(ctx, queryVehicleByID, id)
	r.metrics.ObserveQueryDuration("vehicle.get_by_id", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_by_id")
		return Vehicle{}, fmt.Errorf("VehicleRepo.GetByID(%s): %w", id, err)
	}
	return v, nil
}

// UpdateTelemetry performs a partial update of real-time telemetry fields
// for one vehicle. Only non-nil fields in the update are written.
//
// MYR-63: when an Encryptor is wired the GPS pairs are dual-written —
// plaintext Float columns AND `*Enc` TEXT shadows in the same UPDATE.
// Half-pair input (one half nil) is rejected for the *Enc dual-write
// per the atomic-pair invariant; the plaintext column still updates.
func (r *VehicleRepo) UpdateTelemetry(ctx context.Context, vin string, update VehicleUpdate) error {
	encShadows, err := r.buildShadows(update)
	if err != nil {
		return fmt.Errorf("VehicleRepo.UpdateTelemetry(%s): %w", redactVIN(vin), err)
	}

	query, args, ok := buildTelemetryUpdate(vin, update, encShadows)
	if !ok {
		return nil // nothing to update
	}

	start := time.Now()
	tag, err := r.pool.Exec(ctx, query, args...)
	r.metrics.ObserveQueryDuration("vehicle.update_telemetry", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.update_telemetry")
		return fmt.Errorf("VehicleRepo.UpdateTelemetry(%s): %w", redactVIN(vin), err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("VehicleRepo.UpdateTelemetry(%s): %w", redactVIN(vin), ErrVehicleNotFound)
	}
	return nil
}

// UpdateStatus sets the vehicle's status enum.
func (r *VehicleRepo) UpdateStatus(ctx context.Context, vin string, status VehicleStatus) error {
	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryUpdateVehicleStatus, string(status), vin)
	r.metrics.ObserveQueryDuration("vehicle.update_status", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.update_status")
		return fmt.Errorf("VehicleRepo.UpdateStatus(%s): %w", redactVIN(vin), err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("VehicleRepo.UpdateStatus(%s): %w", redactVIN(vin), ErrVehicleNotFound)
	}
	return nil
}

// buildShadows encrypts each GPS pair in update into the matching `*Enc`
// columns. Returns a map keyed by *Enc column name; an entry exists only
// if both halves of a pair were present in the update. A nil Encryptor
// short-circuits to a nil map so the dual-write is opt-in via the
// constructor — buildTelemetryUpdate treats nil and empty identically.
//
//nolint:nilnil // (nil map, nil err) signals "no encryptor wired".
func (r *VehicleRepo) buildShadows(update VehicleUpdate) (map[string]string, error) {
	if r.encryptor == nil {
		return nil, nil
	}
	out := make(map[string]string, 6)
	pairs := []struct {
		pair gpsPair
		lat  *float64
		lng  *float64
	}{
		{gpsPairs[0], update.Latitude, update.Longitude},
		{gpsPairs[1], update.DestinationLatitude, update.DestinationLongitude},
		{gpsPairs[2], update.OriginLatitude, update.OriginLongitude},
	}
	for _, p := range pairs {
		shadow, has, err := buildEncryptedGPSPair(p.lat, p.lng, r.encryptor, r.logger, p.pair)
		if err != nil {
			return nil, fmt.Errorf("encrypt GPS pair %s/%s: %w", p.pair.lat, p.pair.lng, err)
		}
		if !has {
			continue
		}
		out[p.pair.lat+"Enc"] = *shadow.latEnc
		out[p.pair.lng+"Enc"] = *shadow.lngEnc
	}
	return out, nil
}

// scanVehicle executes a query expected to return one vehicle row and
// scans it into a Vehicle struct, applying the dual-read GPS resolution.
// scanVehicleRow + applyResolvedGPS live in vehicle_repo_scan.go.
func (r *VehicleRepo) scanVehicle(ctx context.Context, query string, arg any) (Vehicle, error) {
	row := r.pool.QueryRow(ctx, query, arg)
	v, err := r.scanVehicleRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Vehicle{}, ErrVehicleNotFound
	}
	if err != nil {
		return Vehicle{}, fmt.Errorf("scan vehicle: %w", err)
	}
	return v, nil
}
