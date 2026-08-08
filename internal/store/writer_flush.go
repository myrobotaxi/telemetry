package store

// Split out of writer.go to keep that file under the 300-line cap (CLAUDE.md
// "File Rules"). This file owns the telemetry-event ingress (handleTelemetry)
// plus the periodic and on-demand flush paths (flushLoop, flush). The flush
// loop is the hot path for vehicle-row writes; keeping it adjacent to its
// per-flush helpers (applyDestinationAddress, scheduleLocationGeocode) makes
// the per-tick budget legible at a glance.

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// handleTelemetry is the event handler for VehicleTelemetryEvent. It
// extracts fields, maps them to a VehicleUpdate, and coalesces into
// the pending map. If the batch size is reached, it triggers a flush.
func (w *Writer) handleTelemetry(event events.Event) {
	telEvt, ok := event.Payload.(events.VehicleTelemetryEvent)
	if !ok {
		w.logger.Error("unexpected payload type for telemetry event",
			slog.String("event_id", event.ID),
		)
		return
	}

	update := mapTelemetryToUpdate(telEvt.Fields)
	if update == nil {
		return
	}
	update.LastUpdated = telEvt.CreatedAt
	// MYR-454: carry provenance through to the status fold, which must not
	// act on a REST backfill frame — see VehicleUpdate.Streamed.
	update.Streamed = telEvt.Streamed()
	// MYR-409: date the NAV READING, not the row. Only a frame that actually
	// carried a navigation-group member gets a stamp; a motion-, charge- or
	// climate-only frame leaves it zero so nav freshness ages honestly while
	// the row stays current. Same instant as LastUpdated when it is set at
	// all — the distinction is WHETHER, never WHICH clock.
	if update.carriedNavReading() {
		update.NavReadingAt = telEvt.CreatedAt
	}

	shouldFlush := w.coalesce(telEvt.VIN, update)
	if shouldFlush {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w.flush(ctx)
	}
}

// flushLoop runs a ticker that calls flush at each interval until the
// context is cancelled.
func (w *Writer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

// flush drains the pending map and writes each VIN's coalesced update
// to the database. Errors are logged but do not halt the writer.
func (w *Writer) flush(ctx context.Context) {
	w.pendingMu.Lock()
	if len(w.pending) == 0 {
		w.pendingMu.Unlock()
		return
	}
	batch := w.pending
	w.pending = make(map[string]*VehicleUpdate)
	w.count = 0
	w.pendingMu.Unlock()

	w.logger.Debug("flushing telemetry batch",
		slog.Int("vehicles", len(batch)),
	)

	for vin, update := range batch {
		w.applyDestinationAddress(ctx, vin, update)
		// MYR-269: persist the owner-control side-table fields first, so a
		// non-streaming car's lock/trunk/climate/charge-port state lands durably
		// even if the Vehicle-table write below is a no-op or fails. This is the
		// write half of the fix — the snapshot read (GetByID LEFT JOIN) returns
		// these with no live socket.
		w.persistSideTable(ctx, vin, update)
		if err := w.vehicles.UpdateTelemetry(ctx, vin, *update); err != nil {
			w.logger.Warn("failed to write telemetry update",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
			continue
		}
		// MYR-144: schedule an async reverse-geocode of the current
		// location so Vehicle.locationName / Vehicle.locationAddress
		// stay fresh without blocking the flush loop. The scheduler
		// applies a per-VIN time + distance debounce, so a stable
		// parked vehicle does not burn the Mapbox quota.
		w.scheduleLocationGeocode(vin, update)
	}
}

// persistSideTable writes everything this flush owes the Go-owned
// go_vehicle_control_state row: the MYR-269 owner-control read-backs and the
// MYR-409 nav-reading stamp.
//
// The two share this function because they share the only expensive part — the
// side table is keyed by vehicle cuid, not VIN, so each needs a VIN-cache
// resolve, and doing it once keeps a nav frame from paying for two. They are
// otherwise independent: a frame can carry controls with no navigation (a lock
// toggle), navigation with no controls (the common streaming case), or both.
//
// A frame owing neither returns before the resolve, which is the majority of
// flushes.
func (w *Writer) persistSideTable(ctx context.Context, vin string, update *VehicleUpdate) {
	hasControls := update.ControlState != nil && update.ControlState.HasAny()
	hasNavStamp := !update.NavReadingAt.IsZero()
	if !hasControls && !hasNavStamp {
		return
	}
	vehicleID, err := w.vinCache.ResolveID(ctx, vin)
	if err != nil {
		w.logger.Warn("failed to resolve vehicle id for side-table persist",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}
	if hasControls {
		if err := w.vehicles.UpsertControlState(ctx, vehicleID, *update.ControlState); err != nil {
			w.logger.Warn("failed to persist owner control state",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
	}
	if !hasNavStamp {
		return
	}
	// Best-effort, like its neighbour, and the failure direction is the safe
	// one: a stamp that does not land leaves the column at its previous value,
	// so nav freshness reads OLDER than the truth and the gates it feeds decline
	// rather than fire. A burnt alert rung is durable; a declined one is not.
	if err := w.vehicles.SetNavReadingAt(ctx, vehicleID, update.NavReadingAt); err != nil {
		w.logger.Warn("failed to persist nav reading stamp",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
	}
}
