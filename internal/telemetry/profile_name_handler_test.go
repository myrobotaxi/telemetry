package telemetry

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProfileNameStore struct {
	updated  bool
	err      error
	gotUser  string
	gotName  string
	callable bool
}

func (f *fakeProfileNameStore) UpdateUserName(_ context.Context, userID, name string) (bool, error) {
	f.callable = true
	f.gotUser = userID
	f.gotName = name
	return f.updated, f.err
}

func patchName(t *testing.T, store *fakeProfileNameStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewProfileNameHandler(&stubTokenValidator{userID: "usr_1"}, store, nil)
	req := httptest.NewRequestWithContext(t.Context(), "PATCH", "/api/users/me", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	h.ServePatch(w, req)
	return w
}

func TestProfileName_HappyPathEchoesTheCommittedName(t *testing.T) {
	store := &fakeProfileNameStore{updated: true}
	w := patchName(t, store, `{"name":"  James Guan  "}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if store.gotUser != "usr_1" || store.gotName != "James Guan" {
		t.Fatalf("stored (%q, %q), want trimmed name for the caller", store.gotUser, store.gotName)
	}
	var resp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Name != "James Guan" {
		t.Fatalf("echo = %s, want the trimmed committed name", w.Body.String())
	}
}

// The world's names pass — no ASCII filter on a person's own name.
func TestProfileName_NonASCIINamesAreNames(t *testing.T) {
	for _, name := range []string{"José", "Ольга", "李小龙", "Mary-Jane O'Neil"} {
		store := &fakeProfileNameStore{updated: true}
		w := patchName(t, store, `{"name":"`+name+`"}`)
		if w.Code != 200 {
			t.Fatalf("%q refused (%d) — a person's own name is not a URL parameter", name, w.Code)
		}
	}
}

func TestProfileName_Refusals(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty after trimming", `{"name":"   "}`},
		{"absent", `{}`},
		{"over the cap", `{"name":"` + strings.Repeat("a", 61) + `"}`},
		{"control characters", `{"name":"a\u0007b"}`},
		{"invalid json", `not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeProfileNameStore{updated: true}
			w := patchName(t, store, tt.body)
			if w.Code != 400 {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if store.callable {
				t.Fatal("a refused name must never reach the store")
			}
		})
	}
}

func TestProfileName_NoMatchedRowIs404(t *testing.T) {
	store := &fakeProfileNameStore{updated: false}
	w := patchName(t, store, `{"name":"James"}`)
	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
