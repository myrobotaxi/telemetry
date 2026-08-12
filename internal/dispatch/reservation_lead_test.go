package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-535 — the dispatch lead. The client's decision, verbatim: "computed
// travel time … the server estimates drive time from the car's current
// position to the pickup and dispatches that early (+ ~3 min buffer, floored
// ~5 min, capped 30 min)", plus the field evidence that motivated it — a
// reservation scheduled 21:00:00Z was dispatched 21:00:08Z, i.e. the car was
// told to LEAVE at the instant it was booked to ARRIVE.

// leadTestConfig is the shipped policy, spelled out rather than defaulted so
// a change to the defaults fails a test instead of silently reshaping these.
func leadTestConfig() ReservationConfig {
	return ReservationConfig{
		LeadBuffer: 3 * time.Minute,
		LeadFloor:  5 * time.Minute,
		LeadCap:    30 * time.Minute,
	}
}

func TestTravelEstimate_ClosedFormParity(t *testing.T) {
	// SF Ferry Building → Golden Gate Park (~5.3 mi great-circle). The closed
	// form is great-circle × 1.3 ÷ 24 mph — the platform's ONE estimate
	// (TripEstimate parity on the client). ~5.3 × 1.3 / 24 × 60 ≈ 17.3 min.
	got := travelEstimate(37.7955, -122.3937, 37.7694, -122.4862)
	if got < 15*time.Minute || got > 20*time.Minute {
		t.Errorf("travelEstimate = %v, want ≈17 min for a ~5.3 mi hop", got)
	}
	// Zero distance is zero time.
	if got := travelEstimate(37.7955, -122.3937, 37.7955, -122.3937); got != 0 {
		t.Errorf("same-point estimate = %v, want 0", got)
	}
}

func TestClampLead(t *testing.T) {
	cfg := leadTestConfig()
	tests := []struct {
		name   string
		travel time.Duration
		want   time.Duration
	}{
		// A car already at the pickup still leaves LeadFloor early.
		{name: "floor", travel: 0, want: 5 * time.Minute},
		// Just under the floor after the buffer.
		{name: "buffered under floor", travel: time.Minute, want: 5 * time.Minute},
		// Ordinary case: estimate + buffer.
		{name: "estimate plus buffer", travel: 10 * time.Minute, want: 13 * time.Minute},
		// A cross-country estimate is capped, matching the SELECT horizon —
		// no lead may exceed what the sweep can even see.
		{name: "cap", travel: 4 * time.Hour, want: 30 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampLead(tt.travel, cfg); got != tt.want {
				t.Errorf("clampLead(%v) = %v, want %v", tt.travel, got, tt.want)
			}
		})
	}
}

func TestHasFix(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	if hasFix(nil, nil) {
		t.Error("nil pair must not be a fix")
	}
	if hasFix(f(37.7), nil) {
		t.Error("half pair must not be a fix")
	}
	// (0, 0) is the wire's NO-FIX SENTINEL (§2.3) and storage does not
	// collapse it — an estimate from Null Island would put every real pickup
	// at the LeadCap.
	if hasFix(f(0), f(0)) {
		t.Error("the (0,0) sentinel must not be a fix")
	}
	if !hasFix(f(37.7), f(-122.4)) {
		t.Error("a real pair is a fix")
	}
	// A genuine 0 on ONE axis is a real place (the equator, the prime
	// meridian) — only the exact (0,0) pair is the sentinel.
	if !hasFix(f(0), f(-122.4)) {
		t.Error("a single zero axis is still a fix")
	}
}

// --- the worker gate -------------------------------------------------------

// earlyReservation returns the test reservation re-scheduled AHEAD of the
// sweeper clock by the given margin, as the widened SELECT horizon now
// surfaces it (MYR-535).
func earlyReservation(ahead time.Duration) DueReservation {
	r := testReservation()
	r.ScheduledFor = testSweepNow.Add(ahead)
	return r
}

// TestSweep_EarlyCandidateWaitsUntilItsLeaveTime: a candidate 25 minutes
// ahead with no known car position gets the FLOOR lead (5 min), so it waits —
// nothing probed, nothing claimed, nothing pushed — and the same candidate 4
// minutes ahead dispatches.
func TestSweep_EarlyCandidateWaitsUntilItsLeaveTime(t *testing.T) {
	tests := []struct {
		name  string
		ahead time.Duration
		want  sweepDecision
	}{
		{name: "before the leave-time waits", ahead: 25 * time.Minute, want: decisionWaiting},
		{name: "inside the floor lead dispatches", ahead: 4 * time.Minute, want: decisionDispatched},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			resStore := &fakeReservationStore{busy: map[string]bool{}}
			s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

			r := earlyReservation(tt.ahead)
			got := s.handleDue(context.Background(), &r)
			if got != tt.want {
				t.Fatalf("decision = %v, want %v", got, tt.want)
			}
			if tt.want == decisionWaiting {
				if resStore.posCount() != 1 {
					t.Errorf("position reads = %d, want exactly 1 for a waiting candidate", resStore.posCount())
				}
				if resStore.busyCnt != 0 {
					t.Errorf("busy probes = %d, want 0 — a waiting candidate burns nothing else", resStore.busyCnt)
				}
				if claims, _, _ := latch.snapshot(); claims != 0 {
					t.Errorf("claims = %d, want 0 — waiting must never latch", claims)
				}
			}
		})
	}
}

