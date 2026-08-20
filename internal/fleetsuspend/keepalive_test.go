package fleetsuspend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// MYR-594 — the keepalive arm. The cases that matter are the ones where the arm
// declines to act: every rotation it performs spends a SINGLE-USE credential,
// so a gate that fails open is not a missed optimisation, it is a call against
// Tesla's OAuth endpoint that nobody asked for.

// fakeKeepStore models go_tesla_token_keepalive plus the candidate read, so a
// test can assert on the state the arm left behind rather than on a call script.
type fakeKeepStore struct {
	mu sync.Mutex

	owners []KeepaliveCandidate

	successes map[string]time.Time
	failures  map[string]time.Time
	// pinned records the token generation each failure was filed against.
	pinned map[string]int64

	listErr    error
	successErr error
	failureErr error

	lastQuery KeepaliveQuery
	listCalls int
}

func newFakeKeepStore(owners ...KeepaliveCandidate) *fakeKeepStore {
	return &fakeKeepStore{
		owners:    owners,
		successes: map[string]time.Time{},
		failures:  map[string]time.Time{},
		pinned:    map[string]int64{},
	}
}

func (f *fakeKeepStore) ListStaleTokenOwners(_ context.Context, q KeepaliveQuery) ([]KeepaliveCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.lastQuery = q
	if f.listErr != nil {
		return nil, f.listErr
	}
	// The cap is the STORE's LIMIT in production, so the fake honours it here —
	// otherwise a test could pass while the arm silently ignored q.Limit.
	out := append([]KeepaliveCandidate(nil), f.owners...)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeKeepStore) RecordKeepaliveSuccess(_ context.Context, userID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.successErr != nil {
		return f.successErr
	}
	f.successes[userID] = at
	delete(f.failures, userID)
	delete(f.pinned, userID)
	return nil
}

func (f *fakeKeepStore) RecordKeepaliveFailure(_ context.Context, userID string, at time.Time, expiry int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failureErr != nil {
		return f.failureErr
	}
	f.failures[userID] = at
	f.pinned[userID] = expiry
	return nil
}

func (f *fakeKeepStore) failed(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.failures[id]
	return ok
}

func (f *fakeKeepStore) succeeded(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.successes[id]
	return ok
}

// fakeRotator stands in for the locked store rotation. It records which owners
// were asked and, crucially, whether the STORED token would have been touched —
// modelled as a per-owner "generation" that only a successful rotation moves.
type fakeRotator struct {
	mu sync.Mutex

	asked []string
	// generation is the stored grant. A rotation that fails must leave it.
	generation map[string]int
	err        error
	errFor     map[string]error
}

func newFakeRotator() *fakeRotator {
	return &fakeRotator{generation: map[string]int{}, errFor: map[string]error{}}
}

func (f *fakeRotator) RotateTeslaToken(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, userID)

	err := f.err
	if e, ok := f.errFor[userID]; ok {
		err = e
	}
	if err != nil {
		// THE STORED PAIR IS NOT TOUCHED. This is the property the real
		// store.RotateTeslaTokenLocked provides by aborting its transaction,
		// and the fake models it so the assertion below is about behaviour
		// rather than about the fake's convenience.
		return err
	}
	f.generation[userID]++
	return nil
}

func (f *fakeRotator) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (f *fakeRotator) gen(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation[id]
}

func staleOwner(id string, expiredFor time.Duration) KeepaliveCandidate {
	at := sweepNow.Add(-expiredFor)
	return KeepaliveCandidate{OwnerID: id, TokenExpiresAt: at, TokenExpiryUnix: at.Unix()}
}

// newKeepHarness builds a kill-switch-on sweeper whose only wired arm that
// matters is the keepalive. Config is taken from withDefaults rather than a
// parameter: every case here is about the SHIPPING policy, and a test that
// passed its own thresholds in would stop noticing if the defaults moved.
func newKeepHarness(t *testing.T, ks *fakeKeepStore, rot TokenRotator) *Sweeper {
	t.Helper()
	cfg := Config{Enabled: true}
	var opts []SweeperOption
	if ks != nil && rot != nil {
		opts = append(opts, WithKeepalive(ks, rot))
	}
	return NewSweeper(newFakeStore(), &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
		cfg, nil, opts...).
		withClock(func() time.Time { return sweepNow })
}

