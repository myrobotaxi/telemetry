// Create-request validation for the MYR-184 sharing surface. Split out of
// share_invite_handler.go to keep that file under the 300-line cap: this is a
// pure function over the request body with no handler state, so it reads
// perfectly well on its own.

package telemetry

import "strings"

// validateCreateInvite checks the body and normalizes the vehicle set.
// Returns a non-empty reason when the request must be rejected 400.
func validateCreateInvite(body createShareInviteRequest, pathVehicleID, ownerID string) (in ShareInviteCreateInput, rejectReason string) {
	label := strings.TrimSpace(body.Label)
	switch {
	case label == "":
		return ShareInviteCreateInput{}, "label is required"
	case len(label) > maxShareInviteLabelLen:
		return ShareInviteCreateInput{}, "label is too long"
	case !validSharePermission(body.Permission):
		return ShareInviteCreateInput{}, "permission must be one of live, live_history, rides"
	}

	// vehicleIds is OPTIONAL: omitting it is exactly equivalent to sending
	// the path vehicle alone.
	ids := body.VehicleIDs
	if body.VehicleIDs == nil {
		ids = []string{pathVehicleID}
	}
	if len(ids) == 0 {
		return ShareInviteCreateInput{}, "vehicleIds must not be empty"
	}
	if len(ids) > maxShareInviteVehicles {
		return ShareInviteCreateInput{}, "vehicleIds exceeds the per-invite limit"
	}

	// The PATH vehicle is what authorizes the call, so a set that omits it
	// is rejected rather than silently extended: accepting it would let a
	// caller use one owned vehicle's path to mint invites for a set that
	// never included it.
	deduped := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	includesPath := false
	for _, id := range ids {
		if id == "" {
			return ShareInviteCreateInput{}, "vehicleIds must not contain empty ids"
		}
		if id == pathVehicleID {
			includesPath = true
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		deduped = append(deduped, id)
	}
	if !includesPath {
		return ShareInviteCreateInput{}, "vehicleIds must include the vehicle in the path"
	}

	return ShareInviteCreateInput{
		OwnerUserID:   ownerID,
		PathVehicleID: pathVehicleID,
		VehicleIDs:    deduped,
		Label:         label,
		Permission:    body.Permission,
	}, ""
}
