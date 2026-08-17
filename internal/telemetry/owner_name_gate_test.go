package telemetry

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-581 nameless-owner gate on the two REQUEST-TIME paths: the
// rider-facing create (§7.8 POST /api/ride-requests) and the owner-facing accept
// backstop (§7.8 POST /api/ride-requests/{id}/accept). The third layer, the
// reservation sweeper, is covered in internal/dispatch.
//
// Written against ride_share_gate_test.go's shape on purpose — same two layers,
// same fixtures, same 409 code — so the two gates can be read side by side. What
// these tests pin that is NOT obvious:
//
//  1. SELF-RIDES ARE REFUSED TOO. There is no counterparty to name in an owner's
//     own ride, and the gate refuses it anyway: the subject is the CAR's
//     offerability, not the pair of people, and the owner who only ever
//     self-rides is precisely the one who would otherwise never meet the client's
//     name sheet.
//  2. Both ride shapes are refused. Like the pause and unlike the MYR-313
//     in-service exemption — an owner acquiring a name is not something a
//     reservation can wait for the way it waits out a service visit.
//  3. The gate sits AFTER the access check, so a stranger learns nothing about
//     whether somebody else has set their name.
//  4. The flag is a BOOLEAN off the SAME GetByID row. There is no name anywhere
//     on this path to leak, by construction rather than by discipline.

// namelessSnapshotRow is the fixture vehicle whose OWNER has no resolvable name.
// Everything else about the car is ordinary — in particular ride sharing is still
// ENABLED, so a refusal here can only have come from this gate.
func namelessSnapshotRow(ownerID string) VehicleSnapshotRow {
	row := fixtureSnapshotRow(ownerID)
	row.OwnerNamed = false
	return row
}

// --- Layer (a): the create gate ---

func TestRideRequestHandler_Create_NamelessOwner(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"instant create against a nameless owner's car", rideShareInstantBody},
		{"scheduled create against a nameless owner's car (NO MYR-313 exemption)", rideShareScheduledBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)}, pub, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", tc.body, rideAuthOK)

			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
			if store.createCalled {
				t.Error("Create must not run for a car whose owner has no name")
			}
			if len(pub.events) != 0 {
				t.Errorf("no event may be published for a refused create, got %d", len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_Create_NamelessOwnerCoversSelfRides is the arm that is
// easy to get wrong in the "helpful" direction. `rideUserID` owns this car and is
// asking for it themselves — there is genuinely nobody to name — and the refusal
// still stands. Making the gate depend on who is asking would give one vehicle
// two different offerability answers.
func TestRideRequestHandler_Create_NamelessOwnerCoversSelfRides(t *testing.T) {
	store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
	// The caller IS the owner: newRideHandler authenticates as rideUserID and the
	// row's UserID is rideUserID, so the access check passes as owner.
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
	if store.createCalled {
		t.Error("an owner's own ride must be refused too — the gate is about the car, not the pair")
	}
}

// TestRideRequestHandler_Create_NamelessMessageAndNoSubCode pins the wire
// details, both of which are contractual and neither of which is the code.
//
// The MESSAGE names the CONDITION, not the person: a rider must not be told
// "the owner has not set a name", which would disclose a fact about somebody
// else's account to a third party.
//
// The SUBCODE IS NULL, deliberately. This is the fifth carrier of
// `vehicle_unavailable` and a plain member of the family — a condition of the car
// right now, which is the class four of the five share. Only `time_conflict`
// needs a sub-code, because it is a property of the TIME the rider picked and
// sends the client back to the picker rather than telling it to retry later.
func TestRideRequestHandler_Create_NamelessMessageAndNoSubCode(t *testing.T) {
	h := newRideHandler(
		&fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)},
		&stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)},
		&fakeRidePublisher{},
		rideUserID,
	)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Message != ownerNamelessMessage {
		t.Errorf("message: got %q want %q", env.Error.Message, ownerNamelessMessage)
	}
	if env.Error.SubCode != nil {
		t.Errorf("subCode must be null — a nameless owner is a condition of the car, "+
			"the same class as the other three; got %v", *env.Error.SubCode)
	}
}

