package drives

// MYR-394 — what the REST position poller does to the drive state machine.
//
// Until this feature, `location` had exactly one source: the live stream, on
// which Tesla applies a 10m MinimumDelta filter IN THE CAR. That filter is load
// bearing in a place nobody wrote down — it is the ONLY reason handleDriving's
// "any location means the car moved" rule was safe, and therefore the only
// reason the MYR-160 stall watchdog could ever fire. A parked, streaming car
// sends gear and speed but no repeated Location, so `moved` stays false and the
// stall condition eventually closes the drive.
//
// REST vehicle_data has no such filter. It answers with the car's cached fix on
// every poll, byte-identical for a car that has not moved, every ~25s for as
// long as the ride row stays open. These tests hold the line on both halves of
// the fix: a REPEATED fix is not movement (so the watchdog still works), a REAL
// displacement still is (so REST-tracked rides still draw a route), and a
// gearless REST frame can no longer resurrect a drive through a stale lastGear.

import (
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// restPollEvent is the frame the MYR-394 poller produces: a position, no gear
// (Tesla answers shift_state=null for a parked car), no speed (null when
// stationary), and — critically — Source=SourceRESTBackfill.
//
//nolint:unparam // lng is fixed by the current fixtures; the pair belongs together.
func restPollEvent(vin string, ts time.Time, lat, lng float64) events.VehicleTelemetryEvent {
	te := telemetryEvent(vin, ts, map[string]events.TelemetryValue{
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: lat, Longitude: lng}},
	})
	te.Source = events.SourceRESTBackfill
	return te
}

// TestDetector_StallWatchdogFiresOnRESTPolledParkedCar is the regression this
// whole delta gate exists for.
//
// The scenario is the ordinary end of a ride: the car finishes its leg and
// parks at the dropoff, Tesla stops streaming on park, and the ride row is
// still open — so 120s later the poller's stream-fresh yield lapses and it
// begins REST-reading position every ~25s. Every one of those reads returns the
// SAME parked coordinate.
//
// Without the gate each identical fix refreshed lastMovementAt, StallTimeout
// could never elapse, and the drive stayed open for a stationary car until the
// 12h MaxDriveDuration backstop — accumulating one identical route point and
// one DriveUpdatedEvent broadcast per cycle the whole way.
func TestDetector_StallWatchdogFiresOnRESTPolledParkedCar(t *testing.T) {
	const vin = "VIN_REST_PARKED"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)
	updatedCh := subscribeTopic(t, h.bus, events.TopicDriveUpdated)

	eventTime := time.Now()
	const parkedLat, parkedLng = 33.0900, -96.8200

	// A real drive, ending at the dropoff coordinate.
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.0800, -96.8200)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	publishTelemetry(t, h.bus,
		telemetryEvent(vin, eventTime.Add(30*time.Second), driveFields("D", 25.0, parkedLat, parkedLng)))
	time.Sleep(50 * time.Millisecond)

	// Drain the DriveUpdatedEvents the two genuinely-moving frames produced;
	// what matters below is that the REST cycles add none.
	drainEvents(updatedCh)

	// Five poll cycles on a car that has not moved an inch. Telemetry stays
	// fresh throughout, so the silence condition cannot be what ends the drive
	// — the stall condition has to.
	for i := 1; i <= 5; i++ {
		h.advance(100 * time.Millisecond)
		publishTelemetry(t, h.bus,
			restPollEvent(vin, eventTime.Add(time.Duration(30+i*25)*time.Second), parkedLat, parkedLng))
		time.Sleep(20 * time.Millisecond)
	}

	if n := drainEvents(updatedCh); n != 0 {
		t.Errorf("DriveUpdatedEvents from repeated identical REST fixes = %d, want 0 —"+
			" a parked car must not broadcast a position change every poll cycle", n)
	}

	h.d.watchdogTick()

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent — REST poll frames kept the stall watchdog from ever firing")
	}
	ended, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if ended.VIN != vin {
		t.Errorf("VIN = %q, want %q", ended.VIN, vin)
	}
	if got := h.metrics.stallEnded.Load(); got != 1 {
		t.Errorf("IncStallEnded count = %d, want 1", got)
	}
	if got := h.metrics.watchdogEnded.Load(); got != 0 {
		t.Errorf("IncWatchdogEnded count = %d, want 0 (telemetry was flowing — silence must not be the cause)", got)
	}
}

