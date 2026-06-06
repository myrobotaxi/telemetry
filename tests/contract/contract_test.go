//go:build contract

// Package contract_test contract conformance harness for the Go REST
// surface (MYR-141 Quality 1 Phase 1).
//
// What this file is: shared helpers for endpoint-level contract tests
// that spin up the real handlers from `internal/telemetry/*`, wired
// against a real Postgres (via testcontainers-go) using the same lean
// repos the production binary mounts. The point is to catch contract
// drift — missing routes (MYR-132's 404 on /drives), data-shape
// mismatches (MYR-139), and schema-breaking changes — on every PR
// against the actual handler code rather than against fixtures alone.
//
// Build tag: `contract`. The default `go test ./...` does NOT execute
// these tests, mirroring the pattern in `internal/store/db_test.go` —
// local dev without Docker still passes. CI runs them in a dedicated
// `Contract conformance` job per `.github/workflows/ci.yml`.
//
// Docker gating: when Docker is unreachable, TestMain logs a skip
// notice and exits 0 without running any tests. This matches the
// behavior in `internal/store/db_test.go` so a workstation without
// Docker is never a hard failure.
//
// Schema validation: bodies are validated against the canonical
// `docs/contracts/schemas/*.schema.json` shapes using
// `github.com/santhosh-tekuri/jsonschema/v6` (already a dependency for
// `fixtures_test.go` — Phase 1 adds no new modules). The task spec
// proposed v5; v6 is the version already vendored, so we reuse it
// rather than introduce a parallel version.
package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ---------------------------------------------------------------------------
// Shared container state (TestMain)
// ---------------------------------------------------------------------------

// contractTestSecret is the HS256 signing secret the JWT minter and the
// JWTAuthenticator share for all contract tests. Hardcoded because the
// secret is irrelevant outside the test process — what matters is the
// minter and the validator agree.
const contractTestSecret = "contract-test-secret-key" // #nosec G101 -- test-only

// testPool is the shared pgx pool for every contract test. Initialized
// in TestMain and reused across t.Run subtests; each subtest owns its
// data by calling cleanTables before seeding.
var testPool *pgxpool.Pool

// dockerAvailable mirrors internal/store/db_test.go: when false, the
// suite exits 0 instead of running tests against a missing daemon.
var dockerAvailable bool

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker is not available, skipping contract tests")
		os.Exit(0)
	}

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("contractdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start postgres container: %v\n", err)
		os.Exit(1)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get connection string: %v\n", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
		os.Exit(1)
	}

	if err := createContractSchema(ctx, pool); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create schema: %v\n", err)
		os.Exit(1)
	}

	testPool = pool
	dockerAvailable = true

	code := m.Run()

	pool.Close()
	if err := pgContainer.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
	}

	os.Exit(code)
}

// isDockerRunning probes the Docker daemon. A 5s ceiling keeps a stalled
// daemon from blocking CI indefinitely.
func isDockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info") // #nosec G204 -- hardcoded command, not user input
	return cmd.Run() == nil
}

