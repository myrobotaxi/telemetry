package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The Live Activity leg-progress anchor (MYR-398, migration 0027).
//
// Four columns on go_live_activities that remember what a fraction is a
// fraction OF. The car tells us how far it still has to go; only we can
// remember how far it had to go when the leg began, and only the database can
// remember it across a deploy and across two replicas that both push to the
// same phone. See the migration header for the full argument.

// LiveActivityProgress is one Activity's leg-progress anchor. The zero value
// means "no anchor", which is what a freshly registered row carries and what
// every leg boundary resets it to.
type LiveActivityProgress struct {
	// Leg is 'pickup' or 'dropoff', "" when there is no anchor.
	Leg string
	// Source is 'nav_distance' or 'eta', "" when there is no anchor.
	Source string
	// Baseline is the reading that corresponds to progress 0, in Source's unit.
	Baseline float64
	// Value is the last fraction actually delivered to this Activity.
	Value float64
	// Reading is the raw observation Value was derived from, in Source's unit,
	// and ReadingAt is when that observation was first SEEN to hold — not when
	// the car's row was last written.
	//
	// They exist because the only timestamp on the car is `Vehicle."lastUpdated"`,
	// which is stamped by every telemetry write of any column: a poller
	// refreshing position alone (MYR-394) keeps it perpetually current while
	// `etaMinutes` and `tripDistanceRemaining` sit frozen at whatever the car
	// last said. Remembering the reading itself is how the sender's 3-minute
	// honesty horizon stays a statement about the NAV DATA rather than about the
	// row. ReadingAt is zero when no reading has been recorded yet.
	Reading   float64
	ReadingAt time.Time
}

// progressColumns is the anchor's projection plus the island-expand high-water
// mark, appended to every read that feeds a send path. Listed once so the two
// readers cannot drift apart.
//
// `alerted_phase` rides here rather than in a projection of its own because it
// answers the same class of question as the six beside it — what has this ONE
// PHONE already been shown — and because a second shared string is a second
// thing one of the two readers can forget to select.
const progressColumns = `a.progress_leg, a.progress_source, a.progress_baseline, a.progress_value,
       a.progress_reading, a.progress_reading_at, a.alerted_phase`

// scanProgress normalises the nullable columns into the value type.
//
// Any one of the FOUR ANCHOR columns being NULL collapses the whole anchor to
// zero rather than producing a partial one: a baseline with no unit, or a unit
// with no baseline, is not a weaker anchor — it is an un-interpretable one, and
// the sender's answer to that is to re-anchor from the next trustworthy
// reading.
//
// The two READING columns are independently nullable and do not participate in
// that rule. They are an optimisation on the freshness gate, not part of the
// measurement: absent, the sender simply falls back to the row-stamp
// pre-filter, which is what it did before they existed.
func scanProgress(leg, source *string, baseline, value, reading *float64, readingAt *time.Time) LiveActivityProgress {
	if leg == nil || source == nil || baseline == nil || value == nil {
		return LiveActivityProgress{}
	}
	p := LiveActivityProgress{Leg: *leg, Source: *source, Baseline: *baseline, Value: *value}
	if reading != nil && readingAt != nil {
		p.Reading, p.ReadingAt = *reading, *readingAt
	}
	return p
}

// querySaveLiveActivityProgress writes the anchor an Activity was just shown.
//
// Deliberately does NOT touch updated_at. That column is the ticker's
// least-recently-pushed ordering key and the sweep's staleness predicate, and
// MarkActivitiesPushed owns it in one batched statement per pass; a second
// writer stamping it per row would make the rotation depend on which of two
// writes landed last for no benefit.
//
// Scoped to live rows so a write racing the ride's terminal end cannot
// resurrect rendering state onto a tombstoned Activity.
//
// THE LAST FOUR WHERE ARMS ARE THE MONOTONE GUARD, and they exist because this
// write is a read-modify-write straddling an APNs round trip on an UNSHARDED
// ticker. Two replicas list the same Activity seconds apart, both read the same
// anchor, and whichever APNs call returns LAST wins the write — which can be
// the one that computed the LOWER fraction. The floor would then record less
// than the phone has actually been shown, and the next push where the clamp is
// load-bearing would deliver a decrease: precisely the sequence the after-
// delivery write ordering exists to make impossible.
//
// Each arm keeps an unconditional write unconditional:
//
//   - `$3::TEXT IS NULL` — a CLEAR (leg boundary, terminal status) must always
//     land; it is not a lower fraction, it is the end of the measurement.
//   - `progress_leg IS DISTINCT FROM $3` / `progress_source IS DISTINCT FROM $4`
//     — a new leg or a new unit is a different measurement, and comparing its
//     fraction against the old one's would be comparing nothing to nothing.
//   - `progress_value IS NULL` — the first anchor of a leg.
//
// Only when all of those agree does the last arm apply, and it applies the one
// rule the contract states: within one leg on one unit, the stored floor never
// goes down. A losing write is silently a no-op, which is correct — the value
// it wanted to record is one the phone has already been shown a better version
// of.
const querySaveLiveActivityProgress = `
UPDATE go_live_activities
SET progress_leg        = $3,
    progress_source     = $4,
    progress_baseline   = $5,
    progress_value      = $6,
    progress_reading    = $7,
    progress_reading_at = $8
WHERE ride_request_id = $1 AND user_id = $2 AND ended_at IS NULL
  AND ($3::TEXT IS NULL
       OR progress_leg IS DISTINCT FROM $3
       OR progress_source IS DISTINCT FROM $4
       OR progress_value IS NULL
       OR progress_value <= $6)`

