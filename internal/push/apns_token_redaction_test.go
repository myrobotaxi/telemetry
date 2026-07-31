package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P1 token redaction on the APNs transport (MYR-172 review).
//
// data-classification.md §1.18 and §3.2 say the device token and the ActivityKit
// update token are "never logged in full". That was a policy sentence, not a
// property: the token is interpolated into the request URL, and BOTH failure
// paths in this file returned a *url.Error, whose Error() prints the whole URL.
// The retry path logs that error on every attempt, so an ordinary dropped
// connection put a P1 capability into the logs.
//
// These tests assert the property rather than the fix, so a future refactor that
// reintroduces a `%w` of a *url.Error fails here rather than in an incident.

// mustNotContainToken fails when a rendered error carries the token.
func mustNotContainToken(t *testing.T, err error, token string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error text carries the P1 token in full:\n  %s", err.Error())
	}
	if prefix := tokenPrefix(token); !strings.Contains(err.Error(), prefix) {
		t.Errorf("error text = %q, want it to identify the token by its %q prefix", err.Error(), prefix)
	}
}

// TestTransportErrorNeverCarriesTheToken covers the ROUTINE failure — a
// connection that goes away — which is the one that actually reaches the logs,
// on every retry, whenever the network is bad.
func TestTransportErrorNeverCarriesTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening: Do() fails with a *url.Error

	client := newTestClient(t, url)

	t.Run("activity token", func(t *testing.T) {
		err := client.SendActivity(context.Background(), testActivityNotification())
		mustNotContainToken(t, err, testActivityValue)
	})

	t.Run("device token", func(t *testing.T) {
		n := testNotification()
		err := client.Send(context.Background(), n)
		mustNotContainToken(t, err, n.DeviceToken)
	})
}

// TestRequestBuildEscapesTheToken covers the other half of the fix: the token
// is percent-escaped into the path, so there is no value for which url.Parse
// fails and hands back an error containing the whole address.
//
// The handler is only reachable if the request was built and sent at all, which
// is the assertion — a token full of URL metacharacters must not produce a
// malformed request, and must not be silently mangled into a different token
// either.
func TestRequestBuildEscapesTheToken(t *testing.T) {
	// Not a real token — registration rejects non-hex at the door (§7.21.1) —
	// but the transport must be safe for anything that reaches it, including a
	// row written before that validation existed.
	const hostile = "aa bb/../%zz?x=1#frag"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testActivityNotification()
	n.ActivityToken = hostile

	if err := newTestClient(t, srv.URL).SendActivity(context.Background(), n); err != nil {
		t.Fatalf("SendActivity() with a hostile token error = %v", err)
	}
	if want := "/3/device/" + hostile; gotPath != want {
		t.Errorf("decoded path = %q, want %q — the token must survive escaping unchanged", gotPath, want)
	}
}
