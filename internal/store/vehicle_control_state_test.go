// Package store_test — MYR-269 integration coverage for the owner-control side
// table (go_vehicle_control_state). These tests reproduce the exact prod bug:
// the owner controls (Lock/Trunk/Climate/Charge-port) showed "Unavailable" on a
// snapshot for a non-streaming car because the stream-only fields were never
// persisted. The fix persists them, so a LATER snapshot (sheet-open, NO live
// socket) returns the last-known value. Test 1 drives that end to end: persist,
// then read back via GetByID with no writer/socket in between.
package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// ensureControlMigration guarantees the go_vehicle_control_state table exists
// regardless of test-file execution order (the shared container's go_* tables
// are created by RunMigrations, which is idempotent). Named boolean helpers keep
// the assertions readable.
func ensureControlMigration(t *testing.T) {
	t.Helper()
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations (ensure go_vehicle_control_state): %v", err)
	}
}

func bp(b bool) *bool { return &b }

func wantBoolPtr(t *testing.T, field string, want, got *bool) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, fmtBP(want), fmtBP(got))
	case *want != *got:
		t.Errorf("%s: want %v, got %v", field, *want, *got)
	}
}

func fmtBP(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "true"
	}
	return "false"
}

// TestControlState_PersistThenSnapshotAcrossSocketGap is the headline MYR-269
// scenario: a non-streaming car's controls are persisted (as they would be by
// the live persist path or the MYR-260 /vehicle_data backfill), THEN a later
// GET /snapshot — with no live socket — returns them. Before MYR-269 GetByID
// returned nothing for these fields; now it hydrates them from the side table.
func TestControlState_PersistThenSnapshotAcrossSocketGap(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_gap_1"
		vin   = "5YJ3E1EA1NF00GAP1"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Persist happens (simulating the writer during a live socket, or the
	// non-waking REST backfill for an in-service car).
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		IsLocked:       bp(true),
		FrunkOpen:      bp(false),
		TrunkOpen:      bp(true),
		IsClimateOn:    bp(true),
		ChargePortOpen: bp(false),
	}); err != nil {
		t.Fatalf("UpsertControlState: %v", err)
	}

	// The socket gap: no writer, no live stream — just a later snapshot read.
	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantBoolPtr(t, "IsLocked", bp(true), v.IsLocked)
	wantBoolPtr(t, "FrunkOpen", bp(false), v.FrunkOpen)
	wantBoolPtr(t, "TrunkOpen", bp(true), v.TrunkOpen)
	wantBoolPtr(t, "IsClimateOn", bp(true), v.IsClimateOn)
	wantBoolPtr(t, "ChargePortOpen", bp(false), v.ChargePortOpen)

	// Vehicle.status is persisted separately (Prisma "Vehicle".status) and does
	// NOT depend on the control read-back — confirm it still resolves.
	if v.Status == "" {
		t.Error("Vehicle.Status is empty; status must be independent of control read-back")
	}
}

// TestControlState_PerFieldLastWriterWins proves the COALESCE upsert updates
// only the fields present in a frame and leaves the rest intact — and that a
// real false observation overwrites a prior true.
func TestControlState_PerFieldLastWriterWins(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_lww_1"
		vin   = "5YJ3E1EA1NF00LWW1"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Frame 1: only lock arrives.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{IsLocked: bp(true)}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Frame 2: only frunk arrives (lock absent → must be preserved).
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{FrunkOpen: bp(true)}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID after frame 2: %v", err)
	}
	wantBoolPtr(t, "IsLocked (preserved)", bp(true), v.IsLocked)
	wantBoolPtr(t, "FrunkOpen (new)", bp(true), v.FrunkOpen)
	wantBoolPtr(t, "TrunkOpen (never read)", nil, v.TrunkOpen)

	// Frame 3: lock flips to a real false — must overwrite, not be treated as absent.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{IsLocked: bp(false)}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	v, err = repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID after frame 3: %v", err)
	}
	wantBoolPtr(t, "IsLocked (overwritten false)", bp(false), v.IsLocked)
	wantBoolPtr(t, "FrunkOpen (still preserved)", bp(true), v.FrunkOpen)
}

