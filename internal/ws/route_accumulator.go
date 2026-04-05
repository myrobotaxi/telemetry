package ws

import (
	"sync"
	"time"
)

// routeCoordinate is a single GPS point stored in the accumulator.
type routeCoordinate struct {
	Longitude float64
	Latitude  float64
}

// routeAccumulator keeps a running GPS trail per vehicle (keyed by VIN) and
// periodically signals the broadcaster to send the full trail to WebSocket
// clients. The buffer is never cleared on flush — it grows for the duration
// of the drive so each broadcast contains the complete driven path. Only
// Clear (called on drive end) resets the buffer.
type routeAccumulator struct {
	mu             sync.Mutex
	routes         map[string][]routeCoordinate // VIN → full driven path
	lastFlush      map[string]time.Time
	lastFlushCount map[string]int // VIN → len(routes[vin]) at last flush
	batchSize      int
	flushInterval  time.Duration
	now            func() time.Time // injectable clock for testing
}

// defaultRouteBatchSize is the number of NEW points (since last flush)
// that trigger a broadcast. At ~1 Hz GPS updates this means roughly
// 5 seconds between broadcasts.
const defaultRouteBatchSize = 5

// defaultRouteFlushInterval is the maximum time between broadcasts.
// Ensures clients receive updates even during slow GPS sample rates.
const defaultRouteFlushInterval = 3 * time.Second

// newRouteAccumulator creates a routeAccumulator. batchSize controls how
// many new points trigger an immediate broadcast; flushInterval controls
// how long before a time-based broadcast is triggered.
func newRouteAccumulator(batchSize int, flushInterval time.Duration) *routeAccumulator {
	return &routeAccumulator{
		routes:         make(map[string][]routeCoordinate),
		lastFlush:      make(map[string]time.Time),
		lastFlushCount: make(map[string]int),
		batchSize:      batchSize,
		flushInterval:  flushInterval,
		now:            time.Now,
	}
}

// addResult is returned by Add to indicate whether the caller should flush.
type addResult struct {
	ShouldFlush bool
	Points      []routeCoordinate
}

// Add appends a coordinate for the given VIN and returns whether the
// full trail should be broadcast. When ShouldFlush is true, Points
// contains the complete driven path (the buffer is NOT cleared — it
// keeps accumulating until Clear is called on drive end).
func (a *routeAccumulator) Add(vin string, coord routeCoordinate) addResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.routes[vin] = append(a.routes[vin], coord)
	now := a.now()

	newSinceLast := len(a.routes[vin]) - a.lastFlushCount[vin]
	sizeTriggered := a.batchSize > 0 && newSinceLast >= a.batchSize

	last, hasLast := a.lastFlush[vin]
	intervalTriggered := hasLast && a.flushInterval > 0 && now.Sub(last) >= a.flushInterval

	// On the very first point for a VIN, initialize the timer but don't
	// force a flush (wait for batch size or interval).
	if !hasLast {
		a.lastFlush[vin] = now
	}

	if !sizeTriggered && !intervalTriggered {
		return addResult{ShouldFlush: false}
	}

	// Return a copy of the full trail so the caller can broadcast it.
	// The buffer stays intact — we just update the flush markers.
	points := make([]routeCoordinate, len(a.routes[vin]))
	copy(points, a.routes[vin])
	a.lastFlush[vin] = now
	a.lastFlushCount[vin] = len(a.routes[vin])

	return addResult{
		ShouldFlush: true,
		Points:      points,
	}
}

// Flush returns all accumulated points for the given VIN without clearing
// the buffer. Returns nil if no points are accumulated.
func (a *routeAccumulator) Flush(vin string) []routeCoordinate {
	a.mu.Lock()
	defer a.mu.Unlock()

	points := a.routes[vin]
	if len(points) == 0 {
		return nil
	}
	out := make([]routeCoordinate, len(points))
	copy(out, points)
	a.lastFlush[vin] = a.now()
	a.lastFlushCount[vin] = len(points)
	return out
}

// Clear removes all accumulated points for the given VIN. Called when a
// drive ends.
func (a *routeAccumulator) Clear(vin string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.routes, vin)
	delete(a.lastFlush, vin)
	delete(a.lastFlushCount, vin)
}

// coordsToMapbox converts route coordinates to the [lng, lat] slice format
// expected by the frontend (Mapbox/GeoJSON convention).
func coordsToMapbox(points []routeCoordinate) [][]float64 {
	out := make([][]float64, len(points))
	for i, p := range points {
		out[i] = []float64{p.Longitude, p.Latitude}
	}
	return out
}
