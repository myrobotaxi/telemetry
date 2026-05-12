package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// TestVehicleDeletedDispatcher_ClosesSubscribedClient is a small
// end-to-end: connect a real WS client, publish VehicleDeletedEvent
// for that client's vehicle, assert the close arrives.
func TestVehicleDeletedDispatcher_ClosesSubscribedClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, logger)
	defer bus.Close(context.Background())

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()

	dispatcher := newVehicleDeletedDispatcher(hub, nil, nil, nil, logger)
	if _, err := dispatcher.Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	authn := &fakeAuth{userID: "u", vehicleIDs: []string{"v-deleted"}}
	srv := newWSTestServer(t, hub, authn)
	defer srv.Close()
	conn := dialWSAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	waitClients(t, hub, 1)

	if err := bus.Publish(context.Background(), events.NewEvent(events.VehicleDeletedEvent{
		VehicleID: "v-deleted",
		UserID:    "u",
	})); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(readCtx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected close error, got: %v", err)
	}
	if closeErr.Code != 4002 {
		t.Errorf("close code = %d, want 4002", closeErr.Code)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

type countingHubMetrics struct {
	ws.NoopHubMetrics
	closes atomic.Int32
}

func (m *countingHubMetrics) IncCloseUserDeletion() { m.closes.Add(1) }
