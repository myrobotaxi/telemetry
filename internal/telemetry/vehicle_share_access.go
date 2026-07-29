package telemetry

import (
	"context"
	"errors"
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

// VehicleShareReader resolves the tier a caller holds over a vehicle through an
// ACCEPTED share. Implementations return an error wrapping sdk.ErrNotFound when
// there is no such grant — which callers MUST treat as "no access", never as a
// default tier.
type VehicleShareReader interface {
	SharePermissionFor(ctx context.Context, userID, vehicleID string) (auth.SharePermission, error)
}

// vehicleAccess is the resolved answer for one (caller, vehicle) pair.
type vehicleAccess struct {
	// Role is owner or viewer.
	Role auth.Role
	// Tier is the share tier for a viewer, and empty for an owner — an
	// owner is not on a tier. Branch on Role before reading it.
	Tier auth.SharePermission
}

// errNoVehicleAccess is the internal denial sentinel. Handlers convert it to
// the status their endpoint's rule prescribes (403 for the ones whose existence
// is already established by a successful row read, 404 where the vehicle's
// existence must stay hidden) — it is never surfaced to a client verbatim.
var errNoVehicleAccess = errors.New("no access to vehicle")

// vehicleAccessFor resolves whether userID may act on a vehicle whose owner is
// ownerUserID, requiring at least minTier when the caller is not the owner.
//
// ownerUserID comes from a row the caller ALREADY read, so this costs one extra
// query only on the non-owner path — the owner path, which is the overwhelming
// majority of traffic, is unchanged.
//
// Fail-closed in three distinct ways, all deliberate:
//   - a nil shares reader denies every non-owner (a deployment that forgot to
//     wire sharing does not silently grant viewer access);
//   - a missing grant denies;
//   - a grant BELOW minTier denies, rather than being rounded up.
//
// A transient lookup failure is returned as a real error, NOT as a denial, so
// the handler answers 500 and the outage is visible instead of looking like a
// permissions problem.
func vehicleAccessFor(
	ctx context.Context,
	shares VehicleShareReader,
	userID, vehicleID, ownerUserID string,
	minTier auth.SharePermission,
) (vehicleAccess, error) {
	if ownerUserID == userID {
		return vehicleAccess{Role: auth.RoleOwner}, nil
	}
	if shares == nil {
		return vehicleAccess{}, errNoVehicleAccess
	}

	tier, err := shares.SharePermissionFor(ctx, userID, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			return vehicleAccess{}, errNoVehicleAccess
		}
		return vehicleAccess{}, err
	}
	if !tier.AtLeast(minTier) {
		return vehicleAccess{}, errNoVehicleAccess
	}
	return vehicleAccess{Role: auth.RoleViewer, Tier: tier}, nil
}

// denyVehicleAccess writes the standard 403 for a failed access check and logs
// it. Kept next to vehicleAccessFor so every caller emits the same code and the
// same log shape.
//
// The message is deliberately identical whether the caller has no grant at all
// or a grant one tier too low: telling them which would let a `live` viewer
// enumerate exactly what they are missing.
func denyVehicleAccess(w http.ResponseWriter, logger *slog.Logger, surface, vehicleID, userID string) {
	logger.Warn(surface+": vehicle access denied",
		slog.String("vehicle_id", vehicleID),
		slog.String("user_id", userID),
	)
	wserrors.WriteErrorEnvelope(w, logger, http.StatusForbidden,
		wserrors.ErrCodeVehicleNotOwned, "you do not have access to this vehicle")
}
