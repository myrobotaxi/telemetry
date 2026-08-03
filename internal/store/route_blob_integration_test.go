package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// jsonEqual compares two JSON byte slices semantically, ignoring
// whitespace and key ordering. PostgreSQL's `jsonb` round-trip
// reformats array literals (`[[-1,2]]` → `[[-1, 2]]`) so a byte-level
// equality check would fail spuriously after a Vehicle/Drive read.
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Errorf("jsonEqual got: %v (raw=%s)", err, got)
		return false
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Errorf("jsonEqual want: %v (raw=%s)", err, want)
		return false
	}
	return reflect.DeepEqual(a, b)
}

// silentRouteBlobLogger keeps deliberate decrypt-failure warnings out
// of test output. Same pattern as silentGPSLogger in vehicle_repo_gps_test.go.
func silentRouteBlobLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// readNavRouteShadows pulls the (plaintext, ciphertext) pair for one VIN
// directly via SQL. After MYR-433 the plaintext column is neither written
// nor selected by VehicleRepo, so raw SQL is the only way to prove it
// stayed empty — reading through the repo could not distinguish "never
// written" from "not projected".
func readNavRouteShadows(t *testing.T, pool *pgxpool.Pool, vin string) (plain json.RawMessage, ct *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT "navRouteCoordinates", "navRouteCoordinatesEnc" FROM "Vehicle" WHERE "vin" = $1`, vin,
	).Scan(&plain, &ct); err != nil {
		t.Fatalf("readNavRouteShadows: %v", err)
	}
	return plain, ct
}

// TestVehicleRepo_NavRoute_WritesCiphertextOnly exercises the happy
// path: an UPDATE through the encryption-aware repo writes
// navRouteCoordinatesEnc and leaves the plaintext jsonb column NULL, and
// a subsequent GetByVIN returns the encrypted-then-decrypted value.
//
// A nav route is a polyline of where the driver is about to go. MYR-433
// made the ciphertext the only copy, so the assertion that the plaintext
// column stays NULL is the one carrying the security property — the
// decrypt round trip alone would still pass if a readable copy were being
// deposited beside it.
func TestVehicleRepo_NavRoute_WritesCiphertextOnly(t *testing.T) {
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
	if plain != nil {
		t.Errorf("navRouteCoordinates plaintext = %s, want NULL — the server must not write a readable copy", plain)
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
	if !jsonEqual(t, readBack.NavRouteCoordinates, rawCoords) {
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
	if !jsonEqual(t, got.NavRouteCoordinates, realRaw) {
		t.Errorf("read = %s, want ciphertext-resolved %s", got.NavRouteCoordinates, realRaw)
	}
}

// TestVehicleRepo_NavRoute_DecryptFailureYieldsNoRoute seeds a corrupt
// ciphertext (non-base64 garbage) alongside a valid plaintext decoy.
//
// The read must degrade to NO route: not an error (a key-rotation slip
// must not 500 every snapshot), and emphatically not the plaintext. The
// pre-MYR-433 fallback is what this inverts — a read path willing to use
// the plaintext column is a read path that requires the plaintext column
// to remain readable, which is the leak the issue closes. The decoy below
// is deliberately valid so a resurrected fallback fails loudly here.
func TestVehicleRepo_NavRoute_DecryptFailureYieldsNoRoute(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanTables(t, testPool)
	seedVehicle(t, testPool, "veh_nav_003", "5YJ3E1EA1NF00NAV3")

	decoy := json.RawMessage(`[[-96.80,33.10]]`)
	corruptCT := "not-base64-at-all"
	if _, err := testPool.Exec(context.Background(),
		`UPDATE "Vehicle" SET "navRouteCoordinates"=$1::jsonb, "navRouteCoordinatesEnc"=$2 WHERE "vin"=$3`,
		decoy, corruptCT, "5YJ3E1EA1NF00NAV3"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	repo := store.NewVehicleRepoWithEncryption(testPool, store.NoopMetrics{}, newTestEncryptor(t), silentRouteBlobLogger())
	got, err := repo.GetByVIN(context.Background(), "5YJ3E1EA1NF00NAV3")
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	if len(got.NavRouteCoordinates) != 0 && string(got.NavRouteCoordinates) != "null" {
		t.Errorf("read = %s, want no route — an undecryptable blob must not fall back to the plaintext decoy %s",
			got.NavRouteCoordinates, decoy)
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
// legacy NewVehicleRepo constructor (no encryptor) leaves the *Enc column
// untouched. It exists so callers with no Encryptor in scope keep
// compiling; after MYR-433 that means they persist no route at all rather
// than persisting a readable one, because the plaintext column dropped
// out of the write path along with the read path.
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

// TestDriveRepo_RoutePoints_AppendWritesCiphertextOnly verifies that
// AppendRoutePoints lands the trail in routePointsEnc and leaves the
// plaintext routePoints column at the literal '[]' the INSERT seeded it
// with.
//
// The drive trail is the most sensitive non-credential data in this
// database — a minute-by-minute record of where somebody drove — and
// MYR-433's acceptance bar was that an operator with a database dump
// cannot read it. The '[]' assertion is what enforces that: the column
// still exists (Prisma declares it NOT NULL) but must never accumulate
// points again.
func TestDriveRepo_RoutePoints_AppendWritesCiphertextOnly(t *testing.T) {
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

	// Plaintext jsonb stays at the '[]' the INSERT wrote.
	var rawArr json.RawMessage
	var ct *string
	if err := testPool.QueryRow(ctx,
		`SELECT "routePoints", "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "drv1",
	).Scan(&rawArr, &ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !jsonEqual(t, rawArr, []byte(`[]`)) {
		t.Errorf("routePoints jsonb = %s, want [] — the append must not deposit a readable trail", rawArr)
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

	// GetByID reconstructs the trail from the ciphertext alone.
	d, err := repo.GetByID(ctx, "drv1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	var readBack []store.RoutePointRecord
	if err := json.Unmarshal(d.RoutePoints, &readBack); err != nil {
		t.Fatalf("GetByID routePoints unmarshal: %v (raw=%s)", err, d.RoutePoints)
	}
	if len(readBack) != 2 {
		t.Fatalf("GetByID routePoints = %d points, want 2 (decrypted from routePointsEnc)", len(readBack))
	}
	if readBack[0].Latitude != 33.1 || readBack[1].Longitude != -96.9 {
		t.Errorf("GetByID routePoints = %+v", readBack)
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

// TestDriveRepo_RoutePoints_DecryptFailureYieldsEmptyTrail: a corrupt
// ciphertext reads as an EMPTY trail, never as the plaintext jsonb.
//
// The seed below deliberately puts a real point in the plaintext column
// (the shape a pre-MYR-433 row has) before stomping the shadow, so a
// regression that restores the fallback surfaces that point and fails
// here. Serving nothing is the correct answer for an unreadable trail:
// the alternative requires the plaintext column to stay readable, which
// is the property MYR-433 exists to remove.
func TestDriveRepo_RoutePoints_DecryptFailureYieldsEmptyTrail(t *testing.T) {
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
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Plant a legacy-shaped row: a readable plaintext trail beside a
	// ciphertext the repo cannot decrypt.
	decoy := json.RawMessage(`[{"lat":1,"lng":2,"speed":3,"heading":4,"timestamp":"seed"}]`)
	if _, err := testPool.Exec(ctx,
		`UPDATE "Drive" SET "routePoints" = $1::jsonb, "routePointsEnc" = $2 WHERE "id" = $3`,
		decoy, "not-base64", "drv3"); err != nil {
		t.Fatalf("stomp: %v", err)
	}

	d, err := repo.GetByID(ctx, "drv3")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(d.RoutePoints) != 0 {
		t.Errorf("GetByID returned %s, want an empty trail — the plaintext decoy must never be surfaced",
			d.RoutePoints)
	}
}

// TestDriveRepo_RoutePoints_LegacyConstructorRefusesAppend pins the
// MYR-433 fail-closed contract for the legacy NewDriveRepo path: with no
// Encryptor there is nowhere legitimate to put a trail, so
// AppendRoutePoints refuses outright with ErrEncryptionRequired.
//
// The old behaviour — write the points to the plaintext column and leave
// the shadow NULL — is precisely the leak the issue closes, and it would
// be a silent one: the caller would see a successful append. Failing the
// call instead surfaces the misconfiguration to whoever wired the repo,
// and the caller keeps its buffer and can retry once a key is present.
// Both columns must be left exactly as the INSERT wrote them.
func TestDriveRepo_RoutePoints_LegacyConstructorRefusesAppend(t *testing.T) {
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
	err := repo.AppendRoutePoints(ctx, "drv4", []store.RoutePointRecord{
		{Latitude: 1, Longitude: 2, Speed: 3, Heading: 4, Timestamp: "t1"},
	})
	if !errors.Is(err, store.ErrEncryptionRequired) {
		t.Fatalf("AppendRoutePoints error = %v, want ErrEncryptionRequired", err)
	}

	var plain json.RawMessage
	var ct *string
	if err := testPool.QueryRow(ctx,
		`SELECT "routePoints", "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "drv4",
	).Scan(&plain, &ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ct != nil {
		t.Errorf("legacy repo wrote *Enc shadow: %v", *ct)
	}
	if !jsonEqual(t, plain, []byte(`[]`)) {
		t.Errorf("routePoints = %s, want [] — a refused append must not leave plaintext points behind", plain)
	}
}
