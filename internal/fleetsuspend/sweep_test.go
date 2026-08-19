package fleetsuspend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-592 — the sweeper's arms. Every case here is a state a real fleet reaches;
// the ones that matter most are the failures, because every one of them must
// HOLD rather than advance a vehicle toward an irreversible action.

var sweepNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// fakeStore models the two Go-owned tables rather than a call script, so a test
// can assert on the state the sweeper left behind.
type fakeStore struct {
	mu sync.Mutex

	candidates []Candidate
	warned     map[string]time.Time
	suspended  map[string]time.Time

	resetCount int64
	listCalls  int

	resetErr     error
	listErr      error
	markWarnErr  error
	markSuspdErr error

	lastWarnBefore time.Time
}

func newFakeStore(candidates ...Candidate) *fakeStore {
	return &fakeStore{
		candidates: candidates,
		warned:     map[string]time.Time{},
		suspended:  map[string]time.Time{},
	}
}

func (f *fakeStore) ClearReturnedOwnerEpisodes(_ context.Context, warnBefore time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastWarnBefore = warnBefore
	if f.resetErr != nil {
		return 0, f.resetErr
	}
	return f.resetCount, nil
}

func (f *fakeStore) ListInactiveOwnerVehicles(_ context.Context, _ time.Time, _ int) ([]Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Candidate(nil), f.candidates...), nil
}

func (f *fakeStore) MarkWarned(_ context.Context, vehicleID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markWarnErr != nil {
		return f.markWarnErr
	}
	f.warned[vehicleID] = at
	return nil
}

func (f *fakeStore) MarkSuspended(_ context.Context, vehicleID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markSuspdErr != nil {
		return f.markSuspdErr
	}
	f.suspended[vehicleID] = at
	return nil
}

func (f *fakeStore) wasWarned(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.warned[id]
	return ok
}

func (f *fakeStore) wasSuspended(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.suspended[id]
	return ok
}

// fakeConfigs records every Tesla config delete, with the exact VIN.
type fakeConfigs struct {
	mu      sync.Mutex
	deleted []string
	tokens  []string
	err     error
}

func (f *fakeConfigs) DeleteTelemetryConfig(_ context.Context, token, vin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	f.deleted = append(f.deleted, vin)
	return f.err
}

func (f *fakeConfigs) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

type fakeTokens struct {
	token string
	err   error
}

func (f fakeTokens) AccessToken(_ context.Context, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// countingBus records published events and never fails.
type countingBus struct {
	mu     sync.Mutex
	events []events.Event
	err    error
}

func (b *countingBus) Publish(_ context.Context, e events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return b.err
}

func (b *countingBus) published() []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]events.Event(nil), b.events...)
}

func candidate(silentFor time.Duration, warnedAt *time.Time) Candidate {
	return Candidate{
		VehicleID:       "veh_1",
		VIN:             "5YJ3E1EA7KF000001",
		OwnerID:         "user_owner",
		VehicleName:     "Optimus",
		OwnerLastSeenAt: sweepNow.Add(-silentFor),
		WarnedAt:        warnedAt,
	}
}

func newHarness(t *testing.T, st *fakeStore, cfgs ConfigRemover, tok TokenSource, bus Publisher) *Sweeper {
	t.Helper()
	return NewSweeper(st, cfgs, tok, bus, Config{Enabled: true}, nil).
		withClock(func() time.Time { return sweepNow })
}

