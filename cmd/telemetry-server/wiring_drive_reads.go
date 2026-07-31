package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupDriveReadEndpoints mounts the two drive-scoped read endpoints —
// GET /api/drives/{driveId}/route (§7.4) and GET /api/drives/{driveId} (§7.3).
//
// Split out of setupHTTPHandlers so that function stays inside the length cap.
// The pair belongs together: both resolve the drive first, take the vehicleId
// off the drive record, and then run the SAME access gate as the drives list —
// owner, or a viewer holding at least `live_history` (MYR-184).
func setupDriveReadEndpoints(deps httpRouteDeps, snapshotAdapter telemetry.VehicleSnapshotReader) {
	// DV-20 (FR-3.3): GET /api/drives/{driveId}/route — the recorded
	// breadcrumb polyline for a completed drive. Same auth + ownership +
	// role-mask flow as the drives list; the route's vehicleId comes off
	// the drive record, then snapshotAdapter checks ownership.
	driveRouteHandler := telemetry.NewDriveRouteHandler(
		deps.authenticator,
		snapshotAdapter,
		&driveRouteAdapter{repo: deps.driveRepo},
		deps.logger.With(slog.String("component", "drive-route")),
		telemetry.WithDriveRouteRoleResolver(deps.authenticator),
		// MYR-369: NO share reader. The drives surfaces are owner-only
		// again — the `live_history` capability was removed from the
		// product — and the handler no longer has a seam to pass one
		// through, so re-opening them is a deliberate change rather than
		// one wiring line.
		telemetry.WithDriveRouteMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/drives/{driveId}/route"),
	)
	deps.srv.HandleFunc("GET /api/drives/{driveId}/route", driveRouteHandler.ServeHTTP)

	// MYR-130 (FR-3.4): GET /api/drives/{driveId} — the full per-drive
	// stats record (distance, duration, energy, FSD, interventions,
	// start/end loc+addr) minus routePoints. Closes the last DV-20
	// SDK-surface gap. Same auth + ownership + role-mask flow as the
	// route endpoint; the drive's vehicleId comes off the drive record,
	// then snapshotAdapter checks ownership.
	driveDetailHandler := telemetry.NewDriveDetailHandler(
		deps.authenticator,
		snapshotAdapter,
		&driveDetailAdapter{repo: deps.driveRepo},
		deps.logger.With(slog.String("component", "drive-detail")),
		telemetry.WithDriveDetailRoleResolver(deps.authenticator),
		// MYR-369: NO share reader. The drives surfaces are owner-only
		// again — the `live_history` capability was removed from the
		// product — and the handler no longer has a seam to pass one
		// through, so re-opening them is a deliberate change rather than
		// one wiring line.
		telemetry.WithDriveDetailMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/drives/{driveId}"),
	)
	deps.srv.HandleFunc("GET /api/drives/{driveId}", driveDetailHandler.ServeHTTP)
}
