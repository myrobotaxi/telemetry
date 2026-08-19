package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeActivityWriter records every Touch and models the SQL-side throttle, so a
// test can tell "the stamper skipped the call" from "the call was made and the
// database declined it" — the two halves of MYR-592's double throttle.
type fakeActivityWriter struct {
	mu    sync.Mutex
	calls []activityCall
	// stored is the last-seen instant the fake's "database" holds, per user.
	stored map[string]time.Time
	// err, when non-nil, fails every Touch.
	err error
}

type activityCall struct {
	userID   string
	now      time.Time
	throttle time.Duration
}

func newFakeActivityWriter() *fakeActivityWriter {
	return &fakeActivityWriter{stored: map[string]time.Time{}}
}

func (f *fakeActivityWriter) Touch(_ context.Context, userID string, now time.Time, throttle time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, activityCall{userID: userID, now: now, throttle: throttle})
	if f.err != nil {
		return false, f.err
	}
	prev, ok := f.stored[userID]
	if ok && now.Sub(prev) < throttle {
		return false, nil
	}
	f.stored[userID] = now
	return true, nil
}

func (f *fakeActivityWriter) snapshot() []activityCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]activityCall(nil), f.calls...)
}

func TestUserActivityStamper_Throttle(t *testing.T) {
	base := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// offsets are the instants Stamp is called at, relative to base.
		offsets   []time.Duration
		userIDs   []string
		wantCalls int
		reason    string
	}{
		{
			name:      "the first sighting of an account always writes",
			offsets:   []time.Duration{0},
			userIDs:   []string{"user_a"},
			wantCalls: 1,
			reason:    "an account with no row has never been observed; there is nothing to throttle against",
		},
		{
			name:      "a burst inside the window costs exactly one write",
			offsets:   []time.Duration{0, time.Second, time.Minute, 30 * time.Minute, 59 * time.Minute},
			userIDs:   []string{"user_a", "user_a", "user_a", "user_a", "user_a"},
			wantCalls: 1,
			reason:    "this is the whole point: a WebSocket client revalidating continuously must not be the busiest writer in the database",
		},
		{
			name:      "the window reopens once it has elapsed",
			offsets:   []time.Duration{0, time.Hour + time.Second},
			userIDs:   []string{"user_a", "user_a"},
			wantCalls: 2,
			reason:    "an account still active an hour later has to move its stamp, or a live owner ages into the sweeper",
		},
		{
			name:      "exactly at the window boundary writes (the window is half-open)",
			offsets:   []time.Duration{0, time.Hour},
			userIDs:   []string{"user_a", "user_a"},
			wantCalls: 2,
			reason:    "elapsed == throttle is outside the skip predicate; erring toward one extra write is the safe direction",
		},
		{
			name:      "the gate is per account, not global",
			offsets:   []time.Duration{0, time.Second, 2 * time.Second},
			userIDs:   []string{"user_a", "user_b", "user_c"},
			wantCalls: 3,
			reason:    "three people were seen; throttling one behind another would silently lose two of them",
		},
		{
			name:      "an empty subject is never written",
			offsets:   []time.Duration{0},
			userIDs:   []string{""},
			wantCalls: 0,
			reason:    "ValidateToken rejects an empty sub before it reaches here; a row keyed by '' would belong to nobody",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writer := newFakeActivityWriter()
			var nowIdx int
			offsets := tc.offsets
			s := NewUserActivityStamper(writer, nil).
				withThrottle(time.Hour).
				withClock(func() time.Time {
					// Consumed once per Stamp call, in order.
					d := offsets[nowIdx]
					if nowIdx < len(offsets)-1 {
						nowIdx++
					}
					return base.Add(d)
				})

			for i := range tc.offsets {
				s.Stamp(context.Background(), tc.userIDs[i])
			}

			if got := len(writer.snapshot()); got != tc.wantCalls {
				t.Errorf("Touch calls = %d, want %d (%s)", got, tc.wantCalls, tc.reason)
			}
		})
	}
}

