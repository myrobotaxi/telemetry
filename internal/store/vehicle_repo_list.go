// Vehicle catalog-list read path. Split out of vehicle_repo.go so the
// wide-read path (full Vehicle row + GPS dual-read + nav-route blob
// resolution) stays adjacent to its scan helpers and this file stays
// focused on the slim projection.
//
// MYR-122: `GET /api/vehicles` was reading 37 columns per row including
// the six `*Enc` GPS shadows and the `navRouteCoordinatesEnc` blob,
// then decrypting all of them on every row. The handler only emits
// ~10 of those fields. This file provides the lean read companion to
// VehicleRepo.ListByUser: same WHERE clause, narrower SELECT, no
// decryption, no JSON blob.
//
// AGENTS.md "Performance invariants": list endpoints use lean
// projections; wide selects belong only in detail/edit handlers.

package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// VehicleSummary is the slim catalog shape returned by
// VehicleRepo.ListSummariesByUser. Mirrors the columns selected by
// `queryVehiclesByUserList` and the wire fields emitted by
// `internal/telemetry/vehicles_list_handler.go` `vehicleSummary`. No
// GPS, no nav, no climate — those belong in the wide detail read.
type VehicleSummary struct {
	ID             string
	UserID         string
	VIN            string
	Name           string
	Model          string
	Year           int
	Color          string
	Status         VehicleStatus
	ChargeLevel    int
	EstimatedRange int
	LastUpdated    time.Time

	// HasActiveRide is DERIVED, not a column: it is the correlated
	// EXISTS in `vehicleListHasActiveRideExpr` (MYR-233), true iff the
	// vehicle currently holds an open INSTANT ride request
	// (`scheduled_for IS NULL AND status IN
	// ('accepted','enroute','arrived')`) — exactly the predicate of the
	// `uq_go_ride_requests_active_instant_vehicle` partial unique index
	// (migration 0013, MYR-266) that the accept guard races on.
	// Scheduled rides and `requested` never set it.
	HasActiveRide bool
}

// ListSummariesByUser returns the catalog rows for every vehicle owned
// by the given user, ordered by name and VIN. Uses the lean
// `queryVehiclesByUserList` projection so the list endpoint never
// pulls encrypted-shadow GPS, the navRouteCoordinates JSON blob, or
// the other detail-only columns. Returns an empty slice (and nil
// error) when the user has no linked vehicles.
//
// Detail consumers (snapshot, edit, telemetry update) MUST use
// `ListByUser` / `GetByID` / `GetByVIN` — those still return the full
// Vehicle struct with the dual-read GPS + nav-route resolution.
//
// A duration log fires for every call. Anchored by the AGENTS.md
// performance invariant: "Wrap suspect DB calls with `time.Since`
// logging for one deploy when investigating latency — guesses are not
// data."
func (r *VehicleRepo) ListSummariesByUser(ctx context.Context, userID string) ([]VehicleSummary, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryVehiclesByUserList, userID)
	if err != nil {
		r.metrics.IncQueryError("vehicle.list_summaries_by_user")
		r.metrics.ObserveQueryDuration("vehicle.list_summaries_by_user", time.Since(start).Seconds())
		r.logListSummariesDuration(userID, time.Since(start), 0, err)
		return nil, fmt.Errorf("VehicleRepo.ListSummariesByUser(%s): %w", userID, err)
	}
	defer rows.Close()

	var out []VehicleSummary
	for rows.Next() {
		v, scanErr := scanVehicleSummaryRow(rows)
		if scanErr != nil {
			r.metrics.IncQueryError("vehicle.list_summaries_by_user")
			r.metrics.ObserveQueryDuration("vehicle.list_summaries_by_user", time.Since(start).Seconds())
			r.logListSummariesDuration(userID, time.Since(start), len(out), scanErr)
			return nil, fmt.Errorf("VehicleRepo.ListSummariesByUser(%s): %w", userID, scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("vehicle.list_summaries_by_user")
		r.metrics.ObserveQueryDuration("vehicle.list_summaries_by_user", time.Since(start).Seconds())
		r.logListSummariesDuration(userID, time.Since(start), len(out), err)
		return nil, fmt.Errorf("VehicleRepo.ListSummariesByUser(%s): rows: %w", userID, err)
	}

	r.metrics.ObserveQueryDuration("vehicle.list_summaries_by_user", time.Since(start).Seconds())
	r.logListSummariesDuration(userID, time.Since(start), len(out), nil)
	return out, nil
}

// logListSummariesDuration emits a structured duration log for one
// ListSummariesByUser call. MYR-122 ships this so the next deploy
// produces hard numbers (rows × duration × outcome) on the list path
// instead of guesses. Drop or downgrade to Debug once the perf
// investigation closes.
func (r *VehicleRepo) logListSummariesDuration(userID string, dur time.Duration, rows int, err error) {
	if r.logger == nil {
		return
	}
	attrs := []any{
		slog.String("query", "vehicle.list_summaries_by_user"),
		slog.String("user_id", userID),
		slog.Int("rows", rows),
		slog.Duration("duration", dur),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		r.logger.Warn("vehicle list summary query failed", attrs...)
		return
	}
	r.logger.Info("vehicle list summary query", attrs...)
}

// scanVehicleSummaryRow scans the lean projection into a
// VehicleSummary. Pure stdlib — no encryptor, no GPS resolution, no
// nav-route blob handling.
func scanVehicleSummaryRow(row rowScanner) (VehicleSummary, error) {
	var (
		v      VehicleSummary
		status string
	)
	if err := row.Scan(
		&v.ID,
		&v.UserID,
		&v.VIN,
		&v.Name,
		&v.Model,
		&v.Year,
		&v.Color,
		&status,
		&v.ChargeLevel,
		&v.EstimatedRange,
		&v.LastUpdated,
		&v.HasActiveRide,
	); err != nil {
		return VehicleSummary{}, fmt.Errorf("scan vehicle summary: %w", err)
	}
	v.Status = VehicleStatus(status)
	return v, nil
}