// TestKeepalive_Thresholds pins the four gates the candidate read is asked for.
// The arm cannot enforce them itself — the SQL does — so what is asserted is
// that it computes and passes the right instants, because an off-by-one here is
// a silent policy change nothing else would catch.
func TestKeepalive_Thresholds(t *testing.T) {
	ks := newFakeKeepStore()
	s := newKeepHarness(t, ks, newFakeRotator())

	s.KeepaliveOnce(context.Background())

	tests := []struct {
		name   string
		got    time.Time
		want   time.Time
		reason string
	}{
		{
			name: "the staleness gate is 45 days of no rotation",
			got:  ks.lastQuery.StaleBefore, want: sweepNow.Add(-45 * 24 * time.Hour),
			reason: "Tesla lapses an unused refresh token at ~90 days; half of that is the margin",
		},
		{
			name: "the quiet gate is the SUSPENSION threshold, not a knob of its own",
			got:  ks.lastQuery.QuietBefore, want: sweepNow.Add(-5 * 24 * time.Hour),
			reason: "the owner must be at least as silent as the thing that suspended them, " +
				"so this arm is never rotating a grant somebody is about to reconnect with",
		},
		{
			name: "the attempt gate is a day",
			got:  ks.lastQuery.AttemptBefore, want: sweepNow.Add(-24 * time.Hour),
			reason: "the rotation-loop brake: a rotation whose store failed must not be retried hourly",
		},
		{
			name: "the failure cooldown is a week",
			got:  ks.lastQuery.FailureBefore, want: sweepNow.Add(-7 * 24 * time.Hour),
			reason: "a refused grant is lapsed or revoked; no retry cures either",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.got.Equal(tc.want) {
				t.Errorf("threshold = %v, want %v (%s)", tc.got, tc.want, tc.reason)
			}
		})
	}
}

// TestKeepalive_CapsThePass is the blast radius. Twenty is not a tuning
// preference: these are signed calls against Tesla's OAuth endpoint, where
// being throttled costs credentials rather than latency.
func TestKeepalive_CapsThePass(t *testing.T) {
	owners := make([]KeepaliveCandidate, 0, 64)
	for i := range 50 {
		owners = append(owners, staleOwner(fmt.Sprintf("user_%02d", i), 60*24*time.Hour))
	}
	ks := newFakeKeepStore(owners...)
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if ks.lastQuery.Limit != 20 {
		t.Errorf("query limit = %d, want 20 — the cap has to reach the STORE, "+
			"or a backlog arrives in memory and is only trimmed afterwards", ks.lastQuery.Limit)
	}
	if got := len(rot.calls()); got != 20 {
		t.Errorf("rotations = %d, want 20; one pass must not burst on Tesla's OAuth endpoint", got)
	}
	if res.Rotated != 20 || res.Due != 20 {
		t.Errorf("result = %+v, want 20 due and 20 rotated", res)
	}
}

// TestKeepalive_OncePerOwnerPerPass — the candidate read groups by owner, so a
// person with three suspended cars is one rotation. Modelled here by handing the
// arm the list it would really get and asserting it adds nothing of its own.
func TestKeepalive_OncePerOwnerPerPass(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_a", 50*24*time.Hour))
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	s.KeepaliveOnce(context.Background())

	if got := rot.calls(); len(got) != 1 {
		t.Errorf("rotations for one owner = %v, want exactly one — a grant is per ACCOUNT, "+
			"and rotating it twice in a pass would spend the pair the first rotation just minted", got)
	}
}

