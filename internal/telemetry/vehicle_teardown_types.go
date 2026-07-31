package telemetry

import (
	"context"
	neturl "net/url"
)

// VehicleTeardownResult is the store-layer outcome of a per-vehicle teardown,
// re-declared at the consumer (telemetry) site so this package does not import
// internal/store. The adapter in cmd/telemetry-server maps store.TeardownResult
// onto it.
type VehicleTeardownResult struct {
	Removed            bool
	AlreadyGone        bool
	WasLastVehicle     bool
	TeslaTokensCleared bool
	DriveCount         int
}

// VehicleTeardownWriter performs the authoritative, transactional, audited
// local teardown of one owner-scoped vehicle (store.OwnerTeardown). It MUST
// enforce ownership at the query level (WHERE userId = caller) and be
// idempotent (removing an already-removed car is a clean no-op success).
type VehicleTeardownWriter interface {
	RemoveVehicle(ctx context.Context, userID, vehicleID string) (VehicleTeardownResult, error)
}

// teslaTokenResolver resolves a currently-valid Tesla OAuth token for a user
// off the request path (satisfied by *TeslaTokenResolver). Used best-effort:
// a failure means we skip the Tesla-side stream-config delete and proceed with
// the authoritative local teardown.
type teslaTokenResolver interface {
	Resolve(ctx context.Context, userID string) (TeslaToken, error)
}

// VehicleTeardownOption configures optional dependencies on
// VehicleTeardownHandler.
type VehicleTeardownOption func(*VehicleTeardownHandler)

// WithTeslaGrantRevocation enables the MYR-366 active revocation of the Tesla
// OAuth grant on a last-vehicle removal. Without it the handler behaves as it
// did before MYR-366: the local teardown clears our stored tokens and the
// owner-confirmed revokeUrl remains the only way to withdraw the grant.
func WithTeslaGrantRevocation(revoker teslaLinkRevoker) VehicleTeardownOption {
	return func(h *VehicleTeardownHandler) { h.grant = revoker }
}

// liveActivityEnder ends the Live Activities running on a vehicle's rides
// (satisfied by *push.ActivityNotifier). Fire-and-forget by signature — it
// returns nothing, because there is no failure a teardown should react to.
type liveActivityEnder interface {
	EndForVehicleTeardown(ctx context.Context, vehicleID string)
}

// WithVehicleTeardownLiveActivities makes the teardown end the rider-facing
// Live Activities on this car's rides before it deletes those rides (MYR-172).
//
// Optional, and nil is the pre-MYR-172 behaviour, but "optional" understates
// what it fixes: the teardown deletes go_ride_requests by vehicle with a bare
// DELETE that publishes no event, and migration 0025's FK cascades the Activity
// registrations away with them. Nothing is left to push from and nothing was
// ever notified, so every rider with a Live Activity on one of those rides is
// left with a lock screen frozen on "your car is on its way" for hours.
func WithVehicleTeardownLiveActivities(ender liveActivityEnder) VehicleTeardownOption {
	return func(h *VehicleTeardownHandler) { h.activities = ender }
}

// FleetConfigDeleter removes a vehicle's fleet-telemetry config at Tesla
// (satisfied by *FleetAPIClient.DeleteTelemetryConfig). It is nil in tests/CI
// and only wired when the tesla-http-proxy is configured at runtime, so no
// real Tesla call ever fires from a unit test.
type FleetConfigDeleter interface {
	DeleteTelemetryConfig(ctx context.Context, token, vin string) error
}

// virtualKeyRemoval is the owner-manual key-removal instruction block. There
// is NO Fleet API path and NO deep link for this screen (car-offboarding.md
// §1.3), so it is always presented as steps, never a button.
type virtualKeyRemoval struct {
	Required    bool     `json:"required"`
	Automatable bool     `json:"automatable"`
	Steps       []string `json:"steps"`
}

// vehicleTeardownResponse is the honest post-teardown body (car-offboarding.md
// §4). removed is authoritative ("gone from the app"); the Tesla-side items are
// the two steps only the owner can complete.
type vehicleTeardownResponse struct {
	Removed             bool              `json:"removed"`
	WasLastVehicle      bool              `json:"wasLastVehicle"`
	TeslaTokensCleared  bool              `json:"teslaTokensCleared"`
	StreamConfigDeleted bool              `json:"streamConfigDeleted"`
	RevokeURL           string            `json:"revokeUrl,omitempty"`
	VirtualKeyRemoval   virtualKeyRemoval `json:"virtualKeyRemoval"`
}

// teslaConsentRevokeBase is the owner-confirmed consent-revoke page. It revokes
// the ENTIRE third-party grant for our client_id (invalidates access + refresh
// tokens and removes MyRoboTaxi from the owner's tesla.com third-party apps).
// It is owner-confirmed, NOT a partner machine call (car-offboarding.md §1.2).
const teslaConsentRevokeBase = "https://auth.tesla.com/user/revoke/consent"

// defaultVirtualKeyRemovalSteps are the canonical car-touchscreen steps
// (car-offboarding.md §1.3). Controls → Locks is the always-present path.
func defaultVirtualKeyRemovalSteps() []string {
	return []string{
		"Open the Tesla app or your car's touchscreen",
		"Go to Controls → Locks",
		"Tap the “MyRoboTaxi” key",
		"Tap Remove/Delete Key",
		"Authenticate by tapping a key card on the center console",
	}
}

// buildRevokeURL assembles the owner-confirmed consent-revoke deep link for the
// configured Tesla client_id and app back_url. Returns "" when no client_id is
// configured (the link is meaningless without it), so the field is omitted.
func buildRevokeURL(clientID, backURL string) string {
	if clientID == "" {
		return ""
	}
	q := neturl.Values{}
	q.Set("revoke_client_id", clientID)
	if backURL != "" {
		q.Set("back_url", backURL)
	}
	return teslaConsentRevokeBase + "?" + q.Encode()
}
