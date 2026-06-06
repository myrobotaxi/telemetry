package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/geocode"
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
	RouteBuffer   RouteBufferConfig

	// LocationGeocodeDebounce is the minimum time between successive
	// reverse-geocode calls for the SAME VIN's current location
	// (Vehicle.locationName / Vehicle.locationAddress). A stable parked
	// vehicle streams (lat, lng) every flush window; we don't want to
	// burn the Mapbox quota on coordinates that haven't moved.
	// Defaults to 60s when zero — see DefaultWriterConfig.
	LocationGeocodeDebounce time.Duration

	// LocationGeocodeMinMeters is the minimum great-circle distance (in
	// meters) the current location must move before triggering a new
	// reverse-geocode call. Below this threshold the cached address is
	// kept and the row is left alone. Defaults to 50m when zero — see
	// DefaultWriterConfig. Sits alongside the time-based debounce so
	// both rate-AND-distance must pass to fire.
	LocationGeocodeMinMeters float64
}

// DefaultWriterConfig returns production-ready defaults.
func DefaultWriterConfig() WriterConfig {
	return WriterConfig{
		FlushInterval:            5 * time.Second,
		BatchSize:                100,
		RouteBuffer:              DefaultRouteBufferConfig(),
		LocationGeocodeDebounce:  defaultLocationGeocodeDebounce,
		LocationGeocodeMinMeters: defaultLocationGeocodeMinMeters,
	}
}

// Writer subscribes to telemetry and drive events on the event bus and
// persists them to the database. Telemetry updates are coalesced per VIN
// and flushed in batches on a timer or when the batch size is reached.
type Writer struct {
	vehicles vehicleUpdater
	drives   drivePersister
	bus      events.Bus
	vinCache *VINCache
	geocoder geocode.Geocoder
	logger   *slog.Logger
	cfg      WriterConfig
	routeBuf *routeBuffer

	pendingMu sync.Mutex
	pending   map[string]*VehicleUpdate // VIN → coalesced update
	count     int                       // total telemetry events since last flush

	// destAddrCache memoizes the most recent (lat, lng) → address
	// reverse-geocode result per VIN so a stable navigation destination
	// streamed every flush window does not burn the Mapbox rate budget
	// or cycle the same address through the DB on each interval. Cleared
	// for a VIN when the destination GPS is cleared (navigation cancelled
	// — atomic group clear per data-lifecycle.md §6.1).
	//
	// TODO: bound the cache size. Today every VIN that ever streamed a
	// destination keeps an entry forever; for a small fleet this is
	// fine but it grows monotonically. Cheap mitigations (file in a
	// follow-up issue): drop the entry on disconnect when other
	// per-VIN state is cleaned up, or wrap the map in an LRU.
	destAddrMu    sync.Mutex
	destAddrCache map[string]destAddrEntry

	// locAddrMu guards locAddrCache, which tracks the most recent
	// reverse-geocode call per VIN for the current vehicle location.
	// Unlike destAddrCache (destination), this cache records WHEN the
	// last geocode fired and whether a goroutine is currently in flight
	// for that VIN — both inputs to the debounce decision. See
	// writer_location_address.go.
	locAddrMu    sync.Mutex
	locAddrCache map[string]locAddrEntry

	// geocodeWG tracks async location-reverse-geocode goroutines so
	// Stop() can wait for them to finish before the writer is torn down.
	// Without this, a slow Mapbox call in flight at shutdown could write
	// to a pool whose connections were already closed.
	geocodeWG sync.WaitGroup

	subs      []events.Subscription
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	flushDone chan struct{} // closed when flushLoop goroutine exits
}

// NewWriter creates a Writer that will subscribe to telemetry and drive
// events, coalesce vehicle updates, and flush them periodically. The
// geocoder is used to reverse geocode drive start/end locations. Pass
// geocode.NoopGeocoder{} to disable geocoding.
func NewWriter(
	vehicles vehicleUpdater,
	drives drivePersister,
	vinLookup vinIDLookup,
	bus events.Bus,
	geocoder geocode.Geocoder,
	logger *slog.Logger,
	cfg WriterConfig,
) *Writer {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultWriterConfig().FlushInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultWriterConfig().BatchSize
	}
	if cfg.LocationGeocodeDebounce <= 0 {
		cfg.LocationGeocodeDebounce = defaultLocationGeocodeDebounce
	}
	if cfg.LocationGeocodeMinMeters <= 0 {
		cfg.LocationGeocodeMinMeters = defaultLocationGeocodeMinMeters
	}
	return &Writer{
		vehicles:      vehicles,
		drives:        drives,
		bus:           bus,
		vinCache:      NewVINCache(vinLookup, logger),
		geocoder:      geocoder,
		logger:        logger,
		cfg:           cfg,
		routeBuf:      newRouteBuffer(drives, logger, cfg.RouteBuffer),
		pending:       make(map[string]*VehicleUpdate),
		destAddrCache: make(map[string]destAddrEntry),
		locAddrCache:  make(map[string]locAddrEntry),
		done:          make(chan struct{}),
		flushDone:     make(chan struct{}),
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

	startSub, err := w.bus.Subscribe(events.TopicDriveStarted, w.handleDriveStarted())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		return fmt.Errorf("Writer.Start: subscribe drive.started: %w", err)
	}

	updatedSub, err := w.bus.Subscribe(events.TopicDriveUpdated, w.handleDriveUpdated())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		_ = w.bus.Unsubscribe(startSub)
		return fmt.Errorf("Writer.Start: subscribe drive.updated: %w", err)
	}

	endSub, err := w.bus.Subscribe(events.TopicDriveEnded, w.handleDriveEnded())
	if err != nil {
		_ = w.bus.Unsubscribe(telSub)
		_ = w.bus.Unsubscribe(startSub)
		_ = w.bus.Unsubscribe(updatedSub)
		return fmt.Errorf("Writer.Start: subscribe drive.ended: %w", err)
	}

	w.subs = []events.Subscription{telSub, startSub, updatedSub, endSub}

	// #nosec G118 -- cancel is deferred in the goroutine and also stored in w.cancel for Stop()
	tickCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go func() {
		defer cancel()
		defer close(w.flushDone)
		w.flushLoop(tickCtx)
	}()

	w.routeBuf.start(ctx)

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

	// Stop the route buffer and flush any remaining buffered points.
	w.routeBuf.stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	w.routeBuf.flushAll(shutdownCtx)

	// Final telemetry flush with a short deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.flush(ctx)

	// Wait for any in-flight async location-reverse-geocode goroutines
	// to finish writing their Vehicle row updates before declaring the
	// writer stopped. The Mapbox client honors per-call timeouts so the
	// wait is bounded; ignoring this race risks writes against a torn-down
	// connection pool on shutdown.
	w.geocodeWG.Wait()

	w.closeOnce.Do(func() { close(w.done) })

	w.logger.Info("store writer stopped")
	return nil
}

// Done returns a channel that is closed after Stop completes.
func (w *Writer) Done() <-chan struct{} {
	return w.done
}
