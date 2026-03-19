package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// vehicleUpdater is the consumer-site interface for writing vehicle
// telemetry updates to the database.
type vehicleUpdater interface {
	UpdateTelemetry(ctx context.Context, vin string, update VehicleUpdate) error
	UpdateStatus(ctx context.Context, vin string, status VehicleStatus) error
}

// drivePersister is the consumer-site interface for persisting drive
// records to the database.
type drivePersister interface {
	Create(ctx context.Context, drive DriveRecord) error
	Complete(ctx context.Context, driveID string, stats DriveCompletion) error
	AppendRoutePoints(ctx context.Context, driveID string, points []RoutePointRecord) error
}

// WriterConfig holds tunable parameters for the Writer's batch flush behavior.
type WriterConfig struct {
	FlushInterval time.Duration
	BatchSize     int
}

// DefaultWriterConfig returns production-ready defaults.
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{
		FlushInterval: 5 * time.Second,
		BatchSize:     100,
	}
}

// Writer subscribes to telemetry and drive events on the event bus and
// persists them to the database. Telemetry updates are coalesced per VIN
// and flushed in batches on a timer or when the batch size is reached.
type Writer struct {
	vehicles vehicleUpdater
	drives   drivePersister
	bus      events.Bus
	vinCache *vinCache
	logger   *slog.Logger
	cfg      WriterConfig

	pendingMu sync.Mutex
	pending   map[string]*VehicleUpdate // VIN → coalesced update
	count     int                       // total telemetry events since last flush

	subs      []events.Subscription
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	flushDone chan struct{} // closed when flushLoop goroutine exits
}

// NewWriter creates a Writer that will subscribe to telemetry and drive
// events, coalesce vehicle updates, and flush them periodically.
func NewWriter(
	vehicles vehicleUpdater,
	drives drivePersister,
	vinLookup vinLookup,
	bus events.Bus,
	logger *slog.Logger,
	cfg WriterConfig,
) *Writer {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultWriterConfig().FlushInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultWriterConfig().BatchSize
	}
	return &Writer{
		vehicles: vehicles,
		drives:   drives,
		bus:      bus,
		vinCache: newVINCache(vinLookup, logger),
		logger:   logger,
		cfg:      cfg,
		pending:  make(map[string]*VehicleUpdate),
		done:      make(chan struct{}),
		flushDone: make(chan struct{}),
	}
}

// Start subscribes to telemetry and drive events and launches the flush
// ticker goroutine. It blocks until subscriptions are registered, then
// returns. Call Stop to shut down.
func (w *Writer) Start(ctx context.Context) error {
	telSub, err := w.bus.Subscribe(events.TopicVehicleTelemetry, w.handleTelemetry)
	if err != nil {
		return fmt.Errorf("Writer.Start: subscribe telemetry: %w", err)
	}

	startSub, err := w.bus.Subscribe(events.TopicDriveStarted, w.handleDriveStarted(ctx))
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		return fmt.Errorf("Writer.Start: subscribe drive.started: %w", err)
	}

	endSub, err := w.bus.Subscribe(events.TopicDriveEnded, w.handleDriveEnded(ctx))
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		_ = w.bus.Unsubscribe(startSub)
		return fmt.Errorf("Writer.Start: subscribe drive.ended: %w", err)
	}

	w.subs = []events.Subscription{telSub, startSub, endSub}

	tickCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go func() {
		defer cancel() // also called via Stop(); cancel is idempotent
		defer close(w.flushDone)
		w.flushLoop(tickCtx)
	}()

	w.logger.Info("store writer started",
		slog.Duration("flush_interval", w.cfg.FlushInterval),
		slog.Int("batch_size", w.cfg.BatchSize),
	)
	return nil
}

