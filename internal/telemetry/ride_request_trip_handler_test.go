package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-541 — the trip-shape edit. The client's rule, verbatim: "allow the
// users to change the pick up or drop off at anytime. Obv if pick up has past
// they can only change drop-off."

func tripPatch(t *testing.T, h *RideRequestHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, rideMux(h), http.MethodPatch, "/api/ride-requests/"+rideID+"/trip", body, rideAuthOK)
}

const newDropoffBody = `{"tripVersion":0,"dropoff":{"lat":32.9,"lng":-99.2,"label":"Fort Griffin"}}`
const newPickupBody = `{"tripVersion":0,"pickup":{"lat":33.1,"lng":-96.8,"label":"New pickup"}}`

// The window matrix, both endpoints × every status.
func TestRideTripPatch_Windows(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{name: "dropoff from requested", status: rideStatusRequested, body: newDropoffBody, wantStatus: http.StatusOK},
		{name: "dropoff from accepted", status: rideStatusAccepted, body: newDropoffBody, wantStatus: http.StatusOK},
		{name: "dropoff from arrived", status: rideStatusArrived, body: newDropoffBody, wantStatus: http.StatusOK},
		{name: "dropoff from enroute (aboard)", status: rideStatusEnroute, body: newDropoffBody, wantStatus: http.StatusOK},
		{name: "pickup from requested", status: rideStatusRequested, body: newPickupBody, wantStatus: http.StatusOK},
		{name: "pickup from accepted", status: rideStatusAccepted, body: newPickupBody, wantStatus: http.StatusOK},
		// The client's own boundary: "if pick up has past they can only
		// change drop-off" — and the refusal NAMES the door still open.
		{name: "pickup from arrived refused", status: rideStatusArrived, body: newPickupBody, wantStatus: http.StatusConflict, wantMsg: "only the drop-off"},
		{name: "pickup from enroute refused", status: rideStatusEnroute, body: newPickupBody, wantStatus: http.StatusConflict, wantMsg: "only the drop-off"},
		{name: "nothing after completed", status: rideStatusCompleted, body: newDropoffBody, wantStatus: http.StatusConflict, wantMsg: "ended"},
		{name: "nothing after cancelled", status: rideStatusCancelled, body: newDropoffBody, wantStatus: http.StatusConflict, wantMsg: "ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(rideUserID, tt.status)}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

			resp := tripPatch(t, h, tt.body)
			if resp.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", resp.Code, tt.wantStatus, resp.Body.String())
			}
			if tt.wantMsg != "" && !strings.Contains(resp.Body.String(), tt.wantMsg) {
				t.Errorf("refusal should say %q, body=%s", tt.wantMsg, resp.Body.String())
			}
		})
	}
}

// A winning edit bumps the version on the wire, and publishes BOTH events:
// the TripEdit-marked status frame (unchanged status, new version — the WS
// refetch signal) and the trip-changed seam with the new place.
func TestRideTripPatch_PublishesTheEditPair(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusEnroute)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := tripPatch(t, h, newDropoffBody)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &got)
	if got["tripVersion"] != float64(1) {
		t.Errorf("wire tripVersion = %v, want 1", got["tripVersion"])
	}
	if len(pub.events) != 2 {
		t.Fatalf("published %d events, want the pair", len(pub.events))
	}
	status, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
	if !ok || !status.TripEdit || status.Status != rideStatusEnroute || status.TripVersion != 1 {
		t.Errorf("first event = %+v, want TripEdit-marked, status unchanged, version 1", pub.events[0].Payload)
	}
	trip, ok := pub.events[1].Payload.(events.RideTripChangedEvent)
	if !ok || trip.NewDropoff == nil || trip.NewDropoff.Label != "Fort Griffin" || trip.NewPickup != nil {
		t.Errorf("second event = %+v, want the edited dropoff alone", pub.events[1].Payload)
	}
	if trip.EditorUserID != rideUserID {
		t.Errorf("editor = %q", trip.EditorUserID)
	}
}

// A stale version loses in the guard: 409, nothing published.
func TestRideTripPatch_StaleVersionLoses(t *testing.T) {
	rec := fixtureRideData(rideUserID, rideStatusAccepted)
	rec.TripVersion = 3
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := tripPatch(t, h, newDropoffBody) // holds version 0 against 3
	assertErrEnvelope(t, resp, http.StatusConflict, wserrors.ErrCodeConflict)
	if len(pub.events) != 0 {
		t.Errorf("a losing edit must publish nothing, got %d", len(pub.events))
	}
}

// Validation: an empty body and a bad place are 400s that never reach the store.
func TestRideTripPatch_Validation(t *testing.T) {
	for name, body := range map[string]string{
		"missing version":  `{"dropoff":{"lat":1,"lng":2,"label":"X"}}`,
		"nothing to edit":  `{"tripVersion":0}`,
		"out-of-range lat": `{"tripVersion":0,"dropoff":{"lat":99,"lng":2,"label":"X"}}`,
		"missing label":    `{"tripVersion":0,"dropoff":{"lat":1,"lng":2,"label":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusAccepted)}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
			resp := tripPatch(t, h, body)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400. body=%s", resp.Code, resp.Body.String())
			}
			if store.updateCalls != 0 {
				t.Errorf("an invalid body must never reach the store")
			}
		})
	}
}

// The OWNER is a party and may edit (the client's collaboration decision);
// a non-party is a 404, never an existence leak.
func TestRideTripPatch_ACL(t *testing.T) {
	owner := rideOwnerID + "X"
	store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusAccepted)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
	if resp := tripPatch(t, h, newDropoffBody); resp.Code != http.StatusOK {
		t.Fatalf("owner edit: %d %s", resp.Code, resp.Body.String())
	}

	stranger := &fakeRideStore{getRec: fixtureRideData(rideOwnerID+"X", rideStatusAccepted)}
	h2 := newRideHandler(stranger, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
	if resp := tripPatch(t, h2, newDropoffBody); resp.Code != http.StatusNotFound {
		t.Fatalf("non-party: %d, want 404", resp.Code)
	}
}
