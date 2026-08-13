package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// RideRequestHandler serves the rider-facing ride-request REST surface
// (P10, MYR-174): POST /api/ride-requests (create), POST
// /api/ride-requests/{id}/cancel, GET /api/ride-requests/{id} (party-only),
// GET /api/ride-requests (rider's own list). The owner-facing surface
// (incoming feed, accept/decline — MYR-175) is served by the sibling
// RideRequestOwnerHandler.
//
// Authorization model. The create access check is the vehicle owner OR a
// viewer holding an accepted share at the `rides` tier — the top tier, whose
// entire increment over `live_history` is exactly this: "send the car to pick
// them up".
//
// MYR-184 CORRECTED THE PREDICTION THIS COMMENT USED TO MAKE. It previously
// said shared-viewer requests would "land when the access set widens, with no
// change to this handler". That was wrong, and rest-api.md §7.8 repeated the
// same error. The owner-equality check below is a SEPARATE code path from the
// read-side access set: widening GetUserVehicles put shared cars in a viewer's
// list and let them read the snapshot, but this check would still have refused
// every one of their ride requests. Granting `rides` required changing it, and
// this is that change.
//
// The ride's ownerId is still the VEHICLE's owner, never the requester, so a
// rider≠owner request routes to the car's owner for accept/decline exactly as
// an owner's own request does.
type RideRequestHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	store    RideRequestStore
	events   RideEventPublisher
	// shares admits a non-owner rider holding an accepted `rides` share.
	// Nil keeps the endpoint owner-only — the fail-closed default.
	shares VehicleShareReader
	// activities is the Live Activity token registry (MYR-172). Nil leaves the
	// §7.21 endpoints answering 500 — a deployment error, not a runtime state.
	activities LiveActivityRegistry
	// bookedWindowsMax is the widest [from, to) span §7.22 will answer about,
	// INJECTED from store.MaxBookedWindowRange by wiring.go (MYR-385). It is a
	// field rather than a const here because this package must not import
	// internal/store, and a restated literal is a cap that drifts from the one
	// the store is actually built around. Zero — the option not wired — leaves
	// the endpoint answering 500, the same fail-closed reading `activities`
	// gets: a deployment error, not a runtime state.
	bookedWindowsMax time.Duration
	// links signs the MYR-540 group-ride join URL (RideRequest.shareUrl). Nil
	// omits the key from every projection — a keyless deployment emits no link
	// rather than an unsigned one the landing shell would bounce. It is the
	// SAME signer the MYR-368 invite links use, under a domain-separated
	// payload prefix; see ride_link.go.
	links *InviteLinkSigner
	// members answers "is this caller a joined member of this ride" — the
	// MYR-540 ACL widening. Nil keeps every party check at rider-or-owner, the
	// fail-closed default and the pre-MYR-540 behaviour.
	members RideMemberReader
	// access busts a fresh member's cached vehicle set so the car they just
	// joined a ride in appears on their very next request (MYR-540).
	access AccessCacheInvalidator
	// dispatchNow runs the reservation sweeper's claimed dispatch path for the
	// owner's MYR-556 "send it now" tap. Nil leaves POST
	// /api/ride-requests/{id}/dispatch-now answering 500 — a deployment error,
	// not a runtime state, and deliberately not a fail-open: there is no safe
	// way to pretend a car was sent.
	dispatchNow ReservationDispatcher
	// joinLimiter is the per-user attempt budget on POST /api/ride-requests/join.
	// Its OWN instance, never shared with the invite redeem's: the two code
	// spaces are separate, so spending one endpoint's allowance must not close
	// the other. Always non-nil — built by the constructor.
	joinLimiter *redeemLimiter
	logger      *slog.Logger
}

// RideMemberReader is the membership probe the party-scoped ride surfaces widen
// through (MYR-540). Defined at the consumer site; satisfied by the cmd-side
// adapter over store.RideRequestRepo.
type RideMemberReader interface {
	// IsRideMember reports whether userID has joined rideID. An error is a
	// lookup failure, never a denial.
	IsRideMember(ctx context.Context, rideID, userID string) (bool, error)
}

