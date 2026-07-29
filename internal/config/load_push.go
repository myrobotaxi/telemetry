// Push-notification environment overrides (MYR-186). Extracted from load.go so
// the APNs credential handling lives with its kill-switch and load.go stays
// under the file-size cap.

package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Apple account constants. They identify the MyRoboTaxi developer team and the
// iOS app's bundle id, which are fixed properties of the app rather than
// per-deployment settings — but each has an env override so a fork, a staging
// bundle, or an account migration needs no code change.
const (
	defaultAPNSTeamID = "NFKX777598"
	defaultAPNSTopic  = "app.myrobotaxi.ios"
)

// applyPushEnvOverrides reads the push-notification settings.
//
// PUSH_ENABLED FAILS FAST on a non-boolean value rather than silently falling
// back: a typo like `no` or `off` must not read as "push enabled" and leave an
// operator believing notifications are stopped when they are not (the same
// lesson DISPATCH_ENABLED encodes — a kill-switch you cannot trust is worse
// than none).
//
// The APNs credentials are OPTIONAL by design. With no signing key the service
// runs keyless: the notifier still subscribes and logs each would-be
// notification as skipped, so the pipeline is observable before the secrets
// are set on the deploy. Absent credentials must never block startup.
func applyPushEnvOverrides(fc *fileConfig) error {
	enabled, err := parseBoolEnv("PUSH_ENABLED", true)
	if err != nil {
		return err
	}
	fc.pushEnabled = enabled

	keyPEM, err := readAPNsKeyEnv()
	if err != nil {
		return err
	}
	fc.apnsKeyP8 = keyPEM
	fc.apnsKeyID = os.Getenv("APNS_KEY_ID")

	fc.apnsTeamID = defaultAPNSTeamID
	if v := os.Getenv("APNS_TEAM_ID"); v != "" {
		fc.apnsTeamID = v
	}
	fc.apnsTopic = defaultAPNSTopic
	if v := os.Getenv("APNS_TOPIC"); v != "" {
		fc.apnsTopic = v
	}

	return nil
}

// readAPNsKeyEnv reads the APNs .p8 auth key from the environment.
// APNS_KEY_P8 holds the raw PKCS#8 PEM; APNS_KEY_P8_B64 holds the same PEM
// base64-encoded (the Fly / container convention — a single env line with no
// embedded newlines). The raw form wins if both are set. Returns "" (no key)
// when neither is set, which is the supported keyless mode.
//
// The value is P0 secret material: it is never logged, and a decode failure
// reports only that the decode failed.
func readAPNsKeyEnv() (string, error) {
	if raw := os.Getenv("APNS_KEY_P8"); raw != "" {
		return raw, nil
	}
	b64 := os.Getenv("APNS_KEY_P8_B64")
	if b64 == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("config.Load: decode APNS_KEY_P8_B64: %w", ErrInvalidValue)
	}
	return string(decoded), nil
}
