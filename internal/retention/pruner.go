package retention

import (
	"context"
	"log/slog"
)

// The drive retention sweep (MYR-439, NFR-3.27, docs/contracts/
// data-lifecycle.md §5).
//
// Once a day it deletes every drive older than the retention window, in
// bounded batches, writing an audit row for each batch before destroying it.
// The window itself lives in the store layer as a compile-time constant; this
// package owns only the cadence, the per-pass budget and the retries.
//
// Shape follows the MYR-172 Live Activity ticker and the MYR-320 in-service
// re-poll — a timer loop, a kill-switch, an exported RunPass so tests need no
// sleeps, and a pass that yields to context cancellation at every boundary —
// because that is this service's established answer to "run something
// periodically", and a third shape would be a third set of operational
// surprises. It differs in one respect: the wait is to a wall-clock time of day
// rather than an interval, because §5.2 specifies 03:00 UTC.
//
// SINCE MYR-447 that loop lives in sweeper.go and is shared with the ride
// retention sweep. Nothing below changed shape or behaviour: Pruner embeds the
// engine, so Run / RunPass / untilNextRun and the injectable clock are exactly
// the surface they were, and this file is now only the drive-specific bindings
// — the outcome type, the store interface, and which metric a row count feeds.

// BatchOutcome is one batch's result, as this package sees it. It mirrors
// store.PruneBatchResult across the consumer-site interface below.
type BatchOutcome struct {
	// DrivesDeleted is how many drive rows the batch destroyed.
	DrivesDeleted int
	// AuditRows is how many drives_pruned audit entries it wrote.
	AuditRows int
	// Exhausted is true only when the claim found nothing left to delete. The
	// pass loop's terminator. A batch that merely came back SHORT does not
	// qualify — under the store's SKIP LOCKED claim that means "no rows I can
	// take", which may be a peer holding the rest, so the loop keeps going and
	// stops on a genuinely empty claim.
	Exhausted bool
}

// DriveStore is the sweep's view of persistence. Declared here, at the consumer
// site, and satisfied by an adapter in cmd/ so this package never imports
// internal/store.
type DriveStore interface {
	// PruneBatch deletes up to batchSize drives past the retention window,
	// atomically and with an audit row, and reports what it destroyed.
	PruneBatch(ctx context.Context, batchSize int) (BatchOutcome, error)
}

// driveSweepLabels names the drive sweep in every log line it emits. The
// strings are the ones MYR-439 shipped, kept verbatim so existing log-based
// alerts and runbooks still match.
var driveSweepLabels = sweepLabels{subject: "drive retention pruner", noun: "drives"}

// Pruner runs the daily drive retention sweep.
type Pruner struct {
	*sweeper[BatchOutcome]
}

// NewPruner builds the sweep over a store adapter.
func NewPruner(drives DriveStore, cfg Config, metrics Metrics, logger *slog.Logger) *Pruner {
	if metrics == nil {
		metrics = NoopMetrics{}
	}
	fns := sweepFuncs[BatchOutcome]{
		facts: func(o BatchOutcome) batchFacts {
			return batchFacts{rows: o.DrivesDeleted, audits: o.AuditRows, exhausted: o.Exhausted}
		},
		record: func(o BatchOutcome) { metrics.AddDrivesDeleted(o.DrivesDeleted) },
	}
	// Left nil when no store is wired, which is what Run checks: binding the
	// method value of a nil interface would produce a non-nil func that panics
	// on the first pass instead of the clean "not wired" exit.
	if drives != nil {
		fns.prune = drives.PruneBatch
	}
	return &Pruner{sweeper: newSweeper(driveSweepLabels, cfg, metrics, logger, fns)}
}
