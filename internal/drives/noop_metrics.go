package drives

// NoopDetectorMetrics is a no-op implementation of DetectorMetrics for use in
// tests and when Prometheus is not configured.
type NoopDetectorMetrics struct{}

var _ DetectorMetrics = NoopDetectorMetrics{}

func (NoopDetectorMetrics) IncDriveStarted()              {}
func (NoopDetectorMetrics) IncDriveEnded()                {}
func (NoopDetectorMetrics) IncMicroDriveDiscarded()        {}
func (NoopDetectorMetrics) IncDebounceCancelled()          {}
func (NoopDetectorMetrics) ObserveDriveDuration(float64)   {}
func (NoopDetectorMetrics) ObserveDriveDistance(float64)   {}
func (NoopDetectorMetrics) SetActiveVehicles(int)          {}
