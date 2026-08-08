package telemetry

// Fleet-read, token-resolution, persistence and per-VIN debounce helpers for
// the ServiceStatusMonitor. Split out of service_status_monitor.go (which owns
// the subscriptions and the edge/transition logic) so that file stays inside
// the 300-line cap as the MYR-316 service-window pipeline grows alongside it.

import (
	"context"
	"log/slog"
	"time"
)

// resolveStatus maps the REST in_service flag to the persisted status. Called
// only after ServiceMode has been confirmed off, so the OR reduces to the flag.
func resolveStatus(inService bool) string {
	if inService {
		return serviceStatusInService
	}
	return serviceStatusParked
}

// resolveOwnerToken resolves a VIN's owning user and their (auto-refreshed)
// Tesla access token. ok is false on any failure — every failure is logged and
// non-fatal. Shared by the vehicle read and the MYR-316 service-window read so
// neither grows its own copy of the owner→token walk.
func (m *ServiceStatusMonitor) resolveOwnerToken(ctx context.Context, vin string) (token string, ok bool) {
	userID, err := m.owners.GetVehicleOwner(ctx, vin)
	if err != nil {
		m.logger.Warn("service-status: owner lookup failed — skipping read",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return "", false
	}
	tok, err := m.tokens.Resolve(ctx, userID)
	if err != nil {
		m.logger.Warn("service-status: no Tesla token — skipping read",
			slog.String("vin", redactVIN(vin)), slog.String("user_id", userID))
		return "", false
	}
	return tok.AccessToken, true
}

// readVehicleREST resolves the owner token and reads Tesla's authoritative REST
// vehicle object (GET /api/1/vehicles/{vin}). It returns the state plus the
// resolved owner access token so a follow-up vehicle_data backfill (MYR-260) or
// service_data read (MYR-316) can reuse the token without a second owner/token
// resolution. ok is false (the read is skipped/failed) on any error — every
// failure is logged and non-fatal.
func (m *ServiceStatusMonitor) readVehicleREST(ctx context.Context, vin string) (state *FleetVehicleState, token string, ok bool) {
	token, ok = m.resolveOwnerToken(ctx, vin)
	if !ok {
		return nil, "", false
	}
	st, err := m.reader.GetVehicle(ctx, token, vin)
	if err != nil {
		m.logger.Warn("service-status: vehicle read failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return nil, "", false
	}
	return st, token, true
}

// persist writes the resolved status, non-fatal on failure.
//
// MYR-454: the `parked` value on this path is a CONNECTED BASELINE, not an
// observation that the car is stationary — the monitor has no motion data at
// all. Writing it unconditionally would stomp a `driving` the telemetry fold
// had just derived, on every connectivity edge, reproducing the very bug
// MYR-454 fixes. So the baseline goes through the guarded write, which only
// applies over `in_service` or `offline` — the two states this path exists to
// move a car out of. `in_service` itself stays an unconditional write: that IS
// an observation, from ServiceMode or Tesla's REST flag, and it must win.
func (m *ServiceStatusMonitor) persist(ctx context.Context, vin, status string) {
	write := m.updater.UpdateVehicleStatus
	if status == serviceStatusParked {
		write = m.updater.UpdateVehicleStatusBaseline
	}
	if err := write(ctx, vin, status); err != nil {
		m.logger.Warn("service-status: persist failed (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("status", status),
			slog.String("error", err.Error()))
		return
	}
	m.logger.Info("service-status persisted",
		slog.String("vin", redactVIN(vin)),
		slog.String("status", status))
}

// serviceModeOn reports the cached ServiceMode (proto 159) state for a VIN,
// defaulting to false when none has been seen since the last (re)connect.
func (m *ServiceStatusMonitor) serviceModeOn(vin string) bool {
	if v, ok := m.serviceMode.Load(vin); ok {
		if sm, ok := v.(bool); ok {
			return sm
		}
	}
	return false
}

// allow implements the per-VIN read debounce. It returns true (and stamps the
// read time) only when the cooldown since the last read has elapsed. Serial
// per-subscription delivery plus the sync.Map keep it safe across the two
// subscriptions (connectivity + telemetry).
func (m *ServiceStatusMonitor) allow(vin string) bool {
	now := m.now()
	if v, ok := m.lastRead.Load(vin); ok {
		if last, ok := v.(time.Time); ok && now.Sub(last) < m.cooldown {
			return false
		}
	}
	m.lastRead.Store(vin, now)
	return true
}

// stamp records a read time without gating, so a subsequent connectivity edge
// within the cooldown is debounced.
func (m *ServiceStatusMonitor) stamp(vin string) {
	m.lastRead.Store(vin, m.now())
}
