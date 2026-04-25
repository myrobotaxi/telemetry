package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VehicleRepo reads and writes vehicle records in the Prisma-owned
// "Vehicle" table. It never creates or deletes vehicles -- that is
// the Next.js app's responsibility.
type VehicleRepo struct {
	pool    *pgxpool.Pool
	metrics Metrics
}

// NewVehicleRepo creates a VehicleRepo backed by the given connection pool.
func NewVehicleRepo(pool *pgxpool.Pool, metrics Metrics) *VehicleRepo {
	return &VehicleRepo{pool: pool, metrics: metrics}
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
		var v Vehicle
		var status string
		if err := rows.Scan(
			&v.ID, &v.UserID, &v.VIN, &v.Name,
			&v.Model, &v.Year, &v.Color, &status,
			&v.ChargeLevel, &v.EstimatedRange, &v.Speed, &v.GearPosition,
			&v.Heading, &v.Latitude, &v.Longitude,
			&v.LocationName, &v.LocationAddress,
			&v.InteriorTemp, &v.ExteriorTemp,
			&v.OdometerMiles, &v.FsdMilesSinceReset,
			&v.DestinationName, &v.DestinationAddress,
			&v.DestinationLatitude, &v.DestinationLongitude,
			&v.OriginLatitude, &v.OriginLongitude,
			&v.EtaMinutes, &v.TripDistRemaining,
			&v.NavRouteCoordinates, &v.LastUpdated,
		); err != nil {
			return nil, fmt.Errorf("VehicleRepo.ListByUser(%s): scan: %w", userID, err)
		}
		v.Status = VehicleStatus(status)
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
func (r *VehicleRepo) UpdateTelemetry(ctx context.Context, vin string, update VehicleUpdate) error {
	query, args, ok := buildTelemetryUpdate(vin, update)
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
// scans it into a Vehicle struct.
func (r *VehicleRepo) scanVehicle(ctx context.Context, query string, arg any) (Vehicle, error) {
	row := r.pool.QueryRow(ctx, query, arg)

	var v Vehicle
	var status string
	err := row.Scan(
		&v.ID, &v.UserID, &v.VIN, &v.Name,
		&v.Model, &v.Year, &v.Color, &status,
		&v.ChargeLevel, &v.EstimatedRange, &v.Speed, &v.GearPosition,
		&v.Heading, &v.Latitude, &v.Longitude,
		&v.LocationName, &v.LocationAddress,
		&v.InteriorTemp, &v.ExteriorTemp,
		&v.OdometerMiles, &v.FsdMilesSinceReset,
		&v.DestinationName, &v.DestinationAddress,
		&v.DestinationLatitude, &v.DestinationLongitude,
		&v.OriginLatitude, &v.OriginLongitude,
		&v.EtaMinutes, &v.TripDistRemaining,
		&v.NavRouteCoordinates, &v.LastUpdated,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Vehicle{}, ErrVehicleNotFound
	}
	if err != nil {
		return Vehicle{}, fmt.Errorf("scan vehicle: %w", err)
	}

	v.Status = VehicleStatus(status)
	return v, nil
}
