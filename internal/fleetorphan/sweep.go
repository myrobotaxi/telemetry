// Package fleetorphan finds Tesla fleet-telemetry configs that outlived the
// local "Vehicle" row they belonged to, and — under -apply — deletes the ones
// it can still authenticate.
//
// ── WHY THESE EXIST ─────────────────────────────────────────────────────────
//
// Tesla bills fleet-telemetry PER VEHICLE, and a config's `exp` is 350 days. A
// config whose car we deleted locally is invisible to us and still billable:
// the vehicle streams to a receiver that rejects it, forever, until the exp
// lapses. MYR-593 makes the teardown path delete the Tesla-side config going
// forward; this package clears the backlog that predates it.
//
// ── THE HONEST LIMIT: THERE IS NO PARTNER TOKEN ─────────────────────────────
//
// Every FleetAPIClient call in this repo authenticates with a per-call OWNER
// bearer token. There is no partner token, no client_credentials grant and no
// partner_accounts call anywhere in the codebase. So:
//
//   - A config belonging to an account we already DELETED is NOT reachable.
//     Its tokens were destroyed with the account. Those VINs are labelled
//     `unreachable_no_token`, and they keep streaming and billing until their
//     exp lapses. This tool reports them; it cannot fix them.
//   - What IS reachable: VINs whose owner still has a live Tesla link — a
//     mid-fleet car removal whose Tesla-side delete failed or was skipped, and
//     cars provisioned then removed. Plus the DB-only go_fleet_config_attempts
//     garbage, which needs no token at all.
//
// That split is the report's main product. Do not read the counts as "cost
// recovered" without reading `unreachable_no_token` alongside them.
//
// ── SHAPE ───────────────────────────────────────────────────────────────────
//
// Serial and best-effort throughout. One VIN's failure is a `failed` line and
// never aborts the run. Cancellation is honoured only BETWEEN VINs, after the
// previous one has finished, so a Ctrl-C cannot guillotine a delete mid-flight.
// Every collaborator is a consumer-site interface, so the whole sweep runs
// against fakes with no database and no network.
package fleetorphan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Deps are the sweep's collaborators. Deleter may be nil; the others may not.
type Deps struct {
	Store   Store
	Fleet   FleetLister
	Reader  ConfigReader
	Deleter ConfigDeleter
	Tokens  TokenSource
}

// Config tunes one run.
type Config struct {
	// Apply turns the dry run into a real one. FALSE IS THE DEFAULT AND THE
	// SAFE STATE: with Apply false nothing is written and no DELETE is issued,
	// at either Tesla or the database. The reads still happen — they are the
	// point.
	Apply bool
	// MaxTombstones caps the source-A read. Zero means defaultMaxTombstones.
	MaxTombstones int
	// CallTimeout bounds each individual Tesla call and each DB statement.
	CallTimeout time.Duration
}

const (
	defaultMaxTombstones = 500
	defaultCallTimeout   = 30 * time.Second
)

func (c Config) withDefaults() Config {
	if c.MaxTombstones <= 0 {
		c.MaxTombstones = defaultMaxTombstones
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultCallTimeout
	}
	return c
}

// Sweeper runs the one-off sweep.
type Sweeper struct {
	deps   Deps
	cfg    Config
	logger *slog.Logger
	// tokenCache memoises token resolution for the run. See cachedToken for
	// why the material is held at all and where it is guaranteed not to go.
	tokenCache map[string]cachedToken
}

// New builds a sweeper. A nil logger discards.
//
// A nil Deleter is legitimate and means exactly one thing: no delete can be
// issued. Combined with Apply=true it would be a lie, so Run downgrades such a
// run to a dry run rather than reporting deletions it did not perform.
func New(deps Deps, cfg Config, logger *slog.Logger) *Sweeper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Sweeper{
		deps:       deps,
		cfg:        cfg.withDefaults(),
		logger:     logger,
		tokenCache: make(map[string]cachedToken),
	}
}

