package store_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// ownerSchemaSQL re-creates the slim Prisma-owned shapes the OwnerProvisioner
// writes (MYR-257) plus the go_identity_apple binding it converges. It mirrors
// the classification tables in docs/contracts/data-classification.md §1.1/§1.8
// and migration 0003. It widens the shared TestMain "User" fixture and adds the
// Account compound-unique the provision upsert arbitrates on. The cross-repo
// schema-verification gate (self-serve-onboarding.md §7) owns parity with prod.
const ownerSchemaSQL = `
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
CREATE TABLE IF NOT EXISTS go_identity_apple (
    apple_sub     TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    email         TEXT,
    name          TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- MYR-261 removed-vehicle tombstone (migration 0006). Composite natural key.
CREATE TABLE IF NOT EXISTS go_removed_vehicles (
    user_id           TEXT        NOT NULL,
    tesla_vehicle_id  TEXT        NOT NULL,
    vin               TEXT,
    removed_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, tesla_vehicle_id)
);
`

func newTestProvisioner(t *testing.T) *store.OwnerProvisioner {
	t.Helper()
	prov, _ := newTestProvisionerWithEncryptor(t)
	return prov
}

// newTestProvisionerWithEncryptor also hands back the Encryptor the
// provisioner was built with. MYR-433 made the *_enc columns the only
// place Tesla tokens land, so a test that wants to assert on what was
// written has to decrypt it — the plaintext column it used to read is
// deliberately NULL.
func newTestProvisionerWithEncryptor(t *testing.T) (*store.OwnerProvisioner, cryptox.Encryptor) {
	t.Helper()
	enc := newTestEncryptor(t)
	return store.NewOwnerProvisioner(testPool, enc,
		slog.New(slog.NewTextHandler(io.Discard, nil))), enc
}

// assertTokenSealed reads one Account token column pair and asserts the
// MYR-433 shape: plaintext NULL, ciphertext decrypting to want.
//
// Both halves matter. The ciphertext check proves the token survived the
// write; the NULL check proves the server did not also deposit a
// fleet-control credential in a column any operator with a database dump
// can read. Before MYR-433 both columns were written, and the plaintext
// one is what made a dump equivalent to being able to drive the car.
func assertTokenSealed(t *testing.T, enc cryptox.Encryptor, whereCol, whereVal, ptCol, encCol, want string) {
	t.Helper()
	var pt, ct *string
	q := fmt.Sprintf(`SELECT %q, %q FROM "Account" WHERE %q = $1`, ptCol, encCol, whereCol)
	if err := testPool.QueryRow(context.Background(), q, whereVal).Scan(&pt, &ct); err != nil {
		t.Fatalf("read %s/%s: %v", ptCol, encCol, err)
	}
	if pt != nil {
		t.Errorf("%s plaintext = %q, want NULL (MYR-433: tokens are ciphertext-only)", ptCol, *pt)
	}
	if ct == nil || *ct == "" {
		t.Fatalf("%s is null/empty; the token was not persisted at all", encCol)
	}
	got, err := enc.DecryptString(*ct)
	if err != nil {
		t.Fatalf("decrypt %s: %v", encCol, err)
	}
	if got != want {
		t.Errorf("%s decrypts to %q, want %q", encCol, got, want)
	}
}

func ensureOwnerSchema(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), ownerSchemaSQL); err != nil {
		t.Fatalf("apply owner schema: %v", err)
	}
}

func cleanOwnerTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{`"Account"`, `"Settings"`, `go_identity_apple`, `go_removed_vehicles`, `"User"`} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func seedOwnerUser(t *testing.T, id, name, email string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "User" ("id","name","email","updatedAt") VALUES ($1, NULLIF($2,''), NULLIF($3,''), NOW())`,
		id, name, email); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

func seedAppleBinding(t *testing.T, appleSub, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_identity_apple (apple_sub, user_id) VALUES ($1,$2)`, appleSub, userID); err != nil {
		t.Fatalf("seed apple binding: %v", err)
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
	prov, enc := newTestProvisionerWithEncryptor(t)
	ctx := context.Background()

	t.Run("new user creates all three rows", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cnewuser0001"
		res, err := prov.ProvisionTeslaOwner(ctx, newProvisionInput(uid))
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.CanonicalUserID != uid || res.Outcome != store.OutcomeNewUser {
			t.Errorf("result = %+v, want canonical=%s outcome=new_user", res, uid)
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

		// MYR-433: provisioning seals both tokens and writes no readable copy.
		assertTokenSealed(t, enc, `userId`, uid, "access_token", "access_token_enc", "access-"+uid)
		assertTokenSealed(t, enc, `userId`, uid, "refresh_token", "refresh_token_enc", "refresh-"+uid)
	})

	t.Run("returning user is idempotent (no duplicates, tokens refreshed)", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "creturning01"
		in := newProvisionInput(uid)
		if _, err := prov.ProvisionTeslaOwner(ctx, in); err != nil {
			t.Fatalf("first provision: %v", err)
		}
		in.AccessToken = "access-rotated"
		if _, err := prov.ProvisionTeslaOwner(ctx, in); err != nil {
			t.Fatalf("second provision: %v", err)
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 1 {
			t.Errorf("User rows = %d, want 1 (no dup)", got)
		}
		if got := countRows(t, `"Account"`, `"userId"`, uid); got != 1 {
			t.Errorf("Account rows = %d, want 1 (no dup)", got)
		}
		// The rotated token lands in the ciphertext column; the plaintext
		// column stays NULL across the re-link rather than being refreshed.
		assertTokenSealed(t, enc, `userId`, uid, "access_token", "access_token_enc", "access-rotated")
	})

	t.Run("Apple-verified email matches existing web user -> adopt, one User row", func(t *testing.T) {
		cleanOwnerTables(t)
		// Existing web user (created via NextAuth long ago).
		const webID, webEmail = "cwebuser0001", "crossover@web.example"
		seedOwnerUser(t, webID, "Web Ada", webEmail)
		// Caller: a fresh go_users id whose Apple-VERIFIED email equals the web
		// email (e.g. they signed in with Apple sharing their real address).
		const appleID, appleSub = "cappleid0001", "apple-sub-xyz"
		seedAppleBinding(t, appleSub, appleID)

		in := newProvisionInput(appleID)
		in.Email = webEmail // OUR Apple-verified email (never a Tesla-provided one)
		res, err := prov.ProvisionTeslaOwner(ctx, in)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.CanonicalUserID != webID || res.Outcome != store.OutcomeAdoptedByEmail {
			t.Fatalf("result = %+v, want canonical=%s outcome=adopted_by_email", res, webID)
		}
		// No colliding INSERT: exactly one User (the web user), none for appleID.
		if got := countRows(t, `"User"`, `"id"`, appleID); got != 0 {
			t.Errorf("User rows for appleID = %d, want 0 (adopted, no new row)", got)
		}
		var total int
		if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM "User"`).Scan(&total); err != nil {
			t.Fatalf("count users: %v", err)
		}
		if total != 1 {
			t.Errorf("total User rows = %d, want 1", total)
		}
		// Apple binding converged onto the web user.
		var boundTo string
		if err := testPool.QueryRow(ctx,
			`SELECT user_id FROM go_identity_apple WHERE apple_sub=$1`, appleSub).Scan(&boundTo); err != nil {
			t.Fatalf("read binding: %v", err)
		}
		if boundTo != webID {
			t.Errorf("apple binding = %s, want %s (converged)", boundTo, webID)
		}
		// Account + Settings live under the canonical web user.
		if got := countRows(t, `"Account"`, `"userId"`, webID); got != 1 {
			t.Errorf("Account rows for webID = %d, want 1", got)
		}
	})

	t.Run("unverified Tesla email does NOT auto-adopt: empty Apple email -> fresh user", func(t *testing.T) {
		cleanOwnerTables(t)
		// Existing web user whose email equals what Tesla would report. The caller
		// signed in with Apple Hide-My-Email, so their Apple-verified email is
		// empty. owner_link never passes the Tesla email as the match anchor — the
		// provisioner sees Email="" — so path (b) must NOT fire. With no matching
		// Tesla account either, the caller provisions a FRESH user (c).
		const webID, sharedEmail = "cwebuser0002", "shared@web.example"
		seedOwnerUser(t, webID, "Web Bob", sharedEmail)
		const appleID, appleSub = "chidden00001", "apple-sub-hidden"
		seedAppleBinding(t, appleSub, appleID)

		in := newProvisionInput(appleID)
		in.Email = "" // Apple hidden relay — no verified email; Tesla email NOT used
		res, err := prov.ProvisionTeslaOwner(ctx, in)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.CanonicalUserID != appleID || res.Outcome != store.OutcomeNewUser {
			t.Fatalf("result = %+v, want canonical=%s outcome=new_user (no unverified adopt)", res, appleID)
		}
		// The existing web user must be untouched (not adopted, not merged).
		if got := countRows(t, `"Account"`, `"userId"`, webID); got != 0 {
			t.Errorf("Account rows for webID = %d, want 0 (web user untouched)", got)
		}
		// A fresh User exists for the caller, distinct from the web user.
		if got := countRows(t, `"User"`, `"id"`, appleID); got != 1 {
			t.Errorf("User rows for appleID = %d, want 1 (fresh user)", got)
		}
		// Apple binding stays on the caller (not converged onto the web user).
		var boundTo string
		if err := testPool.QueryRow(ctx,
			`SELECT user_id FROM go_identity_apple WHERE apple_sub=$1`, appleSub).Scan(&boundTo); err != nil {
			t.Fatalf("read binding: %v", err)
		}
		if boundTo != appleID {
			t.Errorf("apple binding = %s, want %s (unchanged)", boundTo, appleID)
		}
	})

	t.Run("cross-user relink keeps original owner, converges caller, no rewrite", func(t *testing.T) {
		cleanOwnerTables(t)
		const ownerA, callerB = "cownerA00001", "ccallerB0001"
		const teslaSub, appleSubB = "tesla-sub-shared", "apple-sub-b"
		seedOwnerUser(t, ownerA, "Owner A", "a@example.com")
		seedOwnerUser(t, callerB, "Caller B", "b@example.com")
		seedAppleBinding(t, appleSubB, callerB)
		// A already owns the Tesla account.
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Account" ("id","userId","type","provider","providerAccountId","access_token")
			 VALUES ('acct-a', $1, 'oauth', 'tesla', $2, 'old-access')`, ownerA, teslaSub); err != nil {
			t.Fatalf("seed A account: %v", err)
		}

		in := newProvisionInput(callerB)
		in.ProviderAccountID = teslaSub
		in.AccessToken = "b-new-access"
		res, err := prov.ProvisionTeslaOwner(ctx, in)
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		if res.CanonicalUserID != ownerA || res.Outcome != store.OutcomeAdoptedByAccount {
			t.Fatalf("result = %+v, want canonical=%s outcome=adopted_by_account", res, ownerA)
		}
		// Account.userId must NOT be rewritten to B.
		var acctUser string
		if err := testPool.QueryRow(ctx,
			`SELECT "userId" FROM "Account" WHERE "providerAccountId"=$1`, teslaSub).Scan(&acctUser); err != nil {
			t.Fatalf("read account: %v", err)
		}
		if acctUser != ownerA {
			t.Errorf("Account.userId = %s, want %s (never reassigned)", acctUser, ownerA)
		}
		if got := countRows(t, `"Account"`, `"userId"`, callerB); got != 0 {
			t.Errorf("Account rows for B = %d, want 0 (B never owns it)", got)
		}
		// B's apple identity converged onto A.
		var boundTo string
		if err := testPool.QueryRow(ctx,
			`SELECT user_id FROM go_identity_apple WHERE apple_sub=$1`, appleSubB).Scan(&boundTo); err != nil {
			t.Fatalf("read binding: %v", err)
		}
		if boundTo != ownerA {
			t.Errorf("B apple binding = %s, want %s", boundTo, ownerA)
		}
		// Tokens updated on A's account (owner keeps ownership, tokens refreshed).
		// The row was seeded with a pre-MYR-433 plaintext access_token; the
		// re-link must scrub it to NULL rather than refresh it in place —
		// re-linking is the one moment this server can clean up a legacy
		// credential it would otherwise leave sitting readable forever.
		assertTokenSealed(t, enc, `providerAccountId`, teslaSub,
			"access_token", "access_token_enc", "b-new-access")
		// Only A's Settings flipped; B's Settings must not be created stale.
		if got := countRows(t, `"Settings"`, `"userId"`, ownerA); got != 1 {
			t.Errorf("A Settings = %d, want 1", got)
		}
		if got := countRows(t, `"Settings"`, `"userId"`, callerB); got != 0 {
			t.Errorf("B Settings = %d, want 0 (never linked)", got)
		}
	})

	t.Run("empty providerAccountId errors and writes nothing", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cbadinput001"
		in := newProvisionInput(uid)
		in.ProviderAccountID = ""
		if _, err := prov.ProvisionTeslaOwner(ctx, in); err == nil {
			t.Fatal("expected error for empty providerAccountId, got nil")
		}
		if got := countRows(t, `"User"`, `"id"`, uid); got != 0 {
			t.Errorf("User rows = %d, want 0 (no orphan on guard failure)", got)
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
				_, errs[idx] = prov.ProvisionTeslaOwner(ctx, in)
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

func TestOwnerProvisioner_UpsertOwnedVehicle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping vehicle upsert integration test")
	}
	ensureOwnerSchema(t)
	prov := newTestProvisioner(t)
	ctx := context.Background()

	t.Run("idempotent on teslaVehicleId, preserves live name", func(t *testing.T) {
		cleanOwnerTables(t)
		uid := "cvehowner001"
		seedOwnerUser(t, uid, "", "")
		if _, err := testPool.Exec(ctx, `DELETE FROM "Vehicle" WHERE "teslaVehicleId"='vid-77'`); err != nil {
			t.Fatalf("clean vehicle: %v", err)
		}
		in := store.OwnedVehicleInput{UserID: uid, TeslaVehicleID: "vid-77", VIN: "5YJ3E1EA7KF000077", Name: "Lunar"}
		out, err := prov.UpsertOwnedVehicle(ctx, in)
		if err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		if out != store.VehicleOwned {
			t.Errorf("outcome = %q, want owned", out)
		}
		in.Name = "Renamed" // must not clobber an existing non-empty name
		if _, err := prov.UpsertOwnedVehicle(ctx, in); err != nil {
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

	t.Run("cross-user teslaVehicleId is skipped, never reassigned", func(t *testing.T) {
		cleanOwnerTables(t)
		const ownerA, callerB = "cvehownerA01", "cvehcallerB1"
		seedOwnerUser(t, ownerA, "", "")
		seedOwnerUser(t, callerB, "", "")
		if _, err := testPool.Exec(ctx, `DELETE FROM "Vehicle" WHERE "teslaVehicleId"='vid-shared'`); err != nil {
			t.Fatalf("clean vehicle: %v", err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Vehicle" ("id","userId","teslaVehicleId","vin","name","updatedAt")
			 VALUES ('veh-a', $1, 'vid-shared', '5YJ3E1EA7KF000088', 'A car', NOW())`, ownerA); err != nil {
			t.Fatalf("seed A vehicle: %v", err)
		}
		out, err := prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
			UserID: callerB, TeslaVehicleID: "vid-shared", VIN: "5YJ3E1EA7KF000088", Name: "B tries",
		})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if out != store.VehicleSkippedCrossUser {
			t.Errorf("outcome = %q, want skipped_cross_user", out)
		}
		var owner string
		if err := testPool.QueryRow(ctx,
			`SELECT "userId" FROM "Vehicle" WHERE "teslaVehicleId"='vid-shared'`).Scan(&owner); err != nil {
			t.Fatalf("read vehicle: %v", err)
		}
		if owner != ownerA {
			t.Errorf("Vehicle.userId = %s, want %s (never reassigned)", owner, ownerA)
		}
	})
}
