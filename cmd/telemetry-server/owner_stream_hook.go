package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// buildOwnerStreamHook assembles the best-effort post-link stream setup. The
// vehicle lister always targets the Fleet API directly (a read). The fleet
// pusher (a state-changing call that targets a real car) is wired ONLY when the
// tesla-http-proxy + telemetry endpoint are configured — otherwise it stays nil
// and the config push is left to ops/web. This runtime guard is what keeps a
// live push out of any dev/test process. Always returns a non-nil hook.
func buildOwnerStreamHook(cfg *config.Config, upsert vehicleUpserter, logger *slog.Logger) postLinkHook {
	lister := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL: cfg.Proxy().FleetAPIBaseURL, // empty => default NA Fleet API
	}, logger.With(slog.String("component", "fleet-list")))

	var pusher fleetConfigPusher
	if cfg.Proxy().URL != "" && cfg.Proxy().FleetTelemetryHostname != "" {
		pusher = &realFleetPusher{
			client: telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
				BaseURL:    cfg.Proxy().URL,
				HTTPClient: proxyHTTPClient(cfg.Proxy().URL, logger),
			}, logger.With(slog.String("component", "fleet-push"))),
			endpoint: telemetry.EndpointConfig{
				Hostname: cfg.Proxy().FleetTelemetryHostname,
				Port:     cfg.Proxy().FleetTelemetryPort,
				CA:       cfg.Proxy().FleetTelemetryCA,
			},
		}
		logger.Info("owner-onboarding fleet-config auto-push enabled")
	} else {
		logger.Warn("owner-onboarding fleet-config auto-push disabled: proxy/telemetry endpoint not configured")
	}

	return &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: logger}
}

// vehicleLister lists a linked owner's vehicles from the Fleet API.
type vehicleLister interface {
	ListVehicles(ctx context.Context, token string) ([]telemetry.FleetVehicle, error)
}

// vehicleUpserter seeds "Vehicle" identity rows (store.OwnerProvisioner).
type vehicleUpserter interface {
	UpsertOwnedVehicle(ctx context.Context, in store.OwnedVehicleInput) (store.VehicleUpsertOutcome, error)
}

// fleetConfigPusher pushes the fleet-telemetry config for one VIN so the car
// starts streaming. The real implementation calls the tesla-http-proxy; it is
// injected (and nil unless the proxy is configured) so the SAFETY invariant
// holds: no live push is ever wired in tests or when unconfigured.
type fleetConfigPusher interface {
	PushForVIN(ctx context.Context, token, vin string) error
}

// ownerStreamHook is the best-effort post-link stream setup (MYR-257 steps 2+3):
// list the owner's vehicles, seed their "Vehicle" rows, and (when a real proxy
// is configured) push the fleet-telemetry config per VIN so the car streams
// without an ops `fleet-config push`. Every step is best-effort — a failure is
// logged and never fails the link.
type ownerStreamHook struct {
	lister vehicleLister
	upsert vehicleUpserter
	pusher fleetConfigPusher // nil => push disabled (proxy unconfigured); guard keeps live pushes out of tests
	logger *slog.Logger
}

// AfterLink implements postLinkHook.
func (h *ownerStreamHook) AfterLink(ctx context.Context, userID, accessToken string) {
	vehicles, err := h.lister.ListVehicles(ctx, accessToken)
	if err != nil {
		h.logger.Warn("owner stream setup: list vehicles failed (skipping)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return
	}

	for _, v := range vehicles {
		vin := v.VIN

		// Ownership filter (MYR-257 finding 3): never provision a car the caller
		// only shares (shared-driver access). Skip + audit non-owner vehicles.
		if !v.IsOwner() {
			h.logger.Info("owner_vehicle_skipped",
				slog.String("event", "owner_vehicle_skipped"),
				slog.String("user_id", userID),
				slog.String("reason", "not_owner"),
				slog.String("vin", redactVIN(vin)))
			continue
		}

		outcome, err := h.upsert.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
			UserID:         userID,
			TeslaVehicleID: v.ID.String(),
			VIN:            vin,
			Name:           v.DisplayName,
		})
		if err != nil {
			h.logger.Warn("owner stream setup: vehicle upsert failed (skipping vehicle)",
				slog.String("user_id", userID), slog.String("error", err.Error()))
			continue
		}
		if outcome == store.VehicleSkippedCrossUser {
			// The teslaVehicleId already belongs to another user — never
			// reassigned. Audit and do NOT push config for a car we don't own.
			h.logger.Warn("owner_vehicle_skipped",
				slog.String("event", "owner_vehicle_skipped"),
				slog.String("user_id", userID),
				slog.String("reason", "cross_user_teslaVehicleId"),
				slog.String("vin", redactVIN(vin)))
			continue
		}
		h.logger.Info("owner_vehicle_owned",
			slog.String("event", "owner_vehicle_owned"),
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)))

		if h.pusher == nil {
			continue // proxy unconfigured — stream starts when ops/web pushes config
		}
		if len(vin) != vinLength {
			continue // malformed VIN — nothing safe to push against
		}
		if err := h.pusher.PushForVIN(ctx, accessToken, vin); err != nil {
			h.logger.Warn("owner stream setup: fleet-config push failed (owner still linked; retriable)",
				slog.String("user_id", userID), slog.String("error", err.Error()))
		}
	}
}

// vinLength is the fixed Tesla VIN length used to guard the fleet push.
const vinLength = 17

// redactVIN returns a VIN with only the last 4 characters visible, for log
// safety (data-classification §2.1 VIN redaction rule).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// realFleetPusher is the runtime fleetConfigPusher. It is only constructed when
// a proxy URL + telemetry endpoint are configured, so it never exists in tests.
type realFleetPusher struct {
	client   *telemetry.FleetAPIClient
	endpoint telemetry.EndpointConfig
}

// PushForVIN pushes the default field config + endpoint to one VIN. Mirrors the
// request construction in `ops fleet-config push` (cmd/ops/fleet.go).
func (p *realFleetPusher) PushForVIN(ctx context.Context, token, vin string) error {
	expTime := time.Now().Add(350 * 24 * time.Hour).Unix()
	var ca *string
	if p.endpoint.CA != "" {
		ca = &p.endpoint.CA
	}
	req := telemetry.FleetConfigRequest{
		VINs: []string{vin},
		Config: telemetry.FleetConfig{
			Hostname:   p.endpoint.Hostname,
			Port:       p.endpoint.Port,
			CA:         ca,
			Fields:     telemetry.DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &expTime,
		},
	}
	_, err := p.client.PushTelemetryConfig(ctx, token, req)
	return err
}
