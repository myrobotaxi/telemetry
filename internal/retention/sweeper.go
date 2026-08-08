package retention

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// The shared retention-sweep engine (MYR-439 drives, MYR-447 rides).
//
// WHY THIS EXISTS. MYR-439 shipped one sweep — Pruner, over drives — and every
// property that made it correct is a property of PACE, not of drives: a daily
// wall-clock slot with jitter, a per-pass batch budget, a bounded retry ladder,
// and a pass that yields to context cancellation at every boundary. MYR-447
// needed the identical pace over a different table. Copying the loop would have
// produced a second set of these decisions to keep in sync, and the file-level
// consts here (defaultRunAtUTC, pruneAttempts, runJitter, …) are package-scoped,
// so a second Pruner type in this package could not even have had its own.
//
// So the loop moved down here, unchanged, and each sweep is now a thin typed
// facade over it: Pruner (drives) in pruner.go, RidePruner (rides) in
// ride_pruner.go. Both embed *sweeper, so their exported surface — Run, RunPass,
// the injectable `now` clock — is exactly what it was.
//
// It is generic over the batch outcome T rather than over a single shared
// outcome struct because the two sweeps count different things: a drive batch
// reports drives deleted, a ride batch reports rides deleted AND passenger
// fields scrubbed. Collapsing those onto one "rowsAffected" would have thrown
// away the second count before the metrics saw it. The engine itself only ever
// needs three numbers out of T (see batchFacts), which is what `facts` extracts;
// `record` hands the WHOLE outcome to that sweep's own metrics.
//
// This package still knows no retention window. Both windows are compile-time
// constants in the store layer (CG-DL-4). Everything here governs PACE, never
// SCOPE.

// batchFacts is the loop-relevant projection of one batch outcome: the only
// three things the engine itself has to reason about, whatever T is.
type batchFacts struct {
	// rows is how many rows the batch destroyed or rewrote, for the pass tally.
	rows int
	// audits is how many audit entries it wrote, for the pass tally.
	audits int
	// exhausted is true only when the claim came back EMPTY. See the per-sweep
	// outcome types for why a merely short batch does not qualify.
	exhausted bool
}

// sweepLabels is how one sweep names itself in logs. Two fields, because every
// line is either "<subject> did X" or "<noun>_deleted=N".
type sweepLabels struct {
	// subject is the sweep's name in prose: "drive retention pruner".
	subject string
	// noun is the plural row noun for log phrases and keys: "drives".
	noun string
}

// passMetrics is the part of a sweep's observability surface the engine drives
// on its own — everything that counts PASSES and BATCHES rather than rows.
//
// It is deliberately unexported and satisfied structurally: both Metrics and
// RideMetrics declare these four methods, so each sweep's own exported metrics
// interface is accepted here without an adapter type. Row counts are NOT here,
// because their signatures are what differs between sweeps; they travel through
// sweepFuncs.record instead.
type passMetrics interface {
	IncBatch()
	IncBatchError()
	ObserveRunDuration(d time.Duration)
	SetLastSuccess(t time.Time)
}

// sweepFuncs binds the engine to one concrete sweep.
type sweepFuncs[T any] struct {
	// prune runs one batch. NIL means the sweep is not wired, which Run reports
	// and exits on rather than spinning.
	prune func(ctx context.Context, batchSize int) (T, error)
	// facts projects an outcome onto what the loop needs.
	facts func(T) batchFacts
	// record hands the whole outcome to that sweep's row-count metrics.
	record func(T)
}

// sweeper is the daily batched sweep: the cadence, the per-pass budget and the
// retries. Constructed only via newSweeper.
type sweeper[T any] struct {
	labels  sweepLabels
	cfg     Config
	metrics passMetrics
	logger  *slog.Logger
	fns     sweepFuncs[T]

	// now is the clock, injectable so tests drive the schedule deterministically.
	now func() time.Time
}

