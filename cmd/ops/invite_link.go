package main

import (
	"fmt"
	"os"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// `ops invite-link public-key` — the MYR-368 public-key retrieval mechanism.
//
// The web join shell verifies every `?k=` signature against a public key
// COMPILED INTO the shell, so somebody has to move that key from the server's
// secret into the web repo exactly once per rotation. This is that step.
//
// It is deliberately the boring option. The alternatives were an authenticated
// admin endpoint (a new route, a new auth surface, and a running server, all to
// print a constant) or nothing at all (leaving the operator to write a
// throwaway Go program to turn a seed into a public key). This subcommand needs
// no database, no network, and no deployed process: it is pure arithmetic on
// the seed the operator already has in their hands, which also means it can be
// run BEFORE the secret is ever set — generate, derive, paste, deploy.
//
// The other half of the story is the server's own startup log line
// (`invite-link signing key loaded`, cmd/telemetry-server/invite_link_signer.go).
// That answers the question this command cannot: which key the RUNNING process
// actually loaded. Use this to obtain the value, that to confirm the deploy
// took it.

func runInviteLink(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("invite-link requires a subcommand (public-key)")
	}
	switch args[0] {
	case "public-key":
		return runInviteLinkPublicKey()
	default:
		return fmt.Errorf("unknown invite-link subcommand %q", args[0])
	}
}

// runInviteLinkPublicKey derives the Ed25519 public key from the seed in
// INVITE_LINK_SIGNING_KEY and prints it, base64, on one line.
//
// One bare line on stdout, no JSON envelope: the output is meant to be pasted
// into a source constant or piped somewhere, and quoting it would only give the
// operator something to strip. The PRIVATE seed is never printed, never
// echoed into an error, and never leaves this process.
func runInviteLinkPublicKey() error {
	seed := os.Getenv("INVITE_LINK_SIGNING_KEY")
	if seed == "" {
		return fmt.Errorf("INVITE_LINK_SIGNING_KEY is required (base64 32-byte Ed25519 seed; generate with `openssl rand -base64 32`)")
	}
	signer, err := telemetry.NewInviteLinkSignerFromSeedBase64(seed)
	if err != nil {
		return fmt.Errorf("read INVITE_LINK_SIGNING_KEY: %w", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, signer.PublicKeyBase64()); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}
