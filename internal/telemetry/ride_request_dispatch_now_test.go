package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-556 — START EARLY. The owner's manual override of the MYR-535 leave-time.
//
// What these pin, in one sentence each: nobody but the owner can move the car,
// nothing but an undispatched accepted reservation can be moved, the ONE thing
// the dispatch path decides (is the car busy right now) is spoken rather than
// blamed on the ride, and a repeated tap needs no rate limit because the claim
// already is one.

// fakeReservationDispatcher records the ride it was handed and answers a fixed
// outcome. It is what makes "the handler decides the refusals, the dispatch path
// decides the dispatch" a testable claim: a case that expects a 403/409 asserts
// this was never called at all.
type fakeReservationDispatcher struct {
	mu      sync.Mutex
	calls   []DispatchNowRide
	outcome DispatchNowOutcome
	err     error
}

func (f *fakeReservationDispatcher) DispatchNow(_ context.Context, ride DispatchNowRide) (DispatchNowOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ride)
	return f.outcome, f.err
}

func (f *fakeReservationDispatcher) called() []DispatchNowRide {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DispatchNowRide(nil), f.calls...)
}

// dispatchNowHandler is newRideHandler with the MYR-556 seam wired.
func dispatchNowHandler(
	store RideRequestStore, pub RideEventPublisher, caller string, d ReservationDispatcher,
) *RideRequestHandler {
	opts := []RideRequestOption{}
	if d != nil {
		opts = append(opts, WithReservationDispatcher(d))
	}
	return NewRideRequestHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: availableSnapshotRow()},
		store,
		pub,
		discardLogger(),
		opts...,
	)
}

func dispatchNowMux(h *RideRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ride-requests/{id}/dispatch-now", h.ServeDispatchNow)
	return mux
}

func postDispatchNow(t *testing.T, h *RideRequestHandler) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, dispatchNowMux(h), http.MethodPost,
		"/api/ride-requests/"+rideID+"/dispatch-now", "", rideAuthOK)
}

// errMessage reads the envelope's human-readable sentence. It must be called
// BEFORE assertErrEnvelope, which drains the recorder's buffer.
func errMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return body.Error.Message
}

// undispatchedReservation is the ONE shape this endpoint serves: accepted,
// scheduled, and the leg-1 latch still unclaimed.
// The owner is always rideOtherUsr here: this endpoint's whole subject is the
// owner/rider split, so a fixture where the two ids coincide would make every
// 403 case vacuous.
func undispatchedReservation() RideRequestData {
	rec := reservationRide(rideOtherUsr, rideStatusAccepted, nil)
	rec.DispatchedAt = nil
	return rec
}

// TestDispatchNow_AuthorizationLadder pins who may move the car, and — just as
// importantly — WHICH refusal each non-owner gets. A stranger must not learn the
// ride exists; a party must not be told it does not.
func TestDispatchNow_AuthorizationLadder(t *testing.T) {
	tests := []struct {
		name        string
		caller      string
		rec         RideRequestData
		getErr      error
		wantStatus  int
		wantErrCode wserrors.ErrorCode
		wantCalled  bool
	}{
		{
			name: "the owner may send the car early", caller: rideOtherUsr,
			rec:        undispatchedReservation(),
			wantStatus: http.StatusOK, wantCalled: true,
		},
		{
			// The rider IS a party and can see this ride in their own app, so a
			// 404 would read as a bug. They simply do not get to move somebody
			// else's car.
			name: "the rider is a party and still gets 403", caller: rideUserID,
			rec:        undispatchedReservation(),
			wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied,
		},
		{
			// No existence oracle: a caller with no relation to the ride is told
			// nothing about it.
			name: "a stranger gets 404, never 403", caller: "clstranger00000000000",
			rec:        undispatchedReservation(),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "unknown ride is 404", caller: rideOtherUsr, getErr: fmtNotFound(),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeReservationDispatcher{outcome: DispatchNowDispatched}
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			h := dispatchNowHandler(store, &fakeRidePublisher{}, tt.caller, d)

			rec := postDispatchNow(t, h)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
			}
			if called := len(d.called()) > 0; called != tt.wantCalled {
				t.Errorf("dispatch path called = %v, want %v — a refusal must never move a car", called, tt.wantCalled)
			}
		})
	}
}

