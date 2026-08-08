// Package store — VehicleRepo stores vehicle coordinates as ciphertext
// only (NFR-3.23, NFR-3.25, MYR-433).
//
// Read path: every (lat, lng) pair (`latitude`, `destinationLatitude`,
// `originLatitude` and their longitude mates) comes from the `*Enc`
// ciphertext columns. The plaintext Float columns are not selected. A
// half-pair `*Enc` row (one column populated, the other NULL) is corrupt
// and surfaces as no location for the entire pair — see
// vehicle_gps_encryption.go for the rationale and the byte-compatible TS
// counterpart. There is no plaintext fallback: MYR-433 removed it,
// because a fallback requires the plaintext column to stay readable,
// which is the exposure it set out to close.
//
// Write path: every UPDATE encrypts the pair into the `*Enc` TEXT
// columns and writes nothing to the plaintext Floats. Half-pair input
// (one half nil) writes neither half, preserving the atomic-pair
// invariant.
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

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// VehicleRepo reads and writes vehicle records in the Prisma-owned
// "Vehicle" table. It never creates or deletes vehicles -- that is
// the Next.js app's responsibility.
//
// The six GPS columns and the nav-route blob are encrypted on write and
// decrypted on read; ciphertext is their only store. See the package
// comment above.
type VehicleRepo struct {
	pool      *pgxpool.Pool
	metrics   Metrics
	encryptor cryptox.Encryptor // nil means this repo cannot read or write location
	logger    *slog.Logger      // optional; warnings go here when non-nil
}

// NewVehicleRepo creates a VehicleRepo without column-level encryption.
// Retained for call sites that have no Encryptor in scope and do not
// need location data. New call sites should prefer
// NewVehicleRepoWithEncryption.
//
// Since MYR-433 the repo built here reads NO coordinates and writes NO
// coordinates — every GPS column and the nav-route blob come back zero
// or nil. That is a real capability loss, not a degraded mode: there is
// no plaintext column left to fall back to. Anything that renders a
// position MUST use the encrypting constructor.
func NewVehicleRepo(pool *pgxpool.Pool, metrics Metrics) *VehicleRepo {
	return &VehicleRepo{pool: pool, metrics: metrics}
}

// NewVehicleRepoWithEncryption is the constructor every location-reading
// caller needs: the Encryptor is required and used on every read
// (decrypting `*Enc`) and every write (encrypting into `*Enc`). The
// logger is optional but recommended — half-pair reads log at Warn and
// decrypt failures at Error, alongside the
// telemetry_store_decrypt_failures_total counter.
func NewVehicleRepoWithEncryption(pool *pgxpool.Pool, metrics Metrics, encryptor cryptox.Encryptor, logger *slog.Logger) *VehicleRepo {
	if encryptor == nil {
		// Defensive: a nil Encryptor would silently produce empty *Enc
		// columns — and since MYR-433 those are the only place
		// coordinates live, that means silently discarding every
		// position this server receives. Fail loudly so the composition
		// root catches it at startup rather than at the first frame.
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
	v, err := r.scanVehicleByID(ctx, id)
	r.metrics.ObserveQueryDuration("vehicle.get_by_id", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_by_id")
		return Vehicle{}, fmt.Errorf("VehicleRepo.GetByID(%s): %w", id, err)
	}
	return v, nil
}

// scanVehicleByID runs the snapshot read (queryVehicleByID, which LEFT JOINs the
// go_vehicle_control_state side table) and scans the base vehicle row plus every
// owner-control column. A NULL control column (no side-table row, or a field
// never observed) scans into a nil pointer, which the snapshot surfaces as an
// absent/unknown control — never a fabricated value.
//
// The destinations and their Vehicle assignments live on controlStateScan
// (vehicle_control_scan.go), whose dests() order MUST track queryVehicleByID.
// MYR-303/308 moved them there: at 31 columns the inline form was three lines
// per column and had outgrown the funlen cap.
func (r *VehicleRepo) scanVehicleByID(ctx context.Context, id string) (Vehicle, error) {
	row := r.pool.QueryRow(ctx, queryVehicleByID, id)
	var cs controlStateScan
	v, err := r.scanVehicleRowExtra(row, cs.dests()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Vehicle{}, ErrVehicleNotFound
	}
	if err != nil {
		return Vehicle{}, fmt.Errorf("scan vehicle: %w", err)
	}
	cs.applyTo(&v)
	return v, nil
}

// UpdateTelemetry performs a partial update of real-time telemetry fields
// for one vehicle. Only non-nil fields in the update are written.
//
// MYR-433: when an Encryptor is wired the GPS pairs are written to the
// `*Enc` TEXT shadows and nowhere else. Half-pair input (one half nil)
// writes neither half, per the atomic-pair invariant — and since the
// plaintext columns are no longer written either, such an update simply
// leaves that pair unchanged.
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