// createContractSchema provisions the Prisma-owned tables the handlers
// read from. Mirrors the shape in internal/store/db_test.go for
// Vehicle + Drive, plus a minimal User table for the JWTAuthenticator's
// fail-closed existence check (data-lifecycle.md §3.5).
func createContractSchema(ctx context.Context, pool *pgxpool.Pool) error {
	schema := `
	CREATE TYPE "VehicleStatus" AS ENUM (
		'driving', 'parked', 'charging', 'offline', 'in_service'
	);

	CREATE TABLE "User" (
		"id" TEXT PRIMARY KEY
	);

	CREATE TABLE "Vehicle" (
		"id"               TEXT PRIMARY KEY,
		"userId"           TEXT NOT NULL,
		"teslaVehicleId"   TEXT UNIQUE,
		"vin"              TEXT UNIQUE,
		"name"             TEXT NOT NULL DEFAULT '',
		"model"            TEXT NOT NULL DEFAULT '',
		"year"             INT NOT NULL DEFAULT 0,
		"color"            TEXT NOT NULL DEFAULT '',
		"licensePlate"     TEXT NOT NULL DEFAULT '',
		"chargeLevel"      INT NOT NULL DEFAULT 0,
		"estimatedRange"   INT NOT NULL DEFAULT 0,
		"chargeState"      TEXT,
		"timeToFull"       DOUBLE PRECISION,
		"status"           "VehicleStatus" NOT NULL DEFAULT 'offline',
		"speed"            INT NOT NULL DEFAULT 0,
		"gearPosition"     TEXT,
		"heading"          INT NOT NULL DEFAULT 0,
		"locationName"     TEXT NOT NULL DEFAULT '',
		"locationAddress"  TEXT NOT NULL DEFAULT '',
		"latitude"         DOUBLE PRECISION NOT NULL DEFAULT 0,
		"longitude"        DOUBLE PRECISION NOT NULL DEFAULT 0,
		"latitudeEnc"          TEXT,
		"longitudeEnc"         TEXT,
		"interiorTemp"     INT NOT NULL DEFAULT 0,
		"exteriorTemp"     INT NOT NULL DEFAULT 0,
		"odometerMiles"    INT NOT NULL DEFAULT 0,
		"fsdMilesSinceReset"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"virtualKeyPaired" BOOLEAN NOT NULL DEFAULT FALSE,
		"destinationName"  TEXT,
		"destinationLatitude"  DOUBLE PRECISION,
		"destinationLongitude" DOUBLE PRECISION,
		"destinationLatitudeEnc"  TEXT,
		"destinationLongitudeEnc" TEXT,
		"originLatitude"       DOUBLE PRECISION,
		"originLongitude"      DOUBLE PRECISION,
		"originLatitudeEnc"    TEXT,
		"originLongitudeEnc"   TEXT,
		"destinationAddress" TEXT,
		"etaMinutes"       INT,
		"tripDistanceMiles" DOUBLE PRECISION,
		"tripDistanceRemaining" DOUBLE PRECISION,
		"navRouteCoordinates" JSONB,
		"navRouteCoordinatesEnc" TEXT,
		"lastUpdated"      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"createdAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		"updatedAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE "Drive" (
		"id"               TEXT PRIMARY KEY,
		"vehicleId"        TEXT NOT NULL REFERENCES "Vehicle"("id"),
		"date"             TEXT NOT NULL,
		"startTime"        TEXT NOT NULL,
		"endTime"          TEXT NOT NULL DEFAULT '',
		"startLocation"    TEXT NOT NULL DEFAULT '',
		"startAddress"     TEXT NOT NULL DEFAULT '',
		"endLocation"      TEXT NOT NULL DEFAULT '',
		"endAddress"       TEXT NOT NULL DEFAULT '',
		"distanceMiles"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"durationMinutes"  INT NOT NULL DEFAULT 0,
		"avgSpeedMph"      DOUBLE PRECISION NOT NULL DEFAULT 0,
		"maxSpeedMph"      DOUBLE PRECISION NOT NULL DEFAULT 0,
		"energyUsedKwh"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"startChargeLevel" INT NOT NULL DEFAULT 0,
		"endChargeLevel"   INT NOT NULL DEFAULT 0,
		"fsdMiles"         DOUBLE PRECISION NOT NULL DEFAULT 0,
		"fsdPercentage"    DOUBLE PRECISION NOT NULL DEFAULT 0,
		"interventions"    INT NOT NULL DEFAULT 0,
		"routePoints"      JSONB NOT NULL DEFAULT '[]',
		"routePointsEnc"   TEXT,
		"createdAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test server: real handlers, real repos, slim mux
// ---------------------------------------------------------------------------

// seedHelpers exposes the seed primitives every test needs to plant
// rows into the test DB. Returned by setupTestServer so a test holds a
// pointer to both the server and the seed surface in one value.
type seedHelpers struct {
	pool *pgxpool.Pool
}

// setupTestServer wires the three REST handlers under test against a
// real Postgres and a real JWTAuthenticator. The mux is intentionally
// lean — only the three routes Phase 1 covers are mounted; cmd/
// composition concerns (TLS, metrics, fleet config, debug stream) are
// out of scope. The signature mirrors the structure proposed in the
// task: `(*httptest.Server, *seedHelpers)`.
//
// The httptest.Server and the test pool are torn down via t.Cleanup
// so callers don't need defer.
func setupTestServer(t *testing.T) (*httptest.Server, *seedHelpers) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available; skipping contract test")
	}

	cleanTables(t, testPool)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	vehicleRepo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	driveRepo := store.NewDriveRepo(testPool, store.NoopMetrics{})

	// Issuer / audience left empty: the test minter omits them too, so
	// the JWT validator skips those checks. This mirrors the dev-mode
	// shape — sufficient for contract conformance (we care about
	// route + body shape, not claim-policy enforcement).
	authenticator := auth.NewJWTAuthenticator(contractTestSecret, "", "", testPool)

	listAdapter := &contractVehicleLister{repo: vehicleRepo}
	snapshotAdapter := &contractVehicleSnapshotReader{repo: vehicleRepo}
	driveAdapter := &contractDriveLister{repo: driveRepo}

	listHandler := telemetry.NewVehiclesListHandler(authenticator, listAdapter, logger)
	snapshotHandler := telemetry.NewVehicleSnapshotHandler(
		authenticator, snapshotAdapter, logger,
		telemetry.WithSnapshotRoleResolver(authenticator),
	)
	drivesHandler := telemetry.NewVehicleDrivesHandler(
		authenticator, snapshotAdapter, driveAdapter, logger,
		telemetry.WithDrivesRoleResolver(authenticator),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vehicles", listHandler.ServeHTTP)
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/snapshot", snapshotHandler.ServeHTTP)
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/drives", drivesHandler.ServeHTTP)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, &seedHelpers{pool: testPool}
}

// ---------------------------------------------------------------------------
// Real-repo adapters (mirror cmd/telemetry-server/adapters.go but live
// in the test package so we don't introduce a cyclic dep through cmd/)
// ---------------------------------------------------------------------------

type contractVehicleLister struct{ repo *store.VehicleRepo }

func (a *contractVehicleLister) ListByUser(ctx context.Context, userID string) ([]telemetry.VehicleCatalogRow, error) {
	rows, err := a.repo.ListSummariesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("contractVehicleLister.ListByUser: %w", err)
	}
	out := make([]telemetry.VehicleCatalogRow, 0, len(rows))
	for i := range rows {
		v := &rows[i]
		out = append(out, telemetry.VehicleCatalogRow{
			ID:             v.ID,
			VIN:            v.VIN,
			Name:           v.Name,
			Model:          v.Model,
			Year:           v.Year,
			Color:          v.Color,
			Status:         string(v.Status),
			ChargeLevel:    v.ChargeLevel,
			EstimatedRange: v.EstimatedRange,
			LastUpdated:    v.LastUpdated,
		})
	}
	return out, nil
}

type contractVehicleSnapshotReader struct{ repo *store.VehicleRepo }

func (a *contractVehicleSnapshotReader) GetByID(ctx context.Context, vehicleID string) (telemetry.VehicleSnapshotRow, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return telemetry.VehicleSnapshotRow{}, fmt.Errorf("contractVehicleSnapshotReader.GetByID: %w", err)
	}
	return telemetry.VehicleSnapshotRow{
		ID:                   v.ID,
		UserID:               v.UserID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		ChargeState:          v.ChargeState,
		TimeToFull:           v.TimeToFull,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesSinceReset,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated,
	}, nil
}

type contractDriveLister struct{ repo *store.DriveRepo }

func (a *contractDriveLister) ListByVehicleID(ctx context.Context, vehicleID string, cursor telemetry.DriveListCursor, limit int) (telemetry.DriveListPage, error) {
	page, err := a.repo.ListByVehicleID(ctx, vehicleID, store.DriveListCursor{
		StartTime: cursor.StartTime,
		ID:        cursor.ID,
	}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, fmt.Errorf("contractDriveLister.ListByVehicleID: %w", err)
	}
	items := make([]telemetry.DriveListItem, 0, len(page.Items))
	for i := range page.Items {
		d := &page.Items[i]
		items = append(items, telemetry.DriveListItem{
			ID:               d.ID,
			VehicleID:        d.VehicleID,
			StartTime:        d.StartTime,
			EndTime:          d.EndTime,
			Date:             d.Date,
			DistanceMiles:    d.DistanceMiles,
			DurationMinutes:  d.DurationMinutes,
			AvgSpeedMph:      d.AvgSpeedMph,
			MaxSpeedMph:      d.MaxSpeedMph,
			StartChargeLevel: d.StartChargeLevel,
			EndChargeLevel:   d.EndChargeLevel,
			CreatedAt:        d.CreatedAt,
		})
	}
	return telemetry.DriveListPage{Items: items, HasMore: page.HasMore}, nil
}

// ---------------------------------------------------------------------------
// Token minting
// ---------------------------------------------------------------------------

// mintToken signs an HS256 JWT using the shared contract-test secret.
// The `scopes` argument is reserved for future per-scope checks
// (rest-api.md §3.x); v1 carries it as a claim but the validator
// ignores it. Kept in the signature so that callers under §7.0 / §7.1
// / §7.2 don't need refactoring when scope-based gating lands.
//
//nolint:unparam // `scopes` is the contract-test harness public API per MYR-141.
func mintToken(t *testing.T, userID string, scopes []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	}
	if len(scopes) > 0 {
		claims["scopes"] = scopes
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(contractTestSecret))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	return signed
}

// mintExpiredToken returns a JWT signed with the right secret but with
// `exp` in the past. Used by the 401 expired-token case.
func mintExpiredToken(t *testing.T, userID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(contractTestSecret))
	if err != nil {
		t.Fatalf("mintExpiredToken: %v", err)
	}
	return signed
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

// cleanTables truncates the contract test tables. Called by
// setupTestServer for every test so subtests can seed freely without
// worrying about leaks from earlier tests. Order matters: Drive has a
// FK to Vehicle.
func cleanTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM "Drive"`); err != nil {
		t.Fatalf("clean Drive: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "Vehicle"`); err != nil {
		t.Fatalf("clean Vehicle: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "User"`); err != nil {
		t.Fatalf("clean User: %v", err)
	}
}

// seedUser inserts a minimal User row so the JWTAuthenticator's FR-10.1
// fail-closed existence check (data-lifecycle.md §3.5) accepts the
// caller's token. A test that mints a token for userID MUST seed that
// user first; ValidateToken rejects unknown subjects.
func (h *seedHelpers) seedUser(ctx context.Context, t *testing.T, userID string) {
	t.Helper()
	if _, err := h.pool.Exec(ctx, `INSERT INTO "User" ("id") VALUES ($1)`, userID); err != nil {
		t.Fatalf("seedUser(%s): %v", userID, err)
	}
}

// vehicleSeed bundles the columns most contract tests want to set on a
// Vehicle row. Zero-valued fields fall through to their Prisma defaults
// (status=offline, charge=0, etc.) so a minimal call site can pass just
// the identifiers and let the schema fill the rest.
type vehicleSeed struct {
	ID              string
	UserID          string
	VIN             string
	Name            string
	Model           string
	Year            int
	Color           string
	Status          string  // empty string falls through to schema default 'offline'
	ChargeLevel     int
	EstimatedRange  int
	LocationName    string
	LocationAddress string
	FsdMilesReset   float64
	LastUpdated     time.Time
}

// seedVehicle inserts a Vehicle row. The fixture covers every field the
// snapshot handler reads back (status, charge, name, address, etc.) —
// the wide-read path will scan all columns whether or not the test
// cares about them.
func (h *seedHelpers) seedVehicle(ctx context.Context, t *testing.T, v vehicleSeed) {
	t.Helper()
	status := v.Status
	if status == "" {
		status = "parked"
	}
	if v.LastUpdated.IsZero() {
		v.LastUpdated = time.Now().UTC()
	}
	_, err := h.pool.Exec(ctx, `
		INSERT INTO "Vehicle" (
			"id", "userId", "vin", "name",
			"model", "year", "color", "status",
			"chargeLevel", "estimatedRange",
			"locationName", "locationAddress",
			"fsdMilesSinceReset", "lastUpdated"
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8::"VehicleStatus",
			$9, $10,
			$11, $12,
			$13, $14
		)`,
		v.ID, v.UserID, v.VIN, v.Name,
		v.Model, v.Year, v.Color, status,
		v.ChargeLevel, v.EstimatedRange,
		v.LocationName, v.LocationAddress,
		v.FsdMilesReset, v.LastUpdated,
	)
	if err != nil {
		t.Fatalf("seedVehicle(%s): %v", v.ID, err)
	}
}

// driveSeed bundles the columns that drive the drives-list ordering and
// pagination assertions. The store-layer ListByVehicleID reads
// startTime + id as the cursor anchor, so a test that wants
// deterministic pagination MUST set both per row.
//
// MYR-145 added Start/End Location + Address. Leave them empty to
// exercise the "drive in progress / not yet geocoded" branch (the wire
// payload omits the key entirely); set them to non-empty strings to
// assert the populated branch.
type driveSeed struct {
	ID               string
	VehicleID        string
	Date             string
	StartTime        string
	EndTime          string
	StartLocation    string
	StartAddress     string
	EndLocation      string
	EndAddress       string
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	StartChargeLevel int
	EndChargeLevel   int
	CreatedAt        time.Time
}

// seedDrive inserts a Drive row. Empty Start/End Location + Address
// values rely on the table's NOT NULL DEFAULT '' to stay consistent
// with the Prisma-owned column semantics.
func (h *seedHelpers) seedDrive(ctx context.Context, t *testing.T, d driveSeed) {
	t.Helper()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	_, err := h.pool.Exec(ctx, `
		INSERT INTO "Drive" (
			"id", "vehicleId", "date", "startTime", "endTime",
			"startLocation", "startAddress", "endLocation", "endAddress",
			"distanceMiles", "durationMinutes",
			"avgSpeedMph", "maxSpeedMph",
			"startChargeLevel", "endChargeLevel",
			"createdAt"
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11,
			$12, $13,
			$14, $15,
			$16
		)`,
		d.ID, d.VehicleID, d.Date, d.StartTime, d.EndTime,
		d.StartLocation, d.StartAddress, d.EndLocation, d.EndAddress,
		d.DistanceMiles, d.DurationMinutes,
		d.AvgSpeedMph, d.MaxSpeedMph,
		d.StartChargeLevel, d.EndChargeLevel,
		d.CreatedAt,
	)
	if err != nil {
		t.Fatalf("seedDrive(%s): %v", d.ID, err)
	}
}

// ---------------------------------------------------------------------------
// HTTP convenience helpers
// ---------------------------------------------------------------------------

// doGET issues a GET against the test server. When token is non-empty
// it sets the Authorization header — pass "" to exercise the missing-
// token branch.
func doGET(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// readBody reads the response body and closes it.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// decodeJSON unmarshals body into v. Fails the test on parse error —
// every contract test SHOULD reach this point with a JSON body.
func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, string(body))
	}
}

