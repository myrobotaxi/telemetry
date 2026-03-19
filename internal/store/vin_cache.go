package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// vinLookup is the consumer-site interface for looking up a vehicle by VIN.
// VehicleRepo satisfies this interface.
type vinLookup interface {
	GetByVIN(ctx context.Context, vin string) (Vehicle, error)
}

// sentinel value stored in the cache when a VIN is not found in the DB,
// preventing repeated lookups for unknown vehicles.
const vinMiss = ""

// vinCache maps VIN strings to vehicle IDs (cuid), backed by a sync.Map
// for lock-free concurrent reads. Cache entries never expire because
// vehicle IDs are immutable.
type vinCache struct {
	lookup vinLookup
	cache  sync.Map // VIN → string (vehicleID or vinMiss)
	logger *slog.Logger
}

// newVINCache creates a vinCache that resolves cache misses against the
// given vinLookup.
func newVINCache(lookup vinLookup, logger *slog.Logger) *vinCache {
	return &vinCache{
		lookup: lookup,
		logger: logger,
	}
}

// resolve returns the vehicleID for the given VIN. It checks the cache
// first, then falls back to the database. Returns ErrVehicleNotFound if
// the VIN has no matching vehicle in the DB (this result is cached).
func (c *vinCache) resolve(ctx context.Context, vin string) (string, error) {
	if v, ok := c.cache.Load(vin); ok {
		id := v.(string) //nolint:forcetypeassert // cache only stores strings
		if id == vinMiss {
			return "", fmt.Errorf("vinCache.resolve(%s): %w", redactVIN(vin), ErrVehicleNotFound)
		}
		return id, nil
	}

	vehicle, err := c.lookup.GetByVIN(ctx, vin)
	if errors.Is(err, ErrVehicleNotFound) {
		c.cache.Store(vin, vinMiss)
		c.logger.Warn("vehicle not found for VIN, caching miss",
			slog.String("vin", redactVIN(vin)),
		)
		return "", fmt.Errorf("vinCache.resolve(%s): %w", redactVIN(vin), ErrVehicleNotFound)
	}
	if err != nil {
		// Don't cache transient errors — let the next call retry.
		return "", fmt.Errorf("vinCache.resolve(%s): %w", redactVIN(vin), err)
	}

	c.cache.Store(vin, vehicle.ID)
	c.logger.Debug("cached VIN → vehicleID mapping",
		slog.String("vin", redactVIN(vin)),
		slog.String("vehicle_id", vehicle.ID),
	)
	return vehicle.ID, nil
}
