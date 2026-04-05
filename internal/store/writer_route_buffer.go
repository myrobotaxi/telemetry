package store

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// RouteBufferConfig holds tunable parameters for the route point buffer.
type RouteBufferConfig struct {
	FlushInterval time.Duration // how often to flush buffered points
	FlushSize     int           // flush when a drive accumulates this many points
}

// DefaultRouteBufferConfig returns production-ready defaults.
func DefaultRouteBufferConfig() RouteBufferConfig {
	return RouteBufferConfig{
		FlushInterval: 10 * time.Second,
		FlushSize:     10,
	}
}

// routeBuffer accumulates route points per drive and flushes them to the
// database in batches. This avoids writing a DB row per GPS sample while
// keeping the frontend up-to-date during active drives.
type routeBuffer struct {
	drives drivePersister
	logger *slog.Logger
	cfg    RouteBufferConfig

	mu      sync.Mutex
	buffers map[string][]RoutePointRecord // driveID → buffered points

	cancel    context.CancelFunc
	flushDone chan struct{}
}

// newRouteBuffer creates a routeBuffer. Call start() to begin the periodic
// flush goroutine, and stop() to drain and shut down.
func newRouteBuffer(drives drivePersister, logger *slog.Logger, cfg RouteBufferConfig) *routeBuffer {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultRouteBufferConfig().FlushInterval
	}
	if cfg.FlushSize <= 0 {
		cfg.FlushSize = DefaultRouteBufferConfig().FlushSize
	}
	return &routeBuffer{
		drives:    drives,
		logger:    logger,
		cfg:       cfg,
		buffers:   make(map[string][]RoutePointRecord),
		flushDone: make(chan struct{}),
	}
}

// start launches the periodic flush goroutine.
func (rb *routeBuffer) start(ctx context.Context) {
	flushCtx, cancel := context.WithCancel(ctx)
	rb.cancel = cancel
	go func() {
		defer cancel()
		defer close(rb.flushDone)
		rb.flushLoop(flushCtx)
	}()
}

// stop cancels the flush goroutine and waits for it to exit.
func (rb *routeBuffer) stop() {
	if rb.cancel != nil {
		rb.cancel()
	}
	<-rb.flushDone
}

// add buffers a route point for the given drive. Returns true if the
// buffer for this drive has reached the flush size threshold.
func (rb *routeBuffer) add(driveID string, pt RoutePointRecord) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.buffers[driveID] = append(rb.buffers[driveID], pt)
	return len(rb.buffers[driveID]) >= rb.cfg.FlushSize
}

// flushDrive writes all buffered points for a single drive to the database
// and removes them from the buffer.
func (rb *routeBuffer) flushDrive(ctx context.Context, driveID string) {
	rb.mu.Lock()
	pts := rb.buffers[driveID]
	delete(rb.buffers, driveID)
	rb.mu.Unlock()

	if len(pts) == 0 {
		return
	}

	if err := rb.drives.AppendRoutePoints(ctx, driveID, pts); err != nil {
		rb.logger.Warn("failed to flush buffered route points",
			slog.String("drive_id", driveID),
			slog.Int("points", len(pts)),
			slog.String("error", err.Error()),
		)
		// Re-buffer the points so they can be retried on next flush.
		rb.mu.Lock()
		rb.buffers[driveID] = append(pts, rb.buffers[driveID]...)
		rb.mu.Unlock()
		return
	}

	rb.logger.Debug("flushed route points",
		slog.String("drive_id", driveID),
		slog.Int("points", len(pts)),
	)
}

// flushAll writes all buffered points for all drives to the database.
func (rb *routeBuffer) flushAll(ctx context.Context) {
	rb.mu.Lock()
	driveIDs := make([]string, 0, len(rb.buffers))
	for id := range rb.buffers {
		driveIDs = append(driveIDs, id)
	}
	rb.mu.Unlock()

	for _, id := range driveIDs {
		rb.flushDrive(ctx, id)
	}
}

// flushLoop runs a ticker that calls flushAll at each interval until the
// context is cancelled.
func (rb *routeBuffer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(rb.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rb.flushAll(ctx)
		}
	}
}
