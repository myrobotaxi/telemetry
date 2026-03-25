package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

const (
	// maxMessageSize is the maximum WebSocket message size the receiver
	// will accept (1 MiB). Tesla telemetry payloads are typically < 10 KB.
	maxMessageSize = 1 << 20

	// defaultMaxMessagesPerSec is the default per-vehicle rate limit.
	defaultMaxMessagesPerSec = 10.0
)

// ReceiverConfig holds tuning parameters for the telemetry receiver.
type ReceiverConfig struct {
	// MaxVehicles is the maximum number of simultaneous vehicle connections.
	// Zero means unlimited.
	MaxVehicles int

	// MaxMessagesPerSec is the per-vehicle rate limit. Zero or negative
	// means no rate limiting.
	MaxMessagesPerSec float64
}

// Receiver accepts mTLS WebSocket connections from Tesla vehicles, decodes
// their protobuf telemetry payloads, and publishes domain events to the
// event bus.
type Receiver struct {
	decoder     *Decoder
	bus         events.Bus
	logger      *slog.Logger
	metrics     ReceiverMetrics
	rateLimiter *rateLimiter
	maxVehicles int

	connections sync.Map // VIN -> *vehicleConn
	connCount   atomic.Int32
}

// NewReceiver creates a Receiver. The decoder converts raw protobuf into
// domain events; pass NewDecoder() for production use.
func NewReceiver(decoder *Decoder, bus events.Bus, logger *slog.Logger, metrics ReceiverMetrics, cfg ReceiverConfig) *Receiver {
	maxPerSec := cfg.MaxMessagesPerSec
	if maxPerSec == 0 {
		maxPerSec = defaultMaxMessagesPerSec
	}

	return &Receiver{
		decoder:     decoder,
		bus:         bus,
		logger:      logger,
		metrics:     metrics,
		rateLimiter: newRateLimiter(maxPerSec),
		maxVehicles: cfg.MaxVehicles,
	}
}

// Handler returns an http.Handler that accepts WebSocket connections from
// Tesla vehicles. It extracts the VIN from the mTLS client certificate,
// upgrades the connection, and starts the read loop.
func (r *Receiver) Handler() http.Handler {
	return http.HandlerFunc(r.handleUpgrade)
}

// handleUpgrade extracts the VIN from the client cert, enforces the
// max-vehicle limit, upgrades the HTTP connection to WebSocket, and
// hands off to the read loop.
func (r *Receiver) handleUpgrade(w http.ResponseWriter, req *http.Request) {
	vin, err := extractVIN(req)
	if err != nil {
		r.logger.Warn("rejected connection: no valid client certificate",
			slog.Any("error", err),
			slog.String("remote_addr", req.RemoteAddr),
		)
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return
	}

	redacted := redactVIN(vin)

	// Enforce max vehicle limit.
	if r.maxVehicles > 0 && int(r.connCount.Load()) >= r.maxVehicles {
		r.logger.Warn("rejected connection: max vehicles reached",
			slog.String("vin", redacted),
			slog.Int("max", r.maxVehicles),
		)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	// If this VIN already has a connection, close the old one.
	// Decrement count here since the old cleanupConnection won't
	// (CompareAndDelete will fail because we already removed the entry).
	if old, loaded := r.connections.LoadAndDelete(vin); loaded {
		oldVC := old.(*vehicleConn)
		oldVC.cancel()
		r.connCount.Add(-1)
		r.rateLimiter.remove(vin)
		r.logger.Info("replaced existing connection",
			slog.String("vin", redacted),
		)
	}

	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		// Tesla vehicles set their own origin. We rely on mTLS for auth.
		InsecureSkipVerify: true,
	})
	if err != nil {
		r.logger.Error("websocket accept failed",
			slog.String("vin", redacted),
			slog.Any("error", err),
		)
		return
	}

	conn.SetReadLimit(maxMessageSize)

	connCtx, connCancel := context.WithCancel(req.Context())
	defer connCancel() // also called via cleanupConnection; cancel is idempotent
	vc := &vehicleConn{
		vin:       vin,
		conn:      conn,
		connected: time.Now(),
		cancel:    connCancel,
	}

	r.connections.Store(vin, vc)
	r.connCount.Add(1)
	r.metrics.SetConnectedVehicles(int(r.connCount.Load()))

	r.logger.Info("vehicle connected",
		slog.String("vin", redacted),
		slog.String("remote_addr", req.RemoteAddr),
	)

	r.publishConnectivity(connCtx, vin, events.StatusConnected)
	r.handleConnection(connCtx, vc)
}

// handleConnection runs the read loop for a single vehicle connection.
// It blocks until the connection is closed or the context is cancelled.
func (r *Receiver) handleConnection(ctx context.Context, vc *vehicleConn) {
	defer r.cleanupConnection(vc)

	redacted := redactVIN(vc.vin)

	for {
		start := time.Now()

		_, data, err := vc.conn.Read(ctx)
		if err != nil {
			if !isNormalClose(err) {
				r.logger.Warn("read error",
					slog.String("vin", redacted),
					slog.Any("error", err),
				)
			}
			return
		}

		r.metrics.IncMessagesReceived(redacted)

		if !r.rateLimiter.allow(vc.vin) {
			r.metrics.IncRateLimited(redacted)
			r.logger.Debug("message rate limited",
				slog.String("vin", redacted),
			)
			continue
		}

		result, err := r.decoder.Decode(data)
		if err != nil {
			r.metrics.IncDecodeErrors(redacted)
			r.logger.Warn("decode failed",
				slog.String("vin", redacted),
				slog.Any("error", err),
			)
			continue
		}

		if len(result.FieldErrors) > 0 {
			r.logger.Debug("field decode warnings",
				slog.String("vin", redacted),
				slog.Int("count", len(result.FieldErrors)),
			)
		}

		evt := result.Event
		r.reconcileVIN(&evt, result.DeviceID, vc.vin, redacted)

		if err := r.bus.Publish(ctx, events.NewEvent(evt)); err != nil {
			r.logger.Error("publish telemetry event failed",
				slog.String("vin", redacted),
				slog.Any("error", err),
			)
			return
		}

		vc.lastMessage.Store(time.Now())
		vc.messageCount.Add(1)

		latency := time.Since(start)
		r.metrics.ObserveMessageLatency(latency.Seconds())
		r.logger.Debug("telemetry received",
			slog.String("vin", redacted),
			slog.String("topic", result.Topic),
			slog.Int("fields", len(evt.Fields)),
			slog.Duration("latency", latency),
		)
	}
}

// reconcileVIN cross-checks the envelope and payload VINs against the cert
// VIN and overwrites any mismatch. The cert VIN is authoritative because
// it was validated during the mTLS handshake.
func (r *Receiver) reconcileVIN(evt *events.VehicleTelemetryEvent, envelopeVIN, certVIN, redacted string) {
	if envelopeVIN != "" && envelopeVIN != certVIN {
		r.logger.Warn("envelope deviceId mismatch, using cert VIN",
			slog.String("cert_vin", redacted),
			slog.String("envelope_vin", redactVIN(envelopeVIN)),
		)
	}

	if evt.VIN != certVIN {
		r.logger.Warn("payload VIN mismatch, using cert VIN",
			slog.String("cert_vin", redacted),
			slog.String("payload_vin", redactVIN(evt.VIN)),
		)
		evt.VIN = certVIN
	}
}

