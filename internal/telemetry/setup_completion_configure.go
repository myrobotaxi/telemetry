// Step 4 of the MYR-505 sequence: turning a confirmed virtual key into a car
// that is actually being configured.
//
// Split from setup_completion.go purely for the CLAUDE.md 300-line file cap;
// the sequence and this file are one component, exactly as
// fleet_config_reconciler.go and fleet_config_push.go are.
//
// NOTHING HERE PUSHES CONFIG ITSELF. Every line below is about deciding
// WHETHER the MYR-489 escalation may run and handing it a candidate in the
// shape a background pass would have produced; the delete-then-create, the
// classification of Tesla's answer and the schedule bookkeeping all happen
// inside `forceRepush`, unchanged and unforked.

package telemetry

import (
	"context"
	"log/slog"
)

// configure performs step 4: reset the schedule, then spend the pairing epoch's
// one forced re-push.
//
// THE ORDER OF THE BUDGET CHECK MATTERS AND IS NOT OBVIOUS. The epoch is read
// BEFORE the schedule reset, because the reset stamps `signed_command_at = now`
// and would therefore RE-ARM the very budget being tested. Checked in this
// order, a second complete-setup call inside the same epoch finds the budget
// spent and pushes nothing — which is what makes the endpoint idempotent in the
// only sense that matters to a car.
//
// The row is RE-READ here rather than reused from the top of Complete: up to
// ProbeBudget has passed, and the reconciler's own pass may have escalated this
// very vehicle in the meantime. Re-reading is what stops the two paths from
// each spending the same epoch.
func (s *SetupCompleter) configure(
	ctx context.Context, token string, row VehicleSnapshotRow, vin string,
) (SetupState, error) {
	if s.deps.Repusher == nil {
		return SetupState{}, ErrSetupPushUnavailable
	}

	c := s.candidate(ctx, row)
	if epochForceSpent(c) {
		// Already done for this epoch — by an earlier call, or by the
		// reconciler. Report the state that spend produced instead of spending
		// it again.
		s.logger.Info("complete-setup: forced re-push already spent for this pairing epoch",
			slog.String("event", "setup_complete_push_already_spent"),
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		return *setupStateAt(SetupStateConfiguring, c.ForcedRepushAt, s.now()), nil
	}

	s.resetSchedule(ctx, row, vin, &c)

	if !s.deps.Repusher.ForceConfigRepushNow(ctx, c, token) {
		// forceRepush has already logged the failure — including the loud
		// UNCONFIGURED event when the delete landed and the create did not —
		// and scheduled the repair. Saying `configuring` here would claim a
		// success that did not happen.
		return SetupState{}, ErrSetupPushFailed
	}

	now := s.now()
	s.logger.Info("complete-setup: config re-pushed on demand; vehicle should now connect",
		slog.String("event", "setup_complete_configured"),
		slog.String("vehicle_id", row.ID), slog.String("vin", vin))
	return *setupStateAt(SetupStateConfiguring, now, now), nil
}

// resetSchedule clears the backoff and opens a pairing epoch, via the SAME
// write an applied signed command uses (MYR-489).
//
// A miss is normal and not an error: the VIN is unknown, or the reset was
// debounced because a signed command opened this epoch minutes ago. In both
// cases the candidate we already hold is the right one to push with — `c` keeps
// whatever epoch it had.
//
// "NO SCHEDULE ROW" USED TO BE A THIRD KIND OF MISS, AND IT LEFT THIS ENDPOINT
// ABLE TO FIRE EXACTLY ONCE PER CAR, FOREVER (MYR-517). The write was an UPDATE,
// so a car with no row got no epoch stamp — and then the forced re-push below
// recorded `forced_repush_at` against a NULL `signed_command_at`. Read back,
// that row says `epochForceSpent` = true (a force exists and does not predate a
// pairing epoch, because there is no pairing epoch), which is a latch: every
// later call answers `configuring` from a spend that may have accomplished
// nothing, and the reconciler's escalation is blocked on the same predicate.
// The store write is now an upsert, so the epoch is stamped before the force
// and the budget re-arms the way it was designed to.
func (s *SetupCompleter) resetSchedule(
	ctx context.Context, row VehicleSnapshotRow, vin string, c *FleetConfigCandidate,
) {
	fresh, found, err := s.deps.Pairing.ResetFleetConfigScheduleOnPairing(
		ctx, row.VIN, s.now(), s.now().Add(-pairingResetDebounce))
	if err != nil {
		s.logger.Warn("complete-setup: could not reset the attempt schedule",
			slog.String("vehicle_id", row.ID), slog.String("vin", vin),
			slog.String("error", err.Error()))
		return
	}
	if !found {
		return
	}
	s.logger.Info("complete-setup: attempt schedule reset, pairing epoch opened",
		slog.String("event", "setup_complete_schedule_reset"),
		slog.String("vehicle_id", row.ID), slog.String("vin", vin),
		slog.String("previous_outcome", fresh.LastOutcome))
	*c = fresh
}

// candidate re-reads the vehicle and projects it onto the reconciler's own
// candidate type, so the push path receives exactly the shape a background pass
// would have handed it. A failed re-read falls back to the row we already have
// — degraded (a stale epoch reading can only cost one extra push) rather than
// an outright refusal to finish the owner's setup.
func (s *SetupCompleter) candidate(ctx context.Context, row VehicleSnapshotRow) FleetConfigCandidate {
	if fresh, err := s.deps.Vehicles.GetByID(ctx, row.ID); err == nil {
		row = fresh
	} else {
		s.logger.Warn("complete-setup: could not re-read the schedule row; using the pre-probe read",
			slog.String("vehicle_id", row.ID), slog.String("error", err.Error()))
	}
	return FleetConfigCandidate{
		VehicleID:       row.ID,
		VIN:             row.VIN,
		UserID:          row.UserID,
		LastUpdated:     row.LastUpdated,
		LastOutcome:     row.SetupSchedule.LastOutcome,
		LastAttemptAt:   row.SetupSchedule.LastAttemptAt,
		SignedCommandAt: row.SetupSchedule.SignedCommandAt,
		ForcedRepushAt:  row.SetupSchedule.ForcedRepushAt,
	}
}

// pairingEvidenceNotifier receives "Tesla says this VIN's virtual key is
// enrolled" (MYR-529). Satisfied by *FleetConfigReconciler; nil when the
// deployment has no reconciler at all.
//
// It is a NON-BLOCKING hand-off into the reconcile loop's inbox, which is why
// this endpoint can afford to fire it on paths where it has just done nothing
// itself — a paired car whose wake was refused, or one whose epoch budget was
// already spent. Those are exactly the answers that used to leave the schedule
// standing, and they are now the ones that arm the hot ladder.
type pairingEvidenceNotifier interface {
	PairingEvidence(vin string)
}

// notifyPairing hands Tesla's paired answer to the reconciler. Best-effort and
// non-blocking by contract; a deployment with no reconciler simply has no one
// to tell.
func (s *SetupCompleter) notifyPairing(rawVIN, vin string) {
	if s.deps.Notifier == nil {
		return
	}
	s.logger.Info("complete-setup: handing proven pairing to the reconciler",
		slog.String("event", "setup_complete_pairing_signalled"),
		slog.String("vin", vin))
	s.deps.Notifier.PairingEvidence(rawVIN)
}
