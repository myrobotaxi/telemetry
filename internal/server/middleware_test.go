package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLogger(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		skipPaths   map[string]struct{}
		wantLogged  bool
		handlerCode int
	}{
		{
			name:        "logs normal request",
			path:        "/readyz",
			skipPaths:   map[string]struct{}{"/healthz": {}},
			wantLogged:  true,
			handlerCode: http.StatusOK,
		},
		{
			name:        "skips healthz",
			path:        "/healthz",
			skipPaths:   map[string]struct{}{"/healthz": {}},
			wantLogged:  false,
			handlerCode: http.StatusOK,
		},
		{
			name:        "logs when skip set is empty",
			path:        "/healthz",
			skipPaths:   map[string]struct{}{},
			wantLogged:  true,
			handlerCode: http.StatusOK,
		},
		{
			name:        "captures non-200 status",
			path:        "/fail",
			skipPaths:   map[string]struct{}{},
			wantLogged:  true,
			handlerCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.handlerCode)
			})

			middleware := requestLogger(logger, tt.skipPaths)
			handler := middleware(inner)

			req := httptest.NewRequestWithContext(context.Background(),http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			logged := buf.String()
			hasLogLine := strings.Contains(logged, "http request")

			if tt.wantLogged && !hasLogLine {
				t.Errorf("expected log output, got: %q", logged)
			}
			if !tt.wantLogged && hasLogLine {
				t.Errorf("expected no log output, got: %q", logged)
			}

			if tt.wantLogged && hasLogLine {
				if !strings.Contains(logged, tt.path) {
					t.Errorf("log missing path %q: %s", tt.path, logged)
				}
			}
		})
	}
}

func TestResponseRecorder_DefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: rec, statusCode: http.StatusOK}

	// Write body without calling WriteHeader — status should remain 200.
	if _, err := rr.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if rr.statusCode != http.StatusOK {
		t.Errorf("status code: got %d, want %d", rr.statusCode, http.StatusOK)
	}
}

func TestResponseRecorder_CapturesWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: rec, statusCode: http.StatusOK}

	rr.WriteHeader(http.StatusNotFound)

	if rr.statusCode != http.StatusNotFound {
		t.Errorf("status code: got %d, want %d", rr.statusCode, http.StatusNotFound)
	}
}

func TestRequestLogger_IncludesMethodAndDuration(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := requestLogger(logger, map[string]struct{}{})
	handler := middleware(inner)

	req := httptest.NewRequestWithContext(context.Background(),http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	logged := buf.String()
	if !strings.Contains(logged, "POST") {
		t.Errorf("log missing method: %s", logged)
	}
	if !strings.Contains(logged, "duration") {
		t.Errorf("log missing duration: %s", logged)
	}
}

// Ensure responseRecorder satisfies the http.ResponseWriter interface
// by using it as a plain io.Writer as well.
func TestResponseRecorder_ImplementsWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: rec, statusCode: http.StatusOK}

	var w io.Writer = rr
	n, err := w.Write([]byte("test"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 4 {
		t.Errorf("bytes written: got %d, want 4", n)
	}
}
