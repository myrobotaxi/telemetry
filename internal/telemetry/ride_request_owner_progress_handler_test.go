package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

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
	}
	for _, tt := range tests {
		runOwnerProgress(t, "/picked-up", tt)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, tt.caller)

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
	})
}
