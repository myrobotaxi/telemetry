package telemetry

import (
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-342 ride-sharing pause gate, shared by the rider-facing create
// (§7.8 POST /api/ride-requests) and the owner-facing accept backstop
// (§7.8 POST /api/ride-requests/{id}/accept).
//
// WHAT THE PAUSE IS. `rideShareEnabled` is the owner's own switch on their own
// car: "I am not lending this out right now". It is NOT a statement about the
// vehicle. A paused car may be online, parked, charged and idle; nothing about
// its telemetry differs from an available one.
//
// WHY IT APPLIES TO SCHEDULED RIDES TOO — the DELIBERATE DEVIATION.
// MYR-313 exempts SCHEDULED rides from the MYR-277 in-service/offline
// availability gate, and rest-api.md §7.8 documents that exemption. The pause
// deliberately does NOT inherit it, on either path. The exemption's argument is
// that a reservation days out says nothing about the car's status today,
// because a service visit ENDS: the car will be back, so refusing the
// reservation strands the owner over a condition that will have cleared by the
// time it matters.
//
// An owner's pause has no such horizon. Nothing ends it but the owner reaching
// for the switch again, so "the condition will have cleared by then" is not a
// fact about a pause — it is a hope. Honouring the exemption here would let a
// rider book a car whose owner has withdrawn it indefinitely, and would leave
// the owner with the reservation still in their queue: the exact hand-declining
// treadmill the toggle exists to end.
//
// The lateness ceiling in the reservation sweeper is what makes that safe
// rather than merely strict. A reservation the owner never re-enables is not
// held forever — it expires naturally at scheduledFor + MaxLateness with an
// honest `reservation_expired` outcome (internal/dispatch/reservation_worker.go).
//
// WHERE THE GATE SITS. AFTER the access/ownership check on both paths, always.
// A caller with no business with the vehicle must get the access denial they
// would have got anyway; making the pause visible earlier would turn this into
// an oracle answering questions about a stranger's car.
//
// HOW THE FLAG IS READ. Off the SAME GetByID row that established access
// (create) or status (accept) — one statement, two facts, no window in which
// they can disagree. There is a TOCTOU gap between that read and the eventual
// row insert / status write, bounded by the handler's own latency: an owner who
// pauses in exactly that window may see one request land. That is accepted and
// backstopped rather than closed, because the two writes it precedes are
// guarded on different things (the per-rider and per-vehicle unique indexes,
// and the status-guarded UPDATE) and neither can carry a vehicle-level
// predicate. The backstops are the point of having THREE layers: a request that
// slips past create is refused at accept, and a reservation that slips past
// accept is refused at dispatch.

// rideSharePausedMessage is the rider- and owner-facing copy for a refusal on a
// paused vehicle. One constant so the create gate, the accept backstop and the
// contract tests cannot drift from each other.
const rideSharePausedMessage = "Ride sharing is paused for this vehicle"

// rejectIfRideSharePaused writes the 409 and returns true when the vehicle's
// owner has paused ride sharing.
//
// The typed code is `vehicle_unavailable`, shared with the MYR-266
// already-on-another-ride guard and the MYR-277 in-service/offline gate, and
// that sharing is correct: all three are CAPABILITY refusals — the request
// itself is well-formed and the transition legal, the car simply cannot serve
// it. It is emphatically not `conflict`, which means an illegal lifecycle
// transition, and not `permission_denied`, which would tell the caller they are
// the wrong person when they are not.
//
// Applies to instant AND scheduled rides — see the file header for why the
// MYR-313 exemption stops here.
func rejectIfRideSharePaused(
	w http.ResponseWriter,
	logger *slog.Logger,
	op string,
	row VehicleSnapshotRow,
	userID string,
) bool {
	if row.RideShareEnabled {
		return false
	}
	logger.Info(op+": vehicle has ride sharing paused",
		slog.String("vehicle_id", row.ID),
		slog.String("user_id", userID),
	)
	wserrors.WriteErrorEnvelope(w, logger, http.StatusConflict,
		wserrors.ErrCodeVehicleUnavailable, rideSharePausedMessage)
	return true
}
