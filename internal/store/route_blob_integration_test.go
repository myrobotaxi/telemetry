package store_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/routeblob"
)

// silentRouteBlobLogger keeps deliberate decrypt-failure warnings out
// of test output. Same pattern as silentGPSLogger in vehicle_repo_gps_test.go.
func silentRouteBlobLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readNavRouteShadows pulls the (plaintext, ciphertext) pair for one
// VIN directly via SQL so tests can assert on the exact column shapes
// the dual-write path produced. Bypasses VehicleRepo so a Read-fallback
// test can't be fooled by the same Read code that wrote the row.
func readNavRouteShadows(t *testing.T, pool *pgxpool.Pool, vin string) (plain json.RawMessage, ct *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT "navRouteCoordinates", "navRouteCoordinatesEnc" FROM "Vehicle" WHERE "vin" = $1`, vin,
	).Scan(&plain, &ct); err != nil {
		t.Fatalf("readNavRouteShadows: %v", err)
	}
	return plain, ct
}

// TestVehicleRepo_NavRoute_DualWriteOnUpdate exercises the happy path:
// an UPDATE through the encryption-aware repo writes BOTH the
// plaintext jsonb column and the navRouteCoordinatesEnc shadow, and a
// subsequent GetByVIN returns the encrypted-then-decrypted value.
func TestVehicleRepo_NavRoute_DualWriteOnUpdate(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_001", "5YJ3E1EA1NF00NAV1")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	rawCoords := json.RawMessage(`[[-96.80,33.10],[-96.81,33.11],[-96.82,33.12]]`)
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00NAV1", store.VehicleUpdate{
		NavRouteCoordinates: &rawCoords,
		LastUpdated:         time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	plain, ct := readNavRouteShadows(t, testPool, "5YJ3E1EA1NF00NAV1")
	if string(plain) != string(rawCoords) {
		t.Errorf("plaintext = %s, want %s", plain, rawCoords)
	}
	if ct == nil || *ct == "" {
		t.Fatal("navRouteCoordinatesEnc not written")
	}
	got, err := routeblob.DecryptNavRoute(*ct, enc)
	if err != nil {
		t.Fatalf("DecryptNavRoute: %v", err)
	}
	if len(got) != 3 || got[0] != [2]float64{-96.80, 33.10} {
		t.Errorf("decoded = %v", got)
	}

	// Read path returns the decrypted shape.
	readBack, err := repo.GetByVIN(ctx, "5YJ3E1EA1NF00NAV1")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if string(readBack.NavRouteCoordinates) != string(rawCoords) {
		t.Errorf("read = %s, want %s", readBack.NavRouteCoordinates, rawCoords)
	}
}

// TestVehicleRepo_NavRoute_PrefersCiphertextOnRead seeds STALE
// plaintext alongside encrypted "real" coordinates and verifies the
// read returns the encrypted shape — proving the preference is real.
func TestVehicleRepo_NavRoute_PrefersCiphertextOnRead(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_002", "5YJ3E1EA1NF00NAV2")

	enc := newTestEncryptor(t)
	stale := json.RawMessage(`[[1,2]]`)
	realRaw := []byte(`[[-96.80,33.10]]`)
	realCT, err := enc.EncryptString(string(realRaw))
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`UPDATE "Vehicle" SET "navRouteCoordinates"=$1::jsonb, "navRouteCoordinatesEnc"=$2 WHERE "vin"=$3`,
		stale, realCT, "5YJ3E1EA1NF00NAV2"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	got, err := repo.GetByVIN(context.Background(), "5YJ3E1EA1NF00NAV2")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if string(got.NavRouteCoordinates) != string(realRaw) {
		t.Errorf("read = %s, want ciphertext-resolved %s", got.NavRouteCoordinates, realRaw)
	}
}

// TestVehicleRepo_NavRoute_DecryptFailureFallsBackToPlaintext seeds a
// corrupt ciphertext (non-base64 garbage) alongside valid plaintext.
// The read MUST return the plaintext rather than 500 the request.
func TestVehicleRepo_NavRoute_DecryptFailureFallsBackToPlaintext(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_003", "5YJ3E1EA1NF00NAV3")

	plain := json.RawMessage(`[[-96.80,33.10]]`)
	corruptCT := "not-base64-at-all"
	if _, err := testPool.Exec(context.Background(),
		`UPDATE "Vehicle" SET "navRouteCoordinates"=$1::jsonb, "navRouteCoordinatesEnc"=$2 WHERE "vin"=$3`,
		plain, corruptCT, "5YJ3E1EA1NF00NAV3"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, newTestEncryptor(t), silentRouteBlobLogger())
	got, err := repo.GetByVIN(context.Background(), "5YJ3E1EA1NF00NAV3")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if string(got.NavRouteCoordinates) != string(plain) {
		t.Errorf("read = %s, want plaintext fallback %s", got.NavRouteCoordinates, plain)
	}
}

// TestVehicleRepo_NavRoute_ClearAlsoClearsShadow verifies that a
// ClearFields=['navRouteCoordinates'] entry NULLs both the plaintext
// JSON column AND its *Enc shadow — otherwise navigation cancellation
// would leave a NULL plaintext + stale ciphertext.
func TestVehicleRepo_NavRoute_ClearAlsoClearsShadow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_004", "5YJ3E1EA1NF00NAV4")

	enc := newTestEncryptor(t)
	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	// Seed populated route.
	raw := json.RawMessage(`[[-96.80,33.10]]`)
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00NAV4", store.VehicleUpdate{
		NavRouteCoordinates: &raw,
		LastUpdated:         time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry seed: %v", err)
	}

	// Clear navigation.
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00NAV4", store.VehicleUpdate{
		ClearFields: []string{"navRouteCoordinates"},
		LastUpdated: time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry clear: %v", err)
	}

	plain, ct := readNavRouteShadows(t, testPool, "5YJ3E1EA1NF00NAV4")
	if plain != nil {
		t.Errorf("navRouteCoordinates not cleared: %s", plain)
	}
	if ct != nil {
		t.Errorf("navRouteCoordinatesEnc not cleared: %v", ct)
	}
}

// TestVehicleRepo_NavRoute_LegacyConstructorSkipsDualWrite asserts the
// legacy NewVehicleRepo constructor (no encryptor) writes only the
// plaintext column and reads only plaintext, leaving the *Enc column
// untouched. Lets old callers keep compiling.
func TestVehicleRepo_NavRoute_LegacyConstructorSkipsDualWrite(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_005", "5YJ3E1EA1NF00NAV5")

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	raw := json.RawMessage(`[[-96.80,33.10]]`)
	if err := repo.UpdateTelemetry(ctx, "5YJ3E1EA1NF00NAV5", store.VehicleUpdate{
		NavRouteCoordinates: &raw,
		LastUpdated:         time.Now(),
	}); err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	_, ct := readNavRouteShadows(t, testPool, "5YJ3E1EA1NF00NAV5")
	if ct != nil {
		t.Errorf("legacy repo wrote *Enc shadow: %v", ct)
	}
}

// TestDriveRepo_RoutePoints_DualWriteOnAppend verifies AppendRoutePoints
// concatenates the plaintext array AND re-encrypts the full array into
// routePointsEnc.
func TestDriveRepo_RoutePoints_DualWriteOnAppend(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_drv_001", "5YJ3E1EA1NF00DRV1")

	enc := newTestEncryptor(t)
	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	if err := repo.Create(ctx, store.DriveRecord{
		ID: "drv1", VehicleID: "veh_drv_001",
		Date: "2026-05-09", StartTime: "2026-05-09T12:00:00Z",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pts := []store.RoutePointRecord{
		{Latitude: 33.1, Longitude: -96.8, Speed: 35, Heading: 90, Timestamp: "2026-05-09T12:00:01Z"},
		{Latitude: 33.2, Longitude: -96.9, Speed: 36, Heading: 91, Timestamp: "2026-05-09T12:00:02Z"},
	}
	if err := repo.AppendRoutePoints(ctx, "drv1", pts); err != nil {
		t.Fatalf("AppendRoutePoints: %v", err)
	}

	// Plaintext jsonb has two points.
	var rawArr json.RawMessage
	var ct *string
	if err := testPool.QueryRow(ctx,
		`SELECT "routePoints", "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "drv1",
	).Scan(&rawArr, &ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if string(rawArr) == "[]" {
		t.Errorf("routePoints jsonb empty after append")
	}
	if ct == nil || *ct == "" {
		t.Fatal("routePointsEnc not written")
	}
	got, err := routeblob.DecryptRoutePoints(*ct, enc)
	if err != nil {
		t.Fatalf("DecryptRoutePoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded points = %d, want 2", len(got))
	}
	if got[0].Latitude != 33.1 || got[1].Longitude != -96.9 {
		t.Errorf("decoded = %+v", got)
	}

	// GetByID prefers ciphertext.
	d, err := repo.GetByID(ctx, "drv1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if string(d.RoutePoints) == "[]" {
		t.Errorf("GetByID returned empty routePoints")
	}
}

