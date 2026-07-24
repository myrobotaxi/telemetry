package teslaauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFetchUserInfo(t *testing.T) {
	t.Run("returns sub and email on 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
				t.Errorf("Authorization = %q, want Bearer tok-123", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sub":"tesla-sub-42","email":"owner@example.com"}`)
		}))
		defer srv.Close()
		old := UserInfoEndpoint
		UserInfoEndpoint = srv.URL
		defer func() { UserInfoEndpoint = old }()

		info, err := FetchUserInfo(context.Background(), discardLogger(), "tok-123")
		if err != nil {
			t.Fatalf("FetchUserInfo: %v", err)
		}
		if info.Sub != "tesla-sub-42" || info.Email != "owner@example.com" {
			t.Errorf("info = %+v", info)
		}
	})

	t.Run("empty token is rejected before any request", func(t *testing.T) {
		if _, err := FetchUserInfo(context.Background(), discardLogger(), ""); err == nil {
			t.Fatal("expected error for empty token")
		}
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		old := UserInfoEndpoint
		UserInfoEndpoint = srv.URL
		defer func() { UserInfoEndpoint = old }()

		if _, err := FetchUserInfo(context.Background(), discardLogger(), "tok"); err == nil {
			t.Fatal("expected error on 401")
		}
	})

	t.Run("missing sub is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"email":"x@example.com"}`)
		}))
		defer srv.Close()
		old := UserInfoEndpoint
		UserInfoEndpoint = srv.URL
		defer func() { UserInfoEndpoint = old }()

		if _, err := FetchUserInfo(context.Background(), discardLogger(), "tok"); err == nil {
			t.Fatal("expected error when sub is missing")
		}
	})
}
