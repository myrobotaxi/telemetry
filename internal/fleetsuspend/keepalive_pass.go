package fleetsuspend

// One keepalive pass and the per-owner policy. Split from keepalive.go to keep
// both files inside the 300-line cap, exactly as sweep.go is split from
// sweeper.go. The reasoning for everything below — why the arm exists, why it
// cannot strand an account, and what a refusal is allowed to do — lives in
// keepalive.go's header and is not repeated here.

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// keepDecision is what one pass did about one owner.
type keepDecision int

const (
	keepRotated keepDecision = iota
	keepRefused
	keepBusy
	keepSkipped
)

// KeepaliveResult counts one keepalive pass.
type KeepaliveResult struct {
	Due     int
	Rotated int
	Refused int
	Busy    int
	Skipped int
}

func (r *KeepaliveResult) count(d keepDecision) {
	switch d {
	case keepRotated:
		r.Rotated++
	case keepRefused:
		r.Refused++
	case keepBusy:
		r.Busy++
	case keepSkipped:
		r.Skipped++
	}
}

// KeepaliveOnce runs one keepalive pass.
//
// EXPORTED and kill-switched in its own right. It is called from SweepOnce, so
// the sweeper's Run gate already silences it in production, but SweepOnce is a
// test seam and this is now a second entry point that spends real credentials —
// an entry point that can rotate a token must carry the stop button itself
// rather than inherit it from whoever happened to call.
//
// SERIAL, like the arms it runs beside, and for a sharper reason than theirs:
// these are signed calls against Tesla's OAuth endpoint, the one upstream where
// being rate-limited costs credentials rather than latency.
func (s *Sweeper) KeepaliveOnce(ctx context.Context) KeepaliveResult {
	var res KeepaliveResult
	if !s.cfg.Enabled || s.keep == nil {
		return res
	}

	now := s.now()
	kc := s.cfg.Keepalive
	q := KeepaliveQuery{
		StaleBefore:   now.Add(-kc.RefreshAfter),
		AttemptBefore: now.Add(-kc.RetryAfter),
		FailureBefore: now.Add(-kc.FailureCooldown),
		// The anti-race gate, deliberately borrowed from the suspension
		// threshold rather than given a knob of its own: the owner must be at
		// least as quiet as the thing that suspended them.
		QuietBefore: now.Add(-s.cfg.SuspendAfter),
		Limit:       kc.MaxPerSweep,
	}

	listCtx, cancel := context.WithTimeout(ctx, s.cfg.PassTimeout)
	owners, err := s.keep.store.ListStaleTokenOwners(listCtx, q)
	cancel()
	if err != nil {
		s.logger.Error("telemetry sweep: stale-token read failed; no grants refreshed this pass",
			slog.String("error", err.Error()))
		return res
	}
	res.Due = len(owners)

	for i := range owners {
		if ctx.Err() != nil {
			// Shutdown, honoured between owners — before a single-use token has
			// been put in flight for this one.
			res.Skipped += len(owners) - i
			return res
		}
		res.count(s.rotateOne(ctx, owners[i], now))
	}

	if res.Due > 0 {
		s.logger.Info("tesla token keepalive pass",
			slog.Int("due", res.Due),
			slog.Int("rotated", res.Rotated),
			slog.Int("refused", res.Refused),
			slog.Int("busy", res.Busy),
			slog.Int("skipped", res.Skipped),
		)
	}
	return res
}

// rotateOne keeps one owner's grant alive.
//
// NO TOKEN VALUE IS LOGGED HERE OR ANYWHERE BELOW IT — user ids, an expiry
// instant and an outcome, which is the whole vocabulary this arm needs.
func (s *Sweeper) rotateOne(ctx context.Context, c KeepaliveCandidate, now time.Time) keepDecision {
	// Detached from the pass context for the same reason the vehicle arms are:
	// a SIGTERM must not guillotine a rotation between Tesla consuming the old
	// refresh token and the transaction storing the new one.
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Keepalive.Timeout)
	defer cancel()

	err := s.keep.rotator.RotateTeslaToken(workCtx, c.OwnerID)
	switch {
	case err == nil:
		if rErr := s.keep.store.RecordKeepaliveSuccess(workCtx, c.OwnerID, now); rErr != nil {
			// Harmless. The rotation itself moved the authoritative clock — the
			// grant's stored expiry is now hours away, not weeks — so the
			// staleness gate excludes this owner for another full window with
			// or without our note about it.
			s.logger.Warn("tesla token keepalive: refreshed but the record failed",
				slog.String("user_id", c.OwnerID),
				slog.String("error", rErr.Error()))
		}
		s.logger.Info("tesla token keepalive refreshed",
			slog.String("user_id", c.OwnerID),
			slog.Time("previous_expiry", c.TokenExpiresAt),
		)
		return keepRotated

	case errors.Is(err, ErrRotationBusy):
		// The healthiest possible signal: somebody else is rotating this grant,
		// which is the very thing we were about to do for them. NO RECORD is
		// written — spending the cooldown here would suppress a real keepalive
		// for a week because a reconnect happened to be in flight for a second.
		s.logger.Info("tesla token keepalive: grant is being rotated elsewhere; skipping",
			slog.String("user_id", c.OwnerID))
		return keepBusy

	default:
		// Tesla refused, or the grant is gone from under us. THE STORED TOKEN
		// IS UNTOUCHED — store.RotateTeslaTokenLocked writes nothing on a
		// rotation error — and the record below is what stops this becoming an
		// hourly call against a dead account forever.
		if rErr := s.keep.store.RecordKeepaliveFailure(workCtx, c.OwnerID, now, c.TokenExpiryUnix); rErr != nil {
			// The one bad case: unrecorded, so the next pass re-asks. Logged at
			// Error because a persistent version of this IS the retry loop the
			// record exists to prevent.
			s.logger.Error("tesla token keepalive: refusal could not be recorded; the next pass will re-ask",
				slog.String("user_id", c.OwnerID),
				slog.String("error", rErr.Error()))
		}
		s.logger.Warn("tesla token keepalive refused; the stored grant was left untouched",
			slog.String("user_id", c.OwnerID),
			slog.Time("token_expiry", c.TokenExpiresAt),
			slog.String("error", err.Error()),
		)
		return keepRefused
	}
}
