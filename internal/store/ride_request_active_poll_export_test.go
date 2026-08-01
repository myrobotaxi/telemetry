package store

// Test-only export of the MYR-394 active-ride poll-target statement.
//
// It is unexported because nothing outside this package should be executing
// hand-assembled SQL. The exception is the PLAN test
// (ride_request_active_poll_index_plan_test.go, package store_test), whose
// whole subject is what Postgres does with this exact string: it EXPLAINs it
// and asserts the read reaches idx_go_ride_requests_active_poll rather than
// seq-scanning and sorting the whole ride table. That test cannot be written
// against the repository method, because a method call cannot be EXPLAINed —
// the seam has to be the SQL. Same reasoning, and same shape, as the MYR-383
// export next door. Lives in a _test.go file that never ships in the binary.
var QueryActiveRidePollTargetsForTest = queryActiveRidePollTargets
