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

// --- Incoming feed (MYR-175) ---

func TestRideRequestHandler_Incoming(t *testing.T) {
	t.Run("scopes to owner and requested status", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming?limit=7", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if store.ownerCall.id != rideOtherUsr || store.ownerCall.limit != 7 {
			t.Errorf("owner call: id=%q limit=%d", store.ownerCall.id, store.ownerCall.limit)
		}
		if store.ownerCall.status == nil || *store.ownerCall.status != rideStatusRequested {
			t.Errorf("status filter: %v", store.ownerCall.status)
		}
	})

	t.Run("empty feed yields items [] and null cursor", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", rideAuthOK)
		if !strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"nextCursor":null`) {
			t.Errorf("envelope: %s", rec.Body.String())
		}
	})

	t.Run("hasMore emits decodable cursor over (createdAt,id)", func(t *testing.T) {
		last := fixtureRideData(rideOtherUsr, rideStatusRequested)
		store := &fakeRideStore{ownerPage: RideRequestListPage{Items: []RideRequestData{last}, HasMore: true}}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", rideAuthOK)
		var env struct {
			NextCursor *string `json:"nextCursor"`
			HasMore    bool    `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !env.HasMore || env.NextCursor == nil {
			t.Fatalf("expected hasMore + cursor: %+v", env)
		}
		cur, err := decodeRideCursor(*env.NextCursor)
		if err != nil || cur.ID != last.ID || !cur.CreatedAt.Equal(last.CreatedAt) {
			t.Errorf("cursor: %+v err=%v", cur, err)
		}
	})

	t.Run("400 on malformed cursor", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming?cursor=@@", "", rideAuthOK)
		assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
	})

	t.Run("401 without auth", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", "")
		assertErrEnvelope(t, rec, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed)
	})
}

// --- Accept / decline (MYR-175) ---

func TestRideRequestHandler_AcceptDecline(t *testing.T) {
	const owner = rideOtherUsr // rides fixture: rider=rideUserID, owner=rideOtherUsr

	tests := []struct {
		name        string
		action      string // "accept" | "decline"
		caller      string
		rec         RideRequestData
		getErr      error
		wantStatus  int
		wantErrCode wserrors.ErrorCode
		wantState   string // expected UpdateStatus arg on success
	}{
		{name: "owner accepts requested", action: "accept", caller: owner, rec: fixtureRideData(owner, rideStatusRequested), wantStatus: http.StatusOK, wantState: rideStatusAccepted},
		{name: "owner declines requested", action: "decline", caller: owner, rec: fixtureRideData(owner, rideStatusRequested), wantStatus: http.StatusOK, wantState: rideStatusDeclined},
		{name: "accept conflict from accepted", action: "accept", caller: owner, rec: fixtureRideData(owner, rideStatusAccepted), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "accept conflict from cancelled", action: "accept", caller: owner, rec: fixtureRideData(owner, rideStatusCancelled), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "decline conflict from declined", action: "decline", caller: owner, rec: fixtureRideData(owner, rideStatusDeclined), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "decline conflict from completed", action: "decline", caller: owner, rec: fixtureRideData(owner, rideStatusCompleted), wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeConflict},
		{name: "rider cannot accept (owner-only)", action: "accept", caller: rideUserID, rec: fixtureRideData(owner, rideStatusRequested), wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied},
		{name: "rider cannot decline (owner-only)", action: "decline", caller: rideUserID, rec: fixtureRideData(owner, rideStatusRequested), wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied},
		{name: "non-party gets 404", action: "accept", caller: "clstranger00000000000", rec: fixtureRideData(owner, rideStatusRequested), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
		{name: "unknown id 404", action: "decline", caller: owner, getErr: fmtNotFound(), wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tt.rec, getErr: tt.getErr}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, tt.caller)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/"+tt.action, "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
				if len(pub.events) != 0 {
					t.Errorf("no events expected on failure, got %d", len(pub.events))
				}
				return
			}

			if store.updatedState != tt.wantState {
				t.Errorf("UpdateStatus arg: got %q want %q", store.updatedState, tt.wantState)
			}
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if got["status"] != tt.wantState {
				t.Errorf("body status: %v", got["status"])
			}

			// Both actions emit ride_status_changed; accept additionally
			// emits the dispatch seam.
			wantEvents := 1
			if tt.action == "accept" {
				wantEvents = 2
			}
			if len(pub.events) != wantEvents {
				t.Fatalf("events: got %d want %d", len(pub.events), wantEvents)
			}
			if _, ok := pub.events[0].Payload.(events.RideStatusChangedEvent); !ok {
				t.Errorf("first event: expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
			}
			if tt.action == "accept" {
				if _, ok := pub.events[1].Payload.(events.RideAcceptedEvent); !ok {
					t.Errorf("second event: expected RideAcceptedEvent, got %T", pub.events[1].Payload)
				}
			}
		})
	}
}

