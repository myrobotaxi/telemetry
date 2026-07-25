package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Status enum values written to Vehicle.status. They MUST equal the
// store.VehicleStatus values (vehicle-state-schema.md §2.4); kept as local
// consts so this package does not import internal/store.
const (
	serviceStatusInService = "in_service"
	serviceStatusParked    = "parked" // neutral connected baseline; the live stream refines to driving/charging
)

// serviceModeFieldKey is the internal telemetry field name for ServiceMode
// (proto 159) inside a VehicleTelemetryEvent (fields.go FieldServiceMode).
const serviceModeFieldKey = "serviceMode"

// defaultServiceReadCooldown is the per-VIN debounce window for the
// connectivity-edge in_service read. A flapping mTLS connection emits
// connect/disconnect edges in bursts; the cooldown collapses a burst into a
// single Fleet API read so we never spam Tesla (MYR-259).
const defaultServiceReadCooldown = 45 * time.Second

// FleetVehicleReader reads a single vehicle's REST object from Tesla's Fleet
// API. Satisfied by *FleetAPIClient.GetVehicle. Declared at the consumer
// site so the monitor can be unit-tested with a fake reader — NO real Tesla
// call ever fires from a test (safety invariant, MYR-259).
type FleetVehicleReader interface {
	GetVehicle(ctx context.Context, token, vin string) (*FleetVehicleState, error)
}

// VehicleStatusUpdater persists a vehicle's derived status enum. Satisfied
// by an adapter over store.VehicleRepo.UpdateStatus in cmd/telemetry-server.
// The status is passed as its string enum value so this package does not
// import internal/store.
type VehicleStatusUpdater interface {
	UpdateVehicleStatus(ctx context.Context, vin, status string) error
}

// ServiceStatusMonitor surfaces a vehicle's "In Service" status EVENT-DRIVEN
// (no polling, no timer). It owns the persisted Vehicle.status transition
// BOTH into and out of `in_service`:
//
//   - Transition IN: a ServiceMode (proto 159) OFF→ON telemetry edge, or a
//     connectivity edge whose authoritative REST `in_service` read is true.
//   - Transition OUT — the cases the wire-only live derivation cannot persist:
//   - any connectivity edge (e.g. reconnect) after leaving service: the
//     debounced REST read returns not-in-service, so the resolved write
//     persists `parked` (the neutral connected baseline; the live stream
//     refines to driving/charging as frames arrive).
//   - a continuously-connected car that never disconnects: a ServiceMode
//     ON→OFF telemetry edge triggers an authoritative reconcile that
//     persists `parked` unless REST independently keeps it in service.
//
// This closes the "stuck in_service" defect: without the OUT transitions the
// persisted column would only ever leave in_service via a completed drive, so
// a car that comes back from service and parks WITHOUT driving would read
// "In Service" forever on GET /api/vehicles and every on-connect snapshot.
//
// A genuinely in-service car (ServiceMode on OR REST in_service true) stays
// in_service — the reconcile never flaps it out while a signal holds. Reads
// are debounced per-VIN and every failure mode is non-fatal.
type ServiceStatusMonitor struct {
	bus      events.Bus
	reader   FleetVehicleReader
	tokens   teslaTokenResolver
	owners   VehicleOwnerLookup
	updater  VehicleStatusUpdater
	cooldown time.Duration
	logger   *slog.Logger

	now         func() time.Time // injectable clock (debounce tests)
	lastRead    sync.Map         // VIN string → time.Time
	serviceMode sync.Map         // VIN string → bool (last-seen ServiceMode from the stream)
	subs        []events.Subscription
}

// ServiceStatusMonitorOption configures optional monitor dependencies.
type ServiceStatusMonitorOption func(*ServiceStatusMonitor)

// WithServiceReadCooldown overrides the per-VIN debounce window. A
// non-positive value is ignored (the default is kept).
func WithServiceReadCooldown(d time.Duration) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if d > 0 {
			m.cooldown = d
		}
	}
}

// withServiceClock injects a clock for deterministic debounce tests.
func withServiceClock(now func() time.Time) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if now != nil {
			m.now = now
		}
	}
}

