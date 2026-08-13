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

// MYR-539 — the stop list on the trip PATCH. The client's model: exactly ONE
// pickup → N ordered stops → ONE destination, both parties editing, instantly,
// under the one tripVersion the endpoints already share.

// rideWithStops returns a ride carrying stops in the given statuses. The OWNER
// is the other user, so rideUserID is the RIDER — the party the start endpoint
// admits and a party the trip PATCH admits.
func rideWithStops(status string, stopStatuses ...string) RideRequestData {
	rec := fixtureRideData(rideOtherUsr, status)
	for i, st := range stopStatuses {
		rec.Stops = append(rec.Stops, RideStopData{
			ID:     []string{"crs01", "crs02", "crs03"}[i],
			Place:  RidePlaceData{Latitude: 37.78 + float64(i)/100, Longitude: -122.4, Label: "Stop"},
			Status: st,
		})
	}
	return rec
}

const twoNewStopsBody = `{"tripVersion":0,"stops":[` +
	`{"place":{"lat":37.781,"lng":-122.41,"label":"Bakery"}},` +
	`{"place":{"lat":37.782,"lng":-122.42,"label":"Pharmacy"}}]}`

// The window: stops take the DROP-OFF's, so they are editable through every
// live status and refused only once the ride has ended.
func TestRideTripPatch_StopsWindow(t *testing.T) {
	tests := []struct {
		status     string
		wantStatus int
	}{
		{status: rideStatusRequested, wantStatus: http.StatusOK},
		{status: rideStatusAccepted, wantStatus: http.StatusOK},
		{status: rideStatusArrived, wantStatus: http.StatusOK},
		{status: rideStatusEnroute, wantStatus: http.StatusOK},
		{status: rideStatusCompleted, wantStatus: http.StatusConflict},
		{status: rideStatusCancelled, wantStatus: http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run("stops from "+tt.status, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(rideUserID, tt.status)}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)

			resp := tripPatch(t, h, twoNewStopsBody)
			if resp.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", resp.Code, tt.wantStatus, resp.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			// A stops edit is handed the DROP-OFF's window, never the pickup's
			// narrower one — a stop edit must not lose to a concurrent
			// picked-up the way a pickup edit must.
			if !sameStatusSet(store.updatedFrom, tripDropoffEditableFrom) {
				t.Errorf("guarded window = %v, want the dropoff window %v", store.updatedFrom, tripDropoffEditableFrom)
			}
		})
	}
}

// The wire projection: the stops come back in travel order with their
// server-owned ids and statuses, and a trip with none omits the key entirely.
func TestRideTripPatch_StopsWireProjection(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusAccepted)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)

	resp := tripPatch(t, h, twoNewStopsBody)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	var got struct {
		Stops []struct {
			ID    string `json:"id"`
			Statu string `json:"status"`
			Place struct {
				Lat   float64 `json:"lat"`
				Label string  `json:"label"`
			} `json:"place"`
		} `json:"stops"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Stops) != 2 {
		t.Fatalf("stops = %d, want 2. body=%s", len(got.Stops), resp.Body.String())
	}
	if got.Stops[0].Place.Label != "Bakery" || got.Stops[1].Place.Label != "Pharmacy" {
		t.Errorf("travel order lost: %+v", got.Stops)
	}
	for i, s := range got.Stops {
		if s.ID == "" {
			t.Errorf("stop %d came back with no server-minted id", i)
		}
		if s.Statu != rideStopUpcoming {
			t.Errorf("stop %d status = %q, want upcoming — statuses are server-owned", i, s.Statu)
		}
	}
}

// A plain two-endpoint trip OMITS the key, so every ride written before stops
// existed serializes byte-identically.
func TestRideRequestWire_OmitsStopsWhenThereAreNone(t *testing.T) {
	body, err := json.Marshal(toRideRequestWire(fixtureRideData(rideUserID, rideStatusAccepted), nil, time.Now()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "stops") {
		t.Errorf("a stopless trip must omit the key entirely: %s", body)
	}
}

// The event pair a stops edit publishes: the dispatcher is told the list moved
// AND where the current leg now ends, because once a trip has stops that target
// is not derivable from the endpoints.
func TestRideTripPatch_StopsEditCarriesTheLegTarget(t *testing.T) {
	rec := rideWithStops(rideStatusEnroute, rideStopCurrent, rideStopUpcoming)
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	// Keep the current stop where it is, replace the second.
	body := `{"tripVersion":0,"stops":[` +
		`{"id":"crs01","place":{"lat":37.78,"lng":-122.4,"label":"Stop"}},` +
		`{"place":{"lat":37.9,"lng":-122.5,"label":"New second"}}]}`
	resp := tripPatch(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	if len(pub.events) != 2 {
		t.Fatalf("published %d events, want the pair", len(pub.events))
	}
	trip, ok := pub.events[1].Payload.(events.RideTripChangedEvent)
	if !ok {
		t.Fatalf("second event = %T, want RideTripChangedEvent", pub.events[1].Payload)
	}
	if !trip.StopsChanged {
		t.Error("StopsChanged must be set — the dispatcher cannot otherwise know the leg target may have moved")
	}
	if trip.LegTarget == nil {
		t.Fatal("LegTarget must be set on a stops edit")
	}
	// The CURRENT stop is where the car is driving; a replacement that left it
	// in place must not re-point the dash at anything else.
	if trip.LegTarget.Label != "Stop" {
		t.Errorf("LegTarget = %+v, want the current stop", trip.LegTarget)
	}
	if trip.NewPickup != nil || trip.NewDropoff != nil {
		t.Error("a stops-only edit must not claim either endpoint moved")
	}
}

// With every stop cleared, the current leg's target is the drop-off again.
func TestRideTripPatch_ClearingStopsTargetsTheDropoff(t *testing.T) {
	rec := rideWithStops(rideStatusEnroute, rideStopCurrent)
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := tripPatch(t, h, `{"tripVersion":0,"stops":[]}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	trip, ok := pub.events[1].Payload.(events.RideTripChangedEvent)
	if !ok {
		t.Fatalf("second event = %T", pub.events[1].Payload)
	}
	if trip.LegTarget == nil || trip.LegTarget.Latitude != rec.Dropoff.Latitude {
		t.Errorf("LegTarget = %+v, want the drop-off %+v", trip.LegTarget, rec.Dropoff)
	}
}

