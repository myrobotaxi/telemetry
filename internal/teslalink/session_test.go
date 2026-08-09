package teslalink

import (
	"testing"
	"time"
)

func TestSessionStore_Take(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		put       bool
		takeState string
		// advance the clock by this much between Put and Take.
		advance  time.Duration
		wantTake TakeResult
	}{
		{name: "valid within ttl", put: true, takeState: "s1", advance: time.Minute, wantTake: TakeOK},
		// MYR-517: an expired session is no longer indistinguishable from one
		// that never existed. It is the difference between "you took too long"
		// and "this state was never ours", and only the first is a clean
		// re-entry into /start.
		{name: "expired past ttl", put: true, takeState: "s1", advance: 31 * time.Minute, wantTake: TakeExpired},
		{name: "missing state", put: false, takeState: "nope", advance: 0, wantTake: TakeUnknown},
		{name: "wrong state", put: true, takeState: "other", advance: 0, wantTake: TakeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := base
			store := NewSessionStore(30 * time.Minute).WithClock(func() time.Time { return now })
			if tt.put {
				store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})
			}
			now = base.Add(tt.advance)

			sess, taken := store.Take(tt.takeState)
			if taken != tt.wantTake {
				t.Fatalf("Take result: got %v, want %v", taken, tt.wantTake)
			}
			if taken == TakeOK {
				if sess.UserID != "u1" || sess.PKCEVerifier != "v1" {
					t.Errorf("session fields: got %+v", sess)
				}
			}
		})
	}
}

// A consent flow that takes longer than ten minutes is ordinary, not abusive:
// a first-time owner may have to recover a password, clear MFA and read six
// permission checkboxes on a phone. Ten minutes cost Spencer White his first
// link attempt (MYR-517); the store must now carry that flow.
func TestSessionStore_SurvivesASlowFirstConsent(t *testing.T) {
	now := time.Date(2026, 8, 9, 17, 55, 0, 0, time.UTC)
	store := NewSessionStore(30 * time.Minute).WithClock(func() time.Time { return now })
	store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})

	now = now.Add(12 * time.Minute) // longer than the old TTL, well inside a real flow
	if _, taken := store.Take("s1"); taken != TakeOK {
		t.Fatalf("a 12-minute consent flow must survive; got %v", taken)
	}
}

func TestSessionStore_SingleUse(t *testing.T) {
	store := NewSessionStore(30 * time.Minute)
	store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})

	if _, taken := store.Take("s1"); taken != TakeOK {
		t.Fatal("first Take should succeed")
	}
	if _, taken := store.Take("s1"); taken == TakeOK {
		t.Error("second Take must fail — sessions are single-use")
	}
	if store.Len() != 0 {
		t.Errorf("store should be empty after Take, got %d", store.Len())
	}
}

func TestSessionStore_PutReapsExpired(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(30 * time.Minute).WithClock(func() time.Time { return now })

	store.Put(Session{State: "old", UserID: "u1"})
	now = now.Add(31 * time.Minute) // "old" is now expired
	store.Put(Session{State: "fresh", UserID: "u2"})

	// The expired "old" session must have been reaped by the second Put.
	if store.Len() != 1 {
		t.Fatalf("expected 1 live session after reap, got %d", store.Len())
	}
	if _, taken := store.Take("old"); taken == TakeOK {
		t.Error("expired session should have been reaped")
	}
	if _, taken := store.Take("fresh"); taken != TakeOK {
		t.Error("fresh session should survive")
	}
}

// MYR-517. The cap used to be ONE, so a second /start invalidated the state of
// the browser flow the owner was standing in — a duplicate request from the
// client produced an "expired" link that no amount of patience could fix. The
// cap now tolerates a duplicate and evicts the OLDEST only when it binds.
func TestSessionStore_PerUserCap(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(30 * time.Minute).WithClock(func() time.Time { return now })

	put := func(state, user string) {
		store.Put(Session{State: state, PKCEVerifier: "v-" + state, UserID: user})
		now = now.Add(time.Second) // keep insertion order distinguishable
	}

	put("state-A", "u1")
	put("state-X", "u2") // another user is never touched by u1's cap
	put("state-B", "u1")

	// A duplicate /start must NOT strand the flow the owner is already in.
	if _, taken := store.Take("state-A"); taken != TakeOK {
		t.Fatalf("a duplicate /start must not invalidate the live flow; got %v", taken)
	}

	put("state-A", "u1")
	put("state-C", "u1")
	put("state-D", "u1") // cap binds: u1's oldest live session (state-B) goes

	if _, taken := store.Take("state-B"); taken == TakeOK {
		t.Error("the oldest session should have been evicted once the cap bound")
	}
	for _, state := range []string{"state-A", "state-C", "state-D"} {
		if _, taken := store.Take(state); taken != TakeOK {
			t.Errorf("%s should still be live, got %v", state, taken)
		}
	}
	if _, taken := store.Take("state-X"); taken != TakeOK {
		t.Error("another user's session must never be evicted")
	}
}
