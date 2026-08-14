package arrival

// MYR-563 — the field failure this package was structurally blind to.
//
// THE SEQUENCE (prod ride c563655e…, 2026-08-13): the ride went `enroute` at
// 23:17:43 with no stops, a stop was added MID-LEG at 23:24:39 (landing
// 'current', the dash correctly re-pointed at it), the car arrived and PARKED
// there at ~23:30, and ten minutes later nothing had happened — no
// ride.waypoint_arrived, no leg advance, no re-share of the drop-off.
//
// THE REASON IS THE FRAME SOURCE, NOT THE CANDIDATE SET. A car that parks stops
// streaming: Tesla's fleet telemetry goes quiet within seconds of the shift to
// P, which is precisely the moment this detector exists to notice. (The prod
// row says so exactly: the ride's Drive ended at 23:31:00 — the timestamp of
// the LAST located frame of any kind — and the next drive did not start until
// 23:39:20.) From the shift to P until the car moves again, the only frames are
// MYR-394 REST backfill fixes, and
// internal/telemetry/service_status_vehicle_data_fields.go states what one of
// those carries for a parked car:
//
//	"Speed is skipped when nil (Tesla sends null for a stationary car)"
//	"NULL IS NOT PARK. Tesla answers shift_state=null for a car that is
//	 parked or asleep, so a nil is skipped"
//
// A location, and nothing else. Fix.stopped demands POSITIVE evidence — a
// reported speed, or gear P — so every one of those frames read as "no evidence
// the car is stopped", and each one RESET the dwell. The dwell could therefore
// never complete, not late and not eventually: for as long as the car was
// parked, arrival detection was not slow, it was impossible.

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// feedParkedREST delivers one MYR-394 REST backfill frame for a PARKED car:
// a location and nothing else. This is not a degenerate fixture — it is
// byte-for-byte the field set addDriveStateFields produces from Tesla's
// vehicle_data for a stationary vehicle, and it is the ONLY frame this server
// can receive while a car sits at a waypoint.
func (h *harness) feedParkedREST(sec int, meters float64) {
	lat, lng := offsetNorth(meters)
	h.bus.handler(events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       testVIN,
		CreatedAt: h.clock.Add(time.Duration(sec) * time.Second),
		Fields: map[string]events.TelemetryValue{
			string(telemetry.FieldLocation): {
				LocationVal: &events.Location{Latitude: lat, Longitude: lng},
			},
		},
	}))
}

// waypointsPublished returns the waypoint label of every ride.waypoint_arrived
// on the bus, in order.
func (h *harness) waypointsPublished() []string {
	h.bus.mu.Lock()
	defer h.bus.mu.Unlock()
	out := make([]string, 0, len(h.bus.published))
	for _, e := range h.bus.published {
		if ev, ok := e.Payload.(events.RideWaypointArrivedEvent); ok {
			out = append(out, ev.Waypoint)
		}
	}
	return out
}

// THE MYR-563 REPRODUCTION, end to end through the detector: enroute with the
// drop-off leg already shared, a stop added mid-leg, then the car parks at it
// and only REST frames remain.
//
// The candidate half of this is deliberately included even though it turned out
// to be sound — the set is re-derived from the store on the TTL, so the stop
// added at 23:24 IS the watched waypoint by 23:24:40. What was broken is what
// happens next, and that is what this test would not have caught if it started
// at the stop.
func TestDetectorReachesAStopAddedMidLegFromParkedRESTFrames(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second, CandidateTTL: time.Second})

	// 23:17 — enroute, no stops yet: the ride is watched at its DESTINATION,
	// which the car is nowhere near. It is still streaming, so its frames carry
	// a real speed.
	dest := testCandidate()
	dest.Waypoint = events.WaypointDestination
	dest.TargetLatitude, dest.TargetLongitude = offsetNorth(4000)
	h.store.set([]Candidate{dest}, nil)
	h.feed(testVIN, 0, 2500, 38)

	// 23:24 — the stop is added mid-leg and lands 'current', so the next
	// candidate read names it instead.
	h.store.set([]Candidate{stopCandidate("crs821")}, nil)
	h.clock = h.clock.Add(2 * time.Second)

	// ~23:30 — the car parks at the stop. The stream dies with the shift to P
	// and the ~25s REST poll takes over. Five cycles, two minutes, 12 m from
	// the pin and not moving.
	for _, sec := range []int{60, 85, 110, 135, 160} {
		h.feedParkedREST(sec, 12)
	}

	got := h.waypointsPublished()
	if len(got) != 1 {
		t.Fatalf("published %d waypoint arrivals from a car parked at the stop for two "+
			"minutes, want exactly 1 (MYR-563: the leg never advanced)", len(got))
	}
	if want := events.WaypointStop("crs821"); got[0] != want {
		t.Errorf("waypoint = %q, want %q — the stop added mid-leg", got[0], want)
	}
	if w := h.writer.writes(); len(w) != 0 {
		t.Errorf("status writes = %v, want none: a stop must not touch the lifecycle", w)
	}
}

