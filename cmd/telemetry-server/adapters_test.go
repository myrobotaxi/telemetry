package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

func TestResolveWSOriginPatterns(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		devMode    bool
		want       []string
	}{
		{
			name:       "configured wins in production",
			configured: []string{"https://myrobotaxi.app", "https://www.myrobotaxi.app"},
			devMode:    false,
			want:       []string{"https://myrobotaxi.app", "https://www.myrobotaxi.app"},
		},
		{
			name:       "configured wins under --dev",
			configured: []string{"https://staging.myrobotaxi.app"},
			devMode:    true,
			want:       []string{"https://staging.myrobotaxi.app"},
		},
		{
			name:       "empty config + production fails closed",
			configured: nil,
			devMode:    false,
			want:       nil,
		},
		{
			name:       "empty config + --dev admits localhost",
			configured: []string{},
			devMode:    true,
			want:       devLocalhostOriginPatterns,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWSOriginPatterns(tt.configured, tt.devMode, testLogger())
			if len(got) != len(tt.want) {
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}

	// The dev-mode fallback must not alias the package-level slice — a
	// caller mutating it should not poison subsequent calls.
	t.Run("dev fallback returns a defensive copy", func(t *testing.T) {
		got := resolveWSOriginPatterns(nil, true, testLogger())
		if len(got) == 0 {
			t.Fatal("expected non-empty dev fallback")
		}
		got[0] = "evil.example.com"
		if devLocalhostOriginPatterns[0] == "evil.example.com" {
			t.Error("resolveWSOriginPatterns leaked the package-level slice; caller mutation poisoned the constant")
		}
	})
}

func TestResolveDebugFieldsGate(t *testing.T) {
	longToken := strings.Repeat("a", debugFieldsMinTokenLen)

	tests := []struct {
		name        string
		devMode     bool
		token       string
		wantEnabled bool
		wantToken   string
		wantErr     error
	}{
		{name: "off by default", devMode: false, token: "", wantEnabled: false},
		{name: "dev, no token — open for dev", devMode: true, token: "", wantEnabled: true, wantToken: ""},
		{name: "dev + token — enforced in dev too", devMode: true, token: "short", wantEnabled: true, wantToken: "short"},
		{name: "prod + valid token — enabled", devMode: false, token: longToken, wantEnabled: true, wantToken: longToken},
		{name: "prod + short token — rejected", devMode: false, token: "short", wantErr: errDebugFieldsTokenTooShort},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDebugFieldsGate(tt.devMode, tt.token)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err: got %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.Enabled != tt.wantEnabled {
				t.Errorf("enabled: got %v, want %v", got.Enabled, tt.wantEnabled)
			}
			if got.Token != tt.wantToken {
				t.Errorf("token: got %q, want %q", got.Token, tt.wantToken)
			}
			if tt.wantEnabled && got.Reason == "" {
				t.Error("expected Reason to be set when Enabled")
			}
		})
	}
}
