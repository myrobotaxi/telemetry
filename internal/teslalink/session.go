// Package teslalink implements the user-facing, in-app Tesla Fleet OAuth link
// endpoints (MYR-246): POST /api/tesla/link/start and GET
// /api/tesla/link/callback. Together they let a signed-in owner link their
// Tesla account from the iOS app (via ASWebAuthenticationSession) instead of
// the localhost `ops auth link` developer flow.
//
// The OAuth primitives (PKCE, authorize URL, code->token exchange) are shared
// with the ops CLI via internal/teslaauth. This package owns the server-side
// concerns the CLI does not have: a short-lived single-use PKCE+state session
// store keyed to the calling user, bearer-token authentication on /start, and
// the app deep-link handoff on /callback.
package teslalink

import (
	"sync"
	"time"
)

// Session is a single in-flight OAuth link attempt. It binds the OAuth `state`
// (CSRF token, also the store key) to the PKCE verifier and the authenticated
// user id captured at /start time. The callback — which carries no bearer
// token — recovers the user id by looking the session up by state, so a
// forged callback cannot link tokens onto an arbitrary account.
//
// Sessions are single-use (removed on the first Take) and short-lived
// (ExpiresAt). The verifier and user id are never logged.
type Session struct {
	State        string
	PKCEVerifier string
	UserID       string
	ExpiresAt    time.Time
}

// SessionStore is an in-memory, TTL-bounded, single-use store of in-flight
// link sessions. It is safe for concurrent use. A process restart drops all
// in-flight sessions, which merely forces the user to retry the link — no
// tokens are lost because nothing is persisted until the callback succeeds.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	ttl      time.Duration
	now      func() time.Time
}

// NewSessionStore builds a store whose sessions live for ttl. The clock is
// injectable for tests via WithClock.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]Session),
		ttl:      ttl,
		now:      time.Now,
	}
}

// WithClock overrides the store's clock (tests only). Returns the store for
// chaining.
func (s *SessionStore) WithClock(now func() time.Time) *SessionStore {
	s.now = now
	return s
}

// TTL returns the configured session lifetime.
func (s *SessionStore) TTL() time.Duration { return s.ttl }

// Put stores sess keyed by its State, stamping ExpiresAt = now + ttl. It also
// opportunistically reaps expired sessions so an abandoned-flow leak cannot
// grow the map unbounded.
func (s *SessionStore) Put(sess Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess.ExpiresAt = now.Add(s.ttl)
	s.reapLocked(now)
	s.sessions[sess.State] = sess
}

// Take removes and returns the session for state. The second return is false
// when no session exists for state OR the session has expired (an expired
// session is deleted and treated as absent). Removal makes every session
// single-use: a replayed callback finds nothing.
func (s *SessionStore) Take(state string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[state]
	if !ok {
		return Session{}, false
	}
	delete(s.sessions, state)
	if !s.now().Before(sess.ExpiresAt) {
		return Session{}, false
	}
	return sess, true
}

// Len reports the number of stored sessions (tests / diagnostics).
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// reapLocked deletes every expired session. Caller must hold s.mu.
func (s *SessionStore) reapLocked(now time.Time) {
	for k, v := range s.sessions {
		if !now.Before(v.ExpiresAt) {
			delete(s.sessions, k)
		}
	}
}
