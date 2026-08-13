// PATCH /api/ride-requests/{id}/trip — the trip-shape edit (MYR-541,
// CLIENT-DIRECTED: "allow the users to change the pick up or drop off at
// anytime. Obv if pick up has past they can only change drop-off").
//
// THE WINDOW, from the client's rule:
//   - PICKUP editable while the car is still coming: requested/accepted.
//     Once arrived (the car is at the kerb) the pickup is history — the
//     refusal names the drop-off as the thing still editable.
//   - DROP-OFF editable through every live status.
//   - STOPS (MYR-539) take the DROP-OFF's window, because that is what they
//     are: the trip ahead of the car. What protects the trip BEHIND it is not
//     the status window but the stop rules themselves — a completed or current
//     stop cannot be removed or reordered, whatever the ride's status.
//
// THE ACL is the ride's participant set — rider and owner today, plus every
// joined group member when MYR-540's membership lands (this handler is where
// that list will be consulted). Edits apply INSTANTLY, no approval gate
// anywhere (client decision, 2026-08-13); the other participants hear about
// it by push and every live surface converges off the TripEdit-marked
// ride_status_changed frame's tripVersion.
//
// CONCURRENCY is the trip version: the editor echoes the version it holds and
// the guarded UPDATE refuses a stale one, so two people editing at once
// resolve to exactly one winner and one 409 — never a silent clobber.

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// rideTripPatchBody mirrors contracts $defs.RideRequestTripPatch (v0.34.0).
type rideTripPatchBody struct {
	Pickup  *ridePlaceWire `json:"pickup"`
	Dropoff *ridePlaceWire `json:"dropoff"`
	// Stops is the FULL desired list — REPLACE semantics, not diff ops
	// (MYR-539). A double pointer in effect: the outer nil is "the key was
	// absent, leave the stops alone", an empty array is "clear them".
	Stops       *[]rideStopPatchWire `json:"stops"`
	TripVersion *int                 `json:"tripVersion"`
}

// RideTripEditData is the telemetry-layer trip edit handed to the store seam.
type RideTripEditData struct {
	Pickup  *RidePlaceData
	Dropoff *RidePlaceData
	// Stops nil = leave the list as it is; non-nil (possibly empty) = replace
	// it with exactly this, in this order.
	Stops         *[]RideStopEditData
	ExpectVersion int
}

// tripPickupEditableFrom / tripDropoffEditableFrom are the two windows. The
// pickup's must stay strictly narrower — the client's own rule.
var (
	tripPickupEditableFrom  = []string{rideStatusRequested, rideStatusAccepted}
	tripDropoffEditableFrom = []string{rideStatusRequested, rideStatusAccepted, rideStatusArrived, rideStatusEnroute}
)

// ServeTripPatch handles PATCH /api/ride-requests/{id}/trip.
func (h *RideRequestHandler) ServeTripPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return
	}

	edit, msg := decodeTripPatch(r)
	if msg != "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, msg)
		return
	}

	// Friendly fast-path refusals; the guarded write decides under concurrency.
	if !rideStatusIn(rec.Status, tripDropoffEditableFrom) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "the ride has ended — the trip can no longer be changed")
		return
	}
	if edit.Pickup != nil && !rideStatusIn(rec.Status, tripPickupEditableFrom) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "the pickup has passed — only the drop-off can be changed")
		return
	}

	// The WINDOW handed to the guarded write is the intersection of the edited
	// legs' windows: a body carrying a pickup must lose to a concurrent
	// picked-up, and one carrying only a drop-off must not.
	from := tripDropoffEditableFrom
	if edit.Pickup != nil {
		from = tripPickupEditableFrom
	}

	updated, err := h.store.UpdateTrip(ctx, rec.ID, edit, from)
	if err != nil {
		// An unknown stop id is the one refusal from this write that is a BAD
		// REQUEST rather than a race: refetching and re-applying cannot fix an
		// id the ride does not have, so it must not be dressed as a 409 the
		// client would retry (MYR-539).
		if errors.Is(err, ErrRideStopUnknown) {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
				"stops: unknown stop id — omit the id for a new stop")
			return
		}
		h.writeTransitionError(w, rec.ID, "trip", err)
		return
	}

	h.publishTripChanged(ctx, rec, updated, edit, userID)
	h.writeJSON(w, http.StatusOK, h.rideWire(updated))
}

