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
// reconciler may be nil (the safety guard kept it off); it is assigned through
// an explicit check rather than straight into the interface field, because a
// nil *FleetConfigReconciler in an interface is a NON-nil interface holding a
// nil pointer and would sail past `h.pairing == nil` into a nil-receiver send.
func buildOwnerStreamHook(
	cfg *config.Config,
	upsert vehicleUpserter,
	reconciler *telemetry.FleetConfigReconciler,
	logger *slog.Logger,
) *ownerStreamHook {
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

	hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: logger}
	if reconciler != nil {
		hook.pairing = reconciler
	}
	return hook
}

// vehicleLister lists a linked owner's vehicles from the Fleet API.
type vehicleLister interface {
	ListVehicles(ctx context.Context, token string) ([]telemetry.FleetVehicle, error)
}

// vehicleUpserter seeds "Vehicle" identity rows (store.OwnerProvisioner).
//
// MYR-491 widened it with the schedule seed, which is the same actor doing the
// same job one table over: provisioning a car that cannot stream yet, and
// recording WHY. Keeping them on one interface is what guarantees the two
// writes are always wired together — a deployment that provisions vehicles but
// cannot say why they are silent is the state MYR-503 was reported from.
type vehicleUpserter interface {
	UpsertOwnedVehicle(ctx context.Context, in store.OwnedVehicleInput) (store.VehicleUpsertOutcome, error)
	// SeedFleetConfigSchedule records the link-time schedule row — and, when
	// there is one, the observation behind it (Tesla's `missing_key` skip, or a
	// failed push) — so the owner's first app open can name the one thing left
	// to do instead of waiting up to 45 minutes for the reconciler to discover
	// the same fact, and so the in-band self-heal signals have a row to land on.
	SeedFleetConfigSchedule(ctx context.Context, vin, outcome string, now time.Time) error
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
	// pairing receives proof of virtual-key pairing when the link-time push
	// APPLIES (MYR-529). Nil => nobody to tell (no reconciler wired).
	pairing pairingEvidenceNotifier
	logger  *slog.Logger
}

// pairingEvidenceNotifier is the reconciler's inbox for "this VIN's virtual key
// is proven paired". Consumer-site interface, satisfied by
// *telemetry.FleetConfigReconciler.
type pairingEvidenceNotifier interface {
	PairingEvidence(vin string)
}

