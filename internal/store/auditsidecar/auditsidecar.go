// Package auditsidecar provides a best-effort, forward-only S3 mirror for
// AuditLog entries written by the Go telemetry server. It is a defence-in-
// depth companion to the Prisma-owned AuditLog table and NEVER replaces it:
//
//   - The PostgreSQL AuditLog table is the canonical, authoritative source of
//     truth (NFR-3.29). The sidecar is a forward-only mirror whose only purpose
//     is to give the backup-retention runbook a second verification point.
//
//   - Delivery is best-effort, at-most-once. The DB INSERT always completes
//     before the sidecar write is attempted. If the sidecar is slow, full, or
//     unreachable the DB write still succeeds — the sidecar never becomes a
//     single point of failure.
//
//   - The sidecar starts mirroring at the moment the telemetry server process
//     starts with AUDIT_SIDECAR_BUCKET set. Rows written before that deploy
//     date are NOT backfilled. The backup-retention runbook §2 documents this
//     explicitly: for restores predating the sidecar deploy date, operators
//     run STEP 1 against the DB AuditLog directly.
//
// Async write model (to protect DB INSERT latency):
//
//  1. InsertAuditLog calls Emit(ctx, entry) immediately after a successful DB
//     INSERT. Emit is non-blocking: it enqueues the entry into a bounded
//     in-process channel (default capacity 10 000).
//  2. A single worker goroutine drains the channel and issues PutObject calls
//     to S3 with exponential back-off (up to 3 attempts per entry).
//  3. On permanent failure the entry is dropped, a counter is incremented, and
//     slog.Warn is emitted. The worker then moves on to the next entry.
//  4. On queue-full (channel at capacity) the entry is dropped immediately,
//     a counter is incremented, and the DB INSERT is not blocked.
//
// Object key schema:
//
//	audit/v1/{yyyy}/{mm}/{dd}/{userId}/{timestamp_unix_nanos}-{auditLogId}.json
//
// Feature flag:
//
//	AUDIT_SIDECAR_BUCKET — if empty, the sidecar is a no-op (local dev).
//	AUDIT_SIDECAR_REGION — S3 region (default us-east-1).
package auditsidecar

import (
	"encoding/json"
	"time"
)

// AuditEntry is the sidecar's view of an audit row. It mirrors
// store.AuditEntry field-for-field so the caller can copy without an
// import cycle. (store imports auditsidecar; auditsidecar must not import
// store.)
type AuditEntry struct {
	ID         string
	UserID     string
	Timestamp  time.Time
	Action     string
	TargetType string
	TargetID   string
	Initiator  string
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

// Sidecar is the interface that AuditRepo depends on for forward-only
// mirroring. Callers invoke Emit immediately after a successful DB INSERT;
// Emit MUST NOT block on I/O — it may only enqueue.
//
// A non-nil error from Emit means the entry was not queued (e.g., channel
// full). The caller MUST log the error and increment a counter but MUST NOT
// fail the outer DB operation. The sidecar is never a hard dependency.
type Sidecar interface {
	// Emit enqueues an AuditEntry for async S3 upload. Returns ErrQueueFull
	// if the internal queue has reached capacity; the caller must not block
	// waiting for capacity. ctx is used only for cancellation of the enqueue
	// attempt itself, not the eventual S3 write.
	Emit(entry AuditEntry) error
}

// NoopSidecar is the zero-allocation default. Use it when
// AUDIT_SIDECAR_BUCKET is unset (local dev) or in unit tests that don't
// need S3. Every method is a no-op.
type NoopSidecar struct{}

// Emit is a no-op.
func (NoopSidecar) Emit(_ AuditEntry) error { return nil }

// ErrQueueFull is returned by S3Sidecar.Emit when the bounded internal
// channel is at capacity.
var ErrQueueFull = sidecarError("audit_sidecar: queue full — entry dropped")

// ErrSidecarClosed is returned by S3Sidecar.Emit after Close has been
// called. Callers must handle it the same way as ErrQueueFull (log,
// bump a metric, NEVER fail the upstream caller). Distinguishing it
// from ErrQueueFull lets operators tell "drop because we're shutting
// down" from "drop because we can't keep up" via metrics.
var ErrSidecarClosed = sidecarError("audit_sidecar: closed — entry dropped")

type sidecarError string

func (e sidecarError) Error() string { return string(e) }
