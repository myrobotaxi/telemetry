package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// stubAuthorizer is a test double for VehicleAuthorizer.
type stubAuthorizer struct {
	allowed map[string]bool
	err     error
}

func (s *stubAuthorizer) IsAuthorized(_ context.Context, vin string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.allowed[vin], nil
}

// countingMetrics adds a counter for IncRejectedVINNotAuthorized.
type countingMetrics struct {
	NoopReceiverMetrics
	rejected int
}

func (m *countingMetrics) IncRejectedVINNotAuthorized() { m.rejected++ }

// stubBus implements events.Bus with no-op publish.
type stubBus struct{ published int }

func (b *stubBus) Publish(_ context.Context, _ events.Event) error { b.published++; return nil }
func (b *stubBus) Subscribe(_ events.Topic, _ events.Handler) (events.Subscription, error) {
	return events.Subscription{}, nil
}
func (b *stubBus) Unsubscribe(_ events.Subscription) error { return nil }
func (b *stubBus) Close(_ context.Context) error          { return nil }

func newTestReceiver(t *testing.T, authorized map[string]bool) (*Receiver, *stubBus, *countingMetrics) {
	t.Helper()
	bus := &stubBus{}
	metrics := &countingMetrics{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recv := NewReceiver(NewDecoder(), bus, logger, metrics, ReceiverConfig{})
	recv.SetAuthorizer(&stubAuthorizer{allowed: authorized})
	return recv, bus, metrics
}

func TestReceiver_RejectsUnauthorizedVIN(t *testing.T) {
	recv, bus, metrics := newTestReceiver(t, map[string]bool{"AUTH0001": true})

	// Build a request with a synthetic mTLS chain bearing CN=UNAUTH0001.
	req := buildVINRequest(t, "UNAUTH0001")
	rec := httptest.NewRecorder()

	recv.handleUpgrade(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if metrics.rejected != 1 {
		t.Errorf("rejected count = %d, want 1", metrics.rejected)
	}
	if bus.published != 0 {
		t.Errorf("bus.published = %d, want 0 (unauthorized VIN must not publish)", bus.published)
	}
}

func TestReceiver_AuthorizerErrorFailsOpen(t *testing.T) {
	bus := &stubBus{}
	metrics := &countingMetrics{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	recv := NewReceiver(NewDecoder(), bus, logger, metrics, ReceiverConfig{})
	recv.SetAuthorizer(&stubAuthorizer{err: errors.New("db unreachable")})

	req := buildVINRequest(t, "5YJ3E1EA1NF000002")
	rec := httptest.NewRecorder()
	recv.handleUpgrade(rec, req)

	// Fail-open: the upgrade attempt should NOT be rejected with 403.
	// The actual upgrade will fail (httptest Recorder doesn't support
	// hijack), but we just need to confirm the rejection counter
	// stayed at zero and we didn't emit a 403.
	if rec.Code == http.StatusForbidden {
		t.Errorf("status 403 emitted on authorizer error — should fail-open")
	}
	if metrics.rejected != 0 {
		t.Errorf("rejected count = %d, want 0 on authorizer error (fail-open)", metrics.rejected)
	}
}

func TestReceiver_RemoveStream_ClosesActiveConn(t *testing.T) {
	recv, _, _ := newTestReceiver(t, map[string]bool{"5YJ3VIN0001": true})

	// Insert a synthetic vehicleConn directly.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	recv.connections.Store("5YJ3VIN0001", &vehicleConn{
		vin:    "5YJ3VIN0001",
		conn:   nil, // tolerated by RemoveStream's nil guard
		cancel: cancel,
	})
	recv.connCount.Add(1)

	recv.RemoveStream("5YJ3VIN0001")

	if _, ok := recv.connections.Load("5YJ3VIN0001"); ok {
		t.Error("connection entry not removed from map")
	}
	if recv.connCount.Load() != 0 {
		t.Errorf("connCount = %d, want 0", recv.connCount.Load())
	}
}

func TestReceiver_RemoveStream_EmptyVINNoop(t *testing.T) {
	recv, _, _ := newTestReceiver(t, nil)
	recv.RemoveStream("")
	if recv.connCount.Load() != 0 {
		t.Errorf("connCount = %d, want 0 for empty VIN", recv.connCount.Load())
	}
}

// buildVINRequest constructs an *http.Request with a synthetic mTLS
// chain whose leaf CN is the provided VIN. extractVIN reads
// req.TLS.PeerCertificates[0].Subject.CommonName.
func buildVINRequest(t *testing.T, vin string) *http.Request {
	t.Helper()
	cert := &x509.Certificate{
		Subject: pkix.Name{CommonName: vin},
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	return req
}
