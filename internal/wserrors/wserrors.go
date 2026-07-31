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
	// ErrCodeConflict is REST 409 (MYR-174) — a ride-request state
	// mutation is illegal from the row's current lifecycle state (e.g.
	// cancelling a completed ride, accepting a cancelled one). REST-only:
	// the WS transport never emits it (schemas/ws-messages.schema.json
	// ErrorPayload.code notes conflict is REST-only). SDK consumers branch
	// on the typed code and surface the transition as rejected, never
	// auto-retrying the same mutation.
	ErrCodeConflict ErrorCode = "conflict"
	// ErrCodeRideActive is REST 409 (MYR-230) — the caller already has an
	// OPEN instant ride request (status in {requested, accepted, enroute,
	// arrived}) and tried to create another instant one. Only one active
	// instant ride per rider is allowed; scheduled rides are exempt. REST-only:
	// the WS transport never emits it. Distinct from ErrCodeConflict (which is
	// an illegal lifecycle *transition* on a known ride) — this rejects the
	// *creation* of a second concurrent ride. The 409 body carries the
	// existing open request so the SDK can adopt it into the pending/tracking
	// UI rather than surfacing a generic failure (rest-api.md §7.8). SDK
	// consumers branch on the typed code and MUST NOT auto-retry the create.
	ErrCodeRideActive ErrorCode = "ride_active"
	// ErrCodeVehicleUnavailable is REST 409 (MYR-277) — an owner tried to
	// ACCEPT a ride request whose target vehicle cannot currently fulfill it:
	// the vehicle's persisted status is `in_service` (owner put it into
	// service) or `offline` (unreachable). The transition is otherwise legal
	// (the ride is still `requested`), so this is NOT ErrCodeConflict (an
	// illegal lifecycle *transition* on a known ride); it is a capability gate
	// on the target vehicle. The gate reads the CURRENT persisted status at
	// accept time. `parked`/`driving`/`charging` are dispatchable and allow the
	// accept. REST-only: the WS transport never emits it. SDK consumers branch
	// on the typed code and surface the vehicle as unavailable; they MAY retry
	// once the owner brings the vehicle back out of service / online
	// (rest-api.md §7.8).
	ErrCodeVehicleUnavailable ErrorCode = "vehicle_unavailable"
)

// Vehicle-command codes added by MYR-180 (Tesla signed vehicle-command
// proxy). All three are REST-only on the wire — the WS transport is
// stream-oriented and never carries command requests. They are members
// of the shared enum so the SDK's CoreError union stays one enum across
// transports (rest-api.md §4.1.1, §7.9). See docs/operations/vehicle-commands.md.
const (
	// ErrCodeKeyNotPaired is REST 403 — a signed vehicle command cannot be
	// executed because the application's virtual key is not enrolled on the
	// vehicle (owner has not completed the tesla.com/_ak/<domain> pairing,
	// MYR-115), or the command-signing transport is not configured. Terminal:
	// the SDK surfaces it to the UI and prompts the owner to pair; it MUST
	// NOT auto-retry. This is the default pre-pairing outcome for every
	// signer-required command.
	ErrCodeKeyNotPaired ErrorCode = "key_not_paired"
	// ErrCodeVehicleAsleep is REST 503 — the target vehicle was asleep or
	// offline and did not come online within the executor's bounded
	// wake+retry budget. Transient: the SDK auto-retries with backoff
	// (rest-api.md §7.9 wake policy). Callers that receive OK never see this
	// code — the executor wakes and retries internally first.
	ErrCodeVehicleAsleep ErrorCode = "vehicle_asleep"
	// ErrCodeCommandFailed is REST 502 — the vehicle or the Fleet API
	// rejected or failed the command for a non-scope, non-pairing reason
	// (result:false with a vehicle-side reason, a session/counter error that
	// survived re-handshake, or an upstream Fleet/proxy failure). Terminal
	// for the operation; the SDK surfaces the failure. Collapses the several
	// vehicle-side failure modes into one typed code in v1.
	ErrCodeCommandFailed ErrorCode = "command_failed"
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
		ErrCodeConflict,
		ErrCodeRideActive,
		ErrCodeVehicleUnavailable,
		ErrCodeKeyNotPaired,
		ErrCodeVehicleAsleep,
		ErrCodeCommandFailed,
	}
}

// ErrorEnvelope is the canonical REST error response shape per
// docs/contracts/rest-api.md §4.1: {error: {code, message, subCode}}.
//
// SDK consumers branch on Error.Code (typed enum, FR-7.1) and never on
// Error.Message. SubCode is nullable and usually null; the sub-codes in
// v1 are `device_cap` (WS-only, carried on the WS frame payload rather
// than this envelope), `reauth_required` (REST §7.6/§7.7) and
// `reservation_expired` (REST §7.21.1, MYR-172).
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
	writeEnvelope(w, logger, status, ErrorEnvelopeBody{Code: code, Message: msg})
}

// SubCode is the closed enum of typed sub-codes. It exists for the same
// reason ErrorCode does: the compiler, not review, is what stops a
// call site inventing a string the SDK's generated union cannot decode.
type SubCode string

const (
	// SubCodeDeviceCap qualifies rate_limited on the WS per-user
	// concurrent-connection cap. WS-only; declared here because the SDK
	// carries ONE sub-code union across both transports.
	SubCodeDeviceCap SubCode = "device_cap"
	// SubCodeReauthRequired qualifies auth_failed when the recent-login
	// gate rejects an otherwise-valid bearer (rest-api.md §7.6 / §7.7).
	SubCodeReauthRequired SubCode = "reauth_required"
	// SubCodeReservationExpired qualifies conflict on §7.21.1 when a Live
	// Activity registration is refused because the ride's reservation
	// expired (MYR-172). It is NOT derivable from the ride's status: the
	// sweeper leaves the row at `accepted` and records the give-up in the
	// dispatch columns, so a client that read the ride would see a live
	// ride and a 409 it could not explain. With this sub-code it knows to
	// end its Activity and say the reservation expired rather than
	// retrying the registration forever.
	SubCodeReservationExpired SubCode = "reservation_expired"
)

// WriteErrorEnvelopeSub is WriteErrorEnvelope with a typed sub-code, for
// the handful of 4xx paths where the primary code alone does not tell
// the client what to do.
func WriteErrorEnvelopeSub(w http.ResponseWriter, logger *slog.Logger, status int, code ErrorCode, sub SubCode, msg string) {
	s := string(sub)
	writeEnvelope(w, logger, status, ErrorEnvelopeBody{Code: code, Message: msg, SubCode: &s})
}

// writeEnvelope is the single encoder both helpers share, so the wire
// shape cannot drift between a sub-coded error and a plain one.
func writeEnvelope(w http.ResponseWriter, logger *slog.Logger, status int, body ErrorEnvelopeBody) {
	code := body.Code
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorEnvelope{Error: body}); err != nil {
		logger.Error("WriteErrorEnvelope: encode failed",
			slog.Int("status", status),
			slog.String("code", string(code)),
			slog.String("error", err.Error()),
		)
	}
}