// WithRideLinkSigner wires the group-ride link signer (MYR-540).
func WithRideLinkSigner(signer *InviteLinkSigner) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.links = signer
	}
}

// WithRideMemberReader widens every party-scoped ride surface to joined group
// members (MYR-540).
func WithRideMemberReader(members RideMemberReader) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.members = members
	}
}

// rideWire projects a ride onto the wire object, minting `shareUrl` with this
// handler's signer and reading the clock ONCE for the whole projection.
//
// One clock reading per response, not per field: a page of rides must not be
// able to include one ride whose link survived the linger check and another
// whose did not because the loop crossed a five-minute boundary mid-flight.
func (h *RideRequestHandler) rideWire(d RideRequestData) rideRequestWire {
	return toRideRequestWire(d, h.links, time.Now())
}

// RideRequestOption configures optional dependencies on RideRequestHandler.
type RideRequestOption func(*RideRequestHandler)

// WithRideShareReader admits riders who are not the vehicle's owner but hold
// an accepted share at the `rides` tier (MYR-184).
func WithRideShareReader(shares VehicleShareReader) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.shares = shares
	}
}

// NewRideRequestHandler constructs the rider-facing handler. events may be
// nil (WS/dispatch notifications become no-ops) — useful in tests that only
// exercise the HTTP contract.
func NewRideRequestHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	store RideRequestStore,
	publisher RideEventPublisher,
	logger *slog.Logger,
	opts ...RideRequestOption,
) *RideRequestHandler {
	h := &RideRequestHandler{
		auth:        tokens,
		vehicles:    vehicles,
		store:       store,
		events:      publisher,
		joinLimiter: newRedeemLimiter(redeemRateLimit, redeemRateWindow),
		logger:      logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeCancel handles POST /api/ride-requests/{id}/cancel. PARTY-AWARE from
// MYR-522 (client decision, 2026-08-12: "add ability for owner to cancel ride
// from their end as well"):
//
//   - The RIDER may cancel from ANY live status — requested, accepted,
//     arrived, even enroute with themselves aboard (MYR-537, client directive
//     2026-08-12: "Allow rider to cancel ride at anytime even after it's
//     started"). A mid-ride cancel cannot clear the car's dash nav (Tesla has
//     no cancel-navigation API), so the owner's push is the stand-down.
//   - The OWNER may cancel from accepted/arrived. A `requested` ride keeps the
//     existing DECLINE as the owner's one exit (two owner verbs for one moment
//     would be two copies of one decision), and once the rider is aboard
//     (`enroute`) there is no owner cancel at all — ending early is what
//     "Dropped off" already is.
//
// Both paths stamp WHO cancelled into the same guarded UPDATE that decides
// whether the cancel wins (`cancelled_by`, first writer wins), and both
// publish through the one shared transition path — so the WS frame, the
// rider's push and the nav stand-down ride the same delivery every other
// transition uses. Every other current status is a 409 conflict.
func (h *RideRequestHandler) ServeCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return
	}

	// CANCEL DOES NOT WIDEN TO GROUP MEMBERS (MYR-540). loadForParty now admits
	// a joined member — because reading and editing the trip are theirs — but
	// ENDING the ride is not: MYR-537 gave that to the requester and the owner,
	// and a member leaving is not a cancel. The two verbs are not the same size,
	// and there is deliberately no leave endpoint in v1 either.
	//
	// 403 rather than 404, and the difference is the point: this caller is a
	// party and can see the ride perfectly well in their own app, so pretending
	// it does not exist would read as a bug. They are simply not the one who
	// gets to end it.
	//
	// This check must come BEFORE the owner fork below, which would otherwise
	// read "not the rider" as "therefore the owner" and let a member cancel
	// somebody else's ride under the owner's rules.
	if userID != rec.RiderID && userID != rec.OwnerID {
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"only the person who booked this ride, or the vehicle's owner, can cancel it")
		return
	}

	// A self-ride caller (owner riding their own car) is BOTH parties and takes
	// the rider path — their cancel is their own, exactly as before MYR-522.
	if userID != rec.RiderID {
		// The owner's cancel (MYR-522). Non-parties were already 404'd by
		// loadForParty and members refused above, so this caller is the owner.
		h.serveOwnerCancel(ctx, w, rec)
		return
	}

	if !cancellableFrom(rec.Status) {
		// Friendly fast-path message; the guarded write below is what
		// actually decides under concurrency.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be cancelled from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatusCancelled(ctx, w, rec, rideCancellableFrom, rideCancelledByRider)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, h.rideWire(updated))
}

