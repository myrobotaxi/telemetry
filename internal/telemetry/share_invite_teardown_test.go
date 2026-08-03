package telemetry

import (
	"net/http"
	"testing"
)

// MYR-373 handler side: which owner actions announce a live-socket teardown,
// and in what order relative to the cache bust. The hub-side behavior is
// covered in internal/ws; what these pin is that the right call is made, for
// the right person, on the right vehicle, and only when access was actually
// LOST.

// recordingNotifier captures ShareAccessNotifier calls, and — because the
// ordering against the cache bust is a correctness property rather than a
// stylistic one — records whether the bust had already happened when each
// notification arrived.
type recordingNotifier struct {
	calls []notifierCall
	// inv is the invalidator whose state is sampled at notify time.
	inv *fakeAccessInvalidator
}

type notifierCall struct {
	granteeUserID  string
	vehicleID      string
	reason         string
	bustsSoFar     int
	bustedThisUser bool
}

func (n *recordingNotifier) ShareAccessRevoked(granteeUserID, vehicleID, reason string) {
	call := notifierCall{granteeUserID: granteeUserID, vehicleID: vehicleID, reason: reason}
	if n.inv != nil {
		call.bustsSoFar = len(n.inv.busted)
		for _, u := range n.inv.busted {
			if u == granteeUserID {
				call.bustedThisUser = true
			}
		}
	}
	n.calls = append(n.calls, call)
}

// newTeardownMux mounts the revoke and patch routes with both an invalidator
// and a notifier attached.
func newTeardownMux(t *testing.T, store ShareInviteStore, inv *fakeAccessInvalidator, notifier ShareAccessNotifier) *http.ServeMux {
	t.Helper()
	h := NewShareInviteHandler(
		&stubTokenValidator{userID: shareOwnerUser},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
		store,
		inv,
		testShareLinkSigner(t),
		discardLogger(),
		WithShareAccessNotifier(notifier),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/invites/{inviteId}", h.ServeRevoke)
	mux.HandleFunc("PATCH /api/invites/{inviteId}", h.ServePatch)
	return mux
}

func TestRevokeTearsDownTheViewersLiveSocket(t *testing.T) {
	store := &fakeShareInviteStore{revokedViewer: shareViewerUser, revokedVehicle: shareFixtureVeh}
	inv := &fakeAccessInvalidator{}
	notifier := &recordingNotifier{inv: inv}
	mux := newTeardownMux(t, store, inv, notifier)

	rec := doShareRequest(t, mux, http.MethodDelete, sharePatchPath, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204. Body: %s", rec.Code, rec.Body.String())
	}

	if len(notifier.calls) != 1 {
		t.Fatalf("notifier called %d times, want 1", len(notifier.calls))
	}
	got := notifier.calls[0]
	if got.granteeUserID != shareViewerUser {
		t.Errorf("torn down user = %q, want the REVOKED VIEWER %q", got.granteeUserID, shareViewerUser)
	}
	if got.vehicleID != shareFixtureVeh {
		t.Errorf("torn down vehicle = %q, want %q", got.vehicleID, shareFixtureVeh)
	}
	if got.reason != "revoked" {
		t.Errorf("reason = %q, want %q", got.reason, "revoked")
	}
	// ORDER: the cache must already be busted. Otherwise the client's
	// reconnect — triggered by the very close this notification causes — can
	// be served the pre-revoke access set and walk straight back in.
	if !got.bustedThisUser {
		t.Error("the socket teardown was announced BEFORE the grantee's access cache was busted; " +
			"the reconnect it provokes could then be served the stale set")
	}
}

// TestRevokeWithNoGranteeAnnouncesNothing: revoking a still-pending invite, or
// the idempotent second DELETE, removed nobody's access. Announcing an empty
// grantee would push a meaningless event at the hub.
func TestRevokeWithNoGranteeAnnouncesNothing(t *testing.T) {
	store := &fakeShareInviteStore{revokedViewer: "", revokedVehicle: shareFixtureVeh}
	inv := &fakeAccessInvalidator{}
	notifier := &recordingNotifier{inv: inv}
	mux := newTeardownMux(t, store, inv, notifier)

	if rec := doShareRequest(t, mux, http.MethodDelete, sharePatchPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", rec.Code)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notifier called %d times for a revocation that removed nobody's access, want 0: %+v",
			len(notifier.calls), notifier.calls)
	}
}

