package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// countStartedEvents returns how many RideStartedEvent (the leg-2 dropoff
// dispatch seam) a publisher captured — the exactly-once assertion hinges on
// this being 1 for a successful start and 0 otherwise.
func countStartedEvents(evs []events.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.Payload.(events.RideStartedEvent); ok {
			n++
		}
	}
	return n
}

// TestRideRequestHandler_Start covers the RIDER-only arrived→enroute transition
// matrix, party-auth, idempotency, and the exactly-once dropoff dispatch seam.
// Crucially, a start from `accepted` (owner has not confirmed pickup) is a 409:
// the rider CANNOT start before the owner taps "Picked up".
func TestRideRequestHandler_Start(t *testing.T) {
	tests := []struct {
		name          string
		caller        string
		rec           RideRequestData
		getErr        error
		wantStatus    int
		wantErrCode   wserrors.ErrorCode
		wantStatusVal string // expected body status on 200
		wantUpdate    bool   // a guarded write attempt occurred
		wantStarted   int    // RideStartedEvent count (dropoff push)
		wantStatusEvt bool   // a RideStatusChangedEvent was published
	}{
		{
			name: "rider starts from arrived", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusArrived),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusEnroute,
			wantUpdate: true, wantStarted: 1, wantStatusEvt: true,
		},
		{
			name: "idempotent re-start when already enroute", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusEnroute),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusEnroute,
			wantUpdate: false, wantStarted: 0, wantStatusEvt: false,
		},
		{
			name: "reject from accepted (owner has not confirmed pickup)", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusAccepted),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			name: "reject from requested", caller: rideUserID,
			rec:        fixtureRideData(rideUserID, rideStatusRequested),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			// MYR-376: start gets NO new gate — it does not need one. A dormant
			// reservation can never reach `arrived`, because the pickup that
			// would put it there is now refused, so the existing from-status
			// guard (`arrived` only) already makes it unstartable. This case
			// exists so that stays true: if the pickup gate ever regressed and
			// let a dormant reservation through, the ride would arrive here
			// startable and this assertion is the second line of defence.
			name: "reject start of a DORMANT reservation (still accepted, sweeper never dispatched)", caller: rideUserID,
			rec:        reservationRide(rideUserID, rideStatusAccepted, nil),
			wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict,
		},
		{
			// The mirror: a DISPATCHED reservation is startable once the owner
			// has confirmed pickup, exactly like an instant ride.
			name: "dispatched reservation starts normally from arrived", caller: rideUserID,
			rec:        reservationRide(rideUserID, rideStatusArrived, dispatchPtr("sent")),
			wantStatus: http.StatusOK, wantStatusVal: rideStatusEnroute,
			wantUpdate: true, wantStarted: 1, wantStatusEvt: true,
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
			name: "owner cannot start (rider-only)", caller: rideOtherUsr,
			rec:        fixtureRideData(rideOtherUsr, rideStatusArrived),
			wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied,
		},
		{
			name: "non-party gets 404", caller: "clstranger00000000000",
			rec:        fixtureRideData(rideOtherUsr, rideStatusArrived),
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
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, tt.caller)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/start", "", rideAuthOK)

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
			if n := countStartedEvents(pub.events); n != tt.wantStarted {
				t.Errorf("RideStartedEvent (dropoff push) count=%d, want %d", n, tt.wantStarted)
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

// TestRideRequestHandler_Start_DropoffSeamPayload proves the leg-2 dispatch seam
// carries the ride's DROPOFF coordinate (not the pickup) so the nav dispatcher
// pushes the correct destination.
func TestRideRequestHandler_Start_DropoffSeamPayload(t *testing.T) {
	rideRec := fixtureRideData(rideUserID, rideStatusArrived)
	store := &fakeRideStore{getRec: rideRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/start", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
	}

	var started *events.RideStartedEvent
	for _, e := range pub.events {
		if b, ok := e.Payload.(events.RideStartedEvent); ok {
			bb := b
			started = &bb
		}
	}
	if started == nil {
		t.Fatal("no RideStartedEvent published on a successful start")
	}
	if started.RideRequestID != rideID || started.VehicleID != rideVehicle || started.OwnerID != rideRec.OwnerID {
		t.Errorf("started ids: %+v", started)
	}
	if started.Dropoff.Latitude != rideRec.Dropoff.Latitude || started.Dropoff.Longitude != rideRec.Dropoff.Longitude {
		t.Errorf("started dropoff = %+v, want the ride's dropoff %+v", started.Dropoff, rideRec.Dropoff)
	}
}

// TestRideRequestHandler_Start_GuardWinsRace: the pre-check read sees arrived
// (legal) but by write time the row moved outside the allowed-from set (e.g. a
// concurrent cancel). The guarded write refuses — 409, and NO dropoff dispatch
// seam is published.
func TestRideRequestHandler_Start_GuardWinsRace(t *testing.T) {
	readRec := fixtureRideData(rideUserID, rideStatusArrived)    // pre-check passes
	writeRec := fixtureRideData(rideUserID, rideStatusCancelled) // guard sees the concurrent cancel
	store := &fakeRideStore{getRec: readRec, updated: writeRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/start", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
	if store.updateCalls != 1 {
		t.Errorf("expected exactly one guarded write attempt, got %d", store.updateCalls)
	}
	if len(store.updatedFrom) != 1 || store.updatedFrom[0] != rideStatusArrived {
		t.Errorf("guard allowed-from set: %v, want [arrived]", store.updatedFrom)
	}
	if len(pub.events) != 0 {
		t.Errorf("losing start must publish no events (incl. no dropoff seam), got %d", len(pub.events))
	}
}
