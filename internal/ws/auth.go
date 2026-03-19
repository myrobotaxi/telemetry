package ws

import "context"

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
