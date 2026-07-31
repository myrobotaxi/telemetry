package telemetry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles ---

// fakeShareInviteStore is a scriptable ShareInviteStore. Capture fields record
// what the handler passed so validation and scoping can be asserted.
type fakeShareInviteStore struct {
	created      ShareInviteRow
	createErr    error
	createdInput ShareInviteCreateInput
	createCalled bool

	listed   []ShareInviteRow
	listErr  error
	listedAs struct{ vehicleID, ownerID string }

	revokedViewer string
	revokeErr     error
	revokedAs     struct{ inviteID, ownerID string }

	resent    ShareInviteRow
	resendErr error

	// ownerName is what OwnerFirstName returns — the `from` half of the
	// signed share link. Empty by default so the common cases exercise the
	// omitted-name canonical form.
	ownerName    string
	ownerNameErr error
}

func (f *fakeShareInviteStore) CreateInvite(_ context.Context, in ShareInviteCreateInput) (ShareInviteRow, error) {
	f.createCalled = true
	f.createdInput = in
	if f.createErr != nil {
		return ShareInviteRow{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeShareInviteStore) ListInvitesForVehicle(_ context.Context, vehicleID, ownerID string) ([]ShareInviteRow, error) {
	f.listedAs.vehicleID, f.listedAs.ownerID = vehicleID, ownerID
	return f.listed, f.listErr
}

func (f *fakeShareInviteStore) RevokeInvite(_ context.Context, inviteID, ownerID string) (string, error) {
	f.revokedAs.inviteID, f.revokedAs.ownerID = inviteID, ownerID
	return f.revokedViewer, f.revokeErr
}

func (f *fakeShareInviteStore) ResendInvite(_ context.Context, _, _ string) (ShareInviteRow, error) {
	return f.resent, f.resendErr
}

func (f *fakeShareInviteStore) OwnerFirstName(_ context.Context, _ string) (string, error) {
	return f.ownerName, f.ownerNameErr
}

// shareLinkTestSeed is a FIXED Ed25519 seed. It is not a secret and never
// reaches a deploy: a constant seed makes every link in these tests
// reproducible, and the public half is derived from it at assert time rather
// than pasted in, so the pair can never drift.
var shareLinkTestSeed = bytes.Repeat([]byte{0x42}, ed25519.SeedSize)

// testShareLinkSigner is the signer every mounted test handler uses.
func testShareLinkSigner(t *testing.T) *InviteLinkSigner {
	t.Helper()
	signer, err := NewInviteLinkSigner(shareLinkTestSeed)
	if err != nil {
		t.Fatalf("NewInviteLinkSigner: %v", err)
	}
	return signer
}

// testShareLinkPublicKey is the verifying half — what the web join shell would
// hold.
func testShareLinkPublicKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, ok := ed25519.NewKeyFromSeed(shareLinkTestSeed).Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("unexpected public key type")
	}
	return pub
}

// fakeAccessInvalidator records which users had their access set busted.
type fakeAccessInvalidator struct {
	busted []string
}

func (f *fakeAccessInvalidator) InvalidateVehicles(userID string) {
	f.busted = append(f.busted, userID)
}

const (
	shareInviteID   = "csh0123456789abcdef0123456789abcd"
	shareFixtureVeh = fixtureSnapshotRowID
)

// pendingInviteRow is a canonical pending invite for the fixture vehicle.
func pendingInviteRow() ShareInviteRow {
	return ShareInviteRow{
		ID:         shareInviteID,
		VehicleID:  shareFixtureVeh,
		Label:      "Mira Chen",
		Permission: "live_history",
		Code:       "RBO246",
		Status:     shareStatusPending,
		CreatedAt:  time.Date(2026, 7, 29, 15, 4, 5, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 8, 5, 15, 4, 5, 0, time.UTC),
	}
}

// acceptedInviteRow is the same invite after somebody redeemed it. Note the
// empty Code — the store blanks it, and the handler must not resurrect it.
func acceptedInviteRow() ShareInviteRow {
	acceptedAt := time.Date(2026, 7, 30, 9, 12, 44, 0, time.UTC)
	row := pendingInviteRow()
	row.Code = ""
	row.Status = shareStatusAccepted
	row.AcceptedAt = &acceptedAt
	return row
}

