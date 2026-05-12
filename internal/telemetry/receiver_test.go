package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

const waitTimeout = 2 * time.Second

// receiverTestLogger returns a silent logger for tests.
func receiverTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// collectingBus captures published events for test assertions.
type collectingBus struct {
	mu     sync.Mutex
	events []events.Event
	closed bool
}

func (b *collectingBus) Publish(_ context.Context, event events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return events.ErrBusClosed
	}
	b.events = append(b.events, event)
	return nil
}

func (b *collectingBus) Subscribe(_ events.Topic, _ events.Handler) (events.Subscription, error) {
	return events.Subscription{}, nil
}

func (b *collectingBus) Unsubscribe(_ events.Subscription) error {
	return nil
}

func (b *collectingBus) Close(_ context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

func (b *collectingBus) eventsByTopic(topic events.Topic) []events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []events.Event
	for _, e := range b.events {
		if e.Topic == topic {
			result = append(result, e)
		}
	}
	return result
}

// testCA creates a self-signed CA and returns the CA cert and key.
func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	return caCert, caKey
}

// testClientCert creates a client certificate signed by the given CA with
// the VIN as the Common Name.
func testClientCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, vin string) tls.Certificate {
	t.Helper()

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: vin},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{clientDER},
		PrivateKey:  clientKey,
	}
}

// testServerCert creates a server certificate signed by the given CA.
func testServerCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{serverDER},
		PrivateKey:  serverKey,
	}
}

// testEnv holds the shared test infrastructure for receiver tests.
type testEnv struct {
	server   *httptest.Server
	receiver *Receiver
	bus      *collectingBus
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey
}

func newTestEnv(t *testing.T, cfg ReceiverConfig) *testEnv {
	t.Helper()

	bus := &collectingBus{}
	logger := receiverTestLogger()
	recv := NewReceiver(NewDecoder(), bus, logger, NoopReceiverMetrics{}, cfg)

	caCert, caKey := testCA(t)
	serverCert := testServerCert(t, caCert, caKey)

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	srv := httptest.NewUnstartedServer(recv.Handler())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAnyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return &testEnv{
		server:   srv,
		receiver: recv,
		bus:      bus,
		caCert:   caCert,
		caKey:    caKey,
	}
}

// dialWithVIN connects to the test server using a client certificate with
// the given VIN as the Common Name.
func (te *testEnv) dialWithVIN(ctx context.Context, t *testing.T, vin string) *websocket.Conn { //nolint:unparam // vin varies logically across tests even if values coincide
	t.Helper()

	clientCert := testClientCert(t, te.caCert, te.caKey, vin)

	caPool := x509.NewCertPool()
	caPool.AddCert(te.caCert)

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}

	wsURL := "wss" + strings.TrimPrefix(te.server.URL, "https")
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
		},
	})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial with VIN %s: %v", vin, err)
	}

	return conn
}

// makeTestPayload creates a FlatBuffers-wrapped protobuf payload for testing.
// The returned bytes match the wire format Tesla vehicles actually send.
func makeTestPayload(t *testing.T, vin string) []byte {
	t.Helper()

	payload := &tpb.Payload{
		Vin:       vin,
		CreatedAt: timestamppb.Now(),
		Data: []*tpb.Datum{
			makeDatum(tpb.Field_VehicleSpeed, stringVal("65.2")),
			makeDatum(tpb.Field_Location, locationVal(33.0903, -96.8237)),
		},
	}

	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	return BuildTestEnvelope(vin, raw)
}

