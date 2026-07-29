package auth

import (
	"errors"
	"fmt"
)

// SharePermission is the cumulative access tier an owner grants one invited
// person for one vehicle (MYR-184, vehicle-sharing.schema.json
// SharePermission).
//
// STRICTLY CUMULATIVE — the tiers form a total order:
//
//	live < live_history < rides
//
// Each tier grants everything below it PLUS its own increment, which is why
// every gate in this codebase compares with AtLeast rather than testing
// equality against a set of independent capabilities. Getting that wrong makes
// a 'rides' viewer unable to see the live map they were also granted.
//
// Increments:
//   - live          — the vehicle appears in the viewer's GET /api/vehicles,
//     the REST snapshot is readable under the viewer mask,
//     and the WS vehicle subscription is allowed.
//   - live_history  — everything above, plus the drives surfaces.
//   - rides         — everything above, plus creating a ride request against
//     the vehicle as a rider who is NOT its owner.
//
// New members MUST be appended in increasing-privilege order so the rank table
// below stays a total order.
type SharePermission string

const (
	// PermissionLive is the lowest tier: live location and state only.
	PermissionLive SharePermission = "live"
	// PermissionLiveHistory adds the drives / trip-history surfaces.
	PermissionLiveHistory SharePermission = "live_history"
	// PermissionRides adds rider-side ride creation against the vehicle.
	PermissionRides SharePermission = "rides"
)

// sharePermissionRank maps each tier onto its position in the total order.
// The zero value of the map lookup (0) is deliberately BELOW every real tier,
// so an unrecognized or empty permission satisfies no gate — fail-closed by
// construction rather than by a forgotten branch.
var sharePermissionRank = map[SharePermission]int{
	PermissionLive:        1,
	PermissionLiveHistory: 2,
	PermissionRides:       3,
}

// ErrUnknownSharePermission is returned by ParseSharePermission for a value
// outside the tier enum.
var ErrUnknownSharePermission = errors.New("unknown share permission")

// AtLeast reports whether this tier grants everything the required tier does.
//
// An unrecognized value on EITHER side answers false: an unknown held tier
// grants nothing, and an unknown required tier is never satisfied. Both are the
// safe direction — a gate that cannot be evaluated must not open.
func (p SharePermission) AtLeast(required SharePermission) bool {
	have, ok := sharePermissionRank[p]
	if !ok {
		return false
	}
	want, ok := sharePermissionRank[required]
	if !ok {
		return false
	}
	return have >= want
}

// String implements fmt.Stringer.
func (p SharePermission) String() string { return string(p) }

// ParseSharePermission validates a string against the tier enum. The empty
// string is rejected: SharePermission("") is the fail-closed "no tier" sentinel
// and MUST NOT be producible from client input.
func ParseSharePermission(s string) (SharePermission, error) {
	p := SharePermission(s)
	if _, ok := sharePermissionRank[p]; ok {
		return p, nil
	}
	return SharePermission(""), fmt.Errorf("auth.ParseSharePermission(%q): %w", s, ErrUnknownSharePermission)
}
