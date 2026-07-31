package auth

import (
	"errors"
	"fmt"
)

// SharePermission is the INVITE-TIME PRESET an owner picks when they send a
// sharing invite (MYR-184, vehicle-sharing.schema.json SharePermission).
//
// IT IS NO LONGER A CUMULATIVE TIER, AND NO GATE READS IT (MYR-369). Before
// MYR-369 these three values formed a total order (live < live_history < rides)
// that every access gate compared with an AtLeast helper, and a grant's tier was
// fixed for its life. An accepted grant now carries independent, owner-editable
// flags instead — see ShareGrant — and this type survives with exactly two jobs:
//
//  1. On a PENDING invite it is the preset the owner chose. Redemption maps it
//     onto the new grant's flags via GrantForPreset, and that mapping is the
//     only thing the preset ever does.
//  2. On an ACCEPTED grant it is DERIVED BACK from the flags on every read
//     (ShareGrant.Permission) so pre-MYR-369 clients keep getting a coherent
//     answer. Derived output, never stored state, never an input to a decision.
//
// AtLeast IS DELIBERATELY GONE rather than kept for convenience. Leaving a
// comparison helper on a type that no longer models an order is how a future
// gate quietly reintroduces the tier semantics this change removed.
type SharePermission string

const (
	// PermissionLive is the base: the vehicle appears in the viewer's
	// GET /api/vehicles, the REST snapshot is readable under the viewer
	// mask, and the WS vehicle subscription is allowed.
	PermissionLive SharePermission = "live"

	// PermissionLiveHistory is RETIRED (MYR-369) and is NEVER EMITTED by
	// this server on any surface.
	//
	// The capability it named — the drives / trip-history surfaces for a
	// non-owner — has been removed from the product: those endpoints are
	// owner-only again, unconditionally, and a grant created at this preset
	// reads drives no more than a `live` grant does.
	//
	// The constant is kept, and ParseSharePermission still ACCEPTS the
	// value, for one reason: an installed client's send-invite sheet may
	// still submit it, and rejecting it would break invite creation for
	// every un-updated app. NormalizeInvitePermission collapses it to
	// PermissionLive on the way in, so it is never persisted again.
	PermissionLiveHistory SharePermission = "live_history"

	// PermissionRides is the derived value for a grant whose AllowRides
	// flag is set: everything in live, plus creating a ride request against
	// the vehicle as a rider who is NOT its owner.
	PermissionRides SharePermission = "rides"
)

// knownSharePermissions is the set ParseSharePermission accepts. It is a SET,
// not a ranked list — that change is the whole point of MYR-369. The retired
// member is present because input must still be accepted; it is absent from
// every output path.
var knownSharePermissions = map[SharePermission]struct{}{
	PermissionLive:        {},
	PermissionLiveHistory: {},
	PermissionRides:       {},
}

// ErrUnknownSharePermission is returned by ParseSharePermission for a value
// outside the enum.
var ErrUnknownSharePermission = errors.New("unknown share permission")

// String implements fmt.Stringer.
func (p SharePermission) String() string { return string(p) }

// ParseSharePermission validates a string against the enum. The empty string is
// rejected: SharePermission("") is the fail-closed "no preset" sentinel and MUST
// NOT be producible from client input.
//
// Accepts the retired live_history so an un-updated client can still create an
// invite; callers that PERSIST the result MUST run it through
// NormalizeInvitePermission first.
func ParseSharePermission(s string) (SharePermission, error) {
	p := SharePermission(s)
	if _, ok := knownSharePermissions[p]; ok {
		return p, nil
	}
	return SharePermission(""), fmt.Errorf("auth.ParseSharePermission(%q): %w", s, ErrUnknownSharePermission)
}

// NormalizeInvitePermission collapses an accepted preset onto one the server
// still writes: live_history becomes live (MYR-369), everything else is
// unchanged.
//
// Applied at the CREATE boundary rather than at read time, so the retired value
// never enters the database from this point forward and no read path has to
// remember to translate it. Rows written before MYR-369 still hold it, which is
// why GrantForPreset handles it too — normalization stops new ones, it does not
// rewrite history.
//
// An unrecognized value is returned UNCHANGED rather than defaulted: this
// function is not a validator, and silently turning garbage into a real preset
// would hide a bug ParseSharePermission is there to catch.
func NormalizeInvitePermission(p SharePermission) SharePermission {
	if p == PermissionLiveHistory {
		return PermissionLive
	}
	return p
}
