// Orphan-reconciliation read path. Split out of drive_repo.go so the
// wide-read + dual-write (routePoints/routePointsEnc) module stays
// focused and this file owns the narrow "open drive" projection used
// only on startup.
//
// MYR-146: when a Fly redeploy lands mid-drive, the Detector's
// in-memory vehicleState map is lost. Without reconciliation, any
// Drive row whose endTime was unset at deploy time stays open forever
// because the watchdog only scans VINs it already knows about. This
// query is the seed: Detector.Start reads it once, populates
// vehicleState for each row, and the watchdog ends them within
// EndDebounce if telemetry doesn't resume. See
// internal/drives/detector.go reconcileOpenDrives.

package store

import (
	"context"
	"fmt"
	"time"
)

// OpenDriveRow is the slim projection returned by DriveRepo.ListOpen.
// Fields are the minimum the Detector needs to recreate a vehicleState
// keyed by VIN and have the watchdog/end-handler flow produce a
// well-formed DriveEndedEvent for the existing row (the writer's
// handleDriveEnded calls drives.Complete by DriveID, so the in-memory
// activeDrive only needs ID, StartedAt, and StartChargeLevel).
type OpenDriveRow struct {
	ID               string
	VehicleID        string
	VIN              string
	StartTime        string // ISO 8601 — Prisma stores as TEXT
	Date             string
	StartChargeLevel int
}

// ListOpen returns every Drive row whose endTime is unset
// (NULL or empty string). Used exclusively by Detector.Start during
// orphan reconciliation. In steady state the result set is empty;
// after a redeploy mid-drive it contains one row per vehicle that was
// actively driving at the time of restart.
func (r *DriveRepo) ListOpen(ctx context.Context) ([]OpenDriveRow, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryDriveListOpen)
	if err != nil {
		r.metrics.IncQueryError("drive.list_open")
		r.metrics.ObserveQueryDuration("drive.list_open", time.Since(start).Seconds())
		return nil, fmt.Errorf("DriveRepo.ListOpen: %w", err)
	}
	defer rows.Close()

	var out []OpenDriveRow
	for rows.Next() {
		var d OpenDriveRow
		if err := rows.Scan(&d.ID, &d.VehicleID, &d.VIN, &d.StartTime, &d.Date, &d.StartChargeLevel); err != nil {
			r.metrics.IncQueryError("drive.list_open")
			r.metrics.ObserveQueryDuration("drive.list_open", time.Since(start).Seconds())
			return nil, fmt.Errorf("DriveRepo.ListOpen: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("drive.list_open")
		r.metrics.ObserveQueryDuration("drive.list_open", time.Since(start).Seconds())
		return nil, fmt.Errorf("DriveRepo.ListOpen: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("drive.list_open", time.Since(start).Seconds())
	return out, nil
}
