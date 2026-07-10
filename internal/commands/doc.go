// Package commands implements the Tesla Fleet signed vehicle-command
// proxy layer (MYR-180): the actuation foundation for P11 (lock, climate,
// media, charge, trunk) and P10 dispatch (navigation_request, MYR-176).
//
// # Architecture: reuse the tesla-http-proxy sidecar
//
// Modern Tesla Fleet API vehicle commands require end-to-end command
// signing with a virtual key enrolled on the vehicle by the owner. Tesla
// ships that signing protocol as an open-source Go library
// (github.com/teslamotors/vehicle-command) plus a reference HTTP proxy
// (tesla-http-proxy) that converts plain REST command calls into signed
// protocol messages.
//
// This server does NOT embed the library in-process. It reuses the
// tesla-http-proxy sidecar it already runs for fleet_telemetry_config
// pushes (internal/telemetry.FleetAPIClient → TESLA_PROXY_URL). See the
// decision record in docs/operations/vehicle-commands.md §1. In short:
// the proxy process already exists, the P-256 command-signing key already
// lives ONLY in the proxy's config (TESLA_KEY_FILE_B64), and the proxy
// already implements signing, per-vehicle session caching, wake handling,
// and uniform forwarding of unsigned commands (navigation_request). The
// executor here talks to that proxy over the same transport seam.
//
// # Seams
//
//   - Registry maps a command name → {scope, signer-required, param
//     builder}. The public API command name IS the Tesla command name.
//   - Transport is the signed-session seam. ProxyTransport is the concrete
//     tesla-http-proxy client; tests substitute a fake, so no test ever
//     makes a live Tesla call.
//   - Executor gates on scope (no Tesla call when the token lacks the
//     scope), degrades to key_not_paired when signing is unconfigured, and
//     runs the bounded wake+retry and session-refresh-on-counter-error
//     policies.
//
// The layer is complete and testable WITHOUT a paired virtual key: until
// pairing (MYR-115) every signer-required command resolves to the typed
// key_not_paired error, and nothing panics when the key/proxy is absent.
package commands