// publishTripChanged emits the edit's two events: the TripEdit-marked
// ride_status_changed (the WS refetch signal — status unchanged, tripVersion
// bumped) and the internal ride.trip_changed seam the dispatcher's nav
// re-share and the participants' pushes consume.
func (h *RideRequestHandler) publishTripChanged(
	ctx context.Context,
	before, updated RideRequestData,
	edit RideTripEditData,
	editorUserID string,
) {
	h.publish(ctx, events.RideStatusChangedEvent{
		RideRequestID:  updated.ID,
		VehicleID:      updated.VehicleID,
		RiderID:        updated.RiderID,
		OwnerID:        updated.OwnerID,
		Status:         updated.Status,
		RequesterName:  updated.RequesterName,
		TripVersion:    updated.TripVersion,
		TripEdit:       true,
		PreviousStatus: before.Status,
		ScheduledFor:   updated.ScheduledFor,
		UpdatedAt:      updated.UpdatedAt,
	})

	evt := events.RideTripChangedEvent{
		RideRequestID:    updated.ID,
		VehicleID:        updated.VehicleID,
		RiderID:          updated.RiderID,
		OwnerID:          updated.OwnerID,
		EditorUserID:     editorUserID,
		Status:           updated.Status,
		PickupDispatched: before.DispatchStatus != nil && *before.DispatchStatus == "sent",
		RequesterName:    updated.RequesterName,
		UpdatedAt:        updated.UpdatedAt,
	}
	if edit.Pickup != nil {
		evt.NewPickup = toEventPlacePtr(*edit.Pickup)
	}
	if edit.Dropoff != nil {
		evt.NewDropoff = toEventPlacePtr(*edit.Dropoff)
	}
	if edit.Stops != nil {
		// The dispatcher cannot infer the current leg's target from the
		// endpoints once a trip has stops, so the server states it: the place
		// the car should be driving to AFTER this edit, computed from the
		// updated record the write returned (MYR-539).
		evt.StopsChanged = true
		evt.LegTarget = toEventPlacePtr(currentLegTarget(updated))
	}
	h.publish(ctx, evt)
}

// currentLegTarget is where the car is (or will be) driving for the ride's
// current post-pickup leg: the CURRENT stop when the trip has one, else the
// earliest upcoming stop, else the drop-off. It reads the record the guarded
// write returned, which already carries the promotion that write performed —
// so the dispatcher and the arrival detector are told the same target the
// database holds.
func currentLegTarget(rec RideRequestData) RidePlaceData {
	var upcoming *RidePlaceData
	for i := range rec.Stops {
		switch rec.Stops[i].Status {
		case rideStopCurrent:
			return rec.Stops[i].Place
		case rideStopUpcoming:
			if upcoming == nil {
				upcoming = &rec.Stops[i].Place
			}
		}
	}
	if upcoming != nil {
		return *upcoming
	}
	return rec.Dropoff
}

// decodeTripPatch decodes and validates the PATCH body, returning the edit or
// the 400 reason. Split from ServeTripPatch so the request-shape rules read as
// one unit and the handler stays at the routing level.
//
//nolint:gocritic // the (edit, reason) pair matches validateRidePlace's own shape
func decodeTripPatch(r *http.Request) (RideTripEditData, string) {
	var body rideTripPatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return RideTripEditData{}, "invalid JSON body"
	}
	if body.TripVersion == nil {
		return RideTripEditData{}, "tripVersion is required"
	}
	if body.Pickup == nil && body.Dropoff == nil && body.Stops == nil {
		return RideTripEditData{}, "nothing to change: provide pickup, dropoff and/or stops"
	}
	edit := RideTripEditData{ExpectVersion: *body.TripVersion}
	if body.Stops != nil {
		stops, msg := validateRideStops(*body.Stops)
		if msg != "" {
			return RideTripEditData{}, msg
		}
		edit.Stops = &stops
	}
	if body.Pickup != nil {
		place, msg := validateRidePlace("pickup", body.Pickup)
		if msg != "" {
			return RideTripEditData{}, msg
		}
		edit.Pickup = &place
	}
	if body.Dropoff != nil {
		place, msg := validateRidePlace("dropoff", body.Dropoff)
		if msg != "" {
			return RideTripEditData{}, msg
		}
		edit.Dropoff = &place
	}
	return edit, ""
}

// validateRideStops turns the submitted list into the store-layer edit after
// enforcing $defs.RideStopPatch: `place` is REQUIRED on every entry (including
// one that keeps an existing id — the list is the full desired trip, so each
// entry states its own place) and is validated exactly as an endpoint is. An
// entry's `id`, when present, must be non-empty; whether the ride actually HAS
// that stop is the store's question, because only the guarded write sees the
// list it is being applied to.
//
// An EMPTY array is valid and means "clear every stop": a trip is allowed to go
// back to being two endpoints.
//
//nolint:gocritic // the (stops, reason) pair matches validateRidePlace's own shape
func validateRideStops(entries []rideStopPatchWire) ([]RideStopEditData, string) {
	out := make([]RideStopEditData, 0, len(entries))
	for i := range entries {
		field := fmt.Sprintf("stops[%d].place", i)
		place, msg := validateRidePlace(field, entries[i].Place)
		if msg != "" {
			return nil, msg
		}
		stop := RideStopEditData{Place: place}
		if entries[i].ID != nil {
			if *entries[i].ID == "" {
				return nil, fmt.Sprintf("stops[%d].id must not be empty when present", i)
			}
			stop.ID = *entries[i].ID
		}
		out = append(out, stop)
	}
	return out, ""
}

// toEventPlacePtr projects a place onto the events shape, address flattened —
// the same projection every other dispatch-bound place takes.
func toEventPlacePtr(p RidePlaceData) *events.RidePlace {
	out := &events.RidePlace{Latitude: p.Latitude, Longitude: p.Longitude, Label: p.Label}
	if p.Address != nil {
		out.Address = *p.Address
	}
	return out
}

// rideStatusIn reports membership in a status window.
func rideStatusIn(status string, window []string) bool {
	for _, s := range window {
		if s == status {
			return true
		}
	}
	return false
}
