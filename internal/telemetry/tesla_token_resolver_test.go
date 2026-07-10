package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeTokenProvider struct {
	tok TeslaToken
	err error
}

func (f *fakeTokenProvider) GetTeslaToken(context.Context, string) (TeslaToken, error) {
	return f.tok, f.err
}

type fakeRefresher struct {
	out TeslaRefreshedToken
	err error
}

func (f *fakeRefresher) Refresh(context.Context, string) (TeslaRefreshedToken, error) {
	return f.out, f.err
}

type recordingUpdater struct {
	called bool
	err    error
}

func (u *recordingUpdater) UpdateTeslaToken(context.Context, string, string, string, int64) error {
	u.called = true
	return u.err
}

func TestTeslaTokenResolver_ValidToken(t *testing.T) {
	prov := &fakeTokenProvider{tok: TeslaToken{AccessToken: "live", ExpiresAt: time.Now().Add(time.Hour)}}
	r := NewTeslaTokenResolver(prov, nil)

	got, err := r.Resolve(context.Background(), "user1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.AccessToken != "live" {
		t.Errorf("token = %q, want live", got.AccessToken)
	}
}

func TestTeslaTokenResolver_NoTokenOnFile(t *testing.T) {
	prov := &fakeTokenProvider{err: errors.New("no account")}
	r := NewTeslaTokenResolver(prov, nil)

	_, err := r.Resolve(context.Background(), "user1")
	if !errors.Is(err, ErrTeslaTokenUnavailable) {
		t.Errorf("err = %v, want ErrTeslaTokenUnavailable", err)
	}
}

func TestTeslaTokenResolver_ExpiredNoRefresher(t *testing.T) {
	prov := &fakeTokenProvider{tok: TeslaToken{AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)}}
	r := NewTeslaTokenResolver(prov, nil)

	_, err := r.Resolve(context.Background(), "user1")
	if !errors.Is(err, ErrTeslaTokenExpired) {
		t.Errorf("err = %v, want ErrTeslaTokenExpired", err)
	}
}

func TestTeslaTokenResolver_ExpiredRefreshes(t *testing.T) {
	prov := &fakeTokenProvider{tok: TeslaToken{AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)}}
	ref := &fakeRefresher{out: TeslaRefreshedToken{AccessToken: "fresh", RefreshToken: "r2", ExpiresIn: 3600}}
	upd := &recordingUpdater{}
	r := NewTeslaTokenResolver(prov, nil, WithResolverRefresher(ref, upd))

	got, err := r.Resolve(context.Background(), "user1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.AccessToken != "fresh" {
		t.Errorf("token = %q, want fresh", got.AccessToken)
	}
	if !upd.called {
		t.Error("expected refreshed token to be persisted")
	}
}

func TestTeslaTokenResolver_RefreshFails(t *testing.T) {
	prov := &fakeTokenProvider{tok: TeslaToken{AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)}}
	ref := &fakeRefresher{err: errors.New("tesla down")}
	r := NewTeslaTokenResolver(prov, nil, WithResolverRefresher(ref, nil))

	_, err := r.Resolve(context.Background(), "user1")
	if !errors.Is(err, ErrTeslaTokenExpired) {
		t.Errorf("err = %v, want ErrTeslaTokenExpired", err)
	}
}
