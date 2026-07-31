package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Startup resolution of the MYR-368 invite-link signing key.
//
// The FAIL-FAST is the point of this file. It follows the kill-switch
// precedent already in this package (DISPATCH_ENABLED, PUSH_ENABLED,
// SERVICE_REPOLL_ENABLED): a security control an operator cannot trust is
// worse than no control at all. Booting production without the key would mint
// invites whose `shareUrl` is silently absent — every owner's share sheet
// quietly degrades to dictating six characters, and nothing in the running
// system says why.
//
// Dev is the exception, not the rule: a contributor running `go run ./cmd/...
// --dev` has no Fly secret and must not need one.

func testSeedB64(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(priv.Seed())
}

func TestResolveInviteLinkSigner(t *testing.T) {
	seed := testSeedB64(t)

	t.Run("prod without the env fails fast", func(t *testing.T) {
		signer, err := resolveInviteLinkSigner(false, "")
		if !errors.Is(err, errInviteLinkKeyRequired) {
			t.Fatalf("err = %v, want errInviteLinkKeyRequired", err)
		}
		if signer != nil {
			t.Error("expected no signer on failure")
		}
	})

	t.Run("prod with the env signs", func(t *testing.T) {
		signer, err := resolveInviteLinkSigner(false, seed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if signer.PublicKeyBase64() == "" {
			t.Error("expected a public key")
		}
	})

	t.Run("prod with a malformed env fails fast", func(t *testing.T) {
		if _, err := resolveInviteLinkSigner(false, "!!!"); err == nil {
			t.Fatal("expected an error for a malformed seed")
		}
	})

	t.Run("dev without the env generates an ephemeral key", func(t *testing.T) {
		signer, err := resolveInviteLinkSigner(true, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if signer.PublicKeyBase64() == "" {
			t.Fatal("expected an ephemeral key in dev mode")
		}
		// Ephemeral means EPHEMERAL: two dev boots must not share a key,
		// so nobody can mistake a dev link for one production would honor.
		other, err := resolveInviteLinkSigner(true, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if signer.PublicKeyBase64() == other.PublicKeyBase64() {
			t.Error("two dev boots produced the same key")
		}
	})

	t.Run("dev with the env uses it", func(t *testing.T) {
		signer, err := resolveInviteLinkSigner(true, seed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		fixed, err := resolveInviteLinkSigner(false, seed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if signer.PublicKeyBase64() != fixed.PublicKeyBase64() {
			t.Error("dev mode ignored a configured seed")
		}
	})

	t.Run("dev with a malformed env still fails fast", func(t *testing.T) {
		// A typo'd seed in dev must not fall through to an ephemeral key:
		// the operator asked for a specific key and would be debugging
		// signatures that verify against something else.
		if _, err := resolveInviteLinkSigner(true, "!!!"); err == nil {
			t.Fatal("expected an error for a malformed seed in dev mode")
		}
	})

	t.Run("surrounding whitespace is tolerated", func(t *testing.T) {
		signer, err := resolveInviteLinkSigner(false, "  "+seed+"\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		fixed, _ := resolveInviteLinkSigner(false, seed)
		if signer.PublicKeyBase64() != fixed.PublicKeyBase64() {
			t.Error("whitespace changed the resolved key")
		}
	})

	t.Run("no error ever echoes the seed", func(t *testing.T) {
		_, err := resolveInviteLinkSigner(false, "definitely-not-base64-!!!")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "definitely-not-base64") {
			t.Fatalf("error echoes the secret: %v", err)
		}
	})
}
