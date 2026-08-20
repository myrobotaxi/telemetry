package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-594 against a real Postgres. The unit tests in internal/fleetsuspend pin
// the POLICY; what can only be proven here is that the SQL expressing it parses,
// that the gates actually exclude, and — the one property no text assertion can
// reach — that a contended row really does abandon instead of queueing.

const keepaliveOwner = "user_keepalive_owner"

// seedSuspendedOwner builds the whole shape one candidate needs: a vehicle, a
// suspension stamp, an OAuth row with a stored refresh token, and an expiry
// `rotatedAgo` in the past.
func seedSuspendedOwner(t *testing.T, userID, vehID, vin string, rotatedAgo time.Duration) {
	t.Helper()
	ctx := context.Background()
	seedVehicleForOwner(t, testPool, vehID, vin, userID)

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, suspended_at)
		 VALUES ($1, NOW() - INTERVAL '10 days')`, vehID); err != nil {
		t.Fatalf("seed suspension: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Account" ("id", "userId", "provider", "providerAccountId",
		     "refresh_token_enc", "access_token_enc", "expires_at")
		 VALUES ($1, $2, 'tesla', $1, 'ciphertext-refresh', 'ciphertext-access', $3)`,
		"acct_"+userID, userID, time.Now().Add(-rotatedAgo).Unix()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func cleanKeepaliveFixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM go_tesla_token_keepalive`,
		`DELETE FROM go_vehicle_telemetry_suspensions`,
		`DELETE FROM go_user_activity`,
		`DELETE FROM "Account"`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("clean (%s): %v", stmt, err)
		}
	}
	cleanTables(t, testPool)
}

func keepaliveQuery() store.StaleTokenOwnerQuery {
	now := time.Now()
	return store.StaleTokenOwnerQuery{
		StaleBefore:   now.Add(-45 * 24 * time.Hour),
		QuietBefore:   now.Add(-5 * 24 * time.Hour),
		AttemptBefore: now.Add(-24 * time.Hour),
		FailureBefore: now.Add(-7 * 24 * time.Hour),
		Limit:         20,
	}
}

// TestListStaleTokenOwners_Gates walks each refusal by seeding the ONE fact that
// should disqualify an otherwise-perfect candidate.
func TestListStaleTokenOwners_Gates(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ensureAccountSchema(t)
	ctx := context.Background()
	repo := store.NewTeslaTokenKeepaliveRepo(testPool)

	tests := []struct {
		name        string
		disqualify  func(t *testing.T)
		wantOwner   bool
		reason      string
		rotatedAgo  time.Duration
		skipDefault bool
	}{
		{
			name: "a dormant owner with a 60-day-old grant is due", rotatedAgo: 60 * 24 * time.Hour,
			wantOwner: true,
			reason:    "this is the population the arm exists for",
		},
		{
			name: "a grant rotated last week is not", rotatedAgo: 7 * 24 * time.Hour,
			wantOwner: false,
			reason:    "the on-demand path already keeps it alive; rotating again buys nothing",
		},
		{
			name: "an owner who authenticated yesterday is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if _, err := testPool.Exec(ctx,
					`INSERT INTO go_user_activity (user_id, last_seen_at)
					 VALUES ($1, NOW() - INTERVAL '1 day')`, keepaliveOwner); err != nil {
					t.Fatalf("seed activity: %v", err)
				}
			},
			wantOwner: false,
			reason:    "somebody who has just woken up may be reaching for the reconnect button",
		},
		{
			name: "an owner with no activity row at all IS", rotatedAgo: 60 * 24 * time.Hour,
			wantOwner: true,
			reason: "unlike the suspend sweeper's inner join, a missing row here admits: the " +
				"action undoes nothing, and these are the accounts most likely already stranded",
		},
		{
			name: "an owner tried an hour ago is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if _, err := testPool.Exec(ctx,
					`INSERT INTO go_tesla_token_keepalive (user_id, last_attempt_at)
					 VALUES ($1, NOW() - INTERVAL '1 hour')`, keepaliveOwner); err != nil {
					t.Fatalf("seed attempt: %v", err)
				}
			},
			wantOwner: false,
			reason:    "the rotation-loop brake, which is what stops a broken store write spinning",
		},
		{
			name: "an owner refused yesterday is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if err := repo.RecordKeepaliveFailure(ctx, keepaliveOwner,
					time.Now().Add(-25*time.Hour), time.Now().Add(-60*24*time.Hour).Unix()); err != nil {
					t.Fatalf("record failure: %v", err)
				}
			},
			wantOwner: false,
			reason:    "a refused grant is lapsed or revoked and no retry cures either",
		},
		{
			name: "…unless the grant has moved since", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				// A failure pinned to a DIFFERENT token generation: something
				// rotated the grant after we gave up on it.
				if err := repo.RecordKeepaliveFailure(ctx, keepaliveOwner,
					time.Now().Add(-25*time.Hour), 1); err != nil {
					t.Fatalf("record failure: %v", err)
				}
			},
			wantOwner: true,
			reason:    "a re-link makes the reason for the cooldown obsolete",
		},
		{
			name: "a car that is not suspended is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if _, err := testPool.Exec(ctx,
					`UPDATE go_vehicle_telemetry_suspensions SET suspended_at = NULL`); err != nil {
					t.Fatalf("open the episode: %v", err)
				}
			},
			wantOwner: false,
			reason:    "a streaming car's owner refreshes on demand and needs nothing from us",
		},
		{
			name: "a grant with no stored refresh token is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if _, err := testPool.Exec(ctx,
					`UPDATE "Account" SET "refresh_token_enc" = NULL`); err != nil {
					t.Fatalf("clear refresh: %v", err)
				}
			},
			wantOwner: false,
			reason:    "there is nothing to spend",
		},
		{
			name: "a grant with an unknown expiry is not", rotatedAgo: 60 * 24 * time.Hour,
			disqualify: func(t *testing.T) {
				t.Helper()
				if _, err := testPool.Exec(ctx,
					`UPDATE "Account" SET "expires_at" = NULL`); err != nil {
					t.Fatalf("clear expiry: %v", err)
				}
			},
			wantOwner: false,
			reason:    "NULL is unknown, not infinitely stale, and this arm never acts on unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanKeepaliveFixtures(t)
			seedSuspendedOwner(t, keepaliveOwner, "veh_keepalive", "5YJ3E1EA7KF00K001", tc.rotatedAgo)
			if tc.disqualify != nil {
				tc.disqualify(t)
			}

			got, err := repo.ListStaleTokenOwners(ctx, keepaliveQuery())
			if err != nil {
				t.Fatalf("ListStaleTokenOwners: %v", err)
			}
			if (len(got) == 1) != tc.wantOwner {
				t.Fatalf("candidates = %+v, want present=%v (%s)", got, tc.wantOwner, tc.reason)
			}
			if tc.wantOwner && got[0].OwnerID != keepaliveOwner {
				t.Errorf("owner = %q, want %q", got[0].OwnerID, keepaliveOwner)
			}
		})
	}
}

// TestListStaleTokenOwners_OneRowPerOwner. Three suspended cars, one grant.
func TestListStaleTokenOwners_OneRowPerOwner(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ensureAccountSchema(t)
	cleanKeepaliveFixtures(t)
	ctx := context.Background()

	seedSuspendedOwner(t, keepaliveOwner, "veh_ka_1", "5YJ3E1EA7KF00K011", 60*24*time.Hour)
	for _, v := range []struct{ id, vin string }{
		{"veh_ka_2", "5YJ3E1EA7KF00K012"},
		{"veh_ka_3", "5YJ3E1EA7KF00K013"},
	} {
		seedVehicleForOwner(t, testPool, v.id, v.vin, keepaliveOwner)
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, suspended_at)
			 VALUES ($1, NOW())`, v.id); err != nil {
			t.Fatalf("seed suspension: %v", err)
		}
	}

	got, err := store.NewTeslaTokenKeepaliveRepo(testPool).ListStaleTokenOwners(ctx, keepaliveQuery())
	if err != nil {
		t.Fatalf("ListStaleTokenOwners: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d for one owner with three suspended cars, want 1 — rotating a "+
			"single-use grant three times in a pass would spend the pair the first one minted", len(got))
	}
}

