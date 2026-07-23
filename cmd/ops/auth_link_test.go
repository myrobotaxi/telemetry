package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Tesla OAuth PKCE / authorize-URL / token-exchange primitives moved to
// internal/teslaauth (MYR-246) and are tested there. The callback-server
// handler remains CLI-local (the localhost one-shot flow) and is tested here.

func TestCallbackHandler_SuccessPath(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?state=expected-state&code=code-xyz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	select {
	case r := <-result:
		if r.err != nil {
			t.Errorf("unexpected err: %v", r.err)
		}
		if r.code != "code-xyz" {
			t.Errorf("code: got %q, want code-xyz", r.code)
		}
	default:
		t.Fatal("handler did not write to result channel")
	}
}

func TestCallbackHandler_StateMismatchRejected(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?state=wrong&code=code-xyz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	r := <-result
	if r.err == nil || !strings.Contains(r.err.Error(), "state mismatch") {
		t.Errorf("expected state mismatch error, got %v", r.err)
	}
}

func TestCallbackHandler_TeslaErrorSurfaced(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?error=access_denied&error_description=user+declined", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	r := <-result
	if r.err == nil || !strings.Contains(r.err.Error(), "access_denied") {
		t.Errorf("expected access_denied error, got %v", r.err)
	}
}

func TestCallbackHandler_DuplicateSendDoesNotBlock(t *testing.T) {
	// Buffered capacity 1 — if the handler blocked on a second send, this
	// test would deadlock.
	result := make(chan callbackResult, 1)
	handler := callbackHandler("s", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/callback?state=s&code=c1", nil)
	handler(httptest.NewRecorder(), req)

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/callback?state=s&code=c2", nil)
	// Must not block even though the channel is full. Use a timeout guard
	// so a regression surfaces as a test failure instead of a hang.
	done := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), req2)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("duplicate callback blocked on full channel")
	}
}