// TestDispatchNow_ConflictLadder is the 409 matrix. One code, three sentences:
// a client branches on the code, a person reads the sentence, and only the
// sentence can tell "not accepted yet" from "not a reservation at all".
func TestDispatchNow_ConflictLadder(t *testing.T) {
	dispatched := undispatchedReservation()
	at := time.Date(2026, 8, 13, 21, 40, 24, 0, time.UTC)
	dispatched.DispatchedAt = &at
	dispatched.DispatchStatus = dispatchPtr("sent")

	tests := []struct {
		name string
		rec  RideRequestData
	}{
		{name: "still requested — the owner has not accepted it", rec: reservationRide(rideOtherUsr, rideStatusRequested, nil)},
		{name: "already picked up", rec: reservationRide(rideOtherUsr, rideStatusArrived, nil)},
		{name: "under way", rec: reservationRide(rideOtherUsr, rideStatusEnroute, nil)},
		{name: "cancelled", rec: reservationRide(rideOtherUsr, rideStatusCancelled, nil)},
		{name: "completed", rec: reservationRide(rideOtherUsr, rideStatusCompleted, nil)},
		{name: "an on-demand ride was dispatched at accept", rec: instantRide(rideOtherUsr, rideStatusAccepted, nil)},
		{name: "the car has already been sent", rec: dispatched},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeReservationDispatcher{outcome: DispatchNowDispatched}
			store := &fakeRideStore{getRec: tt.rec}
			h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

			rec := postDispatchNow(t, h)

			// Read the body BEFORE assertErrEnvelope, which drains the recorder.
			msg := errMessage(t, rec)
			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
			if len(d.called()) != 0 {
				t.Error("a refused request reached the dispatch path — the handler's own gates must decide first")
			}
			// Each refusal has to SAY something. A code alone leaves an owner
			// staring at a button that does nothing.
			if msg == "" {
				t.Error("409 carried no message")
			}
		})
	}
}

// TestDispatchNow_VehicleBusyIsSpoken: the sweeper's busy HOLD is a bounded
// retry nobody sees. With a human waiting it has to become a sentence — and one
// that says what happens next, because the reservation is perfectly valid and
// will dispatch on its own the moment the car is free.
func TestDispatchNow_VehicleBusyIsSpoken(t *testing.T) {
	d := &fakeReservationDispatcher{outcome: DispatchNowVehicleBusy}
	store := &fakeRideStore{getRec: undispatchedReservation()}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	rec := postDispatchNow(t, h)

	msg := errMessage(t, rec)
	// The VEHICLE is unavailable — a capability gate on the car, the same class
	// as the accept gate's one-active-ride refusal, NOT an illegal transition.
	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)

	if msg == "" {
		t.Fatal("busy refusal carried no message")
	}
	// It must not blame the ride. The ride is fine.
	for _, wrong := range []string{"accepted", "status"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("busy message blames the ride's status: %q", msg)
		}
	}
}

// TestDispatchNow_RepeatedTapIsAlreadyDispatched is the abuse story, and it is
// the claim's story. The second tap finds the latch held — either through the
// handler's pre-check or, under a race, through the dispatch path itself — and
// both land on the same 409. Nothing a repeated call can spend, so no budget.
func TestDispatchNow_RepeatedTapIsAlreadyDispatched(t *testing.T) {
	d := &fakeReservationDispatcher{outcome: DispatchNowAlreadyDispatched}
	store := &fakeRideStore{getRec: undispatchedReservation()}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	// The row still reads undispatched — the sweeper won it in the interval —
	// so the pre-check passes and the CLAIM is what refuses. That is the
	// authority, and this case is what proves the pre-check is not it.
	rec := postDispatchNow(t, h)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
	if len(d.called()) != 1 {
		t.Errorf("dispatch attempts = %d, want 1", len(d.called()))
	}
}

