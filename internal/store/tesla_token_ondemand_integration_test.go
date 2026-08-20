package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-595 against a real Postgres. The keepalive's file next door proves a
// contended row is ABANDONED; this one proves the opposite half — that the
// on-demand path QUEUES for the row, and that what it does after it finally gets
// in is look at what the winner stored rather than spend the token it is holding.
// No fake can demonstrate either: both are properties of a real row lock.

const ondemandOwner = "user_ondemand_owner"

// failingEncryptor decrypts normally and refuses to encrypt, which is how a
// persist failure is staged inside a transaction that is otherwise healthy.
type failingEncryptor struct {
	cryptox.Encryptor
	err error
}

func (e failingEncryptor) EncryptString(string) (string, error) { return "", e.err }

// seedOndemandAccount stores a pair whose access token expired an hour ago —
// the state that sends the on-demand path to the lock in the first place.
func seedOndemandAccount(t *testing.T, enc cryptox.Encryptor) {
	t.Helper()
	ctx := context.Background()
	cleanAccount(t, testPool)

	accessEnc, err := enc.EncryptString("access-original")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	refreshEnc, err := enc.EncryptString("refresh-original")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO "Account" ("id", "userId", "provider", "providerAccountId",
		     "access_token_enc", "refresh_token_enc", "expires_at")
		 VALUES ('acct_ondemand', $1, 'tesla', 'acct_ondemand', $2, $3, $4)`,
		ondemandOwner, accessEnc, refreshEnc, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

// holdRow takes the row lock the way any other writer would and returns the
// transaction holding it.
func holdRow(ctx context.Context, t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM "Account" WHERE "userId" = $1 AND "provider" = 'tesla' FOR UPDATE`,
		ondemandOwner); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	return tx
}