// jsonMarshal is a thin wrapper around json.Marshal used by tests that
// want to re-serialize a decoded sub-object (e.g. a single list item)
// for schema validation. Exported as a package-private helper so call
// sites stay readable.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// ---------------------------------------------------------------------------
// JSON Schema validation
// ---------------------------------------------------------------------------

// validateAgainstSchema validates body against the schema at schemaPath
// (relative to the repo root, e.g. "docs/contracts/schemas/vehicle-state.schema.json").
// The compiler is loaded fresh per call — schemas are small and the
// jsonschema/v6 compiler caches them internally; perf is not a concern
// at test scale.
func validateAgainstSchema(t *testing.T, schemaPath string, body []byte) {
	t.Helper()

	root := repoRoot(t)
	schemasAbs := filepath.Join(root, "docs/contracts/schemas")
	mainSchema := filepath.Join(root, schemaPath)

	compiler := jsonschema.NewCompiler()
	loadSchemaResources(t, compiler, schemasAbs)

	mainID := schemaIDFromFile(t, mainSchema)
	schema, err := compiler.Compile(mainID)
	if err != nil {
		t.Fatalf("compile schema %s: %v", schemaPath, err)
	}

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("unmarshal body for schema validation: %v\nbody: %s", err, string(body))
	}
	if err := schema.Validate(parsed); err != nil {
		t.Errorf("schema validation failed (%s):\n%v\nbody: %s", schemaPath, err, string(body))
	}
}

