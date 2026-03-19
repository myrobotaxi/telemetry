package simulator

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"
)

// Vehicle simulates a single Tesla vehicle sending protobuf telemetry
// over a WebSocket connection.
type Vehicle struct {
	vin      string
	scenario Scenario
	logger   *slog.Logger
	interval time.Duration
}

// NewVehicle creates a simulated vehicle that will run the given scenario.
func NewVehicle(vin string, scenario Scenario, logger *slog.Logger, interval time.Duration) *Vehicle {
	return &Vehicle{
		vin:      vin,
		scenario: scenario,
		logger:   logger.With(slog.String("vin", vin), slog.String("scenario", scenario.Name())),
		interval: interval,
	}
}

// Run connects to the telemetry server and sends protobuf payloads at
// the configured interval until the scenario completes or the context
// is cancelled.
func (v *Vehicle) Run(ctx context.Context, serverURL string, tlsConfig *tls.Config) error {
	v.logger.Info("connecting to server", slog.String("url", serverURL))

	conn, err := v.dial(ctx, serverURL, tlsConfig)
	if err != nil {
		return fmt.Errorf("vehicle.Run(vin=%s): %w", v.vin, err)
	}

	v.logger.Info("connected, starting scenario")

	if err := v.sendLoop(ctx, conn); err != nil {
		_ = conn.CloseNow()
		return fmt.Errorf("vehicle.Run(vin=%s): %w", v.vin, err)
	}

	if err := conn.Close(websocket.StatusNormalClosure, "scenario complete"); err != nil {
		v.logger.Warn("close error", slog.Any("error", err))
	}
	v.logger.Info("scenario complete")
	return nil
}

// dial establishes a WebSocket connection to the server.
func (v *Vehicle) dial(ctx context.Context, serverURL string, tlsConfig *tls.Config) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{}
	if tlsConfig != nil {
		opts.HTTPClient = tlsHTTPClient(tlsConfig)
	}

	conn, resp, err := websocket.Dial(ctx, serverURL, opts)
	if resp != nil {
		v.logger.Debug("dial response", slog.Int("status", resp.StatusCode))
		if resp.Body != nil {
			_ = resp.Body.Close() // #nosec G104 -- best-effort cleanup of HTTP response body
		}
	}
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	return conn, nil
}

// sendLoop runs the scenario tick loop, building and sending protobuf
// payloads until the scenario completes or the context is done. On
// context cancellation it sends a final gear=P tick so the drive
// detector receives a clean end signal.
func (v *Vehicle) sendLoop(ctx context.Context, conn *websocket.Conn) error {
	ticker := time.NewTicker(v.interval)
	defer ticker.Stop()

	var sent int
	for !v.scenario.Done() {
		select {
		case <-ctx.Done():
			v.logger.Info("context cancelled, sending final park tick",
				slog.Int("messages_sent", sent),
			)
			v.sendParkTick(conn)
			return nil
		case <-ticker.C:
			if err := v.sendTick(ctx, conn); err != nil {
				return err
			}
			sent++
			if sent%10 == 0 {
				v.logger.Info("progress",
					slog.Int("messages_sent", sent),
					slog.Bool("done", v.scenario.Done()),
				)
			}
		}
	}

	v.logger.Info("scenario finished", slog.Int("messages_sent", sent))
	return nil
}

// sendParkTick sends a final telemetry tick with gear=P and speed=0.
// This gives the drive detector a clean end signal on graceful shutdown.
// Uses a short-lived background context because the parent ctx is already
// cancelled.
func (v *Vehicle) sendParkTick(conn *websocket.Conn) {
	state := v.scenario.Next()
	state.GearPosition = "P"
	state.Speed = 0
	state.ETA = 0

	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, err := MarshalPayload(v.vin, state)
	if err != nil {
		v.logger.Error("failed to marshal park tick", slog.Any("error", err))
		return
	}

	if err := conn.Write(writeCtx, websocket.MessageBinary, data); err != nil {
		v.logger.Warn("failed to send park tick", slog.Any("error", err))
		return
	}
	v.logger.Info("sent final park tick")
}

// sendTick advances the scenario, builds the payload, and sends it.
func (v *Vehicle) sendTick(ctx context.Context, conn *websocket.Conn) error {
	state := v.scenario.Next()

	data, err := MarshalPayload(v.vin, state)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	return nil
}
