package auth

// ShareGrant is the CAPABILITY SET one accepted share conveys (MYR-369) — the
// thing that replaced the fixed cumulative tier, and the only thing any access
// gate is allowed to read for a non-owner.
//
// The two fields are NOT peers, and every method below exists to make that
// structural rather than a rule somebody has to remember:
//
//	AllowRides — a capability. May this viewer request rides in the car?
//	Suspended  — a GATE OVER EVERYTHING. When true the grant conveys NOTHING,
//	             whatever AllowRides says.
//
// THE SUSPENSION INVARIANT. A suspended grant is excluded from the viewer-merge
// access set at the SQL level (`suspended_at IS NULL` on the access-set
// statements), so in the ordinary path a suspended grant never reaches this type
// at all — the vehicle simply is not in the caller's access set, and the list,
// snapshot, WebSocket, drives and rides surfaces all deny together. The
// Suspended field and the checks here are the SECOND, INDEPENDENT gate for any
// path that reads a grant row directly rather than through the access set. Two
// gates for one invariant is deliberate: an access-control property that holds
// only because every query remembered a predicate is one refactor from not
// holding.
//
// There is no exported constructor and no zero-value ceremony: the zero
// ShareGrant is {AllowRides: false, Suspended: false}, which is a live grant
// conveying the base capability only. That is the correct fail-closed default —
// a grant materialized by a path that forgot to set the flags grants the
// smallest thing, not the largest.
type ShareGrant struct {
	// AllowRides is the per-grant, owner-editable ride capability. Read it
	// through GrantsRides, never directly at a gate — the bare field does
	// not know about suspension.
	AllowRides bool
	// Suspended reports whether the owner has paused this grant. Backed by
	// go_vehicle_shares.suspended_at being non-NULL.
	Suspended bool
}

// Active reports whether the grant conveys anything at all — the base
// capability every live grant carries (catalog row, snapshot, WS subscription).
//
// This is the ONLY meaning of "live" now that the tiers are gone: there is no
// lower state a grant can be in and still exist, so a grant is either active or
// suspended.
func (g ShareGrant) Active() bool { return !g.Suspended }

// GrantsRides reports whether the holder may create a ride request against the
// vehicle as a non-owner.
//
// The suspension term is FIRST and is not redundant with the caller's own
// checks: this is the method a gate calls, so the invariant is enforced here
// rather than trusted to each of the three ride paths (create, owner accept,
// reservation dispatch) getting the conjunction right independently.
func (g ShareGrant) GrantsRides() bool { return !g.Suspended && g.AllowRides }

// GrantsHistory reports whether the holder may read the drives / trip-history
// surfaces. It ALWAYS returns false (MYR-369).
//
// The method is kept, rather than deleted along with the live_history tier, so
// that the drives gates have something explicit to call and so the retirement is
// a fact stated in one place with a test on it — instead of an absence of code
// that reads, at every call site, exactly like an omission. If the capability
// ever returns it gets a real flag and this body changes; until then a reviewer
// asking "can a viewer read drives?" gets an answer rather than silence.
func (g ShareGrant) GrantsHistory() bool { return false }

// Permission derives the wire-compatible SharePermission for this grant — the
// lossy projection pre-MYR-369 clients read (vehicle-sharing.schema.json
// SharePermission, VehicleSummary.sharePermission).
//
// One direction only: flags are authoritative and this is computed from them on
// every read, so the derived value can never disagree with what the gates
// enforce. Never returns live_history — the tier is retired, and a legacy grant
// created at that preset derives `live`, which is exactly what it now conveys.
//
// Callers MUST NOT emit this for a SUSPENDED grant. A suspended grant produces
// no viewer-facing row at all (it is not in the access set), so there is no
// correct value to project; the method stays total rather than returning an
// empty sentinel that a caller could serialize by accident.
func (g ShareGrant) Permission() SharePermission {
	if g.AllowRides {
		return PermissionRides
	}
	return PermissionLive
}

// GrantForPreset maps an invite-time preset onto the flags the accepted grant
// starts life with (MYR-369). This is the ONE place the retired tier is
// interpreted, and it is what "pending invites keep tier-at-redeem" means: an
// invite minted before this change, or by an un-updated client, redeems to
// exactly the capabilities its preset always implied.
//
//	rides                    -> AllowRides true
//	live, live_history, else -> AllowRides false
//
// The default arm is FAIL-CLOSED and covers an unrecognized value: a preset the
// enum does not know grants the base capability, never the ride one. It cannot
// normally happen (create validates through ParseSharePermission and the column
// carries a CHECK constraint), which is precisely why the arm must be the safe
// one — the only way to reach it is a path that has already gone wrong.
//
// Never produces a suspended grant: suspension is an owner action on a live
// grant, not a state anything can be born in.
func GrantForPreset(p SharePermission) ShareGrant {
	return ShareGrant{AllowRides: p == PermissionRides}
}
