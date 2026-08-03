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

	// IncDecryptFailure counts one failure to decrypt an at-rest
	// ciphertext column, labelled by the column that failed (MYR-433).
	//
	// This exists because the encrypt-at-rest read paths are deliberately
	// FAIL-SOFT: a vehicle whose GPS ciphertext will not decrypt reports
	// no location rather than 500ing the snapshot, and a drive whose
	// trail will not decrypt reports an empty trail. That is the right
	// behaviour for availability and the wrong behaviour for silence — a
	// wrong ENCRYPTION_KEY or a corrupt row would otherwise look exactly
	// like "this car has never reported a position", and the first signal
	// would be a user asking why their map is empty.
	//
	// The counter is what turns that into a minutes-not-months detection.
	// Alert on rate > 0.
	//
	// The label is a COLUMN NAME (e.g. "latitudeEnc", "routePointsEnc") —
	// P2 operational metadata. It must never carry a VIN, userId or any
	// decrypted value.
	IncDecryptFailure(column string)

	// SetPoolStats updates connection pool gauge metrics.
	SetPoolStats(acquired, idle, total int32)
}
