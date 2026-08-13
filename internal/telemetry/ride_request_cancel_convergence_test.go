package telemetry

import (
	"net/http"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-548 — "Owner canceled ride but it's still active for riders"
// (prod 2026-08-13 13:31Z, build 202608130735, server v275).
//
// WHAT THE TRACE FOUND, and what these tests hold in place.
//
// The owner-cancel ARM is intact and was never the defect: serveOwnerCancel
// reaches the same guarded stamped write every cancel uses, the winning write
// publishes one `ride.status.changed` with status `cancelled` and
// `cancelledBy: owner`, the WS broadcaster unicasts the frame to the rider, and
// the push notifier's rider copy fires (internal/push/notifier_owner_cancel_test.go).
// Nothing in PR #401's member fan-out or MYR-537's riderCancelled arm shadows
// it — the two audiences answer to different copy functions and the member
// fan-out reuses the RIDER's own delivery envelope, so a suppressed rider copy
// is the only thing that could take the members with it, and the rider's copy
// is not suppressed.
//
// WHAT WAS BROKEN IS THE FRAME'S REFETCH SIGNAL. `tripVersion` is omitempty on
// the wire and every lifecycle publisher left it at zero, so a cancel of a ride
// that had been edited even once told the client the record was at version 0
// while the client held N. The production row was at trip_version 5. These
// tests pin the whole delivery — the stamp, the status, the audience and the
// version — so the convergence signal cannot silently regress again.

// editedRideFixture is a ride the parties have already edited, in the given
// status, owned by an account distinct from the rider.
func editedRideFixture(status string, tripVersion int) (RideRequestData, string) {
	owner := rideOwnerID + "X"
	rec := fixtureRideData(owner, status)
	rec.TripVersion = tripVersion
	return rec, owner
}

// The owner's cancel publishes ONE status event carrying everything the rider's
// two surfaces converge on: the terminal status, the initiator (which forks the
// push copy), and the CURRENT trip version (which is the WS frame's refetch
// signal). A transition does not bump the version — it must still carry it.
func TestOwnerCancelPublishesTheConvergenceSignal(t *testing.T) {
	for _, status := range []string{rideStatusAccepted, rideStatusArrived} {
		t.Run(status, func(t *testing.T) {
			rec, owner := editedRideFixture(status, 5)
			store := &fakeRideStore{getRec: rec}
			pub := &fakeRidePublisher{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)

			resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)
			if resp.Code != http.StatusOK {
				t.Fatalf("status: got %d, body=%s", resp.Code, resp.Body.String())
			}

			if len(pub.events) != 1 {
				t.Fatalf("expected exactly 1 event, got %d", len(pub.events))
			}
			ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
			if !ok {
				t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
			}
			if ev.Status != rideStatusCancelled {
				t.Errorf("status: got %q want %q", ev.Status, rideStatusCancelled)
			}
			if ev.CancelledBy == nil || *ev.CancelledBy != rideCancelledByOwner {
				t.Errorf("cancelledBy: %v — the rider's push copy forks on this", ev.CancelledBy)
			}
			// The whole MYR-548 fix. Zero here is the wire value 0, not "no
			// opinion": omitempty drops the key and a client holding 5 reads
			// absence as a version REGRESSION and refetches nothing.
			if ev.TripVersion != 5 {
				t.Errorf("tripVersion: got %d want 5 — the frame's refetch signal must not regress on a cancel", ev.TripVersion)
			}
			// A cancel is a lifecycle transition, never an edit: the TripEdit
			// mark is what makes the push notifier skip the status copy, so an
			// accidental true here would silence the rider's cancellation push.
			if ev.TripEdit {
				t.Error("TripEdit must be false on a cancel — a true here suppresses the rider's push")
			}
			// Both parties are addressable, which is what the WS unicast and the
			// two push audiences are resolved from.
			if ev.RiderID != rideUserID || ev.OwnerID != owner {
				t.Errorf("parties: rider=%q owner=%q", ev.RiderID, ev.OwnerID)
			}
			if ev.PreviousStatus != status {
				t.Errorf("previousStatus: got %q want %q", ev.PreviousStatus, status)
			}
		})
	}
}

