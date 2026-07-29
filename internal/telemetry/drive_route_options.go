package telemetry

import "github.com/myrobotaxi/telemetry/internal/mask"

// Optional-dependency constructors for the GET /api/drives/{driveId}/route
// handler. Split out of the handler file so it stays inside the 300-line cap;
// each option is inert unless the composition root passes it, and the handler
// fails CLOSED without it.

// DriveRouteOption configures optional dependencies on DriveRouteHandler.
type DriveRouteOption func(*DriveRouteHandler)

// WithDriveRouteRoleResolver enables role-based field masking on the
// handler. Owners and viewers share the DriveRoute allow-list (just
// `routePoints`) per rest-api.md §5.2.4, so this is plumbed for FR-5.1
// sharing readiness — the mask is a no-op for both roles today.
func WithDriveRouteRoleResolver(roles roleResolver) DriveRouteOption {
	return func(h *DriveRouteHandler) {
		h.roles = roles
	}
}

// WithDriveRouteShareReader lets the handler admit VIEWERS holding an accepted
// vehicle share of at least `live_history` (MYR-184). Without it the
// handler is owner-only, which is the pre-MYR-184 behaviour and the
// fail-closed default.
func WithDriveRouteShareReader(shares VehicleShareReader) DriveRouteOption {
	return func(h *DriveRouteHandler) {
		h.shares = shares
	}
}

// WithDriveRouteMaskAudit attaches a mask-audit emitter (MYR-71,
// rest-api.md §5.3). endpoint is the route pattern written to
// metadata.endpoint. emitter MAY be nil — in which case this is a no-op.
func WithDriveRouteMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) DriveRouteOption {
	return func(h *DriveRouteHandler) {
		if emitter == nil {
			return
		}
		h.auditEmitter = emitter
		if metrics == nil {
			metrics = mask.NoopAuditMetrics{}
		}
		h.auditMetrics = metrics
		h.auditEndpoint = endpoint
	}
}
