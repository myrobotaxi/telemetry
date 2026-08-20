package fleetsuspend

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// One pass and the per-vehicle policy. Split from sweeper.go to keep both files
// inside the 300-line cap.

// decision is what one pass did about one vehicle.
type decision int

const (
	// decisionNone: the vehicle is inside the warning threshold, or its
	// episode has already been warned and it has not reached suspension.
	decisionNone decision = iota
	// decisionWarned: the day-4 push was published, once for this episode.
	decisionWarned
	// decisionSuspended: the config was removed at Tesla and stamped.
	decisionSuspended
	// decisionHeld: something was unreadable or unwired. NOTHING was changed
	// and the next pass re-decides. Every failure lands here — an unknown state
	// must never advance a vehicle toward an irreversible action.
	decisionHeld
)

// Result counts one pass. Returned rather than accumulated in place so the
// per-vehicle policy stays free of shared mutable state and so a test can
// assert on a pass without a database.
type Result struct {
	Reset      int64
	Candidates int
	Warned     int
	Suspended  int
	Held       int
	// Keepalive is the MYR-594 arm's own tally. Zero-valued when the arm is
	// unwired or the kill switch is off.
	Keepalive KeepaliveResult
}

func (r *Result) count(d decision) {
	switch d {
	case decisionWarned:
		r.Warned++
	case decisionSuspended:
		r.Suspended++
	case decisionHeld:
		r.Held++
	case decisionNone:
	}
}

// SweepOnce runs one pass: reset the episodes of returned owners, read the
// candidates, and decide each one.
//
// SERIAL, not concurrent, and that is a deliberate difference from the
// reservation sweeper it otherwise copies. That worker races a rider's pickup
// time and has to fan out; this one has an hour to do a day's work. Serial
// execution means the Tesla calls arrive at a knowable rate rather than
// MaxConcurrent at a time, which matters because they are signed calls against
// a rate-limited path, and it means the pass log's ordering is the order things
// happened.
func (s *Sweeper) SweepOnce(ctx context.Context) Result {
	now := s.now()
	warnBefore := now.Add(-s.cfg.WarnAfter)
	suspendBefore := now.Add(-s.cfg.SuspendAfter)

	var res Result

	// FIRST, ALWAYS: an owner who came back ends their episode. Running this
	// before the candidate read is what makes "activity after the warning but
	// before suspension resets the episode" true rather than nearly true — the
	// alternative leaves a stale warned_at that would suppress the NEXT
	// episode's warning entirely, so the owner's next absence would suspend
	// their car with no notice at all.
	resetCtx, cancel := context.WithTimeout(ctx, s.cfg.PassTimeout)
	reset, err := s.store.ClearReturnedOwnerEpisodes(resetCtx, warnBefore)
	cancel()
	if err != nil {
		// Hold the whole pass. Proceeding on a failed reset risks re-warning
		// people who are demonstrably back, which is the noisiest failure this
		// feature has.
		s.logger.Error("telemetry sweep: episode reset failed; holding the pass",
			slog.String("error", err.Error()))
		return res
	}
	res.Reset = reset

	listCtx, cancel := context.WithTimeout(ctx, s.cfg.PassTimeout)
	candidates, err := s.store.ListInactiveOwnerVehicles(listCtx, warnBefore, s.cfg.MaxPerSweep)
	cancel()
	if err != nil {
		s.logger.Error("telemetry sweep: candidate read failed",
			slog.String("error", err.Error()))
		return res
	}
	res.Candidates = len(candidates)

	for i := range candidates {
		if ctx.Err() != nil {
			// Shutdown, honoured at the only free point: between vehicles,
			// before anything irreversible has started for this one.
			res.Held += len(candidates) - i
			return res
		}
		res.count(s.handle(ctx, &candidates[i], now, suspendBefore))
	}

	// LAST, and only on a pass that got this far. The warn and suspend arms are
	// the irreversible ones and must not queue behind a run of Tesla OAuth
	// calls; and a pass that already held on an unreadable episode reset or
	// candidate list has told us the database is sick, which is not the moment
	// to start spending single-use credentials against it.
	res.Keepalive = s.KeepaliveOnce(ctx)

	if res.Candidates > 0 || res.Reset > 0 {
		s.logger.Info("telemetry inactivity sweep",
			slog.Int64("episodes_reset", res.Reset),
			slog.Int("candidates", res.Candidates),
			slog.Int("warned", res.Warned),
			slog.Int("suspended", res.Suspended),
			slog.Int("held", res.Held),
		)
	}
	return res
}

// handle decides one vehicle.
//
// SUSPENSION IS CHECKED FIRST. A server that was down over an owner's fourth day
// wakes up to a car past BOTH thresholds; warning it and waiting another hour
// would be a notice with no notice period, which is worse than the honest
// outcome — the threshold is what suspends, not the warning, and the owner's
// in-app disconnect notice is the backstop the contract requires either way.
// That is also why the episode row tolerates (NULL warned_at, non-NULL
// suspended_at).
func (s *Sweeper) handle(ctx context.Context, c *Candidate, now, suspendBefore time.Time) decision {
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.VehicleTimeout)
	defer cancel()

	if !c.OwnerLastSeenAt.After(suspendBefore) {
		// ONE EXCEPTION to "suspension is checked first", and it exists so this
		// package's own promise is literally true rather than nearly true. On a
		// deploy with no fleet-config path wired, `suspend` can only HOLD — so a
		// car first observed past BOTH thresholds would fall straight through
		// the warn arm into a permanent hold and its owner would never hear
		// anything, on a server whose doc header says it "still WARNS but never
		// suspends". Warning is the only thing such a deploy CAN do, it is
		// strictly better than silence, and the next pass finds warned_at set
		// and holds exactly as before.
		if !s.canSuspend() && c.WarnedAt == nil {
			return s.warn(workCtx, c, now)
		}
		return s.suspend(workCtx, c, now)
	}
	if c.WarnedAt == nil {
		return s.warn(workCtx, c, now)
	}
	// Warned, not yet due. Nothing to do until the fifth day.
	return decisionNone
}

