package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-342 ride-sharing pause, enforced on the two REQUEST-TIME paths: the
// rider-facing create (§7.8 POST /api/ride-requests) and the owner-facing
// accept backstop (§7.8 POST /api/ride-requests/{id}/accept). The third layer,
// the reservation sweeper, is covered in internal/dispatch.
//
// The behaviour these tests pin down that is NOT obvious:
//
//  1. The pause applies to SCHEDULED rides as well as instant ones. That is a
//     deliberate deviation from the MYR-313 exemption, which lets a reservation
//     through for a car that is in service or offline today. The reasoning
//     behind that exemption is that a service visit ENDS, so the car's state
//     today says nothing about the reservation instant. An owner's pause is
//     open-ended: nothing ends it but the owner, so a reservation booked
//     against a paused car has no more reason to become servable than an
//     instant one.
//  2. The gate sits AFTER the access check on both paths, so a caller who has
//     no business with the vehicle learns nothing about its pause state — they
//     get the access denial they would have got anyway.
//  3. The pause is read off the SAME GetByID row that established access /
//     status, not a second lookup. Two facts from one statement cannot
//     disagree.

const rideShareInstantBody = `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"}}`

const rideShareScheduledBody = `{"vehicleId":"` + rideVehicle + `","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"},"scheduledFor":"2026-06-18T16:00:00Z"}`

// pausedSnapshotRow is the fixture vehicle with ride sharing switched OFF.
func pausedSnapshotRow(ownerID string) VehicleSnapshotRow {
	row := fixtureSnapshotRow(ownerID)
	row.RideShareEnabled = false
	return row
}

// --- Layer (a): the create gate ---

// TestRideRequestHandler_Create_PausedVehicle covers both ride shapes. The
// SCHEDULED case is the whole point: it must be refused exactly like the
// instant one, unlike the MYR-313 in-service/offline exemption.
func TestRideRequestHandler_Create_PausedVehicle(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"instant create against a paused vehicle", rideShareInstantBody},
		{"scheduled create against a paused vehicle (NO MYR-313 exemption)", rideShareScheduledBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: pausedSnapshotRow(rideUserID)}, pub, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", tc.body, rideAuthOK)

			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
			if store.createCalled {
				t.Error("Create must not run for a paused vehicle")
			}
			if len(pub.events) != 0 {
				t.Errorf("no event may be published for a refused create, got %d", len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_Create_PausedMessage pins the rider-facing copy. It is
// asserted separately from the code because the two travel to different places:
// the code drives client branching, the message is rendered verbatim.
func TestRideRequestHandler_Create_PausedMessage(t *testing.T) {
	h := newRideHandler(
		&fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)},
		&stubVehicleSnapshotReader{row: pausedSnapshotRow(rideUserID)},
		&fakeRidePublisher{},
		rideUserID,
	)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Message != rideSharePausedMessage {
		t.Errorf("message: got %q want %q", env.Error.Message, rideSharePausedMessage)
	}
}