// loadSchemaResources adds every schema under schemasAbs to the
// compiler so $ref resolution works across files even when the test
// only validates against one of them.
func loadSchemaResources(t *testing.T, c *jsonschema.Compiler, schemasAbs string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(schemasAbs, "*.json"))
	if err != nil {
		t.Fatalf("glob schemas: %v", err)
	}
	for _, f := range files {
		raw, err := readSchemaJSON(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		id, _ := raw.(map[string]any)["$id"].(string)
		if id == "" {
			id = "file:///" + filepath.Base(f)
		}
		if err := c.AddResource(id, raw); err != nil {
			t.Fatalf("add resource %s: %v", f, err)
		}
	}
}

// schemaIDFromFile reads the $id field from a schema file. The main
// compile target uses the $id so $ref-by-id works alongside file-based
// loading.
func schemaIDFromFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := readSchemaJSON(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	id, _ := raw.(map[string]any)["$id"].(string)
	if id == "" {
		t.Fatalf("schema %s has no $id", path)
	}
	return id
}

func readSchemaJSON(path string) (any, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- test-local schema path
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// NOTE: `repoRoot` is provided by tests/contract/fixtures_test.go,
// which is in the same package and compiled in both default and
// contract-tagged builds. We deliberately don't re-declare it here.

// ---------------------------------------------------------------------------
// Error envelope helpers
// ---------------------------------------------------------------------------

// restErrorEnvelope mirrors the rest-api.md §4.1 error shape returned
// by `wserrors.WriteErrorEnvelope`. Tests decode response bodies into
// this struct to assert on the typed `error.code`.
type restErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// assertErrorCode decodes body and checks the typed error code matches
// wantCode (e.g. "auth_failed", "not_found"). Fails the test with the
// full body on mismatch so a regression points straight at the wire
// shape.
func assertErrorCode(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var env restErrorEnvelope
	decodeJSON(t, body, &env)
	if env.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q\nbody: %s", env.Error.Code, wantCode, string(body))
	}
}
