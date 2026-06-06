// Orphan-drive reconciliation. Called from Detector.Start.
//
// MYR-146: a Fly redeploy mid-drive restarts the machine. The
// Detector's in-memory vehicleState map is lost; Tesla resumes
// streaming on a new connection; the state machine sees no prior
// state and either starts a fresh drive (creating a second "active"
// Drive row in the DB) or — if the vehicle has parked in the
// meantime — never emits a DriveEndedEvent for the orphaned row.
//
// Reconciliation closes the gap: on Start, the Detector reads every
// open Drive row from the DB and recreates a vehicleState entry keyed
// by VIN with status=StatusDriving. From there two outcomes are
// possible:
//
//   1. Telemetry resumes for the same VIN before EndDebounce elapses.
//      The state machine treats the new frames as the continuation of
//      the reconciled drive — same DriveID, same row updated, no
//      duplicate. This is the survival path Option B promises.
//
//   2. Telemetry never resumes (the car parked while the deploy was
//      in flight). The watchdog observes the silence and ends the
//      drive within ~EndDebounce, publishing a DriveEndedEvent that
//      the writer handles via the normal Complete path.

package drives

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// OpenDriveRow mirrors the slim projection returned by the store
// adapter wired in from cmd/. Defined at the consumer site so the
// drives package stays independent of internal/store (the cmd-layer
// adapter performs the row-shape translation).
type OpenDriveRow struct {
	ID               string
	VIN              string
	StartTime        string // ISO 8601; parsed best-effort below
	StartChargeLevel int
}

// OpenDriveLister is the read-side dependency the Detector consults on
// Start to discover Drive rows orphaned by a previous restart. A nil
// implementation skips reconciliation entirely — used by unit tests
// and by any future runtime that doesn't persist drives.
type OpenDriveLister interface {
	ListOpen(ctx context.Context) ([]OpenDriveRow, error)
}

// reconcileOpenDrives reads every open Drive row from the lister and
// reattaches each to in-memory vehicleState. It is best-effort: a DB
// hiccup on Start MUST NOT block telemetry processing — we log Warn
// and continue. Worst case the orphaned rows stay open until the next
// successful restart cycle.
//
// Called from Start AFTER bus subscription succeeds but BEFORE the
// watchdog goroutine starts, so the very first watchdog tick observes
// the reconciled state.
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

	now := d.now()
	reconciled := 0
	for _, row := range rows {
		if row.VIN == "" || row.ID == "" {
			continue
		}
		startedAt, parseErr := parseDriveStartTime(row.StartTime)
		if parseErr != nil {
			// Fall back to "now" so the watchdog still has a sane
			// reference and the row gets closed on next idle.
			startedAt = now
			d.logger.Warn("reconciled drive has unparseable startTime; using current time",
				slog.String("drive_id", row.ID),
				slog.String("start_time", row.StartTime),
				slog.String("error", parseErr.Error()),
			)
		}

		state := &vehicleState{
			status:          StatusDriving,
			lastTelemetryAt: now,
			drive: &activeDrive{
				id:            row.ID,
				startedAt:     startedAt,
				startCharge:   float64(row.StartChargeLevel),
				lastSOC:       float64(row.StartChargeLevel),
				lastTimestamp: startedAt,
			},
		}
		// Use Store (not LoadOrStore) — Start runs before any
		// telemetry handlers, so the map is empty and overwriting any
		// concurrent insert would be a bug, not a race we tolerate.
		// In practice no other goroutine touches d.states until
		// Subscribe callbacks fire, which happens after Start returns.
		d.states.Store(row.VIN, state)
		d.activeCount.Add(1)
		reconciled++
	}

	d.metrics.SetActiveVehicles(int(d.activeCount.Load()))

	d.logger.Info("reconciled orphaned drives",
		slog.Int("reconciled_count", reconciled),
	)
}

// parseDriveStartTime parses the Prisma-stored startTime string. The
// column is TEXT, written by mapDriveStarted with time.RFC3339, but we
// accept the alternate "Z"-less form and the broader RFC3339Nano shape
// so a manually-edited row is still recoverable.
func parseDriveStartTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errEmptyStartTime
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	// Last-resort permissive parse.
	t, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parseDriveStartTime(%q): %w", s, err)
	}
	return t, nil
}

// errEmptyStartTime is the sentinel returned by parseDriveStartTime
// for the empty-string input. The reconciler logs and falls back to
// now() — no caller currently needs to branch on it.
var errEmptyStartTime = &reconcileError{msg: "empty startTime"}

// reconcileError is a small named error type so the reconciler's
// fallback path produces a stable message without dragging in
// fmt.Errorf at the cost of an alloc per row.
type reconcileError struct{ msg string }

func (e *reconcileError) Error() string { return e.msg }
