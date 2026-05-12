package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// TestControlFrames_UnsubscribeStopsBroadcast covers the MYR-46 happy
// path for unsubscribe: a client owning v-1 + v-2 unsubscribes from
// v-1, then a Broadcast for v-1 must NOT reach the client; a Broadcast
// for v-2 still does.
func TestControlFrames_UnsubscribeStopsBroadcast(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1", "v-2"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	// Send unsubscribe for v-1.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mustWriteFrame(ctx, t, conn, msgTypeUnsubscribe, unsubscribePayload{VehicleID: "v-1"})

	// Wait for the readPump to apply the unsubscribe before broadcasting,
	// otherwise the broadcast may race ahead of the state mutation.
	waitForCondition(t, func() bool {
		var got *Client
		hub.mu.RLock()
		for c := range hub.clients {
			got = c
		}
		hub.mu.RUnlock()
		if got == nil {
			return false
		}
		got.subMu.RLock()
		_, stillSubscribed := got.subscribed["v-1"]
		got.subMu.RUnlock()
		return !stillSubscribed
	})

	// Order matters: the coder/websocket library closes the read side
	// when a Read context cancels, so do the positive (must-deliver)
	// assertion FIRST, the negative (timeout) LAST.

	// Broadcast for v-2 — must still be delivered (still subscribed).
	v2Msg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeDriveStarted,
		Payload: mustMarshalRaw(t, map[string]any{"vehicleId": "v-2"}),
	})
	hub.Broadcast("v-2", v2Msg)

	deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer deliverCancel()
	if _, _, err := conn.Read(deliverCtx); err != nil {
		t.Fatalf("expected v-2 broadcast to still reach the client, got %v", err)
	}

	// Broadcast for v-1 — must NOT be delivered (post-unsubscribe).
	v1Msg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeDriveStarted,
		Payload: mustMarshalRaw(t, map[string]any{"vehicleId": "v-1"}),
	})
	hub.Broadcast("v-1", v1Msg)

	readCtx, readCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer readCancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("expected no v-1 broadcast after unsubscribe")
	}
}

// TestControlFrames_SubscribeIsIdempotent verifies that subscribing to
// an already-subscribed vehicle does not change broadcast behavior.
func TestControlFrames_SubscribeIsIdempotent(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mustWriteFrame(ctx, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "v-1"})

	// Broadcast still reaches the (still-subscribed) client.
	// No sync needed: v-1 was already in the seeded subscription set
	// at handshake, so subscribe is a no-op state-wise. The broadcast
	// path's only state read is hasVehicle, which has been true since
	// the handshake — there is no race window the dispatch could lose.
	msg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeDriveStarted,
		Payload: mustMarshalRaw(t, map[string]any{"vehicleId": "v-1"}),
	})
	hub.Broadcast("v-1", msg)

	deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer deliverCancel()
	if _, _, err := conn.Read(deliverCtx); err != nil {
		t.Fatalf("expected v-1 broadcast to reach the client after idempotent subscribe, got %v", err)
	}
}

// TestControlFrames_SubscribeNonOwnedEmitsTypedErrorAndCloses4002 is
// the MYR-46 enforcement scenario: subscribe to a vehicle outside the
// caller's ownership set must produce a typed `vehicle_not_owned`
// error frame followed by close code 4002, per websocket-protocol.md
// §6.1.1 / §6.2.
func TestControlFrames_SubscribeNonOwnedEmitsTypedErrorAndCloses4002(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mustWriteFrame(ctx, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "v-not-owned"})

	// Expect a typed error frame.
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("expected error frame before close, got read err %v", err)
	}
	var msg wsMessage
	if jerr := json.Unmarshal(data, &msg); jerr != nil {
		t.Fatalf("unmarshal: %v", jerr)
	}
	if msg.Type != msgTypeError {
		t.Fatalf("expected error frame, got %q", msg.Type)
	}
	var payload errorPayload
	if jerr := json.Unmarshal(msg.Payload, &payload); jerr != nil {
		t.Fatalf("unmarshal payload: %v", jerr)
	}
	if payload.Code != wserrors.ErrCodeVehicleNotOwned {
		t.Fatalf("code: got %q, want %q", payload.Code, wserrors.ErrCodeVehicleNotOwned)
	}

	// The next Read must return the close error with code 4002.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	_, _, err = conn.Read(closeCtx)
	if err == nil {
		t.Fatal("expected close after error frame")
	}
	if got := websocket.CloseStatus(err); got != closeCodePermissionRevoked {
		t.Fatalf("close code: got %d, want %d", got, closeCodePermissionRevoked)
	}
}

// TestControlFrames_PingEchoesPong verifies the ping->pong round-trip
// echoes the nonce.
func TestControlFrames_PingEchoesPong(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	const nonce = "abc-123"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mustWriteFrame(ctx, t, conn, msgTypePing, pingPayload{Nonce: nonce})

	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	var msg wsMessage
	if jerr := json.Unmarshal(data, &msg); jerr != nil {
		t.Fatalf("unmarshal: %v", jerr)
	}
	if msg.Type != msgTypePong {
		t.Fatalf("expected pong, got %q", msg.Type)
	}
	var p pongPayload
	if jerr := json.Unmarshal(msg.Payload, &p); jerr != nil {
		t.Fatalf("unmarshal payload: %v", jerr)
	}
	if p.Nonce != nonce {
		t.Fatalf("nonce: got %q, want %q", p.Nonce, nonce)
	}
}

// TestControlFrames_UnknownTypeIsIgnored asserts a future SDK can send
// a frame type the server does not yet recognize without poisoning the
// connection.
func TestControlFrames_UnknownTypeIsIgnored(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	frame := mustMarshalTest(t, wsMessage{Type: "future_frame_v2"})
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Connection must still accept a Broadcast.
	msg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeDriveStarted,
		Payload: mustMarshalRaw(t, map[string]any{"vehicleId": "v-1"}),
	})
	hub.Broadcast("v-1", msg)

	deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer deliverCancel()
	if _, _, err := conn.Read(deliverCtx); err != nil {
		t.Fatalf("connection should still deliver after unknown frame, got %v", err)
	}
}

func mustWriteFrame(ctx context.Context, t *testing.T, conn *websocket.Conn, msgType string, payload any) {
	t.Helper()
	frame := mustMarshalTest(t, wsMessage{
		Type:    msgType,
		Payload: mustMarshalRaw(t, payload),
	})
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}
