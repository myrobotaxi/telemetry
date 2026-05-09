package store_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// encodeBase64 returns the standard-alphabet base64 of b. Used to seed
// ENCRYPTION_KEY for tests that exercise the production loader path.
func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// accountSchemaSQL re-creates the Account columns the telemetry server
// reads/writes during the MYR-62 dual-write window. This is a slim
// fixture — only the columns AccountRepo touches — and intentionally
// omits the surrounding NextAuth shape (compound unique on
// provider+providerAccountId, FK to User, etc.) so the test pool stays
// independent of the Prisma migration ordering.
//
// Cross-repo coupling: when ../my-robo-taxi/prisma/schema.prisma adds or
// renames any *_enc column, mirror the change here in the same PR.
const accountSchemaSQL = `
CREATE TABLE "Account" (
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
    "expires_at"        BIGINT,
    "token_type"        TEXT,
    "scope"             TEXT,
    "session_state"     TEXT
);
`

var accountSchemaOnce sync.Once

func ensureAccountSchema(t *testing.T) {
	t.Helper()
	accountSchemaOnce.Do(func() {
		ctx := context.Background()
		if _, err := testPool.Exec(ctx, accountSchemaSQL); err != nil {
			t.Fatalf("apply Account schema: %v", err)
		}
	})
}

