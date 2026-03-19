package ws

// NoopHubMetrics is a HubMetrics implementation where all methods are
// no-ops. Use it in tests or when Prometheus is not configured.
type NoopHubMetrics struct{}

var _ HubMetrics = NoopHubMetrics{}

func (NoopHubMetrics) SetConnectedClients(int) {}
func (NoopHubMetrics) IncMessagesSent()        {}
func (NoopHubMetrics) IncMessagesDropped()     {}
func (NoopHubMetrics) IncAuthFailures()        {}
