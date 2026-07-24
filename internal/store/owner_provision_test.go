package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// ownerSchemaSQL re-creates the slim Prisma-owned shapes the OwnerProvisioner
// writes (MYR-257). It mirrors the classification tables in
// docs/contracts/data-classification.md §1.1/§1.8 and reuses the shared
// "Account" fixture from account_repo_test.go (adding the compound unique index
// the provision upsert arbitrates on). It intentionally omits the full NextAuth
// shape — the cross-repo schema-verification gate (self-serve-onboarding.md §7)
// owns parity with prod Prisma.
const ownerSchemaSQL = `
-- The shared TestMain fixture (db_test.go createSchema) owns a slim "User"
-- (id/name/email). Widen it with the columns the provisioner writes so the
-- fixture mirrors the prod Prisma shape without conflicting with that owner.
ALTER TABLE "User" ADD COLUMN IF NOT EXISTS "emailVerified" TIMESTAMPTZ;
ALTER TABLE "User" ADD COLUMN IF NOT EXISTS "image" TEXT;
ALTER TABLE "User" ADD COLUMN IF NOT EXISTS "createdAt" TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE "User" ADD COLUMN IF NOT EXISTS "updatedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW();
CREATE TABLE IF NOT EXISTS "Settings" (
    "id"                      TEXT PRIMARY KEY,
    "userId"                  TEXT NOT NULL UNIQUE,
    "teslaLinked"             BOOLEAN NOT NULL DEFAULT FALSE,
    "virtualKeyPaired"        BOOLEAN NOT NULL DEFAULT FALSE,
    "keyPairingReminderCount" INTEGER NOT NULL DEFAULT 0,
    "notifyDriveStarted"      BOOLEAN NOT NULL DEFAULT TRUE,
    "notifyDriveCompleted"    BOOLEAN NOT NULL DEFAULT TRUE,
    "notifyChargingComplete"  BOOLEAN NOT NULL DEFAULT TRUE,
    "notifyViewerJoined"      BOOLEAN NOT NULL DEFAULT TRUE,
    "createdAt"               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
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
    "expires_at"        BIGINT,
    "token_type"        TEXT,
    "scope"             TEXT,
    "session_state"     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_account_provider_pai
    ON "Account" ("provider", "providerAccountId");
`

func ensureOwnerSchema(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), ownerSchemaSQL); err != nil {
		t.Fatalf("apply owner schema: %v", err)
	}
}

func cleanOwnerTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{`"Account"`, `"Settings"`, `"User"`} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func newProvisionInput(userID string) store.ProvisionInput {
	return store.ProvisionInput{
		UserID:            userID,
		ProviderAccountID: "tesla-sub-" + userID,
		Name:              "Ada Owner",
		Email:             userID + "@example.com",
		AccessToken:       "access-" + userID,
		RefreshToken:      "refresh-" + userID,
		ExpiresAt:         1893456000,
	}
}

