// MYR-505 zero-touch setup completion: the sequence behind
// POST /api/tesla/vehicles/{vehicleId}/complete-setup (rest-api.md §7.23).
//
// THE ABSURDITY THIS DELETES. After MYR-489 the server can heal a car that was
// linked but never streamed, and after MYR-491 it can say so on the wire. What
// it could not do was START. The heal fires on a background pass (up to 45
// minutes) or on a signed command that APPLIED — which is why tester Amruth's
// live onboarding ended with someone telling him to tap Lock. A lock tap is not
// a setup step; it was a trigger wearing a control's clothes.
//
// The client knows the exact moment setup should complete: it handed the owner
// off to the Tesla app and gets `scenePhase` foreground back when they return.
// This is the server half of turning that moment into the trigger.
//
// THE SEQUENCE, AND WHY IT IS IN THIS ORDER.
//
//  1. STREAMING SHORT-CIRCUIT. If the car is already streaming, there is
//     nothing to complete. Answered from the row, with no Tesla call — which is
//     also what makes a second call after success cheap instead of another wake.
//  2. WAKE (unsigned, direct Fleet API). Before the pairing probe, because the
//     probe is fast and the wake is asynchronous — issuing it first buys the car
//     the whole probe budget to come up. It needs no virtual key, which is
//     essential: at this instant the key may not exist yet.
//  3. PROBE `fleet_status` (unsigned, direct Fleet API), bounded. Tesla's own
//     record of key enrollment. See the honesty note below.
//  4. ON PAIRED: reset the attempt schedule (opening a pairing epoch, so a
//     later reconciler pass is not buried under backoff earned before the key
//     existed) and run MYR-489's forced re-push through the signing proxy.
//
// THE HONESTY NOTE, which is the load-bearing design decision here. `not paired
// yet` and `asleep` are DIFFERENT ANSWERS and this endpoint never collapses one
// into the other. It can keep them apart because `fleet_status` is a Tesla-side
// record rather than a car query: a car that never wakes still yields a true
// answer about its pairing. So an expired wake never degrades into
// `awaiting_virtual_key` — a car whose key IS paired but which we could not
// reach reports `503 vehicle_asleep`, and a car Tesla says is unpaired reports
// `awaiting_virtual_key` whether or not it woke. Likewise, a probe we could not
// READ produces an ERROR, never a fabricated state: the one thing worse than no
// answer is a confident wrong one on a card whose whole job is naming the ONE
// thing left to do.
//
// THE READER/WRITER SPLIT IS PRESERVED. Wake and `fleet_status` are unsigned
// authenticated reads on the DIRECT Fleet API; the config push is the only
// call that goes through the tesla-http-proxy, and it goes there inside
// MYR-489's own code. Nothing in this file pushes config itself.

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Sentinel outcomes that are NOT a setup state. Each one is a case where the
// truthful answer is "we do not know" or "we could not act", and inventing a
// state for it would be the lie this endpoint exists to stop telling.
var (
	// ErrSetupProbeUnavailable — Tesla never answered the pairing probe within
	// the budget. Pairing is UNKNOWN; no state is claimed.
	ErrSetupProbeUnavailable = errors.New("fleet_status probe failed")
	// ErrSetupVehicleUnreachable — the key IS paired, but Tesla refused the
	// wake, so the car was not given a chance to act on a new config and none
	// was pushed at it.
	ErrSetupVehicleUnreachable = errors.New("vehicle could not be woken")
	// ErrSetupPushFailed — the key is paired and the forced config re-push did
	// not apply. The car may momentarily be unconfigured; the reconciler is
	// scheduled to repair it. Explicitly NOT `configuring`.
	ErrSetupPushFailed = errors.New("forced config re-push failed")
	// ErrSetupPushUnavailable — this deployment has no signing proxy, so no
	// config can be pushed at all (the same guard that keeps live pushes out of
	// dev and test).
	ErrSetupPushUnavailable = errors.New("config push is not configured")
)

// setupProbeReader is the DIRECT Fleet API surface this sequence needs: waking
// a car and asking Tesla whether its key is enrolled. Both are UNSIGNED
// authenticated calls; a proxy-backed client here would be a wiring bug.
// Consumer-site interface, satisfied by *FleetAPIClient.
type setupProbeReader interface {
	WakeVehicle(ctx context.Context, token, vin string) (*FleetVehicleState, error)
	GetFleetStatus(ctx context.Context, token string, vins []string) (*FleetStatusResponse, error)
}