// waitForEvents waits until the bus has at least n events on the given
// topic or the timeout elapses.
func waitForEvents(t *testing.T, bus *collectingBus, topic events.Topic, n int) []events.Event {
	t.Helper()

	deadline := time.After(waitTimeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for {
		evts := bus.eventsByTopic(topic)
		if len(evts) >= n {
			return evts
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %d events on topic %s, got %d", n, topic, len(evts))
			return nil
		case <-tick.C:
		}
	}
}

// pollConnectedVehicles polls until the receiver reports the expected count.
func pollConnectedVehicles(t *testing.T, recv *Receiver, want int) int {
	t.Helper()
	deadline := time.After(waitTimeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		got := recv.ConnectedVehicles()
		if got == want {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for connected vehicles = %d, got %d", want, got)
			return got
		case <-tick.C:
		}
	}
}

// closeConn closes a websocket connection, ignoring errors (for t.Cleanup).
func closeConn(conn *websocket.Conn) {
	_ = conn.CloseNow()
}

func TestReceiver_ConnectAndReceiveTelemetry(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn := te.dialWithVIN(ctx, t, testVIN)
	t.Cleanup(func() { closeConn(conn) })

	// Wait for connectivity event.
	connectEvts := waitForEvents(t, te.bus, events.TopicConnectivity, 1)
	ce := connectEvts[0].Payload.(events.ConnectivityEvent)
	if ce.Status != events.StatusConnected {
		t.Errorf("connectivity status = %d, want StatusConnected", ce.Status)
	}
	if ce.VIN != testVIN {
		t.Errorf("connectivity VIN = %q, want %q", ce.VIN, testVIN)
	}

	// Send a telemetry payload.
	raw := makeTestPayload(t, testVIN)
	if err := conn.Write(ctx, websocket.MessageBinary, raw); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for telemetry event.
	telEvts := waitForEvents(t, te.bus, events.TopicVehicleTelemetry, 1)
	tEvt := telEvts[0].Payload.(events.VehicleTelemetryEvent)
	if tEvt.VIN != testVIN {
		t.Errorf("telemetry VIN = %q, want %q", tEvt.VIN, testVIN)
	}
	if _, ok := tEvt.Fields["speed"]; !ok {
		t.Error("telemetry event missing speed field")
	}
	if _, ok := tEvt.Fields["location"]; !ok {
		t.Error("telemetry event missing location field")
	}

	if te.receiver.ConnectedVehicles() != 1 {
		t.Errorf("connected vehicles = %d, want 1", te.receiver.ConnectedVehicles())
	}
}

func TestReceiver_DisconnectPublishesEvent(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn := te.dialWithVIN(ctx, t, testVIN)

	// Wait for connect event.
	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Close the connection.
	if err := conn.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Wait for disconnect event (second connectivity event).
	disconnectEvts := waitForEvents(t, te.bus, events.TopicConnectivity, 2)
	de := disconnectEvts[1].Payload.(events.ConnectivityEvent)
	if de.Status != events.StatusDisconnected {
		t.Errorf("disconnect status = %d, want StatusDisconnected", de.Status)
	}

	// Wait briefly for cleanup to complete.
	deadline := time.After(waitTimeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for te.receiver.ConnectedVehicles() != 0 {
		select {
		case <-deadline:
			t.Fatalf("connected vehicles never reached 0, got %d", te.receiver.ConnectedVehicles())
		case <-tick.C:
		}
	}
}

func TestReceiver_RejectsNoCert(t *testing.T) {
	bus := &collectingBus{}
	logger := receiverTestLogger()
	recv := NewReceiver(NewDecoder(), bus, logger, NoopReceiverMetrics{}, ReceiverConfig{})

	// Use a plain HTTP server (no mTLS) to simulate missing cert.
	srv := httptest.NewServer(recv.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	// Attempt to dial — should fail because no TLS/cert.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error when connecting without cert, got nil")
	}
}

func TestReceiver_MaxVehiclesEnforced(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{MaxVehicles: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// First connection should succeed.
	conn1 := te.dialWithVIN(ctx, t, "5YJ3E7EB2NF000001")
	t.Cleanup(func() { closeConn(conn1) })

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Second connection with a different VIN should be rejected.
	clientCert := testClientCert(t, te.caCert, te.caKey, "5YJ3E7EB2NF000002")
	caPool := x509.NewCertPool()
	caPool.AddCert(te.caCert)

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}

	wsURL := "wss" + strings.TrimPrefix(te.server.URL, "https")
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
		},
	})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected error when max vehicles exceeded, got nil")
	}

	if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestReceiver_VINMismatchUsesCertVIN(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	certVIN := "5YJ3E7EB2NF000001"
	payloadVIN := "5YJ3E7EB2NF999999" // Different from cert VIN.

	conn := te.dialWithVIN(ctx, t, certVIN)
	t.Cleanup(func() { closeConn(conn) })

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Send payload with mismatched VIN.
	payload := &tpb.Payload{
		Vin:       payloadVIN,
		CreatedAt: timestamppb.Now(),
		Data: []*tpb.Datum{
			makeDatum(tpb.Field_VehicleSpeed, stringVal("55.0")),
		},
	}
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Wrap in a FlatBuffers envelope with the mismatched VIN as deviceId.
	envelope := BuildTestEnvelope(payloadVIN, raw)
	if err := conn.Write(ctx, websocket.MessageBinary, envelope); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The published event should use the cert VIN, not the payload VIN.
	telEvts := waitForEvents(t, te.bus, events.TopicVehicleTelemetry, 1)
	tEvt := telEvts[0].Payload.(events.VehicleTelemetryEvent)
	if tEvt.VIN != certVIN {
		t.Errorf("event VIN = %q, want cert VIN %q", tEvt.VIN, certVIN)
	}
}

