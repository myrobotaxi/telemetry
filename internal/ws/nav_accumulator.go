package ws

import (
	"sync"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// defaultNavFlushInterval is the time window for accumulating navigation
// fields before broadcasting them as a single vehicle_update message.
const defaultNavFlushInterval = 500 * time.Millisecond

// navAccumulator collects navigation-related telemetry fields within a
// time window per VIN before triggering a flush callback. This prevents
// the frontend from receiving partial nav updates (e.g. route without
// destination name) that cause UI race conditions.
//
// When the first nav field arrives for a VIN, a timer is started. All
// subsequent nav fields for that VIN within the window are merged into
// the pending batch. When the timer fires, the accumulated fields are
// delivered via the onFlush callback and the state is cleared.
type navAccumulator struct {
	mu            sync.Mutex
	pending       map[string]map[string]events.TelemetryValue // VIN -> field name -> value
	timers        map[string]*time.Timer
	flushInterval time.Duration
	onFlush       func(vin string, fields map[string]events.TelemetryValue)
}

// newNavAccumulator creates a navAccumulator with the given flush interval
// and callback. The onFlush callback is invoked when the time window
// expires, delivering all accumulated fields for that VIN.
func newNavAccumulator(interval time.Duration, onFlush func(string, map[string]events.TelemetryValue)) *navAccumulator {
	return &navAccumulator{
		pending:       make(map[string]map[string]events.TelemetryValue),
		timers:        make(map[string]*time.Timer),
		flushInterval: interval,
		onFlush:       onFlush,
	}
}

// Add merges nav fields into the pending batch for the given VIN. If no
// timer is running for this VIN, one is started. When the timer fires,
// the onFlush callback receives all accumulated fields.
func (a *navAccumulator) Add(vin string, fields map[string]events.TelemetryValue) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.pending[vin] == nil {
		a.pending[vin] = make(map[string]events.TelemetryValue, len(fields))
	}

	// Merge fields into pending batch (last-write-wins for duplicate keys).
	for k, v := range fields {
		a.pending[vin][k] = v
	}

	// Start a timer if one isn't already running for this VIN.
	if _, hasTimer := a.timers[vin]; !hasTimer {
		a.timers[vin] = time.AfterFunc(a.flushInterval, func() {
			a.timerFired(vin)
		})
	}
}

// timerFired is called when the flush timer expires for a VIN. It grabs
// the pending fields, clears state, and invokes the callback outside the
// lock to avoid holding it during potentially slow VIN resolution.
func (a *navAccumulator) timerFired(vin string) {
	a.mu.Lock()
	fields := a.pending[vin]
	delete(a.pending, vin)
	delete(a.timers, vin)
	a.mu.Unlock()

	if len(fields) > 0 && a.onFlush != nil {
		a.onFlush(vin, fields)
	}
}

// Flush force-returns and clears all pending fields for the given VIN,
// cancelling any running timer. Returns nil if nothing is pending.
func (a *navAccumulator) Flush(vin string) map[string]events.TelemetryValue {
	a.mu.Lock()
	defer a.mu.Unlock()

	if timer, ok := a.timers[vin]; ok {
		timer.Stop()
		delete(a.timers, vin)
	}

	fields := a.pending[vin]
	delete(a.pending, vin)

	if len(fields) == 0 {
		return nil
	}
	return fields
}

// Clear removes all state for the given VIN, cancelling any running
// timer. Called on drive end or connectivity disconnect.
func (a *navAccumulator) Clear(vin string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if timer, ok := a.timers[vin]; ok {
		timer.Stop()
		delete(a.timers, vin)
	}
	delete(a.pending, vin)
}