// Stop unsubscribes from all events, flushes any remaining pending
// updates, and stops the flush ticker.
func (w *Writer) Stop() error {
	if w.cancel != nil {
		w.cancel()
	}

	// Wait for flushLoop goroutine to exit.
	<-w.flushDone

	for _, sub := range w.subs {
		if err := w.bus.Unsubscribe(sub); err != nil {
			w.logger.Warn("failed to unsubscribe",
				slog.String("subscription_id", sub.ID),
				slog.String("error", err.Error()),
			)
		}
	}

	// Final flush with a short deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.flush(ctx)

	w.closeOnce.Do(func() { close(w.done) })

	w.logger.Info("store writer stopped")
	return nil
}

// Done returns a channel that is closed after Stop completes.
func (w *Writer) Done() <-chan struct{} {
	return w.done
}

// handleTelemetry is the event handler for VehicleTelemetryEvent. It
// extracts fields, maps them to a VehicleUpdate, and coalesces into
// the pending map. If the batch size is reached, it triggers a flush.
func (w *Writer) handleTelemetry(event events.Event) {
	telEvt, ok := event.Payload.(events.VehicleTelemetryEvent)
	if !ok {
		w.logger.Error("unexpected payload type for telemetry event",
			slog.String("event_id", event.ID),
		)
		return
	}

	update := mapTelemetryToUpdate(telEvt.Fields)
	if update == nil {
		return
	}
	update.LastUpdated = telEvt.CreatedAt

	shouldFlush := w.coalesce(telEvt.VIN, update)
	if shouldFlush {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w.flush(ctx)
	}
}

// coalesce merges an update into the pending map for the given VIN.
// Returns true if the total event count has reached the batch size.
func (w *Writer) coalesce(vin string, update *VehicleUpdate) bool {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	existing, ok := w.pending[vin]
	if !ok {
		w.pending[vin] = update
	} else {
		mergeUpdate(existing, update)
	}
	w.count++
	return w.count >= w.cfg.BatchSize
}

// mergeUpdate applies non-nil fields from src onto dst (latest wins).
func mergeUpdate(dst, src *VehicleUpdate) {
	if src.Speed != nil {
		dst.Speed = src.Speed
	}
	if src.ChargeLevel != nil {
		dst.ChargeLevel = src.ChargeLevel
	}
	if src.EstimatedRange != nil {
		dst.EstimatedRange = src.EstimatedRange
	}
	if src.GearPosition != nil {
		dst.GearPosition = src.GearPosition
	}
	if src.Heading != nil {
		dst.Heading = src.Heading
	}
	if src.Latitude != nil {
		dst.Latitude = src.Latitude
	}
	if src.Longitude != nil {
		dst.Longitude = src.Longitude
	}
	if src.InteriorTemp != nil {
		dst.InteriorTemp = src.InteriorTemp
	}
	if src.ExteriorTemp != nil {
		dst.ExteriorTemp = src.ExteriorTemp
	}
	if src.OdometerMiles != nil {
		dst.OdometerMiles = src.OdometerMiles
	}
	if src.LocationName != nil {
		dst.LocationName = src.LocationName
	}
	if src.LocationAddr != nil {
		dst.LocationAddr = src.LocationAddr
	}
	// Always take the later timestamp.
	if src.LastUpdated.After(dst.LastUpdated) {
		dst.LastUpdated = src.LastUpdated
	}
}

// flushLoop runs a ticker that calls flush at each interval until the
// context is cancelled.
func (w *Writer) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

// flush drains the pending map and writes each VIN's coalesced update
// to the database. Errors are logged but do not halt the writer.
func (w *Writer) flush(ctx context.Context) {
	w.pendingMu.Lock()
	if len(w.pending) == 0 {
		w.pendingMu.Unlock()
		return
	}
	batch := w.pending
	w.pending = make(map[string]*VehicleUpdate)
	w.count = 0
	w.pendingMu.Unlock()

	w.logger.Debug("flushing telemetry batch",
		slog.Int("vehicles", len(batch)),
	)

	for vin, update := range batch {
		if err := w.vehicles.UpdateTelemetry(ctx, vin, *update); err != nil {
			w.logger.Warn("failed to write telemetry update",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
	}
}