func TestSweepOnce_Arms(t *testing.T) {
	warnedYesterday := sweepNow.Add(-25 * time.Hour)

	tests := []struct {
		name         string
		candidate    Candidate
		wantWarned   bool
		wantDeleted  bool
		wantEvents   int
		wantDecision func(Result) bool
		reason       string
	}{
		{
			name:         "four days silent, never warned → one warning, no delete",
			candidate:    candidate(4*24*time.Hour+time.Minute, nil),
			wantWarned:   true,
			wantDeleted:  false,
			wantEvents:   1,
			wantDecision: func(r Result) bool { return r.Warned == 1 && r.Suspended == 0 },
			reason:       "the notice period is the whole point; suspending here would give none",
		},
		{
			name:         "four days silent, ALREADY warned → nothing",
			candidate:    candidate(4*24*time.Hour+time.Minute, &warnedYesterday),
			wantWarned:   false,
			wantDeleted:  false,
			wantEvents:   0,
			wantDecision: func(r Result) bool { return r.Warned == 0 && r.Suspended == 0 },
			reason:       "once per episode — an hourly re-warn is twenty-four identical banners",
		},
		{
			name:         "five days silent → suspend",
			candidate:    candidate(5*24*time.Hour+time.Minute, &warnedYesterday),
			wantWarned:   false,
			wantDeleted:  true,
			wantEvents:   0,
			wantDecision: func(r Result) bool { return r.Suspended == 1 },
			reason:       "the threshold is what suspends, and no push accompanies it (client ruling)",
		},
		{
			name:         "exactly at the suspension threshold suspends (the boundary is inclusive)",
			candidate:    candidate(5*24*time.Hour, &warnedYesterday),
			wantWarned:   false,
			wantDeleted:  true,
			wantEvents:   0,
			wantDecision: func(r Result) bool { return r.Suspended == 1 },
			reason:       "an owner exactly five days silent is five days silent",
		},
		{
			name:        "past BOTH thresholds with no warning on record → suspend, do not warn first",
			candidate:   candidate(6*24*time.Hour, nil),
			wantWarned:  false,
			wantDeleted: true,
			wantEvents:  0,
			wantDecision: func(r Result) bool {
				return r.Suspended == 1 && r.Warned == 0
			},
			reason: "a server down over day four wakes to a car past both; warning it now would be " +
				"a notice with no notice period",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore(tc.candidate)
			cfgs := &fakeConfigs{}
			bus := &countingBus{}
			s := newHarness(t, st, cfgs, fakeTokens{token: "tok"}, bus)

			res := s.SweepOnce(context.Background())

			if !tc.wantDecision(res) {
				t.Errorf("result = %+v (%s)", res, tc.reason)
			}
			if st.wasWarned("veh_1") != tc.wantWarned {
				t.Errorf("warned = %v, want %v (%s)", st.wasWarned("veh_1"), tc.wantWarned, tc.reason)
			}
			if got := len(cfgs.calls()); (got > 0) != tc.wantDeleted {
				t.Errorf("config deletes = %d, want deleted=%v (%s)", got, tc.wantDeleted, tc.reason)
			}
			if tc.wantDeleted {
				if cfgs.calls()[0] != "5YJ3E1EA7KF000001" {
					t.Errorf("deleted VIN = %q, want the candidate's own VIN", cfgs.calls()[0])
				}
				if !st.wasSuspended("veh_1") {
					t.Error("the config was removed but the episode was never stamped")
				}
			}
			if got := len(bus.published()); got != tc.wantEvents {
				t.Errorf("published events = %d, want %d (%s)", got, tc.wantEvents, tc.reason)
			}
		})
	}
}

// TestSweepOnce_NoSuspensionPush pins the client's ruling from the other side:
// the ONLY event this package publishes is the day-4 warning.
func TestSweepOnce_NoSuspensionPush(t *testing.T) {
	st := newFakeStore(candidate(9*24*time.Hour, nil))
	bus := &countingBus{}
	s := newHarness(t, st, &fakeConfigs{}, fakeTokens{token: "tok"}, bus)

	s.SweepOnce(context.Background())

	if got := len(bus.published()); got != 0 {
		t.Errorf("published %d events on a suspension; want 0 — the client asked for the "+
			"in-app notice and the day-4 warning, and nothing at the moment of suspension", got)
	}
}

func TestSweepOnce_WarningIsIdempotentAcrossPasses(t *testing.T) {
	st := newFakeStore(candidate(4*24*time.Hour+time.Hour, nil))
	bus := &countingBus{}
	s := newHarness(t, st, &fakeConfigs{}, fakeTokens{token: "tok"}, bus)

	// First pass warns and marks. A real candidate query would then return
	// warned_at set; model that.
	s.SweepOnce(context.Background())
	at := st.warned["veh_1"]
	st.candidates[0].WarnedAt = &at
	s.SweepOnce(context.Background())
	s.SweepOnce(context.Background())

	if got := len(bus.published()); got != 1 {
		t.Errorf("warnings published across three passes = %d, want 1", got)
	}
}

