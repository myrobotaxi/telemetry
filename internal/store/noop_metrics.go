package store

// NoopMetrics is a Metrics implementation where all methods are
// no-ops. Use it in tests or when metrics collection is not required.
type NoopMetrics struct{}

var _ Metrics = NoopMetrics{}

func (NoopMetrics) ObserveQueryDuration(string, float64) {}
func (NoopMetrics) IncQueryError(string)                 {}
func (NoopMetrics) SetPoolStats(int32, int32, int32)     {}
