// Package ridecomplete closes an autonomous ride when its car parks at the
// dropoff. It subscribes to the drive detector's DriveEndedEvent and, when the
// ending drive belongs to a vehicle with an in-flight `enroute` ride (leg 2,
// rider aboard), drives that ride enroute→completed and publishes the summary
// status change (MYR-265).
//
// Wiring lives at cmd/ (consumer-site interfaces here, adapters there) so this
// package depends only on internal/events — the same dependency-rule boundary
// the nav dispatcher (internal/dispatch) follows. DriveEndedEvent carries the
// VIN; the ride rows key on the vehicle cuid, so a VehicleResolver bridges the
// two before the guarded store transition.
package ridecomplete

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// defaultTimeout bounds one drive-end→completion pass (VIN resolve + guarded
// UPDATE + publishes). Generous relative to the two cheap DB round-trips.
const defaultTimeout = 15 * time.Second

// VehicleResolver resolves a Tesla VIN to the vehicle cuid the ride rows key
// on. Satisfied by the VIN cache via a cmd/ adapter.
type VehicleResolver interface {
	ResolveID(ctx context.Context, vin string) (string, error)
}

// CompletedRide is the minimal projection the completer needs to announce a
// ride's completion — everything RideStatusChangedEvent carries. Coordinates
// and passenger PII are deliberately absent (the summary frame never carries
// them).
type CompletedRide struct {
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	Status        string
	RequesterName *string
	UpdatedAt     time.Time
}

// Store performs the guarded enroute→completed transition for a vehicle.
// Satisfied by the ride-request repo via a cmd/ adapter. Returns the completed
// ride(s) — 0 rows when the vehicle has no in-flight enroute ride (a no-op).
type Store interface {
	CompleteEnrouteByVehicle(ctx context.Context, vehicleID string) ([]CompletedRide, error)
}

// Publisher publishes the ride status-change event onto the process bus.
// events.Bus satisfies it.
type Publisher interface {
	Publish(ctx context.Context, event events.Event) error
}

// Completer subscribes to drive.ended and completes the matching enroute ride.
// Construct with New, then Subscribe on the bus.
type Completer struct {
	resolver  VehicleResolver
	store     Store
	publisher Publisher
	logger    *slog.Logger
	timeout   time.Duration
}

// New builds a Completer. logger may be nil (a discard logger is used).
func New(resolver VehicleResolver, store Store, publisher Publisher, logger *slog.Logger) *Completer {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Completer{
		resolver:  resolver,
		store:     store,
		publisher: publisher,
		logger:    logger,
		timeout:   defaultTimeout,
	}
}

// Subscribe registers the completer on the drive.ended topic. The bus runs the
// handler in a dedicated per-subscriber goroutine with serial delivery; the
// completion work (two cheap DB round-trips) runs inline on that goroutine, so
// a slow completion only delays the NEXT drive-end, never another subscriber.
func (c *Completer) Subscribe(bus events.Bus) (events.Subscription, error) {
	sub, err := bus.Subscribe(events.TopicDriveEnded, c.handle)
	if err != nil {
		return events.Subscription{}, fmt.Errorf("ridecomplete.Subscribe: %w", err)
	}
	return sub, nil
}

// handle is the events.Handler: it type-asserts the DriveEndedEvent and runs
// one completion pass under a bounded context.
func (c *Completer) handle(evt events.Event) {
	ev, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		c.logger.Error("ridecomplete: unexpected payload type on drive.ended",
			slog.String("event_id", evt.ID),
		)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	c.complete(ctx, ev.VIN, ev.DriveID)
}

// complete resolves VIN→vehicle, transitions the vehicle's in-flight enroute
// ride to completed, and publishes a ride_status_changed per completed row. A
// drive-end for a vehicle with no active ride resolves to zero rows and is a
// clean no-op. Safe to call directly in tests.
func (c *Completer) complete(ctx context.Context, vin, driveID string) {
	if vin == "" {
		return
	}
	vehicleID, err := c.resolver.ResolveID(ctx, vin)
	if err != nil {
		// Unknown/unresolvable VIN: nothing to complete. Log opaque ids only
		// (VIN redacted to last 4 — never a coordinate or full identifier).
		c.logger.Warn("ridecomplete: vin resolution failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("drive_id", driveID),
			slog.String("error", err.Error()),
		)
		return
	}

	completed, err := c.store.CompleteEnrouteByVehicle(ctx, vehicleID)
	if err != nil {
		c.logger.Error("ridecomplete: complete enroute ride failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("drive_id", driveID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(completed) == 0 {
		// No in-flight enroute ride for this vehicle — drive-end is a no-op.
		return
	}

	for _, ride := range completed {
		c.publish(ctx, events.RideStatusChangedEvent{
			RideRequestID: ride.RideRequestID,
			VehicleID:     ride.VehicleID,
			RiderID:       ride.RiderID,
			OwnerID:       ride.OwnerID,
			Status:        ride.Status,
			RequesterName: ride.RequesterName,
			UpdatedAt:     ride.UpdatedAt,
		})
		c.logger.Info("ride completed on drive-end",
			slog.String("ride_id", ride.RideRequestID),
			slog.String("vehicle_id", ride.VehicleID),
			slog.String("drive_id", driveID),
		)
	}
}

// publish emits the status-change event, swallowing (logging) errors — the ride
// row already committed to completed, so a dropped notification must not matter
// (clients reconcile via REST per FR-9.1/FR-9.2).
func (c *Completer) publish(ctx context.Context, payload events.EventPayload) {
	if c.publisher == nil {
		return
	}
	if err := c.publisher.Publish(ctx, events.NewEvent(payload)); err != nil {
		c.logger.Warn("ridecomplete: publish status change failed",
			slog.String("topic", string(payload.EventTopic())),
			slog.String("error", err.Error()),
		)
	}
}

// redactVIN shows only the last 4 chars of a VIN for logs (empty stays empty).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
