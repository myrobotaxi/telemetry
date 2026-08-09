// Package dispatch implements P10 nav-dispatch (MYR-176): when a vehicle
// owner accepts a ride request, push the rider's pickup into the vehicle's
// Tesla navigation so the car starts driving to the passenger.
//
// # Flow
//
//	events.RideAcceptedEvent (bus, from the owner-accept handler, MYR-175)
//	  → hand off to a bounded worker pool (delivery returns immediately)
//	  → ClaimDispatch (exactly-once latch on the ride row)
//	  → resolve VIN (vehicleId → VIN) + owner Tesla token (refresh-on-expiry)
//	  → commands.Executor.Execute("navigation_gps_request", {lat, lon, order:1})
//	  → RecordDispatchOutcome (sent | failed | skipped) + one audit log line
//	    (persisted on a context detached from the per-event timeout)
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
//   - Bounded retry: retryable command errors (transient command_failed,
//     asleep-after-wake-retries) AND transient VIN/token resolution failures
//     are retried with backoff up to MaxRetries; permanent conditions
//     (key_not_paired, permission_denied, unconfigured transport, token
//     expired/unavailable, vehicle not found) are terminal. The Executor also
//     runs its own wake+retry on a single call.
//   - Bounded concurrency: the bus delivers serially, so the handler hands
//     each event to a worker pool (Config.MaxConcurrent) and returns at once —
//     a slow dispatch never blocks delivery of the next accept.
//   - Monotonic leg order (MYR-526): the car's nav destination is ONE
//     last-write-wins resource, so pushes serialize per vehicle and a leg that
//     has been overtaken by a later leg of the SAME ride never reaches the car
//     (recorded `skipped` / `nav_superseded`). Without it a stalled pickup push
//     can land after the dropoff push that overtook it, leaving the dash on the
//     pickup while BOTH legs correctly record `sent`. See nav_sequencer.go.
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
