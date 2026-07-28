package events

// NoopBusMetrics is a BusMetrics implementation where all methods are no-ops.
// Use it in tests or when metrics collection is not required.
type NoopBusMetrics struct{}

var _ BusMetrics = NoopBusMetrics{}

func (NoopBusMetrics) IncPublished(Topic)                        {}
func (NoopBusMetrics) IncDelivered(Topic)                        {}
func (NoopBusMetrics) IncDropped(Topic)                          {}
func (NoopBusMetrics) ObservePublishDuration(Topic, float64)     {}
func (NoopBusMetrics) SetSubscriberCount(Topic, int)             {}