// TestControlState_NeverReadIsNil proves a vehicle with no side-table row
// returns nil for every control field — the honest "unavailable", never a
// fabricated on/off.
func TestControlState_NeverReadIsNil(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_nil_1"
		vin   = "5YJ3E1EA1NF00NIL1"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	v, err := repo.GetByID(context.Background(), vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantBoolPtr(t, "IsLocked", nil, v.IsLocked)
	wantBoolPtr(t, "FrunkOpen", nil, v.FrunkOpen)
	wantBoolPtr(t, "TrunkOpen", nil, v.TrunkOpen)
	wantBoolPtr(t, "IsClimateOn", nil, v.IsClimateOn)
	wantBoolPtr(t, "ChargePortOpen", nil, v.ChargePortOpen)
}

// TestControlState_NoFieldsIsNoOp proves an empty update does not error or
// create a row.
func TestControlState_NoFieldsIsNoOp(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_noop_1"
		vin   = "5YJ3E1EA1NF00NOP1"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{}); err != nil {
		t.Fatalf("empty UpsertControlState should be a no-op, got: %v", err)
	}
	var count int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_vehicle_control_state WHERE vehicle_id = $1`, vehID,
	).Scan(&count); err != nil {
		t.Fatalf("count control rows: %v", err)
	}
	if count != 0 {
		t.Errorf("empty update created %d rows, want 0", count)
	}
}

// cleanControlState truncates the Go-owned side table between scenarios.
func cleanControlState(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_vehicle_control_state`); err != nil {
		t.Fatalf("clean go_vehicle_control_state: %v", err)
	}
}

func ip(i int) *int         { return &i }
func fp(f float64) *float64 { return &f }

func wantIntPtr(t *testing.T, field string, want, got *int) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, fmtIP(want), fmtIP(got))
	case *want != *got:
		t.Errorf("%s: want %d, got %d", field, *want, *got)
	}
}

func wantFloatPtr(t *testing.T, field string, want, got *float64) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, want, got)
	case *want != *got:
		t.Errorf("%s: want %v, got %v", field, *want, *got)
	}
}

func fmtIP(p *int) string {
	if p == nil {
		return "nil"
	}
	return "set"
}

// TestControlState_CabinLevelsPersistThenSnapshotAcrossSocketGap is the MYR-273
// headline: the cabin SETTINGS (temp setpoints, fan speed, seat heater/cooler
// levels, media volume) are persisted (live persist path or /vehicle_data
// backfill), THEN a later GET /snapshot — with no live socket — returns them.
// Before MYR-273 these showed "—" on a non-streaming snapshot.
func TestControlState_CabinLevelsPersistThenSnapshotAcrossSocketGap(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_cabin_1"
		vin   = "5YJ3E1EA1NF00CAB1"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		DriverTempSetting:    ip(68),
		PassengerTempSetting: ip(70),
		FanSpeed:             ip(3),
		SeatHeaterLeft:       ip(2),
		SeatHeaterRight:      ip(1),
		SeatHeaterRearLeft:   ip(0),
		SeatHeaterRearCenter: ip(0),
		SeatHeaterRearRight:  ip(0),
		SeatCoolerLeft:       ip(1),
		SeatCoolerRight:      ip(2),
		MediaVolume:          fp(5.5),
	}); err != nil {
		t.Fatalf("UpsertControlState: %v", err)
	}

	// The socket gap: no writer, no live stream — just a later snapshot read.
	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantIntPtr(t, "DriverTempSetting", ip(68), v.DriverTempSetting)
	wantIntPtr(t, "PassengerTempSetting", ip(70), v.PassengerTempSetting)
	wantIntPtr(t, "FanSpeed", ip(3), v.FanSpeed)
	wantIntPtr(t, "SeatHeaterLeft", ip(2), v.SeatHeaterLeft)
	wantIntPtr(t, "SeatHeaterRight", ip(1), v.SeatHeaterRight)
	wantIntPtr(t, "SeatHeaterRearLeft", ip(0), v.SeatHeaterRearLeft)
	wantIntPtr(t, "SeatHeaterRearCenter", ip(0), v.SeatHeaterRearCenter)
	wantIntPtr(t, "SeatHeaterRearRight", ip(0), v.SeatHeaterRearRight)
	wantIntPtr(t, "SeatCoolerLeft", ip(1), v.SeatCoolerLeft)
	wantIntPtr(t, "SeatCoolerRight", ip(2), v.SeatCoolerRight)
	wantFloatPtr(t, "MediaVolume", fp(5.5), v.MediaVolume)
}

