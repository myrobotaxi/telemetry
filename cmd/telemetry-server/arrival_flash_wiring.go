package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/arrivalflash"
	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Arrival light-flash wiring (MYR-542). Composes the ride.waypoint_arrived
// subscriber that greets the rider with three headlight flashes when their car
// is observed at the kerb — the client's decision, lights only, no horn.
//
// Its own file rather than an extension of arrival_wiring.go: the detector and
// the greeting share a seam and nothing else. The detector talks to the ride
// repo and writes a lifecycle row; this talks to Tesla and writes nothing. They
// are also stopped by different switches for different reasons (see
// config.ArrivalFlashEnabled), and a file per switchable subsystem is what
// makes that separation visible at the wiring layer.
//
// Lives at cmd/ for the usual dependency-rule reason: internal/arrivalflash
// depends on three small consumer-site seams, and cmd/ is the only layer that
// can see internal/store, internal/telemetry and internal/commands at once to
// satisfy them.

// startArrivalFlash builds and starts the flasher, or does nothing when
// ARRIVAL_FLASH_ENABLED is false. Returns the flasher so the caller can Stop it
// on shutdown; nil when the switch is off.
//
// Started AFTER the auto-arrival detector, and that order is load-bearing in
// one direction only: the detector is the sole publisher of the seam this
// subscribes to, and a subscription registered after a publish misses it. In
// practice the gap is milliseconds at boot against a detector that needs a
// 20-second dwell before it can publish anything, so nothing is at risk either
// way — but the dependency is real and the ordering states it.
func startArrivalFlash(
	ctx context.Context,
	cfg *config.Config,
	bus events.Bus,
	vehicleRepo *store.VehicleRepo,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) (*arrivalflash.Flasher, error) {
	if !cfg.ArrivalFlashEnabled() {
		logger.Info("arrival light flash disabled by ARRIVAL_FLASH_ENABLED; " +
			"an arrival still moves the ride and notifies the rider, and the car does nothing")
		return nil, nil //nolint:nilnil // "off" is not an error and there is no flasher to return
	}

	flashLogger := logger.With(slog.String("component", "arrival-flash"))

	// The same command stack the nav dispatcher builds: the RoutingTransport
	// over the tesla-http-proxy (flash_lights is SignerRequired, so it MUST go
	// through the signing sidecar) and the shared owner-token resolver with its
	// refresher. A car whose virtual key is unpaired or whose owner's token has
	// expired therefore fails here exactly as it fails for a nav push — same
	// typed errors, no second opinion about what the fleet can be told to do.
	transport := newCommandTransport(cfg.Proxy().URL, cfg.Proxy().FleetAPIBaseURL,
		flashLogger.With(slog.String("subcomponent", "transport")))
	executor := commands.NewExecutor(transport, flashLogger.With(slog.String("subcomponent", "executor")))
	resolver := newTeslaTokenResolver(cfg, accountRepo, flashLogger)

	// The dispatch adapters are reused verbatim rather than cloned. Both are
	// pure translations — vehicle cuid → VIN, user id → access token — and the
	// dispatch sentinels they additionally wrap in are simply unread here,
	// because this consumer does not branch on WHY it failed: every failure is
	// the same silent log line.
	flasher := arrivalflash.New(
		bus,
		&dispatchVehicleResolverAdapter{repo: vehicleRepo},
		&dispatchTokenSourceAdapter{resolver: resolver},
		executor,
		arrivalflash.DefaultConfig(),
		flashLogger,
	)
	if err := flasher.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting arrival light flash: %w", err)
	}
	logger.Info("arrival light flash subscriber enabled",
		slog.Bool("signing_transport", transport.Enabled()),
	)
	return flasher, nil
}
