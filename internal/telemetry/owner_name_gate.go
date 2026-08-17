package telemetry

import (
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-581 NAMELESS-OWNER gate, shared by the rider-facing create
// (§7.8 POST /api/ride-requests) and the owner-facing accept backstop
// (§7.8 POST /api/ride-requests/{id}/accept). Modelled on ride_share_gate.go,
// which it sits immediately after on both paths.
//
// WHAT IT REFUSES. A car whose OWNER has no display name in any of the three
// identity sources. A ride is a transaction between two people, and every
// surface that renders one — the incoming request card, the accept toast, the
// Live Activity, the push copy — names the counterparty. With no name to render,
// the client falls back to the make, which is how "Tesla wants a ride" came to be
// said about a human being (MYR-532 item 4). Offering a car whose owner cannot be
// named produces a ride nobody can describe honestly, so the car is not
// offerable until the owner sets a name (`PATCH /api/users/me`, §7.26).
//
// SELF-RIDES ARE COVERED DELIBERATELY. An owner requesting their own car is
// refused too, even though there is no counterparty to name in that ride. Two
// reasons, and the second is the load-bearing one: (a) the gate's subject is the
// CAR's offerability, not the pair of people, so making it depend on who is
// asking would give one vehicle two different answers; and (b) the client's
// mandatory name sheet is what resolves this refusal, and an owner who only ever
// self-rides is exactly the owner who would otherwise never see it — then invite
// somebody and discover the problem from the far side.
//
// MONOTONICITY, NOT REVERSIBILITY — and this is why the three layers are shaped
// differently from MYR-342's. The pause is a switch an owner flips both ways, so
// its layers exist to catch a pause that lands mid-flight. Namelessness only
// ever resolves FORWARD: the only `UPDATE "User"` in this repository is
// `ProfileNameRepo` and it REFUSES the empty string outright (as does every rung
// it writes), so no code path can un-name an account. A car that is offerable
// stays offerable.
//
// The consequence is that layers 2 and 3 are BACKSTOPS FOR PRE-DEPLOY RIDES, not
// for a race: the population they catch is requests and reservations that were
// created before the create gate existed. That population is finite and shrinking,
// which is the honest argument for keeping the backstops cheap (both ride reads
// the enclosing gate was already doing) rather than the argument that the
// condition might come back.
//
// WHERE THE GATE SITS. AFTER the access/ownership check on both paths, always —
// for the same reason as the pause: a caller with no business with the vehicle
// must get the access denial they would have got anyway, and answering this
// earlier would turn the endpoint into an oracle about a stranger's owner.
//
// HOW THE FLAG IS READ. Off the SAME GetByID row that established access
// (create) or status (accept), as a BOOLEAN. The P1 name never enters the
// enforcement path at all — the store resolves the ladder to `IS NOT NULL` in
// SQL (internal/store/owner_name.go) and hands this layer one bit. That is the
// MYR-578 raw-badge discipline applied to something sharper than a badge, and it
// is what makes "never log the name" structurally true here rather than a rule
// somebody has to remember.

// ownerNamelessMessage is the copy for a refusal on a car whose owner has no
// name. One constant so the create gate, the accept backstop and the contract
// tests cannot drift.
//
// IT NAMES THE CONDITION, NOT THE PERSON. "This vehicle is not available for
// ride requests" is what a RIDER must read — they cannot act on the real reason
// and telling them "the owner has not set a name" discloses a fact about
// somebody else's account to a third party. The owner learns the actual reason
// from the client's name sheet, which the same 409 triggers, not from this
// string.
const ownerNamelessMessage = "This vehicle is not available for ride requests"

// rejectIfOwnerNameless writes the 409 and returns true when the vehicle's owner
// has no resolvable display name.
//
// The typed code is `vehicle_unavailable` with NO subCode — the FIFTH carrier of
// that code, and a plain member of the family rather than a special case. The
// four existing carriers split cleanly: four are conditions of the car RIGHT NOW
// (in service, offline, on another ride, paused) which a client answers with
// "try again later", and one is a property of the TIME the rider picked
// (`time_conflict`) which a client answers by returning them to the picker. A
// nameless owner is squarely the first kind — a condition of the car now, which
// clears when the owner acts — so it needs no sub-code to tell it apart from its
// siblings, and adding one would imply a branch a client does not have.
//
// It is emphatically not `conflict` (which means an illegal lifecycle
// transition) and not `permission_denied` (which would tell the caller they are
// the wrong person when they are not).
//
// LOGS THE EVENT, NEVER THE NAME — there is no name here to log even by
// accident: `row.OwnerNamed` is a boolean.
func rejectIfOwnerNameless(
	w http.ResponseWriter,
	logger *slog.Logger,
	op string,
	row VehicleSnapshotRow,
	userID string,
) bool {
	if row.OwnerNamed {
		return false
	}
	logger.Info(op+": vehicle owner has no display name",
		slog.String("vehicle_id", row.ID),
		slog.String("user_id", userID),
	)
	wserrors.WriteErrorEnvelope(w, logger, http.StatusConflict,
		wserrors.ErrCodeVehicleUnavailable, ownerNamelessMessage)
	return true
}