// forcedConfigRepusher is MYR-489's escalation, invoked on demand. Satisfied by
// *FleetConfigReconciler; nil when the deployment has no signing proxy.
type forcedConfigRepusher interface {
	ForceConfigRepushNow(ctx context.Context, c FleetConfigCandidate, accessToken string) bool
}

// SetupCompleterDeps bundles the collaborators so the struct stays under the
// no-God-struct bar and cmd/ names each one at the call site.
type SetupCompleterDeps struct {
	// Vehicles resolves vehicleId → VIN, owner, status, freshness AND the
	// MYR-491 schedule row, in the one read the snapshot endpoint already does.
	Vehicles VehicleSnapshotReader
	// Tokens resolves (and refreshes) the owner's Tesla access token.
	Tokens teslaTokenResolver
	// Probe is the DIRECT Fleet API reader: wake + fleet_status.
	Probe setupProbeReader
	// Pairing opens a pairing epoch and clears the backoff, exactly as an
	// applied signed command does (MYR-489).
	Pairing FleetConfigPairingResetter
	// Repusher runs the MYR-489 forced re-push. Nil disables the push step.
	Repusher forcedConfigRepusher
	// Notifier tells the reconciler that pairing is proven, so the hot
	// schedule is armed even on the branches this endpoint cannot finish
	// itself (MYR-529). Nil disables the nudge.
	Notifier pairingEvidenceNotifier
}

// SetupCompleter runs the zero-touch completion sequence for one vehicle.
type SetupCompleter struct {
	deps   SetupCompleterDeps
	cfg    SetupCompletionConfig
	logger *slog.Logger
	now    func() time.Time
}

// NewSetupCompleter builds the completer. A nil Repusher is legitimate and
// means "this deployment cannot push config"; every other dependency is
// required.
func NewSetupCompleter(
	deps SetupCompleterDeps, cfg SetupCompletionConfig, logger *slog.Logger,
) *SetupCompleter {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &SetupCompleter{deps: deps, cfg: cfg.withDefaults(), logger: logger, now: time.Now}
}

