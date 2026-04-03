package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DriveRepo manages drive records in the Prisma-owned "Drive" table.
type DriveRepo struct {
	pool    *pgxpool.Pool
	metrics Metrics
}

// NewDriveRepo creates a DriveRepo backed by the given connection pool.
func NewDriveRepo(pool *pgxpool.Pool, metrics Metrics) *DriveRepo {
	return &DriveRepo{pool: pool, metrics: metrics}
}

// Create inserts a new drive record when a drive starts. The drive is
// created with placeholder end-time fields that will be filled in when
// the drive completes.
func (r *DriveRepo) Create(ctx context.Context, drive DriveRecord) error {
	routePoints := drive.RoutePoints
	if routePoints == nil {
		routePoints = json.RawMessage("[]")
	}

	start := time.Now()
	_, err := r.pool.Exec(ctx, queryDriveInsert,
		drive.ID, drive.VehicleID, drive.Date, drive.StartTime, drive.EndTime,
		drive.StartLocation, drive.StartAddress, drive.EndLocation, drive.EndAddress,
		drive.DistanceMiles, drive.DurationMinutes, drive.AvgSpeedMph, drive.MaxSpeedMph,
		drive.EnergyUsedKwh, drive.StartChargeLevel, drive.EndChargeLevel,
		drive.FsdMiles, drive.FsdPercentage, drive.Interventions, routePoints,
	)
	r.metrics.ObserveQueryDuration("drive.create", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.create")
		return fmt.Errorf("DriveRepo.Create(%s): %w", drive.ID, err)
	}
	return nil
}

// AppendRoutePoints appends route points to the drive's routePoints JSON
// array. Uses PostgreSQL jsonb_concat (||) to avoid read-modify-write.
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
	tag, err := r.pool.Exec(ctx, queryDriveAppendRoutePoints, driveID, json.RawMessage(pointsJSON))
	r.metrics.ObserveQueryDuration("drive.append_route_points", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.append_route_points")
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DriveRepo.AppendRoutePoints(%s): %w", driveID, ErrDriveNotFound)
	}
	return nil
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
func (r *DriveRepo) GetByID(ctx context.Context, id string) (DriveRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryDriveByID, id)

	var d DriveRecord
	err := row.Scan(
		&d.ID, &d.VehicleID, &d.Date, &d.StartTime, &d.EndTime,
		&d.StartLocation, &d.StartAddress, &d.EndLocation, &d.EndAddress,
		&d.DistanceMiles, &d.DurationMinutes, &d.AvgSpeedMph, &d.MaxSpeedMph,
		&d.EnergyUsedKwh, &d.StartChargeLevel, &d.EndChargeLevel,
		&d.FsdMiles, &d.FsdPercentage, &d.Interventions, &d.RoutePoints, &d.CreatedAt,
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
	return d, nil
}
