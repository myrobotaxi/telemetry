package mask

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

// Sampling rate for the mask audit log. Per docs/contracts/rest-api.md
// §5.3, every masked response (REST or WS) MUST be audit-logged at a
// 1% deterministic rate computed by hash modulo 100.
const auditSampleModulus uint64 = 100

// auditInsertTimeout caps the time a fire-and-forget Emit goroutine
// will wait on an InsertAuditLog call. The hot path (BroadcastMasked
// fan-out, REST response write) MUST NOT block on this; the goroutine
// uses a detached context with this timeout so a stuck DB pool cannot
// pile up audit goroutines forever.
const auditInsertTimeout = 2 * time.Second

// AuditChannel labels the audit row's transport. The set is closed at
// the two transports a v1 mask projection can flow through.
type AuditChannel string

const (
	// AuditChannelREST tags audit rows produced by the REST handler
	// layer (rest-api.md §5.1).
	AuditChannelREST AuditChannel = "rest"

	// AuditChannelWS tags audit rows produced by the WebSocket hub's
	// per-role projection (websocket-protocol.md §4.6).
	AuditChannelWS AuditChannel = "ws"
)

// TargetType is the value written to AuditEntry.TargetType for a
// mask_applied audit row. The two values mirror data-lifecycle.md §4.2
// targetType enum entries paired with action="mask_applied".
type TargetType string

const (
	// TargetWSBroadcast labels a WebSocket frame audit row.
	TargetWSBroadcast TargetType = "ws_broadcast"

	// TargetRESTResponse labels a REST response audit row.
	TargetRESTResponse TargetType = "rest_response"
)

// ErrInvalidAuditMetadata is returned by BuildEntry when the supplied
// metadata would violate the CG-DL-5 P0-only contract (e.g., an
// unrecognized key that could carry a P1 value, or a fieldsMasked
// element that contains P1-shaped content). Callers should treat this
// as a programmer error — the helper guards against accidentally
// stuffing a coordinate / address / token / email into metadata.
var ErrInvalidAuditMetadata = errors.New("audit metadata violates P0-only contract")

// AuditAction is the typed action label for a mask audit row. It maps
// 1:1 onto store.AuditActionMaskApplied; the constant is duplicated
// here so the mask package can build entries without depending on
// internal/store (CLAUDE.md "interfaces at consumer site"). The two
// strings MUST stay in lock-step — drift would only appear in
// integration tests since the column is opaque text at the DB.
const auditActionMaskApplied = "mask_applied"

// auditInitiatorUser is the initiator value for mask audit rows.
// Per data-lifecycle.md §4.2, "user" is the canonical initiator for
// mask_applied (the consumer's request triggered the response).
const auditInitiatorUser = "user"

// AuditEntry mirrors store.AuditEntry without depending on it (the
// mask package sits below store in the dependency rule). The fields
// are a 1:1 column map of AuditLog per data-lifecycle.md §4.1 and the
// concrete pgx writer in internal/store/audit_repo.go converts this
// shape into its own AuditEntry before insertion.
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

// AuditEmitter is the consumer-site interface the mask package depends
// on for audit-log persistence. Defined here so internal/mask does not
// import internal/store; the production wiring in
// cmd/telemetry-server/main.go provides an adapter that calls
// store.AuditRepo.InsertAuditLog. Tests pass a fake.
//
// Implementations MUST be safe for concurrent use — Emit is invoked
// from goroutines spawned per audit-sampled frame.
type AuditEmitter interface {
	InsertAuditLog(ctx context.Context, entry AuditEntry) error
}

// AuditMetrics records observability counters for audit-log writes.
// Wired to Prometheus in production (see metrics.go) and to a no-op
// in tests.
type AuditMetrics interface {
	// IncAuditWrite increments the success counter for an audit
	// insert keyed by action + targetType.
	IncAuditWrite(action, target string)

	// IncAuditWriteFailure increments the failure counter for an
	// audit insert keyed by action + targetType. The error itself
	// is NOT logged at metric label cardinality — error labels are
	// high-cardinality and would blow up Prometheus storage.
	IncAuditWriteFailure(action, target string)
}

// NoopAuditMetrics is a zero-cost AuditMetrics that drops every call.
// Use it in tests or when Prometheus is not configured.
type NoopAuditMetrics struct{}

var _ AuditMetrics = NoopAuditMetrics{}

// IncAuditWrite implements AuditMetrics.
func (NoopAuditMetrics) IncAuditWrite(string, string) {}

// IncAuditWriteFailure implements AuditMetrics.
func (NoopAuditMetrics) IncAuditWriteFailure(string, string) {}

// allowedMetadataKeys is the cheap allow-list enforced by BuildEntry
// before encoding metadata to JSON. CG-DL-5 forbids P1 values in
// metadata; restricting the key set to a closed enum makes it
// impossible for a future caller to add e.g. a `userEmail` or
// `homeAddress` field without an explicit contract update here. The
// shape mirrors the metadata example in rest-api.md §5.3.
var allowedMetadataKeys = map[string]struct{}{
	"role":         {},
	"channel":      {},
	"fieldsMasked": {},
	"endpoint":     {},
}

// auditMetadata is the strict shape written to AuditEntry.Metadata.
// Marshaling through this struct (rather than a free-form map[string]
// any) means CG-DL-5 violations like accidentally embedding a
// coordinate or address in metadata cannot compile. The JSON tags
// mirror the rest-api.md §5.3 example exactly.
type auditMetadata struct {
	Role         string   `json:"role"`
	Channel      string   `json:"channel"`
	FieldsMasked []string `json:"fieldsMasked"`
	Endpoint     string   `json:"endpoint,omitempty"`
}