// Complete runs the sequence for an ALREADY-AUTHORIZED vehicle row and returns
// the resulting setup state.
//
// The caller owns authentication, ownership and rate limiting; this method owns
// Tesla. It returns one of the sentinel errors above when no state can honestly
// be claimed.
func (s *SetupCompleter) Complete(ctx context.Context, row VehicleSnapshotRow) (SetupState, error) {
	vin := redactVIN(row.VIN)
	now := s.now()

	// (1) Nothing to complete. Free, and the reason a repeat call after success
	// costs no Tesla traffic at all.
	if isStreamingNow(row.Status, row.LastUpdated, now) {
		s.logger.Info("complete-setup: vehicle is already streaming",
			slog.String("event", "setup_complete_already_streaming"),
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		return *setupStateAt(SetupStateStreaming, row.LastUpdated, now), nil
	}

	tok, err := s.deps.Tokens.Resolve(ctx, row.UserID)
	if err != nil {
		// A real state, not an error: the owner must reconnect Tesla, and
		// MYR-491's card already has the copy for it.
		s.logger.Info("complete-setup: no usable Tesla token (owner must re-link)",
			slog.String("event", "setup_complete_token_failed"),
			slog.String("vehicle_id", row.ID), slog.String("vin", vin),
			slog.Bool("token_expired", errors.Is(err, ErrTeslaTokenExpired)))
		return *setupStateAt(SetupStateTokenFailed, now, now), nil
	}

	// (2) Wake. A failure is recorded and carried, NOT returned: pairing is
	// still knowable and is the more actionable answer for an owner who has not
	// paired. The failure only matters if it turns out we would have pushed.
	woken := s.wake(ctx, tok.AccessToken, row, vin)

	// (3) Probe.
	paired, err := s.probePairing(ctx, tok.AccessToken, row, vin)
	if err != nil {
		return SetupState{}, err
	}
	if !paired {
		s.logger.Info("complete-setup: Tesla reports the virtual key is not enrolled",
			slog.String("event", "setup_complete_awaiting_virtual_key"),
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		observed := s.now()
		return *setupStateAt(SetupStateAwaitingVirtualKey, observed, observed), nil
	}

	if !woken {
		// Paired, but we could not reach the car. Deliberately NOT
		// `awaiting_virtual_key` (the key is fine) and NOT `configuring` (we
		// pushed nothing). Transient — the SDK backs off and retries.
		//
		// The reconciler is told anyway (MYR-529): Tesla has just confirmed the
		// key, which is the one fact that arms the hot schedule, and a car we
		// could not wake this second is precisely the car a background pass
		// three minutes from now should be looking at.
		s.logger.Info("complete-setup: key is paired but the vehicle could not be woken; not pushing config",
			slog.String("event", "setup_complete_wake_refused"),
			slog.String("vehicle_id", row.ID), slog.String("vin", vin))
		s.notifyPairing(row.VIN, vin)
		return SetupState{}, ErrSetupVehicleUnreachable
	}

	st, err := s.configure(ctx, tok.AccessToken, row, vin)
	// AFTER configure, never before. Fired first, the loop's own reset and
	// reconcile could run against this vehicle while the sequence below is
	// mid-probe and mid-force, and the two would race for the epoch's single
	// escalation. Fired here, the reset the loop attempts is inside the
	// ten-minute pairing debounce that configure's own reset just refreshed, so
	// the common case is a no-op and the uncommon one (an epoch older than the
	// debounce, whose budget was already spent) is a legitimate new epoch.
	s.notifyPairing(row.VIN, vin)
	return st, err
}

// probePairing polls `fleet_status` until Tesla reports the key enrolled or the
// budget expires.
//
// THE ERROR BAR IS "NO CLEAN ANSWER AT ALL", not "the last probe failed".
// Tesla's read path is rate-limited and flaky under load, so one 429 inside a
// multi-probe budget should cost a retry rather than the owner's answer; and
// conversely, a probe that cleanly said `unpaired` IS evidence — an owner who
// has genuinely not finished the Tesla-app approval is far better served by
// `awaiting_virtual_key` and a retry affordance than by a generic upstream
// error. Only a budget in which we never once heard from Tesla is unknown.
func (s *SetupCompleter) probePairing(
	ctx context.Context, token string, row VehicleSnapshotRow, vin string,
) (bool, error) {
	deadline := s.now().Add(s.cfg.ProbeBudget)
	var lastErr error
	sawAnswer := false

	for probe := 1; probe <= s.cfg.MaxProbes; probe++ {
		if probe > 1 && !s.waitBeforeProbe(ctx, deadline) {
			break
		}

		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		status, err := s.deps.Probe.GetFleetStatus(callCtx, token, []string{row.VIN})
		cancel()

		if err != nil {
			lastErr = err
			s.logger.Info("complete-setup: pairing probe failed",
				slog.String("vehicle_id", row.ID), slog.String("vin", vin),
				slog.Int("probe", probe),
				slog.String("error", redactedErrorText(err)))
			continue
		}
		sawAnswer = true
		if status.KeyPaired(row.VIN) {
			s.logger.Info("complete-setup: virtual key confirmed enrolled",
				slog.String("event", "setup_complete_key_paired"),
				slog.String("vehicle_id", row.ID), slog.String("vin", vin),
				slog.Int("probe", probe))
			return true, nil
		}
	}

	if !sawAnswer {
		// Tesla never answered. Pairing is UNKNOWN, which is not the same fact
		// as unpaired and must not be reported as one.
		return false, fmt.Errorf("%w: %s", ErrSetupProbeUnavailable, redactedErrorText(lastErr))
	}
	return false, nil
}

// waitBeforeProbe sleeps between probes, and reports whether there is budget
// left to probe again. Cancellation ends the loop rather than erroring: the
// caller has hung up, and the partial answer is still true.
func (s *SetupCompleter) waitBeforeProbe(ctx context.Context, deadline time.Time) bool {
	if !s.now().Add(s.cfg.ProbeInterval).Before(deadline) {
		return false
	}
	timer := time.NewTimer(s.cfg.ProbeInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
