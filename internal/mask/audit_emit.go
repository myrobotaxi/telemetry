package mask

import (
	"context"
	"log/slog"
	"time"
)

// auditInsertTimeout caps the time a fire-and-forget Emit goroutine
// will wait on an InsertAuditLog call. The hot path (BroadcastMasked
// fan-out, REST response write) MUST NOT block on this; the goroutine
// uses a detached context with this timeout so a stuck DB pool cannot
// pile up audit goroutines forever.
const auditInsertTimeout = 2 * time.Second

// EmitAsync is the fire-and-forget wrapper around AuditEmitter.
// Invariants required by the mask audit pipeline (rest-api.md §5.3,
// data-lifecycle.md §4):
//
//   - Hot-path non-blocking: failures MUST NOT drop the masked frame
//     or the REST response. EmitAsync returns immediately to the
//     caller; the actual insert runs on a spawned goroutine.
//   - Detached context: the caller's request context may be canceled
//     mid-write (e.g., the HTTP client closes the connection). We
//     use context.WithoutCancel + a 2 s timeout so the audit row
//     still lands even after the caller's context dies.
//   - Bounded latency: the 2 s timeout caps how long a stuck DB pool
//     can keep an audit goroutine alive. Beyond that we increment the
//     failure metric, log slog.Warn, and drop the row.
//   - Metric coverage: every successful insert increments
//     audit_log_writes_total{action, target}; every failure increments
//     audit_log_write_failures_total{action, target}. The labels match
//     the tuple of allow-listed enum values, so cardinality stays
//     bounded.
//
// EmitAsync is a no-op if emitter is nil — the production wiring may
// pass nil before the AuditRepo is composed (e.g., during dev mode
// when the writer is intentionally disabled), and a no-op keeps the
// hot path quiet rather than logging a flood of "audit emitter not
// configured" warnings.
func EmitAsync(
	parent context.Context,
	emitter AuditEmitter,
	metrics AuditMetrics,
	logger *slog.Logger,
	entry AuditEntry,
) {
	if emitter == nil {
		return
	}
	if metrics == nil {
		metrics = NoopAuditMetrics{}
	}
	go emitDetached(parent, emitter, metrics, logger, entry)
}

// emitDetached runs the actual insert with a detached context. Split
// out so EmitAsync remains trivial and tests can drive it directly.
func emitDetached(
	parent context.Context,
	emitter AuditEmitter,
	metrics AuditMetrics,
	logger *slog.Logger,
	entry AuditEntry,
) {
	defer func() {
		// Recover from unexpected panics inside the emitter so a buggy
		// implementation cannot crash the hot path's goroutine.
		if r := recover(); r != nil {
			metrics.IncAuditWriteFailure(entry.Action, entry.TargetType)
			if logger != nil {
				logger.Warn("mask.EmitAsync: emitter panicked",
					slog.Any("recover", r),
					slog.String("action", entry.Action),
					slog.String("target_type", entry.TargetType),
				)
			}
		}
	}()

	// Detach from the caller's request context — once we decide to
	// emit, we follow through even if the caller cancels.
	ctx := context.WithoutCancel(parent)
	ctx, cancel := context.WithTimeout(ctx, auditInsertTimeout)
	defer cancel()

	if err := emitter.InsertAuditLog(ctx, entry); err != nil {
		metrics.IncAuditWriteFailure(entry.Action, entry.TargetType)
		if logger != nil {
			logger.Warn("mask.EmitAsync: insert failed",
				slog.String("action", entry.Action),
				slog.String("target_type", entry.TargetType),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	metrics.IncAuditWrite(entry.Action, entry.TargetType)
}
