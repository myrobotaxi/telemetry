package fleetsuspend

import (
	"context"
	"testing"
	"time"
)

// MYR-592 — the loop's own contract. Not the policy (that is sweep_test.go):
// only whether Run starts, stops, and refuses to start when it should.

func TestRun_StopsOnContextCancel(t *testing.T) {
	st := newFakeStore()
	// An interval far longer than the test, so no pass can fire and the only
	// thing under test is the cancel path.
	s := NewSweeper(st, &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{},
		Config{Enabled: true, Interval: time.Hour}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if st.listCalls != 0 {
		t.Errorf("candidate reads = %d, want 0 — Run must not sweep before its first tick; "+
			"a restart loop would otherwise run an irreversible action as fast as the process crashed",
			st.listCalls)
	}
}

func TestRun_RefusesToStart(t *testing.T) {
	tests := []struct {
		name   string
		store  Store
		cfg    Config
		reason string
	}{
		{
			name:   "kill switch off",
			store:  newFakeStore(),
			cfg:    Config{Enabled: false, Interval: time.Millisecond},
			reason: "the deploy-time stop button: no NEW suspensions, and nothing un-suspended",
		},
		{
			name:   "no store wired",
			store:  nil,
			cfg:    Config{Enabled: true, Interval: time.Millisecond},
			reason: "nothing can be read, so nothing may be decided",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSweeper(tc.store, &fakeConfigs{}, fakeTokens{token: "tok"}, &countingBus{}, tc.cfg, nil)
			done := make(chan struct{})
			go func() { s.Run(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("Run did not return (%s)", tc.reason)
			}
		})
	}
}

func TestConfigWithDefaults_SuspendAlwaysTrailsWarn(t *testing.T) {
	tests := []struct {
		name   string
		in     Config
		reason string
	}{
		{
			name:   "an inverted pair is corrected",
			in:     Config{WarnAfter: 5 * 24 * time.Hour, SuspendAfter: 24 * time.Hour},
			reason: "a car suspended before its warning gets no notice at all",
		},
		{
			name:   "an equal pair is corrected",
			in:     Config{WarnAfter: 4 * 24 * time.Hour, SuspendAfter: 4 * 24 * time.Hour},
			reason: "warned and suspended on the same pass is a notice with no notice period",
		},
		{
			name:   "zero values take the four/five-day policy",
			in:     Config{},
			reason: "the client's numbers are the defaults, not a caller's responsibility",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.SuspendAfter <= got.WarnAfter {
				t.Errorf("SuspendAfter (%v) <= WarnAfter (%v) (%s)",
					got.SuspendAfter, got.WarnAfter, tc.reason)
			}
		})
	}
}

// TestSuspend_UsesTheOwnersOwnToken pins the two values the Tesla call must
// carry: the owner's freshly resolved access token and the candidate's own VIN.
func TestSuspend_UsesTheOwnersOwnToken(t *testing.T) {
	st := newFakeStore(candidate(6*24*time.Hour, nil))
	cfgs := &fakeConfigs{}
	s := newHarness(t, st, cfgs, fakeTokens{token: "tok_owner"}, &countingBus{})

	s.SweepOnce(context.Background())

	if len(cfgs.tokens) != 1 || cfgs.tokens[0] != "tok_owner" {
		t.Errorf("tokens presented to Tesla = %v, want exactly the owner's", cfgs.tokens)
	}
	if len(cfgs.deleted) != 1 || cfgs.deleted[0] != "5YJ3E1EA7KF000001" {
		t.Errorf("VINs deleted = %v, want exactly the candidate's", cfgs.deleted)
	}
}

func TestRedactVIN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a full VIN shows only its last four", "5YJ3E1EA7KF000001", "***0001"},
		{"a short value is returned whole (nothing to hide)", "0001", "0001"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactVIN(tc.in); got != tc.want {
				t.Errorf("redactVIN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