// TestControlState_CabinLevelsPerFieldLastWriterWins proves the COALESCE upsert
// updates only the cabin-level fields present in a frame and leaves the rest
// intact — including that a real 0 (seat heater off / muted) overwrites a prior
// level, and that bool + level fields coexist in the same row without clobbering.
func TestControlState_CabinLevelsPerFieldLastWriterWins(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_cabin_lww_1"
		vin   = "5YJ3E1EA1NF00CAB2"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	// Frame 1: a bool control + fan speed 3.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		IsLocked: bp(true),
		FanSpeed: ip(3),
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Frame 2: only seat heater arrives (fan speed + lock absent → preserved).
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		SeatHeaterLeft: ip(2),
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	v, err := repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID after frame 2: %v", err)
	}
	wantBoolPtr(t, "IsLocked (preserved)", bp(true), v.IsLocked)
	wantIntPtr(t, "FanSpeed (preserved)", ip(3), v.FanSpeed)
	wantIntPtr(t, "SeatHeaterLeft (new)", ip(2), v.SeatHeaterLeft)
	wantIntPtr(t, "SeatHeaterRight (never read)", nil, v.SeatHeaterRight)

	// Frame 3: fan speed flips to a real 0 — must overwrite, not be treated as absent.
	if err := repo.UpsertControlState(ctx, vehID, store.ControlStateUpdate{
		FanSpeed:    ip(0),
		MediaVolume: fp(0),
	}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	v, err = repo.GetByID(ctx, vehID)
	if err != nil {
		t.Fatalf("GetByID after frame 3: %v", err)
	}
	wantIntPtr(t, "FanSpeed (overwritten 0)", ip(0), v.FanSpeed)
	wantFloatPtr(t, "MediaVolume (overwritten 0)", fp(0), v.MediaVolume)
	wantIntPtr(t, "SeatHeaterLeft (still preserved)", ip(2), v.SeatHeaterLeft)
}

// TestControlState_CabinLevelsNeverReadIsNil proves a vehicle with no side-table
// row returns nil for every cabin-level field — the honest "—", never a
// fabricated 0.
func TestControlState_CabinLevelsNeverReadIsNil(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureControlMigration(t)
	cleanTables(t, testPool)
	cleanControlState(t)

	const (
		vehID = "veh_ctl_cabin_nil_1"
		vin   = "5YJ3E1EA1NF00CAB3"
	)
	seedVehicle(t, testPool, vehID, vin)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	v, err := repo.GetByID(context.Background(), vehID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantIntPtr(t, "DriverTempSetting", nil, v.DriverTempSetting)
	wantIntPtr(t, "PassengerTempSetting", nil, v.PassengerTempSetting)
	wantIntPtr(t, "FanSpeed", nil, v.FanSpeed)
	wantIntPtr(t, "SeatHeaterLeft", nil, v.SeatHeaterLeft)
	wantIntPtr(t, "SeatHeaterRight", nil, v.SeatHeaterRight)
	wantIntPtr(t, "SeatHeaterRearLeft", nil, v.SeatHeaterRearLeft)
	wantIntPtr(t, "SeatHeaterRearCenter", nil, v.SeatHeaterRearCenter)
	wantIntPtr(t, "SeatHeaterRearRight", nil, v.SeatHeaterRearRight)
	wantIntPtr(t, "SeatCoolerLeft", nil, v.SeatCoolerLeft)
	wantIntPtr(t, "SeatCoolerRight", nil, v.SeatCoolerRight)
	wantFloatPtr(t, "MediaVolume", nil, v.MediaVolume)
}