// TestKeepalive_RefusalLeavesTheTokenAndCoolsDown is the failure arm in full.
func TestKeepalive_RefusalLeavesTheTokenAndCoolsDown(t *testing.T) {
	owner := staleOwner("user_dead", 70*24*time.Hour)
	ks := newFakeKeepStore(owner)
	rot := newFakeRotator()
	rot.err = errors.New("Tesla returned 401: invalid_grant")
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if rot.gen("user_dead") != 0 {
		t.Error("a refused rotation moved the stored grant; the stored token is the only " +
			"evidence of which account needs re-pairing and must be left exactly as it is")
	}
	if !ks.failed("user_dead") {
		t.Error("a refusal was not recorded; without the record the arm re-asks every hour, " +
			"forever, against an account Tesla has already told us is gone")
	}
	if ks.pinned["user_dead"] != owner.TokenExpiryUnix {
		t.Errorf("failure pinned to expiry %d, want the observed %d — the cooldown is released "+
			"early when the token row CHANGES, which needs the generation it failed on",
			ks.pinned["user_dead"], owner.TokenExpiryUnix)
	}
	if res.Refused != 1 || res.Rotated != 0 {
		t.Errorf("result = %+v, want one refusal and no rotation", res)
	}
}

// TestKeepalive_RefusalIsNotRetriedNextPass pins the cooldown end to end: the
// store stops offering the owner, and the arm asks for nothing it was not
// offered.
func TestKeepalive_RefusalIsNotRetriedNextPass(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_dead", 70*24*time.Hour))
	rot := newFakeRotator()
	rot.err = errors.New("invalid_grant")
	s := newKeepHarness(t, ks, rot)

	s.KeepaliveOnce(context.Background())
	// A real candidate query excludes an owner inside their cooldown. Model the
	// consequence: the second and third passes see nobody.
	ks.owners = nil
	s.KeepaliveOnce(context.Background())
	s.KeepaliveOnce(context.Background())

	if got := len(rot.calls()); got != 1 {
		t.Errorf("rotations across three passes = %d, want 1", got)
	}
}

// TestKeepalive_BusyIsSkippedWithoutACooldown is the anti-race arm. A contended
// row means somebody else is already rotating this grant — the healthiest signal
// there is — and burning a week's cooldown on it would suppress a real keepalive
// because a reconnect happened to be in flight for a second.
func TestKeepalive_BusyIsSkippedWithoutACooldown(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_busy", 50*24*time.Hour))
	rot := newFakeRotator()
	rot.err = fmt.Errorf("rotate: %w", ErrRotationBusy)
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if ks.failed("user_busy") {
		t.Error("a contended row was recorded as a failure; the owner would then be ignored " +
			"for a week because another writer held the lock for a moment")
	}
	if ks.succeeded("user_busy") {
		t.Error("a contended row was recorded as a success; nothing was rotated")
	}
	if res.Busy != 1 || res.Refused != 0 {
		t.Errorf("result = %+v, want one busy and no refusal", res)
	}
}

// TestKeepalive_KillSwitch — TELEMETRY_INACTIVITY_SUSPENSION_ENABLED=false is
// the whole sweeper's stop button, and an arm that spends credentials must
// honour it at its OWN entry point rather than trusting its caller to.
func TestKeepalive_KillSwitch(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_a", 60*24*time.Hour))
	rot := newFakeRotator()
	s := NewSweeper(newFakeStore(), &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
		Config{Enabled: false}, nil, WithKeepalive(ks, rot)).
		withClock(func() time.Time { return sweepNow })

	res := s.KeepaliveOnce(context.Background())

	if ks.listCalls != 0 {
		t.Errorf("candidate reads with the kill switch off = %d, want 0", ks.listCalls)
	}
	if len(rot.calls()) != 0 {
		t.Error("a token was rotated with the kill switch off")
	}
	if res != (KeepaliveResult{}) {
		t.Errorf("result = %+v, want the zero value", res)
	}
}

