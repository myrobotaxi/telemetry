package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// HTTP-surface coverage for the MYR-383 per-vehicle ride-window refusal at both
// landing sites. The GATE itself (the predicate, the window boundary, the
// advisory-lock race) is pinned against real Postgres in
// internal/store/ride_request_conflict_test.go; what is pinned HERE is the
// contract a client sees: the code, the sub-code, the message, and the fact
// that a refusal moves and publishes nothing.

const windowConflictScheduledBody = `{"vehicleId":"` + rideVehicle +
	`","pickup":{"lat":37.79,"lng":-122.39,"label":"Home"},` +
	`"dropoff":{"lat":37.77,"lng":-122.39,"label":"Caltrain"},` +
	`"scheduledFor":"2026-08-01T12:20:00Z"}`

// windowConflictInstant is the instant the fixtures report as already taken.
var windowConflictInstant = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// decodeErrEnvelope decodes the standard error envelope including its subCode,
// which the shared assertion helper does not expose.
func decodeErrEnvelope(t *testing.T, rec *httptest.ResponseRecorder) wserrors.ErrorEnvelopeBody {
	t.Helper()
	var body struct {
		Error wserrors.ErrorEnvelopeBody `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v. body=%s", err, rec.Body.String())
	}
	return body.Error
}

// assertWindowConflictEnvelope pins the whole contract of the refusal: 409,
// the reused `vehicle_unavailable` capability code, and the `time_conflict`
// sub-code a client MUST branch on instead of reading the message.
func assertWindowConflictEnvelope(t *testing.T, rec *httptest.ResponseRecorder) wserrors.ErrorEnvelopeBody {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409. body=%s", rec.Code, rec.Body.String())
	}
	env := decodeErrEnvelope(t, rec)
	if env.Code != wserrors.ErrCodeVehicleUnavailable {
		t.Errorf("error.code: got %q want %q", env.Code, wserrors.ErrCodeVehicleUnavailable)
	}
	if env.SubCode == nil {
		t.Fatal("error.subCode is null; a client cannot tell a taken SLOT from an unavailable CAR")
	}
	if got, want := *env.SubCode, string(wserrors.SubCodeTimeConflict); got != want {
		t.Errorf("error.subCode: got %q want %q", got, want)
	}
	return env
}

// TestRideWindowConflict_Create is the rider-facing half: a reservation whose
// window the car has already promised away is refused immediately, with the
// conflicting INSTANT named so the client can send the rider back to the picker
// knowing which slot is gone.
func TestRideWindowConflict_Create(t *testing.T) {
	store := &fakeRideStore{
		createErr: fmt.Errorf("create ride request: %w",
			&RideWindowConflictError{ConflictAt: &windowConflictInstant}),
	}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, pub, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", windowConflictScheduledBody, rideAuthOK)

	env := assertWindowConflictEnvelope(t, rec)
	if !strings.Contains(env.Message, "2026-08-01T12:00:00Z") {
		t.Errorf("message must name the conflicting instant, got %q", env.Message)
	}
	if len(pub.events) != 0 {
		t.Errorf("a refused create must publish nothing, got %d events", len(pub.events))
	}
}

// TestRideWindowConflict_MessageDisclosesOnlyTheWindow is the P1 discipline:
// the refusal may say WHEN the car is spoken for and nothing else. The
// conflicting ride's identity, its rider and its places belong to the other
// party — a booking probe must not become a way to read a stranger's calendar.
func TestRideWindowConflict_MessageDisclosesOnlyTheWindow(t *testing.T) {
	store := &fakeRideStore{
		createErr: fmt.Errorf("create ride request: %w",
			&RideWindowConflictError{ConflictAt: &windowConflictInstant}),
	}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(rideUserID)}, &fakeRidePublisher{}, rideUserID)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests", windowConflictScheduledBody, rideAuthOK)
	body := rec.Body.String()

	// Nothing that identifies the OTHER ride or its rider may appear, and the
	// envelope must carry no sibling object either (contrast `ride_active`,
	// which deliberately does).
	for _, forbidden := range []string{
		"riderId", "requesterName", "pickup", "dropoff", "lat", "lng",
		"activeRideRequest", "conflictingRide",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("refusal body leaks %q — the other party's ride is not the caller's to see.\n%s", forbidden, body)
		}
	}
}

// TestRideWindowConflict_Messages pins the three sentences, each of which must
// say only what is TRUE. Calling a merely-requested slot "booked" would
// misinform the very rider this gate exists to inform.
func TestRideWindowConflict_Messages(t *testing.T) {
	tests := []struct {
		name     string
		conflict RideWindowConflictError
		want     string
	}{
		{
			name:     "committed reservation names the instant",
			conflict: RideWindowConflictError{ConflictAt: &windowConflictInstant},
			want:     "Vehicle is already booked for a ride at 2026-08-01T12:00:00Z",
		},
		{
			name:     "pending claim is NOT called booked",
			conflict: RideWindowConflictError{ConflictAt: &windowConflictInstant, Pending: true},
			want:     "Vehicle already has a ride request for 2026-08-01T12:00:00Z",
		},
		{
			name:     "an active instant ride has no instant to name",
			conflict: RideWindowConflictError{},
			want:     "Vehicle is already on a ride and can't also be booked for that time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowConflictMessage(&tt.conflict); got != tt.want {
				t.Errorf("message:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// TestRideWindowConflict_Accept is the owner-facing backstop: the accept runs
// through the BOOKING-LOCKED write, and a reservation whose window is taken is
// refused with the same body the create path emits — no transition, no
// status-change frame, no dispatch seam.
func TestRideWindowConflict_Accept(t *testing.T) {
	const owner = rideOtherUsr
	ride := fixtureRideData(owner, rideStatusRequested)
	scheduled := windowConflictInstant.Add(20 * time.Minute)
	ride.ScheduledFor = &scheduled

	store := &fakeRideStore{getRec: ride, windowConflictAt: &windowConflictInstant}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(owner)}, pub, owner)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	env := assertWindowConflictEnvelope(t, rec)
	if !strings.Contains(env.Message, "2026-08-01T12:00:00Z") {
		t.Errorf("message must name the conflicting instant, got %q", env.Message)
	}
	if store.windowGuardedCalls != 1 {
		t.Errorf("accept must use the booking-locked write (UpdateStatusFromUnconflicted), calls=%d", store.windowGuardedCalls)
	}
	if len(pub.events) != 0 {
		t.Errorf("a refused accept must publish nothing — neither the status frame nor the dispatch seam; got %d", len(pub.events))
	}
}

// TestRideWindowConflict_AcceptUsesTheLockedWriteEvenWhenItSucceeds: the gate
// is not a branch the happy path skips. Every accept goes through the
// booking-locked write — that is what makes an accept racing another accept for
// the same car serialize — and a free window transitions exactly as before.
func TestRideWindowConflict_AcceptUsesTheLockedWriteEvenWhenItSucceeds(t *testing.T) {
	const owner = rideOtherUsr
	ride := fixtureRideData(owner, rideStatusRequested)
	scheduled := windowConflictInstant
	ride.ScheduledFor = &scheduled

	store := &fakeRideStore{getRec: ride}
	pub := &fakeRidePublisher{}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(owner)}, pub, owner)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200. body=%s", rec.Code, rec.Body.String())
	}
	if store.windowGuardedCalls != 1 {
		t.Errorf("accept must always use the booking-locked write, calls=%d", store.windowGuardedCalls)
	}
	if store.dispatchGuardedCalls != 0 {
		t.Errorf("accept must NOT use the pickup dormancy write, calls=%d", store.dispatchGuardedCalls)
	}
	if len(pub.events) != 2 {
		t.Fatalf("a successful accept publishes the status frame + the dispatch seam, got %d", len(pub.events))
	}
}

// TestRideWindowConflict_AcceptLeavesInstantRidesAlone: v1a does not gate an
// INSTANT accept on a near-term reservation. The store's probe short-circuits
// on a nil scheduledFor, so an instant accept behaves exactly as it did before
// this gate existed — pinned here so narrowing that boundary later is a
// deliberate, visible change.
func TestRideWindowConflict_AcceptLeavesInstantRidesAlone(t *testing.T) {
	const owner = rideOtherUsr
	store := &fakeRideStore{
		getRec:           fixtureRideData(owner, rideStatusRequested), // no scheduledFor
		windowConflictAt: &windowConflictInstant,                      // would refuse a reservation
	}
	h := newRideHandler(store, &stubVehicleSnapshotReader{row: fixtureSnapshotRow(owner)}, &fakeRidePublisher{}, owner)

	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	if rec.Code != http.StatusOK {
		t.Fatalf("an INSTANT accept is not window-gated in v1a: got %d. body=%s", rec.Code, rec.Body.String())
	}
}
