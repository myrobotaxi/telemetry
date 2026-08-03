package retention

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
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

// Pruner runs the daily drive retention sweep.
type Pruner struct {
	drives  DriveStore
	cfg     Config
	metrics Metrics
	logger  *slog.Logger

	// now is the clock, injectable so tests drive the schedule deterministically.
	now func() time.Time
}

// NewPruner builds the sweep over a store adapter.
func NewPruner(drives DriveStore, cfg Config, metrics Metrics, logger *slog.Logger) *Pruner {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	if metrics == nil {
		metrics = NoopMetrics{}
	}
	return &Pruner{
		drives:  drives,
		cfg:     cfg.withDefaults(),
		metrics: metrics,
		logger:  logger,
		now:     time.Now,
	}
}

// Run drives the sweep until ctx is cancelled. Started as its own goroutine by
// cmd/ wiring.
//
// Shutdown is context-only, which is the idiom main uses for every timer-driven
// worker (the LA ticker, the reservation sweeper, the cert monitors): this type
// spawns no goroutines of its own, so there is nothing to join. What shutdown
// has to be safe against is a pass in flight, and it is — see RunPass.
func (p *Pruner) Run(ctx context.Context) {
	if p.drives == nil {
		p.logger.Info("drive retention pruner not wired")
		return
	}
	if !p.cfg.Enabled {
		p.logger.Warn("drive retention pruner disabled — drives will accumulate past the retention window")
		return
	}

	// NOTE the absence of a retention_days field here. This package does not
	// know the window and must not: it lives as one compile-time constant in
	// the store layer (CG-DL-4), and the store logs it on every batch. A copy
	// here for the sake of a nicer startup line would be a second place for it
	// to be wrong.
	p.logger.Info("drive retention pruner started",
		slog.Duration("run_at_utc", p.cfg.RunAtUTC),
		slog.Int("batch_size", p.cfg.BatchSize),
		slog.Int("max_batches_per_pass", p.cfg.MaxBatchesPerPass),
		slog.Duration("batch_timeout", p.cfg.BatchTimeout))

	for {
		timer := time.NewTimer(p.untilNextRun(p.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			p.logger.Info("drive retention pruner stopped")
			return
		case <-timer.C:
		}
		p.RunPass(ctx)
	}
}

// untilNextRun is how long to wait for the next daily slot, jittered.
func (p *Pruner) untilNextRun(now time.Time) time.Duration {
	utc := now.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	next := midnight.Add(p.cfg.RunAtUTC)
	if !next.After(utc) {
		next = next.AddDate(0, 0, 1)
	}
	// #nosec G404 -- not a security decision. The only requirement on this
	// number is that two replicas disagree about when to wake up; a
	// cryptographic source would buy nothing.
	spread := time.Duration(rand.Int64N(int64(runJitter))) //nolint:gosec // G404 — see the #nosec rationale above
	return next.Sub(utc) + spread
}

// RunPass performs ONE sweep: batch after batch until the backlog is exhausted,
// the per-pass budget is spent, a batch fails its retries, or shutdown arrives.
//
// EVERY EXIT IS SAFE TO RESUME FROM, which is what lets the pass stop anywhere.
// Each batch is its own transaction that either commits whole or rolls back
// whole, and the claim query always re-reads the oldest expired drives, so a
// pass that stops early leaves no partial state — the next pass simply finds
// the rest. That is also why shutdown needs no drain: the correct response to
// cancellation is to stop at the next batch boundary, not to finish.
//
// Exported so tests drive a pass deterministically instead of racing a timer.
func (p *Pruner) RunPass(ctx context.Context) {
	start := p.now()
	var deleted, audits, batches int

	for batches < p.cfg.MaxBatchesPerPass {
		if ctx.Err() != nil {
			p.finish(start, deleted, batches, "shutdown")
			return
		}

		out, err := p.pruneOneBatch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				p.finish(start, deleted, batches, "shutdown")
				return
			}
			p.metrics.IncBatchError()
			// §5.6 says "skip to next batch", but the claim is deterministic
			// (always the oldest expired rows), so a "next batch" after a
			// failure would be the SAME batch — skipping is not expressible and
			// retrying in-pass would spin. Ending the pass is the honest
			// version of the same intent: the failed batch is retried on the
			// next daily run, which the sweep's idempotence makes free.
			p.logger.Error("drive retention pruner: batch failed, ending pass",
				slog.Int("batches_completed", batches),
				slog.Int("drives_deleted", deleted),
				slog.String("error", err.Error()))
			p.finish(start, deleted, batches, "error")
			return
		}

		batches++
		deleted += out.DrivesDeleted
		audits += out.AuditRows
		p.metrics.IncBatch()
		p.metrics.AddDrivesDeleted(out.DrivesDeleted)

		if out.Exhausted {
			p.metrics.SetLastSuccess(p.now())
			if deleted > 0 {
				p.logger.Info("drive retention pruner: pass complete",
					slog.Int("drives_deleted", deleted),
					slog.Int("audit_rows", audits),
					slog.Int("batches", batches))
			}
			p.finish(start, deleted, batches, "complete")
			return
		}
	}

	// Budget spent with work still outstanding. Not an error — the backlog
	// drains over successive nights — but an operator should know, because it
	// is the one condition under which raising MaxBatchesPerPass is the answer.
	p.metrics.SetLastSuccess(p.now())
	p.logger.Warn("drive retention pruner: pass hit its batch budget with drives still eligible",
		slog.Int("drives_deleted", deleted),
		slog.Int("audit_rows", audits),
		slog.Int("batches", batches),
		slog.Int("max_batches_per_pass", p.cfg.MaxBatchesPerPass))
	p.finish(start, deleted, batches, "budget_exhausted")
}

// finish records the run-duration observation exactly once per pass.
func (p *Pruner) finish(start time.Time, deleted, batches int, outcome string) {
	p.metrics.ObserveRunDuration(p.now().Sub(start))
	p.logger.Debug("drive retention pruner: pass finished",
		slog.String("outcome", outcome),
		slog.Int("drives_deleted", deleted),
		slog.Int("batches", batches))
}

// pruneOneBatch runs one batch with the §5.6 retry ladder: three attempts,
// waiting 1s then 2s between them. A batch is a single transaction, so a failed
// attempt has deleted nothing and a retry is a clean repeat rather than a
// resume.
func (p *Pruner) pruneOneBatch(ctx context.Context) (BatchOutcome, error) {
	var lastErr error
	backoff := pruneBackoffBase

	for attempt := 1; attempt <= pruneAttempts; attempt++ {
		if attempt > 1 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return BatchOutcome{}, fmt.Errorf("retention.PruneBatch: cancelled during backoff: %w", ctx.Err())
			case <-timer.C:
			}
			backoff *= 2
		}

		batchCtx, cancel := context.WithTimeout(ctx, p.cfg.BatchTimeout)
		out, err := p.drives.PruneBatch(batchCtx, p.cfg.BatchSize)
		cancel()
		if err == nil {
			return out, nil
		}
		lastErr = err

		// A cancelled parent is shutdown, not a fault — do not burn retries on it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return BatchOutcome{}, fmt.Errorf("retention.PruneBatch: cancelled: %w", ctxErr)
		}
		p.logger.Warn("drive retention pruner: batch attempt failed",
			slog.Int("attempt", attempt),
			slog.Int("of", pruneAttempts),
			slog.String("error", err.Error()))
	}

	return BatchOutcome{}, fmt.Errorf("retention.PruneBatch: %d attempts failed: %w", pruneAttempts, lastErr)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