// newSweeper builds the engine. Callers are the per-sweep constructors, which
// have already defaulted metrics to their own no-op implementation.
func newSweeper[T any](
	labels sweepLabels,
	cfg Config,
	metrics passMetrics,
	logger *slog.Logger,
	fns sweepFuncs[T],
) *sweeper[T] {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &sweeper[T]{
		labels:  labels,
		cfg:     cfg.withDefaults(),
		metrics: metrics,
		logger:  logger,
		fns:     fns,
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
func (s *sweeper[T]) Run(ctx context.Context) {
	if s.fns.prune == nil {
		s.logger.Info(s.labels.subject + " not wired")
		return
	}
	if !s.cfg.Enabled {
		s.logger.Warn(s.labels.subject + " disabled — " + s.labels.noun +
			" will accumulate past the retention window")
		return
	}

	// NOTE the absence of a retention_days field here. This package does not
	// know the window and must not: it lives as one compile-time constant in
	// the store layer (CG-DL-4), and the store logs it on every batch. A copy
	// here for the sake of a nicer startup line would be a second place for it
	// to be wrong.
	s.logger.Info(s.labels.subject+" started",
		slog.Duration("run_at_utc", s.cfg.RunAtUTC),
		slog.Int("batch_size", s.cfg.BatchSize),
		slog.Int("max_batches_per_pass", s.cfg.MaxBatchesPerPass),
		slog.Duration("batch_timeout", s.cfg.BatchTimeout))

	for {
		timer := time.NewTimer(s.untilNextRun(s.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.logger.Info(s.labels.subject + " stopped")
			return
		case <-timer.C:
		}
		s.RunPass(ctx)
	}
}

// untilNextRun is how long to wait for the next daily slot, jittered.
func (s *sweeper[T]) untilNextRun(now time.Time) time.Duration {
	utc := now.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	next := midnight.Add(s.cfg.RunAtUTC)
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
// whole, and the claim query always re-reads the oldest eligible rows, so a
// pass that stops early leaves no partial state — the next pass simply finds
// the rest. That is also why shutdown needs no drain: the correct response to
// cancellation is to stop at the next batch boundary, not to finish.
//
// Exported so tests drive a pass deterministically instead of racing a timer.
func (s *sweeper[T]) RunPass(ctx context.Context) {
	start := s.now()
	var affected, audits, batches int

	for batches < s.cfg.MaxBatchesPerPass {
		if ctx.Err() != nil {
			s.finish(start, affected, batches, "shutdown")
			return
		}

		out, err := s.pruneOneBatch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.finish(start, affected, batches, "shutdown")
				return
			}
			s.metrics.IncBatchError()
			// §5.6 says "skip to next batch", but the claim is deterministic
			// (always the oldest eligible rows), so a "next batch" after a
			// failure would be the SAME batch — skipping is not expressible and
			// retrying in-pass would spin. Ending the pass is the honest
			// version of the same intent: the failed batch is retried on the
			// next daily run, which the sweep's idempotence makes free.
			s.logger.Error(s.labels.subject+": batch failed, ending pass",
				slog.Int("batches_completed", batches),
				slog.Int(s.labels.noun+"_deleted", affected),
				slog.String("error", err.Error()))
			s.finish(start, affected, batches, "error")
			return
		}

		f := s.fns.facts(out)
		batches++
		affected += f.rows
		audits += f.audits
		s.metrics.IncBatch()
		s.fns.record(out)

		if f.exhausted {
			s.metrics.SetLastSuccess(s.now())
			if affected > 0 {
				s.logger.Info(s.labels.subject+": pass complete",
					slog.Int(s.labels.noun+"_deleted", affected),
					slog.Int("audit_rows", audits),
					slog.Int("batches", batches))
			}
			s.finish(start, affected, batches, "complete")
			return
		}
	}

	// Budget spent with work still outstanding. Not an error — the backlog
	// drains over successive nights — but an operator should know, because it
	// is the one condition under which raising MaxBatchesPerPass is the answer.
	s.metrics.SetLastSuccess(s.now())
	s.logger.Warn(s.labels.subject+": pass hit its batch budget with "+s.labels.noun+" still eligible",
		slog.Int(s.labels.noun+"_deleted", affected),
		slog.Int("audit_rows", audits),
		slog.Int("batches", batches),
		slog.Int("max_batches_per_pass", s.cfg.MaxBatchesPerPass))
	s.finish(start, affected, batches, "budget_exhausted")
}

// finish records the run-duration observation exactly once per pass.
func (s *sweeper[T]) finish(start time.Time, affected, batches int, outcome string) {
	s.metrics.ObserveRunDuration(s.now().Sub(start))
	s.logger.Debug(s.labels.subject+": pass finished",
		slog.String("outcome", outcome),
		slog.Int(s.labels.noun+"_deleted", affected),
		slog.Int("batches", batches))
}

// pruneOneBatch runs one batch with the §5.6 retry ladder: three attempts,
// waiting 1s then 2s between them. A batch is a single transaction, so a failed
// attempt has deleted nothing and a retry is a clean repeat rather than a
// resume.
func (s *sweeper[T]) pruneOneBatch(ctx context.Context) (T, error) {
	var zero T
	var lastErr error
	backoff := pruneBackoffBase

	for attempt := 1; attempt <= pruneAttempts; attempt++ {
		if attempt > 1 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, fmt.Errorf("retention.PruneBatch: cancelled during backoff: %w", ctx.Err())
			case <-timer.C:
			}
			backoff *= 2
		}

		batchCtx, cancel := context.WithTimeout(ctx, s.cfg.BatchTimeout)
		out, err := s.fns.prune(batchCtx, s.cfg.BatchSize)
		cancel()
		if err == nil {
			return out, nil
		}
		lastErr = err

		// A cancelled parent is shutdown, not a fault — do not burn retries on it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, fmt.Errorf("retention.PruneBatch: cancelled: %w", ctxErr)
		}
		s.logger.Warn(s.labels.subject+": batch attempt failed",
			slog.Int("attempt", attempt),
			slog.Int("of", pruneAttempts),
			slog.String("error", err.Error()))
	}

	return zero, fmt.Errorf("retention.PruneBatch: %d attempts failed: %w", pruneAttempts, lastErr)
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