// serveOwnerCancel is the owner's arm of ServeCancel (MYR-522). The two
// refusals that are NOT plain status conflicts get sentences that name the
// door the owner should use instead — a refusal the server can explain must
// be explained (MYR-329's rule): a `requested` ride's exit is DECLINE, and a
// ride with the rider aboard ends only through "Dropped off".
func (h *RideRequestHandler) serveOwnerCancel(ctx context.Context, w http.ResponseWriter, rec RideRequestData) {
	switch rec.Status {
	case rideStatusRequested:
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "decline a requested ride instead of cancelling it")
		return
	case rideStatusEnroute:
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "the rider is aboard — end the ride with dropped-off")
		return
	}
	if !ownerCancellableFrom(rec.Status) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be cancelled from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatusCancelled(ctx, w, rec, rideOwnerCancellableFrom, rideCancelledByOwner)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, h.rideWire(updated))
}

// The two cancelled_by stamps. Written once here and consumed by the push
// notifier's copy fork; the wire projection carries them verbatim.
const (
	rideCancelledByRider = "rider"
	rideCancelledByOwner = "owner"
)

// rideCancellableFrom is the allowed-from set for a rider cancel; must stay
// in lockstep with cancellableFrom and the rest-api.md §7.8 matrix. Every
// LIVE status since MYR-537 — the rider's ride is theirs to end at any point.
var rideCancellableFrom = []string{rideStatusRequested, rideStatusAccepted, rideStatusArrived, rideStatusEnroute}

// rideOwnerCancellableFrom is the allowed-from set for an OWNER cancel
// (MYR-522); must stay in lockstep with ownerCancellableFrom and the
// rest-api.md §7.8 matrix. `requested` is deliberately absent (decline is
// that moment's owner verb) and so is `enroute` (the rider is aboard).
var rideOwnerCancellableFrom = []string{rideStatusAccepted, rideStatusArrived}

// The guarded-transition helpers (mutateStatus and its dormancy-guarded
// sibling) live in ride_request_status_mutation.go.

// cancellableFrom reports whether a rider cancel is legal from the given
// status. Legal only from requested/accepted; enroute/arrived (ride in
// progress) and the terminal states (declined/completed/cancelled) are not.
func cancellableFrom(status string) bool {
	switch status {
	case rideStatusRequested, rideStatusAccepted, rideStatusArrived, rideStatusEnroute:
		return true
	}
	return false
}

// ownerCancellableFrom reports whether an OWNER cancel is legal from the
// given status (MYR-522). Legal only from accepted/arrived — before the rider
// is aboard, after the request stopped being declinable.
func ownerCancellableFrom(status string) bool {
	return status == rideStatusAccepted || status == rideStatusArrived
}

// loadForParty fetches the ride by {id} and enforces party membership: a
// caller who is neither rider nor owner gets a 404 (no existence leak). On
// success returns the record; on any failure writes the response and returns
// ok=false.
func (h *RideRequestHandler) loadForParty(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	id := r.PathValue("id")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing ride request id")
		return RideRequestData{}, false
	}

	rec, err := h.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
			return RideRequestData{}, false
		}
		h.logger.Error("ride-request: lookup failed",
			slog.String("ride_request_id", id),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return RideRequestData{}, false
	}

	if !h.isParty(ctx, rec, userID) {
		// Non-party: return 404 rather than 403 so the server never
		// confirms the existence of a ride the caller has no relation to.
		h.logger.Warn("ride-request: non-party access",
			slog.String("ride_request_id", id),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
		return RideRequestData{}, false
	}

	return rec, true
}

