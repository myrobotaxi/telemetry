package events

import "time"

// RideMemberJoinedEvent announces that somebody JOINED a group ride by
// redeeming its shared link (MYR-540). Published by the join handler on the
// redemption that actually inserted a membership row — never on the idempotent
// re-redeem, which changes nothing and would otherwise put one join on the bus
// as many times as a flaky network retried it.
//
// SUMMARY-ONLY, and more strictly so than its neighbours: ids and an instant.
//
//   - NO CODE. The join code is P1 and bearer, and an internal event is still a
//     value that gets logged by a subscriber somebody writes next year.
//   - NO NAME. The member's first name is P1 and is derivable by any consumer
//     that needs it, from the id, through the read path that already resolves
//     it — carrying it here would be the RideAcceptedEvent passenger-PII mistake
//     (MYR-447) repeated: a field populated on every publish for no reader.
//   - NO PLACES, like every other summary ride event.
//
// Internal-only — never broadcast to WS clients. Membership changes deliberately
// get no WS frame of their own; `ride_status_changed` stays summary-only and a
// client picks up a new member on the refetch it already performs.
//
// Fire-and-forget and drop-safe: the membership has already committed, so a bus
// failure is logged and the join stands.
type RideMemberJoinedEvent struct {
	BasePayload
	RideRequestID string
	VehicleID     string
	// RiderID / OwnerID are the ride's two standing parties — carried so a
	// consumer can address them without re-reading the ride, exactly as the
	// other ride events do.
	RiderID string
	OwnerID string
	// MemberID is the account that just joined. P0 — an opaque identifier.
	MemberID string
	JoinedAt time.Time
}

// EventTopic returns TopicRideMemberJoined.
func (RideMemberJoinedEvent) EventTopic() Topic { return TopicRideMemberJoined }
