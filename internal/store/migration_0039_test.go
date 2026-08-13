package store_test

import (
	"context"
	"testing"
)

// MYR-539 — migration 0039's shape. Three things about go_ride_stops are load
// bearing enough that a silent change to any of them would be a data-loss or a
// privacy defect rather than a performance regression, so they are pinned here.

// TestMigration0039_RideStopsShape pins the columns, the status CHECK, and the
// absence of any plaintext coordinate column.
func TestMigration0039_RideStopsShape(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	var columns int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'go_ride_stops'`,
	).Scan(&columns); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if columns == 0 {
		t.Fatal("go_ride_stops missing after migrate up")
	}

	// ENCRYPT-ONLY (NFR-3.23). There is no plaintext lat/lng column and there
	// must never be one: a stop carries the same P1 GPS data the endpoints do,
	// and the endpoints have no fallback column either.
	var plaintext int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'go_ride_stops' AND column_name IN ('lat', 'lng', 'latitude', 'longitude')`,
	).Scan(&plaintext); err != nil {
		t.Fatalf("read plaintext columns: %v", err)
	}
	if plaintext != 0 {
		t.Errorf("go_ride_stops has %d plaintext coordinate column(s); stops are encrypt-only", plaintext)
	}

	// The status CHECK: the column is server-owned, and a value outside the
	// contract enum is a bug the database should refuse rather than store.
	var checks int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.check_constraints
		  WHERE constraint_name = 'go_ride_stops_status_check'`,
	).Scan(&checks); err != nil {
		t.Fatalf("read check constraint: %v", err)
	}
	if checks == 0 {
		t.Error("go_ride_stops_status_check missing; the status column would accept anything")
	}

	// (ride_id, position) is the ONLY access path — the detail read, the
	// batched list read, the replacement's FOR UPDATE snapshot and the
	// leg-advance probe are all this shape.
	var indexes int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'go_ride_stops' AND indexname = 'idx_go_ride_stops_ride_position'`,
	).Scan(&indexes); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if indexes == 0 {
		t.Error("idx_go_ride_stops_ride_position missing")
	}
}

// TestMigration0039_StopsCascadeWithTheirRide is the retention argument, and it
// is why the FK exists at all. The MYR-447 pruner DELETEs terminal ride rows
// directly; a stop row that survived that delete would be P1 location data with
// no owner and nothing left to remove it.
func TestMigration0039_StopsCascadeWithTheirRide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const rideID = "cride0539cascade"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_requests (
			id, rider_id, owner_id, vehicle_id,
			pickup_lat_enc, pickup_lng_enc, pickup_label,
			dropoff_lat_enc, dropoff_lng_enc, dropoff_label, status
		) VALUES ($1, 'crider0539', 'cowner0539', 'cveh0539',
			'enc', 'enc', 'Home', 'enc', 'enc', 'Work', 'enroute')`, rideID); err != nil {
		t.Fatalf("seed ride: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_stops (id, ride_id, position, lat_enc, lng_enc, label)
		 VALUES ('crs0539a', $1, 0, 'enc', 'enc', 'Bakery')`, rideID); err != nil {
		t.Fatalf("seed stop: %v", err)
	}

	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests WHERE id = $1`, rideID); err != nil {
		t.Fatalf("delete ride: %v", err)
	}
	var left int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_ride_stops WHERE ride_id = $1`, rideID).Scan(&left); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if left != 0 {
		t.Errorf("%d stop row(s) outlived their ride; the retention sweep would never remove them", left)
	}
}
