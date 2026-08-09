// The read half of the MYR-491 setup state: one LEFT JOIN, shared by all three
// vehicle-read surfaces that carry it.
//
// MYR-489 (migration 0036) gave go_fleet_config_attempts a memory of WHY a car
// is not streaming. MYR-491 puts that memory on the wire so a not-yet-streaming
// car can explain itself instead of rendering as a silent empty card. This file
// is the projection; internal/telemetry owns the derivation, exactly as the
// MYR-316 service window keeps its raw columns here and its precedence there.
//
// WHY A JOIN AND NOT A SECOND QUERY. The go_vehicle_control_state precedent
// (MYR-316, MYR-342): a per-vehicle follow-up lookup would be an N+1 on the very
// catalog path MYR-122 exists to protect. This rides one probe of
// go_fleet_config_attempts' vehicle_id PRIMARY KEY per row and pulls four
// fixed-width values — no text, no JSON, no encrypted shadow. It also makes the
// facts CONSISTENT by construction: the outcome and the timestamps it is judged
// against come from one row read at one instant, so the derived state can never
// be assembled from two different moments.
//
// A READ of a Go-owned table joined into a read of the Prisma-owned "Vehicle".
// CG-DL-9 governs MIGRATIONS, not runtime SELECTs, so no migration is required
// or added here — every column already exists (0031 and 0036).

package store

import "time"

// SetupSchedule is one vehicle's go_fleet_config_attempts row, or the absence
// of one. RAW STORAGE: the wire value is derived from it in internal/telemetry
// and is never emitted from here.
//
// Present is the load-bearing field. The table is SELF-DRAINING — a successful
// config push DELETEs the row (migration 0031) — so "no row" is itself the
// strongest signal available: this car is not currently failing to stream. It
// must not be confused with a row whose timestamps happen to be zero, which
// means the opposite (a car we are tracking, whose events have not happened
// yet).
type SetupSchedule struct {
	Present bool
	// LastOutcome is the label the previous reconcile attempt recorded. Empty
	// for a row seeded at link time and never attempted.
	LastOutcome string
	// LastAttemptAt is when we last asked Tesla about this car. Zero == never.
	LastAttemptAt time.Time
	// SignedCommandAt is when a signed command last APPLIED — proof the virtual
	// key is paired, and the start of the pairing epoch. Zero == never.
	SignedCommandAt time.Time
	// ForcedRepushAt is when the MYR-489 escalation last recreated this car's
	// config. Zero == never.
	ForcedRepushAt time.Time
}

// setupScheduleColumns is the projection every setup-state read shares.
//
// `fca.vehicle_id IS NOT NULL` is how row-presence crosses the wire from a LEFT
// JOIN: the three timestamps are all NULLABLE in their own right, so no
// combination of their values can distinguish "no row" from "a row that has not
// been attempted". Deriving presence from a nullable payload column is the
// classic LEFT JOIN bug, and here it would invert the safest rule in the
// feature — that absence of a row means make no claim.
//
// The alias order MUST match setupScheduleScan.dests().
const setupScheduleColumns = `fca.vehicle_id IS NOT NULL AS "setupPresent",
	COALESCE(fca.last_outcome, ''), fca.last_attempt_at,
	fca.signed_command_at, fca.forced_repush_at`

// setupScheduleJoin attaches the schedule row. Written against the quoted
// "Vehicle" relation name rather than an alias so it composes with both the
// unaliased snapshot/owner-list queries and the aliased shared-catalog one.
const setupScheduleJoin = `
LEFT JOIN go_fleet_config_attempts fca ON fca.vehicle_id = "Vehicle"."id"`

// setupScheduleScan is the destination bag for setupScheduleColumns, in the
// same shape and for the same reason as controlStateScan: the column order
// (which tracks the SQL) and the field mapping (which tracks the domain type)
// are kept in separate methods so they fail independently and loudly rather
// than silently transposing two same-typed timestamps.
type setupScheduleScan struct {
	present         bool
	lastOutcome     string
	lastAttemptAt   *time.Time
	signedCommandAt *time.Time
	forcedRepushAt  *time.Time
}

// dests returns the scan destinations in setupScheduleColumns' order.
func (s *setupScheduleScan) dests() []any {
	return []any{&s.present, &s.lastOutcome, &s.lastAttemptAt, &s.signedCommandAt, &s.forcedRepushAt}
}

// value collapses the scanned row into the domain type, mapping every SQL NULL
// onto the zero Time — the same "never observed" reading the reconciler's own
// candidate scan uses (derefTime, vehicle_fleet_config_pairing.go).
func (s *setupScheduleScan) value() SetupSchedule {
	if !s.present {
		// Defensive rather than merely tidy: a row that is absent must carry no
		// residue, so a future caller cannot read a stray timestamp off it.
		return SetupSchedule{}
	}
	return SetupSchedule{
		Present:         true,
		LastOutcome:     s.lastOutcome,
		LastAttemptAt:   derefTime(s.lastAttemptAt),
		SignedCommandAt: derefTime(s.signedCommandAt),
		ForcedRepushAt:  derefTime(s.forcedRepushAt),
	}
}
