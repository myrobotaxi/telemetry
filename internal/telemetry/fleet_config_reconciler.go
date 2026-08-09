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
// migration 0031. Ordering candidates by a column the reconciler cannot change
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
	// The remaining fields carry the MYR-489 escalation state. They are read
	// only by the synced-not-streaming path; every other outcome ignores them.
	//
	// LastOutcome/LastAttemptAt describe the PREVIOUS attempt ('' and zero for
	// a car never attempted). SignedCommandAt is when a signed command last
	// applied to this car — proof of pairing, and the pairing epoch's start.
	// ForcedRepushAt is when the escalation last fired, compared against
	// SignedCommandAt to allow exactly one force per epoch.
	LastOutcome     string
	LastAttemptAt   time.Time
	SignedCommandAt time.Time
	ForcedRepushAt  time.Time
	// ScheduleCreated is set when the schedule row was CREATED by the pairing
	// reset that produced this candidate, rather than reset in place (MYR-517).
	// A created row means we had never recorded a single thing about this car,
	// which is not evidence that it is broken — see handlePairingSignal.
	ScheduleCreated bool
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
	// RecordForcedFleetConfigRepush records an attempt AND spends the pairing
	// epoch's one escalation (MYR-489).
	RecordForcedFleetConfigRepush(ctx context.Context, vehicleID string, attemptedAt, nextAttemptAt time.Time, outcome string) error
}

// fleetTelemetryConfigReader reads a VIN's current state from Tesla — its
// config, and (for the MYR-489 awake check) its connectivity. Both are
// UNSIGNED authenticated reads and MUST target the direct Fleet API, never the
// signing proxy (see GetVehicle's contract note).
type fleetTelemetryConfigReader interface {
	GetTelemetryConfig(ctx context.Context, token, vin string) (*FleetConfigStatusResponse, error)
	fleetVehicleStateReader
}

// fleetTelemetryConfigWriter mutates a VIN's config. Both calls go to the
// tesla-http-proxy: the POST because the proxy signs the config into a JWS, the
// DELETE because it carries no body to sign and rides the same client's
// plain-forward path.
type fleetTelemetryConfigWriter interface {
	PushTelemetryConfig(ctx context.Context, token string, req FleetConfigRequest) (*FleetConfigResponse, error)
	fleetTelemetryConfigDeleter
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
	// Pairing opens a new key-pairing epoch when a signed command proves the
	// virtual key exists (MYR-489).
	Pairing FleetConfigPairingResetter
}

// FleetConfigReconciler heals vehicles that were provisioned but never
// configured to stream.
type FleetConfigReconciler struct {
	deps     FleetConfigReconcilerDeps
	cfg      FleetConfigReconcileConfig
	endpoint EndpointConfig
	logger   *slog.Logger
	now      func() time.Time
	// pairing is the inbox for applied-signed-command signals (MYR-489),
	// drained by RunReconcileLoop in the same select as the periodic tick so a
	// signal can never race a pass.
	pairing chan string
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
		pairing:  make(chan string, pairingSignalBuffer),
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
	// ForcedRepushes counts MYR-489 escalations that applied: a
	// synced-but-quiet car, proven awake, whose config was deleted and
	// recreated to make its firmware re-establish the telemetry session.
	ForcedRepushes int
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

	// Indexed rather than ranged by value: the candidate now carries the
	// escalation timestamps and is large enough that copying it per iteration
	// is wasteful (gocritic rangeValCopy).
	for i := range candidates {
		c := candidates[i]
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
		slog.Int("forced_repushes", out.ForcedRepushes),
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
		// The car IS configured yet its row has gone quiet. MYR-448 logged this
		// and backed off unconditionally; MYR-489 makes it a decision, because
		// prod proved the "it must just be asleep" theory wrong. See
		// fleet_config_force_repush.go — the default is still the backoff.
		r.handleSyncedQuiet(callCtx, ctx, c, tok.AccessToken, vin, out)
		return
	}

	r.push(callCtx, ctx, c, tok.AccessToken, vin, out)
}
