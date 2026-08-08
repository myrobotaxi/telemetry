// Fleet-config reconciler — the recovery path for MYR-448.
//
// WHY THIS EXISTS. Self-serve onboarding pushes a car's fleet-telemetry config
// exactly once, from inside the Tesla OAuth callback (`ownerStreamHook.
// AfterLink`). At that instant the owner CANNOT have paired the virtual key —
// pairing is a later step, performed by hand in the Tesla app at
// tesla.com/_ak/myrobotaxi.app. Tesla therefore answers that push with HTTP
// 200 and `skipped_vehicles: {vin: "missing_key"}`, the config is not applied,
// and nothing in the system ever tries again. The car never streams. Every
// external beta owner hit this.
//
// docs/architecture/self-serve-onboarding.md §5 already specified the missing
// half — "The push is safe to attempt pre-pairing (Tesla no-ops / errors for
// an unpaired VIN); it is retried when pairing completes." — but no retry was
// ever built. This is that retry, and it is a RECONCILER rather than an
// event hook because there is no event to hang it on: pairing happens inside
// Tesla's app and our server is never told.
//
// SHAPE. A slow background loop. Each pass asks Tesla for the authoritative
// per-VIN config state and re-pushes only what is genuinely unconfigured, so a
// healthy fleet costs one cheap read per quiet car and zero writes.
//
// SCHEDULING LIVES IN OUR OWN TABLE, not in "Vehicle"."lastUpdated" — see
// migration 0030. Ordering candidates by a column the reconciler cannot change
// means every car it fails to fix sorts first forever and permanently occupies
// the per-pass limit. go_fleet_config_attempts both guarantees coverage and
// carries the exponential backoff that keeps an unpairable car from costing a
// signed POST every 15 minutes for the rest of time.
//
// SAFETY. The reconciler performs a state-changing call against a real car, so
// cmd/ constructs it ONLY when the tesla-http-proxy and telemetry endpoint are
// configured — the same runtime guard that keeps live pushes out of dev/test
// (self-serve-onboarding.md §5 SAFETY).

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Attempt outcome labels persisted to go_fleet_config_attempts.last_outcome.
// Internal, never wire-exposed, and deliberately NOT a raw Tesla body — those
// can echo a full VIN.
const (
	outcomeAwaitingKey  = "awaiting_virtual_key"
	outcomeSkippedOther = "skipped_other"
	outcomeReadFailed   = "read_failed"
	outcomePushFailed   = "push_failed"
	outcomeTokenFailed  = "token_failed"
	outcomeSyncedQuiet  = "synced_not_streaming"
)

// FleetConfigCandidate is one vehicle that may be missing its telemetry
// config. Declared here rather than imported so internal/telemetry stays
// decoupled from internal/store (the ride-poller precedent); cmd/ adapts the
// store row shape onto it.
type FleetConfigCandidate struct {
	VehicleID   string
	VIN         string
	UserID      string
	LastUpdated time.Time
	// AttemptCount is the number of consecutive unsuccessful attempts already
	// made against this vehicle, and is what the backoff is computed from.
	AttemptCount int
}

// FleetConfigCandidateLister is the reconciler's view of "which cars look like
// they are not streaming and are due another try". Satisfied by
// *store.VehicleRepo via a cmd/ adapter.
type FleetConfigCandidateLister interface {
	ListFleetConfigCandidates(ctx context.Context, cutoff, now time.Time, limit int) ([]FleetConfigCandidate, error)
}

// FleetConfigAttemptRecorder persists the per-vehicle retry schedule.
type FleetConfigAttemptRecorder interface {
	RecordFleetConfigAttempt(ctx context.Context, vehicleID string, attemptedAt, nextAttemptAt time.Time, outcome string) error
	ClearFleetConfigAttempts(ctx context.Context, vehicleID string) error
}

