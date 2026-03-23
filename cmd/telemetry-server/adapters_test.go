package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestProxyHTTPClient(t *testing.T) {
	tests := []struct {
		name       string
		proxyURL   string
		wantClient bool
	}{
		{name: "IPv4 loopback", proxyURL: "https://127.0.0.1:4443", wantClient: true},
		{name: "localhost", proxyURL: "https://localhost:4443", wantClient: true},
		{name: "IPv6 loopback", proxyURL: "https://[::1]:4443", wantClient: true},
		{name: "non-loopback returns nil", proxyURL: "https://proxy.example.com:4443", wantClient: false},
		{name: "invalid URL returns nil", proxyURL: "://invalid", wantClient: false},
		{name: "empty string returns nil", proxyURL: "", wantClient: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := proxyHTTPClient(tt.proxyURL, testLogger())

			if tt.wantClient && client == nil {
				t.Fatal("expected non-nil client, got nil")
			}
			if !tt.wantClient && client != nil {
				t.Fatal("expected nil client, got non-nil")
			}
			if !tt.wantClient {
				return
			}

			// Verify timeout is set.
			if client.Timeout != proxyTimeout {
				t.Errorf("timeout: got %v, want %v", client.Timeout, proxyTimeout)
			}

			// Verify InsecureSkipVerify is set on the transport.
			tr, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatal("transport is not *http.Transport")
			}
			if !tr.TLSClientConfig.InsecureSkipVerify {
				t.Error("expected InsecureSkipVerify to be true")
			}
		})
	}
}