// TestKeepalive_UnwiredDoesNothing is the CI shape and the shape of any deploy
// without Tesla OAuth credentials.
func TestKeepalive_UnwiredDoesNothing(t *testing.T) {
	tests := []struct {
		name   string
		store  KeepaliveStore
		rot    TokenRotator
		reason string
	}{
		{"no store", nil, newFakeRotator(), "nothing to read candidates from, and no failure could be recorded"},
		{"no rotator", newFakeKeepStore(), nil, "candidates it cannot act on"},
		{"neither", nil, nil, "the ordinary unconfigured deploy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSweeper(newFakeStore(), &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
				Config{Enabled: true}, nil, WithKeepalive(tc.store, tc.rot)).
				withClock(func() time.Time { return sweepNow })
			if s.keep != nil {
				t.Errorf("the arm was wired with half its collaborators (%s)", tc.reason)
			}
			if res := s.KeepaliveOnce(context.Background()); res != (KeepaliveResult{}) {
				t.Errorf("result = %+v, want the zero value", res)
			}
		})
	}
}

// TestKeepalive_HoldsOnAnUnreadableList — no list, no rotations. Stated because
// the alternative (acting on a partial or errored read) would spend credentials
// on a guess.
func TestKeepalive_HoldsOnAnUnreadableList(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_a", 60*24*time.Hour))
	ks.listErr = errors.New("boom")
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if len(rot.calls()) != 0 {
		t.Error("rotated a token after failing to read the candidate list")
	}
	if res != (KeepaliveResult{}) {
		t.Errorf("result = %+v, want the zero value", res)
	}
}

// TestKeepalive_SuccessSurvivesAnUnrecordedStamp. The rotation moved the
// authoritative clock — the grant's stored expiry is now hours away — so the
// staleness gate excludes this owner for a full window with or without our note.
// A lost stamp must therefore not be reported as a failure or cool the owner
// down.
func TestKeepalive_SuccessSurvivesAnUnrecordedStamp(t *testing.T) {
	ks := newFakeKeepStore(staleOwner("user_a", 60*24*time.Hour))
	ks.successErr = errors.New("boom")
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if res.Rotated != 1 || res.Refused != 0 {
		t.Errorf("result = %+v, want the rotation counted as the success it was", res)
	}
	if ks.failed("user_a") {
		t.Error("a successful rotation was cooled down because its bookkeeping failed")
	}
	if rot.gen("user_a") != 1 {
		t.Error("the grant was not rotated")
	}
}

// TestKeepalive_ShutdownStopsBetweenOwners. The free point is before a
// single-use token has been put in flight for the next owner.
func TestKeepalive_ShutdownStopsBetweenOwners(t *testing.T) {
	ks := newFakeKeepStore(
		staleOwner("user_a", 60*24*time.Hour),
		staleOwner("user_b", 60*24*time.Hour),
	)
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := s.KeepaliveOnce(ctx)

	if len(rot.calls()) != 0 {
		t.Errorf("rotations during shutdown = %d, want 0", len(rot.calls()))
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", res.Skipped)
	}
}

// TestKeepalive_RunsInsideASweepPass wires the arm to the whole sweeper and
// asserts a pass drives it — including that it runs AFTER the irreversible arms
// and NOT AT ALL on a pass that already held.
func TestKeepalive_RunsInsideASweepPass(t *testing.T) {
	t.Run("a healthy pass drives the arm", func(t *testing.T) {
		st := newFakeStore(candidate(6*24*time.Hour, nil))
		ks := newFakeKeepStore(staleOwner("user_a", 60*24*time.Hour))
		rot := newFakeRotator()
		s := NewSweeper(st, &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
			Config{Enabled: true}, nil, WithKeepalive(ks, rot)).
			withClock(func() time.Time { return sweepNow })

		res := s.SweepOnce(context.Background())

		if res.Suspended != 1 {
			t.Errorf("Suspended = %d; the keepalive must not disturb the arms it runs beside", res.Suspended)
		}
		if res.Keepalive.Rotated != 1 {
			t.Errorf("Keepalive = %+v, want one rotation", res.Keepalive)
		}
	})

	t.Run("a pass that held on the episode reset spends no credentials", func(t *testing.T) {
		st := newFakeStore(candidate(6*24*time.Hour, nil))
		st.resetErr = errors.New("boom")
		ks := newFakeKeepStore(staleOwner("user_a", 60*24*time.Hour))
		rot := newFakeRotator()
		s := NewSweeper(st, &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
			Config{Enabled: true}, nil, WithKeepalive(ks, rot)).
			withClock(func() time.Time { return sweepNow })

		s.SweepOnce(context.Background())

		if ks.listCalls != 0 || len(rot.calls()) != 0 {
			t.Error("the keepalive ran on a pass that had already told us the database is sick; " +
				"that is not the moment to start spending single-use credentials against it")
		}
	})
}

