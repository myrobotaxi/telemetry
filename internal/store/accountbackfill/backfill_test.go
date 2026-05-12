package accountbackfill_test

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

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/accountbackfill"
)

// shared test pool for the package. Initialized in TestMain so the pool
// outlives the first test that uses it (a t.Cleanup registration would
// tie its lifetime to the first test's scope, which silently closes the
// pool for every later test).
var (
	testPool        *pgxpool.Pool
	dockerAvailable bool
	teardown        func()
)

func TestMain(m *testing.M) {
	if !isDockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker not available, skipping accountbackfill tests")
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
	if _, err := pool.Exec(ctx, accountSchemaSQL); err != nil {
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

const accountSchemaSQL = `
CREATE TABLE IF NOT EXISTS "Account" (
    "id"                TEXT PRIMARY KEY,
    "userId"            TEXT NOT NULL,
    "type"              TEXT NOT NULL DEFAULT 'oauth',
    "provider"          TEXT NOT NULL,
    "providerAccountId" TEXT NOT NULL,
    "access_token"      TEXT,
    "access_token_enc"  TEXT,
    "refresh_token"     TEXT,
    "refresh_token_enc" TEXT,
    "id_token"          TEXT,
    "id_token_enc"      TEXT,
    "expires_at"        BIGINT
);
`

// requirePool skips the test if TestMain didn't bring up a Postgres
// container. Tests don't construct their own pool — TestMain owns the
// lifecycle.
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

func cleanAccount(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM "Account"`); err != nil {
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

func seed(t *testing.T, id, userID string, accessPT, refreshPT, idPT *string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO "Account" ("id","userId","type","provider","providerAccountId",
            "access_token","refresh_token","id_token") VALUES ($1,$2,'oauth','tesla',$3,$4,$5,$6)`,
		id, userID, id+"-acct", accessPT, refreshPT, idPT,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func ptr(s string) *string { return &s }

func TestBackfill_EncryptsAllPlaintextColumns(t *testing.T) {
	requirePool(t)
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanAccount(t)

	seed(t, "row1", "u1", ptr("access1"), ptr("refresh1"), ptr("id1"))
	seed(t, "row2", "u2", ptr("access2"), nil, nil)

	enc := newEncryptor(t)
	bf := accountbackfill.New(testPool, enc, silentLogger())

	res, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsScanned != 2 {
		t.Errorf("RowsScanned: got %d, want 2", res.RowsScanned)
	}
	if res.ColumnsEncrypted != 4 {
		t.Errorf("ColumnsEncrypted: got %d, want 4", res.ColumnsEncrypted)
	}
	if res.RowsUpdated != 2 {
		t.Errorf("RowsUpdated: got %d, want 2", res.RowsUpdated)
	}
	if res.Errors() != 0 {
		t.Errorf("Errors: got %d, want 0", res.Errors())
	}

	row := testPool.QueryRow(context.Background(),
		`SELECT "access_token_enc", "refresh_token_enc", "id_token_enc" FROM "Account" WHERE "id"=$1`, "row1")
	var aEnc, rEnc, iEnc *string
	if err := row.Scan(&aEnc, &rEnc, &iEnc); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for label, ct := range map[string]*string{"access": aEnc, "refresh": rEnc, "id": iEnc} {
		if ct == nil || *ct == "" {
			t.Fatalf("%s_token_enc not written", label)
		}
		if _, err := enc.DecryptString(*ct); err != nil {
			t.Fatalf("decrypt %s: %v", label, err)
		}
	}

	for _, col := range accountbackfill.TokenColumns {
		if got := res.PlaintextRemaining[col]; got != 0 {
			t.Errorf("PlaintextRemaining[%s] = %d, want 0", col, got)
		}
	}
}

func TestBackfill_Idempotent(t *testing.T) {
	requirePool(t)
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanAccount(t)

	seed(t, "row1", "u1", ptr("access"), ptr("refresh"), nil)

	bf := accountbackfill.New(testPool, newEncryptor(t), silentLogger())

	first, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.RowsScanned != 1 || first.ColumnsEncrypted != 2 {
		t.Errorf("first Run summary unexpected: %+v", first)
	}

	second, err := bf.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.RowsScanned != 0 {
		t.Errorf("second Run should be no-op; got %+v", second)
	}
	if second.ColumnsEncrypted != 0 {
		t.Errorf("idempotency violated: %+v", second)
	}
}

func TestBackfill_PartialMixedState(t *testing.T) {
	requirePool(t)
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanAccount(t)

	enc := newEncryptor(t)

	preCT, err := enc.EncryptString("pre-encrypted-access")
	if err != nil {
		t.Fatalf("preCT: %v", err)
	}

	_, err = testPool.Exec(context.Background(),
		`INSERT INTO "Account" ("id","userId","type","provider","providerAccountId",
            "access_token","access_token_enc","refresh_token","refresh_token_enc","id_token","id_token_enc")
         VALUES ('mix','umix','oauth','tesla','mix-acct',$1,$2,$3,NULL,NULL,NULL)`,
		"plaintext-access", preCT, "plaintext-refresh",
	)
	if err != nil {
		t.Fatalf("seed mixed: %v", err)
	}

	res, err := accountbackfill.New(testPool, enc, silentLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ColumnsEncrypted != 1 {
		t.Errorf("expected only refresh_token to be encrypted; got %+v", res)
	}

	var aEnc *string
	if err := testPool.QueryRow(context.Background(),
		`SELECT "access_token_enc" FROM "Account" WHERE "id"=$1`, "mix").Scan(&aEnc); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if aEnc == nil || *aEnc != preCT {
		t.Errorf("access_token_enc was modified; got %v want %v", aEnc, preCT)
	}
}

func TestBackfill_NoRowsToProcess(t *testing.T) {
	requirePool(t)
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanAccount(t)

	res, err := accountbackfill.New(testPool, newEncryptor(t), silentLogger()).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RowsScanned != 0 || res.ColumnsEncrypted != 0 {
		t.Errorf("expected zero work; got %+v", res)
	}
	for _, col := range accountbackfill.TokenColumns {
		if got := res.PlaintextRemaining[col]; got != 0 {
			t.Errorf("PlaintextRemaining[%s] = %d, want 0", col, got)
		}
	}
}

func TestPlaintextGauge_RegistersAndCounts(t *testing.T) {
	requirePool(t)
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	cleanAccount(t)

	// 2 plaintext access tokens, 0 plaintext refresh, 1 plaintext id.
	seed(t, "g1", "ug1", ptr("a1"), nil, nil)
	seed(t, "g2", "ug2", ptr("a2"), nil, nil)
	seed(t, "g3", "ug3", nil, nil, ptr("i3"))

	reg := prometheus.NewRegistry()
	// interval=0 disables the periodic loop, but Run still performs one
	// immediate refresh which is what we want to assert against.
	gauge := accountbackfill.NewPlaintextGauge(reg, testPool, 0, silentLogger())

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
		got = readGauge(mfs, "account_token_plaintext_remaining_total")
		if got["access_token"] == 2 && got["refresh_token"] == 0 && got["id_token"] == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got["access_token"] != 2 {
		t.Errorf("access_token = %v, want 2", got["access_token"])
	}
	if got["refresh_token"] != 0 {
		t.Errorf("refresh_token = %v, want 0", got["refresh_token"])
	}
	if got["id_token"] != 1 {
		t.Errorf("id_token = %v, want 1", got["id_token"])
	}

	cancel()
	<-done
}

// readGauge extracts per-column gauge values from a Gather() result.
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
