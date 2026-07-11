package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// withRequester returns a copy of a fixture ride with RequesterName set to the
// given pointer (nil = unresolved → the wire field must be omitted).
func withRequester(d RideRequestData, name *string) RideRequestData {
	d.RequesterName = name
	return d
}

// TestRideRequestHandler_RequesterName_RESTDetail asserts the party-only detail
// projection carries requesterName when resolved and omits the key when not.
func TestRideRequestHandler_RequesterName_RESTDetail(t *testing.T) {
	name := "Maya"
	tests := []struct {
		name        string
		requester   *string
		wantPresent bool
		wantValue   string
	}{
		{name: "resolved name present", requester: &name, wantPresent: true, wantValue: "Maya"},
		{name: "unresolved name omitted", requester: nil, wantPresent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := withRequester(fixtureRideData(rideUserID, rideStatusRequested), tt.requester)
			store := &fakeRideStore{getRec: rec}
			h := newRideHandler(store, nil, nil, rideUserID)

			resp := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/"+rideID, "", rideAuthOK)
			if resp.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200. body=%s", resp.Code, resp.Body.String())
			}

			var body map[string]any
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, present := body["requesterName"]
			if present != tt.wantPresent {
				t.Fatalf("requesterName present=%v want %v (body=%s)", present, tt.wantPresent, resp.Body.String())
			}
			if tt.wantPresent && got != tt.wantValue {
				t.Errorf("requesterName = %v want %q", got, tt.wantValue)
			}
		})
	}
}

// TestRideRequestHandler_RequesterName_RESTList asserts each list item carries
// its own requester projection.
func TestRideRequestHandler_RequesterName_RESTList(t *testing.T) {
	maya := "Maya"
	item := withRequester(fixtureRideData(rideUserID, rideStatusRequested), &maya)
	store := &fakeRideStore{riderPage: RideRequestListPage{Items: []RideRequestData{item}, HasMore: false}}
	h := newRideHandler(store, nil, nil, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200. body=%s", resp.Code, resp.Body.String())
	}

	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("got %d items want 1", len(body.Items))
	}
	if body.Items[0]["requesterName"] != "Maya" {
		t.Errorf("list item requesterName = %v want %q", body.Items[0]["requesterName"], "Maya")
	}
}

// TestRideRequestHandler_RequesterName_CreatedEvent asserts the WS
// ride_request_created event carries the resolved requester name from the
// store's created record.
func TestRideRequestHandler_RequesterName_CreatedEvent(t *testing.T) {
	maya := "Maya"
	store := &fakeRideStore{created: withRequester(fixtureRideData(rideUserID, rideStatusRequested), &maya)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, pub, rideUserID)

	body := `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"}}`
	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", body, rideAuthOK)
	if resp.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201. body=%s", resp.Code, resp.Body.String())
	}

	// REST 201 body carries the field too.
	var got map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["requesterName"] != "Maya" {
		t.Errorf("201 body requesterName = %v want %q", got["requesterName"], "Maya")
	}

	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	ev, ok := pub.events[0].Payload.(events.RideRequestCreatedEvent)
	if !ok {
		t.Fatalf("expected RideRequestCreatedEvent, got %T", pub.events[0].Payload)
	}
	if ev.RequesterName == nil || *ev.RequesterName != "Maya" {
		t.Errorf("created event RequesterName = %v want %q", deref(ev.RequesterName), "Maya")
	}
}

// TestRideRequestHandler_RequesterName_StatusChangedEvent asserts the WS
// ride_status_changed event carries the requester name from the updated record.
func TestRideRequestHandler_RequesterName_StatusChangedEvent(t *testing.T) {
	maya := "Maya"
	readRec := fixtureRideData(rideUserID, rideStatusRequested)
	// The fake treats updated.Status as the pre-write "current" for its
	// legality gate, so keep it a legal from-state (requested); the fake
	// overwrites Status to the target (cancelled) on the returned record.
	writeRec := withRequester(fixtureRideData(rideUserID, rideStatusRequested), &maya)
	store := &fakeRideStore{getRec: readRec, updated: writeRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, nil, pub, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200. body=%s", resp.Code, resp.Body.String())
	}

	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
	if !ok {
		t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
	}
	if ev.RequesterName == nil || *ev.RequesterName != "Maya" {
		t.Errorf("status-changed event RequesterName = %v want %q", deref(ev.RequesterName), "Maya")
	}
}

// deref renders an optional string pointer for test failure messages.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
