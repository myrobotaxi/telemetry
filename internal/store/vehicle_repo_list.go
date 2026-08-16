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
// JSON blob.
//
// MYR-515 narrowed that "no decryption" clause rather than breaking it: the
// projection now carries ONE of the three GPS pairs — `latitudeEnc` /
// `longitudeEnc` — and decrypts it per row, because VehicleSummary.location
// emits it. The invariant this file actually enforces is stated as "MUST NOT
// SELECT columns the response body doesn't emit", and that still holds. The
// other two pairs (destination, origin) and the nav-route blob remain out;
// they were the expensive part, and nothing on the catalog emits them.
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
// nav, no climate, and no destination / origin coordinates — those belong in
// the wide detail read. The car's OWN position is the one exception, added by
// MYR-515 because the rider picker has no other source for a non-watched row.
type VehicleSummary struct {
	ID     string
	UserID string
	VIN    string
	Name   string
	Model  string
	Year   int
	Color  string
	// LicensePlate is the owner-entered plate (MYR-286), read straight
	// off the Prisma-owned column. Empty string == not set.
	LicensePlate   string
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

	// MYR-316 service window, LEFT JOINed from go_vehicle_control_state.
	// ServiceETC (Tesla's own estimate) takes precedence over
	// ServiceExpectedEndAt (owner-entered); the handler emits
	// COALESCE(ServiceETC, ServiceExpectedEndAt) and ONLY while Status is
	// in_service. Both nil for the overwhelming majority of rows — a car that
	// has never been to service has no estimate to carry.
	ServiceETC           *time.Time
	ServiceExpectedEndAt *time.Time

	// RideShareEnabled is the owner's ride-sharing switch (MYR-342), LEFT
	// JOINed from the same go_vehicle_control_state row as the service window
	// via rideShareEnabledExpr. A plain bool: the COALESCE in that expression
	// makes "no side-table row" read as true, so an unpaused car and a car with
	// no control history are the same thing here — which is what the wire
	// contract requires (absent/unset means ENABLED).
	RideShareEnabled bool

	// TrimLabel is the DISPLAY-SAFE trim label (MYR-507), LEFT JOINed from the
	// same go_vehicle_control_state row as the service window and the switch —
	// the SAME column the /snapshot read emits as VehicleState.trimLabel
	// (MYR-320), not a second copy of it.
	//
	// A POINTER because the column is nullable and the distinction is
	// load-bearing: NULL means "Tesla has not told us this car's trim yet",
	// which a consumer must render as "no trim" rather than as an empty
	// fragment in a descriptor. Contrast the sibling Color, a NOT NULL column on
	// the Prisma-owned "Vehicle" row whose "not read yet" spelling is `''`.
	TrimLabel *string

	// Trim is the RAW BADGE CODE ("p100d"), carried since MYR-578 as the second
	// rung of the trim resolver's ladder — NEVER emitted raw on this surface.
	// The handler resolves label → badge → VIN drive-unit and emits the answer.
	Trim *string

	// Latitude / Longitude are the car's freshest known position (MYR-515),
	// decrypted from the `latitudeEnc` / `longitudeEnc` shadows — the SAME
	// ciphertext pair and the SAME resolveGPSPair helper the wide snapshot read
	// uses, so the catalog and the snapshot cannot disagree about where a car is.
	//
	// An ATOMIC PAIR: both non-nil or both nil, never one. resolveGPSPair
	// enforces that (a half-pair is corruption and surfaces no location), which
	// is why these are two pointers rather than two floats — the wire shape
	// above them is a single nullable object for the same reason.
	//
	// Nil means the server holds no fix. Note this is NOT the same as the
	// (0, 0) sentinel: a car that has genuinely streamed 0,0 decrypts to a
	// non-nil pair here and is collapsed to null at the WIRE layer, not here,
	// because the sentinel is a wire convention (vehicle-state-schema.md §2.3)
	// and this struct is storage.
	Latitude  *float64
	Longitude *float64

	// SetupSchedule is the car's go_fleet_config_attempts row (MYR-491), LEFT
	// JOINed alongside the control-state block. RAW STORAGE behind the derived
	// VehicleSummary.setupState — the catalog carries it because the rider-side
	// picker (MYR-437) must be able to show a shared car as "setting up" rather
	// than omitting it or badging it "offline", and it cannot fetch a snapshot
	// per row to find out. Zero value (Present false) means "no claim", which
	// is the safe reading on any hand-built row.
	SetupSchedule SetupSchedule
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
		v, scanErr := r.scanVehicleSummaryRow(rows)
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

// scanVehicleSummaryRow scans the lean projection into a VehicleSummary.
//
// A METHOD as of MYR-515 rather than the free function it used to be: the
// projection now carries the `latitudeEnc` / `longitudeEnc` ciphertext pair, so
// the scan needs the repo's encryptor, logger and metrics to resolve it. No
// nav-route blob and no destination / origin pairs — those stay in the wide
// detail read.
func (r *VehicleRepo) scanVehicleSummaryRow(row rowScanner) (VehicleSummary, error) {
	var (
		v      VehicleSummary
		status string
		gps    summaryGPSScan
		ss     setupScheduleScan
	)
	dests := append([]any{
		&v.ID,
		&v.UserID,
		&v.VIN,
		&v.Name,
		&v.Model,
		&v.Year,
		&v.Color,
		&v.LicensePlate,
		&status,
		&v.ChargeLevel,
		&v.EstimatedRange,
		&v.LastUpdated,
		gps.latDest(),
		gps.lngDest(),
		&v.HasActiveRide,
		&v.ServiceETC,
		&v.ServiceExpectedEndAt,
		&v.RideShareEnabled,
		&v.TrimLabel,
		&v.Trim,
	}, ss.dests()...)
	if err := row.Scan(dests...); err != nil {
		return VehicleSummary{}, fmt.Errorf("scan vehicle summary: %w", err)
	}
	v.Status = VehicleStatus(status)
	v.SetupSchedule = ss.value()
	v.Latitude, v.Longitude = gps.resolve(r)
	return v, nil
}

// summaryGPSScan holds the ciphertext halves of the catalog's position pair
// between the Scan and the decrypt.
//
// It exists so BOTH catalog scans (owner and shared) resolve the pair the same
// way, through the same resolveGPSPair the wide snapshot read uses. A second
// inline copy of the decrypt is exactly where a half-pair or a swallowed
// decrypt error would hide.
type summaryGPSScan struct {
	latEnc *string
	lngEnc *string
}

func (g *summaryGPSScan) latDest() any { return &g.latEnc }
func (g *summaryGPSScan) lngDest() any { return &g.lngEnc }

// resolve decrypts the pair, or returns no location.
//
// A repo built WITHOUT an encryptor (NewVehicleRepo) returns no location rather
// than attempting a decrypt: that constructor's documented contract is already
// "reads NO coordinates", and attempting one would log a per-row ERROR for
// every vehicle on every catalog fetch — noise that says nothing, since the
// absence is by construction rather than a fault.
func (g *summaryGPSScan) resolve(r *VehicleRepo) (lat, lng *float64) {
	if r.encryptor == nil {
		return nil, nil
	}
	return resolveGPSPair(g.latEnc, g.lngEnc, r.encryptor, r.logger, r.metrics, gpsPairs[0])
}