func TestReceiver_RateLimiting(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{
		MaxMessagesPerSec: 2, // Very low limit for testing.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn := te.dialWithVIN(ctx, t, testVIN)
	t.Cleanup(func() { closeConn(conn) })

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Send a burst of messages — more than the rate limit allows.
	raw := makeTestPayload(t, testVIN)
	const burstSize = 10
	for i := range burstSize {
		if err := conn.Write(ctx, websocket.MessageBinary, raw); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Poll until at least one telemetry event is published (confirms processing started).
	waitForEvents(t, te.bus, events.TopicVehicleTelemetry, 1)

	// Some messages should have been published, but not all.
	telEvts := te.bus.eventsByTopic(events.TopicVehicleTelemetry)
	if len(telEvts) >= burstSize {
		t.Errorf("expected rate limiting to drop some messages, got %d/%d published", len(telEvts), burstSize)
	}
	if len(telEvts) == 0 {
		t.Error("expected at least some messages to get through")
	}
}

func TestReceiver_GracefulShutdown(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn := te.dialWithVIN(ctx, t, testVIN)
	t.Cleanup(func() { closeConn(conn) })

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	if te.receiver.ConnectedVehicles() != 1 {
		t.Fatalf("expected 1 connected vehicle, got %d", te.receiver.ConnectedVehicles())
	}

	// Initiate shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(shutdownCancel)
	te.receiver.Shutdown(shutdownCtx)

	if te.receiver.ConnectedVehicles() != 0 {
		t.Errorf("after shutdown: connected vehicles = %d, want 0", te.receiver.ConnectedVehicles())
	}
}

func TestReceiver_DecodeErrorDoesNotDisconnect(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn := te.dialWithVIN(ctx, t, testVIN)
	t.Cleanup(func() { closeConn(conn) })

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Send invalid protobuf.
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("not valid protobuf")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Send a valid message after the invalid one.
	raw := makeTestPayload(t, testVIN)
	if err := conn.Write(ctx, websocket.MessageBinary, raw); err != nil {
		t.Fatalf("write valid: %v", err)
	}

	// The valid message should still be published.
	telEvts := waitForEvents(t, te.bus, events.TopicVehicleTelemetry, 1)
	if len(telEvts) == 0 {
		t.Error("expected telemetry event after decode error")
	}

	// Vehicle should still be connected.
	if te.receiver.ConnectedVehicles() != 1 {
		t.Errorf("connected vehicles = %d, want 1 (decode error should not disconnect)", te.receiver.ConnectedVehicles())
	}
}

func TestReceiver_ReplacesExistingConnection(t *testing.T) {
	te := newTestEnv(t, ReceiverConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// First connection.
	conn1 := te.dialWithVIN(ctx, t, testVIN)

	waitForEvents(t, te.bus, events.TopicConnectivity, 1)

	// Second connection with the same VIN should replace the first.
	conn2 := te.dialWithVIN(ctx, t, testVIN)
	t.Cleanup(func() { closeConn(conn2) })

	// Wait for the second connectivity event (replacement connect).
	waitForEvents(t, te.bus, events.TopicConnectivity, 2)

	// The old connection should eventually fail on read.
	_ = conn1.CloseNow()

	// Poll until connected count settles to 1.
	count := pollConnectedVehicles(t, te.receiver, 1)
	if count != 1 {
		t.Errorf("connected vehicles = %d, want 1 after replacement", count)
	}
}
