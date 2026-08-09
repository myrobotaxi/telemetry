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

// maxSessionsPerUser caps how many link attempts one user may have in flight.
//
// IT USED TO BE ONE, AND THAT WAS A DEAD END IN DISGUISE (MYR-517). A fresh
// /start evicted the user's previous session outright, so any client that
// called /start twice — a re-render, a retry, a re-presented
// ASWebAuthenticationSession — invalidated the state of the browser flow the
// owner was actually standing in. Tesla then redirected back with a state
// nobody was holding and the owner was told their link had expired, with no
// hint that the cause was a duplicate request rather than the clock.
//
// Three is the smallest number that survives that class of mistake while
// keeping the ceiling on map growth the cap exists for: the store is still
// bounded by 3 × in-flight users, sessions are still single-use, and the oldest
// is still the one that goes when the cap binds. It buys tolerance for a
// duplicate /start, not an unbounded queue of abandoned flows.
const maxSessionsPerUser = 3

// TakeResult classifies the outcome of a Take, so the callback can tell a state
// whose TTL RAN OUT from one that was never valid.
//
// The two are the same HTTP outcome and deliberately the same client-facing
// `reason` (§7.11.2 is unchanged), but they are not the same event: an expired
// state means an owner did everything right and simply took longer than we
// allowed, which is a clean re-entry into /start; an unknown state means a
// replay, a forgery, or a process restart, which is not. Collapsing them is how
// MYR-517's expiry became indistinguishable from a bug for everyone looking at
// it — including us.
type TakeResult int

const (
	// TakeOK — a live session was found and consumed.
	TakeOK TakeResult = iota
	// TakeUnknown — no session for this state at all: never issued, already
	// consumed, or dropped when the process restarted.
	TakeUnknown
	// TakeExpired — the session existed and its TTL had elapsed.
	TakeExpired
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

// Put stores sess keyed by its State, stamping ExpiresAt = now + ttl. It caps
// live sessions to at most maxSessionsPerUser in-flight links per user, evicting
// that user's OLDEST when the cap binds, and opportunistically reaps expired
// sessions so an abandoned-flow leak cannot grow the map unbounded.
func (s *SessionStore) Put(sess Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess.ExpiresAt = now.Add(s.ttl)
	s.reapLocked(now)
	s.enforceUserCapLocked(sess.UserID)
	s.sessions[sess.State] = sess
}

// Take removes and returns the session for state, classifying the outcome.
// Removal makes every session single-use: a replayed callback finds nothing.
//
// An expired session is deleted and reported as TakeExpired rather than merely
// absent, which is the whole point — the caller needs to distinguish "you were
// too slow" from "this state was never ours".
func (s *SessionStore) Take(state string) (Session, TakeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[state]
	if !ok {
		return Session{}, TakeUnknown
	}
	delete(s.sessions, state)
	if !s.now().Before(sess.ExpiresAt) {
		return Session{}, TakeExpired
	}
	return sess, TakeOK
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

// enforceUserCapLocked makes room for one more session belonging to userID,
// evicting that user's oldest entries until fewer than maxSessionsPerUser
// remain. Caller must hold s.mu. userID is never empty in practice (it is the
// authenticated caller), but an empty id is left untouched rather than evicting
// all anonymous entries.
//
// Oldest is read off ExpiresAt, which is a faithful proxy for insertion order
// because the TTL is a constant of the store.
func (s *SessionStore) enforceUserCapLocked(userID string) {
	if userID == "" {
		return
	}
	for {
		count := 0
		oldestKey := ""
		var oldestAt time.Time
		for k, v := range s.sessions {
			if v.UserID != userID {
				continue
			}
			count++
			if oldestKey == "" || v.ExpiresAt.Before(oldestAt) {
				oldestKey, oldestAt = k, v.ExpiresAt
			}
		}
		if count < maxSessionsPerUser {
			return
		}
		delete(s.sessions, oldestKey)
	}
}