// THE RECOVERY PROPERTY. A ride ALREADY in the stuck state — stop 'current',
// car parked inside the radius, no further edits by anyone — must start
// detecting on the first telemetry fixes after a deploy.
//
// This is what makes the fix a recovery rather than only a prevention, and it
// holds because nothing about the decision is snapshotted at leg start: the
// candidate set is re-derived from CURRENT ride+stop state on the cache TTL
// (a fresh process holds none at all, so the very first frame reads it), and
// the dwell is measured off frame timestamps, so the two REST cycles it takes
// are two cycles from the deploy — not from the arrival that already happened.
func TestDetectorRecoversARideAlreadyStuckAtItsStop(t *testing.T) {
	// A FRESH detector: no snapshot, no tracks. Exactly the state a restart
	// leaves behind.
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.store.set([]Candidate{stopCandidate("crs821")}, nil)

	// The car has been sitting here for ten minutes already; these are simply
	// the next two poll cycles.
	h.feedParkedREST(0, 9)
	h.feedParkedREST(25, 9)

	got := h.waypointsPublished()
	if len(got) != 1 {
		t.Fatalf("published %d waypoint arrivals, want 1 — a ride already stuck at its "+
			"stop must advance on the next fixes with no further edits", len(got))
	}
	if want := events.WaypointStop("crs821"); got[0] != want {
		t.Errorf("waypoint = %q, want %q", got[0], want)
	}
}

// The other direction, and the reason positional stillness is EVIDENCE rather
// than a relaxation: a car whose REST fixes keep displacing is moving, and
// moving through the radius must still detect nothing. This is the guard that
// stops the fix degenerating into "any located frame near the target counts".
func TestDetectorDoesNotArriveOnRESTFixesThatKeepMoving(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.store.set([]Candidate{stopCandidate("crs821")}, nil)

	// Crawling across a car park inside the radius: 40 m per cycle, which is
	// ~3.6 mph — well above the 1 mph the speed rule calls stopped.
	for i, meters := range []float64{70, 30, 10, 45, 75} {
		h.feedParkedREST(i*25, meters)
	}

	if got := h.waypointsPublished(); len(got) != 0 {
		t.Fatalf("published %v, want none — a car still moving has not arrived", got)
	}
}

// One located frame is not a dwell, whatever else is true of it. A single REST
// fix proves the car was somewhere at an instant; the whole defence against a
// car merely PASSING the waypoint is that it must still be there later.
func TestDetectorNeedsMoreThanOneParkedRESTFix(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.store.set([]Candidate{stopCandidate("crs821")}, nil)

	h.feedParkedREST(0, 9)

	if got := h.waypointsPublished(); len(got) != 0 {
		t.Fatalf("published %v on a single fix, want none", got)
	}
}

// A gap so long that the two fixes say nothing about the interval between them
// is not a dwell either. Two identical positions ten minutes apart are
// consistent with a car that left and came back, so the inference is refused
// and the run restarts from the later fix.
func TestDetectorRefusesToInferStillnessAcrossALongGap(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.store.set([]Candidate{stopCandidate("crs821")}, nil)

	h.feedParkedREST(0, 9)
	h.feedParkedREST(900, 9) // fifteen minutes later

	if got := h.waypointsPublished(); len(got) != 0 {
		t.Fatalf("published %v across a 15-minute gap, want none", got)
	}

	// …but the ordinary cadence resuming from there does arrive, so a gap
	// delays detection rather than poisoning it.
	h.feedParkedREST(925, 9)
	if got := h.waypointsPublished(); len(got) != 1 {
		t.Fatalf("published %v after the cadence resumed, want 1", got)
	}
}

// A trip edit re-points the CAR within seconds, so the detector must not spend
// the rest of a TTL measuring against the target the trip no longer has. The
// edit itself performs no read — it marks the snapshot dirty and the next frame
// pays for it — so this also pins that one edit costs one refresh.
func TestDetectorRebuildsTheCandidateSetOnATripEdit(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second, CandidateTTL: time.Hour})
	h.store.set([]Candidate{stopCandidate("crs01")}, nil)

	h.feedParkedREST(0, 9)
	if got := h.store.callCount(); got != 1 {
		t.Fatalf("store reads = %d, want 1 for the first frame", got)
	}

	// The rider edits the trip: the stop the car is driving to is now a
	// different one. Inside the TTL, so nothing but the edit can cause a re-read.
	h.store.set([]Candidate{stopCandidate("crs02")}, nil)
	h.bus.editTrip()
	if got := h.store.callCount(); got != 1 {
		t.Fatalf("store reads = %d immediately after the edit, want 1 — the edit "+
			"must schedule a refresh, not perform one", got)
	}

	h.feedParkedREST(25, 9)
	if got := h.store.callCount(); got != 2 {
		t.Fatalf("store reads = %d after the next frame, want 2", got)
	}

	// And the arrival that follows names the stop the edit installed, not the
	// one the snapshot held.
	h.feedParkedREST(50, 9)
	got := h.waypointsPublished()
	if len(got) != 1 || got[0] != events.WaypointStop("crs02") {
		t.Fatalf("published %v, want one arrival at the edited stop", got)
	}
}
