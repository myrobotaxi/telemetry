package main

import (
	"context"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// vehicleSnapshotAdapter adapts store.VehicleRepo.GetByID to the
// telemetry.VehicleSnapshotReader interface used by the
// GET /api/vehicles/{vehicleId}/snapshot and
// GET /api/vehicles/{vehicleId}/drives handlers (MYR-133). The
// adapter exists so the handlers stay decoupled from internal/store.
//
// GetByID returns the wide read with GPS dual-resolution and
// nav-route blob decryption already applied per
// vehicle_repo_scan.go — exactly what /snapshot needs. The drives
// handler reuses this for the ownership check (it needs the userID
// off the row; GetByID is the cheapest source of truth for an
// arbitrary vehicleId cuid).
type vehicleSnapshotAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleSnapshotAdapter) GetByID(ctx context.Context, vehicleID string) (telemetry.VehicleSnapshotRow, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return telemetry.VehicleSnapshotRow{}, fmt.Errorf("get vehicle by id: %w", err)
	}
	return snapshotRowFromVehicle(v), nil
}

// snapshotRowFromVehicle is the store.Vehicle → telemetry.VehicleSnapshotRow
// projection — the ONLY boundary the DB-backed /snapshot read crosses on its
// way to the wire.
//
// It is a free function rather than adapter-method body so it is unit-testable
// without a database: MYR-320 shipped trim_label / fsd_version through the
// migration, the writer, the SELECT, the scan, the response struct AND the mask
// table, and they still came back null in production because this one struct
// literal never learned them. Every other layer had a test; this projection did
// not. See adapters_vehicle_snapshot_test.go, which asserts EVERY field of the
// destination row is populated from a fully-populated source vehicle so the
// next column added to the side table cannot repeat the same silent drop.
func snapshotRowFromVehicle(v store.Vehicle) telemetry.VehicleSnapshotRow {
	return telemetry.VehicleSnapshotRow{
		ID:                   v.ID,
		UserID:               v.UserID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		LicensePlate:         v.LicensePlate,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		ChargeState:          v.ChargeState,
		TimeToFull:           v.TimeToFull,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesSinceReset,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated,
		Locked:               v.IsLocked,
		FrunkOpen:            v.FrunkOpen,
		TrunkOpen:            v.TrunkOpen,
		IsClimateOn:          v.IsClimateOn,
		ChargePortDoorOpen:   v.ChargePortOpen,
		DriverTempSetting:    v.DriverTempSetting,
		PassengerTempSetting: v.PassengerTempSetting,
		FanSpeed:             v.FanSpeed,
		SeatHeaterLeft:       v.SeatHeaterLeft,
		SeatHeaterRight:      v.SeatHeaterRight,
		SeatHeaterRearLeft:   v.SeatHeaterRearLeft,
		SeatHeaterRearCenter: v.SeatHeaterRearCenter,
		SeatHeaterRearRight:  v.SeatHeaterRearRight,
		SeatCoolerLeft:       v.SeatCoolerLeft,
		SeatCoolerRight:      v.SeatCoolerRight,
		MediaVolume:          v.MediaVolume,
		SoftwareVersion:      v.SoftwareVersion,
		Trim:                 v.Trim,
		HvacAutoMode:         v.HvacAutoMode,
		HvacAcEnabled:        v.HvacAcEnabled,
		SeatVentEnabled:      v.SeatVentEnabled,
		MediaPlaybackStatus:  v.MediaPlaybackStatus,

		// MYR-303 media now-playing + MYR-308 seat-cooling capability.
		MediaNowPlayingTitle:    v.MediaNowPlayingTitle,
		MediaNowPlayingArtist:   v.MediaNowPlayingArtist,
		MediaNowPlayingAlbum:    v.MediaNowPlayingAlbum,
		MediaNowPlayingStation:  v.MediaNowPlayingStation,
		MediaPlaybackSource:     v.MediaPlaybackSource,
		MediaNowPlayingDuration: v.MediaNowPlayingDuration,
		MediaNowPlayingElapsed:  v.MediaNowPlayingElapsed,
		MediaVolumeMax:          v.MediaVolumeMax,
		SeatCoolingCapable:      v.SeatCoolingCapable,
		ServiceETC:              v.ServiceETC,
		ServiceExpectedEndAt:    v.ServiceExpectedEndAt,

		// MYR-320 vehicle-detail read-backs. Both stand ALONGSIDE a sibling
		// already copied above and must not be confused with it: TrimLabel is
		// the display-safe label next to Trim (the raw badge code), FSDVersion
		// the FSD designation next to SoftwareVersion (the firmware build).
		TrimLabel:  v.TrimLabel,
		FSDVersion: v.FSDVersion,

		// MYR-342 owner ride-sharing switch. Copied like any other joined
		// column, but note it is the ONE field here whose zero value is a
		// meaningful (and wrong) answer: false reads as PAUSED. The source is
		// GetByID, which joins the side table and COALESCEs a missing row to
		// true, so the value is always the store's, never a Go default.
		RideShareEnabled: v.RideShareEnabled,

		// MYR-491 fleet-config setup schedule — raw, from the second side-table
		// join on this read. Mapped through the same helper the two catalog
		// adapters use so all three surfaces feed the derivation identically.
		SetupSchedule: setupScheduleRow(v.SetupSchedule),
	}
}
