package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/identity"
	"github.com/myrobotaxi/telemetry/internal/store"
)

var (
	testPool        *pgxpool.Pool
	testConnStr     string
	dockerAvailable bool
)

func TestMain(m *testing.M) {
	// The unit tests in package identity share this binary but need no DB, so
	// a missing Docker daemon must NOT abort the run — it only skips the
	// integration tests below.
	if !dockerRunning() {
		fmt.Fprintln(os.Stderr, "Docker not available — identity integration tests will skip")
		os.Exit(m.Run())
	}
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}
	testConnStr, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conn str: %v\n", err)
		os.Exit(1)
	}
	testPool, err = pgxpool.New(ctx, testConnStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool: %v\n", err)
		os.Exit(1)
	}
	// The Prisma-owned "User" table (minimal shape) for the email-linkage +
	// existence queries; the go_ tables arrive via our migrations.
	if _, err := testPool.Exec(ctx, `CREATE TABLE "User" (
		"id" TEXT PRIMARY KEY, "email" TEXT UNIQUE, "emailVerified" TIMESTAMPTZ)`); err != nil {
		fmt.Fprintf(os.Stderr, "create User: %v\n", err)
		os.Exit(1)
	}
	if err := store.RunMigrations(ctx, testConnStr, discardLogger()); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		os.Exit(1)
	}
	dockerAvailable = true

	code := m.Run()
	testPool.Close()
	_ = c.Terminate(ctx)
	os.Exit(code)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func dockerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil //nolint:gosec // fixed command
}

func requireDocker(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available — skipping identity integration test")
	}
}

func cleanIdentityTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"go_refresh_tokens", "go_identity_apple", "go_users", `"User"`} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func TestPgStore_AppleIdentityRoundTrip(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	if _, found, err := s.GetAppleIdentity(ctx, "sub-x"); err != nil || found {
		t.Fatalf("expected miss, found=%v err=%v", found, err)
	}
	if err := s.InsertAppleIdentity(ctx, identity.AppleIdentity{
		AppleSub: "sub-x", UserID: "cuser1", Email: "a@example.com", Name: "Ada",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, found, err := s.GetAppleIdentity(ctx, "sub-x")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.UserID != "cuser1" || got.Email != "a@example.com" || got.Name != "Ada" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// Touch backfills nothing new but must not error or change user_id.
	if err := s.TouchAppleLogin(ctx, "sub-x", "a@example.com", "Ada"); err != nil {
		t.Fatalf("touch: %v", err)
	}
}

func TestPgStore_EmailLinkageAndGoUser(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	if _, err := testPool.Exec(ctx,
		`INSERT INTO "User" ("id","email","emailVerified") VALUES ('cprisma','Owner@Example.com', NOW())`); err != nil {
		t.Fatalf("seed User: %v", err)
	}
	// Case-insensitive match.
	id, found, err := s.FindPrismaUserIDByEmail(ctx, "owner@example.com")
	if err != nil || !found || id != "cprisma" {
		t.Fatalf("email match: id=%q found=%v err=%v", id, found, err)
	}
	if _, found, _ := s.FindPrismaUserIDByEmail(ctx, "nobody@example.com"); found {
		t.Error("unexpected match for unknown email")
	}
	// Mint an Apple-native user.
	if err := s.CreateGoUser(ctx, "cgo1", "new@example.com", "New User"); err != nil {
		t.Fatalf("CreateGoUser: %v", err)
	}
}

// TestPgStore_GetUserProfile covers MYR-243: the best-effort name/email
// lookup used by refresh. It exercises the go_identity_apple binding path,
// the go_users fallback (no binding row), and the fully-missing case.
func TestPgStore_GetUserProfile(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	// No row anywhere -> empty strings, no error (best-effort).
	name, email, err := s.GetUserProfile(ctx, "cnobody")
	if err != nil {
		t.Fatalf("missing profile: unexpected error %v", err)
	}
	if name != "" || email != "" {
		t.Errorf("missing profile: got name=%q email=%q, want empty", name, email)
	}

	// go_users only (no apple binding) -> fallback path.
	if err := s.CreateGoUser(ctx, "cgouseronly", "gouser@example.com", "Go User"); err != nil {
		t.Fatalf("CreateGoUser: %v", err)
	}
	name, email, err = s.GetUserProfile(ctx, "cgouseronly")
	if err != nil {
		t.Fatalf("go_users fallback: unexpected error %v", err)
	}
	if name != "Go User" || email != "gouser@example.com" {
		t.Errorf("go_users fallback: got name=%q email=%q", name, email)
	}

	// go_identity_apple binding present -> takes precedence over go_users,
	// even when both rows exist for the same user_id with DIFFERING values
	// (the only way to actually prove precedence rather than just exercising
	// the binding path in isolation). A go_users row for a user_id that also
	// has an apple binding is a real production shape: the fresh-mint path
	// (Service.resolveFirstSignIn) always writes go_users first, then binds
	// go_identity_apple in the same first-sign-in flow.
	if err := s.CreateGoUser(ctx, "capplebound", "conflict-gouser@example.com", "Conflict GoUser"); err != nil {
		t.Fatalf("CreateGoUser (conflicting row): %v", err)
	}
	if err := s.InsertAppleIdentity(ctx, identity.AppleIdentity{
		AppleSub: "sub-profile", UserID: "capplebound", Email: "apple@example.com", Name: "Apple Bound",
	}); err != nil {
		t.Fatalf("InsertAppleIdentity: %v", err)
	}
	name, email, err = s.GetUserProfile(ctx, "capplebound")
	if err != nil {
		t.Fatalf("apple binding: unexpected error %v", err)
	}
	if name != "Apple Bound" || email != "apple@example.com" {
		t.Errorf("apple binding: got name=%q email=%q, want the go_identity_apple values (Apple Bound / apple@example.com), not the conflicting go_users row", name, email)
	}
}

func TestPgStore_RefreshRotationAndReuse(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	root := "hash-root"
	future := time.Now().Add(90 * 24 * time.Hour)
	if err := s.InsertRefreshRoot(ctx, root, "fam1", "cuser", future); err != nil {
		t.Fatalf("insert root: %v", err)
	}
	// Rotate root -> t2.
	res, err := s.RotateRefreshToken(ctx, root, "hash-t2", future)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if res.Outcome != identity.RotateRotated || res.UserID != "cuser" {
		t.Fatalf("rotate outcome = %v user=%q", res.Outcome, res.UserID)
	}
	// Present the spent root again -> reuse, family revoked.
	res, err = s.RotateRefreshToken(ctx, root, "hash-t3", future)
	if err != nil {
		t.Fatalf("reuse rotate: %v", err)
	}
	if res.Outcome != identity.RotateReuse {
		t.Fatalf("expected RotateReuse, got %v", res.Outcome)
	}
	// t2 is now part of a revoked family -> presenting it is reuse too.
	res, err = s.RotateRefreshToken(ctx, "hash-t2", "hash-t4", future)
	if err != nil {
		t.Fatalf("rotate t2: %v", err)
	}
	if res.Outcome != identity.RotateReuse {
		t.Fatalf("expected revoked-family token to be RotateReuse, got %v", res.Outcome)
	}
}

func TestPgStore_RefreshExpiredAndInvalid(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	// Unknown token.
	if res, _ := s.RotateRefreshToken(ctx, "nope", "x", time.Now().Add(time.Hour)); res.Outcome != identity.RotateInvalid {
		t.Fatalf("unknown token outcome = %v, want RotateInvalid", res.Outcome)
	}
	// Expired token (past expiry).
	if err := s.InsertRefreshRoot(ctx, "hash-old", "fam2", "cuser", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if res, _ := s.RotateRefreshToken(ctx, "hash-old", "x", time.Now().Add(time.Hour)); res.Outcome != identity.RotateExpired {
		t.Fatalf("expired token outcome = %v, want RotateExpired", res.Outcome)
	}
}

func TestPgStore_RevokeFamilyByToken(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	future := time.Now().Add(24 * time.Hour)
	_ = s.InsertRefreshRoot(ctx, "hash-r", "fam3", "cuser", future)
	uid, found, err := s.RevokeFamilyByToken(ctx, "hash-r")
	if err != nil || !found || uid != "cuser" {
		t.Fatalf("revoke: uid=%q found=%v err=%v", uid, found, err)
	}
	// Now the token is revoked -> rotation is reuse.
	if res, _ := s.RotateRefreshToken(ctx, "hash-r", "x", future); res.Outcome != identity.RotateReuse {
		t.Fatalf("revoked token rotate = %v, want RotateReuse", res.Outcome)
	}
	// Unknown token -> not found, no error.
	if _, found, err := s.RevokeFamilyByToken(ctx, "unknown"); err != nil || found {
		t.Fatalf("revoke unknown: found=%v err=%v", found, err)
	}
}

// TestPgStore_ConcurrentRotate asserts the FOR UPDATE lock makes exactly one
// of two concurrent rotations win; the other is treated as reuse.
func TestPgStore_ConcurrentRotate(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)
	future := time.Now().Add(24 * time.Hour)
	_ = s.InsertRefreshRoot(ctx, "hash-c", "famc", "cuser", future)

	var wg sync.WaitGroup
	outcomes := make([]identity.RotateOutcome, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := s.RotateRefreshToken(ctx, "hash-c", fmt.Sprintf("hash-c-next-%d", idx), future)
			if err != nil {
				t.Errorf("rotate %d: %v", idx, err)
				return
			}
			outcomes[idx] = res.Outcome
		}(i)
	}
	wg.Wait()

	rotated, reuse := 0, 0
	for _, o := range outcomes {
		switch o {
		case identity.RotateRotated:
			rotated++
		case identity.RotateReuse:
			reuse++
		}
	}
	if rotated != 1 || reuse != 1 {
		t.Fatalf("concurrent rotate: rotated=%d reuse=%d (want 1/1); outcomes=%v", rotated, reuse, outcomes)
	}
}

// TestE2E_DualAlgWithGoUsersExistence mints an ES256 token for an
// Apple-native go_users user and validates it through the real
// JWTAuthenticator (dual-alg + fail-closed existence). A CUID present in
// neither "User" nor go_users is rejected.
func TestE2E_DualAlgWithGoUsersExistence(t *testing.T) {
	requireDocker(t)
	cleanIdentityTables(t)
	ctx := context.Background()
	s := identity.NewPgStore(testPool)

	if err := s.CreateGoUser(ctx, "capple1", "apple@example.com", "Apple User"); err != nil {
		t.Fatalf("create go user: %v", err)
	}
	ks, err := identity.NewKeystoreFromPEM(genP256PEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	minter := identity.NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour)

	validator := auth.NewJWTAuthenticator("hs-secret", "myrobotaxi", "telemetry", testPool, auth.WithES256Resolver(ks))

	token, _, err := minter.MintAccessToken("capple1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	uid, err := validator.ValidateToken(ctx, token)
	if err != nil {
		t.Fatalf("validate go_users token: %v", err)
	}
	if uid != "capple1" {
		t.Errorf("uid = %q, want capple1", uid)
	}

	// A token for a nonexistent user is rejected by the existence check.
	ghost, _, _ := minter.MintAccessToken("cghost-nonexistent")
	if _, err := validator.ValidateToken(ctx, ghost); err == nil {
		t.Fatal("token for nonexistent user was accepted")
	}
}

// genP256PEM is a local PKCS#8 P-256 PEM generator for the integration file.
func genP256PEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
