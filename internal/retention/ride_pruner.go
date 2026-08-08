package retention

import (
	"context"
	"log/slog"
	"time"
)

// The ride retention sweep (MYR-447, docs/contracts/data-lifecycle.md §5).
//
// Same cadence, budget and retry ladder as the drive sweep — literally the same
// engine (sweeper.go) — over a different table. Once a day it does two things
// to go_ride_requests, in one transaction per batch:
//
//  1. DELETES terminal rides (completed / declined / cancelled) that went
//     terminal at least 365 days ago, and
//  2. SCRUBS the two deprecated booked-for-passenger columns on terminal rides
//     older than 30 days, which is the going-forward half of MYR-447's
//     passenger-field retirement.
//
// Both windows are compile-time constants in the store layer (CG-DL-4). This
// package still governs PACE only, and still does not know either number.
//
// WHY ONE SWEEP AND NOT TWO. The scrub is not an independent job: its target
// set is a strict prefix of the delete's (same table, same terminal-status
// predicate, a shorter age), and a row that the delete claims is a row the
// scrub must NOT also touch. Running them as one batch, delete first, makes
// that ordering structural rather than a scheduling coincidence, and gives an
// operator ONE kill switch and ONE freshness gauge to reason about instead of
// two that can disagree.

// RideBatchOutcome is one ride batch's result, as this package sees it. It
// mirrors store.RidePruneBatchResult across the consumer-site interface below.
type RideBatchOutcome struct {
	// RidesDeleted is how many go_ride_requests rows the batch destroyed.
	RidesDeleted int
	// PassengersScrubbed is how many surviving rows had their passenger_name
	// and passenger_phone columns NULLed. Disjoint from RidesDeleted by
	// construction: the delete runs first, so a scrubbed row is one that
	// outlived it.
	PassengersScrubbed int
	// AuditRows is how many rides_pruned + ride_passengers_scrubbed audit
	// entries it wrote.
	AuditRows int
	// Exhausted is true only when BOTH claims found nothing left to do. The
	// pass loop's terminator. A batch that merely came back SHORT does not
	// qualify — under the store's SKIP LOCKED claims that means "no rows I can
	// take", which may be a peer holding the rest, so the loop keeps going and
	// stops on a genuinely empty pair of claims.
	Exhausted bool
}

// RideStore is the ride sweep's view of persistence. Declared here, at the
// consumer site, and satisfied by an adapter in cmd/ so this package never
// imports internal/store.
type RideStore interface {
	// PruneBatch deletes up to batchSize expired terminal rides and scrubs up
	// to batchSize passenger records, atomically and with audit rows, and
	// reports what it did.
	PruneBatch(ctx context.Context, batchSize int) (RideBatchOutcome, error)
}

// RideMetrics is the ride sweep's observability surface. Separate from Metrics
// because the row counts differ: a ride batch reports two of them, and they
// must not land on the same counter.
//
// The four pass-level methods are identical to Metrics' by design — that is
// what lets the shared engine accept either without an adapter.
type RideMetrics interface {
	// AddRidesDeleted records ride rows destroyed by one batch.
	AddRidesDeleted(n int)
	// AddPassengersScrubbed records rows whose passenger columns one batch NULLed.
	AddPassengersScrubbed(n int)
	// IncBatch records one successfully committed batch.
	IncBatch()
	// IncBatchError records one batch that failed all its attempts.
	IncBatchError()
	// ObserveRunDuration records the wall-clock time of one pass.
	ObserveRunDuration(d time.Duration)
	// SetLastSuccess records when a pass last finished without a batch error.
	SetLastSuccess(t time.Time)
}

// NoopRideMetrics discards every observation. The default when no registry is
// wired.
type NoopRideMetrics struct{}

func (NoopRideMetrics) AddRidesDeleted(int)              {}
func (NoopRideMetrics) AddPassengersScrubbed(int)        {}
func (NoopRideMetrics) IncBatch()                        {}
func (NoopRideMetrics) IncBatchError()                   {}
func (NoopRideMetrics) ObserveRunDuration(time.Duration) {}
func (NoopRideMetrics) SetLastSuccess(time.Time)         {}

// rideSweepLabels names the ride sweep in every log line it emits, distinctly
// enough from the drive sweep's that a log search for one never returns the
// other.
var rideSweepLabels = sweepLabels{subject: "ride retention pruner", noun: "rides"}

// RidePruner runs the daily ride retention sweep.
type RidePruner struct {
	*sweeper[RideBatchOutcome]
}

// NewRidePruner builds the sweep over a store adapter.
func NewRidePruner(rides RideStore, cfg Config, metrics RideMetrics, logger *slog.Logger) *RidePruner {
	if metrics == nil {
		metrics = NoopRideMetrics{}
	}
	fns := sweepFuncs[RideBatchOutcome]{
		facts: func(o RideBatchOutcome) batchFacts {
			// rows is the DELETE count only. The scrub count is reported to the
			// metrics below but deliberately kept out of the loop's tally: the
			// pass's "did I make progress" story is about rows destroyed, and a
			// scrub-only batch is already non-exhausted on its own claim.
			return batchFacts{rows: o.RidesDeleted, audits: o.AuditRows, exhausted: o.Exhausted}
		},
		record: func(o RideBatchOutcome) {
			metrics.AddRidesDeleted(o.RidesDeleted)
			metrics.AddPassengersScrubbed(o.PassengersScrubbed)
		},
	}
	// Left nil when no store is wired — see the same note on NewPruner.
	if rides != nil {
		fns.prune = rides.PruneBatch
	}
	return &RidePruner{sweeper: newSweeper(rideSweepLabels, cfg, metrics, logger, fns)}
}
