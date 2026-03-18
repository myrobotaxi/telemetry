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

	// ObserveDriveDuration records the duration of a completed drive.
	ObserveDriveDuration(seconds float64)

	// ObserveDriveDistance records the distance of a completed drive.
	ObserveDriveDistance(miles float64)

	// SetActiveVehicles sets the gauge of vehicles currently in Driving state.
	SetActiveVehicles(count int)
}
