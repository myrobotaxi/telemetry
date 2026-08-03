package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Drive retention pruner tests (MYR-439, NFR-3.27, data-lifecycle.md §5).
//
// The boundary cases are expressed in terms of store.DriveRetentionWindow
// rather than a literal 365 days, so that changing the contract constant makes
// these tests follow it instead of failing — the constant is the single source
// of truth for the tests too.

// prunerRoutePoints is a stand-in GPS trail: the P1 payload whose destruction
// is the entire point of the feature.
const prunerRoutePoints = `[{"lat":37.7749,"lng":-122.4194,"speed":31,"heading":90,"timestamp":"2025-01-01T00:00:00Z"}]`

// prunerRouteCiphertext stands in for the AES-256-GCM shadow (MYR-64).
const prunerRouteCiphertext = "v1:ZmFrZS1jaXBoZXJ0ZXh0"

func setupPruner(t *testing.T) *store.DrivePruner {
	t.Helper()
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping drive pruner integration test")
	}
	ensureAuditSchema(t)
	cleanPrunerTables(t)
	return store.NewDrivePruner(testPool, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

func cleanPrunerTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// "AuditLog" is intentionally omitted — it is append-only, so a DELETE
	// raises. Every audit assertion below filters by a per-test-unique userId.
	for _, tbl := range []string{`"Drive"`, `"Vehicle"`} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func seedPrunerVehicle(t *testing.T, id, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","userId","vin","name","status") VALUES ($1,$2,$1,'Test','parked')`,
		id, userID); err != nil {
		t.Fatalf("seed vehicle %s: %v", id, err)
	}
}

// seedPrunerDrive inserts a drive aged `age` old, carrying BOTH a plaintext GPS
// trail and its ciphertext shadow, so every assertion about "deleted whole" has
// something real to be about.
func seedPrunerDrive(t *testing.T, id, vehicleID string, age time.Duration) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive"
		 ("id","vehicleId","date","startTime","endTime","startAddress","endAddress",
		  "routePoints","routePointsEnc","createdAt")
		 VALUES ($1,$2,'2026-07-24','10:00','10:30','1 Main St','2 Oak Ave',
		  $3::jsonb,$4,NOW() - make_interval(secs => $5))`,
		id, vehicleID, prunerRoutePoints, prunerRouteCiphertext, age.Seconds()); err != nil {
		t.Fatalf("seed drive %s: %v", id, err)
	}
}

func driveExists(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM "Drive" WHERE "id" = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("drive exists check: %v", err)
	}
	return ok
}

// TestPruneBatchRetentionBoundary is the headline test: the day either side of
// the window, and the whole row going with it.
func TestPruneBatchRetentionBoundary(t *testing.T) {
	pruner := setupPruner(t)
	ctx := context.Background()

	const userID = "user-prune-boundary"
	const vehicleID = "veh-prune-boundary"
	seedPrunerVehicle(t, vehicleID, userID)

	day := 24 * time.Hour
	seedPrunerDrive(t, "drive-364d", vehicleID, store.DriveRetentionWindow-day)
	seedPrunerDrive(t, "drive-366d", vehicleID, store.DriveRetentionWindow+day)

	res, err := pruner.PruneBatch(ctx, store.DefaultPruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.DrivesDeleted != 1 {
		t.Errorf("DrivesDeleted = %d, want 1 (only the 366d drive)", res.DrivesDeleted)
	}

	t.Run("in-window drive survives whole", func(t *testing.T) {
		var routePoints, routeEnc string
		var startAddr, endAddr string
		err := testPool.QueryRow(ctx,
			`SELECT "routePoints"::text, "routePointsEnc", "startAddress", "endAddress"
			 FROM "Drive" WHERE "id" = 'drive-364d'`).
			Scan(&routePoints, &routeEnc, &startAddr, &endAddr)
		if err != nil {
			t.Fatalf("read surviving drive: %v", err)
		}
		// Not merely present — UNTOUCHED. A pruner that blanked the trail of an
		// in-window drive would pass a bare existence check.
		if routeEnc != prunerRouteCiphertext {
			t.Errorf("routePointsEnc = %q, want the seeded ciphertext intact", routeEnc)
		}
		if startAddr == "" || endAddr == "" {
			t.Error("surviving drive lost its addresses")
		}
		var points []map[string]any
		if err := json.Unmarshal([]byte(routePoints), &points); err != nil {
			t.Fatalf("surviving routePoints is not valid JSON: %v", err)
		}
		if len(points) != 1 {
			t.Errorf("surviving routePoints has %d points, want 1", len(points))
		}
	})

	t.Run("expired drive is deleted whole", func(t *testing.T) {
		if driveExists(t, "drive-366d") {
			t.Fatal("366d drive still present after prune")
		}
		// The GPS trail is two columns ON the row, so "no orphaned blob" is the
		// claim that no row anywhere still carries this drive's data. Asserted
		// as a row count across the only table keyed by a drive — there is no
		// second one, which is exactly what the PR's table map documents.
		var trails int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM "Drive" WHERE "id" = 'drive-366d' AND "routePoints" <> '[]'::jsonb`).
			Scan(&trails); err != nil {
			t.Fatalf("orphan trail count: %v", err)
		}
		if trails != 0 {
			t.Errorf("orphaned route trail rows = %d, want 0", trails)
		}
	})
}

