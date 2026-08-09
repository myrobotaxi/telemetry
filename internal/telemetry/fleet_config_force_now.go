// The MYR-489 forced re-push, callable ON DEMAND (MYR-505).
//
// MYR-489 built exactly the action MYR-505 needs — delete the car's fleet
// config through the signing proxy and create it again, so the firmware
// re-reads it and re-establishes its telemetry session — and reaching it
// required either a background pass or a signed command the owner happened to
// send. MYR-505's whole point is that neither should be necessary: the owner
// returning from the Tesla app IS the trigger.
//
// SO THIS FILE ADDS AN ENTRY POINT, NOT A SECOND IMPLEMENTATION. Everything
// below delegates to `forceRepush` — the same delete, the same create, the same
// classification of Tesla's answer, the same loud UNCONFIGURED event when the
// delete succeeds and the create does not, and the same bookkeeping that stamps
// the pairing epoch's escalation budget as spent. A fork would have been the
// easier code and the wrong one: the one behaviour that MUST NOT differ between
// the background path and the on-demand path is what happens to a customer's
// car when the create fails.
//
// AND IT SHARES THE BOUND. `epochForceSpent` is lifted out of
// `escalationBlocker` rather than restated here, so "one forced re-push per
// key-pairing epoch" is one predicate that both callers evaluate. The gates
// AROUND it legitimately differ — the reconciler additionally demands a repeat
// synced-but-quiet observation and proof the car is awake, because it is acting
// unattended on a car nobody is looking at, while MYR-505 has Tesla's direct
// answer that the key is paired and an owner standing at the vehicle — but the
// budget itself is not a matter of opinion.

package telemetry

import "context"

// epochForceSpent reports whether this vehicle has already spent its one forced
// config re-push for the CURRENT key-pairing epoch.
//
// Unspent while forced_repush_at is zero (never escalated), and re-armed only
// when a NEW epoch opens — that is, when signed_command_at moves past the last
// force. This is the bound that makes a permanently unreachable car cost one
// escalation and then nothing: the forced path records a normal incrementing
// attempt instead of clearing the schedule, so a car that still does not stream
// falls back into ordinary backoff rather than re-forcing.
//
// MULTI-INSTANCE CAVEAT (unchanged from MYR-489): the budget is read from a row
// and stamped after the fact, so two processes acting on the same car in the
// same window could each spend the epoch once. The bound degrades to "at most
// one forced re-push per epoch PER INSTANCE", never to an unbounded loop.
func epochForceSpent(c FleetConfigCandidate) bool {
	return !c.ForcedRepushAt.IsZero() && !c.ForcedRepushAt.Before(c.SignedCommandAt)
}

// ForceConfigRepushNow runs the MYR-489 escalation immediately for one
// candidate and reports whether the config was actually recreated.
//
// It is the reconciler's on-demand face for MYR-505 and performs NO gating of
// its own: the caller owns the decision, because the caller is the one holding
// the evidence (Tesla's fleet_status answer, an accepted wake). What it does
// guarantee is that the ACTION and its consequences — including the schedule
// write and the epoch stamp — are byte-identical to a background escalation.
//
// A false return means the create step did not apply. The car may at that
// instant have NO config (the delete-then-create window), which `forceRepush`
// has already logged as its uniquely greppable UNCONFIGURED event and
// rescheduled at the base interval for the ordinary push path to repair. A
// caller MUST NOT report success to a user on a false return.
//
// ctx serves as both the Tesla-call context and the bookkeeping context. In the
// reconciler those differ so that an expired per-vehicle budget still records
// the attempt; here they are the same request context, which is correct — a
// caller who has hung up has also stopped waiting for the answer, and the
// bookkeeping write is the last thing to happen either way.
func (r *FleetConfigReconciler) ForceConfigRepushNow(
	ctx context.Context, c FleetConfigCandidate, accessToken string,
) bool {
	var out ReconcileOutcome
	r.forceRepush(ctx, ctx, c, accessToken, redactVIN(c.VIN), &out)
	return out.ForcedRepushes > 0
}
