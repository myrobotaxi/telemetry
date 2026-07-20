package commands

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// ErrTransportNotConfigured is a permanent misconfiguration sentinel: the
// command transport (tesla-http-proxy) is not wired up, so no command can
// reach any vehicle. It is NOT a transient vehicle/Fleet failure — retrying
// it burns budget for nothing (the proxy URL will not appear mid-flight), so
// callers (e.g. the MYR-176 dispatcher) MUST treat it as terminal. Reached
// via errors.Is on the returned *CommandError.
var ErrTransportNotConfigured = errors.New("vehicle command transport is not configured")

// CommandError is the typed failure returned by Executor.Execute. It
// carries the HTTP status and the shared wserrors.ErrorCode so the REST
// handler can write the rest-api.md §4.1 error envelope without a
// status↔code lookup table. Message is developer-facing and MUST NOT
// contain any P1 value (no coordinates, addresses, tokens, or raw VINs) —
// the executor constructs these from opaque, non-sensitive strings only.
//
// Retryable marks a transient failure worth another bounded attempt. cause
// (optional) carries a sentinel for errors.Is classification (e.g.
// ErrTransportNotConfigured) without leaking it into Error()/the wire code.
//
// Detail (optional) is the opaque Tesla/proxy-side reason for the failure
// (e.g. `invalid_command`, `already_locked`) threaded up from the transport's
// TransportResult.Reason. It feeds the dispatch outcome log's error_detail
// without touching the wire code. It is developer-facing and carries no P1
// value: the transport's sanitizeReason ENFORCES this before the reason is set
// here — it strips any URL and any signed-decimal coordinate pair, collapses
// the remainder to a lowercase [a-z0-9_ .:-] charset, and truncates to 120
// runes. Only opaque codes/prose survive.
type CommandError struct {
	Code      wserrors.ErrorCode
	Status    int
	Message   string
	Detail    string
	Retryable bool
	cause     error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("command %s (%d): %s", e.Code, e.Status, e.Message)
}

// Unwrap exposes the optional sentinel cause for errors.Is/As.
func (e *CommandError) Unwrap() error { return e.cause }

func errUnknownCommand(name string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeInvalidRequest,
		Status:  http.StatusBadRequest,
		Message: fmt.Sprintf("unknown command %q", name),
	}
}

func errInvalidParams(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeInvalidRequest,
		Status:  http.StatusBadRequest,
		Message: msg,
	}
}

func errPermissionDenied(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodePermissionDenied,
		Status:  http.StatusForbidden,
		Message: msg,
	}
}

// errKeyNotPaired is the pre-pairing default for signer-required commands:
// the virtual key is not enrolled on the vehicle (MYR-115), or the
// command-signing transport is not configured. Terminal — the owner must
// pair before retrying.
func errKeyNotPaired(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeKeyNotPaired,
		Status:  http.StatusForbidden,
		Message: msg,
	}
}

// errVehicleAsleep is returned only after the bounded wake+retry budget is
// exhausted. Retryable — the SDK backs off and tries again later.
func errVehicleAsleep() *CommandError {
	return &CommandError{
		Code:      wserrors.ErrCodeVehicleAsleep,
		Status:    http.StatusServiceUnavailable,
		Message:   "vehicle asleep or offline after wake retries",
		Retryable: true,
	}
}

// errCommandFailed is the generic transient upstream failure (the vehicle or
// the Fleet API failed to apply, or a transport error reaching the proxy).
// Retryable — the SDK / dispatcher backs off and tries again.
func errCommandFailed(msg string) *CommandError {
	return &CommandError{
		Code:      wserrors.ErrCodeCommandFailed,
		Status:    http.StatusBadGateway,
		Message:   msg,
		Retryable: true,
	}
}

// errTransportNotConfigured is the PERMANENT variant of a command_failed: the
// transport is not wired up at all. It keeps the command_failed wire code (so
// REST callers see the same 502) but is non-retryable and carries the
// ErrTransportNotConfigured sentinel so an async caller can classify it via
// errors.Is and short-circuit its retry budget.
func errTransportNotConfigured() *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeCommandFailed,
		Status:  http.StatusBadGateway,
		Message: "vehicle command transport is not configured",
		cause:   ErrTransportNotConfigured,
	}
}

func errInternal(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeInternalError,
		Status:  http.StatusInternalServerError,
		Message: msg,
	}
}
