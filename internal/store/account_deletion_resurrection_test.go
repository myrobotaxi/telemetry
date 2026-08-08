package store_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/identity"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-452 — deleting an account must not leave anything Apple's `sub` can be
// recognised by.
//
// THE REPORT. An external-beta tester deleted his account, reopened the app,
// tapped Sign in with Apple, and was signed straight back in as Owner.
//
// WHAT APPLE GUARANTEES, AND WHAT IT DOES NOT. Apple's `sub` is stable forever
// for one (Apple ID, team) pair. There is no server-side revocation available to
// us — MYR-433's audit established that neither repo holds the credentials to
// call Apple's token-revocation endpoint, and deleting our rows does not touch
// Apple's grant. So the same `sub` WILL come back on the next sign-in, and no
// amount of server work changes that. What the server owes is narrower and
// absolute: that the returning `sub` resolves to NOTHING, so the sign-in falls
// through to a fresh mint and the person lands on a virgin account.
//
// WHAT WENT WRONG. store.OwnerProvisioner converges two identities onto one
// canonical user when a Tesla link proves they are the same human, re-pointing
// go_identity_apple.user_id off the caller's go_users id. Nothing re-issues the
// caller's tokens, so the JWT subject arriving at DELETE /api/users/me is the
// pre-convergence id — an id that owns nothing. Keyed on that subject alone the
// teardown deleted almost nothing, wrote its audit row, and answered 204, while
// the binding and the entire account stood. The next sign-in found the binding
// and handed the account back.
//
// These tests run the REAL sign-in path (identity.Service over identity.PgStore)
// on both sides of a REAL deletion, per identity shape, and assert the returning
// `sub` lands somewhere new and empty.

// fakeAppleValidator resolves an opaque test token to fixed claims. The token
// string IS the Apple sub, which keeps each case's identity obvious at the call
// site.
type fakeAppleValidator struct {
	email string
}

func (f fakeAppleValidator) Validate(_ context.Context, idToken, _ string) (identity.AppleClaims, error) {
	return identity.AppleClaims{
		Sub:            idToken,
		Email:          f.email,
		EmailVerified:  true,
		IsPrivateEmail: true,
	}, nil
}

