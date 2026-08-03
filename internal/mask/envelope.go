package mask

// Envelope fields and the substantive-payload predicate (MYR-435).
//
// WHY THIS EXISTS. websocket-protocol.md §4.6 requires the hub to suppress a
// frame whose mask projection left nothing — "sending an empty broadcast would
// leak 'something happened on this vehicle' to a viewer who shouldn't even know
// the field existed." That rule was implemented as `len(projected) == 0`, and
// that implementation had a hole the MYR-435 narrowing turned into a live leak.
//
// The broadcast path injects `lastUpdated` into EVERY non-nav frame before
// masking (internal/ws/nav_broadcast.go), and `lastUpdated` is viewer-visible —
// a viewer must be able to tell live from stale. So a frame carrying ONLY
// owner-only deltas does not project to zero fields for a viewer. It projects
// to exactly one: {"lastUpdated": …}. `len(projected) == 0` is false, the
// suppression gate never fires, and the frame goes out.
//
// The values are gone, but the FRAME is the signal. A `mediaNowPlayingElapsedMs`
// tick alone becomes a beacon that pulses only while audio is playing — which is
// verbatim the "someone is listening right now" occupancy signal that
// vehicleStateOwnerOnlyFields removes the media block to prevent. Door, lock and
// climate deltas are the same tell: a pulse each time someone opens the trunk.
// Masking the values while forwarding the timing defeats the point of masking.
//
// So the predicate is not "is the projection non-empty" but "does the projection
// carry anything that is actually ABOUT THE CAR". A frame whose surviving
// content is purely descriptive of the frame itself says nothing the recipient
// is entitled to learn, and must not be sent.

// envelopeFields are wire keys that describe the FRAME rather than the vehicle.
// They ride along on payloads as metadata, they are visible to every role, and
// on their own they carry no vehicle state at all.
//
// Kept here, beside the role tables, deliberately: the ws package is where these
// keys are injected, but if the set lived there it would drift out of step with
// the allow-lists that decide what counts as substantive. A key added here is
// one more thing that cannot, by itself, keep a frame alive.
//
// A key belongs in this set only if BOTH hold:
//
//  1. it is in the viewer allow-list (otherwise masking already removes it), and
//  2. its value is a property of the delivery, not an observation of the car.
//
// `lastUpdated` qualifies: it is the wire freshness marker. Nothing else does
// today — `status` and `chargeLevel` describe the car, `vehicleId` identifies
// it, and all three are legitimate one-field frames.
var envelopeFields = map[string]struct{}{
	"lastUpdated": {},
}

// IsSubstantive reports whether a projected payload carries at least one field
// that is an observation of the vehicle, rather than consisting solely of
// envelope/freshness keys.
//
// This is the suppression predicate callers should use in place of a bare
// `len(projected) == 0`. It is strictly stronger: an empty map is not
// substantive, so every case the old check caught is still caught.
//
// Applied to ALL roles, not just viewers, and that is intentional — a frame
// carrying only "the timestamp changed" informs nobody, whatever their role.
// It cannot regress owners in practice because the broadcast path returns early
// when there are no real fields to send (nav_broadcast.go checks
// `len(fields) == 0` BEFORE adding lastUpdated), so an owner-bound payload of
// nothing-but-freshness is not a shape production emits. Role-independence is
// what keeps this honest: a predicate that suppressed only for viewers would be
// a second, quieter mask table.
func IsSubstantive(projected map[string]any) bool {
	for field := range projected {
		if _, isEnvelope := envelopeFields[field]; !isEnvelope {
			return true
		}
	}
	return false
}

// IsEnvelopeField reports whether a single wire key is envelope metadata.
// Exposed for tests and for callers that need to reason about one key.
func IsEnvelopeField(field string) bool {
	_, ok := envelopeFields[field]
	return ok
}

// OwnerOnlyVehicleStateFields returns the wire keys that MYR-435 (and MYR-279
// for `vin`) withhold from viewers on the vehicle_state resource.
//
// Exported so that the per-SURFACE conformance tests — the REST snapshot
// handler in internal/telemetry, and the WS live-broadcast and connect-time
// snapshot-replay paths in internal/ws — can all assert against ONE
// authoritative list instead of three hand-copied ones. Three copies is how one
// of them silently stops covering a field.
//
// Returns a defensive copy: the caller must not be able to mutate the table
// that governs the projection.
func OwnerOnlyVehicleStateFields() []string {
	out := make([]string, len(vehicleStateOwnerOnlyFields))
	copy(out, vehicleStateOwnerOnlyFields)
	return out
}
