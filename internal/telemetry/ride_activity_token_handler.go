package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// ErrLiveActivityRideClosed is returned by LiveActivityRegistry.RegisterActivity
// when the ride is past registration — a terminal status, or a reservation the
// sweeper gave up on. The cmd adapter translates store.ErrLiveActivityRideClosed
// into this sentinel so the handler layer stays decoupled from internal/store,
// exactly as ErrRideStatusConflict does.
var ErrLiveActivityRideClosed = errors.New("live activity: ride is closed to registration")

// Live Activity token registration (MYR-172, rest-api.md §7.21).
//
// The iOS app starts a Live Activity locally when a ride is accepted and is
// handed a per-Activity push token by ActivityKit. These two endpoints are how
// that token reaches the server, and how the app says the Activity is over.
//
// RIDER-ONLY, deliberately. The owner is a party to the ride and can read it,
// but v1 starts no owner Activity, and an endpoint that accepted an owner's
// token would quietly create rows the sender would then push RIDER content to —
// including the destination label, which the owner has no business seeing on
// their lock screen. The owner variant is an explicit MYR-172 follow-up and
// will arrive with its own content-state.
//
// The token ROTATES. ActivityKit hands the app a replacement mid-ride and
// expects the server to switch to it, so registration is an UPSERT on
// (ride, party) rather than an insert, and a rotation is an ordinary
// re-registration. A client that re-registers after ending clears the
// tombstone, because it is telling us it has a live Activity again.

// maxActivityTokenLen bounds an accepted token. An ActivityKit push token is
// 64 hex characters today, the same shape as a device token, but the bound is
// deliberately generous for the same reason as maxDeviceTokenLen in
// internal/push: rejecting a valid future token would silently break Live
// Activities for everyone on that iOS release, which is far worse than storing
// a slightly odd string.
const maxActivityTokenLen = 256

// isHexToken reports whether every character of the token is a hex digit.
//
// The LENGTH bound above is generous on purpose — Apple could plausibly mint a
// longer token. The CHARSET is a different kind of bet: an APNs push token is
// the hex rendering of opaque binary and has been since the API existed, both
// halves of the platform (device tokens and ActivityKit tokens) agree on it,
// and a token containing anything else could not address a device even if we
// stored it. So this rejects a shape that is already broken rather than one
// that is merely unfamiliar.
//
// It is also a SECURITY control, and that is the reason it is here rather than
// on a wish-list. The token is interpolated into the APNs request path
// (push.Client.newRequest), and the P1 rule in data-classification.md §1.18 is
// that it is never logged in full. That file's belt-and-braces escaping and
// error-unwrapping make the rule hold for any input; refusing non-hex at the
// door means the pathological input never enters the system to begin with.
// Upper case is accepted: it is the same token, and rejecting a client that
// upper-cased its hex would be a bug report nobody could diagnose.
func isHexToken(token string) bool {
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// LiveActivityRegistry is the persistence seam for these two endpoints.
// Satisfied by *store.LiveActivityRepo directly; no adapter needed.
type LiveActivityRegistry interface {
	RegisterActivity(ctx context.Context, rideRequestID, userID, token string, sandbox bool) error
	EndActivity(ctx context.Context, rideRequestID, userID string) (bool, error)
}

// WithLiveActivityRegistry wires the Live Activity token endpoints. Nil leaves
// them answering 500 — the fail-closed default, and a deployment error rather
// than a runtime state.
func WithLiveActivityRegistry(registry LiveActivityRegistry) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.activities = registry
	}
}

// activityTokenRequest is the POST body: `{"activityToken":"…","sandbox":true}`.
type activityTokenRequest struct {
	ActivityToken string `json:"activityToken"`
	Sandbox       bool   `json:"sandbox"`
}

// activityTokenResponse confirms a registration.
//
// It deliberately does NOT echo the token: the token is P1
// (data-classification.md §1.18) and echoing it would put it in every client
// log and proxy trace for no benefit — the caller already knows what it sent.
// Same rule, same reason, as the §7.17 device-registration response.
type activityTokenResponse struct {
	Registered bool `json:"registered"`
	Sandbox    bool `json:"sandbox"`
}

// activityEndedResponse reports whether a live registration was actually
// closed. `false` covers both "already ended" and "never registered",
// deliberately indistinguishable — though on this endpoint the caller has
// already been proven to be the ride's rider, so there is nothing to probe.
type activityEndedResponse struct {
	Ended bool `json:"ended"`
}

// ServeRegisterActivityToken handles
// POST /api/ride-requests/{id}/activity-token.
func (h *RideRequestHandler) ServeRegisterActivityToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, ok := h.authRiderForActivity(w, r)
	if !ok {
		return
	}

	body, ok := h.decodeActivityTokenBody(w, r)
	if !ok {
		return
	}

	token := strings.TrimSpace(body.ActivityToken)
	if token == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is required")
		return
	}
	if len(token) > maxActivityTokenLen {
		// The token is P1: report the violation, never the value.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is too long")
		return
	}
	if !isHexToken(token) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"activityToken must be hexadecimal")
		return
	}

	if !h.guardActivityRegistrable(w, rec) {
		return
	}

	if err := h.activities.RegisterActivity(ctx, rec.ID, rec.RiderID, token, body.Sandbox); err != nil {
		if errors.Is(err, ErrLiveActivityRideClosed) {
			// The write's own guard refused. The checks above already covered
			// every state we could SEE, so reaching here means the ride closed
			// between that read and this write — the race the SQL guard exists
			// for. Same instruction to the client, no sub-code: we no longer
			// know which ending won, and guessing would be worse than silence.
			h.logger.Info("ride-request: activity registration lost a race with the ride ending",
				slog.String("ride_request_id", rec.ID),
			)
			h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
				"ride request is no longer accepting Live Activity registrations")
			return
		}
		h.logger.Error("ride-request: activity token registration failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, activityTokenResponse{Registered: true, Sandbox: body.Sandbox})
}

