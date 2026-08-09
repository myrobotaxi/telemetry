package telemetry

import (
	"testing"
	"time"
)

// The derivation is the whole feature: everything downstream is plumbing, and
// every bug here is a false statement rendered on a card in front of a customer.
// The table below is therefore organised by the LIE each case would tell if the
// rule it pins were removed.

func TestDeriveSetupState(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	tests := []struct {
		name        string
		status      string
		lastUpdated time.Time
		sched       VehicleSetupSchedule
		wantState   string // "" == no claim (nil)
		wantSince   time.Time
	}{
		{
			name:        "no schedule row makes no claim",
			status:      "parked",
			lastUpdated: ago(time.Hour),
			sched:       VehicleSetupSchedule{},
			wantState:   "",
		},
		{
			name:        "seeded row with no attempt yet makes no claim",
			status:      "offline",
			lastUpdated: ago(time.Minute),
			sched:       VehicleSetupSchedule{Present: true, LastOutcome: ""},
			wantState:   "",
		},

		// --- MYR-503: the new owner, minutes old, key never paired ---------
		{
			// The row is FRESH (provisioned 4 minutes ago) and would read as
			// "streaming" to a freshness-only gate. awaiting_virtual_key must
			// not be suppressed by that, or the card is blind for exactly the
			// window in which the owner taps Lock.
			name:        "brand-new car awaiting key reports it despite a fresh row",
			status:      "offline",
			lastUpdated: ago(4 * time.Minute),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeAwaitingKey,
				LastAttemptAt: ago(4 * time.Minute),
			},
			wantState: SetupStateAwaitingVirtualKey,
			wantSince: ago(4 * time.Minute),
		},
		{
			name:        "awaiting key does not expire with age",
			status:      "offline",
			lastUpdated: ago(30 * 24 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeAwaitingKey,
				LastAttemptAt: ago(30 * 24 * time.Hour),
			},
			wantState: SetupStateAwaitingVirtualKey,
			wantSince: ago(30 * 24 * time.Hour),
		},
		{
			// Pairing landed after the skip was recorded. Continuing to say
			// "pair your key" would send the owner to fix something that is
			// already fixed.
			name:        "an applied signed command supersedes a stale awaiting-key",
			status:      "offline",
			lastUpdated: ago(2 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeAwaitingKey,
				LastAttemptAt: ago(2 * time.Hour), SignedCommandAt: ago(20 * time.Minute),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(20 * time.Minute),
		},
		{
			// The command applied BEFORE the skip was observed, so the skip is
			// the newer fact and still stands.
			name:        "a signed command older than the attempt does not supersede it",
			status:      "offline",
			lastUpdated: ago(2 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeAwaitingKey,
				LastAttemptAt: ago(20 * time.Minute), SignedCommandAt: ago(2 * time.Hour),
			},
			wantState: SetupStateAwaitingVirtualKey,
			wantSince: ago(20 * time.Minute),
		},

		// --- MYR-489 / Nabil: paired, synced, silent -----------------------
		{
			name:        "synced-but-quiet with a live pairing epoch is configuring",
			status:      "offline",
			lastUpdated: ago(48 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSyncedQuiet,
				LastAttemptAt: ago(time.Hour), SignedCommandAt: ago(3 * time.Hour),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(3 * time.Hour),
		},
		{
			// A forced re-push is fresh config the car can act on, and is
			// evidence of progress even with no command ever applied
			// (provenAwake can clear on a GetVehicle read alone).
			name:        "a forced re-push is evidence of progress on its own",
			status:      "offline",
			lastUpdated: ago(48 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeForced,
				LastAttemptAt: ago(10 * time.Minute), ForcedRepushAt: ago(10 * time.Minute),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(10 * time.Minute),
		},
		{
			name:        "the later of the epoch and the forced re-push dates the claim",
			status:      "offline",
			lastUpdated: ago(48 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeForced,
				LastAttemptAt:   ago(time.Hour),
				SignedCommandAt: ago(20 * time.Hour), ForcedRepushAt: ago(time.Hour),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(time.Hour),
		},
		{
			// THE BOUND. Nothing may say "connecting…" forever.
			name:        "configuring expires past its window rather than persisting",
			status:      "offline",
			lastUpdated: ago(90 * 24 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSyncedQuiet,
				LastAttemptAt: ago(time.Hour), SignedCommandAt: ago(48 * time.Hour),
			},
			wantState: "",
		},
		{
			// ...and it expires to NO CLAIM, never to awaiting_virtual_key: we
			// have proof the key IS paired, so re-pairing is not the action.
			name:        "an expired configuring never decays into awaiting-key",
			status:      "offline",
			lastUpdated: ago(90 * 24 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeForcedFailed,
				LastAttemptAt: ago(30 * 24 * time.Hour), SignedCommandAt: ago(30 * 24 * time.Hour),
			},
			wantState: "",
		},
		{
			// THE REGRESSION GUARD the issue names explicitly: a car that
			// streamed once and went quiet is the freshness story, not setup.
			// Synced-but-quiet with no pairing epoch and no escalation is
			// exactly that car.
			name:        "quiet with no pairing evidence at all is the freshness story",
			status:      "offline",
			lastUpdated: ago(6 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSyncedQuiet,
				LastAttemptAt: ago(time.Hour),
			},
			wantState: "",
		},
		{
			// THE STALE-ROW TRAP. Only a successful push deletes the row, so a
			// car that healed itself keeps one saying synced_not_streaming.
			name:        "a car that is streaming now overrides its stale schedule row",
			status:      "driving",
			lastUpdated: ago(20 * time.Second),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSyncedQuiet,
				LastAttemptAt: ago(3 * time.Hour), SignedCommandAt: ago(2 * time.Hour),
			},
			wantState: "",
		},
		{
			// The same row on a car whose last frame is older than the
			// reconciler's own staleness horizon IS a claimable state.
			name:        "a parked car past the quiet horizon is claimable again",
			status:      "parked",
			lastUpdated: ago(31 * time.Minute),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSyncedQuiet,
				LastAttemptAt: ago(3 * time.Hour), SignedCommandAt: ago(2 * time.Hour),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(2 * time.Hour),
		},

		// --- token_failed ---------------------------------------------------
		{
			name:        "a dead Tesla token on a quiet car asks for a reconnect",
			status:      "offline",
			lastUpdated: ago(6 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeTokenFailed,
				LastAttemptAt: ago(2 * time.Hour),
			},
			wantState: SetupStateTokenFailed,
			wantSince: ago(2 * time.Hour),
		},
		{
			// mTLS streaming and OAuth are unrelated credentials. A live car is
			// not a setup problem, whatever the token is doing.
			name:        "a dead token on a streaming car is not a setup card",
			status:      "driving",
			lastUpdated: ago(20 * time.Second),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeTokenFailed,
				LastAttemptAt: ago(2 * time.Hour),
			},
			wantState: "",
		},

		// --- operator-side outcomes ----------------------------------------
		{
			name:        "an unrecognised Tesla skip reads as work in progress, not as the owner's problem",
			status:      "offline",
			lastUpdated: ago(6 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: setupOutcomeSkippedOther,
				LastAttemptAt: ago(time.Hour), SignedCommandAt: ago(time.Hour),
			},
			wantState: SetupStateConfiguring,
			wantSince: ago(time.Hour),
		},
		{
			name:        "an unknown outcome label makes no claim",
			status:      "offline",
			lastUpdated: ago(6 * time.Hour),
			sched: VehicleSetupSchedule{
				Present: true, LastOutcome: "something_new",
				LastAttemptAt: ago(time.Hour), SignedCommandAt: ago(time.Hour),
			},
			wantState: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveSetupState(now, tt.status, tt.lastUpdated, tt.sched)

			if tt.wantState == "" {
				if got != nil {
					t.Fatalf("want no claim, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want state %q, got no claim", tt.wantState)
			}
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.State, tt.wantState)
			}
			if want := tt.wantSince.UTC().Format(time.RFC3339); got.Since != want {
				t.Errorf("since = %q, want %q", got.Since, want)
			}
		})
	}
}