// newShareInviteMux mounts the four owner routes against a handler.
func newShareInviteMux(t *testing.T, caller string, store ShareInviteStore, owner string, invalidator AccessCacheInvalidator) *http.ServeMux { //nolint:unparam // caller-vs-owner is the variable under test; collapsing owner to a constant would hide which actor each case exercises
	t.Helper()
	h := NewShareInviteHandler(
		&stubTokenValidator{userID: caller},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(owner)},
		store,
		invalidator,
		testShareLinkSigner(t),
		discardLogger(),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vehicles/{vehicleId}/invites", h.ServeCreate)
	mux.HandleFunc("GET /api/vehicles/{vehicleId}/invites", h.ServeList)
	mux.HandleFunc("DELETE /api/invites/{inviteId}", h.ServeRevoke)
	mux.HandleFunc("POST /api/invites/{inviteId}/resend", h.ServeResend)
	return mux
}

// doShareRequest issues an authenticated request and returns the recorder.
func doShareRequest(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodeShareBody decodes a JSON object response.
func decodeShareBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, rec.Body.String())
	}
	return out
}

func TestShareInviteHandler_Create(t *testing.T) {
	invitePath := "/api/vehicles/" + shareFixtureVeh + "/invites"

	t.Run("owner mints an invite and gets the code back", func(t *testing.T) {
		store := &fakeShareInviteStore{created: pendingInviteRow()}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live_history"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}

		body := decodeShareBody(t, rec)
		if body["code"] != "RBO246" {
			t.Errorf("code = %v, want the minted code (the owner must be able to share it)", body["code"])
		}
		if body["inviteId"] != shareInviteID {
			t.Errorf("inviteId = %v", body["inviteId"])
		}
		if body["status"] != shareStatusPending {
			t.Errorf("status = %v, want pending", body["status"])
		}
		if _, ok := body["expiresAt"]; !ok {
			t.Error("a pending invite must carry expiresAt")
		}
		if _, ok := body["acceptedAt"]; ok {
			t.Error("a pending invite must NOT carry acceptedAt")
		}

		// vehicleIds omitted == the path vehicle alone.
		if got := store.createdInput.VehicleIDs; len(got) != 1 || got[0] != shareFixtureVeh {
			t.Errorf("vehicle set = %v, want just the path vehicle", got)
		}
		if store.createdInput.OwnerUserID != shareOwnerUser {
			t.Errorf("owner = %q, want the JWT subject (never client-supplied)", store.createdInput.OwnerUserID)
		}
	})

	t.Run("multi-vehicle set is passed through de-duplicated", func(t *testing.T) {
		store := &fakeShareInviteStore{created: pendingInviteRow()}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			fmt.Sprintf(`{"label":"Mira","permission":"rides","vehicleIds":[%q,%q,"veh-2"]}`,
				shareFixtureVeh, shareFixtureVeh))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
		if got := len(store.createdInput.VehicleIDs); got != 2 {
			t.Errorf("vehicle set has %d entries (%v), want 2 after de-duplication",
				got, store.createdInput.VehicleIDs)
		}
	})

	t.Run("rejects bad bodies with 400", func(t *testing.T) {
		cases := map[string]string{
			"missing label":                        `{"permission":"live"}`,
			"blank label":                          `{"label":"   ","permission":"live"}`,
			"missing permission":                   `{"label":"Mira"}`,
			"unknown permission":                   `{"label":"Mira","permission":"admin"}`,
			"owner tier is not a share tier":       `{"label":"Mira","permission":"owner"}`,
			"empty vehicleIds":                     `{"label":"Mira","permission":"live","vehicleIds":[]}`,
			"vehicleIds omitting the path vehicle": `{"label":"Mira","permission":"live","vehicleIds":["other-vehicle"]}`,
			"malformed json":                       `{`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				store := &fakeShareInviteStore{created: pendingInviteRow()}
				mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

				rec := doShareRequest(t, mux, http.MethodPost, invitePath, body)
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status %d, want 400. Body: %s", rec.Code, rec.Body.String())
				}
				if store.createCalled {
					t.Error("a rejected body still reached the store")
				}
			})
		}
	})

	t.Run("a non-owner is refused and mints nothing", func(t *testing.T) {
		store := &fakeShareInviteStore{created: pendingInviteRow()}
		// The caller is a viewer of the vehicle; sharing is owner-only, so
		// even a `rides` grant must not let them re-share the car.
		mux := newShareInviteMux(t, shareViewerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira","permission":"live"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rec.Code)
		}
		if store.createCalled {
			t.Error("a non-owner's create reached the store")
		}
	})

	t.Run("an unknown vehicle is 404, not 403", func(t *testing.T) {
		store := &fakeShareInviteStore{created: pendingInviteRow()}
		h := NewShareInviteHandler(
			&stubTokenValidator{userID: shareOwnerUser},
			&stubVehicleSnapshotReader{err: fmt.Errorf("stub: %w", sdk.ErrNotFound)},
			store, nil, testShareLinkSigner(t), discardLogger(),
		)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/vehicles/{vehicleId}/invites", h.ServeCreate)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath, `{"label":"M","permission":"live"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})

	t.Run("an unowned vehicle in the set is 403", func(t *testing.T) {
		store := &fakeShareInviteStore{createErr: ErrShareVehicleNotOwned}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			fmt.Sprintf(`{"label":"M","permission":"live","vehicleIds":[%q,"not-mine"]}`, shareFixtureVeh))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403. Body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing bearer token is 401", func(t *testing.T) {
		mux := newShareInviteMux(t, shareOwnerUser, &fakeShareInviteStore{}, shareOwnerUser, nil)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, invitePath,
			bytes.NewReader([]byte(`{"label":"M","permission":"live"}`)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", rec.Code)
		}
	})
}