// TestDriveRepo_RoutePoints_AppendIsIncremental verifies a second
// AppendRoutePoints call accumulates onto the prior shadow rather than
// overwriting.
func TestDriveRepo_RoutePoints_AppendIsIncremental(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_drv_002", "5YJ3E1EA1NF00DRV2")

	enc := newTestEncryptor(t)
	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	if err := repo.Create(ctx, store.DriveRecord{
		ID: "drv2", VehicleID: "veh_drv_002",
		Date: "2026-05-09", StartTime: "2026-05-09T12:00:00Z",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := []store.RoutePointRecord{{Latitude: 1, Longitude: 2, Speed: 3, Heading: 4, Timestamp: "t1"}}
	second := []store.RoutePointRecord{{Latitude: 5, Longitude: 6, Speed: 7, Heading: 8, Timestamp: "t2"}}
	if err := repo.AppendRoutePoints(ctx, "drv2", first); err != nil {
		t.Fatalf("AppendRoutePoints first: %v", err)
	}
	if err := repo.AppendRoutePoints(ctx, "drv2", second); err != nil {
		t.Fatalf("AppendRoutePoints second: %v", err)
	}

	var ct *string
	if err := testPool.QueryRow(ctx,
		`SELECT "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "drv2",
	).Scan(&ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	got, err := routeblob.DecryptRoutePoints(*ct, enc)
	if err != nil {
		t.Fatalf("DecryptRoutePoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d points after two appends, want 2", len(got))
	}
	if got[0].Latitude != 1 || got[1].Latitude != 5 {
		t.Errorf("incremental append wrong: %+v", got)
	}
}

// TestDriveRepo_RoutePoints_PlaintextFallbackOnDecryptFailure: a
// corrupt ciphertext returns the plaintext jsonb on read.
func TestDriveRepo_RoutePoints_PlaintextFallbackOnDecryptFailure(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_drv_003", "5YJ3E1EA1NF00DRV3")

	enc := newTestEncryptor(t)
	repo := store.NewDriveRepoWithEncryption(testPool, store.NoopMetrics{}, enc, silentRouteBlobLogger())
	ctx := context.Background()

	if err := repo.Create(ctx, store.DriveRecord{
		ID: "drv3", VehicleID: "veh_drv_003",
		Date: "2026-05-09", StartTime: "2026-05-09T12:00:00Z",
		RoutePoints: json.RawMessage(`[{"lat":1,"lng":2,"speed":3,"heading":4,"timestamp":"seed"}]`),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Stomp the shadow with corrupt data.
	if _, err := testPool.Exec(ctx,
		`UPDATE "Drive" SET "routePointsEnc" = $1 WHERE "id" = $2`,
		"not-base64", "drv3"); err != nil {
		t.Fatalf("stomp: %v", err)
	}

	d, err := repo.GetByID(ctx, "drv3")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	// Plaintext seeded above must be what GetByID returns.
	if string(d.RoutePoints) == "" || string(d.RoutePoints) == "[]" {
		t.Errorf("expected plaintext fallback, got %s", d.RoutePoints)
	}
}

// TestDriveRepo_RoutePoints_LegacyConstructorPlaintextOnly verifies
// the legacy NewDriveRepo path leaves the *Enc column NULL on every
// write and reads only plaintext.
func TestDriveRepo_RoutePoints_LegacyConstructorPlaintextOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_drv_004", "5YJ3E1EA1NF00DRV4")

	repo := store.NewDriveRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	if err := repo.Create(ctx, store.DriveRecord{
		ID: "drv4", VehicleID: "veh_drv_004",
		Date: "2026-05-09", StartTime: "2026-05-09T12:00:00Z",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.AppendRoutePoints(ctx, "drv4", []store.RoutePointRecord{
		{Latitude: 1, Longitude: 2, Speed: 3, Heading: 4, Timestamp: "t1"},
	}); err != nil {
		t.Fatalf("AppendRoutePoints: %v", err)
	}

	var ct *string
	if err := testPool.QueryRow(ctx,
		`SELECT "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "drv4",
	).Scan(&ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ct != nil {
		t.Errorf("legacy repo wrote *Enc shadow: %v", ct)
	}
}
