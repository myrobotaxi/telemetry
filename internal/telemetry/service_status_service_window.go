package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// ServiceWindowWriter persists the service-window columns for one vehicle,
// keyed by the Prisma vehicle id (NOT the VIN — the side table is keyed by
// vehicle_id). Satisfied by an adapter over store.VehicleRepo in
// cmd/telemetry-server.
//
// SetServiceETC MUST write a nil through as SQL NULL rather than skipping it:
// Tesla dropping its estimate is a real observation that has to overwrite a
// stale one. ClearServiceWindow drops BOTH sources and reports whether anything
// was actually cleared, so the monitor can stay quiet on the overwhelmingly
// common "car was never in service" edge.
type ServiceWindowWriter interface {
	SetServiceETC(ctx context.Context, vehicleID string, etc *time.Time) error
	ClearServiceWindow(ctx context.Context, vehicleID string) (bool, error)
}

// ServiceDataReader reads Tesla's service-visit record. Satisfied by
// *FleetAPIClient.GetServiceData. Declared at the consumer site so the monitor
// is unit-testable with a fake — NO real Tesla call ever fires from a test.
type ServiceDataReader interface {
	GetServiceData(ctx context.Context, token, vin string) (*ServiceData, error)
}

// VehicleIDLookup resolves a VIN to its Prisma vehicle id. The monitor works in
// VINs (that is what the telemetry stream and the Fleet API speak) but the
// service-window columns live on a table keyed by vehicle id, so one
// translation is unavoidable. Satisfied by the VIN cache adapter.
type VehicleIDLookup interface {
	GetVehicleID(ctx context.Context, vin string) (string, error)
}

// WithServiceWindow enables the MYR-316 service-window pipeline on the monitor.
// All three dependencies are required together; omitting the option leaves the
// monitor behaving exactly as it did before MYR-316, which is what the
// direct-handler unit tests and any deployment without a Fleet reader rely on.
func WithServiceWindow(reader ServiceDataReader, writer ServiceWindowWriter, ids VehicleIDLookup) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if reader == nil || writer == nil || ids == nil {
			return
		}
		m.serviceData = reader
		m.serviceWindow = writer
		m.vehicleIDs = ids
	}
}

// syncServiceWindow keeps the two service-window columns in step with the
// status the monitor just resolved. It is called from every path that persists
// a status, so the wire contract's auto-clear rule — a vehicle whose status is
// not in_service NEVER carries a non-null serviceEstimatedEndAt — holds no
// matter which transition-out path fired.
//
// In service: read Tesla's service_data and store service_etc (including a
// null, which retracts a stale estimate). The owner-entered fallback is left
// alone; it is a separate column precisely so Tesla arriving late does not
// erase what the owner typed.
//
// Out of service: clear BOTH columns. The visit is over, so the owner's guess
// about it is as meaningless as Tesla's, and leaving either behind would
// resurrect a stale "expected back" the next time the car entered service.
//
// Every failure is non-fatal and logged: a service window is advisory, and
// losing it must never block the status write it rides along with. The read
// shares the connectivity-edge debounce already stamped by allow(), so it adds
// at most ONE service_data call per VIN per cooldown — the same debounce family
// as the MYR-260 vehicle_data backfill.
func (m *ServiceStatusMonitor) syncServiceWindow(ctx context.Context, vin, status, token string) {
	if m.serviceWindow == nil {
		return // option not wired (unit tests, or no Fleet reader available)
	}

	vehicleID, err := m.vehicleIDs.GetVehicleID(ctx, vin)
	if err != nil {
		m.logger.Warn("service-window: vehicle id lookup failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	if status != serviceStatusInService {
		m.clearServiceWindow(ctx, vin, vehicleID)
		return
	}
	m.readServiceWindow(ctx, vin, vehicleID, token)
}

// syncServiceWindowInService reads and stores the window for a car we have just
// determined is in service on a path that resolved NO token — the live
// ServiceMode OFF→ON edge and the cached-ServiceMode-on connectivity
// short-circuit. Both deliberately skip the authoritative status read, so this
// pays the per-VIN debounce itself: without allow() a flapping connection would
// turn the short-circuit into an unbounded service_data poll.
func (m *ServiceStatusMonitor) syncServiceWindowInService(ctx context.Context, vin string) {
	if m.serviceWindow == nil || !m.allow(vin) {
		return
	}
	token, ok := m.resolveOwnerToken(ctx, vin)
	if !ok {
		return
	}
	m.syncServiceWindow(ctx, vin, serviceStatusInService, token)
}

// clearServiceWindow drops both estimates for a car that is no longer in
// service, logging only when something was actually cleared.
func (m *ServiceStatusMonitor) clearServiceWindow(ctx context.Context, vin, vehicleID string) {
	cleared, err := m.serviceWindow.ClearServiceWindow(ctx, vehicleID)
	if err != nil {
		m.logger.Warn("service-window: clear failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}
	if cleared {
		m.logger.Info("service-window cleared: vehicle left service",
			slog.String("vin", redactVIN(vin)))
	}
}

// readServiceWindow fetches Tesla's estimate for an in-service car and stores
// it. A token-less call (the caller had no owner token) skips the read rather
// than clearing, so a transient token problem cannot look like "Tesla withdrew
// its estimate".
func (m *ServiceStatusMonitor) readServiceWindow(ctx context.Context, vin, vehicleID, token string) {
	if token == "" {
		return
	}

	data, err := m.serviceData.GetServiceData(ctx, token, vin)
	if err != nil {
		// Advisory read: a failure leaves the last-known estimate in place. The
		// owner-entered fallback still covers the car if there was none.
		m.logger.Debug("service-window: service_data read skipped (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	var etc *time.Time
	if data != nil {
		etc = data.ServiceETC
	}
	if err := m.serviceWindow.SetServiceETC(ctx, vehicleID, etc); err != nil {
		m.logger.Warn("service-window: persist failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	// An all-null service_data body is COMMON AND NORMAL (a visit with no
	// appointment record), so the absent case logs at the same level as the
	// present one rather than as a problem.
	m.logger.Info("service-window updated from Tesla service_data",
		slog.String("vin", redactVIN(vin)),
		slog.Bool("has_estimate", etc != nil))
}
