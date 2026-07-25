package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// countBoardedEvents returns how many RideBoardedEvent (the leg-2 dropoff
// dispatch seam) a publisher captured — the exactly-once assertion hinges on
// this being 1 for a successful board and 0 otherwise.
func countBoardedEvents(evs []events.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.Payload.(events.RideBoardedEvent); ok {
			n++
		}
	}
	return n
}

// TestRideRequestHandler_Board covers the accepted→enroute transition matrix,
// party-auth, idempotency, and the exactly-once dropoff dispatch seam.
func TestRideRequestHandler_Board(t *testing.T) {
	tests := []struct {
		name          string
		caller        string
		rec           RideRequestData
		getErr        error
		wantStatus    int
		wantErrCode   wserrors.ErrorCode
		wantStatusVal string // expected body status on 200
		wantUpdate    bool   // a guarded write attempt occurred
		wantBoarded   int    // RideBoardedEvent count (dropoff push)
		wantStatusEvt bool   // a RideStatusChangedEvent was published
	}{
		{
			name: "rider boards accepted", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusAccepted),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusEnroute,
			wantUpdate: true, wantBoarded: 1, wantStatusEvt: true,
		},
		{
			name: "idempotent re-board when already enroute", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusEnroute),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusEnroute,
			wantUpdate: false, wantBoarded: 0, wantStatusEvt: false,
		},
		{
			name: "reject from requested", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusRequested),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from declined", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusDeclined),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from completed", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusCompleted),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from cancelled", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusCancelled),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "owner cannot board (rider-only)", caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied,
		},
		{
			name: "non-party gets 404", caller: "clstranger00000000000",
			rec:        fixtureRideData(rideOtherUsr, rideStatusAccepted),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "unknown id 404", caller: rideUserID, getErr: fmtNotFound(),
			wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, tt.caller)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/board", "", rideAuthOK)

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
			if tt.wantUpdate && store.updatedState != rideStatusEnroute {
				t.Errorf("expected UpdateStatusFrom(enroute), got %q", store.updatedState)
			}
			if n := countBoardedEvents(pub.events); n != tt.wantBoarded {
				t.Errorf("RideBoardedEvent (dropoff push) count=%d, want %d", n, tt.wantBoarded)
			}
			gotStatusEvt := false
			for _, e := range pub.events {
				if _, ok := e.Payload.(events.RideStatusChangedEvent); ok {
					gotStatusEvt = true
				}
			}
			if gotStatusEvt != tt.wantStatusEvt {
				t.Errorf("RideStatusChangedEvent published=%v want=%v", gotStatusEvt, tt.wantStatusEvt)
			}
		})
	}
}

// TestRideRequestHandler_Board_DropoffSeamPayload proves the leg-2 dispatch seam
// carries the ride's DROPOFF coordinate (not the pickup) so the nav dispatcher
// pushes the correct destination.
func TestRideRequestHandler_Board_DropoffSeamPayload(t *testing.T) {
	rideRec := fixtureRideData(rideUserID, rideStatusAccepted)
	store := &fakeRideStore{getRec: rideRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/board", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
	}

	var boarded *events.RideBoardedEvent
	for _, e := range pub.events {
		if b, ok := e.Payload.(events.RideBoardedEvent); ok {
			bb := b
			boarded = &bb
		}
	}
	if boarded == nil {
		t.Fatal("no RideBoardedEvent published on a successful board")
	}
	if boarded.RideRequestID != rideID || boarded.VehicleID != rideVehicle || boarded.OwnerID != rideRec.OwnerID {
		t.Errorf("boarded ids: %+v", boarded)
	}
	if boarded.Dropoff.Latitude != rideRec.Dropoff.Latitude || boarded.Dropoff.Longitude != rideRec.Dropoff.Longitude {
		t.Errorf("boarded dropoff = %+v, want the ride's dropoff %+v", boarded.Dropoff, rideRec.Dropoff)
	}
}

// TestRideRequestHandler_Board_GuardWinsRace: the pre-check read sees accepted
// (legal) but by write time the row moved outside the allowed-from set (e.g. a
// concurrent cancel). The guarded write refuses — 409, and NO dropoff dispatch
// seam is published.
func TestRideRequestHandler_Board_GuardWinsRace(t *testing.T) {
	readRec := fixtureRideData(rideUserID, rideStatusAccepted)   // pre-check passes
	writeRec := fixtureRideData(rideUserID, rideStatusCancelled) // guard sees the concurrent cancel
	store := &fakeRideStore{getRec: readRec, updated: writeRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/board", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
	if store.updateCalls != 1 {
		t.Errorf("expected exactly one guarded write attempt, got %d", store.updateCalls)
	}
	if len(store.updatedFrom) != 1 || store.updatedFrom[0] != rideStatusAccepted {
		t.Errorf("guard allowed-from set: %v, want [accepted]", store.updatedFrom)
	}
	if len(pub.events) != 0 {
		t.Errorf("losing board must publish no events (incl. no dropoff seam), got %d", len(pub.events))
	}
}
