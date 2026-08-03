package telemetry

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// MYR-372: the dispatch-availability predicate, and the fail-CLOSED rule for
// the vehicle read that every scheduling gate rides.

// TestVehicleAvailability is the predicate's own truth table. The reservation
// sweeper reads it through the exported VehicleStatusDispatchable, so a change
// here changes both surfaces at once — which is the entire reason it is one
// function rather than two switches.
func TestVehicleAvailability(t *testing.T) {
	tests := []struct {
		status          string
		wantDispatchabl bool
	}{
		{status: "parked", wantDispatchabl: true},
		{status: "driving", wantDispatchabl: true},
		{status: "charging", wantDispatchabl: true},
		{status: serviceStatusInService},
		{status: vehicleStatusOffline},
		// An unrecognised status is treated as dispatchable, matching the
		// pre-MYR-372 switch exactly: the blocked set is enumerated, never
		// inferred, so a status this build has never heard of does not
		// silently ground a fleet.
		{status: "some_future_status", wantDispatchabl: true},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			dispatchable, refusal := vehicleAvailability(tt.status)

			if dispatchable != tt.wantDispatchabl {
				t.Fatalf("vehicleAvailability(%q) dispatchable = %v, want %v",
					tt.status, dispatchable, tt.wantDispatchabl)
			}
			if got := VehicleStatusDispatchable(tt.status); got != tt.wantDispatchabl {
				t.Errorf("VehicleStatusDispatchable(%q) = %v, want %v — the exported "+
					"predicate the sweeper uses must agree with the accept gate",
					tt.status, got, tt.wantDispatchabl)
			}
			// Exhaustive by construction: a refusable status always carries a
			// message, and a dispatchable one never does.
			if dispatchable && refusal != "" {
				t.Errorf("a dispatchable status carries a refusal: %q", refusal)
			}
			if !dispatchable && refusal == "" {
				t.Errorf("status %q is refused with no message for the rider", tt.status)
			}
		})
	}
}

// TestRideRequestAccept_UnavailableInstantUsesTheSharedRefusal proves the
// accept gate's 409 body still comes from the shared predicate after MYR-372
// moved the strings there — the messages are contract text riders read.
func TestRideRequestAccept_UnavailableInstantUsesTheSharedRefusal(t *testing.T) {
	for _, status := range []string{serviceStatusInService, vehicleStatusOffline} {
		t.Run(status, func(t *testing.T) {
			const owner = rideOtherUsr
			row := fixtureSnapshotRow(owner)
			row.Status = status
			store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}

			h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, owner)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			// Snapshotted before assertErrEnvelope, which drains the buffer.
			body := rec.Body.String()
			assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)

			_, refusal := vehicleAvailability(status)
			if !strings.Contains(body, refusal) {
				t.Fatalf("the 409 body %s does not carry the shared refusal %q", body, refusal)
			}
		})
	}
}

// TestRideRequestCreate_ScheduledVehicleReadFailsClosed is the create-side half
// of the MYR-372 fail-closed rule. Create was already fail-closed — this pins
// it, so the two scheduling paths cannot drift apart again the way they had
// before MYR-372 (accept fail-open, create fail-closed, same question).
func TestRideRequestCreate_ScheduledVehicleReadFailsClosed(t *testing.T) {
	store := &fakeRideStore{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{err: errors.New("db down")},
		&fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests",
		createBodyScheduled(time.Now().UTC().Add(48*time.Hour)), rideAuthOK)

	if rec.Code < 500 {
		t.Fatalf("an unreadable vehicle must fail CLOSED on create: got %d (body %s)",
			rec.Code, rec.Body.String())
	}
	if store.createCalled {
		t.Error("a ride was created against a vehicle the server could not read")
	}
}
