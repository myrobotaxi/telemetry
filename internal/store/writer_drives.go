package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
)

const driveOpTimeout = 30 * time.Second

// handleDriveStarted returns an event handler that creates a drive record
// when a drive begins. If a geocoder is configured, it reverse geocodes
// the start location into a human-readable address.
func (w *Writer) handleDriveStarted() events.Handler {
	return func(event events.Event) {
		evt, ok := event.Payload.(events.DriveStartedEvent)
		if !ok {
			w.logger.Error("unexpected payload type for drive.started",
				slog.String("event_id", event.ID),
			)
			return
		}

		opCtx, cancel := context.WithTimeout(context.Background(), driveOpTimeout)
		defer cancel()

		vehicleID, err := w.vinCache.ResolveID(opCtx, evt.VIN)
		if err != nil {
			w.logger.Warn("cannot persist drive start: VIN lookup failed",
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
			return
		}

		record := mapDriveStarted(evt, vehicleID)

		// Reverse geocode start location if coordinates are non-zero.
		if evt.Location.Latitude != 0 || evt.Location.Longitude != 0 {
			geo, geoErr := w.geocoder.ReverseGeocode(opCtx, evt.Location.Latitude, evt.Location.Longitude)
			switch {
			case geoErr == nil:
				record.StartLocation = geo.PlaceName
				record.StartAddress = geo.Address
			case !errors.Is(geoErr, geocode.ErrNoResult):
				w.logger.Warn("reverse geocode failed for drive start",
					slog.String("drive_id", evt.DriveID),
					slog.String("error", geoErr.Error()),
				)
			}
		}

		if err := w.drives.Create(opCtx, record); err != nil {
			w.logger.Warn("failed to create drive record",
				slog.String("drive_id", evt.DriveID),
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// handleDriveUpdated returns an event handler that buffers route points
// during an active drive. Points are accumulated in the route buffer and
// periodically flushed to the database so the frontend can display the
// route before the drive ends.
func (w *Writer) handleDriveUpdated() events.Handler {
	return func(event events.Event) {
		evt, ok := event.Payload.(events.DriveUpdatedEvent)
		if !ok {
			w.logger.Error("unexpected payload type for drive.updated",
				slog.String("event_id", event.ID),
			)
			return
		}

		pt := mapSingleRoutePoint(evt.RoutePoint)
		shouldFlush := w.routeBuf.add(evt.DriveID, pt)

		if shouldFlush {
			opCtx, cancel := context.WithTimeout(context.Background(), driveOpTimeout)
			defer cancel()
			w.routeBuf.flushDrive(opCtx, evt.DriveID)
		}
	}
}

// handleDriveEnded returns an event handler that completes a drive record,
// flushes any remaining buffered route points, and sets the vehicle status
// to parked. If a geocoder is configured, it reverse geocodes the end
// location. Route points from DriveStats are not appended here because
// they are persisted incrementally via handleDriveUpdated.
func (w *Writer) handleDriveEnded() events.Handler {
	return func(event events.Event) {
		evt, ok := event.Payload.(events.DriveEndedEvent)
		if !ok {
			w.logger.Error("unexpected payload type for drive.ended",
				slog.String("event_id", event.ID),
			)
			return
		}

		opCtx, cancel := context.WithTimeout(context.Background(), driveOpTimeout)
		defer cancel()

		completion := mapDriveCompletion(evt)

		// Reverse geocode end location if coordinates are non-zero.
		endLoc := evt.Stats.EndLocation
		if endLoc.Latitude != 0 || endLoc.Longitude != 0 {
			geo, geoErr := w.geocoder.ReverseGeocode(opCtx, endLoc.Latitude, endLoc.Longitude)
			switch {
			case geoErr == nil:
				completion.EndLocation = geo.PlaceName
				completion.EndAddress = geo.Address
			case !errors.Is(geoErr, geocode.ErrNoResult):
				w.logger.Warn("reverse geocode failed for drive end",
					slog.String("drive_id", evt.DriveID),
					slog.String("error", geoErr.Error()),
				)
			}
		}

		if err := w.drives.Complete(opCtx, evt.DriveID, completion); err != nil {
			w.logger.Warn("failed to complete drive record",
				slog.String("drive_id", evt.DriveID),
				slog.String("error", err.Error()),
			)
		}

		// Flush any remaining buffered route points for this drive.
		w.routeBuf.flushDrive(opCtx, evt.DriveID)

		if err := w.vehicles.UpdateStatus(opCtx, evt.VIN, VehicleStatusParked); err != nil {
			w.logger.Warn("failed to set vehicle status to parked",
				slog.String("vin", redactVIN(evt.VIN)),
				slog.String("error", err.Error()),
			)
		}
	}
}