func cleanAccount(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM "Account"`); err != nil {
		t.Fatalf("clean Account: %v", err)
	}
}

// newTestEncryptor builds an AES-256-GCM Encryptor backed by a randomly
// generated key. The KeySet is constructed via LoadKeySetFromEnv to
// exercise the real production loader path (single-key shorthand).
func newTestEncryptor(t *testing.T) cryptox.Encryptor {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", encodeBase64(raw))
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

// seedAccountFull inserts a Tesla Account row with whatever combination
// of plaintext / ciphertext columns the test wants. nil values stay NULL
// in the row.
func seedAccountFull(
	t *testing.T,
	pool *pgxpool.Pool,
	id, userID string,
	accessPT, accessEnc, refreshPT, refreshEnc, idPT, idEnc *string,
	expiresAt *int64,
) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO "Account" (
        "id", "userId", "type", "provider", "providerAccountId",
        "access_token", "access_token_enc",
        "refresh_token", "refresh_token_enc",
        "id_token", "id_token_enc",
        "expires_at"
    ) VALUES ($1,$2,'oauth','tesla',$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, userID, id+"-acct",
		accessPT, accessEnc,
		refreshPT, refreshEnc,
		idPT, idEnc,
		expiresAt,
	)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func ptr(s string) *string { return &s }
func i64(v int64) *int64   { return &v }

func TestAccountRepo_GetTeslaToken_PrefersCiphertext(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping AccountRepo integration test")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	enc := newTestEncryptor(t)
	repo := store.NewAccountRepo(testPool, enc)
	ctx := context.Background()

	accessCT, err := enc.EncryptString("access-real")
	if err != nil {
		t.Fatalf("encrypt access: %v", err)
	}
	refreshCT, err := enc.EncryptString("refresh-real")
	if err != nil {
		t.Fatalf("encrypt refresh: %v", err)
	}
	idCT, err := enc.EncryptString("id-real")
	if err != nil {
		t.Fatalf("encrypt id: %v", err)
	}

	// Plaintext columns hold STALE values to prove the read prefers
	// ciphertext when *_enc is non-NULL.
	seedAccountFull(t, testPool, "acc_pref", "user_pref",
		ptr("STALE-access"), ptr(accessCT),
		ptr("STALE-refresh"), ptr(refreshCT),
		ptr("STALE-id"), ptr(idCT),
		i64(1735689600))

	tok, err := repo.GetTeslaToken(ctx, "user_pref")
	if err != nil {
		t.Fatalf("GetTeslaToken: %v", err)
	}
	if tok.AccessToken != "access-real" {
		t.Errorf("AccessToken: got %q, want %q", tok.AccessToken, "access-real")
	}
	if tok.RefreshToken != "refresh-real" {
		t.Errorf("RefreshToken: got %q, want %q", tok.RefreshToken, "refresh-real")
	}
	if tok.ExpiresAt == nil || *tok.ExpiresAt != 1735689600 {
		t.Errorf("ExpiresAt: got %v, want 1735689600", tok.ExpiresAt)
	}
}

func TestAccountRepo_GetTeslaToken_FallsBackToPlaintext(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	repo := store.NewAccountRepo(testPool, newTestEncryptor(t))
	ctx := context.Background()

	// Pre-encryption row: plaintext present, *_enc NULL.
	seedAccountFull(t, testPool, "acc_pt", "user_pt",
		ptr("plaintext-access"), nil,
		ptr("plaintext-refresh"), nil,
		nil, nil,
		i64(1735689601))

	tok, err := repo.GetTeslaToken(ctx, "user_pt")
	if err != nil {
		t.Fatalf("GetTeslaToken: %v", err)
	}
	if tok.AccessToken != "plaintext-access" {
		t.Errorf("AccessToken fallback: got %q", tok.AccessToken)
	}
	if tok.RefreshToken != "plaintext-refresh" {
		t.Errorf("RefreshToken fallback: got %q", tok.RefreshToken)
	}
}

func TestAccountRepo_GetTeslaToken_NotFoundOnMissingRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	repo := store.NewAccountRepo(testPool, newTestEncryptor(t))
	_, err := repo.GetTeslaToken(context.Background(), "user_missing")
	if !errors.Is(err, store.ErrTeslaTokenNotFound) {
		t.Fatalf("expected ErrTeslaTokenNotFound, got %v", err)
	}
}

func TestAccountRepo_GetTeslaToken_NotFoundOnAllNullTokens(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	repo := store.NewAccountRepo(testPool, newTestEncryptor(t))

	// Both access columns NULL — the row exists but holds no usable token.
	seedAccountFull(t, testPool, "acc_null", "user_null",
		nil, nil,
		ptr("refresh-only"), nil,
		nil, nil,
		nil)

	_, err := repo.GetTeslaToken(context.Background(), "user_null")
	if !errors.Is(err, store.ErrTeslaTokenNotFound) {
		t.Fatalf("expected ErrTeslaTokenNotFound on NULL access_token, got %v", err)
	}
}

func TestAccountRepo_UpdateTeslaToken_DualWritesBothColumns(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	enc := newTestEncryptor(t)
	repo := store.NewAccountRepo(testPool, enc)
	ctx := context.Background()

	// Pre-existing row with plaintext only.
	seedAccountFull(t, testPool, "acc_dw", "user_dw",
		ptr("old-access"), nil,
		ptr("old-refresh"), nil,
		nil, nil,
		i64(0))

	if err := repo.UpdateTeslaToken(ctx, "user_dw", "new-access", "new-refresh", 1735690000); err != nil {
		t.Fatalf("UpdateTeslaToken: %v", err)
	}

	var (
		accessPT, accessEnc   *string
		refreshPT, refreshEnc *string
	)
	row := testPool.QueryRow(ctx,
		`SELECT "access_token","access_token_enc","refresh_token","refresh_token_enc"
         FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla'`, "user_dw")
	if err := row.Scan(&accessPT, &accessEnc, &refreshPT, &refreshEnc); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if accessPT == nil || *accessPT != "new-access" {
		t.Errorf("access plaintext not updated: got %v", accessPT)
	}
	if refreshPT == nil || *refreshPT != "new-refresh" {
		t.Errorf("refresh plaintext not updated: got %v", refreshPT)
	}
	if accessEnc == nil {
		t.Fatal("access_token_enc not written")
	}
	if refreshEnc == nil {
		t.Fatal("refresh_token_enc not written")
	}

	// Decrypt to confirm round-trip integrity end-to-end.
	dec, err := enc.DecryptString(*accessEnc)
	if err != nil || dec != "new-access" {
		t.Errorf("decrypt access_token_enc: got (%q, %v), want (%q, nil)", dec, err, "new-access")
	}
	dec, err = enc.DecryptString(*refreshEnc)
	if err != nil || dec != "new-refresh" {
		t.Errorf("decrypt refresh_token_enc: got (%q, %v), want (%q, nil)", dec, err, "new-refresh")
	}

	// Reading back must surface the new value (read prefers ciphertext).
	got, err := repo.GetTeslaToken(ctx, "user_dw")
	if err != nil {
		t.Fatalf("GetTeslaToken after update: %v", err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Errorf("read-after-write mismatch: got access=%q refresh=%q", got.AccessToken, got.RefreshToken)
	}
}

func TestAccountRepo_UpdateTeslaToken_NotFoundOnMissingRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available")
	}
	ensureAccountSchema(t)
	cleanAccount(t, testPool)

	repo := store.NewAccountRepo(testPool, newTestEncryptor(t))
	err := repo.UpdateTeslaToken(context.Background(), "user_missing", "a", "b", 0)
	if !errors.Is(err, store.ErrTeslaTokenNotFound) {
		t.Fatalf("expected ErrTeslaTokenNotFound, got %v", err)
	}
}

// TestNewAccountRepo_PanicsOnNilEncryptor verifies the constructor's
// fail-loud guard against the dual-write contract being silently
// degraded by a nil Encryptor injection.
func TestNewAccountRepo_PanicsOnNilEncryptor(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil encryptor")
		}
	}()
	store.NewAccountRepo(nil, nil)
}

