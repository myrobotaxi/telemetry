package telemetry

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// hasStatusChanged reports whether a RideStatusChangedEvent was published.
func hasStatusChanged(evs []events.Event) bool {
	for _, e := range evs {
		if _, ok := e.Payload.(events.RideStatusChangedEvent); ok {
			return true
		}
	}
	return false
}

// --- MYR-376 reservation-dormancy fixtures ---
//
// A reservation is DORMANT from accept until the EARLIER of its dispatch and
// its due instant: `scheduledFor` set, still in the future, and the MYR-179
// sweeper has not resolved its leg-1 push `sent`. These build the rows the
// pickup gate has to tell apart — and the due instants are relative to now,
// because the gate compares against the clock, not a calendar date.

// reservationRide is a PRE-DUE scheduled ride (due tomorrow) with the given
// dispatch outcome. A nil dispatch is the production defect's row: accepted
// today, due tomorrow, sweeper never ran.
func reservationRide(owner, status string, dispatch *string) RideRequestData {
	return reservationRideDue(owner, status, dispatch, time.Now().Add(24*time.Hour))
}

// pastDueReservationRide is the recovery shape: a reservation whose instant has
// already arrived. Whatever its dispatch outcome, §7.8 lets its parties proceed
// manually, so the pickup gate must let it through.
func pastDueReservationRide(owner, status string, dispatch *string) RideRequestData {
	return reservationRideDue(owner, status, dispatch, time.Now().Add(-2*time.Hour))
}

// reservationRideDue is a SCHEDULED ride with an EXPLICIT due instant.
func reservationRideDue(owner, status string, dispatch *string, due time.Time) RideRequestData {
	rec := fixtureRideData(owner, status)
	rec.ScheduledFor = &due
	rec.DispatchStatus = dispatch
	return rec
}

// instantRide is an INSTANT ride (no scheduledFor) with the given dispatch
// outcome — including a FAILED one, which must still be pickup-able: the car is
// at the kerb and the owner drives anyway.
func instantRide(owner, status string, dispatch *string) RideRequestData {
	rec := fixtureRideData(owner, status)
	rec.DispatchStatus = dispatch
	return rec
}

func dispatchPtr(s string) *string { return &s }

// ownerProgressCase drives both owner handshake endpoints through the same
// table shape. `owner` is the ride's owner id and `caller` the authenticated
// user, so a caller == owner is the happy path and caller == rider (rideUserID)
// exercises the owner-only 403.
type ownerProgressCase struct {
	name          string
	owner         string
	caller        string
	rec           RideRequestData
	getErr        error
	wantStatus    int
	wantErrCode   wserrors.ErrorCode
	wantStatusVal string
	wantUpdate    bool
	wantTo        string
	wantStatusEvt bool
}

// TestRideRequestHandler_PickedUp covers the OWNER-only accepted→arrived
// transition: happy path, idempotency, the 409 matrix, and party/owner auth.
// Picked-up pushes NO Tesla nav (asserted via zero RideStartedEvent).
func TestRideRequestHandler_PickedUp(t *testing.T) {
	tests := []ownerProgressCase{
		{
			name: "owner marks picked up from accepted", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			name: "idempotent when already arrived", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusArrived),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: false, wantStatusEvt: false,
		},
		{
			name: "reject from requested", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusRequested),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from enroute", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusEnroute),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from completed", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusCompleted),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "rider cannot mark picked up (owner-only)", owner: rideOtherUsr, caller: rideUserID,
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied,
		},
		{
			name: "non-party gets 404", owner: rideOtherUsr, caller: "clstranger00000000000",
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "unknown id 404", owner: rideOtherUsr, caller: rideOtherUsr, getErr: fmtNotFound(),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},

		// --- MYR-376 reservation dormancy: PRE-DUE (the defect) ---
		//
		// The refusal comes from the guarded WRITE, so every dormant case sets
		// wantUpdate: true — the handler attempts the transition and the
		// database (here, the fake modelling it) declines to match a row. The
		// code is the SAME `conflict` as an illegal from-status; no new code.
		{
			name:  "reject dormant reservation — accepted, due tomorrow, sweeper never dispatched (the production defect)",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:         reservationRide(rideOtherUsr, rideStatusAccepted, nil),
			wantStatus:  http.StatusConflict,
			wantErrCode: wserrors.ErrCodeConflict,
			wantUpdate:  true, wantTo: rideStatusArrived, wantStatusEvt: false,
		},
		{
			name:  "reject dormant reservation — pre-due with a failed dispatch",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:         reservationRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("failed")),
			wantStatus:  http.StatusConflict,
			wantErrCode: wserrors.ErrCodeConflict,
			wantUpdate:  true, wantTo: rideStatusArrived, wantStatusEvt: false,
		},
		{
			name:  "reject dormant reservation — pre-due with a skipped dispatch (kill-switch)",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:         reservationRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("skipped")),
			wantStatus:  http.StatusConflict,
			wantErrCode: wserrors.ErrCodeConflict,
			wantUpdate:  true, wantTo: rideStatusArrived, wantStatusEvt: false,
		},
		{
			name: "DISPATCHED reservation is picked up normally even before due", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        reservationRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("sent")),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},

		// --- MYR-376 reservation dormancy: PAST-DUE (the §7.8 manual-proceed
		// recovery). Dormancy ends at the due instant, dispatched or not —
		// otherwise an expired reservation would have cancel as its only exit,
		// contradicting the promise that its parties "may still cancel or
		// proceed manually". These are the mirror of the three above.
		{
			name:  "PAST-DUE reservation with NULL dispatch picks up (sweeper never ran)",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        pastDueReservationRide(rideOtherUsr, rideStatusAccepted, nil),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			name:  "PAST-DUE reservation with FAILED dispatch picks up (reservation_expired)",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        pastDueReservationRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("failed")),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			name:  "PAST-DUE reservation with SKIPPED dispatch picks up (kill-switch)",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        pastDueReservationRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("skipped")),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			name: "INSTANT ride with NULL dispatch is unaffected", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        instantRide(rideOtherUsr, rideStatusAccepted, nil),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			name: "INSTANT ride with FAILED dispatch is unaffected", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        instantRide(rideOtherUsr, rideStatusAccepted, dispatchPtr("failed")),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusArrived,
			wantUpdate: true, wantTo: rideStatusArrived, wantStatusEvt: true,
		},
		{
			// Dormancy never overrides the status matrix: a dormant reservation
			// that has not even reached `accepted` conflicts on the from-status,
			// and the fast path says so before the guarded write is reached.
			name:  "dormant reservation from requested still conflicts on status",
			owner: rideOtherUsr, caller: rideOtherUsr,
			rec:         reservationRide(rideOtherUsr, rideStatusRequested, nil),
			wantStatus:  http.StatusConflict,
			wantErrCode: wserrors.ErrCodeConflict,
		},
	}
	for _, tt := range tests {
		runOwnerProgress(t, "/picked-up", tt)
	}
}

