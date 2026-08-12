package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Tests for the MYR-360 narrowing of the §7.8 transition matrix: an owner may
// decline an ACCEPTED ride, but ONLY when it is SCHEDULED. Before MYR-360 the
// `accepted` row's `declined` cell was an unconditional 409 — an owner who
// paused ride sharing with a confirmed reservation outstanding had no way to
// spare the rider, and the sweeper's hold-then-expire backstop only told them
// 30 minutes AFTER their pickup time.
//
// An accepted INSTANT ride keeps returning 409: a car already dispatched to a
// rider standing on a sidewalk is a different situation and out of scope.

// scheduledRide returns a ride in `status` carrying a reservation instant,
// owned by the authenticated caller in these tests (rideOtherUsr).
func scheduledRide(status string, at time.Time) RideRequestData {
	rec := fixtureRideData(rideOtherUsr, status)
	utc := at.UTC()
	rec.ScheduledFor = &utc
	return rec
}

// TestRideRequestHandler_DeclineAcceptedScheduled walks the whole
// (status × scheduled?) matrix for the decline endpoint.
func TestRideRequestHandler_DeclineAcceptedScheduled(t *testing.T) {
	const owner = rideOtherUsr
	future := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)
	past := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		rec  RideRequestData
		// wantFrom is the allowed-from set the guarded write must be given.
		// nil means the write must not be attempted at all.
		wantFrom   []string
		wantStatus int
	}{
		{
			name:       "accepted scheduled reservation is declinable (MYR-360)",
			rec:        scheduledRide(rideStatusAccepted, future),
			wantFrom:   []string{rideStatusRequested, rideStatusAccepted},
			wantStatus: http.StatusOK,
		},
		{
			name: "accepted reservation already past its scheduledFor is still declinable",
			// Deliberate: the ride is still `accepted`, the owner is still the
			// decider, and an explicit decline is strictly better for the rider
			// than the silent `reservation_expired` at the lateness ceiling.
			rec:        scheduledRide(rideStatusAccepted, past),
			wantFrom:   []string{rideStatusRequested, rideStatusAccepted},
			wantStatus: http.StatusOK,
		},
		{
			name:       "accepted INSTANT ride still conflicts",
			rec:        fixtureRideData(owner, rideStatusAccepted),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "requested instant ride is unchanged",
			rec:        fixtureRideData(owner, rideStatusRequested),
			wantFrom:   []string{rideStatusRequested},
			wantStatus: http.StatusOK,
		},
		{
			name:       "requested scheduled ride is unchanged",
			rec:        scheduledRide(rideStatusRequested, future),
			wantFrom:   []string{rideStatusRequested, rideStatusAccepted},
			wantStatus: http.StatusOK,
		},
		{
			name:       "scheduled ride already arrived conflicts",
			rec:        scheduledRide(rideStatusArrived, future),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "scheduled ride enroute conflicts",
			rec:        scheduledRide(rideStatusEnroute, future),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "already declined conflicts",
			rec:        scheduledRide(rideStatusDeclined, future),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "completed conflicts",
			rec:        scheduledRide(rideStatusCompleted, future),
			wantStatus: http.StatusConflict,
		},
		{
			name:       "cancelled conflicts",
			rec:        scheduledRide(rideStatusCancelled, future),
			wantStatus: http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeRideStore{getRec: tt.rec}
			pub := &fakeRidePublisher{}
			h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/decline", "", rideAuthOK)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d want %d. body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus != http.StatusOK {
				assertErrEnvelope(t, rec, tt.wantStatus, wserrors.ErrCodeConflict)
				if st.updateCalls != 0 {
					t.Errorf("rejected decline must not attempt the guarded write, got %d", st.updateCalls)
				}
				if len(pub.events) != 0 {
					t.Errorf("rejected decline must publish nothing, got %d events", len(pub.events))
				}
				return
			}

			if st.updatedState != rideStatusDeclined {
				t.Errorf("transition target: got %q want %q", st.updatedState, rideStatusDeclined)
			}
			// The race discipline: the guarded UpdateStatusFrom write is what
			// decides under concurrency, and its allowed-from set must widen
			// ONLY for the scheduled case.
			if !sameStatusSet(st.updatedFrom, tt.wantFrom) {
				t.Errorf("guarded allowed-from set: got %v want %v", st.updatedFrom, tt.wantFrom)
			}

			// The standard rider lifecycle push rides ride.status.changed.
			if len(pub.events) != 1 {
				t.Fatalf("events: got %d want 1", len(pub.events))
			}
			ev, ok := pub.events[0].Payload.(events.RideStatusChangedEvent)
			if !ok {
				t.Fatalf("expected RideStatusChangedEvent, got %T", pub.events[0].Payload)
			}
			if ev.Status != rideStatusDeclined {
				t.Errorf("event status: got %q want %q", ev.Status, rideStatusDeclined)
			}
			if ev.RiderID != rideUserID || ev.OwnerID != owner {
				t.Errorf("event parties: rider=%q owner=%q", ev.RiderID, ev.OwnerID)
			}
			// The frame unicasts to BOTH parties (ws.Broadcaster sends to
			// [RiderID, OwnerID]); carrying both ids is what makes that possible.
			if ev.VehicleID != rideVehicle {
				t.Errorf("event vehicle: got %q", ev.VehicleID)
			}
		})
	}
}

