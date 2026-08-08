package store

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// MYR-409 — the nav-reading stamp's write half.
//
// The whole value of this column is the frames it does NOT stamp, so the
// negative cases below carry more weight than the positive one. If a motion
// frame ever stamps, the column has silently become the row stamp again and
// every gate keyed on it is back to believing a car that is driving somewhere
// else.

// navStampWriter spins up a writer wired to a VIN that resolves, and returns the
// mock so the test can inspect what reached the side table.
func navStampWriter(t *testing.T, vin, vehicleID string) (events.Bus, *mockVehicleUpdater) {
	t.Helper()
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{
		vin: {id: vehicleID, userID: "user_nav"},
	}}
	w := newTestWriter(t, bus, vehicles, &mockDrivePersister{}, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	return bus, vehicles
}

// TestWriter_NavFrameStampsFreshness is the positive case: a frame carrying a
// navigation-group member records when the car said it.
func TestWriter_NavFrameStampsFreshness(t *testing.T) {
	const vin = "5YJ3E1EA1NF000NV1"
	bus, vehicles := navStampWriter(t, vin, "veh_nav_1")

	minutes := 12.0
	miles := 4.5
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldMinutesToArrival): {FloatVal: &minutes},
		string(telemetry.FieldMilesToArrival):   {FloatVal: &miles},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getNavStampWrites()) > 0
	})
	writes := vehicles.getNavStampWrites()
	if len(writes) == 0 {
		t.Fatal("a frame carrying minutesToArrival + milesToArrival stamped no nav freshness")
	}
	if writes[0].VehicleID != "veh_nav_1" {
		t.Errorf("VehicleID = %q, want veh_nav_1 (resolved from VIN)", writes[0].VehicleID)
	}
	if writes[0].ReadingAt.IsZero() {
		t.Error("ReadingAt is zero; the stamp carries no instant")
	}
}

// TestWriter_MotionFrameDoesNotStampNavFreshness is the defect in one test.
//
// Speed and gear are what a car streams continuously while driving, and before
// MYR-409 they moved the only timestamp any freshness gate had. If this test
// ever fails, a car driving its owner's errand is once again reporting fresh
// navigation to a rider's Arriving gate.
//
// It also covers the MYR-454 derived-status fold specifically: `status` is
// written by exactly this shape of frame (gear/speed, `Streamed`), in the same
// UPDATE, and it must not drag a nav stamp along with it.
func TestWriter_MotionFrameDoesNotStampNavFreshness(t *testing.T) {
	const vin = "5YJ3E1EA1NF000NV2"
	bus, vehicles := navStampWriter(t, vin, "veh_nav_2")

	speed := 43.0
	gear := "D"
	heading := 180.0
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed):   {FloatVal: &speed},
		string(telemetry.FieldGear):    {StringVal: &gear},
		string(telemetry.FieldHeading): {FloatVal: &heading},
	})

	// Wait on the TELEMETRY write, not on a timeout: it proves the frame was
	// fully processed, so an absent nav stamp is a decision rather than a race.
	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) > 0
	})
	if got := vehicles.getNavStampWrites(); len(got) != 0 {
		t.Fatalf("a motion-only frame stamped nav freshness %d time(s): %+v — "+
			"the column is a row stamp again and MYR-409 is re-opened", len(got), got)
	}
}

// TestWriter_NavCancelStampsFreshness — "this car has no route" is a reading.
//
// The same update NULLs etaMinutes, so the stamp can never date a value that is
// no longer in the row. Refusing to stamp here would leave the column ageing
// while the fact it describes is current.
func TestWriter_NavCancelStampsFreshness(t *testing.T) {
	const vin = "5YJ3E1EA1NF000NV3"
	bus, vehicles := navStampWriter(t, vin, "veh_nav_3")

	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldMinutesToArrival): {Invalid: true},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getNavStampWrites()) > 0
	})
	if got := vehicles.getNavStampWrites(); len(got) == 0 {
		t.Fatal("a navigation-cancelled frame stamped nothing; a cancel is a nav reading")
	}
}