// SaveActivityProgress records one Activity's leg-progress anchor.
//
// An empty Leg clears all four columns back to NULL, which is how a leg
// boundary and a terminal status erase leg one's measurement before leg two
// starts taking its own.
//
// A miss (no live row) is not an error: the Activity ended between the send and
// this write, which is an ordinary race and costs nothing — the row will never
// be pushed to again.
func (r *LiveActivityRepo) SaveActivityProgress(ctx context.Context, key LiveActivityKey, p LiveActivityProgress) error {
	if strings.TrimSpace(key.RideRequestID) == "" || strings.TrimSpace(key.UserID) == "" {
		return fmt.Errorf("store.SaveActivityProgress: empty ride request id or user id")
	}

	// Both halves or neither. A leg with no source names a measurement that
	// does not exist, and the CHECK constraints would refuse it anyway — better
	// to store the honest "no anchor" than to fail a write on the send path.
	var leg, source *string
	var baseline, value, reading *float64
	var readingAt *time.Time
	if p.Leg != "" && p.Source != "" {
		leg, source, baseline, value = &p.Leg, &p.Source, &p.Baseline, &p.Value
		// The reading pair travels with the anchor or not at all: a raw
		// observation with no measurement to belong to has nothing to say.
		if !p.ReadingAt.IsZero() {
			reading, readingAt = &p.Reading, &p.ReadingAt
		}
	}

	if _, err := r.pool.Exec(ctx, querySaveLiveActivityProgress,
		key.RideRequestID, key.UserID, leg, source, baseline, value, reading, readingAt,
	); err != nil {
		return fmt.Errorf("store.SaveActivityProgress(ride=%s, user=%s): %w",
			key.RideRequestID, key.UserID, err)
	}
	return nil
}

// querySaveLiveActivityAlertedPhase raises one Activity's island-expand
// high-water mark (MYR-398, migration 0029).
//
// `alerted_phase < $3` IS THE GUARD AND IT IS THE SAME SHAPE AS THE PROGRESS
// FLOOR'S, for the same unsharded-ticker reason: two replicas can list the same
// Activity seconds apart, and whichever APNs call returns last wins the write.
// If that is the one that computed the LOWER phase, an unguarded write would
// lower the mark and the higher phase would alert a second time — the island
// opening twice for one state change, which is the exact failure this column
// exists to prevent. A losing write is silently a no-op, which is correct: the
// phone has already been shown a further phase than the one it wants to record.
//
// Scoped to live rows, like its neighbour, so a write racing the ride's
// terminal end cannot resurrect rendering state onto a tombstoned Activity.
//
// It deliberately does NOT touch updated_at: that column is the ticker's
// least-recently-pushed ordering key and MarkActivitiesPushed owns it in one
// batched statement per pass.
const querySaveLiveActivityAlertedPhase = `
UPDATE go_live_activities
SET alerted_phase = $3
WHERE ride_request_id = $1 AND user_id = $2 AND ended_at IS NULL
  AND alerted_phase < $3`

// SaveActivityAlertedPhase records that this Activity's island has been
// expanded for `phase`, refusing to lower an existing mark.
//
// A miss (no live row, or a mark already at or above `phase`) is not an error:
// both are ordinary races on an unsharded ticker, and both mean the same thing
// to the caller — the phone is not owed this expansion.
func (r *LiveActivityRepo) SaveActivityAlertedPhase(ctx context.Context, key LiveActivityKey, phase int16) error {
	if strings.TrimSpace(key.RideRequestID) == "" || strings.TrimSpace(key.UserID) == "" {
		return fmt.Errorf("store.SaveActivityAlertedPhase: empty ride request id or user id")
	}
	if phase <= 0 {
		// Zero is "never alerted" and is only ever a starting value; a negative
		// is a caller bug. Neither is something to write, and the CHECK
		// constraint would refuse the negative on the send path.
		return fmt.Errorf("store.SaveActivityAlertedPhase(ride=%s, user=%s): non-positive phase %d",
			key.RideRequestID, key.UserID, phase)
	}

	if _, err := r.pool.Exec(ctx, querySaveLiveActivityAlertedPhase,
		key.RideRequestID, key.UserID, phase,
	); err != nil {
		return fmt.Errorf("store.SaveActivityAlertedPhase(ride=%s, user=%s): %w",
			key.RideRequestID, key.UserID, err)
	}
	return nil
}