// TestDispatchNow_HandsTheDispatchPathTheWholeRide pins the payload. The pickup
// is what the car is sent to and the trip version is what makes the MYR-555
// frame a usable refetch signal — a zero in either is a silent product failure.
func TestDispatchNow_HandsTheDispatchPathTheWholeRide(t *testing.T) {
	rideRec := undispatchedReservation()
	rideRec.TripVersion = 5
	d := &fakeReservationDispatcher{outcome: DispatchNowDispatched}
	store := &fakeRideStore{getRec: rideRec}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	if rec := postDispatchNow(t, h); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}

	calls := d.called()
	if len(calls) != 1 {
		t.Fatalf("dispatch attempts = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.RideRequestID != rideID || got.VehicleID != rideVehicle || got.OwnerID != rideOtherUsr || got.RiderID != rideUserID {
		t.Errorf("ids = %+v", got)
	}
	if got.Pickup.Latitude != rideRec.Pickup.Latitude || got.Pickup.Longitude != rideRec.Pickup.Longitude {
		t.Errorf("pickup = (%v, %v), want the ride's (%v, %v) — this is where the car is sent",
			got.Pickup.Latitude, got.Pickup.Longitude, rideRec.Pickup.Latitude, rideRec.Pickup.Longitude)
	}
	if got.TripVersion != 5 {
		t.Errorf("TripVersion = %d, want 5 — a zero makes the MYR-555 frame read as a regression", got.TripVersion)
	}
	if got.ScheduledFor.IsZero() {
		t.Error("ScheduledFor zero, want the reservation instant this dispatch is jumping ahead of")
	}
}

// TestDispatchNow_AnswersTheRefreshedRide: the whole observable result of this
// endpoint is the dispatch columns, so the response must be a RE-READ and not
// the row the gates were evaluated against.
func TestDispatchNow_AnswersTheRefreshedRide(t *testing.T) {
	before := undispatchedReservation()
	after := before
	at := time.Date(2026, 8, 13, 21, 40, 24, 0, time.UTC)
	after.DispatchedAt = &at
	after.DispatchStatus = dispatchPtr("sent")

	d := &fakeReservationDispatcher{outcome: DispatchNowDispatched}
	store := &fakeRideStore{getRec: before, getRecAfter: &after}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	rec := postDispatchNow(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// A bare RideRequest — no new response type (contracts v0.36.0 adds none).
	if got["id"] != rideID {
		t.Errorf("id = %v, want the ride itself", got["id"])
	}
	if got["dispatchedAt"] == nil {
		t.Error("dispatchedAt absent — the response must be the refreshed record, not the pre-dispatch one")
	}
	if got["dispatchStatus"] != "sent" {
		t.Errorf("dispatchStatus = %v, want sent", got["dispatchStatus"])
	}
	// The STATUS did not move. This endpoint dispatches; it does not transition.
	if got["status"] != rideStatusAccepted {
		t.Errorf("status = %v, want accepted — dispatch-now changes no lifecycle state", got["status"])
	}
}

// TestDispatchNow_FailedRereadStillAnswersTheDispatch. The dispatch COMMITTED —
// the latch is stamped and the car has its route. A 500 here would tell the
// owner it failed, after which their retry gets the already-dispatched 409 and
// they cannot tell which answer was true. The stale record is merely incomplete,
// and MYR-555's frame has already asked this client to refetch.
func TestDispatchNow_FailedRereadStillAnswersTheDispatch(t *testing.T) {
	d := &fakeReservationDispatcher{outcome: DispatchNowDispatched}
	store := &fakeRideStore{
		getRec:      undispatchedReservation(),
		getErrAfter: errors.New("database is having a moment"),
	}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	rec := postDispatchNow(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the dispatch committed. body=%s", rec.Code, rec.Body.String())
	}
}

// TestDispatchNow_UnknownOutcomeIsAnHonest500: "we could not tell" must never
// become "your car is on its way". An unreadable busy probe or a claim that
// would not take is a 500 the owner can retry.
func TestDispatchNow_UnknownOutcomeIsAnHonest500(t *testing.T) {
	d := &fakeReservationDispatcher{err: errors.New("busy probe unavailable")}
	store := &fakeRideStore{getRec: undispatchedReservation()}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, d)

	rec := postDispatchNow(t, h)
	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
}

// TestDispatchNow_UnwiredDispatcherIs500 pins the fail-CLOSED default. There is
// no safe way to pretend a car was sent, so an unwired seam is a deployment
// error the endpoint reports rather than a runtime state it papers over.
func TestDispatchNow_UnwiredDispatcherIs500(t *testing.T) {
	store := &fakeRideStore{getRec: undispatchedReservation()}
	h := dispatchNowHandler(store, &fakeRidePublisher{}, rideOtherUsr, nil)

	rec := postDispatchNow(t, h)
	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
}