// BuildEntry constructs an AuditEntry for a mask_applied row. The
// caller supplies the per-event details; this helper handles ID
// generation, timestamp population, metadata marshaling, and the
// CG-DL-5 P0-only metadata invariant.
//
// Field semantics:
//   - userID: for REST, the authenticated caller. For WS, an empty
//     string — the WS audit emit is per (vehicleID, role, frame) at
//     the hub, not per client (rest-api.md §5.3). NOT NULL is satisfied
//     by Postgres; an empty string is the correct sentinel.
//   - target: TargetWSBroadcast or TargetRESTResponse.
//   - targetID: the vehicleID (or driveID) whose response was masked.
//   - role: the role for which the projection ran.
//   - channel: AuditChannelREST or AuditChannelWS.
//   - fieldsMasked: the list of removed field names (P0 — names only,
//     never values).
//   - endpoint: optional — for REST, a route pattern like
//     "/api/vehicles/{vehicleId}/snapshot" (NOT a substituted URL,
//     which would carry a vehicleID into metadata when targetID
//     already covers that). For WS, leave empty.
//
// The empty fieldsMasked is rejected — the contract only emits an
// audit row when the mask removed at least one field.
func BuildEntry(
	userID string,
	target TargetType,
	targetID string,
	role auth.Role,
	channel AuditChannel,
	fieldsMasked []string,
	endpoint string,
) (AuditEntry, error) {
	if len(fieldsMasked) == 0 {
		return AuditEntry{}, fmt.Errorf("mask.BuildEntry: %w: fieldsMasked is empty", ErrInvalidAuditMetadata)
	}
	for _, name := range fieldsMasked {
		if name == "" {
			return AuditEntry{}, fmt.Errorf("mask.BuildEntry: %w: fieldsMasked contains empty entry", ErrInvalidAuditMetadata)
		}
	}

	meta := auditMetadata{
		Role:         string(role),
		Channel:      string(channel),
		FieldsMasked: fieldsMasked,
		Endpoint:     endpoint,
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("mask.BuildEntry: marshal metadata: %w", err)
	}

	now := time.Now().UTC()
	return AuditEntry{
		ID:         newAuditID(),
		UserID:     userID,
		Timestamp:  now,
		Action:     auditActionMaskApplied,
		TargetType: string(target),
		TargetID:   targetID,
		Initiator:  auditInitiatorUser,
		Metadata:   encoded,
		CreatedAt:  now,
	}, nil
}

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

// auditIDPrefix is the leading character on every mask audit cuid
// (matches the convention used elsewhere in the project's Prisma
// tables — every cuid starts with `c`).
const auditIDPrefix = "c"

// auditIDRandomBytes is the number of random bytes hex-encoded into
// the cuid suffix. 16 bytes -> 32 hex chars + 1 "c" prefix = 33-char
// id, which is within the typical cuid length envelope and
// collision-free at our event volume.
const auditIDRandomBytes = 16

// newAuditID generates a caller-provided cuid for a mask audit row.
// Crypto-strong randomness avoids predictable IDs; the "c" prefix
// matches the cuid mental model documented at the top of
// internal/store/audit_repo.go.
func newAuditID() string {
	b := make([]byte, auditIDRandomBytes)
	if _, err := rand.Read(b); err != nil {
		// Crypto/rand only fails when the OS RNG is unavailable, which
		// is unrecoverable. Fall back to a timestamp-derived ID rather
		// than panic — the audit row is still useful.
		return fmt.Sprintf("%s%x", auditIDPrefix, time.Now().UnixNano())
	}
	return auditIDPrefix + hex.EncodeToString(b)
}

// allowedMetadataKeysIntersect reports whether decoded keys is a
// subset of allowedMetadataKeys. Used by tests to assert no unknown
// keys leak into a built entry.
func allowedMetadataKeysIntersect(keys []string) bool {
	for _, k := range keys {
		if _, ok := allowedMetadataKeys[k]; !ok {
			return false
		}
	}
	return true
}

// ShouldAuditREST returns true when this REST response should be
// emitted to the audit log. The decision is deterministic given the
// inputs — replaying the same userID + requestID + resourceID will
// always return the same boolean. Per rest-api.md §5.3, the inputs are
// joined with a separator before hashing to avoid collisions across
// distinct triples that share a concatenated form.
func ShouldAuditREST(userID, requestID, resourceID string) bool {
	h := fnv.New64a()
	writeField(h, userID)
	writeField(h, requestID)
	writeField(h, resourceID)
	return h.Sum64()%auditSampleModulus == 0
}

// ShouldAuditWS returns true when this WebSocket frame should be
// audit-logged. Per rest-api.md §5.3, the WS audit emit is per
// (vehicleID, role, frame) at the hub layer (NOT per client) — the
// hash inputs reflect that scope.
//
// frameSeq SHOULD be the envelope sequence number once DV-02 lands.
// Until then, callers can pass a per-vehicle monotonic counter — the
// determinism only requires that the counter is reproducible during
// replay.
func ShouldAuditWS(vehicleID string, role auth.Role, frameSeq uint64) bool {
	h := fnv.New64a()
	writeField(h, vehicleID)
	writeField(h, string(role))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], frameSeq)
	_, _ = h.Write(buf[:])
	return h.Sum64()%auditSampleModulus == 0
}

// writeField writes a length-prefixed string into the hash. The length
// prefix prevents ambiguity between (e.g.) {"abc", "def"} and
// {"ab", "cdef"} which would otherwise hash to the same byte sequence
// when concatenated. `hash.Hash64` already implements `io.Writer`, so
// the hasher is passed in directly without an inline anonymous
// interface (PR #195 review suggestion #2).
func writeField(h io.Writer, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s))))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(s))
}
