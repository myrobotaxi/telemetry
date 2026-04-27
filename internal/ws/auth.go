package ws

import (
	"context"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

// Authenticator validates session tokens and retrieves the vehicles a user
// is authorized to view. Defined at the consumer site (this package);
// implemented by a real Supabase adapter or NoopAuthenticator for testing.
type Authenticator interface {
	// ValidateToken checks whether the given token represents a valid
	// session. On success it returns the user ID (Prisma cuid).
	ValidateToken(ctx context.Context, token string) (userID string, err error)

	// GetUserVehicles returns the vehicle IDs (Prisma cuids) that the
	// user is authorized to receive telemetry for.
	GetUserVehicles(ctx context.Context, userID string) ([]string, error)

	// ResolveRole returns the caller's role (owner | viewer) for the
	// given vehicle. Used by both the WebSocket per-role projection
	// (websocket-protocol.md §4.6) and the REST handler-layer mask
	// (rest-api.md §5.1) to drive the field-mask in internal/mask.
	//
	// Implementations MUST NOT return auth.Role("") on success — the
	// empty role is the fail-closed "unknown" sentinel that the mask
	// layer interprets as deny-all. On error, returning the zero value
	// is acceptable as the caller is expected to surface the error.
	ResolveRole(ctx context.Context, userID, vehicleID string) (auth.Role, error)
}

// NoopAuthenticator accepts any non-empty token and returns a fixed user
// with all-vehicle access. Use it for local development and testing only.
type NoopAuthenticator struct {
	// UserID is returned for every successful validation. Defaults to
	// "test-user" if empty.
	UserID string

	// VehicleIDs is returned by GetUserVehicles. An empty slice means
	// the user is not authorized for any vehicles.
	VehicleIDs []string
}

var _ Authenticator = (*NoopAuthenticator)(nil)

// ValidateToken accepts any non-empty token and returns the configured
// UserID.
func (a *NoopAuthenticator) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	if a.UserID != "" {
		return a.UserID, nil
	}
	return "test-user", nil
}

// GetUserVehicles returns the configured VehicleIDs slice.
func (a *NoopAuthenticator) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	return a.VehicleIDs, nil
}

// ResolveRole always returns RoleOwner. NoopAuthenticator models
// dev-mode "all access" semantics consistent with ValidateToken
// accepting any non-empty token; the dev caller is treated as the owner
// of every vehicle they see.
func (a *NoopAuthenticator) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return auth.RoleOwner, nil
}