// AfterLink implements postLinkHook. It is the PASSIVE bulk sync: it provisions
// every owned Fleet-API vehicle and NEVER clears a removed-vehicle tombstone, so
// an incidental re-link can't resurrect a car the owner deliberately removed
// (MYR-261 tombstone-wins). The deliberate re-add path (MYR-262) is the only one
// that clears a tombstone — see ReaddVehicle / VehicleReaddHandler.
func (h *ownerStreamHook) AfterLink(ctx context.Context, userID, accessToken string) {
	vehicles, err := h.lister.ListVehicles(ctx, accessToken)
	if err != nil {
		h.logger.Warn("owner stream setup: list vehicles failed (skipping)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return
	}

	for _, v := range vehicles {
		// Ownership filter (MYR-257 finding 3): never provision a car the caller
		// only shares (shared-driver access). Skip + audit non-owner vehicles.
		if !v.IsOwner() {
			h.logger.Info("owner_vehicle_skipped",
				slog.String("event", "owner_vehicle_skipped"),
				slog.String("user_id", userID),
				slog.String("reason", "not_owner"),
				slog.String("vin", redactVIN(v.VIN)))
			continue
		}
		h.provisionVehicle(ctx, userID, accessToken, v)
	}
}

// ReaddVehicle is the targeted re-provision behind the deliberate re-add
// (MYR-262): after VehicleReaddHandler clears the caller's tombstone, this
// provisions ONLY the single owned car matching teslaVehicleID, reusing the same
// per-vehicle path (provisionVehicle) the passive AfterLink sync uses. It is
// owner-filtered (an OWNER-access match is required) so a caller can never
// re-add a car they do not own even with a guessed id, and best-effort (returns
// false, never resurrecting anything, on any list/ownership/miss condition).
// Returns whether the car was provisioned.
func (h *ownerStreamHook) ReaddVehicle(ctx context.Context, userID, accessToken, teslaVehicleID string) bool {
	vehicles, err := h.lister.ListVehicles(ctx, accessToken)
	if err != nil {
		h.logger.Warn("owner re-add: list vehicles failed (tombstone cleared; retriable)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return false
	}
	for _, v := range vehicles {
		if v.ID.String() != teslaVehicleID {
			continue
		}
		if !v.IsOwner() {
			// The caller only shares this car — never attach someone else's car.
			h.logger.Info("owner_vehicle_skipped",
				slog.String("event", "owner_vehicle_skipped"),
				slog.String("user_id", userID),
				slog.String("reason", "not_owner"),
				slog.String("vin", redactVIN(v.VIN)))
			return false
		}
		return h.provisionVehicle(ctx, userID, accessToken, v)
	}
	h.logger.Warn("owner re-add: target not in caller fleet (tombstone cleared, nothing to provision)",
		slog.String("user_id", userID),
		slog.String("tesla_vehicle_id", teslaVehicleID))
	return false
}

// provisionVehicle seeds one OWNER-access vehicle's "Vehicle" row and, when a
// proxy is configured, best-effort pushes its fleet-telemetry config. It is the
// shared per-vehicle body of both the passive AfterLink sync and the deliberate
// ReaddVehicle path — the tombstone gate lives inside UpsertOwnedVehicle, so a
// still-tombstoned car is skipped here (returns false) regardless of caller.
// Returns whether the car was provisioned (owned/reconciled).
func (h *ownerStreamHook) provisionVehicle(ctx context.Context, userID, accessToken string, v telemetry.FleetVehicle) bool {
	vin := v.VIN
	outcome, err := h.upsert.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{
		UserID:         userID,
		TeslaVehicleID: v.ID.String(),
		VIN:            vin,
		Name:           v.DisplayName,
	})
	if err != nil {
		h.logger.Warn("owner stream setup: vehicle upsert failed (skipping vehicle)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
		return false
	}
	if outcome == store.VehicleSkippedTombstoned {
		// The owner deliberately removed this VIN (MYR-261 tombstone). A passive
		// re-link must NOT resurrect it — skip and do NOT push config for a car
		// the owner offboarded. Cleared only by a deliberate re-add (MYR-262).
		h.logger.Info("owner_vehicle_skipped",
			slog.String("event", "owner_vehicle_skipped"),
			slog.String("user_id", userID),
			slog.String("reason", "removed_tombstone"),
			slog.String("vin", redactVIN(vin)))
		return false
	}
	if outcome == store.VehicleSkippedCrossUser {
		// The teslaVehicleId already belongs to another user — never reassigned.
		// Audit and do NOT push config for a car we don't own.
		h.logger.Warn("owner_vehicle_skipped",
			slog.String("event", "owner_vehicle_skipped"),
			slog.String("user_id", userID),
			slog.String("reason", "cross_user_teslaVehicleId"),
			slog.String("vin", redactVIN(vin)))
		return false
	}
	h.logger.Info("owner_vehicle_owned",
		slog.String("event", "owner_vehicle_owned"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)))

	// MYR-517: the seed is UNCONDITIONAL and idempotent, and it is the last
	// thing this function does on every path that provisioned a car. See
	// seedSetupSchedule for why the push outcome may vary but the write may not.
	pushOutcome, applied := h.pushConfig(ctx, userID, accessToken, vin)
	h.seedSetupSchedule(ctx, userID, vin, pushOutcome)
	if applied {
		// MYR-529: the link-time push landed, which Tesla only does for an
		// enrolled virtual key. Signalled AFTER the seed, so the reconciler's
		// pairing reset lands on a row that exists rather than creating a
		// second one, and the epoch is stamped on the same row the card reads.
		h.signalPairing(userID, vin)
	}
	return true
}

// signalPairing hands proven pairing to the reconciler so a car that is not yet
// streaming gets its hot schedule from the very first door (MYR-529).
//
// Best-effort and non-blocking, like every other step in this hook: the
// notifier is a buffered inbox drained by the reconcile loop, and a deployment
// with no reconciler (no signing proxy) simply has nobody to tell.
func (h *ownerStreamHook) signalPairing(userID, vin string) {
	if h.pairing == nil {
		return
	}
	h.logger.Info("owner stream setup: link-time config push APPLIED — virtual key already paired, reconciler notified",
		slog.String("event", "fleet_config_pairing_at_link"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)))
	h.pairing.PairingEvidence(vin)
}
