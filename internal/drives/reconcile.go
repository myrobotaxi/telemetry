// Orphan-drive reconciliation. Called from Detector.Start.
//
// MYR-146: a Fly redeploy mid-drive restarts the machine. The
// Detector's in-memory vehicleState map is lost; any Drive row whose
// endTime was unset at deploy time is orphaned — no in-memory state
// references it, so nothing will ever emit a DriveEndedEvent for it.
//
// MYR-149: the original design reattached the most recent orphan per
// VIN to in-memory state (status=StatusDriving, reconciled=true) and
// let the watchdog close it on telemetry silence. That trap never
// sprang when the car was parked but online: idle telemetry pings
// refreshed lastTelemetryAt on every frame (so the silence condition
// never tripped) and cleared the reconciled bypass (so a later end
// was discarded by the micro-drive filter) — the row stayed open
// forever, and a future real drive's first frames were attributed to
// the stale orphan.
//
// The reconciler now ends EVERY orphan synchronously via
// MarkOrphanEnded (Option A). If the car was genuinely mid-drive when
// the deploy happened, the next gear=D frame starts a fresh drive:
// the journey is split across two DB rows, but neither is stuck. The
// route data lost with the in-memory state could not have been
// reconstructed anyway.
package drives

import (
	"context"
	"log/slog"
)

// OpenDriveRow mirrors the slim projection returned by the store
// adapter wired in from cmd/. Defined at the consumer site so the
// drives package stays independent of internal/store (the cmd-layer
// adapter performs the row-shape translation).
type OpenDriveRow struct {
	ID               string
	VIN              string
	StartTime        string // ISO 8601; informational only since MYR-149
	StartChargeLevel int
}

// OpenDriveLister is the read-side dependency the Detector consults on
// Start to discover Drive rows orphaned by a previous restart. A nil
// implementation skips reconciliation entirely — used by unit tests
// and by any future runtime that doesn't persist drives.
//
// MarkOrphanEnded closes an open Drive row immediately (endTime = now,
// zero stats). Since MYR-149 the reconciler invokes it for every
// orphan — the in-memory reattach path was removed because a parked
// but online car kept the reattached drive alive indefinitely.
type OpenDriveLister interface {
	ListOpen(ctx context.Context) ([]OpenDriveRow, error)
	MarkOrphanEnded(ctx context.Context, driveID string) error
}

// reconcileOpenDrives reads every open Drive row from the lister and
// closes each synchronously via MarkOrphanEnded. It is best-effort: a
// DB hiccup on Start MUST NOT block telemetry processing — we log
// Warn and continue. Worst case the orphaned rows stay open until the
// next successful restart cycle.
//
// Called from Start BEFORE bus subscription and BEFORE the watchdog
// goroutine starts, so no telemetry handler can observe (or race) a
// half-reconciled view of the DB.
func (d *Detector) reconcileOpenDrives(ctx context.Context) {
	if d.reconciler == nil {
		return
	}

	rows, err := d.reconciler.ListOpen(ctx)
	if err != nil {
		d.logger.Warn("orphan drive reconciliation failed; continuing without",
			slog.String("error", err.Error()),
		)
		return
	}

	ended := 0
	for i := range rows {
		row := &rows[i]
		if row.ID == "" {
			continue
		}
		if err := d.reconciler.MarkOrphanEnded(ctx, row.ID); err != nil {
			// Fail open, same posture as ListOpen. The row stays
			// open and will be retried on next restart.
			d.logger.Warn("failed to mark orphan drive ended; will retry on next restart",
				slog.String("drive_id", row.ID),
				slog.String("vin", redactVIN(row.VIN)),
				slog.String("error", err.Error()),
			)
			continue
		}
		ended++
	}

	d.logger.Info("reconciled orphaned drives",
		slog.Int("open_count", len(rows)),
		slog.Int("ended_count", ended),
	)
}
