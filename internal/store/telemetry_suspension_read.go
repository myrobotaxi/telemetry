// The READ half of MYR-592: how `VehicleSummary.telemetrySuspendedAt` reaches
// the three §7.0 catalog queries.
//
// ONE EXPRESSION AND ONE JOIN, composed by constant into all three producers —
// the owner list, the shared/viewer list and the group-ride member list — for
// exactly the reason catalogTrimLabelExpr and setupScheduleColumns are written
// this way. Three hand-copied fragments is three chances for one of them to
// drift, and the one most likely to be forgotten is the member query, which
// reuses the SHARED scan and therefore fails LOUDLY (a scan-order mismatch)
// rather than silently. That loudness is a property of composing the SELECT
// lists from the same constants; keep it.
//
// WHY THE VALUE IS ON A SIDE TABLE AT ALL: CG-DL-9 forbids a Go migration from
// naming the Prisma-owned "Vehicle" table, so `telemetrySuspendedAt` cannot be a
// column there. See migration 0044. The wire shape is unaffected — a consumer
// reads one nullable date-time on the row and cannot tell which table answered.
//
// EMITTED ON BOTH ROLES, deliberately, per contracts v0.38.0. A viewer whose
// friend's car has been suspended must be able to render the ordinary
// no-live-telemetry state rather than an error or an infinite spinner, and they
// can only do that if they are told. The two roles differ in what they are
// OFFERED about it — only the owner gets the reconnect/unlink pair — and that is
// a client concern, not a masking one.

package store

// catalogTelemetrySuspendedExpr projects the suspension instant onto the catalog
// row. NULL means streaming is configured normally, which covers three distinct
// storage states a consumer must not be asked to distinguish: no episode row at
// all, an episode that has been warned but not suspended, and an episode cleared
// by a reconnect.
//
// One nullable timestamp off a primary-key probe. It costs a LEFT JOIN the other
// side-table blocks do not already provide (unlike trim_label, which rides the
// control-state join) — worth stating, because it is the one column here that
// adds a probe rather than a field. The table holds one row per SUSPENDED-or-
// warned vehicle, not one per vehicle, so it stays a fraction of the fleet.
const catalogTelemetrySuspendedExpr = `gts.suspended_at`

// catalogTelemetrySuspendedJoin attaches the episode row. Written against the
// quoted "Vehicle" relation rather than an alias so it composes with the
// unaliased owner query and the aliased shared/member ones, exactly as
// setupScheduleJoin does.
const catalogTelemetrySuspendedJoin = `
LEFT JOIN go_vehicle_telemetry_suspensions gts ON gts.vehicle_id = "Vehicle"."id"`
