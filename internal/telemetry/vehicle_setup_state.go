// The MYR-491 owner-facing SETUP STATE — one derivation, two read surfaces.
//
// WHY THIS EXISTS. MYR-489 taught the server to heal a car that was linked but
// never streamed; it deliberately left the wire untouched. The consequence
// showed up the next night (MYR-503): a brand-new owner opened the app, saw a
// vehicle card with no location and no charge, tapped Lock, and got a dead
// button — because nothing on the wire said "your virtual key is not paired
// yet". The client's instruction was explicit: do NOT hang pairing guidance off
// a failed control ("does this make sense to the end user to have retry
// associated with lock?" — no). The card must render BEFORE any command is
// attempted, and it must name the ONE thing left to do.
//
// So the server publishes the state and the client renders copy. This file is
// the ONLY place the state is decided; both the §7.1 snapshot and the §7.0
// catalog row call it with the same inputs, which is why they cannot disagree
// and why no client ever re-derives it.
//
// THE HONESTY BAR. Every state here is a CLAIM about a real car in front of a
// real owner, so the rule is: say nothing unless there is specific evidence,
// and never say something a longer-lived fact would contradict. Three
// consequences, each of which cost a design round:
//
//  1. ABSENCE IS NOT EVIDENCE. A car with no schedule row gets no state. The
//     row is self-draining (a successful config push DELETEs it — migration
//     0031), so "no row" means healthy OR not yet examined, and neither
//     licenses a setup card.
//  2. `configuring` MUST NOT OUTLIVE ITS EVIDENCE. It is the only state that
//     claims progress rather than naming a blocker, so it requires a live
//     pairing epoch or a fresh forced re-push and expires with them. A card
//     that says "connecting…" for a week is a lie told slowly.
//  3. A CAR THAT STREAMED ONCE AND WENT QUIET IS NOT "SETTING UP". That is the
//     freshness story (`lastUpdated`), it has its own UI, and letting it leak
//     in here would put a setup card on every car parked in a basement garage.
//     The streaming gate below is what keeps the two apart.
//
// NOT DERIVED FROM `key_not_paired` COMMAND REJECTIONS, and that is deliberate
// rather than an omission — the issue names them as a candidate source. The
// wserrors code is OVERLOADED: internal/commands/executor.go returns the very
// same `key_not_paired` when OUR signing proxy is unconfigured
// (`errKeyNotPaired("vehicle command signing is not configured (no proxy)")`),
// with no virtual key involved at all. Deriving "go pair your key in the Tesla
// app" from it would tell every owner in the fleet to re-pair whenever our own
// deployment loses its proxy — the worst possible failure for a card whose
// entire job is naming the ONE thing left to do. Tesla's `missing_key` on the
// config push carries no such ambiguity, so that is the evidence used, and
// owner_stream_hook seeds it at link time so it is present within seconds of
// onboarding rather than after the reconciler's first pass.

package telemetry

import "time"

// Wire values for VehicleState.setupState.state / VehicleSummary.setupState.state
// (rest-api.md §7.0, §7.1). Deliberately a SMALL closed set: each member exists
// because it implies a DIFFERENT action for the person holding the phone.
const (
	// SetupStateAwaitingVirtualKey — Tesla refused to apply this car's
	// telemetry config because the owner's virtual key is not enrolled on the
	// vehicle. ACTION: the owner pairs the key in the Tesla app. Nothing the
	// server can do heals this.
	SetupStateAwaitingVirtualKey = "awaiting_virtual_key"
	// SetupStateConfiguring — the config side is in hand and the server is
	// waiting for the car to open its telemetry session (including after a
	// MYR-489 forced re-push). ACTION: none; the client says so and, past a
	// few minutes, may suggest waking the car's screen.
	SetupStateConfiguring = "configuring"
	// SetupStateTokenFailed — the owner's Tesla authorization can no longer be
	// resolved or refreshed, so neither config pushes nor commands can reach
	// the car. ACTION: the owner reconnects their Tesla account.
	SetupStateTokenFailed = "token_failed"
)

// Attempt-outcome labels this file reads out of go_fleet_config_attempts.
// They are the reconciler's own vocabulary (fleet_config_reconciler.go,
// fleet_config_force_repush.go); named again here as a READ contract so the
// coupling is explicit and a rename breaks compilation rather than silently
// collapsing a state to "no claim".
const (
	setupOutcomeAwaitingKey  = outcomeAwaitingKey
	setupOutcomeTokenFailed  = outcomeTokenFailed
	setupOutcomeSyncedQuiet  = outcomeSyncedQuiet
	setupOutcomeSkippedOther = outcomeSkippedOther
	setupOutcomeReadFailed   = outcomeReadFailed
	setupOutcomePushFailed   = outcomePushFailed
	setupOutcomeForced       = outcomeForcedRepush
	setupOutcomeForcedFailed = outcomeForcedRepushFail
)