// TestPruneBatchWritesGroupedAudit covers CG-DL-3 and §5.5's grouping.
func TestPruneBatchWritesGroupedAudit(t *testing.T) {
	pruner := setupPruner(t)
	ctx := context.Background()

	const userID = "user-prune-audit"
	seedPrunerVehicle(t, "veh-audit-a", userID)
	seedPrunerVehicle(t, "veh-audit-b", userID)
	age := store.DriveRetentionWindow + 48*time.Hour
	seedPrunerDrive(t, "drive-audit-a1", "veh-audit-a", age)
	seedPrunerDrive(t, "drive-audit-a2", "veh-audit-a", age+time.Hour)
	seedPrunerDrive(t, "drive-audit-b1", "veh-audit-b", age)

	res, err := pruner.PruneBatch(ctx, store.DefaultPruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.DrivesDeleted != 3 {
		t.Errorf("DrivesDeleted = %d, want 3", res.DrivesDeleted)
	}
	// Three drives across two cars => two audit rows, not three and not one.
	if res.AuditRows != 2 {
		t.Errorf("AuditRows = %d, want 2 (one per vehicle)", res.AuditRows)
	}

	rows, err := testPool.Query(ctx,
		`SELECT "action","targetType","targetId","initiator","metadata"::text
		 FROM "AuditLog" WHERE "userId" = $1 ORDER BY "targetId"`, userID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()

	type auditRow struct{ action, targetType, targetID, initiator, metadata string }
	var got []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.action, &a.targetType, &a.targetID, &a.initiator, &a.metadata); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(got))
	}

	for _, a := range got {
		if a.action != "drives_pruned" {
			t.Errorf("action = %q, want drives_pruned", a.action)
		}
		if a.targetType != "drive" {
			t.Errorf("targetType = %q, want drive", a.targetType)
		}
		if a.initiator != "system_pruner" {
			t.Errorf("initiator = %q, want system_pruner", a.initiator)
		}
		// CG-DL-5: P0 only. The metadata must carry counts and a window, and
		// must NOT have picked up anything from the P1 columns it deleted.
		var meta map[string]any
		if err := json.Unmarshal([]byte(a.metadata), &meta); err != nil {
			t.Fatalf("audit metadata is not JSON: %v", err)
		}
		for _, key := range []string{"driveCount", "oldestDriveDate", "newestDriveDate"} {
			if _, ok := meta[key]; !ok {
				t.Errorf("audit metadata missing %q: %s", key, a.metadata)
			}
		}
		if len(meta) != 3 {
			t.Errorf("audit metadata has %d keys, want exactly 3 (P0 only): %s", len(meta), a.metadata)
		}
	}
	if got[0].targetID != "veh-audit-a" || got[1].targetID != "veh-audit-b" {
		t.Errorf("targetIds = %q/%q, want the two vehicle ids", got[0].targetID, got[1].targetID)
	}
	// The grouped row for vehicle a covers both of its drives.
	var metaA map[string]any
	if err := json.Unmarshal([]byte(got[0].metadata), &metaA); err != nil {
		t.Fatalf("unmarshal metadata a: %v", err)
	}
	if count, _ := metaA["driveCount"].(float64); count != 2 {
		t.Errorf("vehicle a driveCount = %v, want 2", metaA["driveCount"])
	}
}

