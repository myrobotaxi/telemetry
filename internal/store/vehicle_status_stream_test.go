package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-454 — the persisted Vehicle.status column must follow the stream's own
// gear/speed, not wait for someone to remember to write it.
//
// Before this fix the column had two writers (drive.ended → 'parked', the
// ServiceStatusMonitor → 'in_service'/'parked') and NEITHER ever wrote
// 'driving'; drive.started wrote nothing at all. 'driving' existed only as a
// live WebSocket re-derivation, so every surface reading the column — the
// REST snapshot, the WS on-connect snapshot, the vehicles list behind the map
// sheet — served 'parked' for the whole of a real drive. Prod carried rows
// that contradicted themselves: status 'offline' beside speed 21 and gear 'D'.
//
// These tests exercise the real CASE against real Postgres. A pure Go
// derivation would prove nothing here — the entire mechanism is SQL evaluated
// against the row's prior values.

// statusOf reads the persisted enum back for a VIN.
func statusOf(t *testing.T, pool *pgxpool.Pool, vin string) string {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		`SELECT "status"::text FROM "Vehicle" WHERE "vin" = $1`, vin).Scan(&got)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	return got
}

// setStatus forces a starting status, standing in for whichever writer
// (drive.ended, the service monitor, the schema default) last owned the row.
func setStatus(t *testing.T, pool *pgxpool.Pool, vin, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE "Vehicle" SET "status" = $2::"VehicleStatus" WHERE "vin" = $1`, vin, status)
	if err != nil {
		t.Fatalf("seed status %q: %v", status, err)
	}
}

func TestVehicleStatus_FollowsStream(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker not available")
	}

	tests := []struct {
		name string
		// startStatus is what the column held before the frame arrived.
		startStatus string
		// startGear is the last gear the row had stored, which is what a
		// speed-only frame must resolve against. Empty means NULL — the car
		// has never reported a gear.
		startGear  string
		startSpeed int
		update     store.VehicleUpdate
		want       string
	}{
		{
			name:        "gear D starts driving",
			startStatus: "parked",
			update:      store.VehicleUpdate{GearPosition: strPtr("D"), Speed: intPtr(0)},
			want:        "driving",
		},
		{
			name:        "reverse counts as driving",
			startStatus: "parked",
			update:      store.VehicleUpdate{GearPosition: strPtr("R")},
			want:        "driving",
		},
		{
			// THE REPORTED BUG. Tesla emits Gear on CHANGE only, so a car
			// that shifted into D once sends nothing but speed for the rest
			// of the drive. Stopped at a light at 0 mph, the frame carries
			// speed=0 and no gear — and the row must still say driving,
			// resolved against the gear it last stored. This is also the
			// case the issue's counter-example (0 mph correctly showing
			// Driving) proves cannot be fixed by a speed threshold.
			name:        "speed-only frame at 0 mph stays driving via stored gear",
			startStatus: "parked",
			startGear:   "D",
			update:      store.VehicleUpdate{Speed: intPtr(0)},
			want:        "driving",
		},
		{
			name:        "speed-only frame while moving is driving",
			startStatus: "parked",
			startGear:   "D",
			update:      store.VehicleUpdate{Speed: intPtr(31)},
			want:        "driving",
		},
		{
			// Speed beats a stale gear: rolling with the row still on P
			// must not report parked.
			name:        "moving in stored gear P is still driving",
			startStatus: "parked",
			startGear:   "P",
			update:      store.VehicleUpdate{Speed: intPtr(21)},
			want:        "driving",
		},
		{
			name:        "shifting to park ends driving",
			startStatus: "driving",
			startGear:   "D",
			update:      store.VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)},
			want:        "parked",
		},
		{
			// The MYR-437 trap: 'offline' is the schema default that
			// owner-vehicle provisioning never overwrites, and it makes a
			// happily-streaming car permanently ineligible for dispatch. A
			// frame arriving is proof the car is not offline. This is the
			// exact prod row 7SAYGDED5TA736164 was stuck in: status
			// 'offline', speed 21, gear 'D'.
			name:        "streaming heals a row stuck at the offline default",
			startStatus: "offline",
			update:      store.VehicleUpdate{GearPosition: strPtr("D"), Speed: intPtr(21)},
			want:        "driving",
		},
		{
			name:        "streaming heals offline to parked when stationary",
			startStatus: "offline",
			update:      store.VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)},
			want:        "parked",
		},
		{
			// Precedence from vehicle-state-schema.md §2.4: a tech
			// test-driving a car in service mode surfaces as driving.
			name:        "driving outranks in_service",
			startStatus: "in_service",
			update:      store.VehicleUpdate{GearPosition: strPtr("D")},
			want:        "driving",
		},
		{
			// The service monitor owns in_service and this server never
			// writes 'charging' at all — a stationary frame must not stomp
			// either at a 5s cadence.
			name:        "stationary frame preserves in_service",
			startStatus: "in_service",
			update:      store.VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)},
			want:        "in_service",
		},
		{
			name:        "stationary frame preserves charging",
			startStatus: "charging",
			update:      store.VehicleUpdate{Speed: intPtr(0)},
			want:        "charging",
		},
		{
			name:        "driving outranks charging",
			startStatus: "charging",
			update:      store.VehicleUpdate{Speed: intPtr(15)},
			want:        "driving",
		},
		{
			// A frame with no motion column must not re-derive anything.
			name:        "climate-only frame leaves status untouched",
			startStatus: "driving",
			startGear:   "D",
			update:      store.VehicleUpdate{InteriorTemp: intPtr(67)},
			want:        "driving",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanTables(t, testPool)
			const vin = "5YJ3E1EA1NF000454"
			seedVehicle(t, testPool, "veh_myr454", vin)

			ctx := context.Background()
			if tt.startGear != "" || tt.startSpeed != 0 {
				var gear *string
				if tt.startGear != "" {
					gear = &tt.startGear
				}
				if _, err := testPool.Exec(ctx,
					`UPDATE "Vehicle" SET "gearPosition" = $2, "speed" = $3 WHERE "vin" = $1`,
					vin, gear, tt.startSpeed); err != nil {
					t.Fatalf("seed motion columns: %v", err)
				}
			}
			setStatus(t, testPool, vin, tt.startStatus)

			repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
			upd := tt.update
			upd.LastUpdated = time.Now()
			if err := repo.UpdateTelemetry(ctx, vin, upd); err != nil {
				t.Fatalf("UpdateTelemetry: %v", err)
			}

			if got := statusOf(t, testPool, vin); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVehicleStatus_ReplaysReportedDrive walks the MYR-454 submission window
// as a sequence of frames, asserting the column is never 'parked' while the
// car is actually moving.
//
// This is the shape of Lunar's drive 2f7c7465… (2026-08-05 22:53:31Z →
// 23:04:39Z). The tester's screenshot was taken at 22:57Z — four minutes in,
// stopped at a light at 0 mph — and the map sheet read "Parked". Frame 3 is
// that instant.
func TestVehicleStatus_ReplaysReportedDrive(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker not available")
	}

	cleanTables(t, testPool)
	const vin = "5YJ3E1EA1NF000455"
	seedVehicle(t, testPool, "veh_myr454_replay", vin)

	ctx := context.Background()
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	// The previous drive ended here — this is the write that latched 'parked'
	// and was never undone.
	if err := repo.UpdateStatus(ctx, vin, store.VehicleStatusParked); err != nil {
		t.Fatalf("seed drive.ended parked: %v", err)
	}

	frames := []struct {
		desc   string
		update store.VehicleUpdate
		want   string
	}{
		{"still parked before departure", store.VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)}, "parked"},
		{"shift into D — drive begins", store.VehicleUpdate{GearPosition: strPtr("D"), Speed: intPtr(0)}, "driving"},
		{"underway, gear not re-sent", store.VehicleUpdate{Speed: intPtr(34)}, "driving"},
		{"22:57Z — stopped at a light, 0 mph", store.VehicleUpdate{Speed: intPtr(0)}, "driving"},
		{"moving again", store.VehicleUpdate{Speed: intPtr(27)}, "driving"},
		{"climate frame mid-drive changes nothing", store.VehicleUpdate{InteriorTemp: intPtr(67)}, "driving"},
		{"arrives and shifts to P", store.VehicleUpdate{GearPosition: strPtr("P"), Speed: intPtr(0)}, "parked"},
	}

	for i, f := range frames {
		upd := f.update
		upd.LastUpdated = time.Now()
		if err := repo.UpdateTelemetry(ctx, vin, upd); err != nil {
			t.Fatalf("frame %d (%s): UpdateTelemetry: %v", i, f.desc, err)
		}
		if got := statusOf(t, testPool, vin); got != f.want {
			t.Fatalf("frame %d (%s): status = %q, want %q", i, f.desc, got, f.want)
		}
	}
}

func intPtr(i int) *int { return &i }