// fleetTelemetryConfigReader reads a VIN's current config state. This is an
// UNSIGNED authenticated read and MUST target the direct Fleet API, never the
// signing proxy (see GetVehicle's contract note).
type fleetTelemetryConfigReader interface {
	GetTelemetryConfig(ctx context.Context, token, vin string) (*FleetConfigStatusResponse, error)
}

// fleetTelemetryConfigWriter pushes a config. This one DOES go through the
// tesla-http-proxy, which signs the config into a JWS.
type fleetTelemetryConfigWriter interface {
	PushTelemetryConfig(ctx context.Context, token string, req FleetConfigRequest) (*FleetConfigResponse, error)
}

// FleetConfigReconcilerDeps bundles the collaborators so the struct stays
// under the no-God-struct bar and cmd/ names each one at the call site.
type FleetConfigReconcilerDeps struct {
	// Candidates is the DB view of cars that have gone quiet and are due.
	Candidates FleetConfigCandidateLister
	// Attempts persists the retry schedule and backoff.
	Attempts FleetConfigAttemptRecorder
	// Reader reads authoritative config state from the direct Fleet API.
	Reader fleetTelemetryConfigReader
	// Writer pushes the config through the signing proxy.
	Writer fleetTelemetryConfigWriter
	// Tokens resolves (and refreshes) each owner's Tesla access token.
	Tokens teslaTokenResolver
}

// FleetConfigReconciler heals vehicles that were provisioned but never
// configured to stream.
type FleetConfigReconciler struct {
	deps     FleetConfigReconcilerDeps
	cfg      FleetConfigReconcileConfig
	endpoint EndpointConfig
	logger   *slog.Logger
	now      func() time.Time
}

// NewFleetConfigReconciler builds a reconciler. Every dependency is required;
// cmd/ guards construction so a deployment missing one keeps the pre-MYR-448
// behaviour rather than running a half-wired loop.
func NewFleetConfigReconciler(
	deps FleetConfigReconcilerDeps,
	cfg FleetConfigReconcileConfig,
	endpoint EndpointConfig,
	logger *slog.Logger,
) *FleetConfigReconciler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &FleetConfigReconciler{
		deps:     deps,
		cfg:      cfg.withDefaults(),
		endpoint: endpoint,
		logger:   logger,
		now:      time.Now,
	}
}

// ReconcileOutcome counts what one pass did, for the completion log line and
// for tests to assert against.
type ReconcileOutcome struct {
	Examined      int
	AlreadySynced int
	Repaired      int
	AwaitingKey   int
	SkippedOther  int
	TokenFailures int
	ReadFailures  int
	PushFailures  int
	// Truncated is set when the pass ended early (shutdown or an expired
	// budget) rather than after examining every candidate — without it a pass
	// that did almost nothing logs identically to a complete one.
	Truncated bool
}

// Reconcile runs ONE pass: list quiet, due cars, ask Tesla what each one's
// config state actually is, and re-push the config for those that have none.
//
// A LIST failure returns the error and changes nothing — a DB blip must never
// read as "no cars need healing". Every PER-CAR failure is logged, scheduled
// for a backed-off retry, and the pass continues, so one owner's expired token
// cannot stall the whole fleet.
func (r *FleetConfigReconciler) Reconcile(ctx context.Context) (ReconcileOutcome, error) {
	var out ReconcileOutcome

	now := r.now()
	cutoff := now.Add(-r.cfg.Staleness)
	candidates, err := r.deps.Candidates.ListFleetConfigCandidates(ctx, cutoff, now, r.cfg.MaxPerPass)
	if err != nil {
		return out, fmt.Errorf("FleetConfigReconciler.Reconcile: list candidates: %w", err)
	}
	if len(candidates) == 0 {
		return out, nil
	}

	r.logger.Info("fleet-config reconcile: pass starting",
		slog.Int("candidates", len(candidates)),
		slog.Duration("staleness", r.cfg.Staleness))

	for _, c := range candidates {
		// Shutdown ends the pass and returns what was achieved so far. Phrased
		// as a Done receive rather than a ctx.Err() test so that abandoning the
		// remaining candidates reads as the ordinary control flow it is, not as
		// an error being swallowed.
		select {
		case <-ctx.Done():
			out.Truncated = true
			r.logPassComplete(out)
			return out, nil
		default:
		}
		out.Examined++
		r.reconcileOne(ctx, c, &out)
	}

	r.logPassComplete(out)
	return out, nil
}