// TestRecordKeepalive_SuccessClearsTheCooldown. A refused account that later
// works must start from a clean sheet, or a future refusal would inherit a
// stale cooldown and the arm would go quiet for the wrong reason.
func TestRecordKeepalive_SuccessClearsTheCooldown(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ensureAccountSchema(t)
	cleanKeepaliveFixtures(t)
	ctx := context.Background()
	repo := store.NewTeslaTokenKeepaliveRepo(testPool)

	if err := repo.RecordKeepaliveFailure(ctx, keepaliveOwner, time.Now().Add(-time.Hour), 42); err != nil {
		t.Fatalf("RecordKeepaliveFailure: %v", err)
	}
	if err := repo.RecordKeepaliveSuccess(ctx, keepaliveOwner, time.Now()); err != nil {
		t.Fatalf("RecordKeepaliveSuccess: %v", err)
	}

	var failure, success *time.Time
	var pinned *int64
	if err := testPool.QueryRow(ctx,
		`SELECT last_failure_at, last_success_at, failed_token_expiry
		 FROM go_tesla_token_keepalive WHERE user_id = $1`, keepaliveOwner,
	).Scan(&failure, &success, &pinned); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if failure != nil || pinned != nil {
		t.Errorf("the cooldown survived a success (failure=%v, pinned=%v)", failure, pinned)
	}
	if success == nil {
		t.Error("the success was not recorded")
	}
}

