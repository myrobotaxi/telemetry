package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// setEnroute forces a created ride to the enroute lifecycle state directly
// (bypassing the multi-step guarded path) so completion tests have an
// in-flight ride to act on.
func setEnroute(t *testing.T, id string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET status = 'enroute' WHERE id = $1`, id); err != nil {
		t.Fatalf("force enroute %s: %v", id, err)
	}
}

// TestRideRequestRepo_ClaimDropoffDispatch_ExactlyOnce verifies the leg-2
// (dropoff) latch is independent of leg 1: the first dropoff claim wins, a
// re-delivery loses, and the win stamps dropoff_dispatched_at WITHOUT touching
// the leg-1 dispatched_at column (no clobber).
func TestRideRequestRepo_ClaimDropoffDispatch_ExactlyOnce(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, fullRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, err := repo.ClaimDropoffDispatch(ctx, created.ID)
	if err != nil {
		t.Fatalf("ClaimDropoffDispatch #1: %v", err)
	}
	if !claimed {
		t.Fatal("first dropoff claim = false, want true")
	}
	again, err := repo.ClaimDropoffDispatch(ctx, created.ID)
	if err != nil {
		t.Fatalf("ClaimDropoffDispatch #2: %v", err)
	}
	if again {
		t.Fatal("second dropoff claim = true, want false (already claimed)")
	}

	// Leg-2 latch stamped; leg-1 latch untouched (no clobber).
	var dropoffAt, pickupAt *string
	if err := testPool.QueryRow(ctx,
		`SELECT dropoff_dispatched_at::text, dispatched_at::text FROM go_ride_requests WHERE id = $1`,
		created.ID).Scan(&dropoffAt, &pickupAt); err != nil {
		t.Fatalf("read latches: %v", err)
	}
	if dropoffAt == nil {
		t.Error("dropoff_dispatched_at nil after dropoff claim, want stamped")
	}
	if pickupAt != nil {
		t.Errorf("leg-1 dispatched_at = %v after a DROPOFF claim, want nil (untouched)", *pickupAt)
	}
}

// TestRideRequestRepo_RecordDropoffDispatchOutcome verifies the leg-2 outcome
// persists on the dropoff_* columns and leaves leg-1's dispatch_status intact.
func TestRideRequestRepo_RecordDropoffDispatchOutcome(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, fullRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Record leg-1 sent first so we can prove leg-2 does not clobber it.
	if _, err := repo.ClaimDispatch(ctx, created.ID); err != nil {
		t.Fatalf("ClaimDispatch: %v", err)
	}
	if err := repo.RecordDispatchOutcome(ctx, created.ID, store.DispatchStatusSent, nil); err != nil {
		t.Fatalf("RecordDispatchOutcome: %v", err)
	}

	if _, err := repo.ClaimDropoffDispatch(ctx, created.ID); err != nil {
		t.Fatalf("ClaimDropoffDispatch: %v", err)
	}
	if err := repo.RecordDropoffDispatchOutcome(ctx, created.ID, store.DispatchStatusFailed, strPtr("vehicle_asleep")); err != nil {
		t.Fatalf("RecordDropoffDispatchOutcome: %v", err)
	}

	var legPickup, legDropoff, dropErr *string
	if err := testPool.QueryRow(ctx,
		`SELECT dispatch_status, dropoff_dispatch_status, dropoff_dispatch_error FROM go_ride_requests WHERE id = $1`,
		created.ID).Scan(&legPickup, &legDropoff, &dropErr); err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	if legPickup == nil || *legPickup != "sent" {
		t.Errorf("leg-1 dispatch_status = %v, want sent (untouched by leg 2)", legPickup)
	}
	if legDropoff == nil || *legDropoff != "failed" {
		t.Errorf("dropoff_dispatch_status = %v, want failed", legDropoff)
	}
	if dropErr == nil || *dropErr != "vehicle_asleep" {
		t.Errorf("dropoff_dispatch_error = %v, want vehicle_asleep", dropErr)
	}
}

// TestRideRequestRepo_CompleteEnrouteByVehicle verifies the drive-end
// completion transition: it completes an in-flight enroute ride (stamping
// completed_at), is a no-op for a vehicle with no enroute ride, and is
// idempotent (a second call after completion returns zero rows).
func TestRideRequestRepo_CompleteEnrouteByVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, fullRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vehicleID := created.VehicleID

	// No enroute ride yet (status requested): a drive-end is a clean no-op.
	none, err := repo.CompleteEnrouteByVehicle(ctx, vehicleID)
	if err != nil {
		t.Fatalf("CompleteEnrouteByVehicle (no enroute): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("completed %d rides with none enroute, want 0", len(none))
	}

	// Put the ride enroute, then complete it.
	setEnroute(t, created.ID)
	done, err := repo.CompleteEnrouteByVehicle(ctx, vehicleID)
	if err != nil {
		t.Fatalf("CompleteEnrouteByVehicle: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("completed %d rides, want 1", len(done))
	}
	if done[0].ID != created.ID || done[0].Status != store.RideRequestStatusCompleted {
		t.Errorf("completed ride = {id:%s status:%s}, want {%s completed}", done[0].ID, done[0].Status, created.ID)
	}
	if done[0].CompletedAt == nil {
		t.Error("CompletedAt nil after completion, want stamped")
	}

	// Idempotent: the ride is already completed, so a re-fired drive-end
	// matches zero enroute rows.
	repeat, err := repo.CompleteEnrouteByVehicle(ctx, vehicleID)
	if err != nil {
		t.Fatalf("CompleteEnrouteByVehicle (repeat): %v", err)
	}
	if len(repeat) != 0 {
		t.Errorf("re-completion returned %d rows, want 0 (already completed)", len(repeat))
	}
}

// TestRideRequestRepo_CompleteEnrouteByVehicle_OtherVehicleUnaffected proves the
// completion is scoped to the given vehicle — a drive-end for one car never
// completes another car's enroute ride.
func TestRideRequestRepo_CompleteEnrouteByVehicle_OtherVehicleUnaffected(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	mine, err := repo.Create(ctx, fullRideRequest())
	if err != nil {
		t.Fatalf("Create mine: %v", err)
	}
	setEnroute(t, mine.ID)

	// A different vehicle's drive-end must not touch this ride.
	done, err := repo.CompleteEnrouteByVehicle(ctx, "clOTHERvehicle0000000")
	if err != nil {
		t.Fatalf("CompleteEnrouteByVehicle other: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("other vehicle's drive-end completed %d rides, want 0", len(done))
	}

	got, err := repo.GetByID(ctx, mine.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != store.RideRequestStatusEnroute {
		t.Errorf("my ride status = %s after another car's drive-end, want enroute", got.Status)
	}
}