func (r *FleetConfigReconciler) logPassComplete(out ReconcileOutcome) {
	r.logger.Info("fleet-config reconcile: pass complete",
		slog.Int("examined", out.Examined),
		slog.Int("already_synced", out.AlreadySynced),
		slog.Int("repaired", out.Repaired),
		slog.Int("awaiting_virtual_key", out.AwaitingKey),
		slog.Int("skipped_other", out.SkippedOther),
		slog.Int("token_failures", out.TokenFailures),
		slog.Int("read_failures", out.ReadFailures),
		slog.Int("push_failures", out.PushFailures),
		slog.Bool("truncated", out.Truncated))
}

// reconcileOne heals a single candidate. It never returns an error: every
// outcome is a counted, logged, rescheduled fact so a single bad car cannot
// abort the pass.
func (r *FleetConfigReconciler) reconcileOne(ctx context.Context, c FleetConfigCandidate, out *ReconcileOutcome) {
	vin := redactVIN(c.VIN)

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.CallTimeout)
	defer cancel()

	tok, err := r.deps.Tokens.Resolve(callCtx, c.UserID)
	if err != nil {
		out.TokenFailures++
		// Expected and unactionable for an owner who unlinked or whose refresh
		// token died — Info, not Warn, so it does not read as a server fault.
		r.logger.Info("fleet-config reconcile: no usable Tesla token (owner must re-link)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.Bool("token_expired", errors.Is(err, ErrTeslaTokenExpired)))
		r.reschedule(ctx, c, outcomeTokenFailed)
		return
	}

	status, err := r.deps.Reader.GetTelemetryConfig(callCtx, tok.AccessToken, c.VIN)
	if err != nil {
		out.ReadFailures++
		// Do NOT fall through to a blind push: the read is the cheap, safe
		// call, and if it is failing the push is unlikely to fare better.
		r.logger.Warn("fleet-config reconcile: config read failed (will retry after backoff)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		r.reschedule(ctx, c, outcomeReadFailed)
		return
	}

	if status != nil && status.Response.Synced {
		out.AlreadySynced++
		// The car IS configured yet its row has gone quiet. That is a genuinely
		// different failure (asleep, or an mTLS/cert/reachability problem) and
		// it used to be invisible. Say so out loud — this is the line that
		// tells an operator to stop looking at fleet-config.
		r.logger.Info("fleet-config reconcile: config already synced but vehicle is quiet",
			slog.String("event", "fleet_config_synced_not_streaming"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.Time("last_updated", c.LastUpdated))
		// Backed off like any other non-repair: re-reading a parked car every
		// interval forever is exactly the waste the schedule exists to stop.
		r.reschedule(ctx, c, outcomeSyncedQuiet)
		return
	}

	r.push(callCtx, ctx, c, tok.AccessToken, vin, out)
}

// push sends the config for one unconfigured VIN and classifies the answer.
//
// callCtx bounds the Tesla call; schedCtx is the pass context used for the
// bookkeeping write, so an attempt is still recorded when the per-vehicle
// budget is what expired.
func (r *FleetConfigReconciler) push(
	callCtx, schedCtx context.Context,
	c FleetConfigCandidate,
	accessToken, vin string,
	out *ReconcileOutcome,
) {
	result, err := r.deps.Writer.PushTelemetryConfig(callCtx, accessToken, r.request(c.VIN))
	if err != nil {
		out.PushFailures++
		r.logger.Warn("fleet-config reconcile: push failed (will retry after backoff)",
			slog.String("event", "fleet_config_push_failed"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", redactedErrorText(err)))
		r.reschedule(schedCtx, c, outcomePushFailed)
		return
	}

	// THE BUG THIS RECONCILER EXISTS FOR: a 200 does not mean it applied.
	if skipErr := SkipErrorFor(result, c.VIN); skipErr != nil {
		var skipped *SkippedVehicleError
		if errors.As(skipErr, &skipped) && skipped.AwaitingVirtualKey() {
			out.AwaitingKey++
			// Not a fault — the owner has genuinely not paired yet. Logged at
			// Info every pass so "which testers are still unpaired" is a log
			// query rather than a mystery.
			r.logger.Info("fleet-config reconcile: awaiting virtual-key pairing",
				slog.String("event", "fleet_config_awaiting_virtual_key"),
				slog.String("vehicle_id", c.VehicleID),
				slog.String("vin", vin),
				slog.String("reason", skipped.Reason))
			r.reschedule(schedCtx, c, outcomeAwaitingKey)
			return
		}
		out.SkippedOther++
		r.logger.Warn("fleet-config reconcile: Tesla skipped the vehicle for an unrecognised reason",
			slog.String("event", "fleet_config_skipped_unknown"),
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", vin),
			slog.String("error", skipErr.Error()))
		r.reschedule(schedCtx, c, outcomeSkippedOther)
		return
	}

	out.Repaired++
	// The headline event: a car that was silently unconfigured is now told to
	// stream. Error level would overstate it; this is a repair, and it should
	// be trivially greppable.
	r.logger.Info("fleet-config reconcile: config pushed, vehicle should now stream",
		slog.String("event", "fleet_config_repaired"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", vin),
		slog.Int("updated_vehicles", result.Response.UpdatedVehicles))

	// Success clears the schedule: the car should now stream, and if it ever
	// goes quiet again it deserves a fresh count rather than an old backoff.
	if err := r.deps.Attempts.ClearFleetConfigAttempts(schedCtx, c.VehicleID); err != nil {
		r.logger.Warn("fleet-config reconcile: could not clear attempt schedule",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
	}
}

// reschedule records an unsuccessful attempt and pushes the vehicle's next
// eligibility out by the backoff for its attempt count.
//
// A bookkeeping failure is logged, never fatal — but it does matter: without
// the row the car stays permanently due, so the pass would retry it at full
// rate. That is the pre-backoff behaviour, i.e. degraded but not broken.
func (r *FleetConfigReconciler) reschedule(ctx context.Context, c FleetConfigCandidate, outcome string) {
	now := r.now()
	next := now.Add(r.cfg.backoffFor(c.AttemptCount))
	if err := r.deps.Attempts.RecordFleetConfigAttempt(ctx, c.VehicleID, now, next, outcome); err != nil {
		r.logger.Warn("fleet-config reconcile: could not record attempt schedule (vehicle stays due)",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
	}
}

// request builds the same config body the link hook, the REST handler and
// `ops fleet-config push` send, so a reconciled car is configured identically
// to a hand-pushed one.
func (r *FleetConfigReconciler) request(vin string) FleetConfigRequest {
	var ca *string
	if r.endpoint.CA != "" {
		ca = &r.endpoint.CA
	}
	// Tesla requires exp between ~31 and ~360 days from now.
	exp := r.now().Add(350 * 24 * time.Hour).Unix()
	return FleetConfigRequest{
		VINs: []string{vin},
		Config: FleetConfig{
			Hostname:   r.endpoint.Hostname,
			Port:       r.endpoint.Port,
			CA:         ca,
			Fields:     DefaultFieldConfig(),
			AlertTypes: []string{"service"},
			Exp:        &exp,
		},
	}
}
