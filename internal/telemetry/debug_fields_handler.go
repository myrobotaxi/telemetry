package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// debugFramesBuffer is the per-client channel buffer for raw field frames.
// Sized for ~1.2s of buffering at 10 msg/sec × 5 dev vehicles; slow clients
// have frames dropped rather than back-pressuring the event bus.
const debugFramesBuffer = 64

// DebugFieldsConfig configures the dev-only /api/debug/fields WebSocket
// endpoint. API keys are optional (dev only); when empty, any connection is
// accepted which matches the existing dev-mode WS policy.
//
// Security note: when APIKey is set, clients may supply it via either the
// X-Debug-Token header or the ?token= query parameter. The query param
// form appears in most HTTP access logs — prefer the header whenever
// possible. The CLI (cmd/ops fields watch) always uses the header.
type DebugFieldsConfig struct {
	// APIKey is a shared secret the client must supply via the
	// X-Debug-Token header or ?token= query parameter. Empty disables
	// auth (must only be used on loopback / trusted dev networks).
	APIKey string

	// WriteTimeout bounds per-frame WS writes. Default 5s.
	WriteTimeout time.Duration

	// OriginPatterns restricts which origins may connect (coder/websocket
	// glob syntax). Default: no origin check (dev only).
	OriginPatterns []string
}

// DebugFieldsHandler serves the /api/debug/fields WebSocket endpoint.
// Every connected client receives a JSON frame per decoded Tesla payload
// containing every proto field and its raw value. An optional ?vin=...
// query parameter filters to a single vehicle.
//
// This handler is only mounted when the server runs in dev mode. It is
// not part of the SDK contract surface — the shape is developer-focused
// and may change without notice.
type DebugFieldsHandler struct {
	bus    events.Bus
	logger *slog.Logger
	cfg    DebugFieldsConfig
}

// NewDebugFieldsHandler constructs a DebugFieldsHandler.
func NewDebugFieldsHandler(bus events.Bus, logger *slog.Logger, cfg DebugFieldsConfig) *DebugFieldsHandler {
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	return &DebugFieldsHandler{bus: bus, logger: logger, cfg: cfg}
}

// ServeHTTP upgrades to WebSocket and streams raw field events.
func (h *DebugFieldsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	filterVIN := strings.TrimSpace(r.URL.Query().Get("vin"))

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:     h.cfg.OriginPatterns,
		InsecureSkipVerify: len(h.cfg.OriginPatterns) == 0,
	})
	if err != nil {
		h.logger.Warn("debug_fields: websocket accept failed",
			slog.Any("error", err),
			slog.String("remote_addr", r.RemoteAddr),
		)
		return
	}
	defer conn.CloseNow() //nolint:errcheck // nothing useful to do on close failure

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	frames := make(chan []byte, debugFramesBuffer)
	handler := h.makeHandler(filterVIN, frames)

	sub, err := h.bus.Subscribe(events.TopicVehicleTelemetryRaw, handler)
	if err != nil {
		h.logger.Error("debug_fields: subscribe failed", slog.Any("error", err))
		_ = conn.Close(websocket.StatusInternalError, "subscribe failed")
		return
	}
	defer func() {
		if err := h.bus.Unsubscribe(sub); err != nil && !errors.Is(err, events.ErrSubscriptionNotFound) {
			h.logger.Warn("debug_fields: unsubscribe failed", slog.Any("error", err))
		}
	}()

	go h.readLoop(ctx, conn, cancel)
	h.writeLoop(ctx, conn, frames)
}

// authorize enforces the optional APIKey. Empty APIKey means no auth (dev).
func (h *DebugFieldsHandler) authorize(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	if v := r.Header.Get("X-Debug-Token"); v == h.cfg.APIKey {
		return true
	}
	if v := r.URL.Query().Get("token"); v == h.cfg.APIKey {
		return true
	}
	return false
}

// makeHandler returns an events.Handler that converts raw events to JSON
// frames, filtering by VIN when requested and dropping the event if the
// per-client buffer is full (the client is too slow).
func (h *DebugFieldsHandler) makeHandler(filterVIN string, frames chan<- []byte) events.Handler {
	return func(evt events.Event) {
		payload, ok := evt.Payload.(events.RawVehicleTelemetryEvent)
		if !ok {
			return
		}
		if filterVIN != "" && payload.VIN != filterVIN {
			return
		}
		frame, err := marshalDebugFrame(payload)
		if err != nil {
			h.logger.Warn("debug_fields: marshal failed", slog.Any("error", err))
			return
		}
		select {
		case frames <- frame:
		default:
			// Client is too slow — drop the frame rather than blocking
			// the bus. The developer will see gaps in the stream.
		}
	}
}

// readLoop discards incoming frames; the endpoint is server→client only.
// When the client closes or errors, it cancels the parent context to stop
// the write loop. Unexpected inbound frames are logged at debug level so
// an operator can tell a misbehaving client from a silent disconnect.
func (h *DebugFieldsHandler) readLoop(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		h.logger.Debug("debug_fields: discarded unexpected client frame",
			slog.Int("message_type", int(typ)),
			slog.Int("bytes", len(data)),
		)
	}
}

// writeLoop pulls frames from the channel and writes them to the WebSocket
// with a per-frame deadline.
func (h *DebugFieldsHandler) writeLoop(ctx context.Context, conn *websocket.Conn, frames <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "server shutting down")
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				h.logger.Warn("debug_fields: write failed", slog.Any("error", err))
				return
			}
		}
	}
}

// debugFrame is the JSON shape streamed to clients. One frame per decoded
// Tesla payload.
type debugFrame struct {
	VIN       string            `json:"vin"`
	Timestamp string            `json:"timestamp"`
	Fields    map[string]rawVal `json:"fields"`
}

// rawVal mirrors the issue's example: {value, protoField, type}. Invalid
// datums have value null and type "invalid".
type rawVal struct {
	Value      any    `json:"value"`
	ProtoField int32  `json:"protoField"`
	Type       string `json:"type"`
	Invalid    bool   `json:"invalid,omitempty"`
}

// marshalDebugFrame serializes a RawVehicleTelemetryEvent into the wire
// shape documented in the MYR-36 issue body.
func marshalDebugFrame(evt events.RawVehicleTelemetryEvent) ([]byte, error) {
	fields := make(map[string]rawVal, len(evt.Fields))
	for _, f := range evt.Fields {
		fields[f.ProtoName] = rawVal{
			Value:      f.Value,
			ProtoField: f.ProtoField,
			Type:       f.Type,
			Invalid:    f.Invalid,
		}
	}
	frame := debugFrame{
		VIN:       evt.VIN,
		Timestamp: evt.CreatedAt.UTC().Format(time.RFC3339Nano),
		Fields:    fields,
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("marshal debug frame: %w", err)
	}
	return b, nil
}
