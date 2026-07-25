package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// serviceStatusInService is the wire/enum value written to Vehicle.status
// when Tesla's REST `in_service` flag is true. It MUST equal
// store.VehicleStatusInService ("in_service", vehicle-state-schema.md §2.4);
// kept as a local const so this package does not import internal/store.
const serviceStatusInService = "in_service"

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
// (no polling, no timer). It subscribes to ConnectivityEvent and, on each
// connect/disconnect EDGE for a vehicle, fires exactly ONE authoritative
// Tesla REST `in_service` read (GET /api/1/vehicles/{vin}) — debounced
// per-VIN — and persists status=in_service when the flag is true.
//
// This is Leg 2 of MYR-259. Leg 1 (the pushed ServiceMode telemetry field,
// proto 159) drives the LIVE wire status via ws.deriveVehicleStatus; this
// leg is the authoritative persistence + reconcile that also covers firmware
// that never emits ServiceMode.
//
// Precedence note (vehicle-state-schema.md §2.4): this leg only ever WRITES
// in_service (never clears to parked). A false REST flag leaves the
// last-known status untouched — the live pipeline (drive-ended→parked, gear
// frames) owns the transition back out of in_service, and active driving
// always wins in the live derivation. Read failures are non-fatal.
type ServiceStatusMonitor struct {
	bus      events.Bus
	reader   FleetVehicleReader
	tokens   teslaTokenResolver
	owners   VehicleOwnerLookup
	updater  VehicleStatusUpdater
	cooldown time.Duration
	logger   *slog.Logger

	now      func() time.Time // injectable clock (debounce tests)
	lastRead sync.Map         // VIN string → time.Time
	subs     []events.Subscription
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

// Start subscribes to the connectivity topic. Each edge is handled in the
// subscription's dedicated goroutine with serial per-subscription delivery,
// so the per-VIN debounce needs no locking.
func (m *ServiceStatusMonitor) Start() error {
	sub, err := m.bus.Subscribe(events.TopicConnectivity, m.makeHandler())
	if err != nil {
		return fmt.Errorf("service-status monitor: subscribe connectivity: %w", err)
	}
	m.subs = append(m.subs, sub)
	m.logger.Info("service-status monitor started",
		slog.Duration("read_cooldown", m.cooldown),
	)
	return nil
}

// Stop unsubscribes from the connectivity topic.
func (m *ServiceStatusMonitor) Stop() {
	for _, sub := range m.subs {
		if err := m.bus.Unsubscribe(sub); err != nil {
			m.logger.Warn("service-status monitor: unsubscribe failed",
				slog.String("error", err.Error()),
			)
		}
	}
	m.subs = nil
	m.logger.Info("service-status monitor stopped")
}

// makeHandler adapts handleConnectivity to the bus handler signature, giving
// each edge a fresh detached context so a slow Fleet API read is bounded and
// unaffected by the parent lifetime.
func (m *ServiceStatusMonitor) makeHandler() events.Handler {
	return func(event events.Event) {
		payload, ok := event.Payload.(events.ConnectivityEvent)
		if !ok {
			m.logger.Error("service-status monitor: unexpected payload type",
				slog.String("event_id", event.ID),
			)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultFleetAPITimeout)
		defer cancel()
		m.handleConnectivity(ctx, payload)
	}
}

// handleConnectivity fires ONE debounced in_service read for the edge's VIN
// and persists status=in_service when Tesla reports the vehicle in service.
// Every failure mode (debounced, unknown owner, no token, read error, write
// error) is non-fatal and logged — the last-known status is preserved.
func (m *ServiceStatusMonitor) handleConnectivity(ctx context.Context, evt events.ConnectivityEvent) {
	vin := evt.VIN
	if !m.allow(vin) {
		m.logger.Debug("service-status: connectivity edge debounced",
			slog.String("vin", redactVIN(vin)),
			slog.String("edge", evt.Status.String()),
		)
		return
	}

	userID, err := m.owners.GetVehicleOwner(ctx, vin)
	if err != nil {
		m.logger.Warn("service-status: owner lookup failed — skipping in_service read",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}

	tok, err := m.tokens.Resolve(ctx, userID)
	if err != nil {
		m.logger.Warn("service-status: no Tesla token — skipping in_service read",
			slog.String("vin", redactVIN(vin)),
			slog.String("user_id", userID),
		)
		return
	}

	state, err := m.reader.GetVehicle(ctx, tok.AccessToken, vin)
	if err != nil {
		// Non-fatal: leave the last-known status. A flaky Fleet API read
		// must never crash the edge handler or clobber status.
		m.logger.Warn("service-status: in_service read failed (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}

	if !state.InService {
		// REST authoritatively says not-in-service. Do NOT clobber the
		// live driving/parked/charging status — the live pipeline owns the
		// transition out of in_service (§2.4).
		m.logger.Debug("service-status: vehicle not in service",
			slog.String("vin", redactVIN(vin)),
			slog.String("tesla_state", state.State),
		)
		return
	}

	if err := m.updater.UpdateVehicleStatus(ctx, vin, serviceStatusInService); err != nil {
		m.logger.Warn("service-status: persist in_service failed (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}

	m.logger.Info("vehicle marked in_service (Tesla REST in_service flag)",
		slog.String("vin", redactVIN(vin)),
		slog.String("tesla_state", state.State),
		slog.String("edge", evt.Status.String()),
	)
}

// allow implements the per-VIN debounce. It returns true (and stamps the
// read time) only when the cooldown since the last read has elapsed. Serial
// per-subscription delivery makes this lock-free.
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
