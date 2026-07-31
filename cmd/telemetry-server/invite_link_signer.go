// Startup resolution of the two env-driven gates that can REFUSE TO BOOT: the
// debug-fields token and the MYR-368 invite-link signing key. Kept out of
// main.go so each rule and its dev-mode carve-out reads on its own, rather than
// as two more branches in an already-long composition root.

package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// inviteLinkSeedEnv is the Fly secret holding the base64 Ed25519 SEED (32
// bytes) that signs join links.
//
// Generate and set with:
//
//	openssl rand -base64 32
//	fly secrets set INVITE_LINK_SIGNING_KEY='<that value>'
//
// The PUBLIC half, which the web join shell needs, comes back out with:
//
//	INVITE_LINK_SIGNING_KEY='<seed>' go run ./cmd/ops invite-link public-key
const inviteLinkSeedEnv = "INVITE_LINK_SIGNING_KEY"

// errInviteLinkKeyRequired is returned when a non-dev boot has no signing key.
var errInviteLinkKeyRequired = errors.New(inviteLinkSeedEnv + " is required outside --dev")

// resolveInviteLinkSigner folds the --dev flag and the INVITE_LINK_SIGNING_KEY
// value into the signer the sharing handlers use. Rules:
//
//   - seed set:           use it, dev or not
//   - seed malformed:     FAIL, dev or not
//   - no seed, not --dev: FAIL
//   - no seed, --dev:     generate an ephemeral key for this process
//
// The non-dev failure is the kill-switch precedent applied to a security
// control (DISPATCH_ENABLED / PUSH_ENABLED / SERVICE_REPOLL_ENABLED all refuse
// to boot on a value they cannot trust). Without the key the server runs
// perfectly well and simply stops emitting `shareUrl` — which is precisely the
// failure worth refusing, because it is INVISIBLE: every owner's share sheet
// silently degrades to dictating six characters and nothing in the running
// system explains why. A missing signing key is a deploy mistake, and a deploy
// mistake should stop the deploy.
//
// A MALFORMED seed fails in dev too. The operator named a specific key; falling
// back to an ephemeral one would leave them debugging signatures that verify
// against a key they never chose.
//
// The dev key is EPHEMERAL — a fresh key per process, never persisted. Links
// minted locally verify only against that process's public key, so a dev link
// can never be mistaken for one production would honour.
func resolveInviteLinkSigner(devMode bool, seedB64 string) (*telemetry.InviteLinkSigner, error) {
	if seedB64 != "" {
		signer, err := telemetry.NewInviteLinkSignerFromSeedBase64(seedB64)
		if err != nil {
			// The wrapped error never carries the seed value.
			return nil, fmt.Errorf("invalid %s: %w", inviteLinkSeedEnv, err)
		}
		return signer, nil
	}
	if !devMode {
		return nil, errInviteLinkKeyRequired
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral invite-link key: %w", err)
	}
	return telemetry.NewInviteLinkSigner(priv.Seed())
}

// startupGates bundles the boot-time security resolutions main() needs to hold
// onto. They travel together because they answer the same question in the same
// place — "is this deploy configured well enough to run?" — and because folding
// them into one resolution keeps the composition root's branch count flat as
// gates are added.
type startupGates struct {
	// debugFields decides whether /api/debug/fields is mounted and how it
	// authenticates.
	debugFields debugFieldsGate
	// inviteLinks signs ShareInvite.shareUrl. Never nil on a successful
	// resolution.
	inviteLinks *telemetry.InviteLinkSigner
}

// resolveStartupGates reads the environment, applies both fail-fast rules, and
// logs what it resolved. Called early in boot, before any listener or database
// connection, so a misconfigured deploy costs nothing to refuse.
func resolveStartupGates(devMode bool, logger *slog.Logger) (startupGates, error) {
	debugFields, err := resolveDebugFieldsGate(devMode, os.Getenv("DEBUG_FIELDS_TOKEN"))
	if err != nil {
		return startupGates{}, fmt.Errorf("invalid debug-fields configuration: %w", err)
	}

	seed := os.Getenv(inviteLinkSeedEnv)
	inviteLinks, err := resolveInviteLinkSigner(devMode, seed)
	if err != nil {
		return startupGates{}, fmt.Errorf("invalid invite-link configuration: %w", err)
	}
	logInviteLinkPublicKey(logger, inviteLinks, seed == "")

	return startupGates{debugFields: debugFields, inviteLinks: inviteLinks}, nil
}

// logInviteLinkPublicKey emits the PUBLIC key at startup.
//
// Public by definition and therefore safe to log — this is the half of the pair
// that is meant to be copied around. It is logged so an operator can confirm
// which key the RUNNING process actually loaded, which is the one question
// `ops invite-link public-key` cannot answer: that subcommand derives the
// public key from whatever seed the shell hands it, not from what the deployed
// process is holding. The two together are the whole retrieval story — ops for
// obtaining the value, this line for confirming the deploy took it.
func logInviteLinkPublicKey(logger *slog.Logger, signer *telemetry.InviteLinkSigner, ephemeral bool) {
	if signer == nil {
		return
	}
	logger.Info("invite-link signing key loaded",
		slog.String("public_key_b64", signer.PublicKeyBase64()),
		slog.Bool("ephemeral", ephemeral),
	)
}
