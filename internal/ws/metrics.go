package ws

// HubMetrics collects WebSocket hub operational metrics. Implementations
// must be safe for concurrent use by multiple goroutines.
type HubMetrics interface {
	// SetConnectedClients sets the current number of connected
	// (authenticated) WebSocket clients.
	SetConnectedClients(count int)

	// IncMessagesSent increments the total count of messages written to
	// client WebSocket connections.
	IncMessagesSent()

	// IncMessagesDropped increments the count of messages dropped because
	// a client's send buffer was full (slow client).
	IncMessagesDropped()

	// IncAuthFailures increments the count of authentication failures
	// (invalid token or auth timeout).
	IncAuthFailures()
}