// TestRotateTeslaTokenLocked is the concurrency property, and it is the reason
// this file needs a real database: no fake can demonstrate that a contended row
// is abandoned rather than queued.
func TestRotateTeslaTokenLocked(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	ensureAccountSchema(t)
	cleanKeepaliveFixtures(t)
	ctx := context.Background()

	enc := newTestEncryptor(t)
	repo := store.NewAccountRepo(testPool, enc)
	seedRotatable := func(t *testing.T) {
		t.Helper()
		cleanAccount(t, testPool)
		refreshEnc, err := enc.EncryptString("refresh-original")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		accessEnc, err := enc.EncryptString("access-original")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO "Account" ("id", "userId", "provider", "providerAccountId",
			     "access_token_enc", "refresh_token_enc", "expires_at")
			 VALUES ('acct_rot', $1, 'tesla', 'acct_rot', $2, $3, 100)`,
			keepaliveOwner, accessEnc, refreshEnc); err != nil {
			t.Fatalf("seed account: %v", err)
		}
	}

	t.Run("a rotation stores the new pair", func(t *testing.T) {
		seedRotatable(t)
		var spent string
		err := repo.RotateTeslaTokenLocked(ctx, keepaliveOwner,
			func(_ context.Context, refreshToken string) (store.TeslaTokenPair, error) {
				spent = refreshToken
				return store.TeslaTokenPair{
					AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: 999,
				}, nil
			})
		if err != nil {
			t.Fatalf("RotateTeslaTokenLocked: %v", err)
		}
		if spent != "refresh-original" {
			t.Errorf("the callback was handed %q, want the DECRYPTED stored token", spent)
		}
		tok, err := repo.GetTeslaToken(ctx, keepaliveOwner)
		if err != nil {
			t.Fatalf("GetTeslaToken: %v", err)
		}
		if tok.RefreshToken != "refresh-new" || tok.AccessToken != "access-new" {
			t.Errorf("stored pair = %+v, want the rotated one", tok)
		}
	})

	t.Run("a refused rotation writes nothing", func(t *testing.T) {
		seedRotatable(t)
		wantErr := errors.New("Tesla returned 401: invalid_grant")
		err := repo.RotateTeslaTokenLocked(ctx, keepaliveOwner,
			func(context.Context, string) (store.TeslaTokenPair, error) {
				return store.TeslaTokenPair{}, wantErr
			})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want the callback's own", err)
		}
		tok, err := repo.GetTeslaToken(ctx, keepaliveOwner)
		if err != nil {
			t.Fatalf("GetTeslaToken: %v", err)
		}
		if tok.RefreshToken != "refresh-original" {
			t.Errorf("stored refresh token = %q after a refusal; it must be left exactly as it "+
				"was — it is the only evidence of which account needs re-pairing", tok.RefreshToken)
		}
	})

	t.Run("a contended row is abandoned, not queued", func(t *testing.T) {
		seedRotatable(t)

		// Hold the row the way any other writer would.
		holder, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin holder: %v", err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx,
			`SELECT 1 FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla' FOR UPDATE`,
			keepaliveOwner); err != nil {
			t.Fatalf("holder lock: %v", err)
		}

		called := false
		start := time.Now()
		err = repo.RotateTeslaTokenLocked(ctx, keepaliveOwner,
			func(context.Context, string) (store.TeslaTokenPair, error) {
				called = true
				return store.TeslaTokenPair{}, nil
			})

		if !errors.Is(err, store.ErrTeslaTokenRowBusy) {
			t.Fatalf("error = %v, want ErrTeslaTokenRowBusy", err)
		}
		if called {
			t.Error("the refresh token was SPENT on a contended row; the whole point of NOWAIT " +
				"is that a token is never put in flight while another writer holds the row")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("waited %v for the lock; NOWAIT must fail immediately rather than queue "+
				"behind a user's own reconnect", elapsed)
		}
	})
}
