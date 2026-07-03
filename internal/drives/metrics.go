package drives

// DetectorMetrics collects drive detection operational metrics.
// Defined in internal/drives/. Implemented by a Prometheus adapter or
// a noop for tests.
type DetectorMetrics interface {
	// IncDriveStarted increments the count of drives started.
	IncDriveStarted()

	// IncDriveEnded increments the count of drives ended normally.
	IncDriveEnded()

	// IncMicroDriveDiscarded increments the count of discarded micro-drives.
	IncMicroDriveDiscarded()

	// IncDebounceCancelled increments the count of debounce timers cancelled
	// (vehicle resumed driving before debounce elapsed).
	IncDebounceCancelled()

	// IncWatchdogEnded increments the count of drives ended by the
	// silent-telemetry watchdog (Tesla stopped streaming without a
	// gear=P frame, so the AfterFunc-based debounce never primed).
	// MYR-139 R3a.
	IncWatchdogEnded()

	// IncStallEnded increments the count of drives ended by the stall
	// condition: telemetry kept flowing but no movement was observed
	// for StallTimeout (missed gear=P frame while the parked car
	// streamed idle fields). MYR-160.
	IncStallEnded()

	// IncDurationCapEnded increments the count of drives ended by the
	// MaxDriveDuration backstop. MYR-160.
	IncDurationCapEnded()

	// ObserveDriveDuration records the duration of a completed drive.
	ObserveDriveDuration(seconds float64)

	// ObserveDriveDistance records the distance of a completed drive.
	ObserveDriveDistance(miles float64)

	// SetActiveVehicles sets the gauge of vehicles currently in Driving state.
	SetActiveVehicles(count int)
}
