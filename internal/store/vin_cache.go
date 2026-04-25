package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// vinIDLookup is the consumer-site interface for fetching the immutable
// (vehicleID, userID) pair for a VIN. VehicleRepo satisfies this via
// GetIDsByVIN, which uses a two-column SELECT instead of pulling the
// full vehicle row.
type vinIDLookup interface {
	GetIDsByVIN(ctx context.Context, vin string) (id, userID string, err error)
}

// vinIDs holds the immutable identifier pair cached per VIN.
type vinIDs struct {
	vehicleID string
	userID    string
}

// vinMissSentinel is the cached value for VINs that returned
// ErrVehicleNotFound, so subsequent lookups for the same VIN don't
// re-hit the database. Both fields are empty.
var vinMissSentinel = vinIDs{}

// VINCache maps VIN strings to (vehicleID, userID) pairs, backed by a
// sync.Map for lock-free concurrent reads. Cache entries never expire
// because both identifiers are immutable for the lifetime of a vehicle
// row in the Prisma-owned "Vehicle" table.
//
// Use ResolveID when only the vehicle ID is needed and ResolveOwner when
// only the owning userID is needed; the same cache entry serves both.
type VINCache struct {
	lookup vinIDLookup
	cache  sync.Map // VIN → vinIDs (zero value = cached miss)
	logger *slog.Logger
}

// NewVINCache creates a VINCache that resolves cache misses against the
// given vinIDLookup.
func NewVINCache(lookup vinIDLookup, logger *slog.Logger) *VINCache {
	return &VINCache{
		lookup: lookup,
		logger: logger,
	}
}

// ResolveID returns the vehicleID for the given VIN, using the cache to
// avoid repeated database lookups. Returns ErrVehicleNotFound when the
// VIN has no matching vehicle (this result is cached).
func (c *VINCache) ResolveID(ctx context.Context, vin string) (string, error) {
	ids, err := c.resolve(ctx, vin)
	if err != nil {
		return "", err
	}
	return ids.vehicleID, nil
}

// ResolveOwner returns the owning userID for the given VIN, using the
// cache to avoid repeated database lookups. Returns ErrVehicleNotFound
// when the VIN has no matching vehicle (this result is cached).
func (c *VINCache) ResolveOwner(ctx context.Context, vin string) (string, error) {
	ids, err := c.resolve(ctx, vin)
	if err != nil {
		return "", err
	}
	return ids.userID, nil
}

// resolve is the shared cache-and-fetch path used by ResolveID and
// ResolveOwner. The first call for a given VIN hits the database via
// the slim two-column query; subsequent calls return the cached pair.
func (c *VINCache) resolve(ctx context.Context, vin string) (vinIDs, error) {
	if v, ok := c.cache.Load(vin); ok {
		ids := v.(vinIDs) //nolint:forcetypeassert // cache only stores vinIDs
		if ids == vinMissSentinel {
			return vinIDs{}, fmt.Errorf("VINCache.resolve(%s): %w", redactVIN(vin), ErrVehicleNotFound)
		}
		return ids, nil
	}

	id, userID, err := c.lookup.GetIDsByVIN(ctx, vin)
	if errors.Is(err, ErrVehicleNotFound) {
		c.cache.Store(vin, vinMissSentinel)
		c.logger.Warn("vehicle not found for VIN, caching miss",
			slog.String("vin", redactVIN(vin)),
		)
		return vinIDs{}, fmt.Errorf("VINCache.resolve(%s): %w", redactVIN(vin), ErrVehicleNotFound)
	}
	if err != nil {
		// Don't cache transient errors — let the next call retry.
		return vinIDs{}, fmt.Errorf("VINCache.resolve(%s): %w", redactVIN(vin), err)
	}

	ids := vinIDs{vehicleID: id, userID: userID}
	c.cache.Store(vin, ids)
	c.logger.Debug("cached VIN → (vehicleID, userID) mapping",
		slog.String("vin", redactVIN(vin)),
		slog.String("vehicle_id", id),
		slog.String("user_id", userID),
	)
	return ids, nil
}
