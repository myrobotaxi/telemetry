// Package dispatch implements P10 nav-dispatch (MYR-176): when a vehicle
// owner accepts a ride request, push the rider's pickup into the vehicle's
// Tesla navigation so the car starts driving to the passenger.
//
// # Flow
//
//	events.RideAcceptedEvent (bus, from the owner-accept handler, MYR-175)
//	  → ClaimDispatch (exactly-once latch on the ride row)
//	  → resolve VIN (vehicleId → VIN) + owner Tesla token (refresh-on-expiry)
//	  → commands.Executor.Execute("navigation_gps_request", {lat, lon, order:0})
//	  → RecordDispatchOutcome (sent | failed | skipped) + one audit log line
//
// The dispatcher SUBSCRIBES to the internal ride.accepted seam; it never
// touches the accept handler. navigation_gps_request is UNSIGNED (Tesla
// processes it server-side, so the proxy forwards it without a virtual key —
// MYR-180 registry), which is why dispatch works before command-signing
// pairing lands.
//
// # Policies
//
//   - Idempotent per ride: ClaimDispatch stamps dispatched_at only when NULL,
//     so a re-delivered ride.accepted event finds the latch set and skips —
//     nav is pushed at most once per ride.
//   - Bounded retry: transport (command_failed) and asleep-after-wake-retries
//     (vehicle_asleep) errors are retried with backoff up to MaxRetries;
//     key_not_paired and permission_denied are terminal (→ failed with the
//     code). The Executor already runs its own wake+retry on a single call.
//   - Kill-switch: Config.Enabled=false records the outcome as `skipped`
//     without any Tesla call, so the client can disable nav pushes via the
//     DISPATCH_ENABLED env var with no code change.
//   - No new lifecycle status: `accepted` stays `accepted`. The outcome is an
//     orthogonal annotation on the ride row, surfaced on the REST detail
//     (rest-api.md §7.8); no ride_status_changed frame is emitted.
//
// # Seams
//
// Every external dependency is a small consumer-site interface (VehicleResolver,
// TokenSource, CommandExecutor, OutcomeStore) so tests drive the whole matrix
// — success, asleep-exhausted, key_not_paired, permission_denied, transport,
// idempotency, kill-switch, token-failure — with fakes and no live Tesla call.
package dispatch
