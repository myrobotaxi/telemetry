package telemetry

import (
	"context"
	"testing"
	"time"
)

// TestPairingEvidenceIsDedupedPerVehicle is the rail on MYR-529's widened
// trigger surface. Three doors now hand the reconciler the same fact — the
// signed-command observer, §7.23's `fleet_status` answer, and a link-time push
// that applied — and an owner finishing setup can trip all three in seconds.
// N signals for one car inside the window must cost exactly ONE examination.
func TestPairingEvidenceIsDedupedPerVehicle(t *testing.T) {
	clock := forceNow
	pairing := &fakePairingResetter{
		found: true,
		candidate: FleetConfigCandidate{
			VehicleID:       "veh-1",
			VIN:             reconTestVIN,
			UserID:          "user-1",
			Status:          "offline",
			LastUpdated:     forceNow.Add(-2 * time.Hour),
			SignedCommandAt: forceNow,
		},
	}
	reader := &fakeConfigReader{synced: true}
	r := hotReconciler(&clock, reader, &fakeConfigWriter{}, &fakeAttemptRecorder{})
	r.deps.Pairing = pairing

	for i := 0; i < 5; i++ {
		r.handlePairingSignal(context.Background(), reconTestVIN)
	}

	if len(pairing.calls) != 1 {
		t.Errorf("schedule resets = %d, want 1", len(pairing.calls))
	}
	if reader.calls != 1 {
		t.Errorf("Tesla config reads = %d, want 1 for five signals in one instant", reader.calls)
	}

	// A DIFFERENT car in the same window is a different question and is never
	// suppressed by its neighbour's traffic.
	otherVIN := "5YJ3E1EA7KF000316"
	pairing.candidate.VIN = otherVIN
	pairing.candidate.VehicleID = "veh-2"
	r.handlePairingSignal(context.Background(), otherVIN)
	if reader.calls != 2 {
		t.Errorf("Tesla config reads = %d after a second vehicle, want 2", reader.calls)
	}

	// Past the window the same car is examinable again — the dedupe bounds a
	// burst, it does not mute a vehicle.
	clock = forceNow.Add(pairingExamDedupe + time.Second)
	pairing.candidate.VIN = reconTestVIN
	pairing.candidate.VehicleID = "veh-1"
	r.handlePairingSignal(context.Background(), reconTestVIN)
	if reader.calls != 3 {
		t.Errorf("Tesla config reads = %d after the dedupe window elapsed, want 3", reader.calls)
	}
}

// TestPairingDedupeMapStaysBounded pins the anti-leak property: the map is
// pruned on every call, so it is sized by the vehicles signalling within one
// window rather than by process uptime.
func TestPairingDedupeMapStaysBounded(t *testing.T) {
	clock := forceNow
	r := hotReconciler(&clock, &fakeConfigReader{}, &fakeConfigWriter{}, &fakeAttemptRecorder{})
	r.deps.Pairing = &fakePairingResetter{found: false}

	for i := 0; i < 200; i++ {
		clock = forceNow.Add(time.Duration(i) * pairingExamDedupe)
		r.handlePairingSignal(context.Background(), reconTestVIN)
	}

	if len(r.examined) > 1 {
		t.Errorf("dedupe map holds %d entries after 200 well-spaced signals; want at most 1", len(r.examined))
	}
}

// TestPairingSignalExaminesACreatedRowThatIsNotStreaming is the MYR-517 arm
// after MYR-529 moved it. The row was CREATED by this very reset, which used to
// mean "call nobody" — a rule that also skipped the exact population the seed
// was written for. Now the streaming check answers the healthy case for free
// and a silent car is examined at once.
func TestPairingSignalExaminesACreatedRowThatIsNotStreaming(t *testing.T) {
	clock := forceNow
	reader := &fakeConfigReader{synced: true}
	r := hotReconciler(&clock, reader, &fakeConfigWriter{}, &fakeAttemptRecorder{})
	r.deps.Pairing = &fakePairingResetter{
		found: true,
		candidate: FleetConfigCandidate{
			VehicleID:       "veh-spencer",
			VIN:             reconTestVIN,
			UserID:          "user-spencer",
			Status:          "offline",
			LastUpdated:     forceNow.Add(-time.Minute),
			SignedCommandAt: forceNow,
			ScheduleCreated: true,
		},
	}

	r.handlePairingSignal(context.Background(), reconTestVIN)

	if reader.calls != 1 {
		t.Errorf("config reads = %d, want 1 — a silent car with a proven key is examined now", reader.calls)
	}
}
