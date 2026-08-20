package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// MYR-595. The properties pinned here are the ones a fake can actually prove:
// WHO gets called, HOW MANY TIMES, and what comes back. That the lock is a real
// Postgres row lock — that a second arrival genuinely queues behind a held row
// and reads what the winner committed — is proven against a real database in
// internal/store/tesla_token_ondemand_integration_test.go.

// sequencedTokenProvider answers successive reads from a script, so a test can
// say "the stored token was expired when we looked, and fresh when we looked
// again" — which is exactly what losing a refresh race looks like from outside.
type sequencedTokenProvider struct {
	tokens []TeslaToken
	err    error
	reads  int
}

func (p *sequencedTokenProvider) GetTeslaToken(context.Context, string) (TeslaToken, error) {
	if p.err != nil {
		return TeslaToken{}, p.err
	}
	i := p.reads
	p.reads++
	if i >= len(p.tokens) {
		i = len(p.tokens) - 1
	}
	return p.tokens[i], nil
}

// countingTeslaRefresher is the Tesla call, counted. A refresh token is
// single-use, so "how many times did we call Tesla" IS the correctness question.
type countingTeslaRefresher struct {
	out   TeslaRefreshedToken
	err   error
	calls int
	spent []string
}

func (r *countingTeslaRefresher) Refresh(_ context.Context, refreshToken string) (TeslaRefreshedToken, error) {
	r.calls++
	r.spent = append(r.spent, refreshToken)
	return r.out, r.err
}

// fakeTeslaTokenRotator mimics store.RotateTeslaTokenLockedWaiting: it hands the
// callback what is stored, writes back only when the callback rotated, and fails
// the whole call when the write fails.
type fakeTeslaTokenRotator struct {
	stored   TeslaToken
	busy     bool  // the row is held by another writer for longer than the budget
	writeErr error // the rotated pair could not be persisted
	entered  int   // times the callback ran inside the lock
}

func (f *fakeTeslaTokenRotator) RotateTeslaToken(
	ctx context.Context,
	_ string,
	rotate func(context.Context, TeslaToken) (TeslaToken, bool, error),
) (TeslaToken, error) {
	if f.busy {
		return TeslaToken{}, fmt.Errorf("fake rotator: %w", ErrTeslaTokenRowBusy)
	}
	f.entered++
	next, rotated, err := rotate(ctx, f.stored)
	if err != nil {
		return TeslaToken{}, err
	}
	if !rotated {
		return f.stored, nil
	}
	if f.writeErr != nil {
		// Aborts the transaction: nothing is stored and nothing is returned.
		return TeslaToken{}, f.writeErr
	}
	f.stored = next
	return next, nil
}

func expiredToken(access, refresh string) TeslaToken {
	return TeslaToken{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(-time.Hour)}
}