// setupQuietHorizon is how stale a vehicle row must be before the derivation
// will believe the car is not streaming.
//
// It MIRRORS defaultFleetConfigStaleness on purpose, and the two must stay
// equal: they answer the same question from opposite ends. The reconciler uses
// it to decide a car looks non-streaming and is worth a Tesla read; this uses
// it to decide the same car may be described as non-streaming to its owner. A
// horizon SHORTER than the reconciler's would put a setup card on cars the
// reconciler has not even looked at; a LONGER one would leave the card up on a
// car that is already healed.
const setupQuietHorizon = 30 * time.Minute

// setupConfiguringWindow bounds the ONE state that claims progress.
//
// `awaiting_virtual_key` and `token_failed` need no bound: both are statements
// of a condition that is still true until something changes it, and both name
// an action only the owner can take. `configuring` is different — it says "we
// are working on it", which stops being true at some point whether or not the
// car ever connects. Past the window the state falls back to NO CLAIM rather
// than to `awaiting_virtual_key`: a car whose pairing we have PROVEN is not
// awaiting a key, and telling its owner to re-pair would send them to the Tesla
// app to fix something that is not broken. Absence hands the card back to the
// ordinary offline/last-seen story, which is the honest owner of a car that has
// been quiet for a day.
//
// A day is the same order as the reconciler's AwakeWindow, for the same reason:
// beyond it, a stamp is history rather than evidence about the car right now.
const setupConfiguringWindow = 24 * time.Hour

// VehicleSetupSchedule is the go_fleet_config_attempts row for one vehicle, as
// LEFT JOINed by the snapshot and catalog reads. It is RAW STORAGE — the wire
// value is derived from it, never emitted from it.
//
// Present distinguishes "no row" from "a row whose columns happen to be zero",
// which matters: the table is self-draining, so the absence of a row is itself
// the strongest single signal in this file (the car is not currently failing to
// stream) and must not be confused with a freshly inserted row that has not
// been attempted yet.
type VehicleSetupSchedule struct {
	Present bool
	// LastOutcome is the label the previous reconcile attempt recorded ('' for
	// a row seeded at link time and never attempted).
	LastOutcome string
	// LastAttemptAt is when that attempt ran — when we last ASKED TESLA about
	// this car. Zero when never attempted.
	LastAttemptAt time.Time
	// SignedCommandAt is when a signed command last APPLIED to this car. Proof
	// the virtual key is paired, and the start of the pairing epoch. Zero when
	// never observed.
	SignedCommandAt time.Time
	// ForcedRepushAt is when the MYR-489 escalation last deleted and recreated
	// this car's config. Zero when never.
	ForcedRepushAt time.Time
}

// SetupState is the wire object carried by VehicleState.setupState and
// VehicleSummary.setupState. Identical shape on both surfaces, by construction:
// one type, one builder, two callers.
type SetupState struct {
	// State is one of the SetupState* constants above.
	State string `json:"state"`
	// Since is when the evidence behind State was recorded, RFC 3339 UTC. It is
	// data for the client's copy ("for 2 days"), NOT an input a client is
	// expected to re-derive the state from.
	Since string `json:"since"`
}

