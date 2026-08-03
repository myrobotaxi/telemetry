package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Concurrency shape for the lost-update test: 8 writers, 5 appends each,
// 40 uniquely identifiable points in total.
const (
	concurrentAppendWriters    = 8
	concurrentAppendIterations = 5
)

// concurrentAppendPointID encodes the (writer, iteration) pair that
// produced a point. It is carried in Timestamp so a dropped point can be
// named in the failure output rather than only counted.
func concurrentAppendPointID(writer, iteration int) string {
	return fmt.Sprintf("w%02d-i%02d", writer, iteration)
}

// concurrentAppendSpeed is the same pair encoded numerically. Checked on
// read-back so a point that survives with a scrambled body — rather than
// simply going missing — is also caught.
func concurrentAppendSpeed(writer, iteration int) float64 {
	return float64(writer*100 + iteration)
}

// readTrailPoints reads the drive back through the repo and decodes the
// decrypted trail into route points.
func readTrailPoints(t *testing.T, repo *store.DriveRepo, driveID string) []store.RoutePointRecord {
	t.Helper()
	d, err := repo.GetByID(context.Background(), driveID)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", driveID, err)
	}
	if len(d.RoutePoints) == 0 {
		t.Fatalf("GetByID(%s): empty trail — every append was lost", driveID)
	}
	var points []store.RoutePointRecord
	if err := json.Unmarshal(d.RoutePoints, &points); err != nil {
		t.Fatalf("decode trail: %v (raw=%s)", err, d.RoutePoints)
	}
	return points
}

// summarizeIDs renders at most `limit` identifiers for a failure message,
// sorted so the output is stable across runs.
func summarizeIDs(ids []string, limit int) string {
	sort.Strings(ids)
	if len(ids) > limit {
		return strings.Join(ids[:limit], ", ") + fmt.Sprintf(", … (+%d more)", len(ids)-limit)
	}
	return strings.Join(ids, ", ")
}

// TestDriveRepo_AppendRoutePoints_ConcurrentAppendsLoseNoPoints proves the
// row lock in AppendRoutePoints is load-bearing.
//
// The pre-MYR-433 append was an in-database `jsonb ||` concat, which was
// atomic for free. Ciphertext cannot be concatenated in SQL, so the append
// is now decrypt → append in Go → re-seal → write, and the only thing
// keeping two concurrent flushes for one drive from clobbering each other
// is the `FOR UPDATE` in queryDriveLockRoutePointsEnc: without it both
// transactions decrypt the same trail and whichever commits second writes
// a trail that never saw the first one's points. Nothing else in the suite
// notices that — every other route-point test appends sequentially, so
// deleting the lock leaves them all green.
//
// This test contends deliberately: 8 goroutines released at once, 5
// appends each, every point tagged with the (writer, iteration) pair that
// produced it. The assertion is on the SET of those pairs, not just the
// count, so a duplicated point cannot paper over a lost one.
func TestDriveRepo_AppendRoutePoints_ConcurrentAppendsLoseNoPoints(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_drv_conc", "5YJ3E1EA1NF00CONC")

	enc := newTestEncryptor(t)
	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	const driveID = "drv_concurrent"
	if err := repo.Create(ctx, store.DriveRecord{
		ID: driveID, VehicleID: "veh_drv_conc",
		Date: "2026-05-09", StartTime: "2026-05-09T12:00:00Z",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One error slot per writer: written by its own goroutine only, so the
	// collection needs no lock of its own (and stays clean under -race).
	writerErrs := make([]error, concurrentAppendWriters)

	release := make(chan struct{})
	var wg sync.WaitGroup
	for w := range concurrentAppendWriters {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			<-release // all writers enter the read-modify-write window together
			for i := range concurrentAppendIterations {
				pt := store.RoutePointRecord{
					Latitude:  33.0 + float64(writer)/100,
					Longitude: -96.0 - float64(i)/100,
					Speed:     concurrentAppendSpeed(writer, i),
					Heading:   float64((writer*10 + i) % 360),
					Timestamp: concurrentAppendPointID(writer, i),
				}
				if err := repo.AppendRoutePoints(ctx, driveID, []store.RoutePointRecord{pt}); err != nil {
					writerErrs[writer] = fmt.Errorf("writer %d iteration %d: %w", writer, i, err)
					return
				}
			}
		}(w)
	}
	close(release)
	wg.Wait()

	for _, err := range writerErrs {
		if err != nil {
			t.Fatalf("AppendRoutePoints failed: %v", err)
		}
	}

	points := readTrailPoints(t, repo, driveID)

	seen := make(map[string]int, len(points))
	for _, p := range points {
		seen[p.Timestamp]++
	}

	wantTotal := concurrentAppendWriters * concurrentAppendIterations
	var missing, duplicated []string
	for w := range concurrentAppendWriters {
		for i := range concurrentAppendIterations {
			id := concurrentAppendPointID(w, i)
			switch n := seen[id]; {
			case n == 0:
				missing = append(missing, id)
			case n > 1:
				duplicated = append(duplicated, fmt.Sprintf("%s×%d", id, n))
			}
		}
	}

	if len(missing) > 0 {
		t.Errorf("LOST UPDATE: %d of %d points are missing from the trail — "+
			"concurrent appends overwrote each other. Missing: %s. "+
			"This is exactly what the `FOR UPDATE` row lock in "+
			"queryDriveLockRoutePointsEnc exists to prevent: without it two "+
			"flushes decrypt the same trail and the later write drops the "+
			"earlier one's points.",
			len(missing), wantTotal, summarizeIDs(missing, 10))
	}
	if len(duplicated) > 0 {
		t.Errorf("trail contains duplicated points (%d ids): %s — an append "+
			"was applied twice", len(duplicated), summarizeIDs(duplicated, 10))
	}
	if len(points) != wantTotal {
		t.Errorf("trail has %d points, want %d (%d writers × %d appends)",
			len(points), wantTotal, concurrentAppendWriters, concurrentAppendIterations)
	}

	// Bodies must survive the merge intact, not just the identifiers.
	for _, p := range points {
		var w, i int
		if _, err := fmt.Sscanf(p.Timestamp, "w%d-i%d", &w, &i); err != nil {
			t.Errorf("unrecognized point in trail: %+v", p)
			continue
		}
		if want := concurrentAppendSpeed(w, i); p.Speed != want {
			t.Errorf("point %s has Speed %v, want %v — the merge corrupted a point body",
				p.Timestamp, p.Speed, want)
		}
	}
}