// TestKeepalive_DeployDay is the first pass after this ships, when
// go_tesla_token_keepalive is empty and nobody has a record of any kind.
//
// THE ARM ACTS ANYWAY, and the choice is worth pinning because the obvious
// alternative is defensible and wrong. Stamping every dormant owner "seen now"
// and waiting a full window before touching anybody would indeed prevent a
// deploy-day burst — by ignoring, for 45 days, exactly the grants that are 80
// days stale and about to lapse. Those accounts are the entire reason the arm
// exists. The burst is prevented by the cap instead, which costs at most a few
// extra hourly passes to drain.
//
// This works because the staleness clock is NOT in the Go-owned table: it is
// the OAuth row's own expiry, which every writer of that row already moves. A
// fresh deploy therefore reads the real history rather than a stamp it just
// invented, and "no record" means only "never attempted", never "assume young".
func TestKeepalive_DeployDay(t *testing.T) {
	owners := make([]KeepaliveCandidate, 0, 64)
	// Stalest first, as the candidate query orders them.
	for i := range 40 {
		owners = append(owners, staleOwner(fmt.Sprintf("user_%02d", i), time.Duration(80-i)*24*time.Hour))
	}
	ks := newFakeKeepStore(owners...)
	rot := newFakeRotator()
	s := newKeepHarness(t, ks, rot)

	res := s.KeepaliveOnce(context.Background())

	if res.Rotated != 20 {
		t.Errorf("first-pass rotations = %d, want the cap of 20 — a fleet with a long dormant "+
			"tail must not become one burst of OAuth calls", res.Rotated)
	}
	called := rot.calls()
	if len(called) == 0 || called[0] != "user_00" {
		t.Errorf("first rotation = %v, want the STALEST grant (user_00, 80 days) — with a cap, "+
			"the pass must spend its attempts on the tokens closest to lapsing", called)
	}
	if ks.failed("user_00") || ks.succeeded("user_39") {
		t.Error("the arm invented a record for an owner it did not act on; " +
			"the only writes are the outcome of an attempt")
	}
}

func TestKeepaliveConfigWithDefaults(t *testing.T) {
	tests := []struct {
		name   string
		in     KeepaliveConfig
		want   time.Duration
		reason string
	}{
		{
			name: "zero takes the 45-day policy", in: KeepaliveConfig{},
			want:   45 * 24 * time.Hour,
			reason: "the margin is the default, not a caller's responsibility",
		},
		{
			name: "a window past the lapse is corrected", in: KeepaliveConfig{RefreshAfter: 120 * 24 * time.Hour},
			want:   45 * 24 * time.Hour,
			reason: "Tesla lapses at ~90 days, so this arm would only ever refresh grants already dead",
		},
		{
			name: "a tighter window is respected", in: KeepaliveConfig{RefreshAfter: 30 * 24 * time.Hour},
			want:   30 * 24 * time.Hour,
			reason: "erring toward fresher is the caller's to choose",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.RefreshAfter != tc.want {
				t.Errorf("RefreshAfter = %v, want %v (%s)", got.RefreshAfter, tc.want, tc.reason)
			}
			if got.MaxPerSweep <= 0 || got.Timeout <= 0 || got.RetryAfter <= 0 || got.FailureCooldown <= 0 {
				t.Errorf("a zero-valued guard survived withDefaults: %+v", got)
			}
		})
	}
}
