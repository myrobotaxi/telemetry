package events

// Topic identifies an event channel. Subscribers filter by topic.
type Topic string

const (
	// TopicVehicleTelemetry is published when a batch of telemetry fields
	// arrives from a vehicle. The payload is VehicleTelemetryEvent.
	TopicVehicleTelemetry Topic = "vehicle.telemetry"

	// TopicVehicleTelemetryRaw carries every decoded proto field from a
	// vehicle payload, unfiltered by the broadcast field map. Emitted only
	// when the receiver is configured with PublishRawFields (dev/debug
	// mode). The payload is RawVehicleTelemetryEvent.
	TopicVehicleTelemetryRaw Topic = "vehicle.telemetry.raw"

	// TopicConnectivity is published when a vehicle connects or disconnects
	// from the telemetry server. The payload is ConnectivityEvent.
	TopicConnectivity Topic = "vehicle.connectivity"

	// TopicDriveStarted is published when the drive detector identifies
	// that a vehicle has begun a drive. The payload is DriveStartedEvent.
	TopicDriveStarted Topic = "drive.started"

	// TopicDriveUpdated is published for each route point accumulated
	// during an active drive. The payload is DriveUpdatedEvent.
	TopicDriveUpdated Topic = "drive.updated"

	// TopicDriveEnded is published when the drive detector identifies
	// that a vehicle has completed a drive. The payload is DriveEndedEvent.
	TopicDriveEnded Topic = "drive.ended"

	// TopicDriveDiscarded is published when the drive detector drops an
	// active drive via the micro-drive filter (too short / too little
	// distance). The writer creates the Drive row on drive.started, so
	// without this signal a discarded micro-drive leaks an open row
	// with endTime unset (MYR-160). The payload is DriveDiscardedEvent;
	// the store subscriber deletes the row. Internal-only — never
	// broadcast to WS clients.
	TopicDriveDiscarded Topic = "drive.discarded"

	// TopicRideRequestCreated is published when a rider creates a ride
	// request (P10 ride-hailing, MYR-174). The payload is
	// RideRequestCreatedEvent. The WS broadcaster subscribes and unicasts
	// a summary `ride_request_created` frame to the two parties (rider +
	// vehicle owner) only.
	TopicRideRequestCreated Topic = "ride.request.created"

	// TopicRideStatusChanged is published on every ride-request lifecycle
	// mutation — rider cancel (MYR-174), owner accept/decline (MYR-175),
	// dispatch progress (MYR-176), reschedule sub-state (MYR-192). The
	// payload is RideStatusChangedEvent. The WS broadcaster subscribes and
	// unicasts a summary `ride_status_changed` frame to the rider + owner.
	TopicRideStatusChanged Topic = "ride.status.changed"

	// TopicRideAccepted is the dispatch seam (MYR-175): published once when
	// an owner accepts a ride request, carrying the pickup/dropoff places
	// and passenger contact MYR-176 needs to build the Tesla
	// navigation_request push. Internal-only — never broadcast to WS
	// clients (the WS accept signal travels on TopicRideStatusChanged as a
	// summary). No subscriber exists until MYR-176 lands; the bus tolerates
	// zero subscribers.
	TopicRideAccepted Topic = "ride.accepted"

	// TopicRideStarted is the leg-2 dispatch seam (MYR-270, was ride.boarded in
	// MYR-265): published once when the RIDER starts the ride (the guarded
	// arrived→enroute transition), carrying the DROPOFF place the nav-dispatch
	// pipeline pushes as the car's new destination. Internal-only — never
	// broadcast to WS clients (the WS start signal travels on
	// TopicRideStatusChanged as an `enroute` summary frame). Exactly-once per
	// start: only the winning arrived→enroute write publishes it.
	TopicRideStarted Topic = "ride.started"

	// TopicRideDue is the reservation-due seam (MYR-179): published once when
	// a SCHEDULED ride reaches its `scheduledFor` instant and its pickup nav
	// push has been DELIVERED — i.e. the sweeper won the leg-1 claim and the
	// push resolved `sent`. The payload is RideDueEvent. Internal-only — never
	// broadcast to WS clients (the client-visible signal is the same
	// `dispatchStatus` annotation an instant ride gets).
	//
	// Publication follows the push rather than the claim because the topic's
	// meaning is "your car is on the way": a reservation that resolved
	// `skipped` (the DISPATCH_ENABLED kill-switch), `failed` (token/VIN/command
	// error), or was expired past its lateness ceiling emits NOTHING. The latch
	// admits one winner, so a false event here could never be corrected by a
	// later one — the seam is pinned to the delivered outcome, not the
	// intention.
	//
	// NO subscriber exists yet; the bus tolerates zero subscribers and the
	// publish is fire-and-forget + drop-safe (a publish failure NEVER affects
	// the dispatch, which has already happened). It exists so the planned rider
	// push-notification has a seam to hang off without re-opening the dispatch
	// path. Exactly-once per due ride by construction: only the sweeper that
	// WON the `dispatched_at` claim can reach the publish, and that latch admits
	// one winner for the ride's whole lifetime.
	TopicRideDue Topic = "ride.due"

	// TopicRideNavUnapplied is the nav-share close-loop's failure seam
	// (MYR-527): published once when a leg's nav push resolved `sent` but the
	// car's own reported destination never converged on the shared target —
	// through the verification window AND one re-share. The payload is
	// RideNavUnappliedEvent. Internal-only — never broadcast to WS clients;
	// its consumer is the owner's "check the dash" push.
	TopicRideNavUnapplied Topic = "ride.nav_unapplied"

	// TopicVehicleDeleted is published when a Vehicle row is deleted from
	// the Prisma-owned "Vehicle" table (sourced from a Postgres
	// LISTEN/NOTIFY channel; see internal/store/notify_listener.go). The
	// payload is VehicleDeletedEvent. Consumers: the WS hub (close
	// subscribed clients with code 4002), the telemetry receiver (close
	// the active inbound mTLS stream for the VIN), and the auth layer
	// (invalidate any user-existence cache entry for the owning user).
	TopicVehicleDeleted Topic = "vehicle.deleted"

	// TopicShareAccessRevoked is published when an owner action narrows a
	// grantee's vehicle access set while they may hold a live WebSocket —
	// a §7.5.3 revoke or a §7.5.7 suspend. The payload is
	// ShareAccessRevokedEvent. Consumer: the WS hub (close the grantee's
	// sessions for that vehicle with code 4002), which is what closes
	// websocket-protocol.md §10 DV-09 for the share-mutation case.
	//
	// Distinct from TopicVehicleDeleted on purpose. That event says "this
	// car no longer exists" and every consumer — receiver, VIN cache,
	// user-existence cache — has work to do. This one says "this ONE
	// person may no longer look at a car that still exists and is still
	// streaming to its owner", so the hub is the only consumer and the
	// close must be scoped to the grantee. Folding them together would
	// make a suspension tear down the owner's own session.
	TopicShareAccessRevoked Topic = "share.access_revoked"
)
