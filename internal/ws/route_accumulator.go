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

// routeAccumulator batches GPS route points per vehicle (keyed by VIN) so
// the broadcaster can flush them periodically instead of sending one
// WebSocket message per route point. This prevents flooding clients during
// high-frequency telemetry (1-2 Hz GPS updates).
type routeAccumulator struct {
	mu            sync.Mutex
	routes        map[string][]routeCoordinate // VIN → accumulated points
	lastFlush     map[string]time.Time
	batchSize     int
	flushInterval time.Duration
	now           func() time.Time // injectable clock for testing
}

// defaultRouteBatchSize is the number of route points accumulated before
// flushing to WebSocket clients. At ~1 Hz GPS updates this means roughly
// 5 seconds between batches.
const defaultRouteBatchSize = 5

// defaultRouteFlushInterval is the maximum time between route batches.
// Ensures clients receive updates even during slow GPS sample rates.
const defaultRouteFlushInterval = 3 * time.Second

// newRouteAccumulator creates a routeAccumulator. batchSize controls how
// many points trigger an immediate flush; flushInterval controls how long
// before a time-based flush is triggered.
func newRouteAccumulator(batchSize int, flushInterval time.Duration) *routeAccumulator {
	return &routeAccumulator{
		routes:        make(map[string][]routeCoordinate),
		lastFlush:     make(map[string]time.Time),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		now:           time.Now,
	}
}

// addResult is returned by Add to indicate whether the caller should flush.
type addResult struct {
	ShouldFlush bool
	Points      []routeCoordinate
}

// Add appends a coordinate for the given VIN and returns whether the
// accumulated batch should be flushed. When ShouldFlush is true, Points
// contains all accumulated coordinates (the internal buffer is reset).
func (a *routeAccumulator) Add(vin string, coord routeCoordinate) addResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.routes[vin] = append(a.routes[vin], coord)
	now := a.now()

	sizeTriggered := a.batchSize > 0 && len(a.routes[vin]) >= a.batchSize

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

	points := a.routes[vin]
	a.routes[vin] = nil
	a.lastFlush[vin] = now

	return addResult{
		ShouldFlush: true,
		Points:      points,
	}
}

// Flush returns all accumulated points for the given VIN and resets the
// buffer. Returns nil if no points are accumulated.
func (a *routeAccumulator) Flush(vin string) []routeCoordinate {
	a.mu.Lock()
	defer a.mu.Unlock()

	points := a.routes[vin]
	if len(points) == 0 {
		return nil
	}
	a.routes[vin] = nil
	a.lastFlush[vin] = a.now()
	return points
}

// Clear removes all accumulated points for the given VIN. Called when a
// drive ends.
func (a *routeAccumulator) Clear(vin string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.routes, vin)
	delete(a.lastFlush, vin)
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
