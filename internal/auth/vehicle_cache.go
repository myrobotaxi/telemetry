package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// vehicleCacheTTL is how long cached vehicle IDs remain valid before a
// fresh database lookup is required.
const vehicleCacheTTL = 5 * time.Minute

// vehicleQuerier queries the database for vehicle IDs belonging to a user.
// Defined at the consumer site; satisfied by a closure over pgxpool.Pool.
type vehicleQuerier interface {
	GetUserVehicleIDs(ctx context.Context, userID string) ([]string, error)
}

// cacheEntry holds a list of vehicle IDs and the time they were fetched.
type cacheEntry struct {
	vehicleIDs []string
	fetchedAt  time.Time
}

// vehicleCache maps user IDs to their vehicle IDs with a configurable TTL.
// Expired entries are lazily evicted on the next lookup.
type vehicleCache struct {
	querier vehicleQuerier
	entries sync.Map // userID -> *cacheEntry
	ttl     time.Duration
	now     func() time.Time // injectable for testing
	group   singleflight.Group
}

// newVehicleCache creates a vehicle cache that resolves misses using the
// given querier. Entries expire after the configured TTL.
func newVehicleCache(querier vehicleQuerier, ttl time.Duration) *vehicleCache { //nolint:unparam // ttl varies in tests
	return &vehicleCache{
		querier: querier,
		ttl:     ttl,
		now:     time.Now,
	}
}

// lookup returns the vehicle IDs for the given user. It serves from cache
// when a fresh entry exists, otherwise queries the database.
func (c *vehicleCache) lookup(ctx context.Context, userID string) ([]string, error) {
	if entry, ok := c.loadValid(userID); ok {
		return entry.vehicleIDs, nil
	}

	// Coalesce concurrent lookups for the same user via singleflight.
	val, err, _ := c.group.Do(userID, func() (any, error) {
		// Double-check cache after acquiring the singleflight slot.
		if entry, ok := c.loadValid(userID); ok {
			return entry.vehicleIDs, nil
		}
		ids, err := c.querier.GetUserVehicleIDs(ctx, userID)
		if err != nil {
			return nil, fmt.Errorf("vehicleCache.lookup(user=%s): %w", userID, err)
		}
		c.entries.Store(userID, &cacheEntry{
			vehicleIDs: ids,
			fetchedAt:  c.now(),
		})
		return ids, nil
	})
	if err != nil {
		return nil, fmt.Errorf("vehicleCache.lookup: %w", err)
	}
	return val.([]string), nil
}

// loadValid returns the cache entry if it exists and has not expired.
func (c *vehicleCache) loadValid(userID string) (*cacheEntry, bool) {
	val, ok := c.entries.Load(userID)
	if !ok {
		return nil, false
	}

	entry := val.(*cacheEntry) //nolint:forcetypeassert // cache only stores *cacheEntry
	if c.now().Sub(entry.fetchedAt) > c.ttl {
		c.entries.Delete(userID)
		return nil, false
	}

	return entry, true
}