// TestDetector_RESTFrameWithRealDisplacementIsMovement is the other half: the
// gate must not blind the detector to a car that IS moving but is not
// streaming. A ride tracked entirely over REST still has to draw a route and
// still has to survive the watchdog.
func TestDetector_RESTFrameWithRealDisplacementIsMovement(t *testing.T) {
	const vin = "VIN_REST_MOVING"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)
	updatedCh := subscribeTopic(t, h.bus, events.TopicDriveUpdated)

	eventTime := time.Now()
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.0900, -96.8200)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	time.Sleep(50 * time.Millisecond)
	drainEvents(updatedCh)

	// Two stall windows' worth of REST cycles, each ~1.1 km further on.
	for i := 1; i <= 4; i++ {
		h.advance(100 * time.Millisecond)
		publishTelemetry(t, h.bus,
			restPollEvent(vin, eventTime.Add(time.Duration(i*25)*time.Second), 33.0900+0.01*float64(i), -96.8200))
		time.Sleep(20 * time.Millisecond)
		h.d.watchdogTick()
	}

	expectNoEvent(t, endedCh, 200*time.Millisecond,
		"a car genuinely moving between REST fixes must not be read as stalled")
	if n := drainEvents(updatedCh); n != 4 {
		t.Errorf("DriveUpdatedEvents from four displaced REST fixes = %d, want 4", n)
	}
	if got := h.metrics.stallEnded.Load(); got != 0 {
		t.Errorf("IncStallEnded count = %d, want 0", got)
	}
}

// TestDetector_SubTenMetreRESTJitterIsNotMovement pins the threshold itself to
// the stream's own MinimumDelta. Tesla's cached REST fix wanders by a few
// metres between reads even for a car sitting still, and treating that as
// motion is exactly how the watchdog got defeated.
func TestDetector_SubTenMetreRESTJitterIsNotMovement(t *testing.T) {
	const vin = "VIN_REST_JITTER"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)

	eventTime := time.Now()
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.0900, -96.8200)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	time.Sleep(50 * time.Millisecond)

	// ~0.000045 degrees of latitude is about 5 metres — inside the 10m filter
	// the stream would have applied on the car.
	for i := 1; i <= 5; i++ {
		h.advance(100 * time.Millisecond)
		publishTelemetry(t, h.bus,
			restPollEvent(vin, eventTime.Add(time.Duration(i*25)*time.Second), 33.0900+0.000045*float64(i%2), -96.8200))
		time.Sleep(20 * time.Millisecond)
	}

	h.d.watchdogTick()
	if _, ok := waitForEvent(endedCh); !ok {
		t.Fatal("timed out waiting for DriveEndedEvent — sub-10m GPS jitter was counted as movement")
	}
	if got := h.metrics.stallEnded.Load(); got != 1 {
		t.Errorf("IncStallEnded count = %d, want 1", got)
	}
}

// TestDetector_ResetToIdleClearsGearSoRESTFrameCannotStartPhantomDrive covers
// the other end of the same failure.
//
// handleIdle's only entry condition is `isDriveGear(state.lastGear)`, and every
// non-gear-driven drive end — watchdog silence, stall, duration cap,
// disconnect — leaves lastGear at "D" precisely BECAUSE no gear=P frame was
// ever seen. A REST poll frame then arrives carrying a location and no gear,
// handleIdle reads the stale "D", and a brand-new drive is minted for a car
// parked at the dropoff — with a real start location this time, so it is not
// even discarded as a micro-drive.
func TestDetector_ResetToIdleClearsGearSoRESTFrameCannotStartPhantomDrive(t *testing.T) {
	const vin = "VIN_PHANTOM"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)

	eventTime := time.Now()
	const parkedLat, parkedLng = 33.0900, -96.8200

	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.0800, -96.8200)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	publishTelemetry(t, h.bus,
		telemetryEvent(vin, eventTime.Add(30*time.Second), driveFields("D", 25.0, parkedLat, parkedLng)))
	time.Sleep(50 * time.Millisecond)

	// End the drive the way a parked car actually ends one: the stall
	// watchdog, with lastGear still reading "D" because no Park frame arrived.
	h.advance(400 * time.Millisecond)
	publishTelemetry(t, h.bus, restPollEvent(vin, eventTime.Add(55*time.Second), parkedLat, parkedLng))
	time.Sleep(20 * time.Millisecond)
	h.d.watchdogTick()
	if _, ok := waitForEvent(endedCh); !ok {
		t.Fatal("timed out waiting for DriveEndedEvent")
	}

	// The ride row is still open, so the poller keeps reading. Each frame
	// carries a location and no gear.
	for i := 1; i <= 3; i++ {
		publishTelemetry(t, h.bus,
			restPollEvent(vin, eventTime.Add(time.Duration(55+i*25)*time.Second), parkedLat, parkedLng))
		time.Sleep(20 * time.Millisecond)
	}

	expectNoEvent(t, startedCh, 200*time.Millisecond,
		"a gearless REST frame must not start a drive on a stale lastGear")

	// The state machine is cleared, not broken: a real gear=D frame still
	// starts a real drive.
	publishTelemetry(t, h.bus,
		telemetryEvent(vin, eventTime.Add(3*time.Minute), driveFields("D", 15.0, parkedLat, parkedLng)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("a genuine gear=D frame must still start a drive after resetToIdle cleared the gear")
	}
}

// drainEvents empties ch and reports how many events it held. Used for
// assertions about how many DriveUpdatedEvents a run of frames produced.
func drainEvents(ch <-chan events.Event) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}