// TestPruneBatchConvergesOverBatches proves the LIMIT loop: a backlog larger
// than one batch drains in successive bounded batches, and the last one reports
// Exhausted so the pass loop terminates.
func TestPruneBatchConvergesOverBatches(t *testing.T) {
	pruner := setupPruner(t)
	ctx := context.Background()

	const userID = "user-prune-batching"
	const vehicleID = "veh-prune-batching"
	seedPrunerVehicle(t, vehicleID, userID)

	const batchSize = 10
	const seeded = 25 // deliberately not a multiple of the batch size
	age := store.DriveRetentionWindow + time.Hour
	for i := range seeded {
		seedPrunerDrive(t, fmt.Sprintf("drive-batch-%02d", i), vehicleID, age+time.Duration(i)*time.Minute)
	}
	// One in-window drive, to prove the loop stops at the boundary rather than
	// at "no rows left".
	seedPrunerDrive(t, "drive-batch-keep", vehicleID, store.DriveRetentionWindow-time.Hour)

	var totalDeleted, batches int
	for {
		res, err := pruner.PruneBatch(ctx, batchSize)
		if err != nil {
			t.Fatalf("PruneBatch (batch %d): %v", batches, err)
		}
		batches++
		totalDeleted += res.DrivesDeleted
		if res.DrivesDeleted > batchSize {
			t.Fatalf("batch %d deleted %d rows, exceeding the %d cap", batches, res.DrivesDeleted, batchSize)
		}
		if res.Exhausted {
			break
		}
		if batches > 10 {
			t.Fatal("prune loop did not converge")
		}
	}

	if totalDeleted != seeded {
		t.Errorf("total deleted = %d, want %d", totalDeleted, seeded)
	}
	// 25 rows at 10 per batch: 10, 10, 5, then a FOURTH call that claims nothing
	// and reports Exhausted. The short third batch deliberately does NOT
	// terminate the loop — under SKIP LOCKED "short" means "no rows I can
	// claim", which a peer holding the rest also satisfies.
	if batches != 4 {
		t.Errorf("batches = %d, want 4 (three deleting batches + the confirming empty claim)", batches)
	}
	if !driveExists(t, "drive-batch-keep") {
		t.Error("in-window drive was deleted by the batch loop")
	}
}

// TestPruneBatchEmptyIsCleanNoop covers the steady state: most nights there is
// nothing to do, and that must cost one query and no audit row.
func TestPruneBatchEmptyIsCleanNoop(t *testing.T) {
	pruner := setupPruner(t)
	ctx := context.Background()

	const userID = "user-prune-noop"
	const vehicleID = "veh-prune-noop"
	seedPrunerVehicle(t, vehicleID, userID)
	seedPrunerDrive(t, "drive-noop-fresh", vehicleID, time.Hour)

	res, err := pruner.PruneBatch(ctx, store.DefaultPruneBatchSize)
	if err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}
	if res.DrivesDeleted != 0 || res.AuditRows != 0 {
		t.Errorf("empty prune deleted %d drives / wrote %d audit rows, want 0/0", res.DrivesDeleted, res.AuditRows)
	}
	if !res.Exhausted {
		t.Error("empty prune did not report Exhausted")
	}
	if n := countQuery(t, `SELECT count(*) FROM "AuditLog" WHERE "userId" = $1`, userID); n != 0 {
		t.Errorf("audit rows = %d, want 0 — a no-op prune must not write one", n)
	}
}

// TestPruneBatchRejectsNonPositiveBatchSize guards the programming-error path.
func TestPruneBatchRejectsNonPositiveBatchSize(t *testing.T) {
	pruner := setupPruner(t)
	for _, size := range []int{0, -1} {
		if _, err := pruner.PruneBatch(context.Background(), size); err == nil {
			t.Errorf("PruneBatch(%d) returned nil error, want a rejection", size)
		}
	}
}

// TestPrunedDriveReadsDegradeGracefully is the owner-facing half of the ticket:
// a drive id held by a client (a cached WS drive_ended payload, a deep link)
// that has since been pruned must 404 through the normal not-found path, never
// error differently.
func TestPrunedDriveReadsDegradeGracefully(t *testing.T) {
	pruner := setupPruner(t)
	ctx := context.Background()

	const userID = "user-prune-reads"
	const vehicleID = "veh-prune-reads"
	seedPrunerVehicle(t, vehicleID, userID)
	seedPrunerDrive(t, "drive-read-gone", vehicleID, store.DriveRetentionWindow+time.Hour)
	seedPrunerDrive(t, "drive-read-kept", vehicleID, time.Hour)

	if _, err := pruner.PruneBatch(ctx, store.DefaultPruneBatchSize); err != nil {
		t.Fatalf("PruneBatch: %v", err)
	}

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})

	t.Run("pruned id yields ErrDriveNotFound", func(t *testing.T) {
		_, err := repo.GetByID(ctx, "drive-read-gone")
		if !errors.Is(err, store.ErrDriveNotFound) {
			t.Fatalf("GetByID(pruned) error = %v, want ErrDriveNotFound", err)
		}
	})

	t.Run("in-window drive still lists", func(t *testing.T) {
		page, err := repo.ListByVehicleID(ctx, vehicleID, store.DriveListCursor{}, 10)
		if err != nil {
			t.Fatalf("ListByVehicleID: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("list returned %d drives, want 1", len(page.Items))
		}
		if page.Items[0].ID != "drive-read-kept" {
			t.Errorf("listed drive = %q, want drive-read-kept", page.Items[0].ID)
		}
	})
}