// TestRideRequestHandler_DeclineRemainsOwnerOnly proves MYR-360 widened only
// the LIFECYCLE legality — the authorization model is untouched.
func TestRideRequestHandler_DeclineRemainsOwnerOnly(t *testing.T) {
	future := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)

	tests := []struct {
		name        string
		caller      string
		wantStatus  int
		wantErrCode wserrors.ErrorCode
	}{
		{name: "rider cannot decline their own reservation", caller: rideUserID, wantStatus: http.StatusForbidden, wantErrCode: wserrors.ErrCodePermissionDenied},
		{name: "non-party gets 404, not 403", caller: "clstranger00000000000", wantStatus: http.StatusNotFound, wantErrCode: wserrors.ErrCodeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeRideStore{getRec: scheduledRide(rideStatusAccepted, future)}
			pub := &fakeRidePublisher{}
			h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, tt.caller)
			rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/decline", "", rideAuthOK)
			assertErrEnvelope(t, rec, tt.wantStatus, tt.wantErrCode)
			if st.updateCalls != 0 || len(pub.events) != 0 {
				t.Errorf("refused decline must not write or publish: updates=%d events=%d", st.updateCalls, len(pub.events))
			}
		})
	}
}

// TestRideRequestHandler_AcceptUnaffectedByDeclineWidening pins that ACCEPT is
// untouched: an already-accepted scheduled ride still 409s on accept.
func TestRideRequestHandler_AcceptUnaffectedByDeclineWidening(t *testing.T) {
	const owner = rideOtherUsr
	future := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)

	st := &fakeRideStore{getRec: scheduledRide(rideStatusAccepted, future)}
	pub := &fakeRidePublisher{}
	h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
	rec := doRequest(t, rideMux(h), http.MethodPost, "/api/ride-requests/"+rideID+"/accept", "", rideAuthOK)

	assertErrEnvelope(t, rec, http.StatusConflict, wserrors.ErrCodeConflict)
	if st.updateCalls != 0 {
		t.Errorf("accept from accepted must not reach the guarded write, got %d", st.updateCalls)
	}
}

// TestRideRequestHandler_DeclineRace seals the concurrency discipline: two
// simultaneous declines of the SAME accepted reservation resolve in the store's
// guarded write — exactly one 200, exactly one 409, exactly one published
// status change. The widened allowed-from set must not become a second way to
// double-decline.
func TestRideRequestHandler_DeclineRace(t *testing.T) {
	const owner = rideOtherUsr
	future := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)

	st := newRacingDeclineStore(scheduledRide(rideStatusAccepted, future))
	pub := &syncRidePublisher{}
	h := newRideHandler(st, &stubVehicleSnapshotReader{row: availableSnapshotRow()}, pub, owner)
	mux := rideMux(h)

	const racers = 2
	codes := make([]int, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := doRequest(t, mux, http.MethodPost, "/api/ride-requests/"+rideID+"/decline", "", rideAuthOK)
			codes[i] = rec.Code
		}()
	}
	close(start)
	wg.Wait()

	wins, losses := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			losses++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("racing declines: %d wins / %d conflicts, want exactly 1 / 1 (codes=%v)", wins, losses, codes)
	}
	if n := pub.count(); n != 1 {
		t.Errorf("published status changes: got %d want exactly 1", n)
	}
}