// Every lifecycle transition carries the version, not just the cancel. The
// cancel is where a missing signal costs a rider a ride they think is still
// coming, but a stale `completed` or `arrived` surface is the same defect one
// status earlier — and a fix that only covered the reported transition would
// leave the rest of the ladder regressing.
func TestEveryLifecycleTransitionCarriesTheTripVersion(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		path    string
		asRider bool
		want    string
	}{
		{name: "picked-up", from: rideStatusAccepted, path: "picked-up", want: rideStatusArrived},
		{name: "dropped-off", from: rideStatusEnroute, path: "dropped-off", want: rideStatusCompleted},
		{name: "rider cancel", from: rideStatusEnroute, path: "cancel", asRider: true, want: rideStatusCancelled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, owner := editedRideFixture(tt.from, 3)
			store := &fakeRideStore{getRec: rec}
			pub := &fakeRidePublisher{}
			caller := owner
			if tt.asRider {
				caller = rideUserID
			}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, caller)

			resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/"+tt.path, "", rideAuthOK)
			if resp.Code != http.StatusOK {
				t.Fatalf("status: got %d, body=%s", resp.Code, resp.Body.String())
			}
			if len(pub.events) == 0 {
				t.Fatal("expected a status event")
			}
			ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
			if !ok {
				t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
			}
			if ev.Status != tt.want {
				t.Errorf("status: got %q want %q", ev.Status, tt.want)
			}
			if ev.TripVersion != 3 {
				t.Errorf("tripVersion: got %d want 3", ev.TripVersion)
			}
		})
	}
}

// A SELF-RIDE owner (the common case on this platform) cancelling their own
// ride takes the RIDER arm and is stamped `rider`, not `owner` — and this is
// correct, not the bug MYR-548 went looking for.
//
// It is pinned because the production incident was exactly this row shape
// (rider_id == owner_id, cancelled_by 'rider') and the tempting "fix" is to
// stamp `owner` when the caller came from the owner surface. That would be
// wrong twice over: the stamp records WHO CANCELLED, and for one person there
// is one answer; and the rider arm is the WIDER allowed-from set (MYR-537
// admits `enroute`), so routing a self-rider through the owner's window would
// take away their ability to end a ride they are sitting in.
//
// The push silence that follows is likewise correct — statusAlert stays quiet
// on a rider's own cancel, and the owner copy is suppressed when the two ids
// are one person, so nobody's phone buzzes to report their own thumb. What the
// self-rider gets instead is the WS frame and the Live Activity's ending, both
// of which fired in production.
func TestSelfRideCancelStaysTheRidersOwn(t *testing.T) {
	rec := fixtureRideData(rideUserID, rideStatusEnroute) // owner IS the rider
	rec.TripVersion = 5
	store := &fakeRideStore{getRec: rec}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, rideUserID)

	resp := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/cancel", "", rideAuthOK)
	if resp.Code != http.StatusOK {
		t.Fatalf("a self-rider must be able to end a ride they are sitting in: %d %s", resp.Code, resp.Body.String())
	}
	if store.cancelledBy != rideCancelledByRider {
		t.Errorf("cancelled_by: got %q want %q — one person, one answer", store.cancelledBy, rideCancelledByRider)
	}
	// The rider's set, which is the only one that admits `enroute`.
	if len(store.updatedFrom) != 4 {
		t.Errorf("a self-ride cancel must use the rider's wider allowed-from set: %v", store.updatedFrom)
	}
	ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
	if !ok {
		t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
	}
	if ev.Status != rideStatusCancelled || ev.TripVersion != 5 {
		t.Errorf("the self-rider's own surfaces converge off this frame: %+v", ev)
	}
}