// The two refusals are deliberately different classes: rewriting the road
// behind the car is a 409 the client retries after a refetch; an id the ride
// does not have is a 400 no retry could fix.
func TestRideTripPatch_StopRefusals(t *testing.T) {
	tests := []struct {
		name       string
		storeErr   error
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name:       "removing a stop the car already reached conflicts",
			storeErr:   ErrRideStatusConflict,
			wantStatus: http.StatusConflict,
			wantCode:   wserrors.ErrCodeConflict,
		},
		{
			name:       "an unknown stop id is a bad request",
			storeErr:   ErrRideStopUnknown,
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: rideWithStops(rideStatusEnroute, rideStopCompleted), stopsErr: tt.storeErr}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

			resp := tripPatch(t, h, twoNewStopsBody)
			assertErrEnvelope(t, resp, tt.wantStatus, tt.wantCode)
			if len(pub.events) != 0 {
				t.Errorf("a refused edit must publish nothing, got %d", len(pub.events))
			}
		})
	}
}

// Body validation: every entry states its own place, and a version-only body is
// still nothing to change.
func TestRideTripPatch_StopsValidation(t *testing.T) {
	for name, body := range map[string]string{
		"missing place on an entry": `{"tripVersion":0,"stops":[{"id":"crs01"}]}`,
		"out-of-range stop lat":     `{"tripVersion":0,"stops":[{"place":{"lat":99,"lng":2,"label":"X"}}]}`,
		"empty stop label":          `{"tripVersion":0,"stops":[{"place":{"lat":1,"lng":2,"label":""}}]}`,
		"empty id when present":     `{"tripVersion":0,"stops":[{"id":"","place":{"lat":1,"lng":2,"label":"X"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusAccepted)}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)
			resp := tripPatch(t, h, body)
			assertErrEnvelope(t, resp, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
			if store.updateCalls != 0 {
				t.Errorf("an invalid body must never reach the store, got %d writes", store.updateCalls)
			}
		})
	}
}

// An edit that omits `stops` leaves the list alone — the key's absence is not
// "clear them", which is the difference between moving a drop-off and deleting
// somebody's errands.
func TestRideTripPatch_AbsentStopsKeyLeavesTheListAlone(t *testing.T) {
	store := &fakeRideStore{getRec: rideWithStops(rideStatusEnroute, rideStopCurrent)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)

	resp := tripPatch(t, h, newDropoffBody)
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	if store.tripEdit == nil || store.tripEdit.Stops != nil {
		t.Errorf("an absent stops key must reach the store as nil, got %+v", store.tripEdit)
	}
}