// TestRideRequestHandler_Create_NamedOwnerPasses is the counter-assertion: the
// gate must not refuse the ordinary case, in either ride shape.
func TestRideRequestHandler_Create_NamedOwnerPasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"instant", rideShareInstantBody},
		{"scheduled", rideShareScheduledBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{created: fixtureRideData(rideUserID, rideStatusRequested)}
			// fixtureSnapshotRow carries OwnerNamed true — a named owner.
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", tc.body, rideAuthOK)

			if rec.Code != http.StatusCreated {
				t.Fatalf("a named owner's car must accept the create: got %d. body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRideRequestHandler_Create_NamelessIsCheckedAfterAccess proves the ORDER. A
// caller with no access to the vehicle must get the access denial, never the
// nameless 409 — otherwise the endpoint answers questions about whether a
// stranger has filled in their profile.
func TestRideRequestHandler_Create_NamelessIsCheckedAfterAccess(t *testing.T) {
	store := &fakeRideStore{created: fixtureRideData(rideOtherUsr, rideStatusRequested)}
	// Somebody else's car, whose owner is nameless. The caller holds no share, so
	// h.shares is nil — the fail-closed default.
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideOtherUsr)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	if rec.Code == http.StatusConflict {
		t.Fatalf("a caller without vehicle access must not learn the owner's name state: got 409. body=%s",
			rec.Body.String())
	}
	if store.createCalled {
		t.Error("Create must not run for a caller without access")
	}
}

// TestRideRequestHandler_Create_NamelessCostsNoExtraLookup pins the atomicity
// story it shares with the pause: the flag rides the SAME GetByID row that
// resolved the owner, so there is exactly ONE vehicle read on the create path and
// access, availability and offerability cannot disagree.
func TestRideRequestHandler_Create_NamelessCostsNoExtraLookup(t *testing.T) {
	reader := &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)}
	h := newRideHandler(&fakeRideStore{}, reader, &fakeRidePublisher{}, rideUserID)

	doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", rideShareInstantBody, rideAuthOK)

	if reader.calls != 1 {
		t.Errorf("vehicle reads on the create path: got %d want 1 — the gate must come off the ownership row",
			reader.calls)
	}
}

// --- Layer (b): the accept backstop ---

// TestRideRequestHandler_Accept_NamelessOwner covers the population this layer
// actually exists for: a request created BEFORE the create gate shipped. There is
// no race to catch — namelessness only resolves forward — so what is being pinned
// is that a pre-deploy request cannot be accepted into a ride nobody can describe.
func TestRideRequestHandler_Accept_NamelessOwner(t *testing.T) {
	scheduled := fixtureRideData(rideUserID, rideStatusRequested)
	when := scheduled.CreatedAt.AddDate(0, 0, 3)
	scheduled.ScheduledFor = &when

	tests := []struct {
		name string
		rec  RideRequestData
	}{
		{"instant accept for a nameless owner", fixtureRideData(rideUserID, rideStatusRequested)},
		{"scheduled accept for a nameless owner (NO MYR-313 exemption)", scheduled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRideStore{getRec: tc.rec}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)}, pub, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
			if store.updateCalls != 0 {
				t.Errorf("the guarded transition must not run for a nameless owner, got %d calls", store.updateCalls)
			}
			if len(pub.events) != 0 {
				t.Errorf("no dispatch seam may fire on a refused accept, got %d events", len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_Accept_NamedOwnerPasses is the counter-assertion on the
// accept path.
func TestRideRequestHandler_Accept_NamedOwnerPasses(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusRequested)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("a named owner must be able to accept: got %d. body=%s", rec.Code, rec.Body.String())
	}
}

// TestRideRequestHandler_Decline_IsNeverGatedByTheOwnerName mirrors the standing
// rule the pause gate keeps: an owner may ALWAYS decline. Gating decline would
// trap requests against a car that cannot be offered — leaving riders waiting on
// an answer the owner is not permitted to give.
func TestRideRequestHandler_Decline_IsNeverGatedByTheOwnerName(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideUserID, rideStatusRequested)}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: namelessSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/decline", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("decline must never be gated by the owner's name: got %d. body=%s", rec.Code, rec.Body.String())
	}
}
