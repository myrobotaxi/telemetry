package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/identity"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-488 — an identity convergence must carry the person's live session.
//
// THE FIELD INCIDENT. James Guan signed in with Apple, got a go_users id of his
// own and a refresh-token family under it, then linked the Tesla that a
// legacy Prisma user already owned. The MYR-452 convergence correctly re-pointed
// his Apple binding onto that owner — and left his device's refresh family
// filed under the id it had just abandoned. go_refresh_tokens had ZERO rows for
// the canonical user. His phone went on holding a session for a subject that
// owned no vehicle, no share and no settings, so Settings rendered his cached
// profile above a confident "No vehicles linked to this account" while the car
// itself was provisioned, key-paired and streaming frames.
//
// These tests run the REAL identity service over the REAL provisioner against a
// real Postgres, because every part of the bug lives in the seam between the
// two: sign in for real, converge for real, then refresh with the token the
// device is actually holding and insist it comes back naming the canonical user.

const (
	sessAppleSub = "001432.session.migration.4953"
	sessAppleMai = "james-session@privaterelay.appleid.com"
	sessTeslaSub = "tesla-owner-sub-9274"
	sessCanonID  = "cmnccy4tf0000ky04sessmig"
	sessCanonMai = "owner-9274@example.com"
)

// setupSessionMigration installs the go_ migrations, the Prisma-owned shapes the
// provisioner writes, and a clean slate.
func setupSessionMigration(t *testing.T) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping MYR-488 session-migration integration test")
	}
	mustApplyGoMigrations(t)
	ensureOwnerSchema(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_refresh_tokens`,
		`DELETE FROM go_identity_convergence`,
		`DELETE FROM go_identity_apple`,
		`DELETE FROM go_users`,
		`DELETE FROM "Account"`,
		`DELETE FROM "Settings"`,
		`DELETE FROM "User"`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
}

// seedCanonicalTeslaOwner plants the legacy owner James's link converged onto: a
// Prisma "User" with NO go_users row (his real shape) that already owns the
// Tesla "Account". Its email deliberately differs from the Apple one so the
// sign-in email-match cannot adopt it early — the convergence must come from the
// Tesla account, exactly as it did in production.
func seedCanonicalTeslaOwner(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "User" ("id", "email", "updatedAt") VALUES ($1, $2, NOW())`,
		sessCanonID, sessCanonMai); err != nil {
		t.Fatalf("seed canonical User: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Account" ("id", "userId", "type", "provider", "providerAccountId")
		 VALUES ($1, $2, 'oauth', 'tesla', $3)`,
		"cacct-sessmig-0000000001", sessCanonID, sessTeslaSub); err != nil {
		t.Fatalf("seed Account: %v", err)
	}
}

// linkTesla runs the real provisioner for the caller and insists the link
// converged onto the canonical owner.
func linkTesla(t *testing.T, callerID string) {
	t.Helper()
	//nolint:gosec // G101: fixed test fixtures, not credentials.
	out, err := newTestProvisioner(t).ProvisionTeslaOwner(context.Background(), store.ProvisionInput{
		UserID:            callerID,
		ProviderAccountID: sessTeslaSub,
		Email:             sessAppleMai,
		AccessToken:       "tesla-access",
		RefreshToken:      "tesla-refresh",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("provision tesla owner: %v", err)
	}
	if out.CanonicalUserID != sessCanonID {
		t.Fatalf("convergence target = %q, want %q", out.CanonicalUserID, sessCanonID)
	}
}

func refreshRowsFor(t *testing.T, userID string) int {
	t.Helper()
	return countQuery(t, `SELECT count(*) FROM go_refresh_tokens WHERE user_id = $1`, userID)
}

// usableRowsFor counts rows a device could still rotate: the exact predicate the
// migration keys on, and the exact thing that must never be left stranded.
func usableRowsFor(t *testing.T, userID string) int {
	t.Helper()
	return countQuery(t, `
		SELECT count(*) FROM go_refresh_tokens
		WHERE user_id = $1 AND revoked = FALSE AND rotated_to IS NULL AND expires_at > NOW()`,
		userID)
}

// TestConvergenceCarriesTheLiveSession is the MYR-488 regression: the device
// that was signed in before the Tesla link must still be signed in after it, as
// the canonical user.
func TestConvergenceCarriesTheLiveSession(t *testing.T) {
	setupSessionMigration(t)
	ctx := context.Background()
	seedCanonicalTeslaOwner(t)

	svc := newSignInService(t, sessAppleMai)
	signIn, err := svc.SignInWithApple(ctx, identity.AppleSignInInput{IdentityToken: sessAppleSub})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	callerID := signIn.User.ID
	if callerID == sessCanonID {
		t.Fatalf("precondition: sign-in must mint its own id, got the canonical one")
	}
	if got := usableRowsFor(t, callerID); got != 1 {
		t.Fatalf("precondition: caller has %d usable refresh rows, want 1", got)
	}
	var familyBefore string
	if err := testPool.QueryRow(ctx,
		`SELECT family_id FROM go_refresh_tokens WHERE user_id = $1`, callerID).Scan(&familyBefore); err != nil {
		t.Fatalf("read family: %v", err)
	}

	linkTesla(t, callerID)

	// The session moved with the binding, whole.
	if got := refreshRowsFor(t, callerID); got != 0 {
		t.Errorf("abandoned id still holds %d refresh rows, want 0 — this is the zombie", got)
	}
	if got := usableRowsFor(t, sessCanonID); got != 1 {
		t.Fatalf("canonical user has %d usable refresh rows, want 1", got)
	}
	var familyAfter string
	if err := testPool.QueryRow(ctx,
		`SELECT family_id FROM go_refresh_tokens WHERE user_id = $1`, sessCanonID).Scan(&familyAfter); err != nil {
		t.Fatalf("read migrated family: %v", err)
	}
	if familyAfter != familyBefore {
		t.Errorf("family_id changed across the migration: %q -> %q; rotation lineage must be continuous",
			familyBefore, familyAfter)
	}

	// THE ASSERTION THAT IS THE WHOLE POINT: the token the phone is holding still
	// rotates, and what comes back names the user that owns the car.
	rotated, err := svc.Refresh(ctx, signIn.RefreshToken)
	if err != nil {
		t.Fatalf("refresh with the pre-convergence token: %v", err)
	}
	if rotated.User.ID != sessCanonID {
		t.Errorf("refresh resolved to %q, want the canonical owner %q", rotated.User.ID, sessCanonID)
	}
	if got := refreshRowsFor(t, callerID); got != 0 {
		t.Errorf("rotation re-planted %d rows under the abandoned id", got)
	}
	if got := refreshRowsFor(t, sessCanonID); got != 2 {
		t.Errorf("canonical user has %d refresh rows after rotation, want 2 (spent root + new tip)", got)
	}
}

// TestConvergenceSessionMigrationEdgeCases pins what moves and what deliberately
// does not. Each case seeds one family in a given shape under its own caller,
// converges, and asserts where the rows ended up.
func TestConvergenceSessionMigrationEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		// seed plants the case's families under callerID.
		seed func(t *testing.T, callerID string)
		// wantMoved / wantLeft are row counts under the canonical and the
		// abandoned id; wantRevoked pins the revoked total across both, so the
		// no-resurrection check has an exact expectation rather than a range.
		wantMoved   int
		wantLeft    int
		wantRevoked int
	}{
		{
			// A family is a rotation lineage, and it moves whole: the spent
			// ancestors go with the live tip so reuse-detection (which revokes by
			// family_id) keeps attributing a theft to one identity, not two.
			name: "live family moves whole, spent ancestors included",
			seed: func(t *testing.T, callerID string) {
				t.Helper()
				seedFamilyRow(t, callerID, "fam-live", "h-root", spentRow)
				seedFamilyRow(t, callerID, "fam-live", "h-tip", liveRow)
			},
			wantMoved: 2,
		},
		{
			// Revocation is a decision somebody made — a sign-out, or
			// reuse-detection catching a stolen token. A convergence is not a
			// reason to revisit it, and moving it would relocate the evidence.
			name: "revoked family stays put and stays revoked",
			seed: func(t *testing.T, callerID string) {
				t.Helper()
				seedFamilyRow(t, callerID, "fam-revoked", "h-revoked", revokedRow)
			},
			wantLeft:    1,
			wantRevoked: 1,
		},
		{
			// Nothing to preserve: the person signs in again either way, and that
			// sign-in resolves through the binding that just moved.
			name: "expired family stays put",
			seed: func(t *testing.T, callerID string) {
				t.Helper()
				seedFamilyRow(t, callerID, "fam-expired", "h-expired", expiredRow)
			},
			wantLeft: 1,
		},
		{
			name: "a live family moves while a revoked sibling is left behind",
			seed: func(t *testing.T, callerID string) {
				t.Helper()
				seedFamilyRow(t, callerID, "fam-live2", "h-live2", liveRow)
				seedFamilyRow(t, callerID, "fam-dead2", "h-dead2", revokedRow)
			},
			wantMoved:   1,
			wantLeft:    1,
			wantRevoked: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupSessionMigration(t)
			seedCanonicalTeslaOwner(t)
			const callerID = "ccaller-edgecase-00000001"
			seedAppleBinding(t, sessAppleSub, callerID)
			tt.seed(t, callerID)

			linkTesla(t, callerID)

			if got := refreshRowsFor(t, sessCanonID); got != tt.wantMoved {
				t.Errorf("canonical user holds %d rows, want %d", got, tt.wantMoved)
			}
			if got := refreshRowsFor(t, callerID); got != tt.wantLeft {
				t.Errorf("abandoned id holds %d rows, want %d", got, tt.wantLeft)
			}
			// Nothing this code does may ever clear `revoked` — a convergence
			// must not be a way to bring a dead family back to life — and it
			// must not revoke anything either.
			if got := countQuery(t,
				`SELECT count(*) FROM go_refresh_tokens WHERE revoked = TRUE`); got != tt.wantRevoked {
				t.Errorf("revoked row count = %d, want %d; a revoked family was resurrected or one was created",
					got, tt.wantRevoked)
			}
		})
	}
}

// rowShape describes one seeded go_refresh_tokens row.
type rowShape int

const (
	liveRow rowShape = iota
	spentRow
	revokedRow
	expiredRow
)

func seedFamilyRow(t *testing.T, userID, familyID, hash string, shape rowShape) {
	t.Helper()
	const q = `
		INSERT INTO go_refresh_tokens
			(token_hash, family_id, user_id, expires_at, rotated_to, revoked, revoked_at, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	var (
		expires  = "90 days"
		rotated  *string
		revoked  bool
		revokedT *time.Time
		reason   *string
	)
	switch shape {
	case liveRow:
	case spentRow:
		next := hash + "-next"
		r := "rotated"
		rotated, reason = &next, &r
	case revokedRow:
		now := time.Now().UTC()
		r := "revoked"
		revoked, revokedT, reason = true, &now, &r
	case expiredRow:
		expires = "-1 days"
	}
	if _, err := testPool.Exec(context.Background(), q,
		hash, familyID, userID, timeFromNow(t, expires), rotated, revoked, revokedT, reason); err != nil {
		t.Fatalf("seed refresh row %s: %v", hash, err)
	}
}

// timeFromNow resolves a Postgres interval literal against the DATABASE clock,
// so a fixture's notion of "expired" is the same one the migration's predicate
// uses. Comparing against the test process's time.Now() would be a second clock.
func timeFromNow(t *testing.T, interval string) time.Time {
	t.Helper()
	var ts time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT NOW() + $1::interval`, interval).Scan(&ts); err != nil {
		t.Fatalf("resolve interval %s: %v", interval, err)
	}
	return ts
}

// rotationsPerRound is how many times the racing device refreshes, back to back,
// while the convergence runs.
//
// It is deliberately more than one, and that is the whole design of this test.
// FOR UPDATE row locks are held to end of transaction, so against a SINGLE
// rotation the outcome is already forced: either the rotation got the tip's lock
// first — in which case it has fully committed by the time the convergence's
// locking SELECT returns, and the migrating UPDATE's later snapshot sees
// everything it inserted — or the convergence got there first, and the rotation
// blocks until commit and then re-reads a user_id that has moved.
//
// A CHAIN of rotations is what breaks that symmetry. The first one commits a row
// the convergence's SELECT never saw and therefore never locked; the second is
// free to rotate THAT row in the window before the migrating UPDATE runs, and
// the row it inserts postdates that UPDATE's snapshot. Nothing but the re-sweep
// catches it. Setting maxRefreshMigrationPasses to 1 makes this test fail.
const rotationsPerRound = 3

// TestConvergenceRaceWithConcurrentRefresh runs the convergence against a device
// refreshing in a tight loop, repeatedly, under -race.
//
// The invariant asserted is the strongest useful one and holds for every
// interleaving: when both transactions have finished, the abandoned id may hold
// evidence (spent, revoked, expired rows) but must NOT hold a single token a
// device could still rotate. That is the zombie, and there must be no way to
// arrive at it.
func TestConvergenceRaceWithConcurrentRefresh(t *testing.T) {
	setupSessionMigration(t)
	ctx := context.Background()

	const rounds = 12
	for round := range rounds {
		setupSessionMigration(t)
		seedCanonicalTeslaOwner(t)

		svc := newSignInService(t, sessAppleMai)
		signIn, err := svc.SignInWithApple(ctx, identity.AppleSignInInput{IdentityToken: sessAppleSub})
		if err != nil {
			t.Fatalf("round %d: sign in: %v", round, err)
		}
		callerID := signIn.User.ID

		// A family that was revoked before the race began. No interleaving may
		// bring it back, and it may not be dragged onto the canonical user.
		seedFamilyRow(t, callerID, "fam-prerevoked", "h-prerevoked", revokedRow)

		var (
			wg        sync.WaitGroup
			rotations int
			lastUser  string
			refreshEr error
			linkErr   error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			token := signIn.RefreshToken
			for range rotationsPerRound {
				res, rErr := svc.Refresh(ctx, token)
				if rErr != nil {
					refreshEr = rErr
					return
				}
				token, lastUser = res.RefreshToken, res.User.ID
				rotations++
			}
		}()
		go func() {
			defer wg.Done()
			//nolint:gosec // G101: fixed test fixtures, not credentials.
			_, linkErr = newTestProvisioner(t).ProvisionTeslaOwner(ctx, store.ProvisionInput{
				UserID:            callerID,
				ProviderAccountID: sessTeslaSub,
				Email:             sessAppleMai,
				AccessToken:       "tesla-access",
				RefreshToken:      "tesla-refresh",
				ExpiresAt:         time.Now().Add(time.Hour).Unix(),
			})
		}()
		wg.Wait()

		if linkErr != nil {
			t.Fatalf("round %d: provision raced with refresh: %v", round, linkErr)
		}
		// A refresh may be refused, but only honestly: an unknown/expired token
		// or a reuse detection. Anything else is an infrastructure failure the
		// migration introduced.
		if refreshEr != nil &&
			!errors.Is(refreshEr, identity.ErrInvalidRefreshToken) &&
			!errors.Is(refreshEr, identity.ErrRefreshReuseDetected) {
			t.Fatalf("round %d: refresh: %v", round, refreshEr)
		}

		// THE INVARIANT. No rotatable token may be left on the dead identity.
		if got := usableRowsFor(t, callerID); got != 0 {
			t.Fatalf("round %d: %d usable refresh rows stranded on the abandoned id — zombie session",
				round, got)
		}
		// The pre-revoked family stayed dead and stayed where it happened.
		if got := countQuery(t, `
			SELECT count(*) FROM go_refresh_tokens
			WHERE family_id = 'fam-prerevoked' AND user_id = $1 AND revoked = TRUE`, callerID); got != 1 {
			t.Fatalf("round %d: the pre-revoked family was moved or resurrected (matched %d)", round, got)
		}
		// Migration moves rows; it never copies them. One pre-revoked row, one
		// sign-in root, one more per rotation that got through.
		if got, want := countQuery(t, `SELECT count(*) FROM go_refresh_tokens`), 2+rotations; got != want {
			t.Fatalf("round %d: %d refresh rows total, want %d — rows were duplicated or lost",
				round, got, want)
		}
		if rotations > 0 && lastUser == "" {
			t.Fatalf("round %d: refresh returned an empty user", round)
		}
	}
}

// TestConvergenceBlocksOnAnInFlightRotationAndStillMigrates forces the one
// interleaving that luck alone will not reliably produce, and that got the first
// version of this code wrong.
//
// A rotation holds the tip's row lock and has already inserted its successor,
// uncommitted, when the convergence arrives. The convergence's locking SELECT
// blocks. When it is released, the tip it can finally read has been marked SPENT
// — and the successor that replaced it was inserted after the SELECT's snapshot,
// so it is not there at all. A migration that asked "is there a rotatable token
// here?" answered no and migrated nothing, leaving the device holding a live
// family on a dead identity: the original bug, reproduced by the fix.
//
// The rotation is replayed statement-for-statement from
// identity.RotateRefreshToken rather than called through it, because the test
// has to hold the transaction open across the convergence — which the real
// method, correctly, does not let its caller do.
func TestConvergenceBlocksOnAnInFlightRotationAndStillMigrates(t *testing.T) {
	setupSessionMigration(t)
	ctx := context.Background()
	seedCanonicalTeslaOwner(t)

	svc := newSignInService(t, sessAppleMai)
	signIn, err := svc.SignInWithApple(ctx, identity.AppleSignInInput{IdentityToken: sessAppleSub})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	callerID := signIn.User.ID

	var rootHash, familyID string
	if err := testPool.QueryRow(ctx,
		`SELECT token_hash, family_id FROM go_refresh_tokens WHERE user_id = $1`, callerID).
		Scan(&rootHash, &familyID); err != nil {
		t.Fatalf("read sign-in family: %v", err)
	}

	rot, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rotation: %v", err)
	}
	defer func() { _ = rot.Rollback(ctx) }()

	const nextHash = "hash-of-the-successor-token"
	var lockedUser string
	if err := rot.QueryRow(ctx,
		`SELECT user_id FROM go_refresh_tokens WHERE token_hash = $1 FOR UPDATE`, rootHash).
		Scan(&lockedUser); err != nil {
		t.Fatalf("lock tip: %v", err)
	}
	if _, err := rot.Exec(ctx,
		`UPDATE go_refresh_tokens SET rotated_to = $2, reason = 'rotated' WHERE token_hash = $1`,
		rootHash, nextHash); err != nil {
		t.Fatalf("mark spent: %v", err)
	}
	if _, err := rot.Exec(ctx, `
		INSERT INTO go_refresh_tokens (token_hash, family_id, user_id, expires_at, rotated_from)
		VALUES ($1, $2, $3, $4, $5)`,
		nextHash, familyID, lockedUser, timeFromNow(t, "90 days"), rootHash); err != nil {
		t.Fatalf("insert successor: %v", err)
	}

	// The convergence now runs into the lock the rotation is holding.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		linkTesla(t, callerID)
	}()
	waitForLockWaiter(t)

	if err := rot.Commit(ctx); err != nil {
		t.Fatalf("commit rotation: %v", err)
	}
	wg.Wait()

	// Both rows — the spent tip the convergence could see and the successor it
	// could not — end up under the canonical user.
	if got := refreshRowsFor(t, callerID); got != 0 {
		t.Errorf("abandoned id still holds %d refresh rows, want 0", got)
	}
	if got := refreshRowsFor(t, sessCanonID); got != 2 {
		t.Errorf("canonical user holds %d refresh rows, want 2 (spent tip + successor)", got)
	}
	if got := usableRowsFor(t, sessCanonID); got != 1 {
		t.Errorf("canonical user holds %d rotatable rows, want 1", got)
	}
}

// waitForLockWaiter blocks until some backend on this database is stuck waiting
// on a lock, so the test never races ahead of the transaction it is trying to
// block. Polling pg_stat_activity is what makes the interleaving deterministic
// rather than a sleep long enough to usually work.
func waitForLockWaiter(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n := countQuery(t, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`)
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no backend ever blocked on a lock; the interleaving under test did not happen")
}

// orphanRepairSQL is the operator artifact shipped for the families that were
// already orphaned when the fix landed.
const orphanRepairSQL = "../../docs/migrations/myr-488-orphaned-session-repair.sql"

// TestOrphanedSessionRepairSQL runs the shipped operator artifact against a real
// database, on the exact shape production is in.
//
// A repair script nobody executes in CI is a script that rots: the columns move,
// the predicate drifts, and the first time anyone finds out is the night they
// paste it into a production console. This test is what makes the file a tested
// artifact rather than a suggestion — and it pins the two properties an operator
// is being asked to trust, that live families move and dead ones are left alone.
func TestOrphanedSessionRepairSQL(t *testing.T) {
	setupSessionMigration(t)
	ctx := context.Background()

	const (
		abandoned = "cd38cb3eba91a3b9b69ff6eee5aedcc29"
		canonical = "cmnccy4tf0000ky04coe4is96"
		stranger  = "cstranger000000000000001"
	)
	// The damage: a convergence that already happened, with the session left
	// behind under the abandoned id — spent root and live tip, plus a revoked
	// family and an expired one that must not be dragged along.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_identity_convergence (from_user_id, to_user_id) VALUES ($1, $2)`,
		abandoned, canonical); err != nil {
		t.Fatalf("seed convergence edge: %v", err)
	}
	seedFamilyRow(t, abandoned, "fam-orphan", "h-orphan-root", spentRow)
	seedFamilyRow(t, abandoned, "fam-orphan", "h-orphan-tip", liveRow)
	seedFamilyRow(t, abandoned, "fam-orphan-dead", "h-orphan-dead", revokedRow)
	seedFamilyRow(t, abandoned, "fam-orphan-old", "h-orphan-old", expiredRow)
	// Somebody with no convergence edge at all. The repair must not see them.
	seedFamilyRow(t, stranger, "fam-stranger", "h-stranger", liveRow)

	runSQLFile(t, orphanRepairSQL)

	if got := refreshRowsFor(t, canonical); got != 2 {
		t.Errorf("canonical user holds %d rows, want the 2-row live family", got)
	}
	if got := usableRowsFor(t, abandoned); got != 0 {
		t.Errorf("%d rotatable rows still stranded on the abandoned id", got)
	}
	if got := countQuery(t, `
		SELECT count(*) FROM go_refresh_tokens
		WHERE user_id = $1 AND family_id IN ('fam-orphan-dead', 'fam-orphan-old')`, abandoned); got != 2 {
		t.Errorf("the revoked and expired families did not stay put (matched %d, want 2)", got)
	}
	if got := countQuery(t,
		`SELECT count(*) FROM go_refresh_tokens WHERE revoked = TRUE`); got != 1 {
		t.Errorf("revoked row count = %d, want 1; the repair resurrected or revoked something", got)
	}
	if got := refreshRowsFor(t, stranger); got != 1 {
		t.Errorf("the unrelated user's session was touched (holds %d rows, want 1)", got)
	}

	// Idempotent: the second run is a no-op, which is what makes it safe to
	// paste twice or re-run after an interrupted session.
	runSQLFile(t, orphanRepairSQL)
	if got := refreshRowsFor(t, canonical); got != 2 {
		t.Errorf("re-running the repair changed the outcome (canonical holds %d rows, want 2)", got)
	}
}