// Run executes the sweep and returns the report. It returns an error only when
// nothing could be attempted at all (no Store); every other failure lands in
// the report, because a partial answer about production billing is worth far
// more than an aborted one.
func (s *Sweeper) Run(ctx context.Context) (Report, error) {
	if s.deps.Store == nil {
		return Report{}, errors.New("fleetorphan: a Store is required")
	}
	apply := s.cfg.Apply
	if apply && s.deps.Deleter == nil {
		s.logger.Warn("fleetorphan: -apply requested but no Tesla config deleter is wired; " +
			"running as a dry run so the report cannot claim deletions that did not happen")
		apply = false
	}

	rep := newReport(!apply)

	// SOURCE C FIRST, deliberately. It is the only half that needs no token and
	// no network, so it completes even when Tesla is unreachable or every owner
	// is gone — and an operator who Ctrl-Cs a slow Tesla pass still gets it.
	s.sweepAttempts(ctx, apply, &rep)

	candidates := s.collect(ctx, &rep)
	for _, c := range candidates {
		if ctx.Err() != nil {
			rep.Errors = append(rep.Errors,
				fmt.Sprintf("run cancelled with %d candidate VIN(s) unexamined", len(candidates)-len(rep.VINs)))
			break
		}
		out := s.resolveVIN(ctx, c, apply)
		rep.Counts[out.Outcome]++
		rep.VINs = append(rep.VINs, out)
	}

	s.logger.Info("orphan fleet-config sweep complete",
		slog.Bool("dry_run", rep.DryRun),
		slog.Int("candidate_vins", len(rep.VINs)),
		slog.Int("would_delete", rep.Counts[OutcomeWouldDelete]),
		slog.Int("deleted", rep.Counts[OutcomeDeleted]),
		slog.Int("no_config", rep.Counts[OutcomeNoConfig]),
		slog.Int("unreachable_no_token", rep.Counts[OutcomeUnreachableNoToken]),
		slog.Int("failed", rep.Counts[OutcomeFailed]),
		slog.Int("orphan_attempt_rows", len(rep.Attempts.OrphanRows)),
		slog.Int64("attempt_rows_deleted", rep.Attempts.Deleted),
	)
	return rep, nil
}

// sweepAttempts handles source C: go_fleet_config_attempts rows whose vehicle
// no longer exists. No VIN, so nothing to ask Tesla — pure DB garbage.
func (s *Sweeper) sweepAttempts(ctx context.Context, apply bool, rep *Report) {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	rows, err := s.deps.Store.ListOrphanFleetConfigAttempts(callCtx)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, "list orphan fleet-config attempts: "+errText(err))
		return
	}
	rep.Attempts.OrphanRows = rows
	if len(rows) == 0 || !apply {
		return
	}

	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CallTimeout)
	defer cancel()
	n, err := s.deps.Store.DeleteFleetConfigAttempts(delCtx, rows)
	if err != nil {
		rep.Errors = append(rep.Errors, "delete orphan fleet-config attempts: "+errText(err))
		return
	}
	rep.Attempts.Deleted = n
}

// resolveVIN decides and, under apply, acts on one candidate.
func (s *Sweeper) resolveVIN(ctx context.Context, c candidate, apply bool) VINOutcome {
	out := VINOutcome{VIN: c.VIN, UserID: c.UserID, Sources: c.Sources}
	log := s.logger.With(
		slog.String("vin", redactVIN(c.VIN)),
		slog.String("user_id", c.UserID),
	)

	if s.deps.Reader == nil {
		// Unwired rather than broken, but the honest label is still `failed`:
		// we made no claim about this VIN either way.
		out.Outcome = OutcomeFailed
		out.Detail = "no Fleet API config reader wired; the VIN was never examined"
		return out
	}

	token, err := s.token(ctx, c.UserID)
	if err != nil {
		if errors.Is(err, ErrNoToken) {
			// THE DELETED-ACCOUNT CASE. Nothing this tool can do.
			out.Outcome = OutcomeUnreachableNoToken
			out.Detail = "owner has no Tesla token on file; the config cannot be authenticated and will bill until its exp lapses"
			log.Warn("orphan fleet config is unreachable: no Tesla token for the owner")
			return out
		}
		out.Outcome = OutcomeFailed
		out.Detail = "resolve token: " + errText(err)
		log.Warn("orphan fleet config: token resolution failed", slog.String("error", errText(err)))
		return out
	}

	readCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	status, err := s.deps.Reader.GetTelemetryConfig(readCtx, token, c.VIN)
	cancel()
	switch {
	case errors.Is(err, ErrNoConfig):
		out.Outcome = OutcomeNoConfig
		return out
	case err != nil:
		out.Outcome = OutcomeFailed
		out.Detail = "read config: " + errText(err)
		log.Warn("orphan fleet config: read failed", slog.String("error", errText(err)))
		return out
	}
	out.ConfigExp = status.Exp
	if !status.Configured {
		out.Outcome = OutcomeNoConfig
		return out
	}

	if !apply {
		out.Outcome = OutcomeWouldDelete
		log.Info("orphan fleet config found (dry run; nothing deleted)")
		return out
	}

	// Detached from ctx: a delete that has started must finish or fail on its
	// own terms, not on a signal. The timeout still bounds it.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CallTimeout)
	defer cancel()
	if err := s.deps.Deleter.DeleteTelemetryConfig(delCtx, token, c.VIN); err != nil {
		if errors.Is(err, ErrNoConfig) {
			// Raced with something else removing it. Idempotent, not a failure.
			out.Outcome = OutcomeNoConfig
			return out
		}
		out.Outcome = OutcomeFailed
		out.Detail = "delete config: " + errText(err)
		log.Error("orphan fleet config: delete failed", slog.String("error", errText(err)))
		return out
	}
	out.Outcome = OutcomeDeleted
	log.Info("orphan fleet config deleted")
	return out
}
