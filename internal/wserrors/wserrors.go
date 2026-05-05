// Package wserrors holds the typed error-code catalog and the REST
// error-envelope helper. It is the single source of truth for every
// error code the server emits across both transports (WebSocket
// `error` frame and REST `error.code` envelope).
//
// Both internal/ws/ and internal/telemetry/ depend on this package; it
// depends on no other internal package, which is what lets the WS and
// REST layers share the catalog without forming an import cycle.
package wserrors

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorCode is the closed enum of typed error codes the server returns
// to SDK clients on both the WebSocket `error` frame
// (websocket-protocol.md §6.1.1) and the REST `error.code` envelope
// field (rest-api.md §4.1.1). Per FR-7.1, SDK consumers branch on the
// typed code, never on the human-readable `message`.
//
// The catalog below is the single source of truth for the server's
// emission surface. Adding a new code requires updating, in the same
// PR: this enum, the catalog tables in websocket-protocol.md §6.1.1
// and rest-api.md §4.1.1, the JSON Schema enum at
// schemas/ws-messages.schema.json `ErrorPayload.code`, and the
// reachability test in internal/wserrors/wserrors_test.go.
//
// Function signatures across the codebase (sendError, writeError,
// WriteErrorEnvelope) take ErrorCode rather than `string`, so the
// compiler enforces that every error construction site pulls a value
// from this enum — there is no string-literal-at-call-site path.
type ErrorCode string

// Implemented today on the WebSocket transport.
const (
	// ErrCodeAuthFailed is emitted when the auth token is rejected at
	// handshake or when GetUserVehicles fails. Terminal — the SDK
	// surfaces it to the UI and does not auto-retry.
	ErrCodeAuthFailed ErrorCode = "auth_failed"
	// ErrCodeAuthTimeout is emitted when the client did not send the
	// auth frame within HandlerConfig.AuthTimeout (default 5 s). Treated
	// as transient — the SDK auto-retries with backoff.
	ErrCodeAuthTimeout ErrorCode = "auth_timeout"
)

// REST-side codes added by MYR-47. Some are also in the WS catalog as
// PLANNED, blocked on per-vehicle subscribe (DV-07), per-user cap (DV-08),
// or sequence-number envelope (DV-02) — see catalog table for status.
const (
	// ErrCodeInvalidRequest covers REST 400s: malformed VIN/path
	// param/body/query. WS has no analogue today.
	ErrCodeInvalidRequest ErrorCode = "invalid_request"
	// ErrCodePermissionDenied is the generic 403 — user lacks the role
	// for the requested operation.
	ErrCodePermissionDenied ErrorCode = "permission_denied"
	// ErrCodeVehicleNotOwned is the vehicle-scoped specialization of
	// permission_denied: vehicleId path param is not in the caller's
	// ownership set.
	ErrCodeVehicleNotOwned ErrorCode = "vehicle_not_owned"
	// ErrCodeNotFound is REST 404 — unknown resource (vehicleId, etc.).
	// Intentionally indistinguishable from permission_denied at the
	// transport layer to avoid leaking existence (rest-api.md §4.1.1).
	ErrCodeNotFound ErrorCode = "not_found"
	// ErrCodeRateLimited is emitted when a per-user/per-IP rate cap is
	// breached. WS pairs this with close 4003 + optional subCode
	// "device_cap" for per-user concurrent-connection breaches; REST
	// returns 429 with no subCode (per-request cap).
	ErrCodeRateLimited ErrorCode = "rate_limited"
	// ErrCodeInternalError is the catch-all 500: panics, DB errors,
	// downstream timeouts. SDK auto-retries with backoff.
	ErrCodeInternalError ErrorCode = "internal_error"
	// ErrCodeServiceUnavailable is REST-only 503 — reserved for
	// maintenance windows / graceful shutdown. v1 does not yet emit
	// this code; declared so SDK consumers can write forward-compatible
	// handlers (see rest-api.md §4.1.1.a).
	ErrCodeServiceUnavailable ErrorCode = "service_unavailable"
	// ErrCodeSnapshotRequired is WS-only — server cannot satisfy the
	// client's subscribe.sinceSeq request because the gap is too large.
	// PLANNED, blocked on DV-02 (envelope sequence numbers).
	ErrCodeSnapshotRequired ErrorCode = "snapshot_required"
)

// AllCodes returns every ErrorCode defined in this package, in catalog
// order. Used by the reachability test to iterate the closed enum and
// assert each code is in either the implemented or the documented-as-
// blocked set.
func AllCodes() []ErrorCode {
	return []ErrorCode{
		ErrCodeAuthFailed,
		ErrCodeAuthTimeout,
		ErrCodeInvalidRequest,
		ErrCodePermissionDenied,
		ErrCodeVehicleNotOwned,
		ErrCodeNotFound,
		ErrCodeRateLimited,
		ErrCodeInternalError,
		ErrCodeServiceUnavailable,
		ErrCodeSnapshotRequired,
	}
}

// ErrorEnvelope is the canonical REST error response shape per
// docs/contracts/rest-api.md §4.1: {error: {code, message, subCode}}.
//
// SDK consumers branch on Error.Code (typed enum, FR-7.1) and never on
// Error.Message. SubCode is nullable in v1 — only the WS path emits a
// non-null sub-code (rate_limited.device_cap) and that path uses the
// WS frame payload, not this envelope.
type ErrorEnvelope struct {
	Error ErrorEnvelopeBody `json:"error"`
}

// ErrorEnvelopeBody carries the typed code, the human-readable message,
// and the optional sub-code.
type ErrorEnvelopeBody struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	SubCode *string   `json:"subCode"`
}

// WriteErrorEnvelope writes a contract-shaped REST error response. The
// `code` parameter is an ErrorCode (closed enum) so the compiler refuses
// string literals at the call site — every 4xx/5xx path in the codebase
// reaches this helper with a typed code, never a magic string.
func WriteErrorEnvelope(w http.ResponseWriter, logger *slog.Logger, status int, code ErrorCode, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorEnvelope{
		Error: ErrorEnvelopeBody{Code: code, Message: msg},
	}); err != nil {
		logger.Error("WriteErrorEnvelope: encode failed",
			slog.Int("status", status),
			slog.String("code", string(code)),
			slog.String("error", err.Error()),
		)
	}
}