// deriveSetupState answers "is there something the owner of this car has to
// finish, and what is it?" — or nil for "no claim".
//
// nil is the overwhelmingly common answer and the right default: it is what a
// healthy streaming car returns, what a car nobody has examined returns, and
// what an expired claim decays to.
func deriveSetupState(
	now time.Time,
	status string,
	lastUpdated time.Time,
	s VehicleSetupSchedule,
) *SetupState {
	if !s.Present {
		return nil
	}

	streaming := isStreamingNow(status, lastUpdated, now)
	paired := !s.SignedCommandAt.IsZero() && s.SignedCommandAt.After(s.LastAttemptAt)

	switch s.LastOutcome {
	case setupOutcomeAwaitingKey:
		// Tesla itself said the key is missing. NOT gated on the streaming
		// check: a car whose config was never applied cannot be streaming
		// under it, so a fresh `lastUpdated` here is a provisioning write or a
		// web sync, not telemetry — and suppressing on it would blind the card
		// for exactly the first half hour of a new owner's life, which is the
		// window MYR-503 happened in.
		//
		// Superseded by proof: a signed command that applied AFTER that
		// observation means the owner has since paired, so the blocker named
		// here is stale and the car is merely waiting to connect.
		if paired {
			return setupStateAt(SetupStateConfiguring, s.SignedCommandAt, now)
		}
		return setupStateAt(SetupStateAwaitingVirtualKey, s.LastAttemptAt, now)

	case setupOutcomeTokenFailed:
		// Gated on streaming. A dead refresh token does not stop an mTLS
		// stream that is already up (the two credentials are unrelated), and a
		// "finish setup" card on a car happily reporting its position would be
		// the wrong frame for what is really an account-maintenance problem.
		// When the car does go quiet, this surfaces.
		if streaming {
			return nil
		}
		return setupStateAt(SetupStateTokenFailed, s.LastAttemptAt, now)

	case setupOutcomeSyncedQuiet, setupOutcomeForced, setupOutcomeForcedFailed,
		setupOutcomePushFailed, setupOutcomeReadFailed, setupOutcomeSkippedOther:
		if streaming {
			// THE STALE-ROW TRAP. Nothing deletes a schedule row when a car
			// heals itself — only a successful PUSH does — so a car that
			// started streaming on its own keeps a row saying
			// `synced_not_streaming` indefinitely. Without this arm the owner
			// of a perfectly live car would read "we are still connecting it".
			return nil
		}
		return configuringWithinWindow(s, now)

	default:
		// Includes '' — a row seeded at link time and not yet attempted. The
		// seed carries no outcome of its own precisely so it cannot fabricate
		// one; it exists to be OVERWRITTEN by the first real attempt.
		return nil
	}
}

// configuringWithinWindow emits `configuring` only while the evidence that the
// server is actively getting this car connected is still live.
//
// The evidence is the later of the pairing epoch and the forced re-push: either
// "the key is proven paired, so the remaining work is ours" or "we have just
// issued this car a brand-new config to act on". With NEITHER, there is no
// basis for claiming progress — a synced-but-silent car whose owner has never
// sent a command and which we have never escalated is a car we know nothing
// about beyond "quiet", which is the freshness story, not a setup state.
func configuringWithinWindow(s VehicleSetupSchedule, now time.Time) *SetupState {
	epoch := s.SignedCommandAt
	if s.ForcedRepushAt.After(epoch) {
		epoch = s.ForcedRepushAt
	}
	if epoch.IsZero() || now.Sub(epoch) > setupConfiguringWindow {
		return nil
	}
	return setupStateAt(SetupStateConfiguring, epoch, now)
}

// isStreamingNow reports whether the vehicle row looks like it is being written
// by a live telemetry stream right now.
//
// TWO conditions, and the `status` half is load-bearing rather than
// belt-and-braces. `"Vehicle"."lastUpdated"` is bumped by writes that are not
// telemetry at all — the MYR-257 provisioning INSERT, the partner web app's
// sync — so freshness alone would call a car "streaming" seconds after it was
// created, which is precisely the new-owner case the card exists for. `status`
// closes it: `NOT NULL DEFAULT 'offline'` on the Prisma table, and since
// MYR-454 ANY streamed frame folds it to a motion value, so a row still sitting
// at the schema default has never had a frame folded into it (MYR-437, and the
// reservation sweeper's offline-hold argument in
// internal/dispatch/reservation_worker.go).
//
// The composition is deliberately conservative in the direction that matters:
// it is easy to be told "not streaming" about a car that is merely asleep, and
// that is harmless here because a state is only ever emitted alongside specific
// evidence. Being told "streaming" about a car that is not would suppress the
// card, so that answer requires BOTH a fresh row AND a status the stream must
// have written.
func isStreamingNow(status string, lastUpdated, now time.Time) bool {
	if status == vehicleStatusOffline || status == "" {
		return false
	}
	return now.Sub(lastUpdated) < setupQuietHorizon
}

// setupStateAt builds the wire object, defending against a zero or future
// `since`.
//
// A zero timestamp would serialize as year 1 and render as "setting up for
// 2025 years"; a stamp in the future (clock skew between this process and
// Postgres) would render as a negative age. Both collapse to `now`, which is
// the honest floor — the evidence exists as of this read.
func setupStateAt(state string, since, now time.Time) *SetupState {
	if since.IsZero() || since.After(now) {
		since = now
	}
	return &SetupState{
		State: state,
		Since: since.UTC().Format(time.RFC3339),
	}
}