// guardActivityRegistrable refuses a registration for a ride whose Live
// Activity has already been ended for good, writing the error and reporting
// false. It is the readable half of the guard; the enforcing half is the
// predicate inside the upsert (store.queryUpsertLiveActivity), which is what
// actually holds under a race.
//
// TWO ENDINGS ARE FINAL, and they look nothing alike from here.
//
// A TERMINAL STATUS is the obvious one and is visible in rec.Status.
//
// A RESERVATION EXPIRY is invisible in the status. The sweeper gives up on a
// late scheduled ride by recording dispatch_status='failed' with
// dispatch_error='reservation_expired' and LEAVING THE ROW AT `accepted` — and
// it latches dispatched_at, so no second expiry is possible. It ends and
// tombstones the Activities on the way past. A registration that only looked at
// the status would happily re-register (the upsert clears ended_at by design,
// which is right for a token rotation and wrong for this), the ETA ticker would
// pick the row back up, and the rider would watch a lock screen count down to a
// pickup that is never coming, with nothing left in the system able to end it a
// second time.
//
// That second case gets a sub-code precisely because the status does not
// explain it: a client seeing `conflict` on an `accepted` ride would have no
// way to tell a bug from a reservation that lapsed.
func (h *RideRequestHandler) guardActivityRegistrable(w http.ResponseWriter, rec RideRequestData) bool {
	// A ride that has already ended will never be pushed to again — the
	// terminal `event: "end"` has fired and the rows are tombstoned — so
	// accepting a registration here would store a row nothing will ever update
	// and only the 24h sweep would remove. The 409 tells the client to end its
	// Activity locally now, which is exactly what its final-state fallback is
	// for.
	if isTerminalRideStatus(rec.Status) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"ride request is already "+rec.Status)
		return false
	}

	if isReservationExpired(rec) {
		h.writeErrorSub(w, http.StatusConflict, wserrors.ErrCodeConflict,
			wserrors.SubCodeReservationExpired,
			"the reservation for this ride expired and its Live Activity has ended")
		return false
	}

	return true
}

// isReservationExpired reports whether the reservation sweeper gave up on this
// ride. Both columns are checked: dispatch_status alone is `failed` for any
// dispatch failure, and an ordinary failed nav push leaves a ride that is still
// genuinely happening — the owner can drive it manually — whose Activity must
// keep working, token rotations included.
func isReservationExpired(rec RideRequestData) bool {
	if rec.DispatchStatus == nil || *rec.DispatchStatus != dispatchStatusFailed {
		return false
	}
	return rec.DispatchError != nil && *rec.DispatchError == dispatchErrorReservationExpired
}

// The two dispatch-column values this file reads. Kept as constants here
// rather than imported from internal/dispatch, which internal/telemetry does
// not depend on; the migration-0025 registration test pins the pair against the
// value the sweeper actually writes.
const (
	dispatchStatusFailed            = "failed"
	dispatchErrorReservationExpired = "reservation_expired"
)

// ServeEndActivityToken handles DELETE /api/ride-requests/{id}/activity-token.
//
// Called when the Activity ends on the phone — the rider dismissed it, or the
// app ended it from its own final-state fallback. Idempotent: ending an
// already-ended Activity is a 200 reporting `false`, not an error, because the
// client's end and a terminal-state send race by design and both are correct.
func (h *RideRequestHandler) ServeEndActivityToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, ok := h.authRiderForActivity(w, r)
	if !ok {
		return
	}

	ended, err := h.activities.EndActivity(ctx, rec.ID, rec.RiderID)
	if err != nil {
		h.logger.Error("ride-request: activity token end failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, activityEndedResponse{Ended: ended})
}

// authRiderForActivity runs the shared front half of both endpoints:
// authenticate, resolve the ride, and require the caller to be its RIDER.
//
// The 404-vs-403 split is the house rule, unchanged (rest-api.md §5.2):
// loadForParty answers 404 for a stranger — so the endpoint never confirms that
// a ride id exists to somebody with no relation to it — and only a genuine
// party who is the OWNER rather than the rider reaches the 403 below.
func (h *RideRequestHandler) authRiderForActivity(w http.ResponseWriter, r *http.Request) (RideRequestData, bool) {
	ctx := r.Context()

	userID, ok := h.authUser(w, r)
	if !ok {
		return RideRequestData{}, false
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}
	if userID != rec.RiderID {
		// The owner is a party but Live Activities are rider-only in v1;
		// non-parties were already 404'd by loadForParty.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"only the rider may register a Live Activity for this ride")
		return RideRequestData{}, false
	}

	if h.activities == nil {
		h.logger.Error("ride-request: live activity registry not wired")
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return RideRequestData{}, false
	}

	return rec, true
}

// decodeActivityTokenBody strictly decodes the POST body (unknown keys are a
// 400, matching the schema's additionalProperties:false).
func (h *RideRequestHandler) decodeActivityTokenBody(w http.ResponseWriter, r *http.Request) (activityTokenRequest, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body activityTokenRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return activityTokenRequest{}, false
	}
	return body, true
}

// isTerminalRideStatus reports whether a ride has finished for good.
//
// Kept in lockstep with push.terminalStatuses, which decides the same question
// on the send side. The two cannot share a constant without internal/telemetry
// depending on internal/push, so they are pinned together by a test instead.
func isTerminalRideStatus(status string) bool {
	switch status {
	case rideStatusCompleted, rideStatusDeclined, rideStatusCancelled:
		return true
	default:
		return false
	}
}
