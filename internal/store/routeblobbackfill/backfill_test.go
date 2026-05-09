package routeblobbackfill_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/routeblob"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/routeblobbackfill"
)

var (
	testPool        *pgxpool.Pool
	dockerAvailable bool
	teardown        func()
)

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker not available, skipping routeblobbackfill tests")
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
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
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

// schemaSQL is a slim fixture that mirrors the dual-write columns the
// backfill walks. Foreign keys / extra Prisma columns are intentionally
// omitted to keep the test pool independent of migration ordering.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS "Vehicle" (
    "id"                       TEXT PRIMARY KEY,
    "navRouteCoordinates"      JSONB,
    "navRouteCoordinatesEnc"   TEXT
);

CREATE TABLE IF NOT EXISTS "Drive" (
    "id"               TEXT PRIMARY KEY,
    "routePoints"      JSONB NOT NULL DEFAULT '[]',
    "routePointsEnc"   TEXT
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

func cleanTables(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM "Drive"`); err != nil {
		t.Fatalf("clean Drive: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM "Vehicle"`); err != nil {
		t.Fatalf("clean Vehicle: %v", err)
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

// TestBackfill_EncryptsBothTables: a Vehicle with navRouteCoordinates
// and a Drive with routePoints, both *Enc NULL, are fully migrated on
// Run.
func TestBackfill_EncryptsBothTables(t *testing.T) {
	requirePool(t)
	cleanTables(t)

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","navRouteCoordinates") VALUES ($1, $2::jsonb)`,
		"v1", `[[-96.80, 33.10],[-96.81, 33.11]]`); err != nil {
		t.Fatalf("seed Vehicle: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive" ("id","routePoints") VALUES ($1, $2::jsonb)`,
		"d1", `[{"lat":33.1,"lng":-96.8,"speed":35,"heading":90,"timestamp":"2026-05-09T12:00:00Z"}]`); err != nil {
		t.Fatalf("seed Drive: %v", err)
	}

	enc := newEncryptor(t)
	bf := routeblobbackfill.New(testPool, enc, silentLogger())
	res, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.VehicleRowsScanned != 1 {
		t.Errorf("VehicleRowsScanned = %d, want 1", res.VehicleRowsScanned)
	}
	if res.VehicleBlobsEncrypted != 1 {
		t.Errorf("VehicleBlobsEncrypted = %d, want 1", res.VehicleBlobsEncrypted)
	}
	if res.VehicleRowsUpdated != 1 {
		t.Errorf("VehicleRowsUpdated = %d, want 1", res.VehicleRowsUpdated)
	}
	if res.DriveRowsScanned != 1 {
		t.Errorf("DriveRowsScanned = %d, want 1", res.DriveRowsScanned)
	}
	if res.DriveBlobsEncrypted != 1 {
		t.Errorf("DriveBlobsEncrypted = %d, want 1", res.DriveBlobsEncrypted)
	}
	if res.Errors() != 0 {
		t.Errorf("Errors = %d", res.Errors())
	}
	for _, col := range routeblobbackfill.Columns {
		if got := res.PlaintextRemaining[col.Plaintext]; got != 0 {
			t.Errorf("PlaintextRemaining[%s] = %d, want 0", col.Plaintext, got)
		}
	}

	// Verify ciphertext decrypts back to the seeded plaintext.
	var navCT, rpCT *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT "navRouteCoordinatesEnc" FROM "Vehicle" WHERE "id"=$1`, "v1",
	).Scan(&navCT); err != nil {
		t.Fatalf("scan navRouteCoordinatesEnc: %v", err)
	}
	if navCT == nil || *navCT == "" {
		t.Fatal("navRouteCoordinatesEnc not written")
	}
	gotNav, err := routeblob.DecryptNavRoute(*navCT, enc)
	if err != nil {
		t.Fatalf("DecryptNavRoute: %v", err)
	}
	if len(gotNav) != 2 || gotNav[0] != [2]float64{-96.80, 33.10} {
		t.Errorf("decoded nav = %v", gotNav)
	}

	if err := testPool.QueryRow(context.Background(),
		`SELECT "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "d1",
	).Scan(&rpCT); err != nil {
		t.Fatalf("scan routePointsEnc: %v", err)
	}
	if rpCT == nil || *rpCT == "" {
		t.Fatal("routePointsEnc not written")
	}
	gotRP, err := routeblob.DecryptRoutePoints(*rpCT, enc)
	if err != nil {
		t.Fatalf("DecryptRoutePoints: %v", err)
	}
	if len(gotRP) != 1 || gotRP[0].Latitude != 33.1 {
		t.Errorf("decoded routePoints = %v", gotRP)
	}
}

// TestBackfill_Idempotent: a second run over a fully migrated table
// touches zero rows.
func TestBackfill_Idempotent(t *testing.T) {
	requirePool(t)
	cleanTables(t)

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","navRouteCoordinates") VALUES ($1, $2::jsonb)`,
		"v1", `[[-96.80, 33.10]]`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bf := routeblobbackfill.New(testPool, newEncryptor(t), silentLogger())
	first, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.VehicleBlobsEncrypted != 1 {
		t.Errorf("first encrypted = %d, want 1", first.VehicleBlobsEncrypted)
	}
	second, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.VehicleBlobsEncrypted != 0 || second.DriveBlobsEncrypted != 0 {
		t.Errorf("second should be no-op; got %+v", second)
	}
}

// TestBackfill_EmptyArraySkipped: a row whose plaintext is `[]` must
// NOT get an encrypted shadow. This matches the empty-input sentinel
// in routeblob.EncryptJSONBytes — the read fallback handles `[]`
// correctly and the shadow stays NULL.
func TestBackfill_EmptyArraySkipped(t *testing.T) {
	requirePool(t)
	cleanTables(t)

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive" ("id","routePoints") VALUES ($1, '[]'::jsonb)`,
		"d_empty"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := routeblobbackfill.New(testPool, newEncryptor(t), silentLogger()).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DriveBlobsEncrypted != 0 {
		t.Errorf("empty array unexpectedly encrypted: %d", res.DriveBlobsEncrypted)
	}

	var ct *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT "routePointsEnc" FROM "Drive" WHERE "id"=$1`, "d_empty",
	).Scan(&ct); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if ct != nil {
		t.Errorf("routePointsEnc unexpectedly populated for empty array: %v", ct)
	}
}

// TestPlaintextGauge_RegistersAndCounts verifies the
// `route_blob_plaintext_remaining_total` gauge exposes labels for both
// columns and counts plaintext-without-ciphertext rows.
func TestPlaintextGauge_RegistersAndCounts(t *testing.T) {
	requirePool(t)
	cleanTables(t)

	for i := 0; i < 3; i++ {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO "Vehicle" ("id","navRouteCoordinates") VALUES ($1, '[[-1, 2]]'::jsonb)`,
			fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("seed Vehicle %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO "Drive" ("id","routePoints") VALUES ($1, '[{"lat":1,"lng":2,"speed":3,"heading":4,"timestamp":"t"}]'::jsonb)`,
			fmt.Sprintf("d%d", i)); err != nil {
			t.Fatalf("seed Drive %d: %v", i, err)
		}
	}

	reg := prometheus.NewRegistry()
	gauge := routeblobbackfill.NewPlaintextGauge(reg, testPool, 0, silentLogger())

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
		got = readGauge(mfs, "route_blob_plaintext_remaining_total")
		if got["navRouteCoordinates"] == 3 && got["routePoints"] == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got["navRouteCoordinates"] != 3 {
		t.Errorf("navRouteCoordinates gauge = %v, want 3", got["navRouteCoordinates"])
	}
	if got["routePoints"] != 2 {
		t.Errorf("routePoints gauge = %v, want 2", got["routePoints"])
	}
	cancel()
	<-done
}

// TestPlaintextGauge_PreRegistersZero verifies a freshly registered
// gauge exposes both labels with value 0 BEFORE the first refresh
// completes, so /metrics never shows missing series during startup.
func TestPlaintextGauge_PreRegistersZero(t *testing.T) {
	requirePool(t)
	reg := prometheus.NewRegistry()
	_ = routeblobbackfill.NewPlaintextGauge(reg, testPool, 0, silentLogger())

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := readGauge(mfs, "route_blob_plaintext_remaining_total")
	for _, col := range routeblobbackfill.Columns {
		if _, ok := got[col.Plaintext]; !ok {
			t.Errorf("missing label %s on first scrape", col.Plaintext)
		}
	}
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
