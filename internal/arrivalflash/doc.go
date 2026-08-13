// Package arrivalflash makes the car greet its rider: three headlight flashes,
// about a second and a half apart, the moment the server observes it standing
// at the pickup.
//
// THE CLIENT DECISION (MYR-542): flash the lights THREE times. No horn — a horn
// carries down a whole street and reads as aggression at any hour; headlights
// are addressed to the one person looking for the car and are silent to
// everybody else. Exactly once per waypoint, and on by default.
//
// # What it is, structurally
//
// One subscriber on internal/events' `ride.waypoint_arrived` seam, which is
// published ONLY by the MYR-538 auto-arrival detector — that is, only for a car
// whose own telemetry showed it parked inside the pickup radius for the dwell
// window. That provenance is the whole safety argument for pointing a physical
// command at a car with nobody asking: the owner's manual "Picked up" tap does
// NOT publish this seam, so a tap made from a kitchen table three miles away
// cannot flash anything. The event says "the car is at the kerb, right now,
// observed"; that is the only sentence that may drive the lights.
//
// The command goes through the SAME signed executor and owner-token resolution
// the nav dispatcher uses (internal/commands, `flash_lights`, SignerRequired) —
// not a second path to the car. A vehicle whose virtual key is unpaired, whose
// owner's token has expired, or whose proxy is unconfigured therefore fails
// here exactly as it fails there, with the same typed errors.
//
// # Failure is silent, and that is the design
//
// Every failure is LOGGED AND NOTHING ELSE. Nobody is told, nothing is
// persisted, nothing is retried. A missed flash is not actionable by anyone: it
// is a courtesy the rider never knew was coming, the ride itself has already
// advanced to `arrived` (that write happened before this event existed and does
// not depend on it), and the rider's push and Live Activity have already fired.
// Surfacing "your car did not flash its lights" would invent an incident out of
// a gesture, and the one party who could act — the owner — cannot make a
// sleeping car flash from their phone either.
//
// The bound follows from the same reasoning: at most THREE command attempts per
// waypoint, which are the three flashes themselves. A flash is never retried,
// and a failed one ABANDONS the remainder rather than pressing on. Two reasons.
// A refusal here is essentially never transient — asleep-after-wake, unpaired
// key, expired token and revoked scope are all conditions the next call one and
// a half seconds later meets unchanged (the executor has already spent its own
// wake budget inside that first call). And a partial greeting is worse than
// none: one flash, a gap, one flash reads as a fault, whereas nothing at all
// reads as a car that simply did not flash, which is what every car did before
// this feature.
//
// # Exactly once per (ride, waypoint)
//
// The detector already publishes the seam exactly once. The latch here
// (latch.go) is belt-and-braces against redelivery — a bus replay, a future
// second publisher — because the failure it prevents is visible in the street:
// a car flashing six times looks broken. It is in-memory and bounded, and it is
// claimed BEFORE the work starts, so a process restart mid-greeting does not
// resume it and a crash-looping server cannot strobe a car.
package arrivalflash
