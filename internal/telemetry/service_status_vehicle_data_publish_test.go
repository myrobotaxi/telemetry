package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// cancellingReleaseNotesReader models the live failure mode this test exists
// for: by the time the FOURTH sequential Fleet API GET of the edge bundle runs,
// the per-edge deadline is spent. It cancels the caller's context and then
// fails, exactly as a real GetReleaseNotes does when the budget lapses mid-read.
type cancellingReleaseNotesReader struct {
	*fakeVehicleReader

	cancel context.CancelFunc
}

func (f *cancellingReleaseNotesReader) GetReleaseNotes(_ context.Context, _, _ string) ([]ReleaseNote, error) {
	f.cancel()
	return nil, errors.New("context deadline exceeded")
}

// TestRefreshFromVehicleDataPublishesAfterReadDeadlineSpent pins the rule that
// the READ budget must not be able to swallow the WRITE.
//
// The client-observed symptom this reproduces: `color` populated while its
// same-payload siblings `trim` / `trimLabel` / `fsdVersion` stayed null. Those
// two values travel by DIFFERENT routes out of one vehicle_config decode —
// colour by a direct column UPDATE that happens early in the call, the rest by a
// synthetic frame published on the event bus at the very end — so a context that
// lapses in between lands exactly one of them. MYR-320 made that reachable by
// adding a fourth sequential Fleet API GET (release_notes) to the same 30s
// per-edge budget that already funded three, each with its own retry/backoff.
//
// ChannelBus.Publish checks ctx.Done() before EVERY subscriber and returns
// without delivering to any of them, so a spent read deadline silently drops the
// entire backfilled field set — no error surfaces above a Debug log, and the
// stored values simply never change.
func TestRefreshFromVehicleDataPublishesAfterReadDeadlineSpent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reader := &cancellingReleaseNotesReader{
		fakeVehicleReader: &fakeVehicleReader{
			data: &VehicleData{
				VehicleConfig: &VehicleDataVehicleConfig{
					TrimBadging:        ptr("p74d"),
					PerformancePackage: ptr("Performance"),
				},
			},
		},
		cancel: cancel,
	}

	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	m := newBusMonitor(bus, reader.fakeVehicleReader, WithReleaseNotes(reader))

	if err := m.RefreshFromVehicleData(ctx, svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData must not fail when only the read budget lapsed: %v", err)
	}

	evt, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published: the spent READ deadline swallowed the publish, " +
			"so trim/trimLabel never reach the control-state persist")
	}
	assertTelemetryString(t, evt.Fields, string(FieldTrim), "p74d")
	assertTelemetryString(t, evt.Fields, string(FieldTrimLabel), "Performance")
}

// TestRefreshFromVehicleDataPublishSurvivesAlreadyDoneContext is the same rule
// stated at its boundary: even a caller whose context was ALREADY finished
// before the call must still land the values it read. The publish is our own
// in-process fan-out, not a Tesla request, so it has no business inheriting a
// deadline that exists to bound Tesla.
func TestRefreshFromVehicleDataPublishSurvivesAlreadyDoneContext(t *testing.T) {
	t.Parallel()

	reader := &fakeVehicleReader{
		data: &VehicleData{
			VehicleConfig: &VehicleDataVehicleConfig{PerformancePackage: ptr("Performance")},
		},
	}

	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	m := newBusMonitor(bus, reader)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The read itself is a fake, so it succeeds regardless; what is under test
	// is strictly whether the frame reaches the bus.
	if err := m.RefreshFromVehicleData(ctx, svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData: %v", err)
	}
	evt, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published for a done context")
	}
	assertTelemetryString(t, evt.Fields, string(FieldTrimLabel), "Performance")
}
