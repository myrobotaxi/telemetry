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

	// TopicVehicleDeleted is published when a Vehicle row is deleted from
	// the Prisma-owned "Vehicle" table (sourced from a Postgres
	// LISTEN/NOTIFY channel; see internal/store/notify_listener.go). The
	// payload is VehicleDeletedEvent. Consumers: the WS hub (close
	// subscribed clients with code 4002), the telemetry receiver (close
	// the active inbound mTLS stream for the VIN), and the auth layer
	// (invalidate any user-existence cache entry for the owning user).
	TopicVehicleDeleted Topic = "vehicle.deleted"
)