// TestRideRequestHandler_Accept_DispatchSeamPayload asserts the ride.accepted
// event carries everything MYR-176 needs for the Tesla navigation_request:
// both places (with address flattening), the booked-for passenger contact,
// scheduledFor, and the accepted_at stamp.
func TestRideRequestHandler_Accept_DispatchSeamPayload(t *testing.T) {
	const owner = rideOtherUsr
	ride := fixtureRideData(owner, rideStatusRequested)
	name := "Maya Chen"
	phone := "(415) 555-0142"
	sched := time.Date(2026, 6, 18, 16, 0, 0, 0, time.UTC)
	accepted := time.Date(2026, 6, 15, 16, 13, 5, 0, time.UTC)
	ride.PassengerName = &name
	ride.PassengerPhone = &phone
	ride.ScheduledFor = &sched

	updated := ride
	updated.AcceptedAt = &accepted

	store := &fakeRideStore{getRec: ride, updated: updated}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, pub, owner)
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if len(pub.events) != 2 {
		t.Fatalf("events: got %d want 2", len(pub.events))
	}
	ev, ok := pub.events[1].Payload.(events.RideAcceptedEvent)
	if !ok {
		t.Fatalf("expected RideAcceptedEvent, got %T", pub.events[1].Payload)
	}
	if ev.RideRequestID != rideID || ev.VehicleID != rideVehicle || ev.RiderID != rideUserID || ev.OwnerID != owner {
		t.Errorf("ids: %+v", ev)
	}
	if ev.Pickup.Latitude != ride.Pickup.Latitude || ev.Pickup.Label != "Home" || ev.Pickup.Address == "" {
		t.Errorf("pickup: %+v", ev.Pickup)
	}
	if ev.Dropoff.Label != "Caltrain" || ev.Dropoff.Address != "" {
		t.Errorf("dropoff (nil address must flatten to empty): %+v", ev.Dropoff)
	}
	if ev.PassengerName != name || ev.PassengerPhone != phone {
		t.Errorf("passenger: %q / %q", ev.PassengerName, ev.PassengerPhone)
	}
	if ev.ScheduledFor == nil || !ev.ScheduledFor.Equal(sched) {
		t.Errorf("scheduledFor: %v", ev.ScheduledFor)
	}
	if !ev.AcceptedAt.Equal(accepted) {
		t.Errorf("acceptedAt: %v want %v", ev.AcceptedAt, accepted)
	}
}

// TestRideMux_IncomingLiteralWinsOverWildcard is a regression guard for the
// ServeMux precedence assumption: GET /api/ride-requests/incoming must route
// to the owner feed, not to ServeGet with id="incoming".
func TestRideMux_IncomingLiteralWinsOverWildcard(t *testing.T) {
	store := &fakeRideStore{getErr: fmtNotFound()} // ServeGet would 404
	h := newRideHandler(store, &stubVehicleSnapshotReader{}, &fakeRidePublisher{}, rideOtherUsr)
	rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the incoming feed (200), got %d: %s", rec.Code, rec.Body.String())
	}
	if store.ownerCall.id != rideOtherUsr {
		t.Errorf("expected ListByOwnerPage to be hit, got ownerCall=%+v", store.ownerCall)
	}
}
