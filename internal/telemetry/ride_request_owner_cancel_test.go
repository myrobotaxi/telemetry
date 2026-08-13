package telemetry

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-522 — the OWNER's cancel (client decision, 2026-08-12: "add ability for
// owner to cancel ride from their end as well").
//
// The semantics under test, verbatim from the decision: an owner may cancel
// any time BEFORE `enroute` — requested rides keep the existing DECLINE,
// accepted/arrived gain cancel — and once the rider is aboard there is no
// owner cancel at all, because ending early is what "Dropped off" already is.
// Both cancel paths stamp WHO cancelled into the same guarded write, so the
// push notifier (and, via the wire, the rider's client) can tell an owner's
// cancel from the rider's own.

// ownerCancelFixture returns a ride whose OWNER is a distinct account from the
// rider, in the given status, with the handler authenticated as that owner.
func ownerCancelFixture(status string) (RideRequestData, string) {
	owner := rideOwnerID + "X"
	return fixtureRideData(owner, status), owner
}

func TestRideRequestHandler_OwnerCancel(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		wantStatus  int
		wantMessage string // substring of the refusal, "" for success
	}{
		{name: "accepted cancels", status: rideStatusAccepted, wantStatus: http.StatusOK},
		{name: "arrived cancels", status: rideStatusArrived, wantStatus: http.StatusOK},
		// A requested ride's owner exit is DECLINE — the refusal names the door.
		{name: "requested points at decline", status: rideStatusRequested, wantStatus: http.StatusConflict, wantMessage: "decline"},
		// A rider aboard ends only through dropped-off — the refusal names it.
		{name: "enroute points at dropped-off", status: rideStatusEnroute, wantStatus: http.StatusConflict, wantMessage: "dropped-off"},
		{name: "completed conflicts", status: rideStatusCompleted, wantStatus: http.StatusConflict},
		{name: "cancelled conflicts", status: rideStatusCancelled, wantStatus: http.StatusConflict},
		{name: "declined conflicts", status: rideStatusDeclined, wantStatus: http.StatusConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, owner := ownerCancelFixture(tt.status)
			store := &fakeRideStore{getRec: rec}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)

			resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", resp.Code, tt.wantStatus, resp.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				// assertErrEnvelope decodes (and drains) the body — read it first.
				body := resp.Body.String()
				assertErrEnvelope(t, resp, tt.wantStatus, wserrors.ErrCodeConflict)
				if tt.wantMessage != "" && !strings.Contains(body, tt.wantMessage) {
					t.Errorf("refusal should name %q, body=%s", tt.wantMessage, body)
				}
				if store.updateCalls != 0 {
					t.Errorf("a pre-check refusal must not attempt the write, got %d calls", store.updateCalls)
				}
				if len(pub.events) != 0 {
					t.Errorf("a refusal must publish nothing, got %d events", len(pub.events))
				}
				return
			}

			// The guarded write carries the OWNER's allowed-from set and stamp.
			if store.cancelledBy != rideCancelledByOwner {
				t.Errorf("cancelled_by stamp: got %q want %q", store.cancelledBy, rideCancelledByOwner)
			}
			if len(store.updatedFrom) != 2 || store.updatedFrom[0] != rideStatusAccepted || store.updatedFrom[1] != rideStatusArrived {
				t.Errorf("owner allowed-from set: %v", store.updatedFrom)
			}

			// The wire carries the stamp so the rider's client can say who.
			var got map[string]any
			_ = json.Unmarshal(resp.Body.Bytes(), &got)
			if got["status"] != rideStatusCancelled {
				t.Errorf("body status: %v", got["status"])
			}
			if got["cancelledBy"] != rideCancelledByOwner {
				t.Errorf("body cancelledBy: %v", got["cancelledBy"])
			}

			// One ride_status_changed through the shared delivery, carrying the
			// initiator for the push notifier's copy fork.
			if len(pub.events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(pub.events))
			}
			ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
			if !ok {
				t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
			}
			if ev.CancelledBy == nil || *ev.CancelledBy != rideCancelledByOwner {
				t.Errorf("event CancelledBy: %v", ev.CancelledBy)
			}
		})
	}
}

// The RIDER's cancel is byte-for-byte what it was, plus the stamp: the write
// carries "rider", the wire says so, and the event lets the notifier stay
// silent about the rider's own action.
func TestRideRequestHandler_RiderCancelStampsRider(t *testing.T) {
	store := &fakeRideStore{getRec: fixtureRideData(rideOwnerID+"X", rideStatusAccepted)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d. body=%s", resp.Code, resp.Body.String())
	}
	if store.cancelledBy != rideCancelledByRider {
		t.Errorf("cancelled_by stamp: got %q want %q", store.cancelledBy, rideCancelledByRider)
	}
	// MYR-537: the rider's allowed-from set is every LIVE status.
	if len(store.updatedFrom) != 4 || store.updatedFrom[0] != rideStatusRequested || store.updatedFrom[3] != rideStatusEnroute {
		t.Errorf("rider allowed-from set unchanged: %v", store.updatedFrom)
	}
	var got map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &got)
	if got["cancelledBy"] != rideCancelledByRider {
		t.Errorf("body cancelledBy: %v", got["cancelledBy"])
	}
	ev := pub.events[0].Payload.(events.RideStatusChangedEvent)
	if ev.CancelledBy == nil || *ev.CancelledBy != rideCancelledByRider {
		t.Errorf("event CancelledBy: %v", ev.CancelledBy)
	}
}

// A SELF-RIDE caller (owner riding their own car — this platform's common
// case, MYR-325) is both parties and takes the RIDER path: their cancel is
// their own, stamps "rider", and therefore wakes nobody's phone.
func TestRideRequestHandler_SelfRideCancelTakesTheRiderPath(t *testing.T) {
	// fixtureRideData's rider is rideUserID; rideOwnerID == rideUserID (the
	// v1 self-ride constants), so this record is one account in both roles.
	store := &fakeRideStore{getRec: fixtureRideData(rideOwnerID, rideStatusAccepted)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d. body=%s", resp.Code, resp.Body.String())
	}
	if store.cancelledBy != rideCancelledByRider {
		t.Errorf("self-ride must stamp rider, got %q", store.cancelledBy)
	}
	// The rider path also means the RIDER's legality window — since MYR-537,
	// every live status, requested first.
	if len(store.updatedFrom) != 4 || store.updatedFrom[0] != rideStatusRequested {
		t.Errorf("self-ride allowed-from set: %v", store.updatedFrom)
	}
}

// The owner's guarded write is what decides under concurrency: a pre-check
// that read `accepted` loses to a rider boarding (`enroute`) at write time,
// and the loser publishes nothing — the same race discipline every other
// transition has (MYR-174/175).
func TestRideRequestHandler_OwnerCancel_GuardWinsRace(t *testing.T) {
	readRec, owner := ownerCancelFixture(rideStatusAccepted) // pre-check passes
	writeRec, _ := ownerCancelFixture(rideStatusEnroute)     // guard sees the boarding
	store := &fakeRideStore{getRec: readRec, updated: writeRec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)

	assertErrEnvelope(t, resp, http.StatusConflict, wserrors.ErrCodeConflict)
	if store.updateCalls != 1 {
		t.Errorf("expected exactly one guarded write attempt, got %d", store.updateCalls)
	}
	if len(pub.events) != 0 {
		t.Errorf("losing transition must publish no events, got %d", len(pub.events))
	}
}
