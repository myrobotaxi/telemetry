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

	// IncCloseUserDeletion increments the count of WebSocket sessions
	// closed because the underlying Vehicle row was deleted (data
	// lifecycle §3.5 cleanup, MYR-73). Each closed client increments
	// the counter once. The corresponding Prometheus metric name is
	// `ws_close_user_deletion_total`.
	IncCloseUserDeletion()
}
