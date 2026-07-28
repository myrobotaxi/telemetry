package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockChecker is a ReadinessChecker for tests. When err is non-nil,
// Ping returns that error; otherwise it returns nil.
type mockChecker struct {
	err error
}

func (m *mockChecker) Ping(_ context.Context) error { return m.err }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestHandleHealthz(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "returns 200 ok",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantBody:   "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(),tt.method, "/healthz", nil)
			rec := httptest.NewRecorder()

			handleHealthz(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status code: got %d, want %d", rec.Code, tt.wantStatus)
			}

			var resp healthResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Status != tt.wantBody {
				t.Errorf("status: got %q, want %q", resp.Status, tt.wantBody)
			}

			ct := rec.Header().Get("Content-Type")
			if ct != "application/json; charset=utf-8" {
				t.Errorf("content-type: got %q, want %q", ct, "application/json; charset=utf-8")
			}
		})
	}
}

func TestHandleReadyz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
		wantError  bool
	}{
		{
			name:       "ready when db is healthy",
			pingErr:    nil,
			wantStatus: http.StatusOK,
			wantBody:   "ready",
			wantError:  false,
		},
		{
			name:       "not ready when db ping fails",
			pingErr:    errors.New("connection refused"),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "not ready",
			wantError:  true,
		},
		{
			name:       "not ready with wrapped error",
			pingErr:    errors.New("store.DB.Ping: dial tcp: connect: connection refused"),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "not ready",
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &mockChecker{err: tt.pingErr}
			handler := handleReadyz(checker, testLogger())

			req := httptest.NewRequestWithContext(context.Background(),http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status code: got %d, want %d", rec.Code, tt.wantStatus)
			}

			var resp healthResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Status != tt.wantBody {
				t.Errorf("status: got %q, want %q", resp.Status, tt.wantBody)
			}

			if tt.wantError && resp.Error == "" {
				t.Error("expected error field to be non-empty")
			}
			if !tt.wantError && resp.Error != "" {
				t.Errorf("unexpected error field: %q", resp.Error)
			}
		})
	}
}