func TestSweepOnce_AlreadySuspendedIsNeverReExamined(t *testing.T) {
	// The candidate query excludes suspended vehicles, so an already-suspended
	// car simply never appears. This asserts the sweeper does not go looking.
	st := newFakeStore()
	cfgs := &fakeConfigs{}
	s := newHarness(t, st, cfgs, fakeTokens{token: "tok"}, &countingBus{})

	res := s.SweepOnce(context.Background())

	if res.Candidates != 0 || len(cfgs.calls()) != 0 {
		t.Errorf("result = %+v, deletes = %d; a suspended car must cost nothing per hour forever",
			res, len(cfgs.calls()))
	}
}

func TestSweepOnce_HoldsOnEveryFailure(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fakeStore, *fakeConfigs, *fakeTokens)
		wantDel bool
		reason  string
	}{
		{
			name:    "an unreadable episode reset holds the whole pass",
			mutate:  func(st *fakeStore, _ *fakeConfigs, _ *fakeTokens) { st.resetErr = errors.New("boom") },
			wantDel: false,
			reason:  "proceeding risks re-warning people who are demonstrably back",
		},
		{
			name:    "an unreadable candidate list holds",
			mutate:  func(st *fakeStore, _ *fakeConfigs, _ *fakeTokens) { st.listErr = errors.New("boom") },
			wantDel: false,
			reason:  "no list, no decisions",
		},
		{
			name:    "a failing Tesla delete does not stamp a suspension",
			mutate:  func(_ *fakeStore, c *fakeConfigs, _ *fakeTokens) { c.err = errors.New("502") },
			wantDel: true,
			reason:  "the call was attempted; the STAMP is what must not happen",
		},
		{
			name:    "an unresolvable Tesla token holds before any call",
			mutate:  func(_ *fakeStore, _ *fakeConfigs, tk *fakeTokens) { tk.err = errors.New("no account") },
			wantDel: false,
			reason:  "the truth is unknown; the next pass costs one lookup",
		},
		{
			name:    "a failing suspension stamp is a hold, not a success",
			mutate:  func(st *fakeStore, _ *fakeConfigs, _ *fakeTokens) { st.markSuspdErr = errors.New("boom") },
			wantDel: true,
			reason:  "the delete is idempotent, so retrying next pass is free",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore(candidate(6*24*time.Hour, nil))
			cfgs := &fakeConfigs{}
			tk := &fakeTokens{token: "tok"}
			tc.mutate(st, cfgs, tk)

			s := newHarness(t, st, cfgs, tk, &countingBus{})
			res := s.SweepOnce(context.Background())

			if st.wasSuspended("veh_1") {
				t.Errorf("a suspension was stamped despite %s (%s)", tc.name, tc.reason)
			}
			if got := len(cfgs.calls()) > 0; got != tc.wantDel {
				t.Errorf("config delete attempted = %v, want %v (%s)", got, tc.wantDel, tc.reason)
			}
			if res.Suspended != 0 {
				t.Errorf("Suspended = %d, want 0", res.Suspended)
			}
		})
	}
}

