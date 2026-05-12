package vehiclegpsbackfill_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/vehiclegpsbackfill"
)

var (
	testPool        *pgxpool.Pool
	dockerAvailable bool
	teardown        func()
)

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker not available, skipping vehiclegpsbackfill tests")
		os.Exit(m.Run())
	}
	ctx := context.Background()
	c, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("backfill_test"),
		postgres.WithUsername("u"),
		postgres.WithPassword("p"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}
	conn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conn string: %v\n", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new pool: %v\n", err)
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, vehicleSchemaSQL); err != nil {
		fmt.Fprintf(os.Stderr, "schema: %v\n", err)
		pool.Close()
		_ = c.Terminate(ctx)
		os.Exit(1)
	}
	testPool = pool
	dockerAvailable = true
	teardown = func() {
		pool.Close()
		_ = c.Terminate(ctx)
	}

	code := m.Run()
	teardown()
	os.Exit(code)
}

// vehicleSchemaSQL is a slim fixture that mirrors the dual-write
// columns the backfill walks. Foreign keys / extra Prisma columns are
// intentionally omitted to keep the test pool independent of the
// migration ordering.
const vehicleSchemaSQL = `
CREATE TABLE IF NOT EXISTS "Vehicle" (
    "id"                       TEXT PRIMARY KEY,
    "userId"                   TEXT NOT NULL,
    "vin"                      TEXT,
    "latitude"                 DOUBLE PRECISION NOT NULL DEFAULT 0,
    "longitude"                DOUBLE PRECISION NOT NULL DEFAULT 0,
    "latitudeEnc"              TEXT,
    "longitudeEnc"             TEXT,
    "destinationLatitude"      DOUBLE PRECISION,
    "destinationLongitude"     DOUBLE PRECISION,
    "destinationLatitudeEnc"   TEXT,
    "destinationLongitudeEnc"  TEXT,
    "originLatitude"           DOUBLE PRECISION,
    "originLongitude"          DOUBLE PRECISION,
    "originLatitudeEnc"        TEXT,
    "originLongitudeEnc"       TEXT
);
`

func requirePool(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
}

func isDockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil //#nosec G204 -- hardcoded
}