// --- Test doubles ---

// racingDeclineStore models the store's guarded UpdateStatusFrom faithfully
// enough to arbitrate a race: the read is unsynchronised (both racers see
// `accepted`), and the write is a mutex-protected compare-and-set against the
// allowed-from set, exactly as the single `WHERE status = ANY($3)` UPDATE is.
type racingDeclineStore struct {
	mu  sync.Mutex
	rec RideRequestData
}

func newRacingDeclineStore(rec RideRequestData) *racingDeclineStore {
	return &racingDeclineStore{rec: rec}
}

// UpdateStatusFromCancelled reuses the same compare-and-set arbitration with
// the target fixed to "cancelled" (MYR-522); the stamp is carried onto the
// winning row exactly as the single UPDATE writes cancelled_by.
func (s *racingDeclineStore) UpdateStatusFromCancelled(ctx context.Context, id string, from []string, by string) (RideRequestData, error) {
	rec, err := s.UpdateStatusFrom(ctx, id, from, "cancelled")
	if err != nil {
		return RideRequestData{}, err
	}
	stamp := by
	rec.CancelledBy = &stamp
	return rec, nil
}

func (s *racingDeclineStore) GetByID(context.Context, string) (RideRequestData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, nil
}

func (s *racingDeclineStore) UpdateStatusFrom(_ context.Context, _ string, from []string, to string) (RideRequestData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, allowed := range from {
		if allowed == s.rec.Status {
			s.rec.Status = to
			return s.rec, nil
		}
	}
	return RideRequestData{}, fmt.Errorf("update ride request status: %w", ErrRideStatusConflict)
}

// UpdateStatusFromDispatched is unused by the decline race (decline is not the
// pickup path) but keeps the store surface complete; it delegates so the fake
// can never diverge on the shared guard semantics.
func (s *racingDeclineStore) UpdateStatusFromDispatched(ctx context.Context, id string, from []string, to string) (RideRequestData, error) {
	return s.UpdateStatusFrom(ctx, id, from, to)
}

// UpdateStatusFromUnconflicted is likewise unused by the decline race (decline
// is never window-gated — an owner may always decline) and delegates for the
// same reason.
func (s *racingDeclineStore) UpdateStatusFromUnconflicted(ctx context.Context, id string, from []string, to string) (RideRequestData, error) {
	return s.UpdateStatusFrom(ctx, id, from, to)
}

func (s *racingDeclineStore) Create(context.Context, RideRequestCreateInput) (RideRequestData, error) {
	return RideRequestData{}, errors.New("not used")
}

func (s *racingDeclineStore) GetActiveInstantByRider(context.Context, string) (RideRequestData, error) {
	return RideRequestData{}, errors.New("not used")
}

func (s *racingDeclineStore) ListByRiderPage(context.Context, string, RideRequestListCursor, int) (RideRequestListPage, error) {
	return RideRequestListPage{}, errors.New("not used")
}

func (s *racingDeclineStore) ListByOwnerPage(context.Context, string, *string, RideRequestListCursor, int) (RideRequestListPage, error) {
	return RideRequestListPage{}, errors.New("not used")
}

func (s *racingDeclineStore) ListUpcomingByOwnerVehiclePage(context.Context, string, string, RideRequestUpcomingCursor, int) (RideRequestListPage, error) {
	return RideRequestListPage{}, errors.New("not used")
}

func (s *racingDeclineStore) ListActiveByOwnerVehiclePage(context.Context, string, string, RideRequestListCursor, int) (RideRequestListPage, error) {
	return RideRequestListPage{}, errors.New("not used")
}

func (s *racingDeclineStore) ListBookedWindows(context.Context, string, string, time.Time, time.Time) ([]BookedWindowData, error) {
	return nil, errors.New("not used")
}

// syncRidePublisher counts publishes across goroutines.
type syncRidePublisher struct {
	mu sync.Mutex
	n  int
}

func (p *syncRidePublisher) Publish(context.Context, events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return nil
}

func (p *syncRidePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

// sameStatusSet compares two allowed-from sets order-insensitively.
func sameStatusSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, s := range got {
		seen[s]++
	}
	for _, s := range want {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}
