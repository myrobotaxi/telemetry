package store

// Metrics collects database operation metrics. Implementations must
// be safe for concurrent use by multiple goroutines.
type Metrics interface {
	// ObserveQueryDuration records the time taken for a database query.
	// Operation names follow the pattern "entity.method", e.g.,
	// "vehicle.get_by_vin", "drive.create".
	ObserveQueryDuration(operation string, seconds float64)

	// IncQueryError increments the count of failed database queries.
	IncQueryError(operation string)

	// SetPoolStats updates connection pool gauge metrics.
	SetPoolStats(acquired, idle, total int32)
}
