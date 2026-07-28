// Drive catalog-list read path. Split out of drive_repo.go so the
// wide-read path (full Drive row + routePoints dual-read) stays
// adjacent to its scan helpers and this file stays focused on the
// slim projection used by the paginated drive-history endpoint.
//
// MYR-133: `GET /api/vehicles/{vehicleId}/drives` emits ~12 fields per
// drive (DriveSummary per rest-api.md §5.2.2). The wide read would
// pull `routePoints` (potentially 200KB+ per drive) on every row —
// catastrophic at typical history sizes. This file binds to
// `queryDriveListByVehicle*` instead. Detail consumers (single drive,
// route playback) continue to use the wide `GetByID` path.
//
// AGENTS.md "Performance invariants": list endpoints use lean
// projections; wide selects belong only in detail/edit handlers.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DriveSummaryRow is the slim catalog shape returned by
// DriveRepo.ListByVehicleID. Mirrors the columns selected by
// `queryDriveListByVehicle` and the wire fields emitted by
// `internal/telemetry/vehicle_drives_handler.go` driveSummary. No
// routePoints, no energyUsedKwh/interventions — those belong in the
// wide detail read.
//
// MYR-145: start/end Location + Address are included so the SDK can
// render origin/destination labels in the drive-history list without
// a per-row drive-detail fetch. The four columns are small `TEXT`
// values (reverse-geocoded place names + street addresses) and stay
// well within the lean-projection budget. Empty strings are
// represented as `""` (the Prisma columns are `TEXT NOT NULL DEFAULT
// ”`) and are surfaced unchanged to the wire layer; the handler
// decides how to expose them (e.g., empty string vs. omitted JSON key).
//
// MYR-152: FsdMiles + FsdPercentage are included so the list can show
// FSD usage per drive. Both are small `double` columns (P0,
// non-identifying) that stay within the lean-projection budget.
type DriveSummaryRow struct {
	ID               string
	VehicleID        string
	Date             string
	StartTime        string
	EndTime          string
	StartLocation    string
	StartAddress     string
	EndLocation      string
	EndAddress       string
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	StartChargeLevel int
	EndChargeLevel   int
	FsdMiles         float64
	FsdPercentage    float64
	CreatedAt        time.Time
}

// DriveListPage is the paginated result returned by ListByVehicleID.
// HasMore is true when the underlying limit+1 probe returned an extra
// row; Items is trimmed back to the caller's requested limit before
// return.
type DriveListPage struct {
	Items   []DriveSummaryRow
	HasMore bool
}

// DriveListCursor is the (startTime, id) anchor pair encoded into the
// opaque cursor exposed at the REST surface (rest-api.md §4.2.1). The
// zero value means "first page".
type DriveListCursor struct {
	StartTime string
	ID        string
}

// ListByVehicleID returns a page of completed drives for the given
// vehicle, ordered by (startTime DESC, id DESC) per rest-api.md
// §4.2.2. Pagination uses the (startTime, id) tuple anchor — pass a
// zero-value cursor for the first page and the cursor returned by the
// prior call to resume.
//
// `limit` SHOULD be in [1, 100]; callers (REST handler) validate the
// range before invoking this method. An out-of-range limit is not
// rejected at this layer — the SQL LIMIT clause handles it directly.
// The probe size is `limit + 1`: the extra row drives the HasMore
// flag without a separate COUNT.
func (r *DriveRepo) ListByVehicleID(ctx context.Context, vehicleID string, cursor DriveListCursor, limit int) (DriveListPage, error) {
	start := time.Now()
	probe := limit + 1

	var rows pgx.Rows
	var err error
	if cursor.StartTime == "" || cursor.ID == "" {
		rows, err = r.pool.Query(ctx, queryDriveListByVehicle, vehicleID, probe)
	} else {
		rows, err = r.pool.Query(ctx, queryDriveListByVehicleCursor, vehicleID, cursor.StartTime, cursor.ID, probe)
	}
	if err != nil {
		r.metrics.IncQueryError("drive.list_by_vehicle")
		r.metrics.ObserveQueryDuration("drive.list_by_vehicle", time.Since(start).Seconds())
		return DriveListPage{}, fmt.Errorf("DriveRepo.ListByVehicleID(%s): %w", vehicleID, err)
	}
	defer rows.Close()

	out := make([]DriveSummaryRow, 0, probe)
	for rows.Next() {
		d, scanErr := scanDriveSummaryRow(rows)
		if scanErr != nil {
			r.metrics.IncQueryError("drive.list_by_vehicle")
			r.metrics.ObserveQueryDuration("drive.list_by_vehicle", time.Since(start).Seconds())
			return DriveListPage{}, fmt.Errorf("DriveRepo.ListByVehicleID(%s): %w", vehicleID, scanErr)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("drive.list_by_vehicle")
		r.metrics.ObserveQueryDuration("drive.list_by_vehicle", time.Since(start).Seconds())
		return DriveListPage{}, fmt.Errorf("DriveRepo.ListByVehicleID(%s): rows: %w", vehicleID, err)
	}
	r.metrics.ObserveQueryDuration("drive.list_by_vehicle", time.Since(start).Seconds())

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return DriveListPage{Items: out, HasMore: hasMore}, nil
}

// scanDriveSummaryRow scans the lean projection into a
// DriveSummaryRow. Pure stdlib — no encryptor, no routePoints
// resolution.
func scanDriveSummaryRow(row rowScanner) (DriveSummaryRow, error) {
	var d DriveSummaryRow
	if err := row.Scan(
		&d.ID,
		&d.VehicleID,
		&d.Date,
		&d.StartTime,
		&d.EndTime,
		&d.StartLocation,
		&d.StartAddress,
		&d.EndLocation,
		&d.EndAddress,
		&d.DistanceMiles,
		&d.DurationMinutes,
		&d.AvgSpeedMph,
		&d.MaxSpeedMph,
		&d.StartChargeLevel,
		&d.EndChargeLevel,
		&d.FsdMiles,
		&d.FsdPercentage,
		&d.CreatedAt,
	); err != nil {
		return DriveSummaryRow{}, fmt.Errorf("scan drive summary: %w", err)
	}
	return d, nil
}