func freshToken(access, refresh string) TeslaToken {
	return TeslaToken{AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestTeslaTokenRefresh_Serialized(t *testing.T) {
	writeFailed := errors.New("commit: connection reset")

	tests := []struct {
		name        string
		stored      []TeslaToken // successive plain reads
		rotator     *fakeTeslaTokenRotator
		refresher   *countingTeslaRefresher
		wantAccess  string
		wantErr     error
		wantEntered int
		wantTesla   int
		why         string
	}{
		{
			name:      "a fresh token never reaches the lock",
			stored:    []TeslaToken{freshToken("live", "r")},
			rotator:   &fakeTeslaTokenRotator{stored: freshToken("live", "r")},
			refresher: &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "new"}},
			// The fast path is every normal request. Serializing it would put a
			// row lock in front of traffic that rotates nothing.
			wantAccess: "live", wantEntered: 0, wantTesla: 0,
			why: "a plain read answered it; no transaction, no lock",
		},
		{
			name:       "an expired token is rotated inside the lock",
			stored:     []TeslaToken{expiredToken("old", "r1")},
			rotator:    &fakeTeslaTokenRotator{stored: expiredToken("old", "r1")},
			refresher:  &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "fresh", RefreshToken: "r2", ExpiresIn: 3600}},
			wantAccess: "fresh", wantEntered: 1, wantTesla: 1,
			why: "the ordinary refresh, now with the read and the write in one transaction",
		},
		{
			name:   "the second arrival adopts the winner's pair without calling Tesla",
			stored: []TeslaToken{expiredToken("old", "r1")},
			// By the time we hold the lock the row carries somebody else's
			// freshly rotated pair. Spending the refresh token we can see would
			// recreate the very race the lock just serialized away.
			rotator:    &fakeTeslaTokenRotator{stored: freshToken("winner-access", "winner-refresh")},
			refresher:  &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "loser", RefreshToken: "r2", ExpiresIn: 3600}},
			wantAccess: "winner-access", wantEntered: 1, wantTesla: 0,
			why: "the double check inside the lock is what makes serializing worth anything",
		},
		{
			name:      "a rotation that cannot be stored is not a usable token",
			stored:    []TeslaToken{expiredToken("old", "r1")},
			rotator:   &fakeTeslaTokenRotator{stored: expiredToken("old", "r1"), writeErr: writeFailed},
			refresher: &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "fresh", RefreshToken: "r2", ExpiresIn: 3600}},
			wantErr:   ErrTeslaTokenExpired, wantEntered: 1, wantTesla: 1,
			why: "the pre-MYR-595 path logged this and returned the token anyway, leaving the " +
				"stored pair dead and the caller none the wiser",
		},
		{
			name:      "Tesla's refusal surfaces",
			stored:    []TeslaToken{expiredToken("old", "r1")},
			rotator:   &fakeTeslaTokenRotator{stored: expiredToken("old", "r1")},
			refresher: &countingTeslaRefresher{err: errors.New("Tesla returned 400: invalid_grant")},
			wantErr:   ErrTeslaTokenExpired, wantEntered: 1, wantTesla: 1,
			why: "a lapsed or revoked grant still has to reach the user",
		},
		{
			name: "a contended row is re-read, and the winner's token answers it",
			// Expired when we looked; fresh when we looked again, because the
			// writer we could not get past committed in between.
			stored:     []TeslaToken{expiredToken("old", "r1"), freshToken("winner-access", "winner-refresh")},
			rotator:    &fakeTeslaTokenRotator{busy: true},
			refresher:  &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "loser", RefreshToken: "r2", ExpiresIn: 3600}},
			wantAccess: "winner-access", wantEntered: 0, wantTesla: 0,
			why: "contention on this row means another refresh of THIS account, which is a " +
				"reason to look again rather than to fail the request",
		},
		{
			name:      "a contended row that yields nothing fresh fails honestly",
			stored:    []TeslaToken{expiredToken("old", "r1")},
			rotator:   &fakeTeslaTokenRotator{busy: true},
			refresher: &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "unused"}},
			wantErr:   ErrTeslaTokenExpired, wantEntered: 0, wantTesla: 0,
			why: "a wedged transaction must cost a bounded pause and the old error surface, " +
				"never an unbounded wait",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &sequencedTokenProvider{tokens: tc.stored}
			flow := teslaTokenRefresh{
				tokens:    prov,
				refresher: tc.refresher,
				rotator:   tc.rotator,
				logger:    discardLogger(),
			}

			got, err := flow.resolve(context.Background(), "user1")

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v (%s)", err, tc.wantErr, tc.why)
				}
				if got.AccessToken != "" {
					t.Errorf("token = %q alongside an error; a failed refresh must hand back "+
						"nothing usable (%s)", got.AccessToken, tc.why)
				}
			} else {
				if err != nil {
					t.Fatalf("resolve: %v (%s)", err, tc.why)
				}
				if got.AccessToken != tc.wantAccess {
					t.Errorf("token = %q, want %q (%s)", got.AccessToken, tc.wantAccess, tc.why)
				}
			}

			if tc.rotator.entered != tc.wantEntered {
				t.Errorf("entered the lock %d time(s), want %d (%s)", tc.rotator.entered, tc.wantEntered, tc.why)
			}
			if tc.refresher.calls != tc.wantTesla {
				t.Errorf("called Tesla %d time(s), want %d — a refresh token is single-use (%s)",
					tc.refresher.calls, tc.wantTesla, tc.why)
			}
		})
	}
}

// TestTeslaTokenRefresh_StoredPairSurvivesAFailedPersist is the sibling half of
// MYR-594's finding: not only must a failed persist refuse to hand back a token,
// it must leave the stored pair alone for the account that will need re-pairing.
func TestTeslaTokenRefresh_StoredPairSurvivesAFailedPersist(t *testing.T) {
	rot := &fakeTeslaTokenRotator{
		stored:   expiredToken("old-access", "old-refresh"),
		writeErr: errors.New("store: disk full"),
	}
	flow := teslaTokenRefresh{
		tokens:    &sequencedTokenProvider{tokens: []TeslaToken{expiredToken("old-access", "old-refresh")}},
		refresher: &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "fresh", RefreshToken: "r2", ExpiresIn: 3600}},
		rotator:   rot,
		logger:    discardLogger(),
	}

	if _, err := flow.resolve(context.Background(), "user1"); !errors.Is(err, ErrTeslaTokenExpired) {
		t.Fatalf("error = %v, want ErrTeslaTokenExpired", err)
	}
	if rot.stored.RefreshToken != "old-refresh" {
		t.Errorf("stored refresh token = %q; a rotation that failed to land must leave the row "+
			"exactly as it was", rot.stored.RefreshToken)
	}
}

// TestTeslaTokenRefresh_UnserializedFallback pins the shape of a wiring with no
// rotator: it still refreshes, and it still treats a failed persist as
// non-fatal, because without the rotation's transaction there is nothing to make
// the read and the write atomic. Production wires the rotator; this is the path
// unit tests and rotator-less composition roots take.
func TestTeslaTokenRefresh_UnserializedFallback(t *testing.T) {
	upd := &recordingUpdater{err: errors.New("connection refused")}
	flow := teslaTokenRefresh{
		tokens:    &sequencedTokenProvider{tokens: []TeslaToken{expiredToken("old", "r1")}},
		refresher: &countingTeslaRefresher{out: TeslaRefreshedToken{AccessToken: "fresh", RefreshToken: "r2", ExpiresIn: 3600}},
		updater:   upd,
		logger:    discardLogger(),
	}

	got, err := flow.resolve(context.Background(), "user1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.AccessToken != "fresh" {
		t.Errorf("token = %q, want fresh", got.AccessToken)
	}
	if !upd.called {
		t.Error("the unserialized path must still try to persist")
	}
}
