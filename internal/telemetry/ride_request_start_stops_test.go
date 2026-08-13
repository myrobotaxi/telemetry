package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-539 — the rider's Start on a multi-stop trip. Leg 2 goes to the FIRST
// STOP, not past it, and the stop the car is sent to is the same one the
// database now calls `current`.

// startRide runs POST /start as the rider and returns the recorder.
func startRide(t *testing.T, h *RideRequestHandler) *jsonResponse {
	t.Helper()
	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/start", "", rideAuthOK)
	return &jsonResponse{code: resp.Code, body: resp.Body.Bytes()}
}

type jsonResponse struct {
	code int
	body []byte
}

// startedEvent finds the leg-2 dispatch seam among the published events.
func startedEvent(t *testing.T, pub *fakeRidePublisher) events.RideStartedEvent {
	t.Helper()
	for _, e := range pub.events {
		if ev, ok := e.Payload.(events.RideStartedEvent); ok {
			return ev
		}
	}
	t.Fatal("no RideStartedEvent published")
	return events.RideStartedEvent{}
}

func TestRideStart_TargetsTheFirstStop(t *testing.T) {
	rec := rideWithStops(rideStatusArrived, rideStopUpcoming, rideStopUpcoming)
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := startRide(t, h)
	if resp.code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.code, resp.body)
	}
	if store.startFirstStopCalls != 1 {
		t.Errorf("StartFirstStop calls = %d, want exactly 1", store.startFirstStopCalls)
	}

	ev := startedEvent(t, pub)
	first := rec.Stops[0].Place
	if ev.Target.Latitude != first.Latitude || ev.Target.Longitude != first.Longitude {
		t.Errorf("leg-2 target = %+v, want the first stop %+v — the car would drive past every stop",
			ev.Target, first)
	}

	// The rider's own 200 already shows the leg under way, rather than a list
	// claiming `upcoming` for a stop the car is driving to.
	var got struct {
		Stops []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"stops"`
	}
	if err := json.Unmarshal(resp.body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Stops) != 2 || got.Stops[0].Status != rideStopCurrent {
		t.Errorf("response stops = %+v, want the first one current", got.Stops)
	}
	if got.Stops[1].Status != rideStopUpcoming {
		t.Errorf("only ONE stop may be current, got %+v", got.Stops)
	}
}

// A plain two-endpoint trip is unchanged: leg 2 targets the drop-off, exactly
// as it did before stops existed.
func TestRideStart_WithoutStopsTargetsTheDropoff(t *testing.T) {
	rec := fixtureRideData(rideOtherUsr, rideStatusArrived)
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	if resp := startRide(t, h); resp.code != http.StatusOK {
		t.Fatalf("status %d: %s", resp.code, resp.body)
	}
	ev := startedEvent(t, pub)
	if ev.Target.Latitude != rec.Dropoff.Latitude || ev.Target.Longitude != rec.Dropoff.Longitude {
		t.Errorf("leg-2 target = %+v, want the drop-off %+v", ev.Target, rec.Dropoff)
	}
}

// The promotion failing must not strand a rider who is already aboard: the ride
// has started, so the car is dispatched to the drop-off rather than nowhere.
func TestRideStart_PromotionFailureStillDispatches(t *testing.T) {
	rec := rideWithStops(rideStatusArrived, rideStopUpcoming)
	store := &fakeRideStore{getRec: rec, startFirstStopErr: errors.New("database unwell")}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := startRide(t, h)
	if resp.code != http.StatusOK {
		t.Fatalf("a started ride must still answer 200, got %d: %s", resp.code, resp.body)
	}
	ev := startedEvent(t, pub)
	if ev.Target.Latitude != rec.Dropoff.Latitude {
		t.Errorf("fallback target = %+v, want the drop-off %+v", ev.Target, rec.Dropoff)
	}
}

// An idempotent re-start (already enroute) promotes nothing and re-publishes
// nothing — the leg-2 push stays exactly-once per start.
func TestRideStart_IdempotentRestartPromotesNothing(t *testing.T) {
	store := &fakeRideStore{getRec: rideWithStops(rideStatusEnroute, rideStopCurrent)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	if resp := startRide(t, h); resp.code != http.StatusOK {
		t.Fatalf("status %d", resp.code)
	}
	if store.startFirstStopCalls != 0 {
		t.Errorf("a no-op re-start must not promote, got %d calls", store.startFirstStopCalls)
	}
	if len(pub.events) != 0 {
		t.Errorf("a no-op re-start must publish nothing, got %d", len(pub.events))
	}
}
