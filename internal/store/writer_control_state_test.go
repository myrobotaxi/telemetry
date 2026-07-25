package store

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// TestWriter_PersistsControlState proves the MYR-269 write half: a
// VehicleTelemetryEvent carrying the owner-control fields flows through the
// writer flush and upserts the go_vehicle_control_state side table (keyed by the
// vehicle cuid resolved from the VIN). This is the SAME path the MYR-260
// /vehicle_data backfill uses — it republishes a synthetic VehicleTelemetryEvent
// — so this one test covers both the live-stream and the backfill persist paths.
//
// The frame carries ONLY control fields (none map to a "Vehicle" column), which
// also proves a control-only frame is not dropped by mapTelemetryToUpdate.
func TestWriter_PersistsControlState(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	const vin = "5YJ3E1EA1NF000CTL"
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{
		vin: {id: "veh_ctl_1", userID: "user_ctl"},
	}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	on := "On"
	locked := true
	portOpen := true
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldLocked):             {BoolVal: &locked},
		string(telemetry.FieldDoorState):          {IntVal: int64Ptr(doorBits(true, false))},
		string(telemetry.FieldHvacPower):          {StringVal: &on},
		string(telemetry.FieldChargePortDoorOpen): {BoolVal: &portOpen},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getControlWrites()) > 0
	})

	writes := vehicles.getControlWrites()
	if len(writes) == 0 {
		t.Fatal("expected a control-state upsert, got none")
	}
	got := writes[0]
	if got.VehicleID != "veh_ctl_1" {
		t.Errorf("VehicleID = %q, want veh_ctl_1 (resolved from VIN via the cache)", got.VehicleID)
	}
	eqBoolPtr(t, "IsLocked", boolPtr(true), got.Update.IsLocked)
	eqBoolPtr(t, "FrunkOpen", boolPtr(true), got.Update.FrunkOpen)
	eqBoolPtr(t, "TrunkOpen", boolPtr(false), got.Update.TrunkOpen)
	eqBoolPtr(t, "IsClimateOn", boolPtr(true), got.Update.IsClimateOn)
	eqBoolPtr(t, "ChargePortOpen", boolPtr(true), got.Update.ChargePortOpen)
}

// TestWriter_NoControlFrameSkipsControlUpsert proves an ordinary telemetry frame
// (speed only, no controls) never touches the side table — HasAny() short-
// circuits so a stable driving car does not spam the control upsert.
func TestWriter_NoControlFrameSkipsControlUpsert(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	const vin = "5YJ3E1EA1NF000NOP"
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{
		vin: {id: "veh_nop_1", userID: "user_nop"},
	}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	speed := 65.0
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): {FloatVal: &speed},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) > 0
	})
	// Give the flush a beat to have run persistControlState if it were going to.
	if writes := vehicles.getControlWrites(); len(writes) != 0 {
		t.Errorf("expected no control upsert for a speed-only frame, got %d", len(writes))
	}
}
