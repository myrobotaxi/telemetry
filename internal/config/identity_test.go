package config

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestLoad_IdentityDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	id := cfg.Identity()
	if id.AccessTokenTTL != time.Hour {
		t.Errorf("AccessTokenTTL = %v, want 1h", id.AccessTokenTTL)
	}
	if id.RefreshTokenTTL != 90*24*time.Hour {
		t.Errorf("RefreshTokenTTL = %v, want 90d", id.RefreshTokenTTL)
	}
	if id.AuthRateLimitPerMinute != 30 || id.AuthRateLimitBurst != 10 {
		t.Errorf("rate limit defaults = %d/%d, want 30/10", id.AuthRateLimitPerMinute, id.AuthRateLimitBurst)
	}
	// Absent secrets => module disabled.
	if id.ES256PrivateKeyPEM != "" || id.AppleClientID != "" || id.BootstrapEmailToCUID != nil {
		t.Errorf("expected empty identity secrets by default: %+v", id)
	}
}

func TestLoad_IdentityEnv(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)
	t.Setenv("AUTH_ES256_PRIVATE_KEY", "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----")
	t.Setenv("APPLE_NATIVE_CLIENT_ID", "app.myrobotaxi.ios")
	t.Setenv("AUTH_APPLE_BOOTSTRAP", "Owner@Example.com=cmmgr4b1p0005l104ifpctlg8, junk , =bad")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	id := cfg.Identity()
	if id.AppleClientID != "app.myrobotaxi.ios" {
		t.Errorf("AppleClientID = %q", id.AppleClientID)
	}
	if id.ES256PrivateKeyPEM == "" {
		t.Error("ES256PrivateKeyPEM not read from env")
	}
	if got := id.BootstrapEmailToCUID["owner@example.com"]; got != "cmmgr4b1p0005l104ifpctlg8" {
		t.Errorf("bootstrap map = %v, want lower-cased key -> cuid", id.BootstrapEmailToCUID)
	}
	if len(id.BootstrapEmailToCUID) != 1 {
		t.Errorf("malformed bootstrap entries not dropped: %v", id.BootstrapEmailToCUID)
	}
}

func TestLoad_IdentityKeyB64(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)
	pem := "-----BEGIN PRIVATE KEY-----\nxyz\n-----END PRIVATE KEY-----"
	t.Setenv("AUTH_ES256_PRIVATE_KEY_B64", base64.StdEncoding.EncodeToString([]byte(pem)))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Identity().ES256PrivateKeyPEM != pem {
		t.Errorf("B64 key not decoded: %q", cfg.Identity().ES256PrivateKeyPEM)
	}
}

func TestLoad_IdentityBadB64(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)
	t.Setenv("AUTH_ES256_PRIVATE_KEY_B64", "!!!not base64!!!")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid base64 key")
	}
}

func TestValidateIdentity_RejectsBadTTL(t *testing.T) {
	errs := validateIdentity(IdentityConfig{
		AccessTokenTTL:         2 * time.Hour,
		RefreshTokenTTL:        time.Hour, // refresh < access
		AuthRateLimitPerMinute: 0,
		AuthRateLimitBurst:     0,
	})
	if len(errs) == 0 {
		t.Fatal("expected validation errors for bad identity config")
	}
}
