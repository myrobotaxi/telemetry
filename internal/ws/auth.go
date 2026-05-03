package ws

import (
	"context"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

// WildcardVehicleID is the sentinel value the dev-mode NoopAuthenticator
// returns from GetUserVehicles to convey "this client is authorized for
// every vehicle" without overloading the empty-slice case (which a
// production authenticator uses to mean deny-all per NFR-3.21).
//
// Production Authenticator implementations MUST NOT return this value.
// The handshake (handler.go authenticateClient) intercepts it, sets
// Client.allVehicles=true, and excludes it from Client.vehicleIDs and
// the per-vehicle role-resolution loop.
const WildcardVehicleID = "*"

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

// NoopAuthenticator accepts any non-empty token and returns a fixed user.
// Use it for local development and testing only.
type NoopAuthenticator struct {
	// UserID is returned for every successful validation. Defaults to
	// "test-user" if empty.
	UserID string

	// VehicleIDs is returned by GetUserVehicles. A nil or empty slice
	// is interpreted as "dev-mode all-vehicle access" and is expanded to
	// the WildcardVehicleID sentinel by GetUserVehicles. To restrict the
	// dev user to a specific list of vehicles, populate VehicleIDs
	// explicitly.
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

// GetUserVehicles returns the configured VehicleIDs slice. If unset, it
// returns the single-element WildcardVehicleID sentinel so the handshake
// knows to grant dev-mode all-vehicle access. This is what makes empty
// VehicleIDs unambiguously mean "all-access" for NoopAuthenticator while
// still meaning deny-all for any production Authenticator (which never
// emits the sentinel).
func (a *NoopAuthenticator) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	if len(a.VehicleIDs) == 0 {
		return []string{WildcardVehicleID}, nil
	}
	return a.VehicleIDs, nil
}

// ResolveRole always returns RoleOwner. NoopAuthenticator models
// dev-mode "all access" semantics consistent with ValidateToken
// accepting any non-empty token; the dev caller is treated as the owner
// of every vehicle they see.
func (a *NoopAuthenticator) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return auth.RoleOwner, nil
}