// TestSweepOnce_UnwiredProxyWarnsButNeverSuspends is the CI/test shape and the
// shape of any deploy without a proxy. It pins BOTH halves of the promise the
// package header makes — "still WARNS but never suspends" — including the
// past-both-thresholds case, where warning is the only thing such a deploy can
// do and silence would be the alternative.
func TestSweepOnce_UnwiredProxyWarnsButNeverSuspends(t *testing.T) {
	warnedYesterday := sweepNow.Add(-25 * time.Hour)

	tests := []struct {
		name       string
		cfgs       ConfigRemover
		tokens     TokenSource
		candidate  Candidate
		wantWarned bool
		reason     string
	}{
		{
			name: "no config remover", cfgs: nil, tokens: fakeTokens{token: "tok"},
			candidate: candidate(6*24*time.Hour, nil), wantWarned: true,
			reason: "no call to make, so nothing can be suspended",
		},
		{
			name: "no token source", cfgs: &fakeConfigs{}, tokens: nil,
			candidate: candidate(6*24*time.Hour, nil), wantWarned: true,
			reason: "nothing to authenticate the call with; neither half substitutes for the other",
		},
		{
			name: "neither", cfgs: nil, tokens: nil,
			candidate: candidate(6*24*time.Hour, nil), wantWarned: true,
			reason: "a car first observed past BOTH thresholds still gets its notice — " +
				"falling through to a permanent hold would mean its owner never hears anything at all",
		},
		{
			name: "already warned, still cannot suspend", cfgs: nil, tokens: nil,
			candidate: candidate(6*24*time.Hour, &warnedYesterday), wantWarned: false,
			reason: "the fallback fires ONCE per episode, like every other warning",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore(tc.candidate)
			s := newHarness(t, st, tc.cfgs, tc.tokens, &countingBus{})

			res := s.SweepOnce(context.Background())

			if st.wasSuspended("veh_1") {
				t.Error("stamped a suspension it could not perform — every consumer would then " +
					"read a streaming, still-billing car as disconnected, behind a reconnect that also cannot work")
			}
			if st.wasWarned("veh_1") != tc.wantWarned {
				t.Errorf("warned = %v, want %v (%s)", st.wasWarned("veh_1"), tc.wantWarned, tc.reason)
			}
			if tc.wantWarned && res.Warned != 1 {
				t.Errorf("Warned = %d, want 1 (%s)", res.Warned, tc.reason)
			}
			if !tc.wantWarned && res.Held != 1 {
				t.Errorf("Held = %d, want 1 (%s)", res.Held, tc.reason)
			}
		})
	}
}

// TestSweepOnce_MarkBeforePublish pins the ordering that makes the warning
// once-per-episode even when the bus is broken.
func TestSweepOnce_MarkBeforePublish(t *testing.T) {
	st := newFakeStore(candidate(4*24*time.Hour+time.Hour, nil))
	st.markWarnErr = errors.New("boom")
	bus := &countingBus{}
	s := newHarness(t, st, &fakeConfigs{}, fakeTokens{token: "tok"}, bus)

	res := s.SweepOnce(context.Background())

	if len(bus.published()) != 0 {
		t.Error("published a warning it could not record — the next pass would publish it again, " +
			"and the pass after that, once an hour until suspension")
	}
	if res.Held != 1 {
		t.Errorf("Held = %d, want 1", res.Held)
	}
}

func TestSweepOnce_ResetIsCountedAndRunsFirst(t *testing.T) {
	st := newFakeStore()
	st.resetCount = 3
	s := newHarness(t, st, &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{})

	res := s.SweepOnce(context.Background())

	if res.Reset != 3 {
		t.Errorf("Reset = %d, want 3", res.Reset)
	}
	// The reset threshold must be the WARN threshold, not the suspension one:
	// an owner back on day 4.5 has to end their episode, or a stale warned_at
	// suppresses the NEXT episode's warning entirely.
	if want := sweepNow.Add(-4 * 24 * time.Hour); !st.lastWarnBefore.Equal(want) {
		t.Errorf("reset threshold = %v, want the WARN threshold %v", st.lastWarnBefore, want)
	}
}

func TestSweepOnce_ShutdownHoldsRemainingVehicles(t *testing.T) {
	// Two DISTINCT vehicles, because a real candidate list cannot contain the
	// same car twice and because the assertion below is that NEITHER of them
	// moved — which a single repeated id could not tell apart from one of them
	// moving.
	first := candidate(6*24*time.Hour, nil)
	second := candidate(6*24*time.Hour, nil)
	second.VehicleID = "veh_2"
	second.VIN = "5YJ3E1EA7KF000002"

	st := newFakeStore(first, second)
	cfgs := &fakeConfigs{}
	s := newHarness(t, st, cfgs, fakeTokens{token: "tok"}, &countingBus{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := s.SweepOnce(ctx)

	if len(cfgs.calls()) != 0 {
		t.Errorf("config deletes during shutdown = %d, want 0", len(cfgs.calls()))
	}
	// Shutdown is honoured at the only free point — between vehicles, before
	// anything irreversible has started — so neither car is warned either.
	if st.wasWarned("veh_1") || st.wasWarned("veh_2") {
		t.Error("a vehicle was warned during shutdown; the pass must stop at the item boundary")
	}
	if res.Held != 2 {
		t.Errorf("Held = %d, want 2", res.Held)
	}
}
