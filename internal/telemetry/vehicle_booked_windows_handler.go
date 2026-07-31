// GET /api/vehicles/{vehicleId}/booked-windows — the rider schedule picker's
// read side of the MYR-383 booking gate (MYR-385, rest-api.md §7.22).
//
// WHY IT HANGS OFF RideRequestHandler despite serving a /api/vehicles/ path.
// Its authorization rule is not merely SIMILAR to the ride-create rule, it must
// BE the ride-create rule: this endpoint answers "what would create refuse?",
// so any caller create would turn away must be turned away here identically, or
// the endpoint becomes an oracle answering a question POST /api/ride-requests
// would not. Living on the handler that owns create means it reuses the same
// vehicles reader, the same share reader, the same vehicleAccessFor(capRides)
// call and the same denyVehicleAccess response — rather than a second copy of
// the gate that could be one edit away from being a tier more generous. The
// path is vehicle-scoped because the QUESTION is about a vehicle; the
// PERMISSION is about rides. §7.5's invite endpoints set the precedent for a
// handler spanning path families.

package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// bookedWindowsDefaultRange is the horizon used when `to` is omitted: a week
// forward from `from`. It matches what a schedule picker opens on, and it is a
// DEFAULT, never a cap — the cap is injected (WithBookedWindowsMaxRange).
const bookedWindowsDefaultRange = 7 * 24 * time.Hour

// WithBookedWindowsMaxRange injects the cap on the span one §7.22 call may ask
// about. wiring.go passes store.MaxBookedWindowRange, and that is the ONLY
// value this server ever runs with — the cap belongs to the store (it is the
// bound that makes the read's missing LIMIT safe), and this package cannot
// import internal/store to read it.
//
// It used to be a handler-local literal "mirroring" the store's, checked by a
// test that compared two package-local constants and therefore proved nothing;
// store.MaxBookedWindowRange itself was referenced by no code at all. One
// injected value cannot drift from itself.
//
// The over-wide range is REFUSED (400) rather than silently clamped. A clamped
// answer looks complete and is not, which produces a picker that under-dims —
// the exact failure this endpoint exists to remove — whereas a 400 is a bug the
// client author fixes once.
func WithBookedWindowsMaxRange(maxRange time.Duration) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.bookedWindowsMax = maxRange
	}
}

// bookedWindowWire is one item of the §7.22 response
// (contracts schemas/booked-windows.schema.json $defs.BookedWindow).
//
// Instants are RFC 3339 UTC strings, formatted once here. Note what is NOT in
// this struct and must never be added: the occupying ride's id, rider,
// requester name, pickup, dropoff or status. Those are P1 and belong to a party
// the caller is not (data-classification.md §1.9) — a picker needs to know WHEN
// the car is spoken for, never by whom.
type bookedWindowWire struct {
	Start   string `json:"start"`
	End     string `json:"end"`
	Pending bool   `json:"pending"`
	Own     bool   `json:"own"`
}

// bookedWindowsResponse is the §7.22 envelope (VehicleBookedWindowsResponse).
// Deliberately NOT the cursor-paginated list envelope: there is no nextCursor
// and no hasMore, because the caller's [from, to) range is the bound.
type bookedWindowsResponse struct {
	Items []bookedWindowWire `json:"items"`
}

