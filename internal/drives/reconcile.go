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
//
// MYR-147: MarkOrphanEnded closes an open Drive row immediately
// (endTime = now, zero stats). The reconciler invokes it for the
// OLDER orphans in a same-VIN group — those drives are definitively
// over because a newer open drive exists for the same VIN, so there
// is nothing left for the watchdog to do. The most recent orphan per
// VIN still flows through the in-memory reattach + watchdog-end path.
type OpenDriveLister interface {
	ListOpen(ctx context.Context) ([]OpenDriveRow, error)
	MarkOrphanEnded(ctx context.Context, driveID string) error
}

// reconcileOpenDrives reads every open Drive row from the lister and
// reattaches each to in-memory vehicleState. It is best-effort: a DB
// hiccup on Start MUST NOT block telemetry processing — we log Warn
// and continue. Worst case the orphaned rows stay open until the next
// successful restart cycle.
//
// Called from Start BEFORE bus subscription so no handler callback
// goroutine can race the d.states.Store calls below, and BEFORE the
// watchdog goroutine starts so the first watchdog tick observes the
// reconciled state.
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

	groups := groupOrphansByVIN(rows)

	now := d.now()
	reconciled := 0
	ended := 0
	for vin, g := range groups {
		// In-memory reattach for the MOST RECENT orphan per VIN.
		// The watchdog (or resumed telemetry) closes this one.
		d.reattachOrphan(vin, g.latest, now)
		reconciled++

		// Close OLDER same-VIN orphans synchronously. They pre-date
		// the most recent open drive for the same VIN, so by
		// definition no telemetry will ever resume on them — the
		// watchdog path that the reattached drive uses cannot apply
		// here (only one in-memory entry per VIN). Without this,
		// MYR-146's reconciler silently overwrote prior d.states
		// entries and the older rows stayed open forever.
		for i := range g.older {
			row := &g.older[i]
			if err := d.reconciler.MarkOrphanEnded(d.ctx, row.ID); err != nil {
				// Fail open, same posture as ListOpen. The row stays
				// open and will be retried on next restart.
				d.logger.Warn("failed to mark older same-VIN orphan ended; will retry on next restart",
					slog.String("drive_id", row.ID),
					slog.String("error", err.Error()),
				)
				continue
			}
			ended++
		}
	}

	d.metrics.SetActiveVehicles(int(d.activeCount.Load()))

	d.logger.Info("reconciled orphaned drives",
		slog.Int("reconciled_count", reconciled),
		slog.Int("ended_count", ended),
	)
}

// reattachOrphan installs in-memory vehicleState for a single orphan
// row and increments the active-vehicle counter. Extracted from
// reconcileOpenDrives so the same-VIN grouping path can call it once
// per group without duplicating the construction.
//
// The caller guarantees row.VIN and row.ID are non-empty (filtered in
// groupOrphansByVIN).
func (d *Detector) reattachOrphan(vin string, row *OpenDriveRow, now time.Time) {
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
			// reconciled tags the watchdog-end path to bypass the
			// micro-drive filter (MYR-146 critical). With no
			// resumed telemetry the stats are Duration=0,
			// Distance=0 and the prod-default filter
			// (MinDuration=2m, MinDistanceMiles=0.1) would discard
			// the DriveEndedEvent -- leaving the DB row open and
			// reconciled again on the next restart forever.
			// Cleared in handleDriving once real telemetry
			// resumes; from that point the drive is treated as a
			// normal continuation and any subsequent end goes
			// through the standard filter.
			reconciled: true,
		},
	}
	// Use Store (not LoadOrStore) — reconcileOpenDrives runs
	// BEFORE Start calls Subscribe (see Start in detector.go), so
	// no handler goroutine can touch d.states yet. Overwriting any
	// pre-existing entry would be a bug, not a race we tolerate.
	d.states.Store(vin, state)
	d.activeCount.Add(1)
}

// orphanGroup partitions the open-drive rows for a single VIN into
// the most recent (latest) and everything older. The latest goes
// through the in-memory reattach path; the older entries are closed
// synchronously via MarkOrphanEnded.
//
// MYR-147: prod confirmed 3 simultaneously-open Drive rows for the
// same VIN. The MYR-146 reconciler stored them all under d.states
// keyed by VIN and the last write won — earlier rows were silently
// dropped and never closed.
type orphanGroup struct {
	latest *OpenDriveRow
	older  []OpenDriveRow
}

// groupOrphansByVIN partitions the open-drive rows by VIN and, within
// each VIN, picks the lexicographically-greatest StartTime as the
// "latest" orphan. Empty VIN/ID rows are filtered out (matches the
// pre-grouping guard in the original loop).
//
// String comparison on StartTime is safe because every value is RFC3339
// (YYYY-MM-DDTHH:MM:SSZ), which is lexicographically sortable. If a
// future writer ever stores a non-Z offset we'd want to parse first,
// but the only producer today is mapDriveStarted which always emits Z.
func groupOrphansByVIN(rows []OpenDriveRow) map[string]*orphanGroup {
	groups := make(map[string]*orphanGroup)
	for i := range rows {
		row := rows[i]
		if row.VIN == "" || row.ID == "" {
			continue
		}
		g, ok := groups[row.VIN]
		if !ok {
			g = &orphanGroup{}
			groups[row.VIN] = g
		}
		if g.latest == nil {
			// First row for this VIN — promote unconditionally.
			r := row
			g.latest = &r
			continue
		}
		if row.StartTime > g.latest.StartTime {
			// Newer than the current latest — demote the old latest.
			g.older = append(g.older, *g.latest)
			r := row
			g.latest = &r
			continue
		}
		g.older = append(g.older, row)
	}
	return groups
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