// canSuspend reports whether a Tesla config delete is reachable at all.
//
// Both halves are required and neither substitutes for the other: with no
// remover there is no call to make, and with no token source there is nothing to
// authenticate it with.
func (s *Sweeper) canSuspend() bool { return s.configs != nil && s.tokens != nil }

// warn publishes the day-4 notice, once per episode.
//
// THE MARK IS WRITTEN BEFORE THE PUBLISH, and the order is the client's
// requirement rather than a preference. The bus is fire-and-forget, so a publish
// tells us nothing about delivery; if the mark came second and failed, the next
// pass would warn again, and the pass after that, once an hour until suspension
// — twenty-four identical banners. In the other order a failure loses at most
// one warning for one episode, and the owner still gets the in-app disconnect
// notice the contract requires. Spamming is the worse outcome and it is the one
// this order makes impossible.
func (s *Sweeper) warn(ctx context.Context, c *Candidate, now time.Time) decision {
	if err := s.store.MarkWarned(ctx, c.VehicleID, now); err != nil {
		s.logger.Error("telemetry sweep: could not record the warning; not sending one",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
		return decisionHeld
	}

	if s.bus == nil {
		// Ordinary unwired state (tests, and any deploy without a bus). The
		// episode is still marked, so a bus that appears later does not
		// retroactively fire a warning for an episode already past its notice
		// period.
		return decisionWarned
	}

	evt := events.NewEvent(events.VehicleTelemetryWarningEvent{
		OwnerID:     c.OwnerID,
		VehicleID:   c.VehicleID,
		VehicleName: c.VehicleName,
		SuspendAt:   c.OwnerLastSeenAt.Add(s.cfg.SuspendAfter),
	})
	if err := s.bus.Publish(ctx, evt); err != nil {
		// Logged and dropped, like every other fire-and-forget publish in this
		// codebase. The mark stands: see the ordering argument above.
		s.logger.Warn("telemetry sweep: warning publish failed",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
	}
	s.logger.Info("telemetry inactivity warning",
		slog.String("vehicle_id", c.VehicleID),
		slog.String("user_id", c.OwnerID),
		slog.Time("owner_last_seen_at", c.OwnerLastSeenAt),
	)
	return decisionWarned
}

// suspend removes the vehicle's fleet-telemetry config and stamps the episode.
//
// THE TESLA CALL COMES FIRST AND THE STAMP SECOND. Stamping first would produce
// the one state the platform cannot explain to anybody: a catalog row saying
// "disconnected" for a car that is still streaming and still being billed, with
// an owner staring at a disconnect notice while their live map updates behind
// it. In this order the failure mode is a retry on the next pass, which is
// harmless because the Tesla delete is idempotent.
//
// NOTHING ELSE IS TOUCHED. No token clear, no revoke, no share change, no ride
// write, no vehicle-row update. That is the property the §7.28 reconnect
// depends on, and it is the reason this function is three statements long.
func (s *Sweeper) suspend(ctx context.Context, c *Candidate, now time.Time) decision {
	if !s.canSuspend() {
		// The proxy is unconfigured. We cannot remove a config, so we must not
		// claim to have removed one — a stamp here would tell every consumer a
		// streaming car is disconnected and would hide it behind a reconnect
		// button that also cannot work.
		s.logger.Warn("telemetry sweep: due for suspension but no fleet-config path is wired; holding",
			slog.String("vehicle_id", c.VehicleID))
		return decisionHeld
	}

	token, err := s.tokens.AccessToken(ctx, c.OwnerID)
	if err != nil {
		// An owner with no usable Tesla token has no live config we can reach.
		// Hold rather than stamp: the truth is unknown, and the next pass costs
		// one lookup.
		s.logger.Warn("telemetry sweep: no Tesla token for the owner; holding",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("user_id", c.OwnerID),
			slog.String("error", err.Error()))
		return decisionHeld
	}

	if err := s.configs.DeleteTelemetryConfig(ctx, token, c.VIN); err != nil {
		s.logger.Warn("telemetry sweep: fleet-config delete failed; will retry next pass",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("vin", redactVIN(c.VIN)),
			slog.String("error", err.Error()))
		return decisionHeld
	}

	if err := s.store.MarkSuspended(ctx, c.VehicleID, now); err != nil {
		// The config IS gone and we failed to record it. The next pass will
		// re-issue an idempotent delete and try the stamp again; until it
		// succeeds the catalog under-reports (the car reads as connected while
		// silent), which is the same thing an owner sees for any offline car
		// and is far better than the inverse.
		s.logger.Error("telemetry sweep: config removed but the suspension stamp failed; retrying next pass",
			slog.String("vehicle_id", c.VehicleID),
			slog.String("error", err.Error()))
		return decisionHeld
	}

	s.logger.Info("telemetry suspended for owner inactivity",
		slog.String("vehicle_id", c.VehicleID),
		slog.String("user_id", c.OwnerID),
		slog.String("vin", redactVIN(c.VIN)),
		slog.Time("owner_last_seen_at", c.OwnerLastSeenAt),
	)
	return decisionSuspended
}

// redactVIN shows only the last four characters (data-classification.md §2.1).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}