// TestPatchTearsDownOnlyWhenAccessIsLost is the asymmetry between this and the
// unconditional cache bust. Suspension removes the grant from the access set
// the WS handshake resolves through; allowRides governs the ride surface and
// has no WebSocket effect at all, so closing a socket over it would drop a
// viewer's live map for a change that does not touch the live map.
func TestPatchTearsDownOnlyWhenAccessIsLost(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		rowAfter     ShareInviteRow
		wantTeardown bool
	}{
		{
			name:         "suspending tears down",
			body:         `{"suspended":true}`,
			rowAfter:     patchedGrantRow(false, true),
			wantTeardown: true,
		},
		{
			name:         "suspending while keeping rides still tears down — no capability outvotes suspension",
			body:         `{"suspended":true,"allowRides":true}`,
			rowAfter:     patchedGrantRow(true, true),
			wantTeardown: true,
		},
		{
			name:         "granting rides does NOT tear down",
			body:         `{"allowRides":true}`,
			rowAfter:     patchedGrantRow(true, false),
			wantTeardown: false,
		},
		{
			name:         "withdrawing rides does NOT tear down — the live map is unaffected",
			body:         `{"allowRides":false}`,
			rowAfter:     patchedGrantRow(false, false),
			wantTeardown: false,
		},
		{
			name:         "LIFTING a suspension does not tear down — restoring access has no socket to end",
			body:         `{"suspended":false}`,
			rowAfter:     patchedGrantRow(false, false),
			wantTeardown: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeShareInviteStore{patched: tt.rowAfter, patchee: shareViewerUser}
			inv := &fakeAccessInvalidator{}
			notifier := &recordingNotifier{inv: inv}
			mux := newTeardownMux(t, store, inv, notifier)

			rec := doShareRequest(t, mux, http.MethodPatch, sharePatchPath, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
			}

			// The bust is unconditional either way — that part does not change.
			if len(inv.busted) != 1 || inv.busted[0] != shareViewerUser {
				t.Errorf("cache busts = %v, want exactly [%s] on every successful patch",
					inv.busted, shareViewerUser)
			}

			if tt.wantTeardown {
				if len(notifier.calls) != 1 {
					t.Fatalf("notifier called %d times, want 1", len(notifier.calls))
				}
				got := notifier.calls[0]
				if got.granteeUserID != shareViewerUser || got.vehicleID != shareFixtureVeh {
					t.Errorf("torn down (%q, %q), want (%q, %q)",
						got.granteeUserID, got.vehicleID, shareViewerUser, shareFixtureVeh)
				}
				if got.reason != "suspended" {
					t.Errorf("reason = %q, want %q", got.reason, "suspended")
				}
				if !got.bustedThisUser {
					t.Error("teardown announced before the cache bust")
				}
				return
			}
			if len(notifier.calls) != 0 {
				t.Errorf("notifier called %d times for a patch that took no WebSocket access away, want 0: %+v",
					len(notifier.calls), notifier.calls)
			}
		})
	}
}

// TestTeardownIsOptional: a handler built without the option — dev wiring, and
// every pre-existing test — must behave exactly as before rather than panic.
func TestTeardownIsOptional(t *testing.T) {
	store := &fakeShareInviteStore{revokedViewer: shareViewerUser, revokedVehicle: shareFixtureVeh}
	inv := &fakeAccessInvalidator{}
	mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, inv)

	if rec := doShareRequest(t, mux, http.MethodDelete, sharePatchPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 with no notifier configured", rec.Code)
	}
	if len(inv.busted) != 1 {
		t.Errorf("cache busts = %v, want the revoked viewer busted regardless", inv.busted)
	}
}

// TestTeardownToleratesANilNotifierValue guards the wiring path where
// deps.shareAccessNotifier is nil (dev mode) and the option is still applied.
func TestTeardownToleratesANilNotifierValue(t *testing.T) {
	store := &fakeShareInviteStore{revokedViewer: shareViewerUser, revokedVehicle: shareFixtureVeh}
	mux := newTeardownMux(t, store, &fakeAccessInvalidator{}, nil)

	if rec := doShareRequest(t, mux, http.MethodDelete, sharePatchPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204 with a nil notifier", rec.Code)
	}
}
