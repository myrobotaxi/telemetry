package telemetry

import (
	"context"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Owner DECLINE (POST /api/ride-requests/{id}/decline) and the party/role
// checks it shares with accept.
//
// MYR-360 WIDENED THE §7.8 MATRIX, NARROWLY. Before this, the `accepted` row's
// `declined` cell was an unconditional 409: `rideDecidableFrom` was
// `{requested}` and the fast-path check hard-rejected anything else. An owner
// who paused ride sharing while a CONFIRMED FUTURE RESERVATION was outstanding
// therefore had no way to spare the rider — the reservation sweeper HOLDS at
// due time (its pause check runs before the claim), retries, and the ride
// resolves `reservation_expired` at the 30-minute lateness ceiling, so the
// rider learns nobody is coming half an hour AFTER their pickup time.
//
// That hold-then-expire backstop STAYS: it is what covers a crash or an
// offline pause, where nobody was around to decline anything. This endpoint
// makes the COMMON path humane instead — the client warns the owner before
// pausing and offers an immediate decline.
//
// SCHEDULED ONLY. An accepted INSTANT ride still returns 409. A car already
// dispatched to a rider standing on a sidewalk is a different situation with
// different consequences (the nav is already in the car, the rider is already
// outside), and it is out of scope here. The narrowing is expressed as a
// property of the ROW — `scheduled_for IS NOT NULL` — which is immutable for
// the life of a ride: ResolveReschedule moves the reservation INSTANT but
// never turns a reservation into an instant ride or back. That is what makes
// it safe to derive the guarded write's allowed-from set from a pre-check read.
//
// DECLINING PAST THE RESERVATION INSTANT IS ALLOWED, deliberately. A
// reservation whose `scheduledFor` has come and gone but which is still
// `accepted` (the sweeper is holding it, or its dispatch failed) remains
// declinable. The ride is still `accepted`, the owner is still the decider, and
// an explicit decline is strictly better for the rider than the silent
// `reservation_expired` they would otherwise get at the lateness ceiling. There
// is no race with the sweeper either: the sweeper's claim re-checks
// `status = 'accepted'` in the same guarded UPDATE that stamps its latch, so a
// declined reservation simply matches no row and the sweeper does nothing at
// all — no push, no outcome, no `ride.due`.
//
// UNCHANGED: decline stays OWNER-ONLY, stays ungated on vehicle status and on
// the ride-sharing pause (an owner may always decline — gating it would trap
// requests on a car its owner has withdrawn, the opposite of what the pause is
// for), and still publishes the standard `ride.status.changed`, which carries
// the rider's lifecycle push and the WS frame unicast to both parties.

// ServeDecline handles POST /api/ride-requests/{id}/decline. Owner-only;
// legal from `requested` → `declined` for any ride, and additionally from
// `accepted` → `declined` for a SCHEDULED ride (MYR-360). Every other current
// status is 409 conflict.
func (h *RideRequestHandler) ServeDecline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, from, ok := h.loadForOwnerDecline(ctx, w, r, userID)
	if !ok {
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, from, rideStatusDeclined)
	if !ok {
		return
	}

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// Allowed-from sets for an owner decline. rideDeclinableFrom is the
// pre-MYR-360 set and still applies to every INSTANT ride;
// rideDeclinableFromScheduled adds the one new legal edge. Both must stay in
// lockstep with the rest-api.md §7.8 matrix.
var (
	rideDeclinableFrom          = []string{rideStatusRequested}
	rideDeclinableFromScheduled = []string{rideStatusRequested, rideStatusAccepted}
)

// declinableFrom returns the set of statuses this ride may legally be declined
// from. It is a property of the ROW, not of the snapshot we happen to have
// read, which is precisely what keeps the race discipline intact: the set
// handed to the guarded UpdateStatusFrom write is the full legal set, so a
// status that moved between the pre-check read and the write is arbitrated by
// the database exactly as before — and widens ONLY for the scheduled case.
func declinableFrom(rec RideRequestData) []string {
	if rec.ScheduledFor != nil {
		return rideDeclinableFromScheduled
	}
	return rideDeclinableFrom
}

// loadForOwnerDecline fetches the ride for a DECLINE: party + owner-only
// checks, then the legality fast-path against the row's own declinable set
// (→ 409 conflict; friendly message — the guarded write in mutateStatus is
// what actually decides under concurrency). Returns the allowed-from set to
// hand that write. On any failure the response has been written and ok=false.
func (h *RideRequestHandler) loadForOwnerDecline(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, []string, bool) {
	rec, ok := h.loadOwnerParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, nil, false
	}

	from := declinableFrom(rec)
	if !containsStatus(from, rec.Status) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be declined from status "+rec.Status)
		return RideRequestData{}, nil, false
	}

	return rec, from, true
}

// loadOwnerParty fetches the ride by {id} and enforces the two authorization
// rules every owner decision shares: party membership (a caller who is neither
// rider nor owner gets a 404, the same no-existence-leak rule as loadForParty)
// and then the owner-only role (the rider IS a party but cannot decide → 403).
// Lifecycle legality is the caller's business — accept and decline no longer
// have the same legal-from set.
func (h *RideRequestHandler) loadOwnerParty(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}

	if userID != rec.OwnerID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the vehicle owner may decide this request")
		return RideRequestData{}, false
	}

	return rec, true
}

// containsStatus reports whether status is a member of set.
func containsStatus(set []string, status string) bool {
	for _, s := range set {
		if s == status {
			return true
		}
	}
	return false
}
