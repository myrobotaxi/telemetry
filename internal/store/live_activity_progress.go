package store

import (
	"context"
	"fmt"
	"strings"
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
}

// progressColumns is the anchor's projection, appended to every read that
// feeds a send path. Listed once so the two readers cannot drift apart.
const progressColumns = `a.progress_leg, a.progress_source, a.progress_baseline, a.progress_value`

// scanProgress normalises the four nullable columns into the value type.
//
// Any one of them being NULL collapses the whole anchor to zero rather than
// producing a partial one: a baseline with no unit, or a unit with no baseline,
// is not a weaker anchor — it is an un-interpretable one, and the sender's
// answer to that is to re-anchor from the next trustworthy reading.
func scanProgress(leg, source *string, baseline, value *float64) LiveActivityProgress {
	if leg == nil || source == nil || baseline == nil || value == nil {
		return LiveActivityProgress{}
	}
	return LiveActivityProgress{Leg: *leg, Source: *source, Baseline: *baseline, Value: *value}
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
const querySaveLiveActivityProgress = `
UPDATE go_live_activities
SET progress_leg      = $3,
    progress_source   = $4,
    progress_baseline = $5,
    progress_value    = $6
WHERE ride_request_id = $1 AND user_id = $2 AND ended_at IS NULL`

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
	var baseline, value *float64
	if p.Leg != "" && p.Source != "" {
		leg, source, baseline, value = &p.Leg, &p.Source, &p.Baseline, &p.Value
	}

	if _, err := r.pool.Exec(ctx, querySaveLiveActivityProgress,
		key.RideRequestID, key.UserID, leg, source, baseline, value,
	); err != nil {
		return fmt.Errorf("store.SaveActivityProgress(ride=%s, user=%s): %w",
			key.RideRequestID, key.UserID, err)
	}
	return nil
}