// countRows returns the row count of a table filtered by an equality predicate.
func countRows(t *testing.T, table, col, val string) int {
	t.Helper()
	var n int
	err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM `+table+` WHERE `+col+` = $1`, val).Scan(&n)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOwnerProvisioner_ProvisionTeslaOwner(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping OwnerProvisioner integration test")
	}
	ensureOwnerSchema(t)
	enc := newTestEncryptor(t)
	prov := store.NewOwnerProvisioner(testPool, enc)
	ctx := context.Background()

	t.Run("new user creates all three rows", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cnewuser0001"
		if err := prov.ProvisionTeslaOwner(ctx, newProvisionInput(uid)); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 1 {
			t.Errorf("User rows = %d, want 1", got)
		}
		if got := countRows(t, `"Settings"`, `"userId"`, uid); got != 1 {
			t.Errorf("Settings rows = %d, want 1", got)
		}
		if got := countRows(t, `"Account"`, `"userId"`, uid); got != 1 {
			t.Errorf("Account rows = %d, want 1", got)
		}

		var teslaLinked bool
		if err := testPool.QueryRow(ctx,
			`SELECT "teslaLinked" FROM "Settings" WHERE "userId"=$1`, uid).Scan(&teslaLinked); err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if !teslaLinked {
			t.Error("teslaLinked = false, want true")
		}

		// Tokens dual-written: plaintext present AND ciphertext non-null.
		var accessPT, accessEnc *string
		if err := testPool.QueryRow(ctx,
			`SELECT "access_token","access_token_enc" FROM "Account" WHERE "userId"=$1`, uid).
			Scan(&accessPT, &accessEnc); err != nil {
			t.Fatalf("read account: %v", err)
		}
		if accessPT == nil || *accessPT != "access-"+uid {
			t.Errorf("access_token plaintext = %v, want access-%s", accessPT, uid)
		}
		if accessEnc == nil || *accessEnc == "" {
			t.Error("access_token_enc is null/empty; dual-write not applied")
		}
	})

	t.Run("returning user is idempotent (no duplicates, tokens refreshed)", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "creturning01"
		in := newProvisionInput(uid)
		if err := prov.ProvisionTeslaOwner(ctx, in); err != nil {
			t.Fatalf("first provision: %v", err)
		}
		in.AccessToken = "access-rotated"
		if err := prov.ProvisionTeslaOwner(ctx, in); err != nil {
			t.Fatalf("second provision: %v", err)
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 1 {
			t.Errorf("User rows = %d, want 1 (no dup)", got)
		}
		if got := countRows(t, `"Account"`, `"userId"`, uid); got != 1 {
			t.Errorf("Account rows = %d, want 1 (no dup)", got)
		}
		var accessPT *string
		if err := testPool.QueryRow(ctx,
			`SELECT "access_token" FROM "Account" WHERE "userId"=$1`, uid).Scan(&accessPT); err != nil {
			t.Fatalf("read account: %v", err)
		}
		if accessPT == nil || *accessPT != "access-rotated" {
			t.Errorf("access_token = %v, want access-rotated", accessPT)
		}
	})

	t.Run("existing Prisma user by email is reused, not double-provisioned", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cemailmatch1"
		// Simulate the email-match path: the "User" row already exists (created
		// by the web app / resolved at Apple sign-in), sub == that cuid.
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "User" ("id","name","email","updatedAt") VALUES ($1,'Existing Name','existing@example.com',NOW())`,
			uid); err != nil {
			t.Fatalf("seed existing user: %v", err)
		}
		if err := prov.ProvisionTeslaOwner(ctx, newProvisionInput(uid)); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 1 {
			t.Errorf("User rows = %d, want 1 (reuse, no double-provision)", got)
		}
		// ON CONFLICT DO NOTHING must preserve the pre-existing name/email.
		var name, email string
		if err := testPool.QueryRow(ctx,
			`SELECT "name","email" FROM "User" WHERE "id"=$1`, uid).Scan(&name, &email); err != nil {
			t.Fatalf("read user: %v", err)
		}
		if name != "Existing Name" || email != "existing@example.com" {
			t.Errorf("existing user mutated: name=%q email=%q", name, email)
		}
		// Account still provisioned for the reused user.
		if got := countRows(t, `"Account"`, `"userId"`, uid); got != 1 {
			t.Errorf("Account rows = %d, want 1", got)
		}
	})

	t.Run("empty providerAccountId errors and writes nothing", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cbadinput001"
		in := newProvisionInput(uid)
		in.ProviderAccountID = ""
		if err := prov.ProvisionTeslaOwner(ctx, in); err == nil {
			t.Fatal("expected error for empty providerAccountId, got nil")
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 0 {
			t.Errorf("User rows = %d, want 0 (no orphan on guard failure)", got)
		}
	})

	t.Run("upsert owned vehicle is idempotent on teslaVehicleId", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cvehowner001"
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "User" ("id","updatedAt") VALUES ($1, NOW())`, uid); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM "Vehicle" WHERE "userId"=$1`, uid); err != nil {
			t.Fatalf("clean vehicle: %v", err)
		}
		in := store.OwnedVehicleInput{UserID: uid, TeslaVehicleID: "vid-77", VIN: "5YJ3E1EA7KF000077", Name: "Lunar"}
		if err := prov.UpsertOwnedVehicle(ctx, in); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		in.Name = "Renamed" // must not clobber an existing non-empty name
		if err := prov.UpsertOwnedVehicle(ctx, in); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		var count int
		var name string
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*), MIN("name") FROM "Vehicle" WHERE "teslaVehicleId"='vid-77'`).Scan(&count, &name); err != nil {
			t.Fatalf("read vehicle: %v", err)
		}
		if count != 1 {
			t.Errorf("Vehicle rows = %d, want 1 (idempotent)", count)
		}
		if name != "Lunar" {
			t.Errorf("name = %q, want Lunar preserved", name)
		}
	})

	t.Run("concurrent re-link converges with no duplicates", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cconcurrent1"
		in := newProvisionInput(uid)
		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(idx int) {
				defer wg.Done()
				errs[idx] = prov.ProvisionTeslaOwner(ctx, in)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
			}
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 1 {
			t.Errorf("User rows = %d, want 1 after concurrent provision", got)
		}
		if got := countRows(t, `"Settings"`, `"userId"`, uid); got != 1 {
			t.Errorf("Settings rows = %d, want 1 after concurrent provision", got)
		}
		if got := countRows(t, `"Account"`, `"userId"`, uid); got != 1 {
			t.Errorf("Account rows = %d, want 1 after concurrent provision", got)
		}
	})
}
