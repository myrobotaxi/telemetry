package telemetry

import "context"

// VehicleAuthorizer answers "is this VIN owned by an existing Vehicle
// row?". It is consulted at every inbound mTLS frame so the receiver
// rejects frames for VINs whose Vehicle row was deleted (data
// lifecycle §3.5 cleanup, MYR-73). The interface is defined at the
// consumer site (telemetry package) and satisfied by store.VINCache
// via a thin adapter in cmd/telemetry-server/adapters.go.
//
// Implementations MUST be safe for concurrent use by many goroutines
// (the receiver consults the authorizer from each per-vehicle read
// loop). They SHOULD cache positive and negative results to keep the
// hot path off the database.
//
// IsAuthorized returns false (without an error) when the VIN is not
// known. Errors are reserved for transient transport problems (e.g.,
// DB unreachable); the receiver treats errors as "fail open" — a
// transient lookup failure must not drop a real vehicle's stream. The
// rejection-by-cache path runs only when IsAuthorized returns
// (false, nil).
type VehicleAuthorizer interface {
	IsAuthorized(ctx context.Context, vin string) (bool, error)
}

// allowAllAuthorizer is a no-op authorizer used when the receiver is
// constructed without an explicit authorizer (legacy callers / tests).
// It accepts every VIN; the data-lifecycle.md §3.5 reject path is
// only active when a real authorizer is wired by main.go.
type allowAllAuthorizer struct{}

// IsAuthorized always returns true.
func (allowAllAuthorizer) IsAuthorized(_ context.Context, _ string) (bool, error) {
	return true, nil
}
