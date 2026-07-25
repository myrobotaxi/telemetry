package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ridecomplete"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Drive-end ride completion wiring (MYR-265). Composes the drive.ended
// subscriber that closes an autonomous ride when its car parks at the dropoff
// (enroute→completed). Lives at cmd/ so the completer depends only on small
// consumer-site interfaces, wired here to the VIN cache + ride repo + bus —
// the same boundary the nav dispatcher follows.

// setupRideCompletion subscribes the drive-end ride completer on the bus. The
// subscription lives until bus.Close on shutdown.
func setupRideCompletion(
	bus events.Bus,
	vinCache *store.VINCache,
	rideRepo *store.RideRequestRepo,
	logger *slog.Logger,
) error {
	completer := ridecomplete.New(
		&rideCompletionVehicleResolverAdapter{cache: vinCache},
		&rideCompletionStoreAdapter{repo: rideRepo},
		bus,
		logger,
	)
	if _, err := completer.Subscribe(bus); err != nil {
		return fmt.Errorf("subscribe ride completer: %w", err)
	}
	logger.Info("drive-end ride completion subscriber enabled")
	return nil
}

// rideCompletionVehicleResolverAdapter resolves a VIN to its vehicle cuid over
// the shared VIN cache (ridecomplete.VehicleResolver).
type rideCompletionVehicleResolverAdapter struct {
	cache *store.VINCache
}

func (a *rideCompletionVehicleResolverAdapter) ResolveID(ctx context.Context, vin string) (string, error) {
	return a.cache.ResolveID(ctx, vin)
}

// rideCompletionStoreAdapter adapts the ride-request repo to
// ridecomplete.Store, projecting the completed store rows onto the completer's
// minimal CompletedRide shape (ridecomplete.Store).
type rideCompletionStoreAdapter struct {
	repo *store.RideRequestRepo
}

func (a *rideCompletionStoreAdapter) CompleteEnrouteByVehicle(ctx context.Context, vehicleID string) ([]ridecomplete.CompletedRide, error) {
	recs, err := a.repo.CompleteEnrouteByVehicle(ctx, vehicleID)
	if err != nil {
		return nil, err
	}
	out := make([]ridecomplete.CompletedRide, 0, len(recs))
	for i := range recs {
		out = append(out, ridecomplete.CompletedRide{
			RideRequestID: recs[i].ID,
			VehicleID:     recs[i].VehicleID,
			RiderID:       recs[i].RiderID,
			OwnerID:       recs[i].OwnerID,
			Status:        string(recs[i].Status),
			RequesterName: optionalString(recs[i].RequesterName),
			UpdatedAt:     recs[i].UpdatedAt,
		})
	}
	return out, nil
}
