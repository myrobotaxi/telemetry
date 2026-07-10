package commands

import (
	"fmt"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// CommandError is the typed failure returned by Executor.Execute. It
// carries the HTTP status and the shared wserrors.ErrorCode so the REST
// handler can write the rest-api.md §4.1 error envelope without a
// status↔code lookup table. Message is developer-facing and MUST NOT
// contain any P1 value (no coordinates, addresses, tokens, or raw VINs) —
// the executor constructs these from opaque, non-sensitive strings only.
type CommandError struct {
	Code      wserrors.ErrorCode
	Status    int
	Message   string
	Retryable bool
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("command %s (%d): %s", e.Code, e.Status, e.Message)
}

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

func errCommandFailed(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeCommandFailed,
		Status:  http.StatusBadGateway,
		Message: msg,
	}
}

func errInternal(msg string) *CommandError {
	return &CommandError{
		Code:    wserrors.ErrCodeInternalError,
		Status:  http.StatusInternalServerError,
		Message: msg,
	}
}
