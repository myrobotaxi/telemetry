package store

import (
	"context"
	"errors"
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

// discard drops any buffered points for a drive without persisting
// them. Used when the drive detector discards a micro-drive (MYR-160):
// the Drive row is about to be deleted, so flushing the points would
// resurrect data for a dead drive.
func (rb *routeBuffer) discard(driveID string) {
	rb.mu.Lock()
	delete(rb.buffers, driveID)
	rb.mu.Unlock()
}

// flushDrive writes all buffered points for a single drive to the
// database and removes them from the buffer.
//
// Failures split two ways, and the split matters (MYR-433):
//
//   - TRANSIENT (a dropped connection, a pool timeout): re-buffer and
//     retry on the next tick. This is the original behaviour and it is
//     right — the points are still good, the database just blipped.
//
//   - PERMANENT (ErrRouteTrailUnreadable, ErrEncryptionRequired): DROP
//     the batch and free the entry. Retrying cannot help: the drive's
//     stored trail will not decrypt, and it will not start decrypting on
//     the next tick.
//
// Retrying a permanent failure is not merely useless, it is actively
// harmful. `add` returns true once a drive holds FlushSize points, and
// the writer then flushes SYNCHRONOUSLY on the GPS frame — so at 1Hz a
// poisoned drive would attempt a doomed Begin + FOR UPDATE + decrypt +
// Rollback on every single sample, while its buffer grows without bound
// (nothing but drive.discarded ever frees the entry) holding a plaintext
// P1 coordinate trail in memory, and keeps retrying every 10s forever
// after the drive has ended.
//
// Dropping loses that drive's remaining points. That is the correct
// trade: the trail they would be appended to is already unreadable, so
// they are being added to something nobody can use, and the loud Error
// plus the decrypt-failure counter is what actually gets the key or the
// row fixed.
func (rb *routeBuffer) flushDrive(ctx context.Context, driveID string) {
	rb.mu.Lock()
	pts := rb.buffers[driveID]
	delete(rb.buffers, driveID)
	rb.mu.Unlock()

	if len(pts) == 0 {
		return
	}

	err := rb.drives.AppendRoutePoints(ctx, driveID, pts)
	if err == nil {
		rb.logger.Debug("flushed route points",
			slog.String("drive_id", driveID),
			slog.Int("points", len(pts)),
		)
		return
	}

	if isPermanentRouteFlushError(err) {
		rb.logger.Error("dropping buffered route points: the drive's stored trail is unreadable",
			slog.String("drive_id", driveID),
			slog.Int("points_dropped", len(pts)),
			slog.String("error", err.Error()),
			slog.String("action", "check ENCRYPTION_KEY and telemetry_store_decrypt_failures_total"),
		)
		return // entry already deleted above — do NOT re-buffer
	}

	rb.logger.Warn("failed to flush buffered route points; will retry",
		slog.String("drive_id", driveID),
		slog.Int("points", len(pts)),
		slog.String("error", err.Error()),
	)
	rb.mu.Lock()
	rb.buffers[driveID] = append(pts, rb.buffers[driveID]...)
	rb.mu.Unlock()
}

// isPermanentRouteFlushError reports whether a flush failure can never
// succeed on retry, so the batch must be dropped rather than re-buffered.
//
// ErrDriveNotFound is deliberately NOT here. It is usually a race with a
// micro-drive discard that `discard` handles, and the retry is harmless
// and short-lived.
func isPermanentRouteFlushError(err error) bool {
	return errors.Is(err, ErrRouteTrailUnreadable) || errors.Is(err, ErrEncryptionRequired)
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