func TestShareInviteHandler_List(t *testing.T) {
	invitePath := "/api/vehicles/" + shareFixtureVeh + "/invites"

	t.Run("returns the invites envelope with per-status field omission", func(t *testing.T) {
		store := &fakeShareInviteStore{listed: []ShareInviteRow{pendingInviteRow(), acceptedInviteRow()}}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}

		var body struct {
			Invites []map[string]any `json:"invites"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Invites) != 2 {
			t.Fatalf("got %d invites, want 2", len(body.Invites))
		}

		pending, accepted := body.Invites[0], body.Invites[1]
		if pending["code"] != "RBO246" {
			t.Errorf("pending row is missing its code (%v)", pending["code"])
		}
		if _, ok := accepted["code"]; ok {
			t.Error("an ACCEPTED row carried a code — it is a live credential and is not re-readable")
		}
		if _, ok := accepted["expiresAt"]; ok {
			t.Error("an accepted grant carried expiresAt — an accepted grant does not expire")
		}
		if _, ok := accepted["acceptedAt"]; !ok {
			t.Error("an accepted row is missing acceptedAt")
		}
		if accepted["label"] != "Mira Chen" {
			t.Errorf("the owner-typed label was not preserved through redemption: %v", accepted["label"])
		}

		// The list is scoped to the caller in the STORE call, not filtered
		// afterwards — the scoping has to be in the query.
		if store.listedAs.ownerID != shareOwnerUser {
			t.Errorf("store was asked for owner %q, want %q", store.listedAs.ownerID, shareOwnerUser)
		}
	})

	t.Run("an owner with no invites gets an empty array, not null", func(t *testing.T) {
		store := &fakeShareInviteStore{listed: nil}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != "{\"invites\":[]}\n" {
			t.Errorf("body = %q, want an empty array under `invites`", got)
		}
	})

	t.Run("a non-owner is refused", func(t *testing.T) {
		store := &fakeShareInviteStore{listed: []ShareInviteRow{pendingInviteRow()}}
		mux := newShareInviteMux(t, shareViewerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403 — a viewer must not read the owner's labels for other people", rec.Code)
		}
		if store.listedAs.vehicleID != "" {
			t.Error("a non-owner's list reached the store")
		}
	})
}

func TestShareInviteHandler_RevokeAndResend(t *testing.T) {
	revokePath := "/api/invites/" + shareInviteID
	resendPath := revokePath + "/resend"

	t.Run("revoke is 204 and busts the revoked viewer's access cache", func(t *testing.T) {
		invalidator := &fakeAccessInvalidator{}
		store := &fakeShareInviteStore{revokedViewer: shareViewerUser}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, invalidator)

		rec := doShareRequest(t, mux, http.MethodDelete, revokePath, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status %d, want 204. Body: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("204 carried a body: %q", rec.Body.String())
		}
		if len(invalidator.busted) != 1 || invalidator.busted[0] != shareViewerUser {
			t.Errorf("busted = %v, want [%s] — a revoked viewer must lose access immediately, not at TTL",
				invalidator.busted, shareViewerUser)
		}
		// The store call must be owner-scoped.
		if store.revokedAs.ownerID != shareOwnerUser {
			t.Errorf("store was asked to revoke as %q", store.revokedAs.ownerID)
		}
	})

	t.Run("revoking a pending invite busts nobody", func(t *testing.T) {
		invalidator := &fakeAccessInvalidator{}
		store := &fakeShareInviteStore{revokedViewer: ""}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, invalidator)

		if rec := doShareRequest(t, mux, http.MethodDelete, revokePath, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d, want 204", rec.Code)
		}
		if len(invalidator.busted) != 0 {
			t.Errorf("busted %v for a pending invite; nobody held access", invalidator.busted)
		}
	})

	t.Run("revoking an unknown or foreign invite is 404", func(t *testing.T) {
		store := &fakeShareInviteStore{revokeErr: fmt.Errorf("stub: %w", sdk.ErrNotFound)}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodDelete, revokePath, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status %d, want 404", rec.Code)
		}
	})

	t.Run("resend returns the updated invite with a new code", func(t *testing.T) {
		resent := pendingInviteRow()
		resent.Code = "NEW123"
		resent.ExpiresAt = resent.ExpiresAt.Add(24 * time.Hour)
		store := &fakeShareInviteStore{resent: resent}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, resendPath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		body := decodeShareBody(t, rec)
		if body["code"] != "NEW123" {
			t.Errorf("code = %v, want the re-minted code", body["code"])
		}
		if body["inviteId"] != shareInviteID {
			t.Errorf("resend changed the invite id: %v", body["inviteId"])
		}
	})

	t.Run("resending an accepted grant is 409", func(t *testing.T) {
		store := &fakeShareInviteStore{resendErr: ErrShareNotPending}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, resendPath, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d, want 409. Body: %s", rec.Code, rec.Body.String())
		}
		body := decodeShareBody(t, rec)
		if errObj, ok := body["error"].(map[string]any); ok {
			if errObj["code"] != "conflict" {
				t.Errorf("error code = %v, want conflict", errObj["code"])
			}
		}
	})

	t.Run("a store failure is 500, not a leaked error", func(t *testing.T) {
		store := &fakeShareInviteStore{resendErr: errors.New("connection refused")}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, resendPath, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", rec.Code)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("connection refused")) {
			t.Error("the underlying error text leaked into the response envelope")
		}
	})
}

// --- Signed share links (MYR-368) ---

// verifyShareURL re-derives the canonical payload from a link exactly as the
// web join shell must, and verifies it against the PUBLIC key. It reads the
// query itself rather than trusting the signer's own helpers, so a change that
// broke the agreement between the URL and the payload fails here.
func verifyShareURL(t *testing.T, rawURL, wantCode, wantFrom, wantTo string, wantExpiry time.Time) {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse shareUrl %q: %v", rawURL, err)
	}
	if got, want := u.Path, "/join/"+wantCode; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	q := u.Query()
	if got := q.Get("from"); got != wantFrom {
		t.Errorf("from = %q, want %q", got, wantFrom)
	}
	if got := q.Get("to"); got != wantTo {
		t.Errorf("to = %q, want %q", got, wantTo)
	}

	parts := strings.Split(q.Get("k"), ".")
	if len(parts) != 3 {
		t.Fatalf("k = %q, want three parts", q.Get("k"))
	}
	if got, want := parts[1], strconv.FormatInt(wantExpiry.Unix(), 10); got != want {
		t.Errorf("signed expiry = %s, want %s (the row's expires_at)", got, want)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	payload := "join:" + wantCode + ":" + parts[1] + ":" + q.Get("from") + ":" + q.Get("to")
	if !ed25519.Verify(testShareLinkPublicKey(t), []byte(payload), sig) {
		t.Fatalf("signature does not verify over %q", payload)
	}
}

// shareURLOf pulls the shareUrl out of a response body, failing if absent.
func shareURLOf(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, ok := body["shareUrl"].(string)
	if !ok || raw == "" {
		t.Fatalf("no shareUrl in response: %v", body)
	}
	return raw
}

func TestShareInviteHandler_ShareURL(t *testing.T) {
	invitePath := "/api/vehicles/" + shareFixtureVeh + "/invites"
	row := pendingInviteRow()

	t.Run("create emits a verifiable link carrying both names", func(t *testing.T) {
		store := &fakeShareInviteStore{created: row, ownerName: "Alex Rivera"}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live_history"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201. Body: %s", rec.Code, rec.Body.String())
		}
		body := decodeShareBody(t, rec)
		// from is the OWNER's first name; to is the first token of the
		// owner-typed label on the row.
		verifyShareURL(t, shareURLOf(t, body), row.Code, "Alex", "Mira", row.ExpiresAt)
	})

	t.Run("the link's expiry is the row's expires_at, not a re-derived TTL", func(t *testing.T) {
		odd := row
		odd.ExpiresAt = time.Date(2027, 3, 14, 1, 59, 26, 0, time.UTC)
		store := &fakeShareInviteStore{created: odd, ownerName: "Alex"}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		body := decodeShareBody(t, doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live"}`))
		verifyShareURL(t, shareURLOf(t, body), odd.Code, "Alex", "Mira", odd.ExpiresAt)
	})

	t.Run("list signs every pending row and no accepted one", func(t *testing.T) {
		store := &fakeShareInviteStore{
			listed:    []ShareInviteRow{pendingInviteRow(), acceptedInviteRow()},
			ownerName: "Alex Rivera",
		}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodGet, invitePath, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", rec.Code)
		}
		var out struct {
			Invites []map[string]any `json:"invites"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Invites) != 2 {
			t.Fatalf("got %d rows, want 2", len(out.Invites))
		}
		verifyShareURL(t, shareURLOf(t, out.Invites[0]), row.Code, "Alex", "Mira", row.ExpiresAt)
		// The accepted row carries no code, so it must carry no link —
		// a URL there would resurrect the very credential the code
		// branch withheld.
		if _, present := out.Invites[1]["shareUrl"]; present {
			t.Errorf("accepted row carries a shareUrl: %v", out.Invites[1])
		}
	})

	t.Run("resend RE-SIGNS with the new code and the new expiry", func(t *testing.T) {
		resent := pendingInviteRow()
		resent.Code = "NEW123"
		resent.ExpiresAt = time.Date(2026, 8, 12, 15, 4, 5, 0, time.UTC)
		store := &fakeShareInviteStore{resent: resent, ownerName: "Alex Rivera"}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, "/api/invites/"+shareInviteID+"/resend", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		body := decodeShareBody(t, rec)
		newURL := shareURLOf(t, body)
		verifyShareURL(t, newURL, "NEW123", "Alex", "Mira", resent.ExpiresAt)

		// And it is genuinely a different link: the old code must not
		// survive anywhere in it, or the owner would hand out a URL
		// that redeems the credential they just invalidated.
		if strings.Contains(newURL, row.Code) {
			t.Errorf("resent link still carries the old code: %s", newURL)
		}
		createStore := &fakeShareInviteStore{created: row, ownerName: "Alex Rivera"}
		createMux := newShareInviteMux(t, shareOwnerUser, createStore, shareOwnerUser, nil)
		oldBody := decodeShareBody(t, doShareRequest(t, createMux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live"}`))
		if shareURLOf(t, oldBody) == newURL {
			t.Error("resend produced an identical link")
		}
	})

	t.Run("a label that sanitizes away omits `to` and still verifies", func(t *testing.T) {
		odd := row
		odd.Label = "🙂🙂"
		store := &fakeShareInviteStore{created: odd, ownerName: "Alex"}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		body := decodeShareBody(t, doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"x","permission":"live"}`))
		got := shareURLOf(t, body)
		if strings.Contains(got, "to=") {
			t.Errorf("expected no `to` parameter: %s", got)
		}
		verifyShareURL(t, got, odd.Code, "Alex", "", odd.ExpiresAt)
	})

	t.Run("an owner-name lookup failure degrades instead of failing the create", func(t *testing.T) {
		store := &fakeShareInviteStore{created: row, ownerNameErr: errors.New("db down")}
		mux := newShareInviteMux(t, shareOwnerUser, store, shareOwnerUser, nil)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201 — a display-name failure must not sink the invite", rec.Code)
		}
		body := decodeShareBody(t, rec)
		got := shareURLOf(t, body)
		if strings.Contains(got, "from=") {
			t.Errorf("expected no `from` parameter: %s", got)
		}
		verifyShareURL(t, got, row.Code, "", "Mira", row.ExpiresAt)
	})

	t.Run("no signing key means no shareUrl, and the code still ships", func(t *testing.T) {
		store := &fakeShareInviteStore{created: row, ownerName: "Alex"}
		h := NewShareInviteHandler(
			&stubTokenValidator{userID: shareOwnerUser},
			&stubVehicleSnapshotReader{row: fixtureSnapshotRow(shareOwnerUser)},
			store, nil, nil, discardLogger(),
		)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/vehicles/{vehicleId}/invites", h.ServeCreate)

		rec := doShareRequest(t, mux, http.MethodPost, invitePath,
			`{"label":"Mira Chen","permission":"live"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d, want 201", rec.Code)
		}
		body := decodeShareBody(t, rec)
		if _, present := body["shareUrl"]; present {
			t.Error("emitted a shareUrl with no signing key")
		}
		if body["code"] != row.Code {
			t.Errorf("code = %v, want the fallback the client shares instead", body["code"])
		}
	})
}
