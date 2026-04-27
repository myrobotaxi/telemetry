package auth

import (
	"errors"
	"fmt"
)

// Role identifies the access level a caller has against a particular
// vehicle. Roles are resolved per (user, vehicle) at WebSocket handshake
// time and at REST request time and feed the field-mask layer in
// internal/mask. See docs/contracts/rest-api.md §5 for the canonical
// matrix and FR-5.4 / FR-5.5 for the v1 vs. future-role contract.
type Role string

const (
	// RoleOwner identifies the user who owns the vehicle on the Prisma
	// "Vehicle" table (Vehicle.userId == caller). Owners see every
	// SDK-exposed field.
	RoleOwner Role = "owner"

	// RoleViewer identifies a user who has been invited to view a
	// vehicle they do not own. Viewers receive the full real-time stream
	// minus the fields enumerated as owner-only in rest-api.md §5.2
	// (currently just licensePlate, which is forward-looking and not
	// yet on the wire).
	RoleViewer Role = "viewer"
)

// ErrUnknownRole is returned by ParseRole when the input is not one of
// the v1 roles. Treat the empty Role("") sentinel separately — it is the
// fail-closed "unknown" value the mask layer interprets as deny-all.
var ErrUnknownRole = errors.New("unknown role")

// String implements fmt.Stringer.
func (r Role) String() string {
	return string(r)
}

// ParseRole validates a string against the v1 role enum. The empty
// string is intentionally rejected: the empty Role("") value is used as
// a fail-closed "unknown" sentinel inside the mask layer (deny-all
// projection) and MUST NOT be produced by parsing user input. See
// rest-api.md §5 for the fail-closed semantics.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleViewer:
		return Role(s), nil
	default:
		return Role(""), fmt.Errorf("auth.ParseRole(%q): %w", s, ErrUnknownRole)
	}
}
