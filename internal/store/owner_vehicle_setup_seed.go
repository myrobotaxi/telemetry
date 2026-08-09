// The link-time seed for the MYR-491 setup state.
//
// WHY THIS IS NOT SCOPE CREEP. MYR-491 puts the setup state on the wire, and
// the state is read from go_fleet_config_attempts. But nothing WRITES a row to
// that table until the reconciler examines the car, and the reconciler only
// examines a car whose "Vehicle" row has been quiet for Staleness (30 minutes),
// on a pass that runs every Interval (15 minutes). So a brand-new owner has NO
// schedule row for the first half hour of their life with the product — which
// is exactly the window MYR-503 happened in: Amruth linked at 02:10Z and tapped
// Lock at 02:14Z. Without this seed, the card MYR-491 exists to render would
// have been blank for his entire encounter with the bug it was written about.
//
// The evidence is already in hand at link time and was being thrown away. The
// owner-onboarding config push fires inside the OAuth callback, necessarily
// BEFORE virtual-key pairing, and Tesla answers it with an explicit per-VIN
// `missing_key`. ownerStreamHook logged that and moved on. This persists it.
//
// WHY IT CHANGES NOTHING ABOUT MYR-489's BEHAVIOUR — the property to preserve,
// since PR #385 landed hours ago and its scheduling is delicate:
//
//   - attempt_count 0 and next_attempt_at = now make the seeded row
//     SCHEDULING-IDENTICAL to no row at all. The candidate query admits a
//     vehicle when `fa.vehicle_id IS NULL OR fa.next_attempt_at <= now`, and
//     the backoff is computed from attempt_count, so a car with this row is
//     picked up at precisely the same moment, with precisely the same first
//     backoff, as one without it. Seeding attempt_count 1 (what
//     RecordFleetConfigAttempt would have written) would have doubled that
//     first gap — a small instance of the very backoff poisoning MYR-489
//     Hole 2 was about.
//   - ON CONFLICT DO NOTHING. A re-link, a re-add, or a second link callback
//     must never overwrite a live schedule: clobbering last_outcome would
//     disarm a pending synced-not-streaming escalation, and resetting the
//     backoff would let an unpairable car be retried on every re-link.
//
// AND IT FIXES A GAP IN MYR-489 AS A SIDE EFFECT, which is worth stating
// because it was not obvious: ResetFleetConfigScheduleOnPairing is an UPDATE
// with no upsert, on the reasoning that "a car with no row is healthy or not
// yet examined". True in general — but a car mid-onboarding has no row for the
// same first 30 minutes, and that is exactly when its owner pairs the key and
// sends their first command. The in-band pairing signal therefore no-opped for
// the population it was built for. With a row present from link time, it lands.
//
// MYR-517 WIDENED THE SEED FROM ONE OUTCOME TO EVERY DOOR, because prod showed
// the narrow version missing the car it was written for. Spencer White's
// "Tizzy" (linked 2026-08-09 18:07:47Z) has NO row to this day, while four
// other cars' rows persist — and the reason is that MYR-491 hung the seed off a
// single branch of the link-time push: the `missing_key` skip. Every OTHER
// terminal outcome of that push — it applied, it failed with a transport or
// proxy error, it was skipped for some other reason, the deployment has no
// signing proxy at all — left the car with no schedule row, and therefore
// invisible to the in-band pairing signal for as long as it kept not streaming.
//
// So the seed is now unconditional for every provisioned owned vehicle, and the
// OUTCOME is the variable: `awaiting_virtual_key` when Tesla said so,
// `push_failed` when the push genuinely failed, and '' — no claim — when no
// push was attempted or the push applied. That keeps the WIRE behaviour
// byte-identical to before (internal/telemetry.deriveSetupState maps '' and
// `push_failed`-with-no-epoch alike onto "no claim") and the SCHEDULING
// byte-identical (attempt_count 0 + next_attempt_at now is admitted by the
// candidate query at exactly the same instant as no row at all). The only thing
// that changes is that the row EXISTS, which is the one thing the self-heal
// machinery needs and could not assume.

package store

import (
	"context"
	"fmt"
	"time"
)