func TestRotateTeslaTokenLockedWaiting(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureAccountSchema(t)
	ctx := context.Background()
	enc := newTestEncryptor(t)
	repo := store.NewAccountRepo(testPool, enc)

	t.Run("a rotation stores the new pair", func(t *testing.T) {
		seedOndemandAccount(t, enc)

		got, err := repo.RotateTeslaTokenLockedWaiting(ctx, ondemandOwner, time.Second,
			func(_ context.Context, stored store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
				if stored.AccessToken != "access-original" || stored.RefreshToken != "refresh-original" {
					t.Errorf("callback was handed %+v, want the DECRYPTED stored pair", stored)
				}
				return store.TeslaTokenPair{
					AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: 999,
				}, true, nil
			})
		if err != nil {
			t.Fatalf("RotateTeslaTokenLockedWaiting: %v", err)
		}
		if got.AccessToken != "access-new" || got.RefreshToken != "refresh-new" {
			t.Errorf("returned pair = %+v, want the rotated one", got)
		}
		tok, err := repo.GetTeslaToken(ctx, ondemandOwner)
		if err != nil {
			t.Fatalf("GetTeslaToken: %v", err)
		}
		if tok.RefreshToken != "refresh-new" {
			t.Errorf("stored pair = %+v, want the rotated one", tok)
		}
	})

	// THE PROPERTY THE WHOLE ISSUE IS ABOUT. Two refreshes for one account: the
	// first takes the row and commits a new pair; the second must queue for the
	// lock, see that pair, and hand it back WITHOUT spending the single-use
	// refresh token it is now looking at.
	t.Run("the second arrival adopts the winner's pair and calls Tesla zero times", func(t *testing.T) {
		seedOndemandAccount(t, enc)

		winnerAccess, err := enc.EncryptString("access-winner")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		winnerRefresh, err := enc.EncryptString("refresh-winner")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}

		// The winner: holds the row, writes its rotated pair, commits shortly.
		holder := holdRow(ctx, t)
		if _, err := holder.Exec(ctx,
			`UPDATE "Account" SET "access_token_enc" = $1, "refresh_token_enc" = $2, "expires_at" = $3
			 WHERE "userId" = $4 AND "provider" = 'tesla'`,
			winnerAccess, winnerRefresh, time.Now().Add(time.Hour).Unix(), ondemandOwner); err != nil {
			t.Fatalf("winner write: %v", err)
		}
		committed := make(chan error, 1)
		go func() {
			time.Sleep(300 * time.Millisecond)
			committed <- holder.Commit(ctx)
		}()

		var entered, teslaCalls int
		start := time.Now()
		got, err := repo.RotateTeslaTokenLockedWaiting(ctx, ondemandOwner, 5*time.Second,
			func(_ context.Context, stored store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
				entered++
				// The double check, exactly as internal/telemetry performs it.
				if stored.ExpiresAt > time.Now().Unix() {
					return store.TeslaTokenPair{}, false, nil
				}
				teslaCalls++
				return store.TeslaTokenPair{
					AccessToken: "access-loser", RefreshToken: "refresh-loser", ExpiresAt: 999,
				}, true, nil
			})
		elapsed := time.Since(start)
		if cErr := <-committed; cErr != nil {
			t.Fatalf("winner commit: %v", cErr)
		}
		if err != nil {
			t.Fatalf("RotateTeslaTokenLockedWaiting: %v", err)
		}

		if entered != 1 {
			t.Errorf("entered the lock %d time(s), want 1", entered)
		}
		if teslaCalls != 0 {
			t.Errorf("spent %d refresh token(s); the pair in front of us was the winner's brand-new "+
				"single-use one and spending it recreates the race we just serialized away", teslaCalls)
		}
		if got.AccessToken != "access-winner" || got.RefreshToken != "refresh-winner" {
			t.Errorf("returned pair = %+v, want the winner's", got)
		}
		if elapsed < 250*time.Millisecond {
			t.Errorf("returned in %v — it cannot have waited for the row lock, so it read a pair "+
				"the winner had not committed yet", elapsed)
		}

		tok, err := repo.GetTeslaToken(ctx, ondemandOwner)
		if err != nil {
			t.Fatalf("GetTeslaToken: %v", err)
		}
		if tok.RefreshToken != "refresh-winner" {
			t.Errorf("stored refresh token = %q; the declining caller must write NOTHING", tok.RefreshToken)
		}
	})

	// MYR-594's sibling finding, closed. The old on-demand path logged a failed
	// persist and returned the rotated token anyway — one that works once while
	// the stored pair is already dead.
	t.Run("a rotation that cannot be persisted returns an error, not a token", func(t *testing.T) {
		seedOndemandAccount(t, enc)
		wantErr := errors.New("encrypt: key unavailable")
		broken := store.NewAccountRepo(testPool, failingEncryptor{Encryptor: enc, err: wantErr})

		got, err := broken.RotateTeslaTokenLockedWaiting(ctx, ondemandOwner, time.Second,
			func(context.Context, store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
				return store.TeslaTokenPair{
					AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: 999,
				}, true, nil
			})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want the persist failure", err)
		}
		if got.AccessToken != "" {
			t.Errorf("returned %q alongside the error; a rotation nobody can store must not be "+
				"handed out as if it were durable", got.AccessToken)
		}
		tok, err := repo.GetTeslaToken(ctx, ondemandOwner)
		if err != nil {
			t.Fatalf("GetTeslaToken: %v", err)
		}
		if tok.RefreshToken != "refresh-original" || tok.AccessToken != "access-original" {
			t.Errorf("stored pair = %+v after a failed persist; the transaction must abort with the "+
				"row exactly as it was", tok)
		}
	})

	// The bound. A transaction that never lets go must cost a user a known pause
	// and the ordinary error, never an open-ended wait.
	t.Run("a holder that never commits exhausts the budget", func(t *testing.T) {
		seedOndemandAccount(t, enc)
		holder := holdRow(ctx, t)
		defer func() { _ = holder.Rollback(ctx) }()

		called := false
		start := time.Now()
		_, err := repo.RotateTeslaTokenLockedWaiting(ctx, ondemandOwner, 300*time.Millisecond,
			func(context.Context, store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
				called = true
				return store.TeslaTokenPair{}, true, nil
			})
		elapsed := time.Since(start)

		if !errors.Is(err, store.ErrTeslaTokenRowBusy) {
			t.Fatalf("error = %v, want ErrTeslaTokenRowBusy", err)
		}
		if called {
			t.Error("the refresh token was spent on a row we never got the lock for")
		}
		if elapsed < 250*time.Millisecond {
			t.Errorf("gave up after %v; the on-demand path is supposed to WAIT for a lock that is "+
				"almost always about to be released", elapsed)
		}
		if elapsed > 5*time.Second {
			t.Errorf("waited %v on a 300ms budget; a wedged transaction must not hang a request", elapsed)
		}
	})
}

// TestGetTeslaToken_TakesNoLock pins the fast path. Every normal request reads a
// token that has not expired, and putting a row lock in front of that traffic
// would be a needless cost — so the read must answer while another transaction
// holds the row for update.
func TestGetTeslaToken_TakesNoLock(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	ensureAccountSchema(t)
	ctx := context.Background()
	enc := newTestEncryptor(t)
	repo := store.NewAccountRepo(testPool, enc)
	seedOndemandAccount(t, enc)

	holder := holdRow(ctx, t)
	defer func() { _ = holder.Rollback(ctx) }()

	done := make(chan error, 1)
	go func() {
		tok, err := repo.GetTeslaToken(ctx, ondemandOwner)
		if err == nil && tok.AccessToken != "access-original" {
			err = errors.New("read returned the wrong access token")
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GetTeslaToken under a held row lock: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fresh-token read BLOCKED on a row another transaction holds FOR UPDATE — the " +
			"fast path has acquired a lock it must not take")
	}
}