// runSQLFile executes an operator .sql artifact statement by statement.
//
// Comment lines are stripped first, and that is not cosmetic: the file's header
// carries example SELECTs whose semicolons would otherwise be read as statement
// boundaries. Splitting after the strip leaves exactly the executable text.
func runSQLFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed in-repo test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var body strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	for _, stmt := range strings.Split(body.String(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := testPool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("exec (%s): %v", strings.TrimSpace(stmt), err)
		}
	}
}

// TestConvergedTargetNeedsNoGoUsersRow covers James's exact shape: the id his
// binding converged onto is a Prisma-era cuid with NO go_users row at all.
//
// The migration is worthless if the session it hands over cannot then be used,
// so this asserts the two things the moved session depends on: that the access
// tokens minted for that subject survive the fail-closed existence check in
// auth.JWTAuthenticator, and that a refresh under it can still project a name
// and email.
func TestConvergedTargetNeedsNoGoUsersRow(t *testing.T) {
	setupSessionMigration(t)
	ctx := context.Background()
	seedCanonicalTeslaOwner(t)

	ks, err := identity.NewKeystoreFromPEM(genResurrectionPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	minter := identity.NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour)
	svc := identity.NewService(identity.ServiceConfig{
		Store:      identity.NewPgStore(testPool),
		Apple:      fakeAppleValidator{email: sessAppleMai},
		Minter:     minter,
		RefreshTTL: 24 * time.Hour,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	signIn, err := svc.SignInWithApple(ctx, identity.AppleSignInInput{
		IdentityToken: sessAppleSub,
		FullName:      "James Guan",
	})
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	linkTesla(t, signIn.User.ID)

	// The premise: the target genuinely has no Go-side user row.
	if got := countQuery(t, `SELECT count(*) FROM go_users WHERE id = $1`, sessCanonID); got != 0 {
		t.Fatalf("precondition: canonical user has %d go_users rows, want 0", got)
	}

	// Token issuance for such a subject works — go_refresh_tokens carries no FK
	// to either identity source (CG-DL-9), and the rotation proves it end to end.
	rotated, err := svc.Refresh(ctx, signIn.RefreshToken)
	if err != nil {
		t.Fatalf("refresh under the go_users-less target: %v", err)
	}
	if rotated.User.ID != sessCanonID {
		t.Fatalf("refresh resolved to %q, want %q", rotated.User.ID, sessCanonID)
	}
	// The projection comes from the binding that just moved, not from go_users.
	if rotated.User.Name != "James Guan" {
		t.Errorf("refresh projected name %q, want it read off the migrated Apple binding", rotated.User.Name)
	}

	// API auth accepts the subject: the existence probe is satisfied by the
	// Prisma "User" leg, which is the only one this shape has.
	validator := auth.NewJWTAuthenticator(
		"hs-secret", "myrobotaxi", "telemetry", testPool, auth.WithES256Resolver(ks))
	access, _, err := minter.MintAccessToken(sessCanonID)
	if err != nil {
		t.Fatalf("mint access token: %v", err)
	}
	uid, err := validator.ValidateToken(ctx, access)
	if err != nil {
		t.Fatalf("access token for the converged target was rejected: %v", err)
	}
	if uid != sessCanonID {
		t.Errorf("validated subject = %q, want %q", uid, sessCanonID)
	}
}
