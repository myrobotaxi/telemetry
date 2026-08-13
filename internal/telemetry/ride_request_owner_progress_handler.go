package telemetry

import (
	"context"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Owner progress endpoints (P10 owner-driven dispatch v2, MYR-270). The owner
// drives the pickup/dropoff handshake that brackets the rider's start:
//
//   - POST /api/ride-requests/{id}/picked-up   — OWNER-only, accepted→arrived
//     ("Picked up": the rider is aboard, awaiting the rider's Start).
//   - POST /api/ride-requests/{id}/dropped-off — OWNER-only, enroute→completed
//     ("Dropped off": the ride is finished).
//
// Completion is now OWNER-confirmed: MYR-270 removed the MYR-265 drive-end
// auto-completion (internal/ridecomplete), so a car parking no longer closes a
// ride — the owner does, here.
//
// Both endpoints are owner-only (the rider is a party but cannot confirm → 403;
// a non-party → 404), enforce transition legality in the handler (the store
// stays a dumb persistence layer), are idempotent (a repeat of the destination
// state is a 200 no-op; every other state is 409 conflict), and publish the
// summary ride_status_changed via mutateStatus. NEITHER pushes Tesla nav — the
// only nav push in this flow is the DROPOFF, fired by the rider start endpoint.

// ServePickedUp handles POST /api/ride-requests/{id}/picked-up. Owner-only;
// legal only from accepted → arrived (409 otherwise). Idempotent: an already
// `arrived` ride is a 200 no-op that re-returns the current record.
//
// RESERVATION DORMANCY (MYR-376). A pickup is additionally refused — 409, the
// SAME `conflict` code, a message naming the reason — when the ride is a
// SCHEDULED one that is neither dispatched nor yet due (`scheduledFor` set and
// in the FUTURE, and dispatch not resolved `sent`). A reservation is DORMANT
// from accept until the earlier of its dispatch and its due instant: the car
// has been told nothing, so there is no pickup to confirm. In production an
// owner could tap "Picked up" the day before the ride was due and strand it at
// `arrived` with no legal exit.
//
// Dormancy ENDS AT THE DUE INSTANT, dispatched or not — a reservation the
// sweeper failed (`reservation_expired`), skipped, or never reached is picked
// up normally once `scheduledFor` passes, which is what makes §7.8's "may
// still cancel or proceed manually" promise real rather than a dead end. The
// only thing refused is a pickup BEFORE the ride was due.
//
// The refusal is enforced INSIDE the guarded write (mutateStatusDispatched),
// never as a pre-check, so it is race-correct against a concurrently
// dispatching sweeper and against the clock crossing `scheduledFor`. INSTANT
// rides are entirely unaffected, including ones whose nav push failed or was
// skipped.
func (h *RideRequestHandler) ServePickedUp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForOwner(ctx, w, r, userID)
	if !ok {
		return
	}

	// Idempotent no-op: the owner already confirmed pickup.
	if rec.Status == rideStatusArrived {
		h.writeJSON(w, http.StatusOK, h.rideWire(rec))
		return
	}
	// Only accepted → arrived is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency.
	if rec.Status != rideStatusAccepted {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be marked picked-up from status "+rec.Status)
		return
	}

	// The DORMANCY-guarded write (MYR-376): the reservation predicate rides the
	// same UPDATE as the status guard, so neither refusal can be raced past.
	updated, ok := h.mutateStatusDispatched(ctx, w, rec, ridePickedUpFrom, rideStatusArrived)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, h.rideWire(updated))
}

// ServeDroppedOff handles POST /api/ride-requests/{id}/dropped-off. Owner-only;
// legal only from enroute → completed (409 otherwise). Idempotent: an already
// `completed` ride is a 200 no-op that re-returns the current record.
//
// ENDING EARLY IS LEGITIMATE, AND IT WRITES NOTHING TO THE STOP LIST (MYR-547).
//
// An owner may tap "Dropped off" while the trip still has stops the car never
// reached — the passenger asked to get out here, the plan changed, the last two
// stops are not happening. The server does not refuse that and does not need to
// ask: the guard is `enroute → completed` and nothing about a stop is part of
// it. (The CONFIRM belongs on the client, which knows it is showing "stop 2 of
// 4"; that half is iOS's.)
//
// What the completion deliberately does NOT do is rewrite the stops it is
// leaving behind, and the reason is that neither available value would be true:
//
//   - `completed` means "the vehicle arrived here and the stop is behind the
//     car". Stamping it on a stop the car never drove to FABRICATES AN ARRIVAL.
//     It is also lossy in the one direction that matters — it erases where the
//     ride actually ended, which is the whole subject of an early completion,
//     and it is the record a rider disputing a trip would be shown.
//   - demoting the in-progress stop from `current` to `upcoming` throws away the
//     same fact for a cosmetic gain, and `upcoming` ("the vehicle has not
//     started this leg yet") is a second falsehood about a leg it had started.
//
// So the rows stay exactly as the last observed arrival left them, and the
// READING is qualified instead: on a TERMINAL ride the stop statuses are a
// historical record of how far the car got — `completed` is a stop the car
// reached, and anything else is a stop it did not. `current`/`upcoming` are
// live claims only while the ride is live, which is how every consumer already
// reads them, because every consumer of leg progress gates on the ride's own
// status first (the arrival detector watches `enroute` rides only; the position
// poller stops on any terminal transition). No consumer is asked to change; the
// contract now says out loud what they already do.
//
// There is deliberately no fourth stop status for "skipped". A new enum member
// is a contract change for every SDK, to record something the pair
// (ride.status, stop.status) already expresses exactly.
func (h *RideRequestHandler) ServeDroppedOff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	rec, ok := h.loadForOwner(ctx, w, r, userID)
	if !ok {
		return
	}

	// Idempotent no-op: the ride is already completed.
	if rec.Status == rideStatusCompleted {
		h.writeJSON(w, http.StatusOK, h.rideWire(rec))
		return
	}
	// Only enroute → completed is legal. Friendly fast-path message; the guarded
	// write below is what actually decides under concurrency.
	if rec.Status != rideStatusEnroute {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be marked dropped-off from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideDroppedOffFrom, rideStatusCompleted)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, h.rideWire(updated))
}

// ridePickedUpFrom / rideDroppedOffFrom are the owner handshake allowed-from
// sets; must stay in lockstep with the fast-path checks above and the
// rest-api.md §7.8 matrix. The guarded write is what decides under concurrency
// — and for pickup that write also carries the MYR-376 dormancy predicate, so
// the allowed-from set is not the whole precondition.
var (
	ridePickedUpFrom   = []string{rideStatusAccepted}
	rideDroppedOffFrom = []string{rideStatusEnroute}
)

// loadForOwner fetches the ride, enforces party membership (non-party → 404,
// same no-existence-leak rule as loadForParty), then the owner-only role (the
// rider is a party but cannot confirm pickup/dropoff → 403). Unlike
// loadForOwnerDecision it applies NO status fast-path — the
// picked-up/dropped-off handlers each enforce their own transition legality and
// idempotent no-op.
func (h *RideRequestHandler) loadForOwner(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}
	if userID != rec.OwnerID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the vehicle owner may confirm this ride")
		return RideRequestData{}, false
	}
	return rec, true
}
