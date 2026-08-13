package arrival

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-539 — the detector watches more than the pickup. A ride under way is
// measured against the stop it is DRIVING TO, and reaching one is reported
// without touching the ride's lifecycle status.

// stopCandidate is the ride mid-trip, watching its current stop.
func stopCandidate(stopID string) Candidate {
	c := testCandidate()
	c.Waypoint = events.WaypointStop(stopID)
	return c
}

// park drives the car in and holds it still past the dwell window.
func (h *harness) park(secs ...int) {
	for _, s := range secs {
		h.feed(testVIN, s, 10, 0)
	}
}

// Reaching a STOP publishes the waypoint seam and writes NO status: a stop's
// advance belongs to the leg-advance consumer, and the ride is still enroute.
func TestDetectorReportsAStopWithoutWritingStatus(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.store.set([]Candidate{stopCandidate("crs01")}, nil)

	h.park(0, 10, 20, 30)

	if got := h.writer.writes(); len(got) != 0 {
		t.Fatalf("status writes = %v, want none — a stop must not advance the ride's lifecycle", got)
	}
	got := h.bus.topics()
	if len(got) != 1 || got[0] != events.TopicRideWaypointArrived {
		t.Fatalf("published %v, want exactly the waypoint seam", got)
	}
	ev, ok := h.bus.published[0].Payload.(events.RideWaypointArrivedEvent)
	if !ok {
		t.Fatalf("payload = %T", h.bus.published[0].Payload)
	}
	if ev.Waypoint != events.WaypointStop("crs01") {
		t.Errorf("waypoint = %q, want the stop label carrying its id", ev.Waypoint)
	}
	if stopID, isStop := events.StopIDFromWaypoint(ev.Waypoint); !isStop || stopID != "crs01" {
		t.Errorf("the label must round-trip to the stop id, got (%q, %v)", stopID, isStop)
	}
}

// Reaching the DESTINATION publishes the moment and completes NOTHING: ending a
// ride has consequences a wrong guess cannot undo, so dropped-off stays the
// owner's tap and this event exists only for MYR-542's flash.
func TestDetectorReportsTheDestinationWithoutCompletingTheRide(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	final := testCandidate()
	final.Waypoint = events.WaypointDestination
	h.store.set([]Candidate{final}, nil)

	h.park(0, 10, 20, 30)

	if got := h.writer.writes(); len(got) != 0 {
		t.Fatalf("status writes = %v, want none at the destination", got)
	}
	got := h.bus.topics()
	if len(got) != 1 || got[0] != events.TopicRideWaypointArrived {
		t.Fatalf("published %v, want exactly the waypoint seam", got)
	}
}

// One ride reaches SEVERAL places in sequence. The dwell latch is keyed by
// (ride, waypoint), so the stop after the first is detected too — a latch keyed
// by ride alone would have silenced every leg but one.
func TestDetectorDetectsTheNextStopAfterTheListMovesOn(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second, CandidateTTL: time.Second})
	h.store.set([]Candidate{stopCandidate("crs01")}, nil)

	h.park(0, 10, 20, 30)
	if n := len(h.bus.topics()); n != 1 {
		t.Fatalf("published %d events for the first stop, want 1", n)
	}

	// The leg advanced: the ride's CURRENT stop is now the next one, and the
	// snapshot refreshes past its TTL.
	h.store.set([]Candidate{stopCandidate("crs02")}, nil)
	h.clock = h.clock.Add(2 * time.Second)
	h.park(60, 70, 80, 90)

	got := h.bus.topics()
	if len(got) != 2 {
		t.Fatalf("published %v, want one event per stop reached", got)
	}
	second, ok := h.bus.published[1].Payload.(events.RideWaypointArrivedEvent)
	if !ok || second.Waypoint != events.WaypointStop("crs02") {
		t.Errorf("second event = %+v, want the NEXT stop", h.bus.published[1].Payload)
	}
}

// The pickup is unchanged: it still writes the guarded transition and publishes
// the pair. MYR-538's asymmetry survives multi-stop trips intact.
func TestDetectorStillAdvancesThePickup(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})

	h.park(0, 10, 20, 30)

	if got := h.writer.writes(); len(got) != 1 {
		t.Fatalf("status writes = %v, want exactly one for the pickup", got)
	}
	want := []events.Topic{events.TopicRideStatusChanged, events.TopicRideWaypointArrived}
	got := h.bus.topics()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published %v, want %v", got, want)
	}
}