// isParty reports whether userID is a party to the ride: its rider, the
// vehicle's owner, or — since MYR-540 — a JOINED GROUP MEMBER.
//
// THE MEMBER CHECK IS FREE ON EVERY ORDINARY RIDE. The two id comparisons come
// first and short-circuit, and the membership probe is skipped entirely unless
// the ride is a group ride, so a solo ride costs exactly what it did before.
//
// It reads the RECORD'S OWN member list when the record carries one, and only
// falls back to the store probe when it does not. Every §7.8 read attaches the
// list, so the fallback exists for the paths that hand this function a record
// from a leaner source — and asking the record first is what keeps the common
// case at zero queries.
//
// FAILS CLOSED on a lookup error: "we could not tell" collapses to "not a
// party", which on this path means a 404. That is the safe direction and the
// only one available — the alternative, admitting on an unreadable membership,
// would hand a stranger somebody's pickup coordinates on a database blip.
func (h *RideRequestHandler) isParty(ctx context.Context, rec RideRequestData, userID string) bool {
	if userID == rec.RiderID || userID == rec.OwnerID {
		return true
	}
	if !rec.GroupRide {
		return false
	}
	for i := range rec.Members {
		if rec.Members[i].UserID == userID {
			return true
		}
	}
	if h.members == nil {
		return false
	}
	member, err := h.members.IsRideMember(ctx, rec.ID, userID)
	if err != nil {
		h.logger.Error("ride-request: membership lookup failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		return false
	}
	return member
}

// authUser extracts + validates the bearer token, returning the userID.
func (h *RideRequestHandler) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", false
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("ride-request: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", false
	}
	return userID, true
}

// publish builds an Event envelope for the payload and publishes it,
// swallowing (logging) errors — the DB mutation already committed, so a
// dropped notification must not fail the HTTP request.
func (h *RideRequestHandler) publish(ctx context.Context, payload events.EventPayload) {
	if h.events == nil {
		return
	}
	if err := h.events.Publish(ctx, events.NewEvent(payload)); err != nil {
		h.logger.Warn("ride-request: publish event failed",
			slog.String("topic", string(payload.EventTopic())),
			slog.String("error", err.Error()),
		)
	}
}

// decodeCreateBody strictly decodes the request body (unknown keys are a 400,
// matching the schema's additionalProperties:false).
func (h *RideRequestHandler) decodeCreateBody(w http.ResponseWriter, r *http.Request) (rideRequestCreateBody, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body rideRequestCreateBody
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return rideRequestCreateBody{}, false
	}
	return body, true
}

// writeJSON marshals v as JSON with the given status code.
func (h *RideRequestHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("ride-request: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *RideRequestHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}

// writeErrorSub writes the same envelope with a typed sub-code, for the paths
// where the primary code does not tell the client what to do on its own.
func (h *RideRequestHandler) writeErrorSub(w http.ResponseWriter, status int, code wserrors.ErrorCode, sub wserrors.SubCode, msg string) {
	wserrors.WriteErrorEnvelopeSub(w, h.logger, status, code, sub, msg)
}

// writeRideActive writes the 409 `ride_active` response (MYR-230): the
// standard error envelope plus the rider's existing OPEN instant ride under
// `activeRideRequest`, so the client adopts it into the pending/tracking UI.
// The message carries no P1 value; the adopted ride's coordinates go only to
// its own rider (a party, mirroring GET) and are never logged here.
func (h *RideRequestHandler) writeRideActive(w http.ResponseWriter, existing RideRequestData) {
	h.writeJSON(w, http.StatusConflict, rideActiveErrorResponse{
		Error: wserrors.ErrorEnvelopeBody{
			Code:    wserrors.ErrCodeRideActive,
			Message: "you already have an active ride request",
		},
		ActiveRideRequest: h.rideWire(existing),
	})
}
