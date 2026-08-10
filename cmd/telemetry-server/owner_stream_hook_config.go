package main

// The link-time fleet-config push half of ownerStreamHook: sending the config,
// classifying Tesla's answer, recording the schedule seed, and — since MYR-529
// — telling the reconciler when that answer was PROOF OF PAIRING.
//
// Split from owner_stream_hook.go for the CLAUDE.md 300-line file cap; the hook
// and this file are one component.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// pushConfig performs the link-time fleet-config push and returns the
// go_fleet_config_attempts.last_outcome label that describes what happened,
// plus whether the push APPLIED.
//
// SetupOutcomeNone (” — no claim) covers the three cases where there is
// nothing to say about this car yet: no signing proxy is configured, the VIN is
// not pushable, or the push APPLIED. The last one is the important one to read
// as "no claim" rather than as success-with-a-row: a config Tesla accepted is
// not evidence of anything the owner has to finish, and the wire derivation
// treats it accordingly.
//
// THE `applied` RETURN EXISTS BECAUSE THOSE THREE CASES ARE NOT THE SAME EVENT
// (MYR-529), even though they write the same label. Tesla applies a
// fleet-telemetry config only to a VIN whose virtual key is enrolled — an
// unpaired car is exactly what comes back as `missing_key` — so a link-time
// push that applied is PROOF OF PAIRING arriving at the earliest door there is.
// It used to be indistinguishable from "we did not push at all", so the one
// door that could have started the clock at second zero stayed silent.
func (h *ownerStreamHook) pushConfig(ctx context.Context, userID, accessToken, vin string) (string, bool) {
	if h.pusher == nil {
		return store.SetupOutcomeNone, false // proxy unconfigured — stream starts when ops/web pushes config
	}
	if len(vin) != vinLength {
		return store.SetupOutcomeNone, false // malformed VIN — nothing safe to push against
	}
	err := h.pusher.PushForVIN(ctx, accessToken, vin)
	if err == nil {
		return store.SetupOutcomeNone, true
	}
	// The EXPECTED outcome at link time: the owner cannot have paired the
	// virtual key yet, because pairing is a later, Tesla-app-side step. This
	// is not a fault and needs no operator attention — the MYR-448
	// reconciler re-pushes once pairing lands. Logged so the state is
	// visible rather than silent.
	var skipped *telemetry.SkippedVehicleError
	if errors.As(err, &skipped) && skipped.AwaitingVirtualKey() {
		h.logger.Info("owner stream setup: config not applied, awaiting virtual-key pairing (reconciler will re-push)",
			slog.String("event", "fleet_config_awaiting_virtual_key"),
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
			slog.String("reason", skipped.Reason))
		return store.SetupOutcomeAwaitingVirtualKey, false
	}
	h.logger.Warn("owner stream setup: fleet-config push failed (owner still linked; reconciler will retry)",
		slog.String("event", "fleet_config_push_failed"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)),
		slog.String("error", err.Error()))
	return store.SetupOutcomePushFailed, false
}

// seedSetupSchedule persists the link-time schedule row (MYR-491, widened by
// MYR-517) so the owner's vehicle card can name the pairing step immediately —
// rather than rendering empty until the reconciler's first pass reaches this
// car, up to Staleness + Interval later, which for MYR-503's tester would have
// been long after he gave up on the Lock button.
//
// WHY IT RUNS FOR EVERY OUTCOME AND NOT JUST `missing_key`. MYR-491 wrote this
// row only when Tesla answered the push with a missing virtual key, on the
// reading that any other answer meant there was nothing to record. Prod
// disagreed: Spencer White's car (linked 18:07:47Z on 2026-08-09) has no row at
// all, so nothing that keys off the row could see it — the in-band pairing
// signal no-opped when he finally sent a command, and the MYR-491 card had
// nothing to render while he sat watching a silent car. A row costs one
// bounded INSERT per provisioned vehicle and is scheduling- and wire-identical
// to no row; not having one costs the whole self-heal machinery its handle.
//
// BEST-EFFORT, LIKE EVERY OTHER STEP IN THIS HOOK. A failure to write a card's
// input must never surface to an owner who has just successfully linked their
// Tesla account, so this logs and returns.
func (h *ownerStreamHook) seedSetupSchedule(ctx context.Context, userID, vin, outcome string) {
	if err := h.upsert.SeedFleetConfigSchedule(ctx, vin, outcome, time.Now()); err != nil {
		h.logger.Warn("owner stream setup: could not record the setup schedule row (card and self-heal will fill in on the next reconcile pass)",
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
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
	result, err := p.client.PushTelemetryConfig(ctx, token, req)
	if err != nil {
		return err
	}
	// A 200 from Tesla does NOT mean the config was applied (MYR-448). An
	// unpaired car comes back as `skipped_vehicles: {vin: "missing_key"}` with
	// updated_vehicles: 0. Before this check that was recorded as success, so
	// the single automatic push in the whole system silently no-op'd for every
	// new owner — the config push necessarily runs during the OAuth callback,
	// which is always BEFORE the owner pairs the virtual key.
	return telemetry.SkipErrorFor(result, vin)
}