// NewServiceStatusMonitor builds a monitor. All dependencies are required;
// wiring MUST NOT construct it unless a real Fleet API reader + token
// resolver are available (cmd guards this so no live Tesla call is wired in
// tests/CI).
func NewServiceStatusMonitor(
	bus events.Bus,
	reader FleetVehicleReader,
	tokens teslaTokenResolver,
	owners VehicleOwnerLookup,
	updater VehicleStatusUpdater,
	logger *slog.Logger,
	opts ...ServiceStatusMonitorOption,
) *ServiceStatusMonitor {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	m := &ServiceStatusMonitor{
		bus:      bus,
		reader:   reader,
		tokens:   tokens,
		owners:   owners,
		updater:  updater,
		cooldown: defaultServiceReadCooldown,
		logger:   logger,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start subscribes to the connectivity topic (edge-triggered reads) and the
// telemetry topic (ServiceMode ON/OFF edges). Each subscription runs in its
// own goroutine with serial per-subscription delivery; the per-VIN debounce
// and caches use sync.Maps to stay safe across the two subscriptions.
func (m *ServiceStatusMonitor) Start() error {
	conn, err := m.bus.Subscribe(events.TopicConnectivity, m.makeConnHandler())
	if err != nil {
		return fmt.Errorf("service-status monitor: subscribe connectivity: %w", err)
	}
	m.subs = append(m.subs, conn)

	tel, err := m.bus.Subscribe(events.TopicVehicleTelemetry, m.makeTelemetryHandler())
	if err != nil {
		m.unsubscribeAll()
		return fmt.Errorf("service-status monitor: subscribe telemetry: %w", err)
	}
	m.subs = append(m.subs, tel)

	m.logger.Info("service-status monitor started",
		slog.Duration("read_cooldown", m.cooldown),
	)
	return nil
}

// Stop unsubscribes from all topics.
func (m *ServiceStatusMonitor) Stop() {
	m.unsubscribeAll()
	m.logger.Info("service-status monitor stopped")
}

func (m *ServiceStatusMonitor) unsubscribeAll() {
	for _, sub := range m.subs {
		if err := m.bus.Unsubscribe(sub); err != nil {
			m.logger.Warn("service-status monitor: unsubscribe failed",
				slog.String("error", err.Error()),
			)
		}
	}
	m.subs = nil
}

// makeConnHandler adapts handleConnectivity to the bus handler signature,
// giving each edge a fresh detached context so a slow Fleet API read is
// bounded and unaffected by the parent lifetime.
func (m *ServiceStatusMonitor) makeConnHandler() events.Handler {
	return func(event events.Event) {
		payload, ok := event.Payload.(events.ConnectivityEvent)
		if !ok {
			m.logger.Error("service-status monitor: unexpected connectivity payload",
				slog.String("event_id", event.ID),
			)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultFleetAPITimeout)
		defer cancel()
		m.handleConnectivity(ctx, payload)
	}
}

// makeTelemetryHandler adapts handleTelemetry to the bus handler signature.
func (m *ServiceStatusMonitor) makeTelemetryHandler() events.Handler {
	return func(event events.Event) {
		payload, ok := event.Payload.(events.VehicleTelemetryEvent)
		if !ok {
			return // other telemetry payloads are not our concern
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultFleetAPITimeout)
		defer cancel()
		m.handleTelemetry(ctx, payload)
	}
}

// handleConnectivity resolves + persists the vehicle status on a connect or
// disconnect edge, then clears the ServiceMode cache on disconnect so a stale
// service-mode flag cannot leak across a reconnect (mirrors the broadcaster's
// per-VIN cache-wipe on disconnect).
func (m *ServiceStatusMonitor) handleConnectivity(ctx context.Context, evt events.ConnectivityEvent) {
	m.resolveViaRead(ctx, evt.VIN, evt.Status.String())
	if evt.Status == events.StatusDisconnected {
		m.serviceMode.Delete(evt.VIN)
	}
}

// handleTelemetry tracks ServiceMode (proto 159) edges from the stream. An
// OFF→ON edge persists in_service immediately (a definitive live signal); an
// ON→OFF edge triggers an authoritative reconcile so a continuously-connected
// car that leaves service self-corrects out of in_service without needing a
// connectivity edge or a completed drive. Resend frames (unchanged value) are
// no-ops — only true value transitions act, so there is no Fleet API spam.
func (m *ServiceStatusMonitor) handleTelemetry(ctx context.Context, evt events.VehicleTelemetryEvent) {
	tv, ok := evt.Fields[serviceModeFieldKey]
	if !ok || tv.BoolVal == nil {
		return
	}
	sm := *tv.BoolVal
	prev, had := m.serviceMode.Load(evt.VIN)
	m.serviceMode.Store(evt.VIN, sm)
	prevOn := had && prev.(bool)

	switch {
	case sm && !prevOn:
		// OFF/unknown → ON: definitively in service.
		m.persist(ctx, evt.VIN, serviceStatusInService)
	case !sm && prevOn:
		// ON → OFF: left service — reconcile against an authoritative read.
		m.reconcileServiceModeOff(ctx, evt.VIN)
	}
}

// resolveViaRead fires ONE debounced authoritative in_service read for a
// connectivity edge and persists the resolved status. A cached ServiceMode-on
// short-circuits to in_service (definitive live signal, no read). A debounced
// edge or a failed read is skipped (non-fatal, last-known preserved) — no
// stale write and no Fleet API spam under a flapping connection.
func (m *ServiceStatusMonitor) resolveViaRead(ctx context.Context, vin, edge string) {
	if m.serviceModeOn(vin) {
		m.persist(ctx, vin, serviceStatusInService)
		return
	}
	if !m.allow(vin) {
		m.logger.Debug("service-status: connectivity edge debounced",
			slog.String("vin", redactVIN(vin)), slog.String("edge", edge))
		return
	}
	inService, ok := m.readInService(ctx, vin)
	if !ok {
		return // non-fatal; leave last-known status
	}
	m.persist(ctx, vin, resolveStatus(inService))
}

// reconcileServiceModeOff is the guaranteed transition-OUT for a
// continuously-connected car leaving service. ServiceMode is authoritatively
// OFF (live); an authoritative read decides whether REST independently keeps
// it in service. On any read failure we TRUST the live ServiceMode-off signal
// and persist parked — never leaving the car stuck in_service. The debounce is
// stamped so a coincident connectivity edge does not double-read.
func (m *ServiceStatusMonitor) reconcileServiceModeOff(ctx context.Context, vin string) {
	m.stamp(vin)
	if inService, ok := m.readInService(ctx, vin); ok && inService {
		// REST still holds it in service (in_service = ServiceMode OR REST).
		m.persist(ctx, vin, serviceStatusInService)
		return
	}
	m.persist(ctx, vin, serviceStatusParked)
}

// resolveStatus maps the REST in_service flag to the persisted status. Called
// only after ServiceMode has been confirmed off, so the OR reduces to the flag.
func resolveStatus(inService bool) string {
	if inService {
		return serviceStatusInService
	}
	return serviceStatusParked
}

// readInService resolves the owner token and reads Tesla's authoritative REST
// in_service flag. ok is false (and the read is skipped/failed) on any error —
// every failure is logged and non-fatal.
func (m *ServiceStatusMonitor) readInService(ctx context.Context, vin string) (inService, ok bool) {
	userID, err := m.owners.GetVehicleOwner(ctx, vin)
	if err != nil {
		m.logger.Warn("service-status: owner lookup failed — skipping in_service read",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return false, false
	}
	tok, err := m.tokens.Resolve(ctx, userID)
	if err != nil {
		m.logger.Warn("service-status: no Tesla token — skipping in_service read",
			slog.String("vin", redactVIN(vin)), slog.String("user_id", userID))
		return false, false
	}
	state, err := m.reader.GetVehicle(ctx, tok.AccessToken, vin)
	if err != nil {
		m.logger.Warn("service-status: in_service read failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return false, false
	}
	return state.InService, true
}

// persist writes the resolved status, non-fatal on failure.
func (m *ServiceStatusMonitor) persist(ctx context.Context, vin, status string) {
	if err := m.updater.UpdateVehicleStatus(ctx, vin, status); err != nil {
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
