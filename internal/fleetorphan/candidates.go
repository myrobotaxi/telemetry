package fleetorphan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// candidate is one VIN under investigation, with every source that named it.
type candidate struct {
	VIN     string
	UserID  string
	Sources []string
}

// collect builds the union of sources A and B, keyed by VIN.
//
// A failure in either source is an entry in Report.Errors and NOT a returned
// error: a sweep that examined half the fleet is worth running, and the errors
// block is how the operator knows the answer is partial.
func (s *Sweeper) collect(ctx context.Context, rep *Report) []candidate {
	byVIN := make(map[string]*candidate)
	s.collectTombstoned(ctx, rep, byVIN)
	s.collectLiveFleets(ctx, rep, byVIN)

	out := make([]candidate, 0, len(byVIN))
	for _, c := range byVIN {
		out = append(out, *c)
	}
	// VIN-sorted so two runs against the same data produce the same report and
	// diff cleanly. Map iteration order would make the artifact useless for
	// before/after comparison.
	sortVINs(out)
	return out
}

// collectTombstoned adds source A: removed-vehicle tombstones whose VIN no
// local "Vehicle" row claims.
func (s *Sweeper) collectTombstoned(ctx context.Context, rep *Report, byVIN map[string]*candidate) {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	rows, err := s.deps.Store.ListTombstonedOrphanVINs(callCtx, s.cfg.MaxTombstones)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, "list tombstoned orphan VINs: "+errText(err))
		return
	}
	for _, r := range rows {
		add(byVIN, r.VIN, r.UserID, SourceTombstone, false)
	}
}

// collectLiveFleets adds source B: for every owner who still has a Tesla
// "Account" row, the VINs Tesla says they own that we hold no row for.
//
// This is the genuinely reachable set — the token exists by construction — and
// it is the only source that can find a config for a car we never tombstoned
// (provisioned then removed, or removed while the Tesla-side delete failed).
func (s *Sweeper) collectLiveFleets(ctx context.Context, rep *Report, byVIN map[string]*candidate) {
	if s.deps.Fleet == nil {
		rep.Errors = append(rep.Errors,
			"no Fleet API lister wired; skipped the live-owner fleet enumeration (source B)")
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	users, err := s.deps.Store.ListTeslaLinkedUserIDs(callCtx)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, "list Tesla-linked users: "+errText(err))
		return
	}

	for _, userID := range users {
		if ctx.Err() != nil {
			rep.Errors = append(rep.Errors, "cancelled during the live-owner fleet enumeration")
			return
		}
		s.collectOneFleet(ctx, rep, byVIN, userID)
	}
}

// collectOneFleet diffs one live owner's Tesla-side fleet against ours.
func (s *Sweeper) collectOneFleet(
	ctx context.Context, rep *Report, byVIN map[string]*candidate, userID string,
) {
	token, err := s.token(ctx, userID)
	if err != nil {
		// An "Account" row with no usable token is a real anomaly (the
		// ciphertext never landed, or decryption failed), but it is the user's
		// problem and not a VIN we can name — so it is a run-scoped error, not
		// an unreachable_no_token line.
		rep.Errors = append(rep.Errors, fmt.Sprintf("user %s: resolve token: %s", userID, errText(err)))
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	fleet, err := s.deps.Fleet.ListVehicles(listCtx, token)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("user %s: list Tesla vehicles: %s", userID, errText(err)))
		return
	}

	localCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	local, err := s.deps.Store.ListVehicleVINsByUser(localCtx, userID)
	cancel()
	if err != nil {
		// CRITICAL that this skips the user rather than proceeding. With an
		// unreadable local set, EVERY Tesla-side VIN would look orphaned and a
		// -apply run would delete the configs of this owner's live cars.
		rep.Errors = append(rep.Errors, fmt.Sprintf("user %s: list local VINs: %s", userID, errText(err)))
		return
	}
	held := make(map[string]struct{}, len(local))
	for _, v := range local {
		held[v] = struct{}{}
	}

	for _, fv := range fleet {
		if !fv.Owner {
			// A shared-driver entry is somebody else's car. We never
			// provisioned it and must never delete its config.
			continue
		}
		if _, ok := held[fv.VIN]; ok {
			continue
		}
		// preferOwner: this owner provably has a token, so if a tombstone
		// already claimed the VIN under a different (possibly deleted) user,
		// this handle is the one that can actually authenticate the delete.
		add(byVIN, fv.VIN, userID, SourceFleetList, true)
	}
}

// add merges a VIN into the candidate map, unioning sources. preferOwner
// replaces the recorded owner handle with a provably token-backed one.
func add(byVIN map[string]*candidate, vin, userID, source string, preferOwner bool) {
	if vin == "" {
		return
	}
	existing, ok := byVIN[vin]
	if !ok {
		byVIN[vin] = &candidate{VIN: vin, UserID: userID, Sources: []string{source}}
		return
	}
	for _, s := range existing.Sources {
		if s == source {
			return
		}
	}
	existing.Sources = append(existing.Sources, source)
	if preferOwner {
		existing.UserID = userID
	}
}

// cachedToken is one run-scoped token resolution.
//
// THE TOKEN VALUE LIVES ONLY HERE, in process memory, for the seconds the sweep
// runs. It is never logged, never put in the report, and never returned to a
// caller outside this package. The cache exists because source B already
// resolves a token per owner and the per-VIN pass would otherwise resolve it
// again — each miss being a potential OAuth refresh round-trip.
type cachedToken struct {
	token string
	// err is cached ONLY for ErrNoToken, which is a stable fact about the
	// account. A transient failure is left uncached so the next VIN retries.
	err error
}

// token resolves an owner's Tesla access token, memoised for the run.
func (s *Sweeper) token(ctx context.Context, userID string) (string, error) {
	if s.deps.Tokens == nil {
		return "", fmt.Errorf("%w: no token source wired", ErrNoToken)
	}
	if hit, ok := s.tokenCache[userID]; ok {
		return hit.token, hit.err
	}

	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	tok, err := s.deps.Tokens.AccessToken(callCtx, userID)
	cancel()
	if err != nil {
		wrapped := fmt.Errorf("access token (user=%s): %w", userID, err)
		if errors.Is(err, ErrNoToken) {
			// Cache the WRAPPED error so a cache hit and a cache miss produce
			// the identical value — the report's Detail must not depend on
			// which VIN of an owner's set happened to be examined first.
			s.tokenCache[userID] = cachedToken{err: wrapped}
		}
		return "", wrapped
	}
	if tok == "" {
		// An empty token is indistinguishable from an absent one at every call
		// site downstream (the Fleet API client rejects it), so name it here.
		err := fmt.Errorf("%w: token source returned an empty token", ErrNoToken)
		s.tokenCache[userID] = cachedToken{err: err}
		return "", err
	}
	s.tokenCache[userID] = cachedToken{token: tok}
	s.logger.Debug("resolved a Tesla token for the sweep", slog.String("user_id", userID))
	return tok, nil
}
