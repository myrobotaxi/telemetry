package telemetry

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// serviceEnd is the vehicle's estimated service completion in these tests. The
// boundary cases below are expressed relative to it.
var serviceEnd = time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)

// inServiceRow builds an owned, IN-SERVICE vehicle carrying the given raw
// service-window sources.
func inServiceRow(ownerID string, tesla, owner *time.Time) VehicleSnapshotRow {
	row := fixtureSnapshotRow(ownerID)
	row.Status = serviceStatusInService
	row.ServiceETC = tesla
	row.ServiceExpectedEndAt = owner
	return row
}

func createBodyScheduled(at time.Time) string {
	return fmt.Sprintf(`{
		"vehicleId": %q,
		"pickup":  {"lat": 37.77, "lng": -122.41, "label": "Pickup"},
		"dropoff": {"lat": 37.79, "lng": -122.39, "label": "Dropoff"},
		"scheduledFor": %q
	}`, fixtureSnapshotRowID, at.Format(time.RFC3339))
}

func createBodyInstant() string {
	return fmt.Sprintf(`{
		"vehicleId": %q,
		"pickup":  {"lat": 37.77, "lng": -122.41, "label": "Pickup"},
		"dropoff": {"lat": 37.79, "lng": -122.39, "label": "Dropoff"}
	}`, fixtureSnapshotRowID)
}

// boundCases is the shared truth table for the MYR-316 scheduler bound. Applied
// identically to CREATE and ACCEPT, because the two paths must never disagree
// about whether a reservation is bookable.
type boundCase struct {
	name         string
	tesla        *time.Time
	owner        *time.Time
	status       string
	scheduledFor time.Time
	wantRejected bool
}

func boundCases() []boundCase {
	return []boundCase{
		{
			name:         "before Tesla's estimate is refused",
			tesla:        &serviceEnd,
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(-time.Hour),
			wantRejected: true,
		},
		{
			name:         "after Tesla's estimate is allowed",
			tesla:        &serviceEnd,
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(time.Hour),
		},
		{
			// The estimate is an estimate; refusing the boundary instant would
			// be false precision. Strictly "earlier than".
			name:         "exactly at the estimate is allowed",
			tesla:        &serviceEnd,
			status:       serviceStatusInService,
			scheduledFor: serviceEnd,
		},
		{
			name:         "one second before the estimate is refused",
			tesla:        &serviceEnd,
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(-time.Second),
			wantRejected: true,
		},
		{
			// Precedence: Tesla wins, so an EARLIER owner value does not
			// loosen a later Tesla estimate.
			name:         "Tesla's estimate wins over an earlier owner value",
			tesla:        &serviceEnd,
			owner:        ptrTime(serviceEnd.Add(-4 * time.Hour)),
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(-time.Hour),
			wantRejected: true,
		},
		{
			name:         "owner value binds when Tesla has nothing",
			owner:        &serviceEnd,
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(-time.Hour),
			wantRejected: true,
		},
		{
			// The contract is explicit: missing data must NEVER block a booking.
			name:         "no estimate at all means no bound",
			status:       serviceStatusInService,
			scheduledFor: serviceEnd.Add(-100 * time.Hour),
		},
		{
			// A car not in service carries no window even with populated
			// columns, so there is nothing to bind against.
			name:         "parked vehicle imposes no bound despite populated columns",
			tesla:        &serviceEnd,
			owner:        &serviceEnd,
			status:       "parked",
			scheduledFor: serviceEnd.Add(-time.Hour),
		},
	}
}

// TestRideRequestCreate_ServiceWindowBound applies the truth table to CREATE.
func TestRideRequestCreate_ServiceWindowBound(t *testing.T) {
	for _, tt := range boundCases() {
		t.Run(tt.name, func(t *testing.T) {
			row := inServiceRow(rideUserID, tt.tesla, tt.owner)
			row.Status = tt.status
			store := &fakeRideStore{}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, rideUserID)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests",
				createBodyScheduled(tt.scheduledFor), rideAuthOK)

			if !tt.wantRejected {
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d want 201 (body %s)", rec.Code, rec.Body.String())
				}
				return
			}

			assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
			if store.createCalled {
				t.Error("a refused create still reached the store")
			}
		})
	}
}

// The refusal message NAMES the estimate so the client can explain it without a
// second round trip. The instant is P0 (operational timing, the same tier as
// `status`), so echoing it is safe — contrast the P1 plate and place labels,
// which are never echoed.
func TestRideRequestCreate_ServiceWindowMessageNamesTheEstimate(t *testing.T) {
	row := inServiceRow(rideUserID, &serviceEnd, nil)
	h := newRideHandler(&fakeRideStore{}, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests",
		createBodyScheduled(serviceEnd.Add(-time.Hour)), rideAuthOK)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 (body %s)", rec.Code, rec.Body.String())
	}
	want := serviceEnd.Format(time.RFC3339)
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("message did not name the estimate %q: %s", want, rec.Body.String())
	}
}

// INSTANT rides are untouched by the bound — they have no scheduledFor to
// floor, and MYR-277 already refuses them outright for an in-service car.
func TestRideRequestCreate_InstantUnaffectedByServiceWindow(t *testing.T) {
	row := inServiceRow(rideUserID, &serviceEnd, nil)
	store := &fakeRideStore{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", createBodyInstant(), rideAuthOK)

	// Whatever else happens to an instant ride, it must NOT be the 400
	// invalid_request the service-window bound produces.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("instant create hit the service-window bound: %s", rec.Body.String())
	}
}

// TestRideRequestAccept_ServiceWindowBound applies the same truth table to
// ACCEPT, so the two paths can never diverge.
func TestRideRequestAccept_ServiceWindowBound(t *testing.T) {
	const owner = rideOtherUsr

	for _, tt := range boundCases() {
		t.Run(tt.name, func(t *testing.T) {
			ride := fixtureRideData(owner, rideStatusRequested)
			scheduled := tt.scheduledFor
			ride.ScheduledFor = &scheduled

			row := inServiceRow(owner, tt.tesla, tt.owner)
			row.Status = tt.status

			store := &fakeRideStore{getRec: ride}
			h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, owner)

			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

			if !tt.wantRejected {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d want 200 (body %s)", rec.Code, rec.Body.String())
				}
				if store.updatedState != rideStatusAccepted {
					t.Fatalf("UpdateStatus arg = %q want %q", store.updatedState, rideStatusAccepted)
				}
				return
			}

			assertErrEnvelope(t, rec, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest)
			if store.updateCalls != 0 {
				t.Errorf("a refused accept still transitioned the ride (%d update calls)", store.updateCalls)
			}
		})
	}
}

// An INSTANT accept is bound by MYR-277's 409, never by the MYR-316 400 — the
// two gates must not be confused for one another.
func TestRideRequestAccept_InstantStillHitsAvailabilityGateNotTheBound(t *testing.T) {
	const owner = rideOtherUsr
	ride := fixtureRideData(owner, rideStatusRequested) // no ScheduledFor => instant
	row := inServiceRow(owner, &serviceEnd, nil)

	store := &fakeRideStore{getRec: ride}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: row}, &fakeRidePublisher{}, owner)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)
	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable)
}
