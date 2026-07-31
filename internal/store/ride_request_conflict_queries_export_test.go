package store

// Test-only export of the two statements built from the MYR-383 window
// fragments.
//
// They are unexported because nothing outside this package should be executing
// hand-assembled SQL. The exception is the PLAN test
// (ride_request_window_index_plan_test.go, package store_test), whose whole
// subject is what Postgres does with these exact strings: it EXPLAINs them and
// asserts the `scheduled_for` range reaches Index Cond rather than degrading to
// a heap Filter. That test cannot be written against the repository methods,
// because a method call cannot be EXPLAINed — the seam has to be the SQL. Both
// live in a _test.go file that never ships in the binary.
var (
	// QueryRideWindowConflictForTest is the create/accept conflict probe.
	QueryRideWindowConflictForTest = queryRideWindowConflict
	// QueryVehicleBookedWindowsForTest is the MYR-385 picker read.
	QueryVehicleBookedWindowsForTest = queryVehicleBookedWindows
)
