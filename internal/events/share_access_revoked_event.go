package events

// ShareAccessRevokedEvent is published when an owner narrows ONE grantee's
// access to ONE vehicle: a rest-api.md §7.5.3 revoke or a §7.5.7 suspend.
// It is the in-process nudge that closes websocket-protocol.md §10 DV-09 —
// without it the WS access set, frozen at handshake, keeps a suspended or
// revoked viewer's already-open socket streaming the car's live GPS for the
// unbounded life of that connection (MYR-373).
//
// Published by the sharing handlers AFTER the mutation has committed and
// AFTER the grantee's cached access set has been busted, in that order.
// The ordering is load-bearing: the consumer closes the socket, the client
// reconnects, and the reconnect re-runs the handshake against
// GetUserVehicles. If the event overtook the cache bust, the reconnect
// could be served the pre-mutation access set and re-open the very stream
// the close just ended.
//
// SCOPED TO THE GRANTEE, never to the vehicle at large. The owner's own
// sessions — and every other viewer's — are unaffected, which is the whole
// difference between this and VehicleDeletedEvent.
type ShareAccessRevokedEvent struct {
	BasePayload

	// GranteeUserID is the person who LOST access. Required; an empty
	// value is dropped by the consumer rather than being treated as a
	// wildcard, because "close everybody" is never the intended reading
	// of a malformed revocation.
	GranteeUserID string

	// VehicleID is the car they lost access to. An empty value means
	// "re-evaluate every session this grantee holds" and closes all of
	// them — a deliberately blunt fallback for a caller that could not
	// determine the vehicle. It is safe (the client reconnects and
	// re-handshakes into whatever it still legitimately holds) but noisy,
	// so both live call sites populate it.
	VehicleID string

	// Reason is the owner action that caused this, for the close log
	// only: "revoked" or "suspended". It never reaches the wire — the
	// close code and reason a client sees are identical for both, so the
	// server does not tell a viewer which lever the owner pulled.
	Reason string
}

// EventTopic returns TopicShareAccessRevoked.
func (ShareAccessRevokedEvent) EventTopic() Topic { return TopicShareAccessRevoked }