// TestRideRequestHandler_Create_EnabledVehiclePasses is the counter-assertion:
// the gate must not refuse the ordinary case, in either ride shape.
func TestRideRequestHandler_Create_EnabledVehiclePasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"instant", rideShareInstantBody},
		{"scheduled", rideShareScheduledBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", tc.body, rideAuthOK)

			if rec.Code != http.StatusCreated {
				t.Fatalf("enabled vehicle must accept the create: got %d. body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRideRequestHandler_Create_PauseIsCheckedAfterAccess proves the ORDER. A
// caller with no access to a PAUSED vehicle must get the access denial, not the
// pause 409 — otherwise the gate becomes an oracle telling strangers about the
// state of somebody else's car.
func TestRideRequestHandler_Create_PauseIsCheckedAfterAccess(t *testing.T) {
	store := &fakeRideStore{created: fixtureRideData(rideOtherUsr, rideStatusRequested)}
	// The vehicle belongs to somebody else AND is paused. The caller
	// (rideUserID) holds no share, so h.shares is nil — the fail-closed default.
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: pausedSnapshotRow(rideOtherUsr)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	if rec.Code == http.StatusConflict {
		t.Fatalf("a caller without vehicle access must not learn the pause state: got 409. body=%s", rec.Body.String())
	}
	if store.createCalled {
		t.Error("Create must not run for a caller without access")
	}
}

// TestRideRequestHandler_Create_PauseCostsNoExtraLookup pins the atomicity
// story: the pause is read off the SAME GetByID row that resolved the owner, so
// there is exactly ONE vehicle read on the create path and the two facts cannot
// disagree with each other.
func TestRideRequestHandler_Create_PauseCostsNoExtraLookup(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: pausedSnapshotRow(rideUserID)}
	h := newRideHandler(&fakeRideStore{}, reader, &fakeRidePublisher{}, rideUserID)

	doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	if reader.calls != 1 {
		t.Errorf("vehicle reads on the create path: got %d want 1 — the pause must come off the ownership row", reader.calls)
	}
}

// --- Layer (b): the accept backstop ---

// TestRideRequestHandler_Accept_PausedVehicle covers both ride shapes on the
// owner accept. The SCHEDULED case again carries the deviation: MYR-313 exempts
// reservations from the in-service/offline availability gate, and the pause is
// deliberately NOT part of that exemption.
func TestRideRequestHandler_Accept_PausedVehicle(t *testing.T) {
	scheduled := fixtureRideData(rideUserID, rideStatusRequested)
	when := scheduled.CreatedAt.AddDate(0, 0, 3)
	scheduled.ScheduledFor = &when

	tests := []struct {
		name string
		rec  RideRequestData
	}{
		{"instant accept for a paused vehicle", fixtureRideData(rideUserID, rideStatusRequested)},
		{"scheduled accept for a paused vehicle (NO MYR-313 exemption)", scheduled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tc.rec}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: pausedSnapshotRow(rideUserID)}, pub, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
			if store.updateCalls != 0 {
				t.Errorf("the guarded transition must not run for a paused vehicle, got %d calls", store.updateCalls)
			}
			if len(pub.events) != 0 {
				t.Errorf("no dispatch seam may fire on a refused accept, got %d events", len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_Accept_ScheduledPauseKeepsTheFailOpenRead is the
// MYR-313 interaction that is easy to break. A scheduled accept whose vehicle
// read FAILS still proceeds (fail-open) — that is the shape MYR-313 installed
// to stop a lookup hiccup stranding an owner, and adding the pause check must
// not quietly convert it to fail-closed. An unknown pause state does not block.
func TestRideRequestHandler_Accept_ScheduledPauseKeepsTheFailOpenRead(t *testing.T) {
	scheduled := fixtureRideData(rideUserID, rideStatusRequested)
	when := scheduled.CreatedAt.AddDate(0, 0, 3)
	scheduled.ScheduledFor = &when

	store := &fakeRideStore{getRec: scheduled}
	h := newRideHandler(store, &stubVehicleSnapshotReader{err: errors.New("db down")}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("a scheduled accept must still fail OPEN on a vehicle-read failure: got %d. body=%s", rec.Code, rec.Body.String())
	}
}

// TestRideRequestHandler_Accept_EnabledVehiclePasses is the counter-assertion
// on the accept path.
func TestRideRequestHandler_Accept_EnabledVehiclePasses(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusRequested)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("enabled vehicle must accept: got %d. body=%s", rec.Code, rec.Body.String())
	}
}

// TestRideRequestHandler_Decline_IsNeverGatedByThePause mirrors the standing
// MYR-277 rule: an owner may ALWAYS decline. Gating decline on the pause would
// trap requests on a car its owner has withdrawn — the exact opposite of what
// the pause is for.
func TestRideRequestHandler_Decline_IsNeverGatedByThePause(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusRequested)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: pausedSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/decline", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("decline must never be gated by the pause: got %d. body=%s", rec.Code, rec.Body.String())
	}
}
