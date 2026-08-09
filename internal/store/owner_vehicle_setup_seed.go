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

// querySeedFleetConfigAwaitingKey records, against the vehicle owning vin, that
// Tesla has just declined to apply its telemetry config for want of a virtual
// key.
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
const querySeedFleetConfigAwaitingKey = `
INSERT INTO go_fleet_config_attempts
    (vehicle_id, attempt_count, last_attempt_at, next_attempt_at, last_outcome)
SELECT v."id", 0, $2, $2, $3
FROM "Vehicle" v
WHERE v."vin" = $1
ON CONFLICT (vehicle_id) DO NOTHING`

// SeedFleetConfigAwaitingKey persists the link-time observation that vin's
// telemetry config was skipped for a missing virtual key, so the owner's very
// first app open can explain what is left to do.
//
// Best-effort by contract: an unknown VIN inserts nothing, an existing schedule
// row is left exactly as it was, and both are success. The caller treats a
// returned error as loggable and never fatal — this is a card, not a gate, and
// failing an owner's Tesla link over it would be absurd.
func (p *OwnerProvisioner) SeedFleetConfigAwaitingKey(ctx context.Context, vin string, now time.Time) error {
	_, err := p.pool.Exec(ctx, querySeedFleetConfigAwaitingKey, vin, now, SetupOutcomeAwaitingVirtualKey)
	if err != nil {
		return fmt.Errorf("OwnerProvisioner.SeedFleetConfigAwaitingKey: %w", err)
	}
	return nil
}
