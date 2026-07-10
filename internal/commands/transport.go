package commands

import "context"

// Outcome is the classified result of a single command attempt against the
// Fleet transport. It is the seam that lets the Executor run its retry
// policies (wake-on-asleep, re-handshake-on-counter) without knowing the
// concrete HTTP/proxy wire shape, and lets tests drive every branch with a
// fake transport instead of a live Tesla call.
type Outcome int

const (
	// OutcomeOK — the vehicle accepted and applied the command.
	OutcomeOK Outcome = iota
	// OutcomeAsleep — the vehicle is asleep/offline/unavailable; the
	// Executor wakes it and retries within a bounded budget.
	OutcomeAsleep
	// OutcomeCounterError — a signing session/anti-replay-counter error; the
	// proxy re-handshakes on the next attempt, so the Executor retries once.
	OutcomeCounterError
	// OutcomeNotPaired — the vehicle rejected the command because the
	// virtual key is not enrolled (owner has not paired, MYR-115).
	OutcomeNotPaired
	// OutcomePermissionDenied — Tesla rejected the command for access/scope
	// reasons the preflight scope check did not catch.
	OutcomePermissionDenied
	// OutcomeInvalidRequest — Tesla rejected the command parameters.
	OutcomeInvalidRequest
	// OutcomeFailed — any other vehicle-side or upstream failure.
	OutcomeFailed
)

// TransportRequest is a single (possibly signed) command to send to one
// vehicle. Command is the Tesla command name (e.g. "door_lock"); Token is
// the vehicle owner's Tesla OAuth access token; Body is the marshalled
// Tesla command parameter JSON (nil for parameterless commands).
type TransportRequest struct {
	VIN     string
	Command string
	Token   string
	Body    []byte
}

// TransportResult is the classified transport response. Reason is an
// opaque, non-sensitive detail string safe for logs and error messages.
type TransportResult struct {
	Outcome Outcome
	Reason  string
}

// Transport is the signed-session seam over the Fleet API. The concrete
// implementation (ProxyTransport) forwards to the tesla-http-proxy sidecar,
// which owns command signing and per-vehicle session caching. Tests
// substitute a fake so no live Tesla call is ever made.
type Transport interface {
	// Command sends one command attempt and classifies the response.
	Command(ctx context.Context, req TransportRequest) (TransportResult, error)
	// Wake asks the vehicle to wake. Best-effort: the Executor calls it
	// between asleep retries and does not treat a wake error as fatal.
	Wake(ctx context.Context, vin, token string) error
	// Enabled reports whether the command-signing transport is configured
	// (proxy URL present). When false the Executor degrades to
	// key_not_paired for signer-required commands instead of dialing.
	Enabled() bool
}