// TestRideRequestHandler_PickedUp_DormantMessage pins that the two 409 causes
// on this endpoint share the `conflict` CODE but not the message: the dormancy
// refusal must name the reason, or an owner staring at "cannot be marked
// picked-up from status accepted" learns nothing about why a ride they legally
// accepted refuses to advance.
func TestRideRequestHandler_PickedUp_DormantMessage(t *testing.T) {
	store := &fakeRideStore{getRec: reservationRide(rideOtherUsr, rideStatusAccepted, nil)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideOtherUsr)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/picked-up", "", rideAuthOK)

	// Snapshot the body first — assertErrEnvelope consumes the buffer.
	raw := rec.Body.Bytes()
	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if !strings.Contains(body.Error.Message, "dispatch") {
		t.Errorf("409 message must name the dispatch reason, got %q", body.Error.Message)
	}
	// The refusal is the WRITE's, not a pre-check's.
	if store.dispatchGuardedCalls != 1 {
		t.Errorf("expected exactly one dormancy-guarded write attempt, got %d", store.dispatchGuardedCalls)
	}
}

// TestRideRequestHandler_DroppedOff covers the OWNER-only enroute→completed
// transition (owner-confirmed completion — MYR-270 removed drive-end
// auto-completion): happy path, idempotency, the 409 matrix, and party/owner
// auth.
func TestRideRequestHandler_DroppedOff(t *testing.T) {
	tests := []ownerProgressCase{
		{
			name: "owner marks dropped off from enroute", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusEnroute),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusCompleted,
			wantUpdate: true, wantTo: rideStatusCompleted, wantStatusEvt: true,
		},
		{
			name: "idempotent when already completed", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusCompleted),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusCompleted,
			wantUpdate: false, wantStatusEvt: false,
		},
		{
			name: "reject from accepted", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from arrived (rider has not started yet)", owner: rideOtherUsr, caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusArrived),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "rider cannot mark dropped off (owner-only)", owner: rideOtherUsr, caller: rideUserID,
			rec:        fixtureRideData(rideOtherUsr, rideStatusEnroute),
			wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied,
		},
		{
			name: "non-party gets 404", owner: rideOtherUsr, caller: "clstranger00000000000",
			rec:        fixtureRideData(rideOtherUsr, rideStatusEnroute),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		runOwnerProgress(t, "/dropped-off", tt)
	}
}

func runOwnerProgress(t *testing.T, path string, tt ownerProgressCase) {
	t.Helper()
	t.Run(tt.name, func(t *testing.T) {
		store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
		pub := &fakeRidePublisher{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, tt.caller)

		rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+path, "", rideAuthOK)

		if rec.Code != tt.wantStatus {
			t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
		}
		if tt.wantStatus == http.StatusOK {
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if got["status"] != tt.wantStatusVal {
				t.Errorf("body status: got %v want %q", got["status"], tt.wantStatusVal)
			}
		} else {
			assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
		}

		if (store.updateCalls > 0) != tt.wantUpdate {
			t.Errorf("guarded write attempts=%d, wantUpdate=%v", store.updateCalls, tt.wantUpdate)
		}
		if tt.wantUpdate && store.updatedState != tt.wantTo {
			t.Errorf("expected UpdateStatusFrom(%s), got %q", tt.wantTo, store.updatedState)
		}
		if hasStatusChanged(pub.events) != tt.wantStatusEvt {
			t.Errorf("RideStatusChangedEvent published=%v want=%v", hasStatusChanged(pub.events), tt.wantStatusEvt)
		}
		// The owner handshake NEVER pushes Tesla nav — no leg-2 dispatch seam.
		if n := countStartedEvents(pub.events); n != 0 {
			t.Errorf("owner handshake must not publish a RideStartedEvent (dropoff push), got %d", n)
		}
		// MYR-376 routing: pickup goes through the DORMANCY-guarded write and
		// dropped-off must NOT — the gate belongs to exactly one transition.
		switch path {
		case "/picked-up":
			if store.updateCalls > 0 && store.dispatchGuardedCalls == 0 {
				t.Error("pickup must use the dormancy-guarded write (UpdateStatusFromDispatched)")
			}
		default:
			if store.dispatchGuardedCalls != 0 {
				t.Errorf("%s must not use the dormancy-guarded write, got %d calls", path, store.dispatchGuardedCalls)
			}
		}
	})
}
