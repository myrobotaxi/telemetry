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
		&v.ID, &v.UserID, &v.VIN, &v.Name, &status,
		&v.ChargeLevel, &v.EstimatedRange, &v.Speed, &v.GearPosition,
		&v.Heading, &v.Latitude, &v.Longitude,
		&v.InteriorTemp, &v.ExteriorTemp, &v.OdometerMiles,
		&v.DestinationName, &v.DestinationLatitude,
		&v.DestinationLongitude, &v.OriginLatitude, &v.OriginLongitude,
		&v.EtaMinutes, &v.TripDistRemaining,
		&v.LastUpdated,
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
