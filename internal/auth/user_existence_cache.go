package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrUserNotFound is returned when ValidateToken's wrapped existence
// check confirms (via the database) that no User row exists for the
// JWT's `sub`. The error wraps ErrInvalidToken so existing callers
// that switch on ErrInvalidToken keep working without changes — the
// FR-10.1 cleanup contract requires the JWT to be rejected, and the
// existing handler maps that to `auth_failed`.
var ErrUserNotFound = errors.New("user not found")

// userExistenceTTL is the lifetime of a positive cache entry.
// Per the MYR-73 issue spec, the user-existence check must be cheap
// enough that it can run on every WS handshake without a per-frame DB
// hit. The 1s TTL is the longest acceptable staleness window: a
// deleted user's stale token may pass ValidateToken for at most 1s
// after the deletion commits, then the next call refetches.
const userExistenceTTL = time.Second

// userExistenceChecker is the consumer-site interface used by
// userExistenceCache to fetch authoritative existence answers.
// Satisfied by pgUserExistenceQuerier (production) or a stub (tests).
type userExistenceChecker interface {
	UserExists(ctx context.Context, userID string) (bool, error)
}

// userExistenceEntry stores a cached existence answer plus its fetch
// timestamp.
type userExistenceEntry struct {
	exists    bool
	fetchedAt time.Time
}

// userExistenceCache maps userID -> existence answer with a TTL.
// Lookups are singleflight-coalesced so 100 concurrent WS handshakes
// for the same userID fan out to a single DB query.
type userExistenceCache struct {
	checker userExistenceChecker
	entries sync.Map // userID -> *userExistenceEntry
	ttl     time.Duration
	now     func() time.Time
	group   singleflight.Group
}

// newUserExistenceCache constructs a cache backed by checker.
func newUserExistenceCache(checker userExistenceChecker, ttl time.Duration) *userExistenceCache { //nolint:unparam // ttl varies in tests
	if ttl <= 0 {
		ttl = userExistenceTTL
	}
	return &userExistenceCache{
		checker: checker,
		ttl:     ttl,
		now:     time.Now,
	}
}

// Exists returns whether userID has a row in the User table. Cached
// answers (both positive and negative) are reused for up to ttl. A
// transient DB error is propagated to the caller — the JWT path
// treats "lookup failed" as fail-closed (rejected) by wrapping it as
// ErrUserNotFound; we do not silently allow auth on a database
// outage.
func (c *userExistenceCache) Exists(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}

	if entry, ok := c.loadValid(userID); ok {
		return entry.exists, nil
	}

	val, err, _ := c.group.Do(userID, func() (any, error) {
		// Double-check after acquiring the singleflight slot.
		if entry, ok := c.loadValid(userID); ok {
			return entry.exists, nil
		}
		exists, err := c.checker.UserExists(ctx, userID)
		if err != nil {
			return false, fmt.Errorf("userExistenceCache.Exists(user=%s): %w", userID, err)
		}
		c.entries.Store(userID, &userExistenceEntry{
			exists:    exists,
			fetchedAt: c.now(),
		})
		return exists, nil
	})
	if err != nil {
		return false, err
	}
	return val.(bool), nil
}

// Invalidate removes the cached entry for userID. After Invalidate
// returns, the next Exists call refetches from the database. Used by
// the data-lifecycle.md §3.5 cleanup path so a deleted user's
// existence answer flips immediately instead of after the 1s TTL.
func (c *userExistenceCache) Invalidate(userID string) {
	if userID == "" {
		return
	}
	c.entries.Delete(userID)
}

// loadValid returns the cache entry if it exists and has not expired.
func (c *userExistenceCache) loadValid(userID string) (*userExistenceEntry, bool) {
	val, ok := c.entries.Load(userID)
	if !ok {
		return nil, false
	}
	entry := val.(*userExistenceEntry) //nolint:forcetypeassert // cache only stores *userExistenceEntry
	if c.now().Sub(entry.fetchedAt) > c.ttl {
		c.entries.Delete(userID)
		return nil, false
	}
	return entry, true
}