// ServeBookedWindows handles GET /api/vehicles/{vehicleId}/booked-windows.
//
// The order of the stages is load-bearing and mirrors ServeCreate exactly:
// authenticate, validate the range, resolve the vehicle, then run the
// capability gate. Validation before the vehicle read so a malformed range is a
// 400 whoever asks; the vehicle read before the gate because the gate needs the
// owner id it produces.
//
// Note what this endpoint deliberately does NOT check: the MYR-342 ride-share
// pause and the MYR-316 service window. Both refuse a CREATE, neither describes
// a window on the calendar, and folding them in would make a paused car answer
// "no windows" — read by a picker as "wide open", the most misleading answer
// available. A paused or in-service car's refusal is create's to deliver.
func (h *RideRequestHandler) ServeBookedWindows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}

	from, to, ok := h.parseBookedWindowRange(w, r)
	if !ok {
		return
	}

	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return
		}
		h.logger.Error("booked windows: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	if !h.authorizeBookedWindows(ctx, w, vehicleID, userID, row.UserID) {
		return
	}

	windows, err := h.store.ListBookedWindows(ctx, vehicleID, userID, from, to)
	if err != nil {
		h.logger.Error("booked windows: list failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	items := make([]bookedWindowWire, 0, len(windows))
	for _, win := range windows {
		items = append(items, bookedWindowWire{
			Start:   win.Start.UTC().Format(time.RFC3339),
			End:     win.End.UTC().Format(time.RFC3339),
			Pending: win.Pending,
			Own:     win.Own,
		})
	}
	h.writeJSON(w, http.StatusOK, bookedWindowsResponse{Items: items})
}

// authorizeBookedWindows runs the ride-CREATE capability gate — the vehicle's
// owner, or a viewer whose live grant carries the ride capability (capRides).
// Byte-for-byte ServeCreate's check, including the refusal it writes, so this
// endpoint never admits somebody create would turn away and never turns away
// somebody create would admit.
//
// A viewer holding only the base capability is refused: they can SEE the car,
// but they cannot book it, and a picker they cannot submit from has nothing to
// dim. Refusing them also keeps the surface from becoming a way for anybody
// with any share to watch a car's calendar fill up.
func (h *RideRequestHandler) authorizeBookedWindows(
	ctx context.Context, w http.ResponseWriter, vehicleID, userID, ownerUserID string,
) bool {
	if _, err := vehicleAccessFor(ctx, h.shares, userID, vehicleID, ownerUserID, capRides); err != nil {
		if errors.Is(err, errNoVehicleAccess) {
			denyVehicleAccess(w, h.logger, "booked windows", vehicleID, userID)
			return false
		}
		h.logger.Error("booked windows: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}
	return true
}

// parseBookedWindowRange decodes and validates `from` / `to` per §7.22.
//
// Both are optional: `from` defaults to now and `to` to a week past `from`, so
// the bare URL answers the question a picker opens with. Both must be RFC 3339
// when present — no other layout is accepted, and in particular a bare date is
// a 400 rather than being read as midnight in some timezone nobody named.
//
// `from` may be in the PAST, deliberately. An active instant ride's window
// straddles now, and a picker that opened a few minutes ago and is scrolled to
// "today" would otherwise silently lose it. Clamping to now would also make the
// response depend on the server clock in a way the caller could not predict.
func (h *RideRequestHandler) parseBookedWindowRange(
	w http.ResponseWriter, r *http.Request,
) (from, to time.Time, ok bool) {
	// An unconfigured cap is a WIRING bug, not a request the caller can fix:
	// answering without one would let a single call ask for a decade. Fail the
	// same way a missing Live Activity registry does — loudly, at the first
	// request, with nothing served.
	if h.bookedWindowsMax <= 0 {
		h.logger.Error("booked windows: max range not configured — wiring.go must pass WithBookedWindowsMaxRange")
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return time.Time{}, time.Time{}, false
	}

	q := r.URL.Query()

	from = time.Now().UTC()
	if raw := q.Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "from must be an RFC 3339 instant")
			return time.Time{}, time.Time{}, false
		}
		from = parsed
	}

	to = from.Add(bookedWindowsDefaultRange)
	if raw := q.Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "to must be an RFC 3339 instant")
			return time.Time{}, time.Time{}, false
		}
		to = parsed
	}

	// STRICT: from == to is refused along with from > to. An empty range can
	// only ever answer `items: []`, which a picker would read as "wide open" —
	// a wrong answer dressed as a right one, and far worse than a 400.
	if !from.Before(to) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "from must be earlier than to")
		return time.Time{}, time.Time{}, false
	}
	if to.Sub(from) > h.bookedWindowsMax {
		// The number comes from the configured cap, never from a literal — a
		// message that names a different 14 than the check enforces is worse
		// than no number. The cap is a whole number of days by construction
		// (store.MaxBookedWindowRange); a fractional one would round down here.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"range must not exceed "+strconv.Itoa(int(h.bookedWindowsMax.Hours())/24)+" days")
		return time.Time{}, time.Time{}, false
	}

	return from, to, true
}
