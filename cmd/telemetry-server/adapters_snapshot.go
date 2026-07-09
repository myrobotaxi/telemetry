package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// wsVehicleSnapshotAdapter adapts store.VehicleRepo.GetByID to the
// ws.VehicleSnapshotReader interface (MYR-137). The hub uses this to
// unicast an initial vehicle_update snapshot to a client the moment it
// subscribes, seeding model/year/color/estimatedRange/fsdMilesSinceReset
// (and everything else persisted for the vehicle) before any live Tesla
// telemetry has arrived on the connection — closing the gap where those
// fields never appeared on the WS wire at all despite MYR-24 already
// persisting them.
//
// Deliberately separate from vehicleSnapshotAdapter (adapters.go, which
// targets telemetry.VehicleSnapshotReader for the REST endpoint) rather
// than shared: internal/ws defines its own consumer-site interface per
// this repo's Go conventions, so internal/ws does not need to import
// internal/telemetry just to describe the shape it needs.
type wsVehicleSnapshotAdapter struct {
	repo *store.VehicleRepo
}

func (a *wsVehicleSnapshotAdapter) GetVehicleSnapshot(ctx context.Context, vehicleID string) (ws.VehicleSnapshot, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return ws.VehicleSnapshot{}, fmt.Errorf("get vehicle by id: %w", err)
	}
	return ws.VehicleSnapshot{
		Fields:    vehicleToSnapshotFields(v),
		Timestamp: v.LastUpdated.UTC().Format(time.RFC3339),
	}, nil
}

// vehicleToSnapshotFields projects a store.Vehicle row into the wire
// field-name-keyed map ws.sendSnapshot partitions by atomic group. Field
// set and names mirror vehicleSnapshotResponse.toMaskMap() in
// internal/telemetry/vehicle_snapshot_types.go (the REST /snapshot
// response) minus vehicleId, which vehicle_update carries at the
// envelope level rather than inside `fields`. Kept in sync with
// docs/contracts/vehicle-state-schema.md §1.1.
func vehicleToSnapshotFields(v store.Vehicle) map[string]any {
	return map[string]any{
		"name":                  v.Name,
		"model":                 v.Model,
		"year":                  v.Year,
		"color":                 v.Color,
		"status":                string(v.Status),
		"speed":                 v.Speed,
		"heading":               v.Heading,
		"latitude":              v.Latitude,
		"longitude":             v.Longitude,
		"locationName":          v.LocationName,
		"locationAddress":       v.LocationAddress,
		"gearPosition":          derefStringOrNil(v.GearPosition),
		"chargeLevel":           v.ChargeLevel,
		"chargeState":           derefStringOrNil(v.ChargeState),
		"estimatedRange":        v.EstimatedRange,
		"timeToFull":            derefFloat64OrNil(v.TimeToFull),
		"interiorTemp":          v.InteriorTemp,
		"exteriorTemp":          v.ExteriorTemp,
		"odometerMiles":         v.OdometerMiles,
		"fsdMilesSinceReset":    v.FsdMilesSinceReset,
		"destinationName":       derefStringOrNil(v.DestinationName),
		"destinationAddress":    derefStringOrNil(v.DestinationAddress),
		"destinationLatitude":   derefFloat64OrNil(v.DestinationLatitude),
		"destinationLongitude":  derefFloat64OrNil(v.DestinationLongitude),
		"originLatitude":        derefFloat64OrNil(v.OriginLatitude),
		"originLongitude":       derefFloat64OrNil(v.OriginLongitude),
		"etaMinutes":            derefIntOrNil(v.EtaMinutes),
		"tripDistanceRemaining": derefFloat64OrNil(v.TripDistRemaining),
		"navRouteCoordinates":   navRouteCoordinatesOrNil(v.NavRouteCoordinates),
	}
}

func derefStringOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefFloat64OrNil(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefIntOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func navRouteCoordinatesOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