// TestSweep_LeadFollowsTheCarsPosition: with a fix in hand the leave-time is
// the travel estimate plus the buffer. A car ~24 road-minutes out (with the
// 1.3 route factor) plus the 3-minute buffer is a ~27-minute lead, so a
// pickup 25 minutes ahead is ALREADY past its leave-time and dispatches —
// while the same pickup with the car parked next door waits on the floor.
func TestSweep_LeadFollowsTheCarsPosition(t *testing.T) {
	pickup := testReservation().Pickup
	tests := []struct {
		name string
		car  [2]float64
		want sweepDecision
	}{
		// ~7.4 mi great-circle from the pickup ≈ 24 road-min + 3 buffer.
		{name: "far car leaves now", car: [2]float64{37.69, -122.44}, want: decisionDispatched},
		// Effectively at the pickup: floor lead (5 min) < 25 min ahead.
		{name: "near car waits", car: [2]float64{pickup.Latitude + 0.001, pickup.Longitude}, want: decisionWaiting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			resStore := &fakeReservationStore{busy: map[string]bool{}}
			resStore.positions = map[string][2]float64{testReservation().VehicleID: tt.car}
			s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

			r := earlyReservation(25 * time.Minute)
			if got := s.handleDue(context.Background(), &r); got != tt.want {
				t.Fatalf("decision = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSweep_PositionDegradationsLandOnTheFloor: an unreadable position row
// and the (0,0) no-fix sentinel both fall to the FLOOR lead rather than
// holding — the lead only tunes WHEN a car that should go actually leaves,
// so the asymmetry with the other probes' hold-on-error posture is the point.
func TestSweep_PositionDegradationsLandOnTheFloor(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeReservationStore)
	}{
		{name: "read error", setup: func(f *fakeReservationStore) { f.posErr = errors.New("db down") }},
		{name: "no-fix sentinel", setup: func(f *fakeReservationStore) {
			f.positions = map[string][2]float64{testReservation().VehicleID: {0, 0}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			resStore := &fakeReservationStore{busy: map[string]bool{}}
			tt.setup(resStore)
			s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

			// 4 minutes ahead sits INSIDE the floor lead → dispatches.
			r := earlyReservation(4 * time.Minute)
			if got := s.handleDue(context.Background(), &r); got != decisionDispatched {
				t.Fatalf("decision = %v, want dispatched on the floor lead", got)
			}
			// 6 minutes ahead sits OUTSIDE it → waits.
			r = earlyReservation(6 * time.Minute)
			if got := s.handleDue(context.Background(), &r); got != decisionWaiting {
				t.Fatalf("decision = %v, want waiting outside the floor lead", got)
			}
		})
	}
}

// TestSweep_DueCandidateNeverReadsThePosition: at or past scheduledFor the
// lead is moot — the answer is "now" whatever the estimate says — so the
// position read is skipped entirely. A position-read outage can therefore
// delay a dispatch by at most the lead it would have granted, never hold one
// past its pickup instant. (testSweepNow is 10s past testScheduledFor, so the
// pre-existing testReservation IS the at-or-past case.)
func TestSweep_DueCandidateNeverReadsThePosition(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}, posErr: errors.New("must never be asked")}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	r := testReservation()
	if got := s.handleDue(context.Background(), &r); got != decisionDispatched {
		t.Fatalf("decision = %v, want dispatched", got)
	}
	if resStore.posCount() != 0 {
		t.Errorf("position reads = %d, want 0 for an at-or-past-scheduledFor candidate", resStore.posCount())
	}
}

// TestSweep_EarlyDispatchStillPublishesDue: a dispatch that fires on its lead
// publishes ride.due with DueAt BEFORE ScheduledFor — the seam consumers
// (both parties' pushes) fire at the leave-time, which is the whole point.
func TestSweep_EarlyDispatchStillPublishesDue(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}}
	bus := &fakeBus{}
	s, _ := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	r := earlyReservation(4 * time.Minute)
	if got := s.handleDue(context.Background(), &r); got != decisionDispatched {
		t.Fatalf("decision = %v, want dispatched", got)
	}
	bus.mu.Lock()
	evts := append([]events.Event(nil), bus.published...)
	bus.mu.Unlock()
	if len(evts) != 1 {
		t.Fatalf("published %d events, want 1 ride.due", len(evts))
	}
	due, ok := evts[0].Payload.(events.RideDueEvent)
	if !ok {
		t.Fatalf("payload = %T, want RideDueEvent", evts[0].Payload)
	}
	if !due.DueAt.Before(due.ScheduledFor) {
		t.Errorf("DueAt %v should precede ScheduledFor %v on an early dispatch", due.DueAt, due.ScheduledFor)
	}
}
