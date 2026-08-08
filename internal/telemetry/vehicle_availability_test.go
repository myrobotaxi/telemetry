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

// TestVehicleAvailability is the shared enumeration's truth table: which
// blocker each status carries, and which arm the sweeper acts on.
func TestVehicleAvailability(t *testing.T) {
	tests := []struct {
		status          string
		wantBlocker     vehicleBlocker
		wantSweeperHold bool
	}{
		{status: "parked", wantBlocker: blockerNone},
		{status: "driving", wantBlocker: blockerNone},
		{status: "charging", wantBlocker: blockerNone},
		{status: serviceStatusInService, wantBlocker: blockerInService, wantSweeperHold: true},
		// Blocks ACCEPT but must NOT hold the sweeper — see the dedicated
		// test below for why.
		{status: vehicleStatusOffline, wantBlocker: blockerOffline},
		// An unrecognised status is treated as dispatchable, matching the
		// pre-MYR-372 switch exactly: the blocked set is enumerated, never
		// inferred, so a status this build has never heard of does not
		// silently ground a fleet.
		{status: "some_future_status", wantBlocker: blockerNone},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			blocker, refusal := vehicleAvailability(tt.status)

			if blocker != tt.wantBlocker {
				t.Fatalf("vehicleAvailability(%q) blocker = %v, want %v",
					tt.status, blocker, tt.wantBlocker)
			}
			if got := VehicleStatusIsInService(tt.status); got != tt.wantSweeperHold {
				t.Errorf("VehicleStatusIsInService(%q) = %v, want %v — this is the arm "+
					"the reservation sweeper holds on", tt.status, got, tt.wantSweeperHold)
			}
			// Exhaustive by construction: a blocked status always carries a
			// message for the accept gate, and an unblocked one never does.
			if blocker == blockerNone && refusal != "" {
				t.Errorf("an unblocked status carries a refusal: %q", refusal)
			}
			if blocker != blockerNone && refusal == "" {
				t.Errorf("status %q is blocked with no message for the rider", tt.status)
			}
		})
	}
}

// TestVehicleAvailability_OfflineIsNotASweeperHold is a REGRESSION GUARD, and
// the asymmetry it pins is the whole reason the two readers act on different
// arms. Do not "simplify" it away.
//
// `offline` is the "Vehicle" schema default (NOT NULL DEFAULT 'offline'). The
// owned-vehicle provisioning INSERT never sets the column, and nothing writes
// the value back for a car that has never STREAMED: every status writer
// requires a live stream event, the MYR-320 periodic re-poll selects
// `in_service` rows only, and the §7.15 refresh writes everything except
// status. So a key-paired car that has not yet streamed telemetry — every new
// owner, for their first days — sits at `offline` with nothing scheduled to
// correct it.
//
// MYR-454 added a status writer (the telemetry fold heals `offline` on the
// first frame carrying gear/speed) but did NOT weaken this guard: it made the
// permanently-`offline` population exactly the never-streamed cars above.
//
// If the sweeper held on that, it would hold every 30 seconds for 30 minutes
// and then expire the reservation, every time, forever — while suppressing the
// one thing that would have fixed it, since internal/commands classifies
// offline/asleep alike as OutcomeAsleep and runs a wake ladder. `offline` is
// therefore left to the dispatcher's existing arbitration.
//
// ACCEPT still refuses it (pre-existing MYR-277 semantics, a different question
// asked with a human waiting), which is what the blocker assertion pins.
func TestVehicleAvailability_OfflineIsNotASweeperHold(t *testing.T) {
	if VehicleStatusIsInService(vehicleStatusOffline) {
		t.Fatal("the sweeper must NOT hold a reservation for an `offline` car: nothing in " +
			"this server ever clears that status, so the hold is self-perpetuating and " +
			"guarantees reservation_expired for every not-yet-streaming vehicle")
	}
	if blocker, _ := vehicleAvailability(vehicleStatusOffline); blocker != blockerOffline {
		t.Error("`offline` must still be a blocker for the instant-accept gate (MYR-277)")
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

// TestRideRequestAccept_MissingVehicleIsPermanentNotRetryable separates the two
// kinds of read failure the fail-closed rule collapsed together.
//
// A transient store error is a retryable `500`. A vehicle that no longer exists
// is PERMANENT — retrying forever will not bring it back — so it must not wear
// the same code. It takes `409 vehicle_unavailable`, the capability code this
// gate already uses, rather than a `404`, whose documented meaning on this
// endpoint is "unknown ride / non-party" and which would tell an owner their
// own request had vanished.
func TestRideRequestAccept_MissingVehicleIsPermanentNotRetryable(t *testing.T) {
	tests := []struct {
		name       string
		readErr    error
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name:       "vehicle deleted is a permanent capability refusal",
			readErr:    fmtNotFound(),
			wantStatus: http.StatusConflict,
			wantCode:   wserrors.ErrCodeVehicleUnavailable,
		},
		{
			name:       "a store blip stays retryable",
			readErr:    errors.New("db down"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const owner = rideOtherUsr
			store := &fakeRideStore{getRec: fixtureRideData(owner, rideStatusRequested)}
			h := newRideHandler(store, &stubVehicleSnapshotReader{err: tt.readErr}, &fakeRidePublisher{}, owner)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			assertErrEnvelope(t, rec, tt.wantStatus, tt.wantCode)
			if store.updateCalls != 0 {
				t.Errorf("no transition may run on an unreadable vehicle, got %d calls", store.updateCalls)
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
