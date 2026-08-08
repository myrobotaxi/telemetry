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
		w.persistControlState(ctx, vin, update)
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

// persistControlState upserts the MYR-269 owner-control read-backs for one VIN
// into the Go-owned side table. It resolves the vehicle cuid via the VIN cache
// (the side table is keyed by cuid, not VIN). A resolve or upsert failure is
// logged and swallowed — control-state persistence is best-effort and must not
// stall the flush loop or drop the Vehicle-table write. A frame with no control
// fields is a cheap no-op.
func (w *Writer) persistControlState(ctx context.Context, vin string, update *VehicleUpdate) {
	if update.ControlState == nil || !update.ControlState.HasAny() {
		return
	}
	vehicleID, err := w.vinCache.ResolveID(ctx, vin)
	if err != nil {
		w.logger.Warn("failed to resolve vehicle id for control-state persist",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}
	if err := w.vehicles.UpsertControlState(ctx, vehicleID, *update.ControlState); err != nil {
		w.logger.Warn("failed to persist owner control state",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
	}
}
