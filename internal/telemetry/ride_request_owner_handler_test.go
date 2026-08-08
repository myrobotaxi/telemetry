package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// --- Incoming feed (MYR-175) ---

func TestRideRequestHandler_Incoming(t *testing.T) {
	t.Run("scopes to owner and requested status", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", rideAuthOK)
		if !strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"nextCursor":null`) {
			t.Errorf("envelope: %s", rec.Body.String())
		}
	})

	t.Run("hasMore emits decodable cursor over (createdAt,id)", func(t *testing.T) {
		last := fixtureRideData(rideOtherUsr, rideStatusRequested)
		store := &fakeRideStore{ownerPage: RideRequestListPage{Items: []RideRequestData{last}, HasMore: true}}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
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
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
		rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming?cursor=@@", "", rideAuthOK)
		assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
	})

	t.Run("401 without auth", func(t *testing.T) {
		store := &fakeRideStore{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
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
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, tt.caller)
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

// TestRideRequestHandler_Accept_VehicleAvailabilityGate seals the MYR-277
// dispatch-capability gate: an owner may accept a ride only for a vehicle that
// can currently fulfill it. `in_service` (owner put it into service) and
// `offline` (unreachable) are blocked with 409 vehicle_unavailable and must
// NOT transition the ride or publish any event (neither the status change nor
// the dispatch seam). `parked`/`driving`/`charging` are dispatchable and let
// the accept proceed to the guarded requested->accepted write. The gate reads
// the vehicle's CURRENT persisted status at accept time (via the snapshot
// reader), independent of the ride row's lifecycle state.
func TestRideRequestHandler_Accept_VehicleAvailabilityGate(t *testing.T) {
	const owner = rideOtherUsr

	tests := []struct {
		name          string
		vehicleStatus string
		wantStatus    int
		wantErrCode   wserrors.ErrorCode
		wantAccepted  bool
	}{
		{name: "in_service blocks accept", vehicleStatus: "in_service", wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeVehicleUnavailable},
		{name: "offline blocks accept", vehicleStatus: "offline", wantStatus: http.StatusConflict, wantErrCode: wserrors.ErrCodeVehicleUnavailable},
		{name: "parked allows accept", vehicleStatus: "parked", wantStatus: http.StatusOK, wantAccepted: true},
		{name: "driving allows accept", vehicleStatus: "driving", wantStatus: http.StatusOK, wantAccepted: true},
		{name: "charging allows accept", vehicleStatus: "charging", wantStatus: http.StatusOK, wantAccepted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
			pub := &fakeRidePublisher{}
			row := fixtureSnapshotRow(owner)
			row.Status = tt.vehicleStatus
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, pub, owner)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if !tt.wantAccepted {
				assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
				// A blocked accept must not transition the ride nor publish
				// anything (no status-change frame, no dispatch seam).
				if store.updateCalls != 0 {
					t.Errorf("blocked accept must not call UpdateStatusFrom, got %d", store.updateCalls)
				}
				if len(pub.events) != 0 {
					t.Errorf("blocked accept must publish no events, got %d", len(pub.events))
				}
				return
			}

			if store.updatedState != rideStatusAccepted {
				t.Errorf("allowed accept: UpdateStatus arg got %q want %q", store.updatedState, rideStatusAccepted)
			}
			// A successful accept emits ride_status_changed + the dispatch seam.
			if len(pub.events) != 2 {
				t.Fatalf("allowed accept: events got %d want 2", len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_Accept_ScheduledExemptFromAvailabilityGate seals the
// MYR-313 narrowing: a ride with scheduledFor set is EXEMPT from the
// dispatch-capability gate, so an owner can confirm a reservation for Saturday
// while the car is in service today (the client's report — the accept came back
// "Vehicle is in service and can't be dispatched"). The instant counterpart in
// the same table still 409s, so the gate is narrowed, not removed.
//
// This matches the two guards the gate is the analogue of (the per-rider
// `ride_active` index and the per-vehicle one-active-ride index, both partial
// on `scheduled_for IS NULL`) and what rest-api.md §4.1.1 already documents.
func TestRideRequestHandler_Accept_ScheduledExemptFromAvailabilityGate(t *testing.T) {
	const owner = rideOtherUsr
	scheduled := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)

	tests := []struct {
		name          string
		vehicleStatus string
		scheduled     bool
		wantStatus    int
		wantAccepted  bool
	}{
		{name: "scheduled accept on in_service vehicle", vehicleStatus: "in_service", scheduled: true, wantStatus: http.StatusOK, wantAccepted: true},
		{name: "scheduled accept on offline vehicle", vehicleStatus: "offline", scheduled: true, wantStatus: http.StatusOK, wantAccepted: true},
		{name: "scheduled accept on parked vehicle", vehicleStatus: "parked", scheduled: true, wantStatus: http.StatusOK, wantAccepted: true},
		{name: "instant accept on in_service vehicle still blocked", vehicleStatus: "in_service", wantStatus: http.StatusConflict},
		{name: "instant accept on offline vehicle still blocked", vehicleStatus: "offline", wantStatus: http.StatusConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ride := fixtureRideData(owner, rideStatusRequested)
			if tt.scheduled {
				ride.ScheduledFor = &scheduled
			}
			store := &fakeRideStore{getRec: ride}
			pub := &fakeRidePublisher{}
			row := fixtureSnapshotRow(owner)
			row.Status = tt.vehicleStatus
			reader := &stubVehicleSnapshotReader{row: row}
			h := newRideHandler(store, reader, pub, owner)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !tt.wantAccepted {
				assertErrEnvelope(t, rec, tt.wantStatus, wserrors.ErrCodeVehicleUnavailable)
				if store.updateCalls != 0 {
					t.Errorf("blocked accept must not call UpdateStatusFrom, got %d", store.updateCalls)
				}
				return
			}
			if store.updatedState != rideStatusAccepted {
				t.Errorf("scheduled accept: UpdateStatus arg got %q want %q", store.updatedState, rideStatusAccepted)
			}
			// MYR-316 amended this: a scheduled accept now DOES read the
			// vehicle, because the service-window bound needs it. What must
			// still hold is that the read's ANSWER never blocks a scheduled
			// accept on availability grounds — asserted by the wantAccepted
			// rows above, where in_service and offline both still yield 200.
			if reader.calls != 1 {
				t.Errorf("scheduled accept reads the vehicle once for the MYR-316 bound, got %d lookups", reader.calls)
			}
		})
	}
}

// TestRideRequestHandler_Accept_ScheduledVehicleLookupFailsClosed is the
// MYR-372 inversion of what this test used to assert.
//
// It previously pinned FAIL-OPEN: since MYR-316 a scheduled accept reads the
// vehicle for the service-window bound, and an unreadable vehicle left the
// reservation UNBOUNDED rather than refused, on the argument that refusing it
// would resurrect the MYR-313 stranding. The two failures are not alike. MYR-313
// was a PERMANENT 409 — the car was in service, so every retry that day refused.
// A lookup failure is a TRANSIENT 500: the owner retries and it succeeds.
// Granting a reservation that may sit squarely inside a service visit, silently,
// to avoid one retry is the worse trade — sharply so with external beta riders
// on the other end.
//
// So the vehicle read now fails CLOSED on every path: unknown blocks, and the
// ride is NOT accepted.
//
// The error here is a TRANSIENT store failure, which is what earns the
// retryable 500. A vehicle that is merely GONE is a different, permanent thing
// and answers 409 — see TestRideRequestAccept_MissingVehicleIsPermanentNot-
// Retryable in vehicle_availability_test.go.
func TestRideRequestHandler_Accept_ScheduledVehicleLookupFailsClosed(t *testing.T) {
	const owner = rideOtherUsr
	scheduled := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)
	ride := fixtureRideData(owner, rideStatusRequested)
	ride.ScheduledFor = &scheduled
	store := &fakeRideStore{getRec: ride}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{err: errors.New("db down")}, pub, owner)
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
	if store.updateCalls != 0 {
		t.Errorf("an unreadable vehicle must not accept the reservation, got %d transition calls", store.updateCalls)
	}
	if len(pub.events) != 0 {
		t.Errorf("no dispatch seam may fire on a refused accept, got %d events", len(pub.events))
	}
}

// TestRideRequestHandler_Accept_VehicleLookupFailsClosed asserts the gate fails
// CLOSED: if the vehicle's status cannot be read at accept time the server
// returns 500 and does NOT accept the ride (we never dispatch a ride whose
// vehicle we cannot confirm can serve it).
//
// MYR-372 changed the error this test injects from a not-found to a transient
// store failure. The RULE is unchanged — an unreadable vehicle never accepts —
// but not-found is now separated out as a permanent 409, so the 500 case must
// be exercised with the failure that actually is retryable.
func TestRideRequestHandler_Accept_VehicleLookupFailsClosed(t *testing.T) {
	const owner = rideOtherUsr
	store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{err: errors.New("db down")}, pub, owner)
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
	assertErrEnvelope(t, rec, http.StatusInternalServerError, wserrors.ErrCodeInternalError)
	if store.updateCalls != 0 {
		t.Errorf("fail-closed accept must not transition the ride, got %d update calls", store.updateCalls)
	}
	if len(pub.events) != 0 {
		t.Errorf("fail-closed accept must publish no events, got %d", len(pub.events))
	}
}

// TestRideRequestHandler_Accept_DispatchExactlyOnce seals the double-accept
// (owner double-tap / two devices) semantics: the WINNING guarded write
// publishes the ride.accepted dispatch seam exactly once; a LOSING accept —
// whose pre-check read still saw `requested` but whose guarded write found
// the row already accepted — 409s and publishes nothing. Together: one
// dispatch event per accept, never two, regardless of interleaving.
func TestRideRequestHandler_Accept_DispatchExactlyOnce(t *testing.T) {
	const owner = rideOtherUsr

	t.Run("winner publishes the seam exactly once", func(t *testing.T) {
		store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
		pub := &fakeRidePublisher{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
		rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		dispatches := 0
		for _, ev := range pub.events {
			if _, ok := ev.Payload.(events.RideAcceptedEvent); ok {
				dispatches++
			}
		}
		if dispatches != 1 {
			t.Fatalf("dispatch seam published %d times, want exactly 1", dispatches)
		}
		if store.updateCalls != 1 || len(store.updatedFrom) != 1 || store.updatedFrom[0] != rideStatusRequested {
			t.Errorf("guarded write: calls=%d from=%v", store.updateCalls, store.updatedFrom)
		}
	})

	t.Run("loser 409s and publishes nothing", func(t *testing.T) {
		// Pre-check read sees `requested`; by write time a concurrent accept
		// already landed (fake: guard evaluates against updated.Status).
		store := &fakeRideStore{
			getRec:  fixtureRideData(owner, rideStatusRequested),
			updated: fixtureRideData(owner, rideStatusAccepted),
		}
		pub := &fakeRidePublisher{}
		h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
		rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
		assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
		if len(pub.events) != 0 {
			t.Fatalf("losing accept must publish no events (no double dispatch), got %d", len(pub.events))
		}
	})
}

// TestRideRequestHandler_Accept_VehicleAlreadyOnRide seals the per-vehicle
// one-active-ride guard's HTTP surface (MYR-266): when the guarded
// requested->accepted write is rejected because the target car is already on
// another active ride (store.ErrVehicleRideActive → telemetry.ErrVehicleRide
// Active), the accept returns 409 vehicle_unavailable and publishes NOTHING —
// no status-change frame and no dispatch seam (a second nav push to a car
// already serving a ride is exactly what the guard prevents). The vehicle
// snapshot is dispatchable, so the MYR-277 gate passes and the request reaches
// the guarded write where the per-vehicle index arbitrates.
func TestRideRequestHandler_Accept_VehicleAlreadyOnRide(t *testing.T) {
	const owner = rideOtherUsr
	store := &fakeRideStore{
		getRec:    fixtureRideData(owner, rideStatusRequested),
		updateErr: ErrVehicleRideActive,
	}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
	if store.updateCalls != 1 {
		t.Errorf("expected the guarded write to be attempted once, got %d", store.updateCalls)
	}
	if len(pub.events) != 0 {
		t.Fatalf("vehicle-busy accept must publish no events (no double dispatch), got %d", len(pub.events))
	}
}

// TestRideRequestHandler_Accept_DispatchSeamPayload asserts the ride.accepted
// event carries everything MYR-176 needs for the Tesla navigation_request:
// both places (with address flattening), scheduledFor, and the accepted_at
// stamp.
//
// It also asserts what the event must NOT carry. The record below still has a
// booked-for passenger name and phone — legacy rows do — and MYR-447 removed
// those two fields from the event because nothing ever read them. The
// reflection check below is the regression guard: re-adding a passenger field
// to events.RideAcceptedEvent puts P1 PII back on the internal bus, and doing
// it by accident is exactly how it got there the first time.
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
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
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
	// MYR-447: the passenger contact stays on the RECORD and must never reach
	// the bus. Asserted structurally rather than by value, because the honest
	// claim is "this type has no passenger field at all", not "this one instance
	// happens to be empty".
	evType := reflect.TypeOf(ev)
	for i := range evType.NumField() {
		if strings.Contains(evType.Field(i).Name, "Passenger") {
			t.Errorf("RideAcceptedEvent.%s reintroduces booked-for passenger PII on the internal bus (MYR-447)",
				evType.Field(i).Name)
		}
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
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, &fakeRidePublisher{}, rideOtherUsr)
	rec := doRequest(t, rideMux(h), http.MethodGet, "/api/ride-requests/incoming", "", rideAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the incoming feed (200), got %d: %s", rec.Code, rec.Body.String())
	}
	if store.ownerCall.id != rideOtherUsr {
		t.Errorf("expected ListByOwnerPage to be hit, got ownerCall=%+v", store.ownerCall)
	}
}

// --- MYR-369: the accept-time ride-capability backstop ---------------------

// grantReaderFor builds a share reader granting one capability set to riderID
// on the ride fixture's vehicle, and nothing to anybody else.
func grantReaderFor(riderID string, grant auth.ShareGrant) *stubShareReader {
	return &stubShareReader{
		grants: map[string]auth.ShareGrant{riderID + "|" + rideVehicle: grant},
	}
}

// newRideHandlerWithShares mounts the owner surface WITH sharing wired, which
// is what production looks like once MYR-184 shipped.
func newRideHandlerWithShares(store RideRequestStore, caller string, shares VehicleShareReader) *RideRequestHandler { //nolint:unparam // the accepting actor is spelled at every call site on purpose: these are authorization tests and an implicit caller would hide which actor each case exercises
	return NewRideRequestHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: acceptBackstopVehicle()},
		store,
		&fakeRidePublisher{},
		discardLogger(),
		WithRideShareReader(shares),
	)
}

// acceptBackstopVehicle is an available vehicle owned by rideOtherUsr — the
// owner in the ride fixture — so the rider is genuinely a non-owner and the
// backstop is actually reached.
func acceptBackstopVehicle() VehicleSnapshotRow {
	row := availableSnapshotRow()
	row.ID = rideVehicle
	row.UserID = rideOtherUsr
	row.Status = "parked"
	return row
}

// TestAccept_RiderRideCapabilityBackstop is the window MYR-369 opened and this
// gate closes: an owner suspends a grant, or switches allowRides off, while a
// request from that rider sits unanswered in their queue. Without the backstop,
// tapping Accept would dispatch the car to somebody the owner had just decided
// may not have it.
func TestAccept_RiderRideCapabilityBackstop(t *testing.T) {
	const owner = rideOtherUsr

	tests := []struct {
		name       string
		grant      auth.ShareGrant
		wantStatus int
	}{
		{
			name:       "a live ride grant still accepts",
			grant:      auth.ShareGrant{AllowRides: true},
			wantStatus: http.StatusOK,
		},
		{
			// The owner patched allowRides off after the request landed.
			name:       "a grant whose ride capability was withdrawn is refused",
			grant:      auth.ShareGrant{},
			wantStatus: http.StatusForbidden,
		},
		{
			// Suspension gates everything — the stored ride flag does not
			// survive it, which is why the gate reads GrantsRides and not
			// the bare field.
			name:       "a SUSPENDED grant is refused even carrying allowRides",
			grant:      auth.ShareGrant{AllowRides: true, Suspended: true},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
			h := newRideHandlerWithShares(store, owner, grantReaderFor(rideUserID, tt.grant))

			rec := doRequest(t, rideMux(h), http.MethodPost,
				"/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}

	t.Run("a revoked grant (no row at all) is refused", func(t *testing.T) {
		store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
		h := newRideHandlerWithShares(store, owner, &stubShareReader{})

		rec := doRequest(t, rideMux(h), http.MethodPost,
			"/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — a revoked rider must not be accepted", rec.Code)
		}
	})

	t.Run("an unreadable grant is 500, not a silent accept", func(t *testing.T) {
		// FAIL-CLOSED, unlike the MYR-313 fail-open on an unreadable
		// VEHICLE: dispatching a car to somebody who may have just been
		// suspended is not recoverable the way a retried accept is.
		store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
		h := newRideHandlerWithShares(store, owner, &stubShareReader{err: errors.New("db down")})

		rec := doRequest(t, rideMux(h), http.MethodPost,
			"/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", rec.Code)
		}
	})

	t.Run("the OWNER accepting their own ride never consults a grant", func(t *testing.T) {
		// The common case must not pay for the backstop, and an owner has
		// no grant to find.
		selfRide := fixtureRideData(owner, rideStatusRequested)
		selfRide.RiderID = owner
		store := &fakeRideStore{getRec: selfRide}
		reader := &stubShareReader{}
		h := newRideHandlerWithShares(store, owner, reader)

		rec := doRequest(t, rideMux(h), http.MethodPost,
			"/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200. body=%s", rec.Code, rec.Body.String())
		}
		if reader.calls != 0 {
			t.Errorf("the share store was consulted %d times for an owner's own ride", reader.calls)
		}
	})
}
