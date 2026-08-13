package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"
)

// MYR-547 (server half) — the owner ends the ride at an intermediate stop.
//
// THE DECISION THESE TESTS HOLD. An early end is legitimate and is never
// refused; the stop rows it leaves behind are left EXACTLY as the last observed
// arrival wrote them. On a terminal ride the statuses read as a record of how
// far the car got — `completed` is a stop the car reached, anything else is one
// it did not — rather than as live claims about a leg in progress.
//
// The rejected alternative is marking the remainder `completed` at ride end.
// That fabricates arrivals the car never made, and it erases where the ride
// actually stopped, which is the whole subject of an early completion and the
// record a disputed trip would be read from. There is no "skipped" member
// either: a new enum value is a contract change for every SDK, to say something
// the pair (ride.status, stop.status) already says exactly.
//
// The CLIENT gains a confirm ("you still have 2 stops") — that half is iOS's,
// and it is a confirm, not a block.

// partwayTripFixture is a ride mid-trip: the car has reached stop 1, is driving
// to stop 2, and stop 3 is still ahead of it.
func partwayTripFixture() RideRequestData {
	owner := rideOwnerID + "X"
	rec := fixtureRideData(owner, rideStatusEnroute)
	rec.Stops = []RideStopData{
		{ID: "crs001", Place: RidePlaceData{Latitude: 37.78, Longitude: -122.40, Label: "Pharmacy"}, Status: rideStopCompleted},
		{ID: "crs002", Place: RidePlaceData{Latitude: 37.77, Longitude: -122.41, Label: "School"}, Status: rideStopCurrent},
		{ID: "crs003", Place: RidePlaceData{Latitude: 37.76, Longitude: -122.42, Label: "Gym"}, Status: rideStopUpcoming},
	}
	return rec
}

// The early end is ALLOWED — an owner is not trapped in a trip because a stop
// list still has entries — and it writes nothing to the stop list.
func TestEarlyCompletionIsAllowedAndLeavesTheStopsAlone(t *testing.T) {
	rec := partwayTripFixture()
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rec.OwnerID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/dropped-off", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("an early end must not be refused: %d %s", resp.Code, resp.Body.String())
	}

	// Exactly ONE write, and it is the status transition — no second statement
	// touching the stops rode along with it.
	if store.updateCalls != 1 {
		t.Errorf("expected exactly the status write, got %d writes", store.updateCalls)
	}
	if store.updatedState != rideStatusCompleted {
		t.Errorf("target status: got %q want %q", store.updatedState, rideStatusCompleted)
	}
	if len(store.updatedFrom) != 1 || store.updatedFrom[0] != rideStatusEnroute {
		t.Errorf("allowed-from set: %v — the stop list is no part of the guard", store.updatedFrom)
	}
	// The completion is not an edit: nothing bumps the trip version and nothing
	// goes through the trip write.
	if store.tripEdit != nil {
		t.Errorf("completion must not run a trip edit: %+v", store.tripEdit)
	}
	if store.startFirstStopCalls != 0 {
		t.Errorf("completion must not promote a stop, got %d calls", store.startFirstStopCalls)
	}
}

// The response — and therefore the client's record of the finished ride —
// carries the stops with their statuses UNCHANGED. `completed` on stop 1 is the
// arrival that happened; `current` on stop 2 is where the ride ended; `upcoming`
// on stop 3 is a place the car never went. Two of those three would be lies if
// the completion had stamped them completed.
func TestEarlyCompletionKeepsTheStopStatusesHonest(t *testing.T) {
	rec := partwayTripFixture()
	store := &fakeRideStore{getRec: rec}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rec.OwnerID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/dropped-off", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: %d %s", resp.Code, resp.Body.String())
	}

	var got struct {
		Status string `json:"status"`
		Stops  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"stops"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != rideStatusCompleted {
		t.Errorf("ride status: got %q want %q", got.Status, rideStatusCompleted)
	}
	// The list survives the completion — a finished trip still says where it
	// went, which is what makes the statuses readable as a record at all.
	if len(got.Stops) != 3 {
		t.Fatalf("expected all 3 stops on the completed ride, got %d", len(got.Stops))
	}
	want := []string{rideStopCompleted, rideStopCurrent, rideStopUpcoming}
	for i, w := range want {
		if got.Stops[i].Status != w {
			t.Errorf("stop %d (%s): got %q want %q — the completion must not rewrite the road",
				i, got.Stops[i].ID, got.Stops[i].Status, w)
		}
	}
}

// The same reading holds for the other early end. A cancelled mid-trip ride
// leaves its stops exactly where they were too, so one rule covers both terminal
// transitions rather than two that could drift.
func TestMidTripCancelLeavesTheStopsAlone(t *testing.T) {
	rec := partwayTripFixture()
	store := &fakeRideStore{getRec: rec}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("MYR-537 lets a rider end a live ride: %d %s", resp.Code, resp.Body.String())
	}
	if store.updateCalls != 1 || store.tripEdit != nil {
		t.Errorf("a cancel is one status write and no stop write: calls=%d edit=%+v", store.updateCalls, store.tripEdit)
	}

	var got struct {
		Stops []struct {
			Status string `json:"status"`
		} `json:"stops"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Stops) != 3 || got.Stops[1].Status != rideStopCurrent {
		t.Errorf("a cancelled trip keeps the leg it was on: %+v", got.Stops)
	}
}
