package store_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// testPool is the shared connection pool for all store integration tests.
// Initialized once in TestMain.
var testPool *pgxpool.Pool

// testConnStr is the connection string for the test container.
var testConnStr string

// dockerAvailable is set during TestMain. When false, all tests skip.
var dockerAvailable bool

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker is not available, skipping store integration tests")
		os.Exit(0)
	}

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("testdb"),
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
	testConnStr = connStr

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create pool: %v\n", err)
		os.Exit(1)
	}

	if err := createSchema(ctx, pool); err != nil {
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

// isDockerRunning checks whether the Docker daemon is reachable.
func isDockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info") // #nosec G204 -- hardcoded command, not user input
	return cmd.Run() == nil
}

// createSchema reproduces the Prisma-owned tables for testing.
func createSchema(ctx context.Context, pool *pgxpool.Pool) error {
	schema := `
	CREATE TYPE "VehicleStatus" AS ENUM (
		'driving', 'parked', 'charging', 'offline', 'in_service'
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
		-- MYR-63 Phase 2 — encrypted shadow columns. Mirrors the Prisma
		-- schema in ../my-robo-taxi/prisma/schema.prisma so the dual-write
		-- path (UPDATE both Float + *Enc) is exercised against a realistic
		-- table shape.
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
		-- MYR-64 Phase 2 — encrypted shadow for navRouteCoordinates.
		-- Phase 1 added the column to the Prisma schema; the test
		-- fixture mirrors it so the dual-write path is exercised end
		-- to end.
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
		-- MYR-64 Phase 2 — encrypted shadow for routePoints. Phase 1
		-- added the column on the Prisma schema (Text?); test fixture
		-- mirrors it so the dual-write path is exercised end to end.
		"routePointsEnc"   TEXT,
		"createdAt"        TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);`

	_, err := pool.Exec(ctx, schema)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// testLogger returns a no-op logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(
		discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError},
	))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// cleanVehicles deletes all rows from Vehicle and Drive tables.
func cleanTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM "Drive"`)
	if err != nil {
		t.Fatalf("clean Drive table: %v", err)
	}
	_, err = pool.Exec(ctx, `DELETE FROM "Vehicle"`)
	if err != nil {
		t.Fatalf("clean Vehicle table: %v", err)
	}
}

// seedVehicle inserts a test vehicle and returns its ID.
func seedVehicle(t *testing.T, pool *pgxpool.Pool, id, vin string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO "Vehicle" ("id", "userId", "vin", "name", "status")
		 VALUES ($1, 'user_001', $2, 'Test Model 3', 'parked')`,
		id, vin)
	if err != nil {
		t.Fatalf("seed vehicle: %v", err)
	}
}

// seedVehicleWithCatalog inserts a test vehicle and also sets the seven
// catalog columns promoted by MYR-24. Used by TestVehicleRepo_CatalogFields
// to verify the SELECT / scan path loads every column.
func seedVehicleWithCatalog(
	t *testing.T, pool *pgxpool.Pool, id, vin string, cat catalogFields,
) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO "Vehicle" (
			"id", "userId", "vin", "name", "status",
			"model", "year", "color",
			"locationName", "locationAddress", "fsdMilesSinceReset",
			"destinationAddress"
		) VALUES (
			$1, 'user_001', $2, 'Test Model 3', 'parked',
			$3, $4, $5,
			$6, $7, $8,
			$9
		)`,
		id, vin,
		cat.model, cat.year, cat.color,
		cat.locationName, cat.locationAddress, cat.fsdMilesSinceReset,
		cat.destinationAddress,
	)
	if err != nil {
		t.Fatalf("seed vehicle with catalog: %v", err)
	}
}

func TestDB_NewDB(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{
			name:    "valid connection",
			url:     testConnStr,
			wantErr: false,
		},
		{
			name:    "invalid URL",
			url:     "postgres://bad:bad@localhost:1/nope", // #nosec G101 -- test credentials, not real
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DatabaseConfig{
				URL:      tt.url,
				MaxConns: 2,
				MinConns: 1,
			}
			db, err := store.NewDB(ctx, cfg, testLogger(), store.NoopMetrics{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			t.Cleanup(db.Close)

			if err := db.Ping(ctx); err != nil {
				t.Fatalf("Ping failed: %v", err)
			}
			if db.Pool() == nil {
				t.Fatal("Pool() returned nil")
			}
		})
	}
}

func TestDB_Ping(t *testing.T) {
	ctx := context.Background()
	cfg := config.DatabaseConfig{URL: testConnStr, MaxConns: 2, MinConns: 1}
	db, err := store.NewDB(ctx, cfg, testLogger(), store.NoopMetrics{})
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestDB_CollectPoolStats(t *testing.T) {
	ctx := context.Background()
	cfg := config.DatabaseConfig{URL: testConnStr, MaxConns: 2, MinConns: 1}

	recorder := &metricsRecorder{}
	db, err := store.NewDB(ctx, cfg, testLogger(), recorder)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(db.Close)

	db.CollectPoolStats()

	if !recorder.poolStatsCalled {
		t.Fatal("expected SetPoolStats to be called")
	}
}

// metricsRecorder tracks whether metrics methods were called.
type metricsRecorder struct {
	poolStatsCalled bool
}

func (m *metricsRecorder) ObserveQueryDuration(string, float64) {}
func (m *metricsRecorder) IncQueryError(string)                 {}
func (m *metricsRecorder) SetPoolStats(_, _, _ int32)           { m.poolStatsCalled = true }