// TestWriter_ControlOnlyFrameDoesNotStampNavFreshness guards the seam between
// the two writes that now share persistSideTable. They resolve the same VIN and
// touch the same row, and folding them together must not let one imply the
// other: a lock toggle says nothing about navigation.
func TestWriter_ControlOnlyFrameDoesNotStampNavFreshness(t *testing.T) {
	const vin = "5YJ3E1EA1NF000NV4"
	bus, vehicles := navStampWriter(t, vin, "veh_nav_4")

	locked := true
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldLocked): {BoolVal: &locked},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getControlWrites()) > 0
	})
	if got := vehicles.getNavStampWrites(); len(got) != 0 {
		t.Fatalf("a control-only frame stamped nav freshness: %+v", got)
	}
}

// TestCarriedNavReading enumerates the decision directly, because the writer
// tests above can only reach it through a live bus and a flush.
func TestCarriedNavReading(t *testing.T) {
	minutes := 7
	miles := 2.5
	name := "Home"
	lat := 37.77
	speed := 30

	tests := []struct {
		name   string
		update VehicleUpdate
		want   bool
	}{
		{"empty", VehicleUpdate{}, false},
		{"speed only", VehicleUpdate{Speed: &speed}, false},
		{"eta minutes", VehicleUpdate{EtaMinutes: &minutes}, true},
		{"remaining distance", VehicleUpdate{TripDistRemaining: &miles}, true},
		{"destination name", VehicleUpdate{DestinationName: &name}, true},
		{"destination coordinate", VehicleUpdate{DestinationLatitude: &lat}, true},
		{"origin coordinate", VehicleUpdate{OriginLatitude: &lat}, true},
		{"route line cleared", VehicleUpdate{ClearFields: []string{"navRouteCoordinates"}}, true},
		{"eta cleared", VehicleUpdate{ClearFields: []string{"etaMinutes"}}, true},
		// The server's own reverse geocode of a destination the car reported
		// earlier. Letting it stamp would date a nav reading by the geocoder's
		// clock rather than the car's — and it lands on the flush path, after
		// the stamp decision, so it must not count.
		{"geocoded destination address only", VehicleUpdate{
			DestinationAddress: &name,
		}, false},
		// A non-nav clear must not stamp: nothing about navigation was said.
		{"unrelated clear", VehicleUpdate{ClearFields: []string{"locationName"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.update
			if got := u.carriedNavReading(); got != tt.want {
				t.Errorf("carriedNavReading() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMergeUpdate_NavStampMovesForwardOnly is the coalescing invariant.
//
// A flush window normally holds one nav frame among several motion frames, in
// arrival order, and the merge must not let the motion frames erase the stamp
// the nav frame earned. Nor may an out-of-order pair move it backwards: a stamp
// that regresses reports a fresh reading as stale, which is exactly the flicker
// a client gate must never see.
func TestMergeUpdate_NavStampMovesForwardOnly(t *testing.T) {
	early := time.Now().UTC().Add(-30 * time.Second)
	late := time.Now().UTC()

	tests := []struct {
		name     string
		dst, src time.Time
		want     time.Time
	}{
		{"nav then motion — motion must not erase", late, time.Time{}, late},
		{"motion then nav — nav must land", time.Time{}, late, late},
		{"out of order — must not regress", late, early, late},
		{"in order — must advance", early, late, late},
		{"neither carried nav", time.Time{}, time.Time{}, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &VehicleUpdate{NavReadingAt: tt.dst}
			src := &VehicleUpdate{NavReadingAt: tt.src}
			mergeUpdate(dst, src)
			if !dst.NavReadingAt.Equal(tt.want) {
				t.Errorf("merged NavReadingAt = %v, want %v", dst.NavReadingAt, tt.want)
			}
		})
	}
}
