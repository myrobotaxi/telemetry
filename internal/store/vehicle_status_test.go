package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_GetDispatchState exercises the MYR-372 one-read vehicle probe
// against a real Postgres. It backs the reservation sweeper, so five things
// matter: it reports the status FAITHFULLY (noticing a car that entered service
// after the accept is the sweeper's whole job), it carries the ride-share
// flag's absent-means-enabled rule across the LEFT JOIN, it answers the MYR-581
// owner-named question off the same row, every fact comes from ONE statement,
// and a vehicle it cannot find is an ERROR rather than a fabricated state the
// sweeper would happily dispatch against.
func TestVehicleRepo_GetDispatchState(t *testing.T) {
	// The join reaches the Go-owned side table, which TestMain's Prisma-only
	// createSchema does not create. Idempotent.
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)

	const (
		ownerID = "user_status_owner"
		parked  = "veh_status_001"
		inSvc   = "veh_status_002"
	)
	seedVehicleSummaryRow(t, parked, ownerID, "5YJ3E1EA1NF000T01", "Ready", "Model 3", 2024, "White",
		store.VehicleStatusParked, 61, 190)
	seedVehicleSummaryRow(t, inSvc, ownerID, "5YJ3E1EA1NF000T02", "AtShop", "Model 3", 2024, "Blue",
		store.VehicleStatusInService, 44, 150)

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	t.Run("reports the persisted status", func(t *testing.T) {
		tests := []struct {
			vehicle string
			want    store.VehicleStatus
		}{
			{vehicle: parked, want: store.VehicleStatusParked},
			{vehicle: inSvc, want: store.VehicleStatusInService},
		}
		for _, tt := range tests {
			got, err := repo.GetDispatchState(ctx, tt.vehicle)
			if err != nil {
				t.Fatalf("GetDispatchState(%s): %v", tt.vehicle, err)
			}
			if got.Status != tt.want {
				t.Errorf("GetDispatchState(%s) status = %q, want %q", tt.vehicle, got.Status, tt.want)
			}
		}
	})

	t.Run("a car with no control-state row reads as ride-share ENABLED", func(t *testing.T) {
		// The COALESCE over the LEFT JOIN, which is what lets the sweeper's
		// pause arm ride this read without a second statement. A missing row
		// must be indistinguishable from a row at the column default.
		var rows int
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM go_vehicle_control_state WHERE vehicle_id = $1`, parked).Scan(&rows); err != nil {
			t.Fatalf("count control-state rows: %v", err)
		}
		if rows != 0 {
			t.Fatalf("precondition: expected no control-state row yet, got %d", rows)
		}

		got, err := repo.GetDispatchState(ctx, parked)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if !got.RideShareEnabled {
			t.Error("a car nobody has ever touched must read as ride-share ENABLED")
		}
	})

	// MYR-581: the owner-named arm, on the same statement. The seed creates a
	// "Vehicle" row whose "userId" has NO row in any of the three identity
	// sources, which is precisely the fail-closed case — an owner nobody can look
	// up must read NAMELESS, so the sweeper holds rather than dispatching a ride
	// nobody could describe.
	t.Run("an owner with no identity row anywhere reads as NAMELESS", func(t *testing.T) {
		got, err := repo.GetDispatchState(ctx, inSvc)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if got.OwnerNamed {
			t.Error("an owner with no identity row must read as nameless (fail closed)")
		}
	})

	// MYR-583 CHANGED THIS ARM'S ANSWER, which is the whole of the issue: a name
	// on a rung is no longer enough, the OWNER MUST HAVE CONFIRMED IT. Before the
	// confirmation gate, seeding the go_users name alone flipped this to NAMED.
	t.Run("a CONFIRMED name on any rung makes the owner NAMED", func(t *testing.T) {
		// go_users is the LOWEST rung, so proving it alone flips the gate proves
		// the COALESCE reaches past the two rungs above it — the exact defect a
		// `"User"."name"`-only lookup had.
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_users (id, name) VALUES ($1, $2)
			 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, ownerID, "Ada Lovelace"); err != nil {
			t.Fatalf("seed go_users name: %v", err)
		}

		// UNCONFIRMED FIRST. This is the state every legacy account is in, and the
		// state the client ruled must read as "Setting Up": a perfectly resolvable
		// name that nobody approved.
		got, err := repo.GetDispatchState(ctx, inSvc)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if got.OwnerNamed {
			t.Error("an UNCONFIRMED name must not satisfy the gate (MYR-583)")
		}

		seedNameConfirmation(t, ownerID)
		got, err = repo.GetDispatchState(ctx, inSvc)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if !got.OwnerNamed {
			t.Error("a CONFIRMED name on the go_users rung must satisfy the gate")
		}

		// Whitespace-only is NOT a name: TRIM/NULLIF must collapse it to NULL at
		// every rung, or the gate would say "named" while the catalog emitted
		// null — the one disagreement the shared ladder exists to prevent. Note
		// the confirmation row is still in place, so this arm proves the two
		// conditions are a CONJUNCTION rather than either one alone.
		if _, err := testPool.Exec(ctx,
			`UPDATE go_users SET name = '   ' WHERE id = $1`, ownerID); err != nil {
			t.Fatalf("blank go_users name: %v", err)
		}
		got, err = repo.GetDispatchState(ctx, inSvc)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if got.OwnerNamed {
			t.Error("a whitespace-only name must not satisfy the gate, confirmed or not")
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM go_users WHERE id = $1`, ownerID); err != nil {
			t.Fatalf("clean go_users: %v", err)
		}
		cleanNameConfirmations(t, ownerID)
	})

	t.Run("a pause is visible on the same read as the status", func(t *testing.T) {
		if err := repo.SetRideShareEnabled(ctx, parked, false); err != nil {
			t.Fatalf("SetRideShareEnabled(false): %v", err)
		}

		got, err := repo.GetDispatchState(ctx, parked)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		if got.RideShareEnabled {
			t.Error("the owner's pause did not survive the join")
		}
		if got.Status != store.VehicleStatusParked {
			t.Errorf("status = %q, want %q — pausing must not disturb the vehicle row", got.Status, store.VehicleStatusParked)
		}
	})

	t.Run("a status flip is visible to the next read", func(t *testing.T) {
		// The sweeper's entire reason for existing: the car was dispatchable
		// when the reservation was accepted and is not any more. A cached or
		// stale answer here would reinstate the bug MYR-372 closes.
		if _, err := testPool.Exec(ctx,
			`UPDATE "Vehicle" SET "status" = $2 WHERE "id" = $1`, parked, store.VehicleStatusInService); err != nil {
			t.Fatalf("flip status: %v", err)
		}

		got, err := repo.GetDispatchState(ctx, parked)
		if err != nil {
			t.Fatalf("GetDispatchState after flip: %v", err)
		}
		if got.Status != store.VehicleStatusInService {
			t.Errorf("status after flip = %q, want %q", got.Status, store.VehicleStatusInService)
		}
	})

	t.Run("an unknown vehicle is ErrVehicleNotFound, never a status", func(t *testing.T) {
		got, err := repo.GetDispatchState(ctx, "veh_does_not_exist")
		if !errors.Is(err, store.ErrVehicleNotFound) {
			t.Fatalf("GetDispatchState(unknown) error = %v, want ErrVehicleNotFound", err)
		}
		if got != (store.DispatchState{}) {
			t.Errorf("GetDispatchState(unknown) returned %+v; absence must never be dressed as a value", got)
		}
	})
}
