package telemetry

import (
	"context"
	"fmt"
	"log/slog"
)

// The vehicle half of the account-deletion sequence (MYR-355, MYR-593): read
// the fleet ONCE, stop it streaming at Tesla, then tear the rows down. Split
// out of account_deletion_sequence.go so both files stay inside the 300-line
// cap. The ordering argument — why the Tesla call has to precede the token
// purge — lives at the call site in run(), where the order is what you can see.

// ownedVehicleRef pairs one car with the owner id it is filed under, because
// after an identity convergence that is NOT the same id for every car in the
// account and both later steps need the right one: the teardown enforces
// ownership at the SQL layer, and the Tesla token is stored per owner id.
type ownedVehicleRef struct {
	OwnerID string
	Vehicle OwnedVehicle
}

// listOwnedVehicles reads the whole account's fleet across the identity
// closure, in one pass, before anything is torn down.
//
// It runs per id in the closure: after an identity convergence the cars are
// filed under the canonical id while the caller's token still names the
// abandoned one, so listing by the subject alone would find no cars to tear
// down and report a clean zero over a garage full of them.
func (h *AccountDeletionHandler) listOwnedVehicles(ctx context.Context, scope AccountDeletionScope) ([]ownedVehicleRef, error) {
	var owned []ownedVehicleRef
	for _, ownerID := range scope.IDs {
		vehicles, err := h.deps.Vehicles.ListOwnedVehicles(ctx, ownerID)
		if err != nil {
			return nil, fmt.Errorf("list owned vehicles: %w", err)
		}
		for _, v := range vehicles {
			owned = append(owned, ownedVehicleRef{OwnerID: ownerID, Vehicle: v})
		}
	}
	return owned, nil
}

// deleteStreamConfigs stops each of the account's cars streaming at Tesla and
// returns how many configs Tesla accepted a delete for (MYR-593).
//
// It goes through the SAME StreamConfigTeardown the per-vehicle teardown
// endpoint uses, rather than a second copy of the call, so the two severing
// paths cannot drift — the drift is what MYR-593 was.
//
// No error return, by design. Every outcome short of success is a car we could
// not reach, and the account is going either way; see the type doc on
// StreamConfigTeardown. A nil deleter (no proxy configured, and every test)
// makes the whole step a no-op.
func (h *AccountDeletionHandler) deleteStreamConfigs(ctx context.Context, owned []ownedVehicleRef) int {
	if h.deps.StreamConfigs == nil {
		if len(owned) > 0 {
			// Worth one line: on a deployment with no proxy wired this is the
			// moment the cost leak becomes unfixable, and an operator should be
			// able to find it in a log rather than in a Tesla invoice.
			h.logger.Warn("account deletion: no Tesla config deleter wired — owned cars may keep streaming",
				slog.Int("vehicle_count", len(owned)),
			)
		}
		return 0
	}
	deleted := 0
	for _, ref := range owned {
		if h.deps.StreamConfigs.DeleteStreamConfig(ctx, ref.OwnerID, ref.Vehicle.VIN) {
			deleted++
		}
	}
	return deleted
}

// tearDownOwnedVehicles runs the existing per-vehicle teardown for every car in
// the already-read fleet, returning how many were actually removed. A car that
// is already gone counts as done, not as a failure — that is what makes the
// step re-runnable. The FIRST real failure aborts: the remaining cars keep their
// data and a re-run picks them up, which is strictly better than pressing on
// and reporting success over a half-finished teardown.
func (h *AccountDeletionHandler) tearDownOwnedVehicles(ctx context.Context, owned []ownedVehicleRef) (int, error) {
	removed := 0
	for _, ref := range owned {
		result, err := h.deps.Teardown.RemoveVehicle(ctx, ref.OwnerID, ref.Vehicle.ID)
		if err != nil {
			return removed, fmt.Errorf("remove vehicle %s: %w", ref.Vehicle.ID, err)
		}
		if result.Removed {
			removed++
		}
	}
	return removed, nil
}