// SetupOutcomeAwaitingVirtualKey is the go_fleet_config_attempts.last_outcome
// label meaning "Tesla refused this car's config because the virtual key is not
// paired". It is the reconciler's own vocabulary
// (internal/telemetry.outcomeAwaitingKey), repeated rather than imported
// because internal/telemetry is deliberately decoupled from this package — the
// same reason FleetConfigCandidate is declared twice.
//
// The duplication is guarded END TO END rather than by a string comparison:
// TestSeedFleetConfigAwaitingKey asserts the seeded row round-trips through
// GetByID, and the internal/telemetry derivation table asserts that this exact
// outcome yields the `awaiting_virtual_key` wire state. A drift in either
// spelling breaks one of the two.
const SetupOutcomeAwaitingVirtualKey = "awaiting_virtual_key"

// SetupOutcomePushFailed is the go_fleet_config_attempts.last_outcome label
// meaning "the config push itself failed" — a transport error, a proxy error,
// or a Tesla status we could not read as an answer. Duplicated from
// internal/telemetry.outcomePushFailed for the same reason
// SetupOutcomeAwaitingVirtualKey is, and guarded the same way: the
// internal/telemetry derivation table asserts what this outcome yields on the
// wire (no claim, absent a live pairing epoch), and the store round-trip test
// asserts the label survives a write.
const SetupOutcomePushFailed = "push_failed"

// SetupOutcomeNone is the empty last_outcome: a row seeded because a vehicle
// was provisioned, carrying NO claim about why it is not streaming. It exists
// to be overwritten by the first real attempt, and until then it is
// wire-invisible and scheduling-identical to no row at all.
const SetupOutcomeNone = ""

// querySeedFleetConfigSchedule records, against the vehicle owning vin, that a
// link-time provisioning pass happened and what (if anything) it observed.
//
// Keyed by VIN through a SELECT on "Vehicle" rather than by vehicle id for the
// same reason ResetFleetConfigScheduleOnPairing is: the caller is the link-time
// hook, which holds Tesla's VIN and not our cuid, and resolving it in SQL keeps
// this one statement instead of a lookup plus an insert that could race the
// provisioning INSERT that just ran.
//
// A READ of the Prisma-owned "Vehicle" feeding an INSERT into a Go-owned table.
// CG-DL-9 constrains MIGRATIONS naming Prisma tables; this is a runtime
// statement and adds no schema.
const querySeedFleetConfigSchedule = `
INSERT INTO go_fleet_config_attempts
    (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome)
SELECT v."id", 0, $2, $2, $3
FROM "Vehicle" v
WHERE v."vin" = $1
ON CONFLICT (vehicle_id) DO NOTHING`

// SeedFleetConfigSchedule persists the link-time schedule row for vin, labelled
// with what the provisioning pass observed (SetupOutcomeAwaitingVirtualKey,
// SetupOutcomePushFailed, or SetupOutcomeNone).
//
// IDEMPOTENT BY CONSTRUCTION, which is the property MYR-517 needs: it is called
// on EVERY successful link/auth completion — a fresh provision, a re-link over
// an already-provisioned car, a deliberate re-add — and a car that already has
// a schedule row keeps it EXACTLY as it was. Clobbering a live schedule on a
// re-link would disarm a pending escalation and reset a hard-won backoff, so
// "seed on every door" and "never overwrite" have to hold together, and
// ON CONFLICT DO NOTHING is what makes the second one free.
//
// Best-effort by contract: an unknown VIN inserts nothing, an existing schedule
// row is left exactly as it was, and both are success. The caller treats a
// returned error as loggable and never fatal — this is a card, not a gate, and
// failing an owner's Tesla link over it would be absurd.
func (p *OwnerProvisioner) SeedFleetConfigSchedule(
	ctx context.Context, vin, outcome string, now time.Time,
) error {
	_, err := p.pool.Exec(ctx, querySeedFleetConfigSchedule, vin, now, outcome)
	if err != nil {
		return fmt.Errorf("OwnerProvisioner.SeedFleetConfigSchedule: %w", err)
	}
	return nil
}
