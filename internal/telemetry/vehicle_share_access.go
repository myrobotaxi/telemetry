package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// The ONE place a per-vehicle handler decides whether a non-owner may proceed
// (MYR-184 / MYR-91 viewer merge).
//
// Every read handler that used to answer `row.UserID != userID → 403` now
// routes through vehicleAccessFor instead. Keeping the decision in a single
// function is the point: a per-handler copy of "owner, or a share of at least
// tier X" is precisely where one endpoint would end up a tier more generous
// than the rest.

// VehicleShareReader resolves the CAPABILITY SET a caller holds over a vehicle
// through a LIVE ACCEPTED share (MYR-369). Implementations return an error
// wrapping sdk.ErrNotFound when there is no such grant OR when the grant is
// SUSPENDED — the two are deliberately the same answer, and callers MUST treat
// both as "no access", never as a default.
type VehicleShareReader interface {
	ShareGrantFor(ctx context.Context, userID, vehicleID string) (auth.ShareGrant, error)
}

// vehicleAccess is the resolved answer for one (caller, vehicle) pair.
type vehicleAccess struct {
	// Role is owner or viewer.
	Role auth.Role
	// Grant is the viewer's capability set, and the ZERO VALUE for an owner
	// — an owner holds no grant, they hold everything. The zero value is
	// the most restrictive one, so a caller that forgets to branch on Role
	// under-grants rather than over-grants. Branch on Role first.
	Grant auth.ShareGrant
}

// errNoVehicleAccess is the internal denial sentinel. Handlers convert it to
// the status their endpoint's rule prescribes (403 for the ones whose existence
// is already established by a successful row read, 404 where the vehicle's
// existence must stay hidden) — it is never surfaced to a client verbatim.
var errNoVehicleAccess = errors.New("no access to vehicle")

// shareCapability names what a gate requires of a non-owner (MYR-369). It
// replaced the `minTier` parameter, and the replacement is not cosmetic: a tier
// argument invited a >= comparison, and the model no longer has an order to
// compare over.
type shareCapability int

const (
	// capBase is what every live grant conveys: the vehicle is visible.
	// Requiring it means "any non-suspended accepted share will do".
	capBase shareCapability = iota
	// capRides additionally requires the grant's ride capability.
	capRides
)

// grants reports whether a resolved grant satisfies this requirement.
//
// Every arm routes through a ShareGrant method that carries the suspension term
// itself, so no gate can accidentally read a capability off a paused grant. The
// default arm DENIES: an unrecognized requirement is never satisfied, which is
// the safe direction for a gate that cannot be evaluated.
func (c shareCapability) grants(g auth.ShareGrant) bool {
	switch c {
	case capBase:
		return g.Active()
	case capRides:
		return g.GrantsRides()
	default:
		return false
	}
}

// vehicleAccessFor resolves whether userID may act on a vehicle whose owner is
// ownerUserID, requiring the given capability when the caller is not the owner.
//
// ownerUserID comes from a row the caller ALREADY read, so this costs one extra
// query only on the non-owner path — the owner path, which is the overwhelming
// majority of traffic, is unchanged.
//
// Fail-closed in four distinct ways, all deliberate:
//   - a nil shares reader denies every non-owner (a deployment that forgot to
//     wire sharing does not silently grant viewer access);
//   - a missing grant denies;
//   - a SUSPENDED grant denies, and denies identically to a missing one;
//   - a grant lacking the required capability denies, rather than being widened.
//
// A transient lookup failure is returned as a real error, NOT as a denial, so
// the handler answers 500 and the outage is visible instead of looking like a
// permissions problem.
func vehicleAccessFor(
	ctx context.Context,
	shares VehicleShareReader,
	userID, vehicleID, ownerUserID string,
	need shareCapability,
) (vehicleAccess, error) {
	if ownerUserID == userID {
		return vehicleAccess{Role: auth.RoleOwner}, nil
	}
	if shares == nil {
		return vehicleAccess{}, errNoVehicleAccess
	}

	grant, err := shares.ShareGrantFor(ctx, userID, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			return vehicleAccess{}, errNoVehicleAccess
		}
		return vehicleAccess{}, fmt.Errorf("resolve share grant(vehicle=%s): %w", vehicleID, err)
	}
	if !need.grants(grant) {
		return vehicleAccess{}, errNoVehicleAccess
	}
	return vehicleAccess{Role: auth.RoleViewer, Grant: grant}, nil
}

// vehicleAccessForOwnerOnly is the gate for a surface no share of any shape
// opens (MYR-369: the drives surfaces).
//
// It takes no share reader and issues no query, which is the point — a surface
// that is owner-only should not be able to consult a grant at all, so there is
// nothing for a future edit to widen by passing a different capability. The
// error is the same errNoVehicleAccess sentinel every other denial produces, so
// callers keep their existing 403 branch and a viewer learns nothing about why.
func vehicleAccessForOwnerOnly(_ context.Context, userID, ownerUserID string) (vehicleAccess, error) {
	if ownerUserID != userID {
		return vehicleAccess{}, errNoVehicleAccess
	}
	return vehicleAccess{Role: auth.RoleOwner}, nil
}

// denyVehicleAccess writes the standard 403 for a failed access check and logs
// it. Kept next to vehicleAccessFor so every caller emits the same code and the
// same log shape.
//
// The message is deliberately identical whether the caller has no grant at all,
// a suspended one, or a live one lacking the required capability: telling them
// which would let a base-capability viewer enumerate exactly what they are
// missing, and would tell a suspended viewer they have been suspended rather
// than simply that they cannot see the car.
func denyVehicleAccess(w http.ResponseWriter, logger *slog.Logger, surface, vehicleID, userID string) {
	logger.Warn(surface+": vehicle access denied",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
	)
	wserrors.WriteErrorEnvelope(w, logger, http.StatusForbidden,
		wserrors.ErrCodeVehicleNotOwned, "you do not have access to this vehicle")
}
