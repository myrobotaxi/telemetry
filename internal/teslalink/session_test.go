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
		advance time.Duration
		wantOK  bool
	}{
		{name: "valid within ttl", put: true, takeState: "s1", advance: time.Minute, wantOK: true},
		{name: "expired past ttl", put: true, takeState: "s1", advance: 11 * time.Minute, wantOK: false},
		{name: "missing state", put: false, takeState: "nope", advance: 0, wantOK: false},
		{name: "wrong state", put: true, takeState: "other", advance: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := base
			store := NewSessionStore(10 * time.Minute).WithClock(func() time.Time { return now })
			if tt.put {
				store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})
			}
			now = base.Add(tt.advance)

			sess, ok := store.Take(tt.takeState)
			if ok != tt.wantOK {
				t.Fatalf("Take ok: got %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if sess.UserID != "u1" || sess.PKCEVerifier != "v1" {
					t.Errorf("session fields: got %+v", sess)
				}
			}
		})
	}
}

func TestSessionStore_SingleUse(t *testing.T) {
	store := NewSessionStore(10 * time.Minute)
	store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})

	if _, ok := store.Take("s1"); !ok {
		t.Fatal("first Take should succeed")
	}
	if _, ok := store.Take("s1"); ok {
		t.Error("second Take must fail — sessions are single-use")
	}
	if store.Len() != 0 {
		t.Errorf("store should be empty after Take, got %d", store.Len())
	}
}

func TestSessionStore_PutReapsExpired(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := NewSessionStore(10 * time.Minute).WithClock(func() time.Time { return now })

	store.Put(Session{State: "old", UserID: "u1"})
	now = now.Add(11 * time.Minute) // "old" is now expired
	store.Put(Session{State: "fresh", UserID: "u2"})

	// The expired "old" session must have been reaped by the second Put.
	if store.Len() != 1 {
		t.Fatalf("expected 1 live session after reap, got %d", store.Len())
	}
	if _, ok := store.Take("old"); ok {
		t.Error("expired session should have been reaped")
	}
	if _, ok := store.Take("fresh"); !ok {
		t.Error("fresh session should survive")
	}
}