func TestUserActivityStamper_PassesTheSameThrottleToSQL(t *testing.T) {
	writer := newFakeActivityWriter()
	s := NewUserActivityStamper(writer, nil)

	s.Stamp(context.Background(), "user_a")

	calls := writer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Touch calls = %d, want 1", len(calls))
	}
	// The in-process gate and the SQL guard MUST be the same window, or a
	// deploy's worth of cold caches writes at a cadence the database never
	// agreed to.
	if calls[0].throttle != activityThrottle {
		t.Errorf("throttle passed to the store = %v, want %v (one constant, not two that agree today)",
			calls[0].throttle, activityThrottle)
	}
}

func TestUserActivityStamper_WriteFailureIsSwallowedAndNotGated(t *testing.T) {
	writer := newFakeActivityWriter()
	writer.err = errors.New("connection refused")

	base := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	var calls int
	s := NewUserActivityStamper(writer, nil).
		withThrottle(time.Hour).
		withClock(func() time.Time {
			calls++
			return base.Add(time.Duration(calls) * time.Second)
		})

	// Must not panic and must not return anything: an authentication may never
	// fail because a bookkeeping row would not write.
	s.Stamp(context.Background(), "user_a")
	s.Stamp(context.Background(), "user_a")

	// Both attempts reach the writer. Marking the in-process gate on a FAILED
	// write would hide the outage for a full window per account, which is the
	// wrong direction to be quiet in.
	if got := len(writer.snapshot()); got != 2 {
		t.Errorf("Touch calls after a failing write = %d, want 2 (a failure must not arm the throttle)", got)
	}
}

func TestUserActivityStamper_UnwiredIsANoOp(t *testing.T) {
	tests := []struct {
		name    string
		stamper *UserActivityStamper
	}{
		{"nil stamper", nil},
		{"nil writer", NewUserActivityStamper(nil, nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Both are ordinary states — --dev mode and most tests — not errors.
			// A panic here would be a panic inside token validation.
			tc.stamper.Stamp(context.Background(), "user_a")
		})
	}
}

func TestUserActivityStamper_BackwardsClockReopensTheGate(t *testing.T) {
	writer := newFakeActivityWriter()
	base := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	// An NTP step backwards: the second reading precedes the first.
	instants := []time.Time{base.Add(time.Hour), base}
	var idx int
	s := NewUserActivityStamper(writer, nil).
		withThrottle(time.Hour).
		withClock(func() time.Time {
			v := instants[idx]
			if idx < len(instants)-1 {
				idx++
			}
			return v
		})

	s.Stamp(context.Background(), "user_a")
	s.Stamp(context.Background(), "user_a")

	// A future-stamped entry must not pin the account shut until real time
	// catches up; the SQL guard is the authority on whether the row moves.
	if got := len(writer.snapshot()); got != 2 {
		t.Errorf("Touch calls across a backwards clock = %d, want 2", got)
	}
}

// TestValidateToken_StampsOnlyTheSuccessPath is the seam test: the hook has to
// fire for a good token on either surface and stay silent for a bad one.
func TestValidateToken_StampsOnlyTheSuccessPath(t *testing.T) {
	writer := newFakeActivityWriter()
	stamper := NewUserActivityStamper(writer, nil)

	// Built by hand rather than through NewJWTAuthenticator: the constructor
	// wires a pool-backed existence cache, and this test has no pool. Same
	// shape the sibling jwt_auth_test.go cases use.
	a := &JWTAuthenticator{secret: []byte(testSecret)}
	WithActivityStamper(stamper)(a)

	tests := []struct {
		name      string
		token     string
		wantCalls int
		reason    string
	}{
		{
			name:      "a rejected token stamps nothing",
			token:     "not-a-jwt",
			wantCalls: 0,
			reason:    "the signal is 'an accepted bearer arrived'; a forged one is not evidence its subject was around",
		},
		{
			name: "an accepted token stamps its subject",
			token: signToken(t, testSecret, jwt.MapClaims{
				"sub": "user_stamped",
				"exp": time.Now().Add(time.Hour).Unix(),
			}),
			wantCalls: 1,
			reason:    "this one hook is what covers both the REST handlers and the WS handshake",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(writer.snapshot())
			_, _ = a.ValidateToken(context.Background(), tc.token)
			got := len(writer.snapshot()) - before
			if got != tc.wantCalls {
				t.Errorf("Touch calls = %d, want %d (%s)", got, tc.wantCalls, tc.reason)
			}
		})
	}
}
