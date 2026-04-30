package ws

import (
	"sync"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// defaultGroupFlushInterval is the time window for accumulating fields of
// an atomic group before flushing them as a single vehicle_update message.
//
// 500 ms aligns with Tesla's vehicle-side emission bucket floor (per
// docs/contracts/vehicle-state-schema.md §2.2). Server windows shorter
// than 500 ms cannot improve atomicity because Tesla itself cannot deliver
// companion fields any faster.
const defaultGroupFlushInterval = 500 * time.Millisecond

// groupKey identifies an accumulator slot. State is per (atomic group, VIN)
// so each vehicle's per-group batch is independent — one VIN's nav fields
// never mix with another VIN's nav fields, and the navigation batch never
// mixes with another atomic group's batch.
type groupKey struct {
	group atomicGroupID
	vin   string
}

// groupAccumulator buffers telemetry fields belonging to atomic groups
// before delivering them via the onFlush callback. It prevents partial
// group updates from reaching SDK clients (NFR-3.3, NFR-3.4) by holding
// fields for flushInterval after the first arrival in the slot. All
// fields received within that window are merged and delivered together.
//
// In v1 only the navigation group registers an accumulator — charge
// relies on Tesla's 500 ms upstream bucket atomicity, and GPS/gear emit
// synchronously (vehicle-state-schema.md §2). The accumulator is
// generalized over groupID so future groups can opt in without a refactor.
type groupAccumulator struct {
	mu            sync.Mutex
	pending       map[groupKey]map[string]events.TelemetryValue
	timers        map[groupKey]*time.Timer
	flushInterval time.Duration
	onFlush       func(group atomicGroupID, vin string, fields map[string]events.TelemetryValue)
}

// newGroupAccumulator creates a groupAccumulator with the given flush
// interval and callback. The onFlush callback is invoked when a slot's
// time window expires, delivering all accumulated fields for that
// (group, VIN) pair. Callers must filter incoming fields to a single
// group before calling Add — the accumulator does not split mixed input.
func newGroupAccumulator(interval time.Duration, onFlush func(atomicGroupID, string, map[string]events.TelemetryValue)) *groupAccumulator {
	return &groupAccumulator{
		pending:       make(map[groupKey]map[string]events.TelemetryValue),
		timers:        make(map[groupKey]*time.Timer),
		flushInterval: interval,
		onFlush:       onFlush,
	}
}

// Add merges fields into the pending batch for the given (group, VIN). If
// no timer is running for this slot, one is started. When the timer fires,
// onFlush receives all accumulated fields. Last-write-wins for duplicate
// field keys within the window.
func (a *groupAccumulator) Add(group atomicGroupID, vin string, fields map[string]events.TelemetryValue) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := groupKey{group: group, vin: vin}
	if a.pending[key] == nil {
		a.pending[key] = make(map[string]events.TelemetryValue, len(fields))
	}

	for k, v := range fields {
		a.pending[key][k] = v
	}

	if _, hasTimer := a.timers[key]; !hasTimer {
		a.timers[key] = time.AfterFunc(a.flushInterval, func() {
			a.timerFired(key)
		})
	}
}

// timerFired is called when the flush timer expires for a slot. It grabs
// the pending fields, clears state, and invokes the callback outside the
// lock to avoid holding it during potentially slow downstream work.
func (a *groupAccumulator) timerFired(key groupKey) {
	a.mu.Lock()
	fields := a.pending[key]
	delete(a.pending, key)
	delete(a.timers, key)
	a.mu.Unlock()

	if len(fields) > 0 && a.onFlush != nil {
		a.onFlush(key.group, key.vin, fields)
	}
}

// Flush force-returns and clears all pending fields for the given
// (group, VIN), cancelling any running timer. Returns nil if nothing is
// pending. Used to drain pending state on drive-end so the final batch
// reaches clients before the drive_ended frame.
func (a *groupAccumulator) Flush(group atomicGroupID, vin string) map[string]events.TelemetryValue {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := groupKey{group: group, vin: vin}
	if timer, ok := a.timers[key]; ok {
		timer.Stop()
		delete(a.timers, key)
	}

	fields := a.pending[key]
	delete(a.pending, key)

	if len(fields) == 0 {
		return nil
	}
	return fields
}

// Clear removes all state for the given (group, VIN), cancelling any
// running timer. Called on connectivity disconnect to avoid broadcasting
// stale data on reconnect.
func (a *groupAccumulator) Clear(group atomicGroupID, vin string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := groupKey{group: group, vin: vin}
	if timer, ok := a.timers[key]; ok {
		timer.Stop()
		delete(a.timers, key)
	}
	delete(a.pending, key)
}

// Stop cancels all pending timers across all slots. Called during
// Broadcaster shutdown to prevent timer callbacks from racing with
// teardown. Does not invoke onFlush for pending fields.
func (a *groupAccumulator) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for key, timer := range a.timers {
		timer.Stop()
		delete(a.timers, key)
	}
	for key := range a.pending {
		delete(a.pending, key)
	}
}