func cleanVehicle(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM "Vehicle"`); err != nil {
		t.Fatalf("clean: %v", err)
	}
}

func newEncryptor(t *testing.T) cryptox.Encryptor {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(raw))
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedVehicle inserts a minimal vehicle row with the given GPS values.
// A nil destLat/lng or originLat/lng leaves the column NULL so tests
// can exercise the half-pair branch.
func seedVehicle(t *testing.T, id, vin string, lat, lng float64, destLat, destLng, originLat, originLng *float64) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","userId","vin","latitude","longitude","destinationLatitude","destinationLongitude","originLatitude","originLongitude")
         VALUES ($1,'u',$2,$3,$4,$5,$6,$7,$8)`,
		id, vin, lat, lng, destLat, destLng, originLat, originLng,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func float64Ptr(f float64) *float64 { return &f }

// TestBackfill_EncryptsAllPairs is the happy-path: a row with every
// plaintext pair set + every *Enc NULL is fully encrypted on Run.
func TestBackfill_EncryptsAllPairs(t *testing.T) {
	requirePool(t)
	cleanVehicle(t)

	seedVehicle(t, "row1", "VIN1",
		33.10, -96.80,
		float64Ptr(32.78), float64Ptr(-96.80),
		float64Ptr(33.20), float64Ptr(-96.85),
	)

	enc := newEncryptor(t)
	bf := vehiclegpsbackfill.New(testPool, enc, silentLogger())
	res, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsScanned != 1 {
		t.Errorf("RowsScanned: got %d, want 1", res.RowsScanned)
	}
	if res.PairsEncrypted != 3 {
		t.Errorf("PairsEncrypted: got %d, want 3", res.PairsEncrypted)
	}
	if res.RowsUpdated != 1 {
		t.Errorf("RowsUpdated: got %d, want 1", res.RowsUpdated)
	}
	if res.Errors() != 0 {
		t.Errorf("Errors: got %d", res.Errors())
	}
	for _, col := range vehiclegpsbackfill.Columns {
		if got := res.PlaintextRemaining[col]; got != 0 {
			t.Errorf("PlaintextRemaining[%s] = %d, want 0", col, got)
		}
	}

	// Verify ciphertext decrypts to the seeded plaintext.
	row := testPool.QueryRow(context.Background(),
		`SELECT "latitudeEnc", "longitudeEnc",
                "destinationLatitudeEnc", "destinationLongitudeEnc",
                "originLatitudeEnc", "originLongitudeEnc"
         FROM "Vehicle" WHERE "id"=$1`, "row1")
	var lat, lng, dLat, dLng, oLat, oLng *string
	if err := row.Scan(&lat, &lng, &dLat, &dLng, &oLat, &oLng); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for label, ct := range map[string]*string{"lat": lat, "lng": lng, "dLat": dLat, "dLng": dLng, "oLat": oLat, "oLng": oLng} {
		if ct == nil || *ct == "" {
			t.Fatalf("%s ciphertext not written", label)
		}
		if _, err := enc.DecryptString(*ct); err != nil {
			t.Fatalf("decrypt %s: %v", label, err)
		}
	}
}

// TestBackfill_Idempotent verifies a second run over a fully migrated
// table touches zero rows.
func TestBackfill_Idempotent(t *testing.T) {
	requirePool(t)
	cleanVehicle(t)

	seedVehicle(t, "row1", "VIN1", 33.10, -96.80, nil, nil, nil, nil)

	bf := vehiclegpsbackfill.New(testPool, newEncryptor(t), silentLogger())
	first, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.RowsScanned != 1 || first.PairsEncrypted != 1 {
		t.Errorf("first Run summary unexpected: %+v", first)
	}

	second, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.RowsScanned != 0 || second.PairsEncrypted != 0 {
		t.Errorf("second Run should be no-op; got %+v", second)
	}
}

// TestBackfill_HalfPairPlaintextSkipped: a row that has only
// destinationLatitude set (no destinationLongitude) is ineligible —
// the atomic-pair invariant says we cannot encrypt half a pair. Such
// rows are still scanned (other pairs may be eligible) but the
// half-pair pair contributes zero PairsEncrypted.
func TestBackfill_HalfPairPlaintextSkipped(t *testing.T) {
	requirePool(t)
	cleanVehicle(t)

	// destinationLongitude NULL — main pair eligible, dest pair half.
	seedVehicle(t, "row1", "VIN1", 33.10, -96.80,
		float64Ptr(32.78), nil, // half-pair
		nil, nil,
	)

	bf := vehiclegpsbackfill.New(testPool, newEncryptor(t), silentLogger())
	res, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PairsEncrypted != 1 {
		t.Errorf("PairsEncrypted = %d, want 1 (main pair only)", res.PairsEncrypted)
	}

	// Verify dest *Enc columns stayed NULL.
	row := testPool.QueryRow(context.Background(),
		`SELECT "destinationLatitudeEnc", "destinationLongitudeEnc" FROM "Vehicle" WHERE "id"=$1`, "row1")
	var dLat, dLng *string
	if err := row.Scan(&dLat, &dLng); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if dLat != nil || dLng != nil {
		t.Errorf("dest *Enc unexpectedly written: lat=%v lng=%v", dLat, dLng)
	}
}

// TestBackfill_MixedState: some pairs already encrypted, others not.
// Idempotency must compose at pair granularity.
func TestBackfill_MixedState(t *testing.T) {
	requirePool(t)
	cleanVehicle(t)

	enc := newEncryptor(t)
	preLat, _ := enc.EncryptString(strconv.FormatFloat(33.10, 'g', -1, 64))
	preLng, _ := enc.EncryptString(strconv.FormatFloat(-96.80, 'g', -1, 64))

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","userId","vin","latitude","longitude","latitudeEnc","longitudeEnc","destinationLatitude","destinationLongitude") VALUES ($1,'u',$2,$3,$4,$5,$6,$7,$8)`,
		"row1", "VIN1", 33.10, -96.80, preLat, preLng, 32.78, -96.80,
	); err != nil {
		t.Fatalf("seed mixed: %v", err)
	}

	res, err := vehiclegpsbackfill.New(testPool, enc, silentLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PairsEncrypted != 1 {
		t.Errorf("PairsEncrypted = %d, want 1 (dest pair only)", res.PairsEncrypted)
	}

	// main *Enc must not be modified.
	row := testPool.QueryRow(context.Background(),
		`SELECT "latitudeEnc", "longitudeEnc" FROM "Vehicle" WHERE "id"=$1`, "row1")
	var lat, lng *string
	if err := row.Scan(&lat, &lng); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lat == nil || *lat != preLat {
		t.Errorf("latitudeEnc was modified: got=%v want=%q", lat, preLat)
	}
	if lng == nil || *lng != preLng {
		t.Errorf("longitudeEnc was modified: got=%v want=%q", lng, preLng)
	}
}

// TestPlaintextGauge_RegistersAndCounts verifies the
// `vehicle_gps_plaintext_remaining_total` gauge exposes one label per
// of the six columns and counts plaintext-without-ciphertext rows.
func TestPlaintextGauge_RegistersAndCounts(t *testing.T) {
	requirePool(t)
	cleanVehicle(t)

	// 2 rows with main plaintext (both columns), 0 with destination,
	// 1 with origin half-pair (originLatitude only — counted as one
	// plaintext-remaining for originLatitude, zero for originLongitude).
	seedVehicle(t, "g1", "V1", 33.0, -96.0, nil, nil, nil, nil)
	seedVehicle(t, "g2", "V2", 34.0, -97.0, nil, nil, nil, nil)
	seedVehicle(t, "g3", "V3", 35.0, -98.0, nil, nil, float64Ptr(33.5), nil)

	reg := prometheus.NewRegistry()
	gauge := vehiclegpsbackfill.NewPlaintextGauge(reg, testPool, 0, silentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { gauge.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	var got map[string]float64
	for time.Now().Before(deadline) {
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		got = readGauge(mfs, "vehicle_gps_plaintext_remaining_total")
		if got["latitude"] == 3 && got["originLatitude"] == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got["latitude"] != 3 {
		t.Errorf("latitude gauge = %v, want 3", got["latitude"])
	}
	if got["longitude"] != 3 {
		t.Errorf("longitude gauge = %v, want 3", got["longitude"])
	}
	if got["destinationLatitude"] != 0 {
		t.Errorf("destinationLatitude gauge = %v, want 0", got["destinationLatitude"])
	}
	if got["originLatitude"] != 1 {
		t.Errorf("originLatitude gauge = %v, want 1", got["originLatitude"])
	}
	if got["originLongitude"] != 0 {
		t.Errorf("originLongitude gauge = %v, want 0", got["originLongitude"])
	}
	cancel()
	<-done
}

func readGauge(mfs []*dto.MetricFamily, name string) map[string]float64 {
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var col string
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "column" {
					col = lp.GetValue()
				}
			}
			out[col] = m.GetGauge().GetValue()
		}
	}
	return out
}