// newSignInService builds the production identity service over the test pool.
// Only the Apple token validator is faked — everything the bug lives in
// (linkage precedence, the binding table, the fresh mint) is the real code.
func newSignInService(t *testing.T, email string) *identity.Service {
	t.Helper()
	ks, err := identity.NewKeystoreFromPEM(genResurrectionPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	return identity.NewService(identity.ServiceConfig{
		Store:      identity.NewPgStore(testPool),
		Apple:      fakeAppleValidator{email: email},
		Minter:     identity.NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour),
		RefreshTTL: 24 * time.Hour,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func genResurrectionPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// runFullDeletion executes the store-side deletion the way the handler does:
// resolve the caller's subject to its identity closure FIRST, then run every
// user-scoped step over that closure, then the identity transaction.
func runFullDeletion(t *testing.T, callerID string) store.DeletionScope {
	t.Helper()
	ctx := context.Background()
	deleter := newAccountDeleter(t)

	scope, err := deleter.ResolveDeletionScope(ctx, callerID)
	if err != nil {
		t.Fatalf("resolve deletion scope: %v", err)
	}
	for _, id := range scope.IDs {
		if _, err := deleter.RevokeSharesReceived(ctx, id); err != nil {
			t.Fatalf("revoke shares received(%s): %v", id, err)
		}
		if _, err := deleter.ScrubSharesReceivedLabel(ctx, id); err != nil {
			t.Fatalf("scrub share labels(%s): %v", id, err)
		}
		if _, err := deleter.DeletePushDevices(ctx, id); err != nil {
			t.Fatalf("delete push devices(%s): %v", id, err)
		}
		if _, err := deleter.DeleteSavedPlaces(ctx, id); err != nil {
			t.Fatalf("delete saved places(%s): %v", id, err)
		}
		if _, err := deleter.RevokeRefreshTokens(ctx, id); err != nil {
			t.Fatalf("revoke refresh tokens(%s): %v", id, err)
		}
	}
	if _, err := deleter.DeleteIdentity(ctx, scope, store.AccountDeletionCounts{}); err != nil {
		t.Fatalf("delete identity: %v", err)
	}
	return scope
}

// TestPostDeletionSignInYieldsAVirginAccount is the MYR-452 regression, run once
// per identity shape the dual-source model can produce.
//
// Each case signs in for real, reaches some state, deletes the account through
// the real deleter using the subject THE CLIENT'S TOKEN ACTUALLY CARRIES, then
// signs in again with the same Apple sub — the sub Apple will always re-present
// — and asserts the person landed on a brand-new, empty account.
func TestPostDeletionSignInYieldsAVirginAccount(t *testing.T) {
	const (
		appleSub  = "001083.resurrection.test.2352"
		appleMail = "j46virgin@privaterelay.appleid.com"
		teslaSub  = "ef40b79d-tesla-owner-sub"
	)

	tests := []struct {
		name string
		// arrange reaches the pre-deletion state and returns the user id the
		// client's JWT carries — which is NOT always the id owning the rows.
		arrange func(t *testing.T, svc *identity.Service) string
		// wantConverged pins whether this shape exercises the convergence that
		// caused the bug, so a refactor that quietly stops converging is caught
		// rather than silently passing.
		wantConverged bool
	}{
		{
			// Apple-native: go_users + binding, no Prisma "User" row. Every
			// external-beta tester who never linked a car is this shape.
			name: "apple-native account",
			arrange: func(t *testing.T, svc *identity.Service) string {
				t.Helper()
				res, err := svc.SignInWithApple(context.Background(),
					identity.AppleSignInInput{IdentityToken: appleSub})
				if err != nil {
					t.Fatalf("first sign-in: %v", err)
				}
				return res.User.ID
			},
		},
		{
			// Legacy web: the binding is minted straight onto an existing
			// Prisma "User" matched by verified email, and no go_users row is
			// ever created. Subject and owner agree from the start.
			name: "legacy web account matched by verified email",
			arrange: func(t *testing.T, svc *identity.Service) string {
				t.Helper()
				ctx := context.Background()
				if _, err := testPool.Exec(ctx,
					`INSERT INTO "User" ("id", "email", "updatedAt") VALUES ($1, $2, NOW())`,
					"clegacyweb0000000000000001", appleMail); err != nil {
					t.Fatalf("seed legacy User: %v", err)
				}
				res, err := svc.SignInWithApple(ctx,
					identity.AppleSignInInput{IdentityToken: appleSub})
				if err != nil {
					t.Fatalf("first sign-in: %v", err)
				}
				if res.User.ID != "clegacyweb0000000000000001" {
					t.Fatalf("email linkage did not adopt the legacy user: got %q", res.User.ID)
				}
				return res.User.ID
			},
		},
		{
			// THE BUG. Sign in fresh (go_users id S), then link a Tesla account
			// that already belongs to Prisma user C. The provisioner adopts C as
			// canonical and re-points the Apple binding onto it — but the
			// client's token still says S, and S is what arrives at the delete
			// endpoint.
			name: "converged owner whose token still carries the pre-convergence id",
			arrange: func(t *testing.T, svc *identity.Service) string {
				t.Helper()
				ctx := context.Background()
				const canonical = "cconverged00000000000001"

				if _, err := testPool.Exec(ctx,
					`INSERT INTO "User" ("id", "email", "updatedAt") VALUES ($1, $2, NOW())`,
					canonical, "owner-of-the-car@example.com"); err != nil {
					t.Fatalf("seed canonical User: %v", err)
				}
				// The Tesla account already belongs to the canonical user —
				// this is what makes the provisioner take its (a) branch.
				if _, err := testPool.Exec(ctx,
					`INSERT INTO "Account" ("id", "userId", "type", "provider", "providerAccountId")
					 VALUES ($1, $2, 'oauth', 'tesla', $3)`,
					"cacct00000000000000000001", canonical, teslaSub); err != nil {
					t.Fatalf("seed Account: %v", err)
				}

				res, err := svc.SignInWithApple(ctx,
					identity.AppleSignInInput{IdentityToken: appleSub})
				if err != nil {
					t.Fatalf("first sign-in: %v", err)
				}
				callerID := res.User.ID
				if callerID == canonical {
					t.Fatalf("precondition: fresh sign-in should mint its own id, got the canonical one")
				}

				prov := newTestProvisioner(t)
				//nolint:gosec // G101: fixed test fixtures, not credentials.
				out, err := prov.ProvisionTeslaOwner(ctx, store.ProvisionInput{
					UserID:            callerID,
					ProviderAccountID: teslaSub,
					Email:             appleMail,
					AccessToken:       "tesla-access",
					RefreshToken:      "tesla-refresh",
					ExpiresAt:         time.Now().Add(time.Hour).Unix(),
				})
				if err != nil {
					t.Fatalf("provision tesla owner: %v", err)
				}
				if out.CanonicalUserID != canonical {
					t.Fatalf("precondition: convergence target = %q, want %q", out.CanonicalUserID, canonical)
				}
				// The binding now names the canonical user while the client's
				// token still names callerID. That gap IS the bug.
				if got := countQuery(t,
					`SELECT count(*) FROM go_identity_apple WHERE user_id = $1`, canonical); got != 1 {
					t.Fatalf("precondition: binding did not converge onto the canonical user")
				}
				return callerID
			},
			wantConverged: true,
		},
		{
			// THE SHAPE A go_users COLUMN WOULD HAVE MISSED. Same convergence,
			// but the caller's subject is a Prisma "User" id with NO go_users
			// row — which is what identity.linkage mints on a verified-email
			// match. Recording the convergence against go_users would silently
			// write nothing here and leave the account resurrectable, so this
			// case is the reason the trail lives in its own table.
			name: "converged caller whose pre-convergence id is a Prisma user, not a go_users row",
			arrange: func(t *testing.T, svc *identity.Service) string {
				t.Helper()
				ctx := context.Background()
				const (
					legacyCaller = "clegacycaller00000000001"
					canonical    = "cconverged00000000000002"
				)
				// The caller: a legacy web user the email match will adopt.
				if _, err := testPool.Exec(ctx,
					`INSERT INTO "User" ("id", "email", "updatedAt") VALUES ($1, $2, NOW())`,
					legacyCaller, appleMail); err != nil {
					t.Fatalf("seed legacy caller: %v", err)
				}
				// The canonical owner of the Tesla account they are about to link.
				if _, err := testPool.Exec(ctx,
					`INSERT INTO "User" ("id", "email", "updatedAt") VALUES ($1, $2, NOW())`,
					canonical, "other-owner@example.com"); err != nil {
					t.Fatalf("seed canonical User: %v", err)
				}
				if _, err := testPool.Exec(ctx,
					`INSERT INTO "Account" ("id", "userId", "type", "provider", "providerAccountId")
					 VALUES ($1, $2, 'oauth', 'tesla', $3)`,
					"cacct00000000000000000002", canonical, teslaSub); err != nil {
					t.Fatalf("seed Account: %v", err)
				}

				res, err := svc.SignInWithApple(ctx,
					identity.AppleSignInInput{IdentityToken: appleSub})
				if err != nil {
					t.Fatalf("first sign-in: %v", err)
				}
				if res.User.ID != legacyCaller {
					t.Fatalf("precondition: email match should adopt the legacy user, got %q", res.User.ID)
				}
				if n := countQuery(t, `SELECT count(*) FROM go_users WHERE id = $1`, legacyCaller); n != 0 {
					t.Fatalf("precondition: the legacy caller must have no go_users row, found %d", n)
				}

				prov := newTestProvisioner(t)
				//nolint:gosec // G101: fixed test fixtures, not credentials.
				out, err := prov.ProvisionTeslaOwner(ctx, store.ProvisionInput{
					UserID:            legacyCaller,
					ProviderAccountID: teslaSub,
					Email:             appleMail,
					AccessToken:       "tesla-access",
					RefreshToken:      "tesla-refresh",
					ExpiresAt:         time.Now().Add(time.Hour).Unix(),
				})
				if err != nil {
					t.Fatalf("provision tesla owner: %v", err)
				}
				if out.CanonicalUserID != canonical {
					t.Fatalf("precondition: convergence target = %q, want %q", out.CanonicalUserID, canonical)
				}
				return legacyCaller
			},
			wantConverged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAccountDeletion(t)
			if _, err := testPool.Exec(context.Background(), ownerSchemaSQL); err != nil {
				t.Fatalf("owner schema: %v", err)
			}
			ctx := context.Background()
			svc := newSignInService(t, appleMail)

			callerID := tt.arrange(t, svc)

			scope := runFullDeletion(t, callerID)
			if got := scope.Converged(); got != tt.wantConverged {
				t.Fatalf("scope.Converged() = %v, want %v (scope=%v)", got, tt.wantConverged, scope.IDs)
			}

			// (1) Nothing Apple's sub can be recognised by may remain. This is
			// the single assertion the whole issue reduces to: the sub comes
			// back regardless, so it must resolve to nothing.
			if got := countQuery(t,
				`SELECT count(*) FROM go_identity_apple WHERE apple_sub = $1`, appleSub); got != 0 {
				t.Errorf("apple binding survived deletion (%d rows) — the returning sub will be re-recognised", got)
			}

			// (2) No identity row survives anywhere in the closure, and no
			// abandoned convergence alias is left holding the person's email.
			for _, id := range scope.IDs {
				if got := countQuery(t, `SELECT count(*) FROM go_users WHERE id = $1`, id); got != 0 {
					t.Errorf("go_users row for %s survived deletion", id)
				}
				if got := countQuery(t, `SELECT count(*) FROM "User" WHERE "id" = $1`, id); got != 0 {
					t.Errorf("Prisma User row for %s survived deletion", id)
				}
			}
			if got := countQuery(t, `SELECT count(*) FROM go_users WHERE email = $1`, appleMail); got != 0 {
				t.Errorf("a go_users row still holds the deleted person's email (%d rows)", got)
			}

			// (3) The Tesla grant goes with the account. For the converged
			// shape this only happens if the scope reached the CANONICAL id —
			// the id the caller's token never named — since the grant is filed
			// there and is deleted by userId, not by providerAccountId.
			if got := countQuery(t,
				`SELECT count(*) FROM "Account" WHERE "providerAccountId" = $1`, teslaSub); got != 0 {
				t.Errorf("Tesla Account row survived deletion (%d rows) — sealed OAuth tokens outlived the account", got)
			}

			// (4) The real sign-in path, with the sub Apple will re-present.
			after, err := svc.SignInWithApple(ctx, identity.AppleSignInInput{IdentityToken: appleSub})
			if err != nil {
				t.Fatalf("post-deletion sign-in: %v", err)
			}
			for _, old := range scope.IDs {
				if after.User.ID == old {
					t.Fatalf("post-deletion sign-in resurrected %s — the account was handed back", old)
				}
			}

			// (5) …and what it landed on is genuinely empty.
			if got := countQuery(t,
				`SELECT count(*) FROM "Account" WHERE "userId" = $1`, after.User.ID); got != 0 {
				t.Errorf("fresh account already owns %d Tesla Account rows", got)
			}
			if got := countQuery(t,
				`SELECT count(*) FROM "Settings" WHERE "userId" = $1`, after.User.ID); got != 0 {
				t.Errorf("fresh account already owns %d Settings rows", got)
			}
			if got := countQuery(t,
				`SELECT count(*) FROM go_vehicle_shares WHERE owner_user_id = $1 OR accepted_by_user_id = $1`,
				after.User.ID); got != 0 {
				t.Errorf("fresh account already carries %d share grants", got)
			}
			if got := countQuery(t,
				`SELECT count(*) FROM go_users WHERE id = $1`, after.User.ID); got != 1 {
				t.Errorf("post-deletion sign-in did not mint a go_users row (got %d)", got)
			}

			// (6) The deletion is still auditable, and filed against the id that
			// actually owned the account rather than the token's subject.
			if got := countQuery(t,
				`SELECT count(*) FROM "AuditLog" WHERE action = 'account_deleted' AND "targetId" = $1`,
				scope.CanonicalID); got != 1 {
				t.Errorf("account_deleted audit rows for canonical id = %d, want 1", got)
			}
		})
	}
}

// TestResolveDeletionScope_FollowsConvergenceBothWays pins the resolver itself:
// forward (the caller was re-pointed away) and reverse (the caller is the
// canonical id and the abandoned alias is still sitting there). The reverse
// direction matters because a person who signs in again after converging
// presents the RIGHT subject — and their alias row, which still holds their
// email, would otherwise never be collected.
func TestResolveDeletionScope_FollowsConvergenceBothWays(t *testing.T) {
	setupAccountDeletion(t)
	ctx := context.Background()
	deleter := newAccountDeleter(t)

	const (
		alias     = "calias0000000000000000001"
		canonical = "ccanon0000000000000000001"
	)
	seedConvergence(t, alias, canonical)
	// The binding is what makes an id canonical, so seed it where a real
	// convergence would have left it.
	seedAppleIdentity(t, "sub-converged", canonical, nil, nil)

	tests := []struct {
		name          string
		callerID      string
		wantCanonical string
	}{
		{"forward: caller is the abandoned alias", alias, canonical},
		{"reverse: caller is the canonical id", canonical, canonical},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := deleter.ResolveDeletionScope(ctx, tt.callerID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if scope.CanonicalID != tt.wantCanonical {
				t.Errorf("CanonicalID = %q, want %q", scope.CanonicalID, tt.wantCanonical)
			}
			if scope.CallerID != tt.callerID {
				t.Errorf("CallerID = %q, want %q", scope.CallerID, tt.callerID)
			}
			if len(scope.IDs) != 2 {
				t.Fatalf("IDs = %v, want both the alias and the canonical id", scope.IDs)
			}
			if scope.IDs[0] != tt.wantCanonical {
				t.Errorf("IDs[0] = %q, want the canonical id first", scope.IDs[0])
			}
			if !scope.Converged() {
				t.Error("Converged() = false, want true")
			}
		})
	}

	t.Run("un-converged caller stands for itself", func(t *testing.T) {
		scope, err := deleter.ResolveDeletionScope(ctx, "csolo00000000000000000001")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if scope.Converged() {
			t.Errorf("Converged() = true for a plain account (scope=%v)", scope.IDs)
		}
		if len(scope.IDs) != 1 || scope.IDs[0] != "csolo00000000000000000001" {
			t.Errorf("IDs = %v, want exactly the caller", scope.IDs)
		}
	})
}

// TestResolveDeletionScope_SurvivesChainsAndCycles pins the two graph shapes the
// writer cannot rule out.
//
// A CHAIN forms when one person converges twice. A 2-CYCLE forms because
// "Account".userId is never rewritten: after A converges onto B, re-linking that
// same Tesla while authenticated as B resolves canonical A and converges back.
// A one-hop resolver silently drops members of both, and a resolver without a
// visited set spins forever on the cycle.
func TestResolveDeletionScope_SurvivesChainsAndCycles(t *testing.T) {
	tests := []struct {
		name    string
		edges   [][2]string
		binding string // id the Apple binding sits under
		from    string // the caller's subject
		wantIDs []string
	}{
		{
			name:    "chain A->B->C resolves all three from any end",
			edges:   [][2]string{{"cchain0000000000000000a", "cchain0000000000000000b"}, {"cchain0000000000000000b", "cchain0000000000000000c"}},
			binding: "cchain0000000000000000c",
			from:    "cchain0000000000000000a",
			wantIDs: []string{"cchain0000000000000000a", "cchain0000000000000000b", "cchain0000000000000000c"},
		},
		{
			name:    "chain resolves from the middle too",
			edges:   [][2]string{{"cchain0000000000000000a", "cchain0000000000000000b"}, {"cchain0000000000000000b", "cchain0000000000000000c"}},
			binding: "cchain0000000000000000c",
			from:    "cchain0000000000000000b",
			wantIDs: []string{"cchain0000000000000000a", "cchain0000000000000000b", "cchain0000000000000000c"},
		},
		{
			name:    "2-cycle A<->B terminates and yields both",
			edges:   [][2]string{{"ccycle0000000000000000a", "ccycle0000000000000000b"}, {"ccycle0000000000000000b", "ccycle0000000000000000a"}},
			binding: "ccycle0000000000000000a",
			from:    "ccycle0000000000000000b",
			wantIDs: []string{"ccycle0000000000000000a", "ccycle0000000000000000b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupAccountDeletion(t)
			ctx := context.Background()
			for _, e := range tt.edges {
				seedConvergence(t, e[0], e[1])
			}
			seedAppleIdentity(t, "sub-"+tt.from, tt.binding, nil, nil)

			scope, err := newAccountDeleter(t).ResolveDeletionScope(ctx, tt.from)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if scope.CanonicalID != tt.binding {
				t.Errorf("CanonicalID = %q, want the id holding the binding (%q)", scope.CanonicalID, tt.binding)
			}
			got := append([]string(nil), scope.IDs...)
			sort.Strings(got)
			if !slices.Equal(got, tt.wantIDs) {
				t.Errorf("closure = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

// TestResolveDeletionScope_RefusesAnImplausiblyLargeClosure is the blast-radius
// guard. A convergence edge grants DELETE authority over another id, so a
// corrupt graph must fail the deletion rather than fan it out across accounts:
// a 500 is recoverable, deleting a stranger's account is not.
func TestResolveDeletionScope_RefusesAnImplausiblyLargeClosure(t *testing.T) {
	setupAccountDeletion(t)
	ctx := context.Background()

	// A star: many abandoned ids all pointing at one hub, well past the cap.
	const hub = "chub000000000000000000000"
	for i := range 12 {
		seedConvergence(t, fmt.Sprintf("cspoke%019d", i), hub)
	}

	if _, err := newAccountDeleter(t).ResolveDeletionScope(ctx, hub); err == nil {
		t.Fatal("resolved a 13-id closure; want a refusal")
	} else if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error = %v, want it to name the scope cap", err)
	}
}

// seedConvergence records one convergence edge.
func seedConvergence(t *testing.T, from, to string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_identity_convergence (from_user_id, to_user_id) VALUES ($1, $2)
		 ON CONFLICT (from_user_id) DO UPDATE SET to_user_id = EXCLUDED.to_user_id`,
		from, to); err != nil {
		t.Fatalf("seed convergence %s->%s: %v", from, to, err)
	}
}