// A zero `since` would render as year 1 ("setting up for 2025 years") and a
// skewed future one as a negative age. Both are floored at now.
func TestDeriveSetupStateNeverEmitsANonsenseSince(t *testing.T) {
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	wantNow := now.UTC().Format(time.RFC3339)

	t.Run("zero timestamp", func(t *testing.T) {
		got := deriveSetupState(now, "offline", now, VehicleSetupSchedule{
			Present: true, LastOutcome: setupOutcomeAwaitingKey,
		})
		if got == nil || got.Since != wantNow {
			t.Fatalf("got %+v, want since %q", got, wantNow)
		}
	})

	t.Run("clock skew into the future", func(t *testing.T) {
		got := deriveSetupState(now, "offline", now, VehicleSetupSchedule{
			Present: true, LastOutcome: setupOutcomeAwaitingKey,
			LastAttemptAt: now.Add(90 * time.Second),
		})
		if got == nil || got.Since != wantNow {
			t.Fatalf("got %+v, want since %q", got, wantNow)
		}
	})
}

// The quiet horizon must not drift from the reconciler's staleness: they are
// two readings of one question ("does this row look like it is not streaming"),
// and a divergence would either card a car nobody has examined or leave a card
// up on a healed one.
func TestSetupQuietHorizonTracksReconcilerStaleness(t *testing.T) {
	if setupQuietHorizon != defaultFleetConfigStaleness {
		t.Fatalf("setupQuietHorizon = %v, defaultFleetConfigStaleness = %v — "+
			"these must stay equal; see the constant's doc comment",
			setupQuietHorizon, defaultFleetConfigStaleness)
	}
}
