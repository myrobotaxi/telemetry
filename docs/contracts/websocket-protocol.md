# WebSocket Protocol Contract

**Status:** Draft -- v1
**Target artifact:** AsyncAPI 3.0 specification at [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml)
**Owner:** `sdk-architect` agent
**Last updated:** 2026-04-13

## Purpose

Defines every WebSocket message exchanged between the telemetry server (`internal/ws/`) and SDK clients (TypeScript, Swift, web, mobile). This contract is the authoritative source for:

- The connection handshake (auth frame, token validation, accept/reject)
- Server->client and client->server message catalogs
- The envelope schema (type discriminator, payload, planned sequence/timestamp)
- Heartbeat cadence and close-code semantics
- Reconnection and snapshot-resume rules

The markdown is the human source of truth. Its machine-readable twin is [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml). Per-message JSON Schemas live alongside in [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) and [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json). Drift between this doc, the AsyncAPI spec, the schemas, and `internal/ws/` is a CI failure ([`contract-guard`](../../CLAUDE.md#merge-policy-non-negotiable)).

Known, **accepted** divergences between this contract and the current `internal/ws/` implementation are catalogued in §10. Every such entry has a Linear follow-up title. A divergence that is not listed in §10 is contract drift and MUST be fixed, not added.

## Anchored requirements

Every FR/NFR listed here is anchored in at least one section of this doc. The tag in the "Where" column is the exact section the requirement lands in.

| ID | Requirement | Where it lands |
|----|-------------|----------------|
| **FR-1.1** | Live telemetry stream: position, speed, heading, gear | §4.1 vehicle_update; §4.1.3 GPS group; §4.1.5 gear group; §4.1.7 speed (ungrouped) |
| **FR-1.2** | Live charge state: battery level, charge state, range | §4.1.4 charge group (`chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull`) |
| **FR-1.3** | Architecture allows new telemetry fields without architectural change | §3.1 envelope open-object rule; §4.1.1 wire field mapping |
| **FR-2.1** | Nav: destination, ETA, distance, polyline, origin | §4.1.2 navigation group (note: `tripStartTime` relocated to `drive` group -- see DV-13 and §4.2) |
| **FR-2.2** | Nav fields delivered as an atomic group | §3.2 atomic-group rule; §4.1.2 server flow |
| **FR-2.3** | Nav cancellation clears the entire group atomically | §4.1.2 nav clear; §4.1.2 SDK amplification rule |
| **FR-3.1** | Live drive events: drive_started, drive_updated, drive_ended | §4.2 drive_started (carries `startedAt`, satisfying `tripStartTime` after DV-13 amendment); §4.3 drive_ended; §4.1.6 drive_updated (virtual wire form) |
| **FR-6.1** | SDK accepts a getToken() callback | §2.1 handshake; §2.3 `auth_ok` acknowledgement; §5.1 auth message |
| **FR-6.2** | SDK calls getToken() on initial connect and on every auth error | §2.2 auth frame; §6.1.1 auth_failed policy |
| **FR-7.1** | Typed error codes (no string-matching) | §6.1 error frame; §6.1.1 error code catalog |
| **FR-7.3** | Only terminal errors surface to UI; transient errors auto-retry | §6.1.1 reconnect-policy column; §6.2 close code matrix |
| **FR-8.1** | connectionState surface (5 states) | §2.4 connection state mapping; §7.2 reconnect sequence |
| **FR-8.2** | UI composes connectionState and dataState; SDK never collapses them | §4.1 group routing; §4.5 per-vehicle ownership filtering; §7.2 reconnect invariants |
| **NFR-3.1** | Atomic grouping of related fields | §3.2 atomic-group rule; §4.1 catalog. v1 charge group is `{chargeLevel, chargeState, estimatedRange, timeToFull}` (§4.1.4); `tripStartTime` has been relocated to the `drive` group via `drive_started.startedAt` (§4.2). NFR-3.1 literal amendment pending -- see DV-13. |
| **NFR-3.2** | Server-side debounce window for atomic-group accumulation | §3.2.1 debounce window. The canonical v1 window is **500 ms**, constrained by Tesla's 500 ms batch floor (not a server-side choice). NFR-3.2 literal currently reads 200 ms; amendment pending -- see DV-01. |
| **NFR-3.9** | Clear any field in a group → SDK applies clear to full group atomically | §4.1.2 nav clear; §4.1.2 SDK amplification rule |
| **NFR-3.10** | Reconnect with exponential backoff (1s/2x/30s/jitter) | §7.1 reconnect backoff parameters |
| **NFR-3.11** | Reconnect re-fetches DB snapshot, resumes live stream | §7.2 reconnect sequence; §7.3 snapshot-resume semantics |
| **NFR-3.12** | Graceful offline: cached state visible, no forced reloads | §7.2 reconnect invariants #2, #3 |
| **NFR-3.13** | Offline tolerance: no maximum on cached visibility | §7.2 reconnect invariants #3 |
| **NFR-3.19** | Every WS broadcast projected through recipient's role mask; no raw fan-out | §4.6 per-role projection at broadcast (matrix in [`rest-api.md`](rest-api.md) §5.2) |
| **NFR-3.21** | Vehicle ownership enforced on every subscription | §2.2 vehicle resolution; §4.5 ownership filtering (+ DV-09 mid-connection drift) |
| **NFR-3.22** | TLS in transit (WSS for browsers/apps) | §1.1 transport; §1.2 origin enforcement |
| **NFR-3.36** + **NFR-3.36a-d** | Apple platform lifecycle (Swift SDK only): consumer-driven foreground reconnect, suspended-socket close semantics, background-task entry points | §7.5 Apple platform suspend/resume; detailed bindings in [`swift-lifecycle.md`](swift-lifecycle.md) |

---

## 1. Transport and URL

### 1.1 Endpoint

| Field | Value | Source |
|-------|-------|--------|
| Path | `/api/ws` | [`internal/ws/handler.go`](../../internal/ws/handler.go) line 43 |
| Production scheme | `wss://` (TLS termination at the edge, **NFR-3.22**) | Fly.io edge |
| Local dev scheme | `ws://` (allowed only when origin matches dev allow-list) | [`cmd/telemetry-server/main.go`](../../cmd/telemetry-server/main.go) |
| HTTP method | `GET` (with `Upgrade: websocket` header) | RFC 6455 §4 |
| Content type | `application/json`, framed as WebSocket text messages | [`internal/ws/client.go:writeMessage`](../../internal/ws/client.go) |
| WebSocket library | [`github.com/coder/websocket`](https://github.com/coder/websocket) (never `gorilla/websocket`, per `CLAUDE.md`) | [`internal/ws/handler.go`](../../internal/ws/handler.go) line 13 |

### 1.2 Origin enforcement

The server passes `HandlerConfig.OriginPatterns` (populated from `WebSocketConfig.AllowedOrigins`) to `websocket.AcceptOptions.OriginPatterns`. Requests from origins not in the allow-list are rejected with `HTTP 403` **before** the WebSocket upgrade completes. There is no in-band error frame for this case -- the client receives an HTTP error response on the upgrade attempt; the server logs a `slog.Warn` with the rejected `Origin`, the `remote_addr`, and the request `host` so an operator chasing "why is my browser blocked?" sees the failure inline.

The allow-list resolves at startup per [`cmd/telemetry-server/adapters.go:resolveWSOriginPatterns`](../../cmd/telemetry-server/adapters.go) — operator-configured patterns always take precedence; the dev/prod split only governs the empty-config fallback (per NFR-3.22 and MYR-17):

| Mode | When `AllowedOrigins` is set | Empty-config fallback |
|------|------------------------------|------------------------|
| Production (no `--dev`) | Use the configured slice verbatim. Production ships `["https://myrobotaxi.app", "https://www.myrobotaxi.app"]` in [`configs/fly.json`](../../configs/fly.json). | Empty slice — **fail-closed**. `coder/websocket` admits same-origin and empty-`Origin` requests; every cross-origin dial is rejected with HTTP 403. A `slog.Warn` at startup reminds the operator to set `websocket.allowed_origins` or `WEBSOCKET_ALLOWED_ORIGINS`. |
| Dev (`--dev` flag) | Use the configured slice verbatim. | Localhost defaults: `localhost`, `localhost:*`, `127.0.0.1`, `127.0.0.1:*`, `[::1]`, `[::1]:*` (any scheme, any port). |

`WEBSOCKET_ALLOWED_ORIGINS` is the env-var override for `websocket.allowed_origins`. It accepts a comma-separated list (whitespace around each entry is trimmed; entirely-empty input signals fail-closed). The env var, when set, fully replaces the JSON-config allow-list rather than appending to it.

Pattern matching follows the `coder/websocket` `authenticateOrigin` rules: a pattern containing `://` is matched against `<scheme>://<host>` of the `Origin` header (so `https://myrobotaxi.app` rejects an `http://myrobotaxi.app` Origin); a pattern without `://` is matched against `<host>` only (so `localhost:*` admits any scheme on any port). Patterns use `path.Match` glob semantics — `*.myrobotaxi.app` admits one subdomain level. Same-origin requests (Origin host == request host) and requests without an `Origin` header are always admitted.

Operator escape hatches for non-localhost dev workflows: `WEBSOCKET_ALLOWED_ORIGINS=https://*.ngrok.io` admits ngrok tunnels; `https://*.trycloudflare.com` admits Cloudflare tunnels. The env var fully replaces the JSON-config allow-list so the operator must include any other origins (e.g. `https://*.ngrok.io,https://myrobotaxi.app`) in the same value.

### 1.3 Connection limits

v1 enforces **two** concurrent-connection caps with asymmetric policies:

| Cap | Default | Enforcement point | Breach response | Surfaced as |
|-----|---------|-------------------|-----------------|-------------|
| Per-IP (`HandlerConfig.MaxConnectionsPerIP`) | **64** | Pre-auth, during the HTTP upgrade | `HTTP 429 Too Many Requests` (no WS handshake, no `error` frame) | HTTP status on upgrade -> SDK treats as `rate_limited` per §6.1.1 |
| Per-user (`WebSocketConfig.MaxConnectionsPerUser`) | **5** ([`internal/config/defaults.go:67`](../../internal/config/defaults.go)) | Post-auth, after `Authenticator.ValidateToken` succeeds, before `Hub.Register` | `error` frame (`code: "rate_limited"`) followed by WS close code **`4003 Server Overload`** | `error.code = "rate_limited"` + close code 4003 per §6.1.1 / §6.2 |

**Rationale.** The per-IP cap at the upgrade layer survives corporate / campus / carrier NAT (a shared egress IP is common) while still deterring single-host floods. The per-user cap prevents a compromised token from hoarding every slot in the hub; 5 concurrent sessions is enough for a user with phone + desktop + watch + tablet + a debug session without giving an attacker unbounded leverage. The per-IP cap is deliberately higher than the per-user cap because one IP routinely aggregates many legitimate users.

**Close-code choice.** Per-user breaches close with **4003 Server Overload** (reserved in §6.2) because the user IS entitled to *some* session -- they are being shed for overload-management reasons, and the SDK should auto-reconnect with extended backoff after shedding the excess. By contrast, a per-IP breach never reaches the WebSocket layer, so there is no 4xxx code to send -- the SDK observes the HTTP 429 and falls into the §7.1 reconnect backoff.

The client IP is resolved from the leftmost `X-Forwarded-For` entry when present ([`internal/ws/handler.go:resolveClientIP`](../../internal/ws/handler.go)); `hub.ipConnectionCount` provides the per-IP counter and `Hub.Register` tracks per-user state.

> **Divergence (DV-08):** The target behavior above is the v1 contract. Today, [`cmd/telemetry-server/main.go`](../../cmd/telemetry-server/main.go) line 178 builds the `HandlerConfig` without populating `MaxConnectionsPerIP`, and `WebSocketConfig.MaxConnectionsPerUser` is likewise not threaded into the handler, so **neither cap is enforced yet**. The resolution is a wiring change (not a design change); tracked as DV-08 in §10.
>
> **Divergence (DV-14):** Neither cap defends against a slow-auth attack where each TCP connection sits under the per-IP cap but holds the 5 s `AuthTimeout` window. Mitigation is deferred to a follow-up: either a dedicated pre-auth rate-limit on upgrade *attempts* (not just concurrent holdings) OR a shortened `AuthTimeout` under load. Tracked as DV-14 in §10.

SDKs MUST treat both HTTP 429 on upgrade AND WebSocket close code 4003 as `rate_limited` signals, apply the reconnect backoff from §7.1 (with extended ceiling for 4003 per §6.2), and surface `rate_limited` as a typed error per FR-7.1.

---

## 2. Connection handshake

> **Anchored:** FR-6.1, FR-6.2, NFR-3.21, NFR-3.22, FR-8.1.

### 2.1 Sequence

```
Client                                       Server
  |                                            |
  |--- HTTP GET /api/ws (Upgrade: websocket) ->|
  |                                            |
  |                              [origin check]
  |                              [per-IP cap check (DV-08 unwired today)]
  |                                            |
  |<-- HTTP 101 Switching Protocols -----------|
  |                                            |
  |                              [auth deadline starts: 5s default]
  |                                            |
  |--- {"type":"auth","payload":{"token":"..."}} -->
  |                                            |
  |                              [Authenticator.ValidateToken]
  |                              [Authenticator.GetUserVehicles]
  |                              [per-user cap check (DV-08 unwired today)]
  |                                            |
  |  -- success path --                        |
  |                              [Hub.Register, start read+write pumps]
  |                                            |
  |<-- {"type":"auth_ok","payload":{"userId":"...","vehicleCount":N,"issuedAt":"..."}}
  |                                            |
  |   <-- vehicle_update / drive_* / heartbeat |   (live stream begins)
  |                                            |
  |  -- failure path --                        |
  |<-- {"type":"error","payload":{"code":"auth_failed",...}}
  |<-- WebSocket close frame (code 1008, "authentication failed")
```

Implementation: [`internal/ws/handler.go:handleUpgrade`](../../internal/ws/handler.go) -> `authenticateClient`.

### 2.2 Auth frame requirements

1. **First frame.** The client MUST send the auth frame as its FIRST WebSocket frame after the upgrade. Any other `type` value before auth is treated as an auth failure ([`handler.go:authenticateClient`](../../internal/ws/handler.go) line 136: `if msg.Type != msgTypeAuth { return error }`).
2. **No `Authorization` header.** The token MUST NOT be sent as an HTTP header on the upgrade request. Browsers cannot set arbitrary headers on the WebSocket upgrade, so the in-band frame is the only portable channel. Native clients MAY send a duplicate header for defense in depth, but the server ignores it.
3. **Deadline.** The server enforces a 5-second deadline for the auth frame (`HandlerConfig.AuthTimeout`, default `5*time.Second` from `applyHandlerDefaults`). Exceeding the deadline produces error code `auth_timeout` followed by close code 1008.
4. **Token format.** Opaque to the WebSocket layer. The configured `Authenticator` ([`internal/ws/auth.go`](../../internal/ws/auth.go)) validates it. Production uses `internal/auth.NewJWTAuthenticator` (checks signature, issuer, audience, expiry against `AuthConfig`). Dev uses `ws.NoopAuthenticator`, which accepts any non-empty token and returns the configured `VehicleIDs`.
5. **Vehicle resolution.** On a valid token the server calls `Authenticator.GetUserVehicles(ctx, userID)` and stores the resulting vehicle IDs on the `Client` struct ([`client.go:Client.vehicleIDs`](../../internal/ws/client.go)). The set is a snapshot of ownership at handshake time; per-broadcast ownership filtering uses this snapshot (§4.5). See DV-09 for the mid-connection refresh gap.
6. **Token redaction.** The token is **P1** per [`data-classification.md`](data-classification.md) §1.2 (`AuthPayload.token`). It MUST NOT appear in any structured log, error message, metric label, or crash report. The server logs only the resolved `userID` (P0) and `vehicle_count` (P0) -- see [`hub.go:Hub.Register`](../../internal/ws/hub.go) line 40.

### 2.3 Auth result envelope

v1 requires the server to emit a **positive** acknowledgement frame, `auth_ok`, as the FIRST frame the client receives after a successful handshake. Its purpose is to give the SDK a deterministic transition out of `connecting` without having to wait for telemetry traffic -- which on idle vehicles may not arrive for up to one heartbeat interval (default 15 s, §7.4) and on a cold watchOS wake is the entire session.

```jsonc
{
  "type": "auth_ok",
  "payload": {
    "userId":       "clxyz1234567890userid",
    "vehicleCount": 3,
    "issuedAt":     "2026-04-13T18:22:00Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `userId` | `string` (cuid) | **P1** | Echoed back from the token-resolved `userID`. Lets the SDK sanity-check ownership set on reconnect (e.g., compare against a cached user ID). |
| `vehicleCount` | `integer` | **P1** | Size of the user's vehicle set at handshake time. Gives the SDK a quick integrity check against its cached snapshot. |
| `issuedAt` | `string` (ISO 8601 UTC) | **P2** | Server's `time.Now().UTC()` at the moment `Hub.Register` succeeded. Debug-only. |

SDK handling:

1. On receipt of `auth_ok`, the SDK transitions `connectionState`: `connecting -> connected` (state-machine.md C-3; C-3 is the single transition that flips `connectionState` from `connecting` to `connected` -- see [`state-machine.md`](state-machine.md) §1.3). This is the canonical trigger for C-3 -- the SDK MUST NOT use "first data frame" or "first heartbeat" as the trigger. DV-15 RESOLVED: `state-machine.md` §1.3 C-3 now reads `AUTH_OK_RECEIVED`, aligned by MYR-31.
2. If the `userId` field does not match a previously-cached user ID for this consumer, the SDK SHOULD surface this as a warning and clear any per-user cache that may contain values from the wrong account. This is a defense against token mix-up bugs in consumer code, not a server-side issue.
3. `vehicleCount` is informational; the authoritative ownership set is populated on the next REST snapshot fetch (§7.2).
4. **Pre-`auth_ok` liveness bound.** The SDK MUST bound its wait for `auth_ok` with a local timer of **6 seconds** (1-second grace over the server's 5-second `HandlerConfig.AuthTimeout`, to absorb one-way network latency). The timer starts the moment the `auth` frame has been handed to the socket (i.e., alongside C-1 `initializing -> connecting`). If `auth_ok` has not arrived AND no `error` frame has arrived AND the WebSocket has not closed within this window, the SDK MUST treat this as a silent handshake failure: close the socket locally with code 1001 and transition `connecting -> disconnected` (C-4) with a typed reason of `auth_timeout`. This bounds the "Connecting..." UI state on degraded paths (e.g., upgrade succeeded but the server stalled post-`Hub.Register`). Without this bound the only fallback is the §7.4.1 liveness watchdog, which does not start until the first frame arrives and therefore cannot cover the pre-`auth_ok` window. The liveness watchdog (§7.4.1) is a separate, post-`auth_ok` mechanism.

The server **also** sends an explicit error frame on failure (§6.1) followed by a close frame with code 1008. Failure and success paths are mutually exclusive -- the client either sees `auth_ok` (success) or an `error` frame followed by close 1008 (failure) or the SDK pre-`auth_ok` timer expires (silent failure), never more than one.

> **Note on DV-07:** `auth_ok` is v1-required (pulled out of DV-07 in MYR-33). The control-frame portion of DV-07 — `subscribe` / `unsubscribe` / `ping` / `pong` plus the typed `vehicle_not_owned` error and close code 4002 — is RESOLVED by MYR-46. The `sinceSeq` snapshot-resume sub-row remains open under DV-02 (envelope sequence numbers).

### 2.4 Connection state mapping

The handshake drives the following [`state-machine.md`](state-machine.md) §1.3 transitions:

| Wire event | `connectionState` transition | Notes |
|------------|------------------------------|-------|
| HTTP 101 + outbound `auth` frame | `initializing -> connecting` (C-1) | Token in flight |
| Receipt of `auth_ok` frame | `connecting -> connected` (C-3) | Canonical trigger. Reset retry counter; start SDK liveness watchdog (§7.4.1). DV-15 RESOLVED: [`state-machine.md`](state-machine.md) §1.3 C-3 now reads `AUTH_OK_RECEIVED`, aligned by MYR-31. |
| SDK pre-`auth_ok` timer expires (6 s, §2.3 rule 4) | `connecting -> disconnected` (C-4) | Bounds the "Connecting..." UI state on degraded paths (post-upgrade stall, dropped `auth_ok` in flight). Surface as typed `auth_timeout`; auto-retry with backoff. Independent of the post-`auth_ok` liveness watchdog (§7.4.1). |
| `Authenticator.ValidateToken` returns error | `connecting -> error` (C-5 terminal if `auth_failed`) | Surface `auth_failed` typed error; no auto-retry |
| Auth deadline exceeded (`ErrAuthTimeout`) | `connecting -> disconnected` (C-4) | Surface `auth_timeout`; auto-retry with backoff. See DV-06. |
| Per-user cap breach (close code 4003) | `connecting -> disconnected` (C-4) | Surface `rate_limited`; auto-retry with extended backoff. See DV-08. |
| HTTP 429 on upgrade (per-IP cap) | `connecting -> disconnected` (C-4) | Surface `rate_limited`; apply reconnect backoff. See DV-08. |
| HTTP 403 on upgrade (origin) | `connecting -> error` (C-5 terminal) | Consumer must fix origin config |

The transition IDs (C-1..C-5) are defined in [`state-machine.md`](state-machine.md) §1.3.

> **Divergence (DV-15): RESOLVED.** The authoritative C-3 trigger is receipt of `auth_ok` (this doc's §2.3). [`state-machine.md`](state-machine.md) §1.3 C-3 now reads `AUTH_OK_RECEIVED`, aligned by MYR-31. Both docs agree on the canonical trigger. See §10 DV-15 for the audit trail.

---

## 3. Envelope schema

> **Anchored:** FR-1.3 (extensibility), NFR-3.1, NFR-3.11.
>
> **Schemas:**
> - [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) (top-level envelope)
> - [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) (per-`type` payloads, discriminated by `$defs`)

### 3.1 Wire shape

Every frame is a JSON object with the following top-level keys:

```jsonc
{
  "type": "vehicle_update",   // string discriminator (required)
  "payload": { ... },         // type-specific object (omitted on bare control frames)
  "seq":  42,                 // PLANNED: monotonic per-connection sequence (DV-02)
  "ts":   "2026-04-13T18:22:01Z" // PLANNED: server-authoritative envelope timestamp (DV-02)
}
```

| Field | Required today | Direction | Description |
|-------|----------------|-----------|-------------|
| `type` | YES | both | Discriminator. See §4 (server->client) and §5 (client->server) for the catalog. Enumerated in [`ws-envelope.schema.json#/properties/type/enum`](schemas/ws-envelope.schema.json). |
| `payload` | Per-type | both | Type-specific object. Omitted on `heartbeat` ([`messages.go:wsMessage.Payload`](../../internal/ws/messages.go) uses `json:"payload,omitempty"` and [`heartbeat.go`](../../internal/ws/heartbeat.go) marshals the bare envelope once at init). |
| `seq` | NO (PLANNED) | server->client | Monotonic per-connection sequence number. NOT currently emitted -- see DV-02. |
| `ts` | NO (PLANNED) | server->client | Server-authoritative ISO 8601 UTC envelope timestamp. NOT currently emitted -- see DV-02. Today, `vehicle_update.payload.timestamp`, `drive_started.payload.timestamp`, etc. carry the same information at the payload level. |

The Go struct that produces this envelope is [`internal/ws/messages.go:wsMessage`](../../internal/ws/messages.go):

```go
type wsMessage struct {
    Type    string          `json:"type"`
    Payload json.RawMessage `json:"payload,omitempty"`
}
```

**Open-object rule (FR-1.3).** All SDK parsers MUST treat the envelope and every payload as open objects: unknown keys are permitted at both levels and MUST be ignored silently. This gives us forward compatibility for new fields without a breaking wire change when FR-1.3 is exercised (extensibility as pattern). Strictness is enforced at validation time by JSON Schema / contract-tester fixtures, not at parse time in the SDK hot path.

> **Note on `additionalProperties: false` in the JSON Schemas.** Both [`ws-envelope.schema.json`](schemas/ws-envelope.schema.json) and [`ws-messages.schema.json`](schemas/ws-messages.schema.json) set `additionalProperties: false` at every level. This is the **`contract-tester` invariant** -- it asserts that every fixture and every captured wire frame validated in CI contains ONLY the fields declared in this contract. It is NOT a runtime SDK requirement: the SDK hot path MUST remain permissive (ignore unknown keys, never throw on extras). Put differently, the schemas' `additionalProperties: false` means "if this frame reaches a conformance test, it MUST be exactly this shape"; the open-object rule means "if this frame reaches an SDK parser, extras MUST be tolerated." The two rules coexist because contract-tester catches drift at PR time while the SDK ships to production built against whichever schema was canonical when it was generated -- and that schema might be older than what the server emits.

### 3.2 Atomic-group rule (NFR-3.1)

A single `vehicle_update` frame's `payload.fields` map MUST contain members of **at most one** atomic group, plus any number of individually-delivered (ungrouped) fields. The atomic groups are declared in [`vehicle-state-schema.md`](vehicle-state-schema.md) §2 and reproduced here for wire-level clarity:

| Group | Members (wire field names) | Classification summary |
|-------|----------------------------|------------------------|
| `navigation` | `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates` | Mixed -- see §4.1.2 |
| `charge` | `chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull` | All **P0** |
| `gps` | `latitude`, `longitude`, `heading` | `lat`/`lng` **P1** (encrypted at rest), `heading` **P0** |
| `gear` | `gearPosition`, `status` | All **P0** |
| `drive` | `startedAt` (carried via `drive_started.payload.startedAt`) | **P0**. This is the v1 home of `tripStartTime` per DV-13. The drive group is not a `vehicle_update.fields` group; it is delivered via the `drive_started` lifecycle message (§4.2) and is never interleaved with telemetry frames. |

`destinationAddress` is nullable on the wire (Prisma `String?`). It is a full member of the navigation atomic group and participates in the active-navigation predicate as of MYR-24 (2026-04-23); the prior spec-only exemption in [`vehicle-state-schema.md`](vehicle-state-schema.md) §3.1 has been retired.

Ungrouped fields (delivered individually, no group membership): `speed`, `odometerMiles`, `interiorTemp`, `exteriorTemp`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `lastUpdated`, and the drive-only `driveTrailCoordinates` field (§4.1.6). Their classification tiers are defined in [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1.

**Server enforcement** ([`internal/ws/nav_broadcast.go:handleTelemetry`](../../internal/ws/nav_broadcast.go)):

1. For each incoming telemetry event, fields are partitioned by atomic group using [`groupOf`](../../internal/ws/atomic_groups.go) (the single source of truth for group membership, per [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1). Navigation-group fields are split off; everything else (charge / gps / gear / individual) is treated uniformly as immediate-broadcast.
2. Navigation-group fields are added to the `groupAccumulator` ([`internal/ws/group_accumulator.go`](../../internal/ws/group_accumulator.go)), which merges them over a 500 ms flush window per `(group, VIN)` slot and flushes a single `vehicle_update` per slot via `flushGroup` ([`internal/ws/nav_broadcast.go:flushGroup`](../../internal/ws/nav_broadcast.go)). In v1 only `groupNavigation` registers an accumulator slot; the accumulator is generalized over groupID so a future MYR can opt charge or another group in.
3. Non-nav fields broadcast immediately via a separate `vehicle_update` frame from `handleTelemetry`.

The result is that a single frame never carries members from two different atomic groups. SDKs MUST validate this rule on receipt and treat a frame carrying fields from more than one atomic group as a contract violation (log it and discard the foreign-group fields; do not crash).

**Atomic groups that do NOT map to a dedicated wire accumulator:** `charge`, `gps`, and `gear`. The server delivers these groups' fields in one `vehicle_update` **iff** Tesla emits the members within a single upstream telemetry batch -- concretely, a Tesla `Payload` protobuf message with a `data[]` array of `Datum` entries covering those fields (see the [`tesla-fleet-telemetry-sme`](../../.claude/skills/tesla-fleet-telemetry-sme/) skill's `data-fields-and-protobuf.md` §"Payload wire shape" for the exact identifiers). Tesla's emission model (value-change + interval) typically batches `{latitude, longitude, heading}` and `{chargeLevel, estimatedRange}` together in the same `Payload`, which satisfies NFR-3.1 for those groups without an extra server-side debounce. Gear updates are typically single-field (`gearPosition`) with `status` derived at broadcast time -- see §4.1.5.

#### 3.2.1 Debounce window (NFR-3.2)

The canonical v1 debounce window for atomic-group accumulation on the server is **500 ms** (`defaultGroupFlushInterval` in [`internal/ws/group_accumulator.go`](../../internal/ws/group_accumulator.go)).

> **Scope note.** The 500 ms debounce applies ONLY to the **navigation** atomic group. GPS position (`latitude`, `longitude`, `heading`) is ungrouped at the server and arrives on Tesla's per-field emission cadence (see §4.1.3 and §4.1.7). `speed` is likewise ungrouped (§4.1.7, DV-10). A consumer rendering a live map therefore sees position updates at Tesla's native cadence (typically ~2 Hz driven by the 10-meter GPS delta filter, independent of the nav group), while nav-group fields (ETA, destination, polyline, trip distance) refresh at most twice per second. Two-hertz nav refresh is perceptually smooth for route-line rendering because the underlying polyline rarely changes shape at that rate; it is the destination/ETA text readout that benefits most from the debounce by avoiding half-updated reads.

**This is NOT a server-side design choice -- it is a Tesla-side floor.** The Tesla fleet telemetry emission model batches field changes in **500 ms buckets on the vehicle side**: multiple field changes that occur within the same 500 ms window are already coalesced into a single upstream `Payload` message before the server sees them. A shorter server-side accumulator window (e.g., the 200 ms literal in the current NFR-3.2 text) would fire before straggler fields from the SAME logical update have arrived, causing exactly the half-updated UI race that the accumulator exists to prevent. See the [`tesla-fleet-telemetry-sme`](../../.claude/skills/tesla-fleet-telemetry-sme/) skill's `architecture-and-setup.md` §"Emission model" for the authoritative reference on the 500 ms bucket behavior.

> **Note on `interval_seconds` minimums.** The MyRoboTaxi server configures its nav-field requests at `interval_seconds: 1` in [`internal/telemetry/fleet_api_fields.go`](../../internal/telemetry/fleet_api_fields.go) lines 112-118. Whether Tesla itself imposes a hard **1-second minimum** on the `interval_seconds` parameter is currently undocumented in the Tesla sources the `tesla-fleet-telemetry-sme` skill has verified -- the 1-second value is our highest-cadence request, not a published Tesla floor. The 500 ms vehicle-side batch behavior above is the authoritative argument for the 500 ms accumulator window; the `interval_seconds` parameter sets the REQUESTED cadence, while the 500 ms bucket sets the DELIVERED-as-one-message cadence. Do not conflate the two.

The NFR-3.2 literal of 200 ms was authored before this Tesla constraint was understood and is incorrect. Amending NFR-3.2 in `requirements.md` from 200 ms to 500 ms is a prerequisite follow-up tracked as **DV-01** in §10 (requirement-drift divergence, not implementation drift -- the server is correct and the requirement is wrong).

For the purpose of this wire contract: **SDKs MUST NOT assume any specific debounce timing**. Two updates to the same atomic group arriving within the debounce window MAY be coalesced into a single frame. The only guarantee is the atomic-group rule above.

### 3.3 Sequence and timestamp (NFR-3.11)

The Linear AC for MYR-11 calls for a "type discriminator, sequence, timestamp" envelope. Today only the `type` discriminator and per-payload `timestamp` field exist. The envelope-level `seq` and `ts` fields are **PLANNED** (DV-02). This doc and the AsyncAPI spec describe the **target shape** so SDK consumers can plan for it. Until the server begins emitting these fields, SDKs MUST tolerate their absence.

Rationale for not shipping `seq`/`ts` in v1.0:

1. The current server has no per-connection sequence counter — the [`client.go:Client`](../../internal/ws/client.go) struct does not carry a `nextSeq` field, and adding one would touch every broadcast hot-path.
2. Reconnect-resume is currently implemented entirely client-side via REST snapshot fetch (see [`state-machine.md`](state-machine.md) §5), which establishes a consistent baseline without needing wire-level sequence numbers. The trade-off: clients cannot detect dropped frames within a single connection.
3. Adding `seq` is a coordinated server + SDK + fixture change. It is a v1.x extension, not a v1.0 ship blocker.

> **SDK MUST:** treat `seq` and `ts` as optional. When they appear, prefer envelope `ts` over payload `timestamp` for ordering.
>
> **SDK SHOULD:** maintain a per-connection "highest seq seen" counter as soon as the server begins emitting `seq`, and pass it as `subscribe.sinceSeq` (§5.2) on reconnect.

### 3.4 Frame size

The server enforces a 4 KiB read limit on inbound (client->server) frames via `Conn.SetReadLimit(readLimit)` ([`internal/ws/client.go`](../../internal/ws/client.go) line 19). The expected client traffic is exclusively `auth` plus the PLANNED control frames -- 4 KiB is generous.

Outbound (server->client) frames have no hard cap. Long `navRouteCoordinates` payloads from city-scale Tesla routes are the largest realistic frames (~5-15 KB serialized). SDKs MUST tolerate server->client frames up to 1 MB without truncation; beyond that is a server-side bug.

---

## 4. Server -> client message catalog

> **Anchored:** FR-1.1, FR-1.2, FR-1.3, FR-2.1, FR-2.2, FR-2.3, FR-3.1, FR-7.1, FR-8.1, FR-8.2, NFR-3.1, NFR-3.2.

This section is the wire-level catalog. Field-level types, units, nullability, and per-field data classification live in [`vehicle-state-schema.md`](vehicle-state-schema.md) and the canonical JSON Schema ([`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json)) -- they are not duplicated here.

### Catalog summary

| `type` | Direction | Source (Go) | Atomic group | Triggers `dataState` transition | Fixture (planned) |
|--------|-----------|-------------|--------------|---------------------------------|-------------------|
| `auth_ok` | server->client | [`handler.go:sendAuthOk`](../../internal/ws/handler.go) | n/a | `connecting -> connected` (C-3) | [`fixtures/websocket/auth_ok.json`](fixtures/README.md) |
| `vehicle_update` | server->client | [`nav_broadcast.go`](../../internal/ws/nav_broadcast.go) / [`route_broadcast.go`](../../internal/ws/route_broadcast.go) | one of `navigation`, `charge`, `gps`, `gear`, or none | per-group `ready/cleared/error -> ready` (D-3 / D-9 / D-12), or `ready -> cleared` on nav clear (D-5) | [`fixtures/websocket/vehicle_update.*.json`](fixtures/README.md) |
| `drive_started` | server->client | [`broadcaster.go:handleDriveStarted`](../../internal/ws/broadcaster.go) | n/a | drive lifecycle `idle -> driving` (DR-1) / `ended -> driving` (DR-6) | [`fixtures/websocket/drive_started.json`](fixtures/README.md) |
| `drive_ended` | server->client | [`broadcaster.go:handleDriveEnded`](../../internal/ws/broadcaster.go) | n/a | drive lifecycle `driving -> ended` (DR-3) | [`fixtures/websocket/drive_ended.json`](fixtures/README.md) |
| `connectivity` | server->client | [`broadcaster.go:handleConnectivity`](../../internal/ws/broadcaster.go) | n/a | none directly (informational; see §4.4) | [`fixtures/websocket/connectivity.{online,offline}.json`](fixtures/README.md) |
| `heartbeat` | server->client | [`heartbeat.go:RunHeartbeat`](../../internal/ws/heartbeat.go) | n/a | resets SDK liveness watchdog (§7.4.1) | [`fixtures/websocket/heartbeat.json`](fixtures/README.md) |
| `error` | server->client | [`handler.go:sendError`](../../internal/ws/handler.go) | n/a | `connecting -> disconnected` or `connecting -> error` (C-4 / C-5) | [`fixtures/websocket/error.{auth_failed,auth_timeout}.json`](fixtures/README.md) |

All fixture files are authored in [`fixtures/`](fixtures/) -- see [`fixtures/README.md`](fixtures/README.md) for the complete index. DV-05 is **RESOLVED** by MYR-13.

### 4.1 `vehicle_update`

> **Anchored:** FR-1.1, FR-1.2, FR-1.3, FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.5, NFR-3.9.
>
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/VehicleUpdatePayload`](schemas/ws-messages.schema.json)

The primary telemetry payload. One frame carries field updates for one vehicle, scoped to at most one atomic group per §3.2.

```jsonc
{
  "type": "vehicle_update",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "fields": {
      // members of AT MOST one atomic group + any number of ungrouped fields
    },
    "timestamp": "2026-04-13T18:22:01Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `payload.vehicleId` | `string` (cuid) | **P0** | Opaque DB ID, never VIN (FR-4.2, `data-classification.md` §1.3 `Vehicle.id`). |
| `payload.fields` | `object` | mixed per-field | See [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1. Atomic-group membership enforced per §3.2. |
| `payload.timestamp` | `string` (ISO 8601 UTC) | **P0** | Server's `time.Now().UTC()` at broadcast (for nav flushes) or telemetry event `CreatedAt` (for non-nav broadcasts). See [`nav_broadcast.go:handleTelemetry`](../../internal/ws/nav_broadcast.go) and [`flushGroup`](../../internal/ws/nav_broadcast.go). |

#### 4.1.1 Wire field names vs. internal names

The wire field names in `payload.fields` are the **frontend / SDK** names, not the Tesla protobuf names. The server translates internal telemetry names to client names in [`internal/ws/field_mapping.go:internalToClientField`](../../internal/ws/field_mapping.go):

| Tesla / internal name | Wire field name |
|-----------------------|-----------------|
| `soc` | `chargeLevel` |
| `gear` | `gearPosition` |
| `odometer` | `odometerMiles` |
| `insideTemp` | `interiorTemp` |
| `outsideTemp` | `exteriorTemp` |
| `minutesToArrival` | `etaMinutes` |
| `milesToArrival` | `tripDistanceRemaining` |
| `fsdMilesSinceReset` | `fsdMilesSinceReset` |
| `location` (compound) | split into `latitude` + `longitude` |
| `destinationLocation` | split into `destinationLatitude` + `destinationLongitude` |
| `originLocation` | split into `originLatitude` + `originLongitude` |
| `routeLine` (Tesla encoded polyline) | `navRouteCoordinates` (decoded `[lng, lat]` array) |

Integer rounding is applied server-side to the fields listed in `integerFields` (`speed`, `heading`, `chargeLevel`, `estimatedRange`, `etaMinutes`, `interiorTemp`, `exteriorTemp`, `odometerMiles`, `fanSpeed`, seat heater and climate settings). See [`field_mapping.go:roundIfInteger`](../../internal/ws/field_mapping.go).

SDKs MUST accept the integer-rounded values as-is and MUST NOT round-trip through floats.

#### 4.1.2 Navigation group

> **Anchored:** FR-2.1, FR-2.2, FR-2.3, NFR-3.1, NFR-3.2, NFR-3.9.
> **dataState target:** `dataState.navigation` (per [`state-machine.md`](state-machine.md) §2)
> **Fixtures:** `vehicle_update.nav_active.json`, `vehicle_update.nav_clear.json` (planned, [`fixtures/README.md`](fixtures/README.md))

Members (per [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.1): `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`.

| Field | Classification | Encrypted at rest (AES-256-GCM) |
|-------|----------------|----------------------------------|
| `destinationName` | **P1** | No (disk encryption only) |
| `destinationAddress` | **P1** | No |
| `destinationLatitude` | **P1** | Yes |
| `destinationLongitude` | **P1** | Yes |
| `originLatitude` | **P1** | Yes |
| `originLongitude` | **P1** | Yes |
| `etaMinutes` | **P0** | No |
| `tripDistanceRemaining` | **P0** | No |
| `navRouteCoordinates` | **P1** | Yes |

Encryption flags are from [`data-classification.md`](data-classification.md) §3.1.

**Server flow:** All nav-related Tesla fields (`destinationName`, `destinationLocation`, `originLocation`, `routeLine`, `minutesToArrival`, `milesToArrival`) are routed through [`groupAccumulator.Add(groupNavigation, …)`](../../internal/ws/group_accumulator.go) via [`handleTelemetry`](../../internal/ws/nav_broadcast.go). The accumulator starts a timer on the first nav field for a VIN and merges subsequent nav fields within the flush window. On timer expiry the batch is delivered via [`flushGroup`](../../internal/ws/nav_broadcast.go), which resolves VIN -> vehicleId, maps fields, appends a `lastUpdated`, and broadcasts a single `vehicle_update`. The accumulator is also drained synchronously on `drive_ended` and on `connectivity.online=false` ([`broadcaster.go`](../../internal/ws/broadcaster.go)), so pending nav fields never outlive their relevance.

**Nav clear (FR-2.3):** When Tesla marks a nav field as `Invalid`, the server translates each invalid field to a JSON `null` in `payload.fields`. The mapping is defined by [`navClearFields`](../../internal/ws/field_mapping.go) line 78:

| Internal field invalidated | Client fields set to `null` |
|----------------------------|------------------------------|
| `destinationName` | `destinationName` |
| `milesToArrival` | `tripDistanceRemaining` |
| `minutesToArrival` | `etaMinutes` |
| `routeLine` | `navRouteCoordinates` |
| `originLocation` | `originLatitude`, `originLongitude` |
| `destinationLocation` | `destinationLatitude`, `destinationLongitude` |

**SDK amplification rule (NFR-3.9 + Rule CG-SM-3):** When ANY nav group field arrives as `null`, the SDK MUST null ALL navigation group fields atomically in its in-memory state, regardless of whether the server explicitly sent every member. The server is permitted (and does today) to send a partial clear -- the SDK is responsible for amplifying it to the full group. This is non-negotiable: it's the only way to satisfy the "no half-cleared nav" invariant (NFR-3.4).

#### 4.1.3 GPS group

> **Anchored:** FR-1.1, NFR-3.1.
> **dataState target:** `dataState.gps`
> **Fixture:** `vehicle_update.gps.json` (planned)

Members: `latitude`, `longitude`, `heading`.

| Field | Classification | Encrypted at rest |
|-------|----------------|-------------------|
| `latitude` | **P1** | Yes (AES-256-GCM) |
| `longitude` | **P1** | Yes |
| `heading` | **P0** | No |

**Server flow:** When Tesla emits a `Location` compound value, the server splits it into separate `latitude` and `longitude` keys via [`splitLocationField`](../../internal/ws/field_mapping.go). `heading` is delivered alongside whenever Tesla emits `GpsHeading`. There is no dedicated server-side accumulator for GPS -- `handleTelemetry` broadcasts non-nav fields immediately. Tesla's upstream emission typically batches `location` + `heading` together, which delivers them in the same outbound `vehicle_update` in practice.

**0,0 sentinel:** Per [`vehicle-state-schema.md`](vehicle-state-schema.md) §2.3, the DB default for unset coordinates is `(0, 0)`. SDKs MUST treat `latitude == 0 && longitude == 0` as "no fix" rather than a valid point in the Gulf of Guinea.

#### 4.1.4 Charge group

> **Anchored:** FR-1.2, NFR-3.1.
> **dataState target:** `dataState.charge`
> **Fixture:** `vehicle_update.charge.json` (planned)

Members (v1): `chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull`. All **P0** (log-safe, no encryption at rest).

| Field | Type | Unit | Tesla source | Notes |
|-------|------|------|--------------|-------|
| `chargeLevel` | `integer` (0-100) | percent | proto field `Soc` / `BatteryLevel` | Integer-rounded server-side. |
| `chargeState` | `string` (enum) | -- | proto field **179** (`DetailedChargeState`) | Enum values: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`. **Re-sourced from proto 2 to proto 179 by MYR-42 on 2026-04-23** — empirical capture showed Tesla firmware ≥ 2024.44.25 accepts proto 2 in `fleet_telemetry_config` but never emits it. Proto 179 fires on the same transitions with identical enum string values via the `Value_DetailedChargeStateValue` oneof variant (primary) or `Value_ChargingValue` (pre-2024.44.25 fallback). See §10 DV-19 for the full capture + analysis. |
| `estimatedRange` | `integer` | miles | proto `EstBatteryRange` | Integer-rounded server-side. |
| `timeToFull` | `number` | **hours** (decimal) | proto field **43** (`TimeToFullCharge`, `double`) | **Hours** (decimal, fractional values supported -- e.g. `1.5` for 90 minutes) to full charge at the current rate. `0` (or `null` when disconnected) means no active charging session. Unit is sourced from the `tesla-fleet-telemetry-sme` skill's `data-fields-and-protobuf.md` §"TimeToFullCharge" ("Estimated hours to reach charge limit") and the legacy Tesla REST API `time_to_full_charge` field, both of which consistently use hours. Tesla proto type is `double`, so the wire value is a JSON number (NOT rounded to integer). **Empirical verification of the unit against a charging vehicle is tracked as DV-17** -- if the observed value disagrees with the skill reference, this row MUST be corrected before any SDK build that generates types against it ships. |

**Server flow:** Mapped from Tesla's native fields above. All four fields are in `DefaultFieldConfig` at the 30-second cadence (MYR-40 landed `FleetFieldChargeState` and `FleetFieldTimeToFullCharge`; `FleetFieldSOC` / `FleetFieldBatteryLevel` / `FleetFieldEstBatteryRange` were already there). When any field changes in the same Tesla 500 ms vehicle-side bucket, they arrive in a single upstream `Payload` protobuf message and the broadcaster emits a four-field batch in one `vehicle_update`. If only a subset changes within a bucket, Tesla emits just that subset — the atomic-group promise is "siblings are included when change happens together," not "every frame carries all four."

**Implementation status:** wire delivery is live as of MYR-40; REST `/snapshot` persistence (DB columns for `chargeState` + `timeToFull` on the Prisma-owned `Vehicle` table) landed in MYR-41 (cross-repo Prisma migration in `../react-frontend` + Go `internal/store` SELECT/UPDATE wiring). Both fields are now persisted on every telemetry event via the writer pipeline; the cold-start snapshot reads them back from the DB.

> **SDK MUST** continue to tolerate `null` for `chargeState` and `timeToFull` on REST `/snapshot` responses. The DB columns are nullable — a vehicle that has never charged (or whose first charge frame has not arrived yet after the MYR-41 deploy) will surface `null` until Tesla emits a value. Steady-state non-null is expected once the vehicle has charged at least once post-deploy.

> **Generated SDK types.** `chargeState` and `timeToFull` remain nullable in generated SDK types (`string | null` in TypeScript, `String?` / `Optional<String>` in Swift, `Double?` for `timeToFull` in Swift) — the DB columns are `String?` / `Float?` and may be null for vehicles that have never charged.
>
> **Consumer UI.** SDK consumers rendering charge state SHOULD surface `null` as a neutral placeholder (`--`, `—`, or hide the field entirely), NOT as a loading spinner. With `dataState.charge = ready` and `chargeState: null`, the steady-state meaning is "no data yet from Tesla for this field," NOT "loading in progress." A spinner implies work is in flight, which is misleading. A time-to-full display might read `— minutes remaining` while the vehicle is disconnected; a charge-state badge might hide until `chargeState` becomes non-null.

#### 4.1.5 Gear group

> **Anchored:** NFR-3.1.
> **dataState target:** `dataState.gear`
> **Fixture:** `vehicle_update.gear.json` (planned)

Members: `gearPosition`, `status`. Both **P0**.

`status` is **derived server-side** at broadcast time by [`deriveVehicleStatus`](../../internal/ws/field_mapping.go) from `gearPosition` (and `speed` as a fallback when gear is missing). The broadcaster injects `status` into the `vehicle_update` iff `gearPosition` is present in the same frame:

```go
// internal/ws/nav_broadcast.go:60-62
if _, hasGear := fields["gearPosition"]; hasGear {
    fields["status"] = deriveVehicleStatus(fields)
}
```

Per [`vehicle-state-schema.md`](vehicle-state-schema.md) §3.4 predicate 1, the SDK MUST validate the gear-to-status derivation on receipt: `D` or `R` => `driving`, `P` or `N` => `parked` (unless overridden by `charging`, `offline`, or `in_service` from server-side logic). A mismatch is logged as a consistency error but the frame is still applied.

#### 4.1.6 Drive route updates (`drive_updated` is virtual)

> **Anchored:** FR-3.1.
> **dataState target:** `dataState.gps`
> **Drive lifecycle target:** `driving -> driving` (DR-2)
> **Fixture:** [`vehicle_update.route.json`](fixtures/websocket/vehicle_update.route.json)

**`driveTrailCoordinates` vs `navRouteCoordinates` -- two different polylines.** The wire carries two distinct route polylines and they MUST NOT be conflated:

| Wire field | Source | Semantics | Atomic group? | Section |
|---|---|---|---|---|
| `driveTrailCoordinates` | [`routeAccumulator`](../../internal/ws/route_accumulator.go) (server-side, fed by `drive_updated` events) | **Where the car has been** -- the accumulated GPS trail of the active drive | No -- drive-only ungrouped field, routed to `dataState.gps` | This section (§4.1.6) |
| `navRouteCoordinates` | Tesla `routeLine` protobuf decode ([`internal/ws/field_mapping.go`](../../internal/ws/field_mapping.go)) | **Where the car is going** -- Tesla's planned route polyline to the active destination | Yes -- member of the `navigation` atomic group | §4.1.2 |

Renaming, atomic-group reassignment, or merging of these two fields is a contract change; both names are owned by [`docs/contracts/schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) and the v1 mask matrix in [`internal/mask/tables.go`](../../internal/mask/tables.go).

**Wire shape.** Per [`state-machine.md`](state-machine.md) §4.1, `drive_updated` is **NOT a distinct wire message type**. During an active drive, the broadcaster's [`handleDriveUpdated`](../../internal/ws/route_broadcast.go) appends each GPS point to a per-VIN [`routeAccumulator`](../../internal/ws/route_accumulator.go). When the accumulator hits its batch threshold (`defaultRouteBatchSize = 5`) or its flush interval (`defaultRouteFlushInterval = 3*time.Second`), the broadcaster sends a `vehicle_update` whose `payload.fields` contains a **single key** `driveTrailCoordinates`:

```jsonc
{
  "type": "vehicle_update",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "fields": {
      "driveTrailCoordinates": [[-122.4194, 37.7749], [-122.4193, 37.7750], ...]
    },
    "timestamp": "2026-04-13T18:23:05Z"
  }
}
```

Coordinates are `[lng, lat]` order (GeoJSON / Mapbox). Each element is a per-point pair derived from [`routeCoordinate`](../../internal/ws/route_accumulator.go). The accumulator's buffer is **not cleared on flush** -- each batch contains the **complete driven path** so the SDK can render the full polyline by replacing rather than appending. The buffer is cleared only on `drive_ended` ([`broadcaster.go:handleDriveEnded`](../../internal/ws/broadcaster.go) line 162).

| Sub-field | Classification | Notes |
|-----------|----------------|-------|
| `driveTrailCoordinates[i][0]` (lng) | **P1** | Same tier as `Vehicle.longitude` |
| `driveTrailCoordinates[i][1]` (lat) | **P1** | Same tier as `Vehicle.latitude` |

**SDK requirement:** On receipt of a `vehicle_update` containing `driveTrailCoordinates` during an active drive, the SDK MUST emit `drive_updated` as a logical event to consumers AND merge the array into its in-memory drive state. Per Rule CG-SM-6 in [`state-machine.md`](state-machine.md) §7, the SDK MUST NOT synthesize `drive_updated` from any other source (no gear-change heuristics, no speed thresholds). Drive detection is server-only.

#### 4.1.7 Ungrouped fields

`speed` is delivered ungrouped even though `requirements.md` NFR-3.1 text puts it in the GPS group. This is a resolved decision in [`vehicle-state-schema.md`](vehicle-state-schema.md) §7.1: speed updates at 2 s cadence while GPS uses a 10 m delta filter, so coupling them would either delay speed updates or flood GPS updates. DV-10 records this as an accepted divergence from the NFR literal.

Other ungrouped fields: `odometerMiles`, `interiorTemp`, `exteriorTemp`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `lastUpdated`. None of these transition a `dataState` group on receipt (per [`state-machine.md`](state-machine.md) §4.3 footnote). Their freshness is implied by `connectionState`.

`lastUpdated` is set by the server on every outbound `vehicle_update` (in `nav_broadcast.go`'s `handleTelemetry` and `flushGroup`) to the event's `CreatedAt` for non-nav broadcasts or `time.Now().UTC()` for nav flushes. SDKs SHOULD surface this to consumers as the "most recent telemetry timestamp" for the vehicle.

### 4.2 `drive_started`

> **Anchored:** FR-3.1, NFR-3.1 (carries the `drive` atomic group's `startedAt` field per DV-13).
> **Drive lifecycle target:** `idle -> driving` (DR-1) or `ended -> driving` (DR-6)
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/DriveStartedPayload`](schemas/ws-messages.schema.json)
> **Fixture:** `drive_started.json` (planned)

```jsonc
{
  "type": "drive_started",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "driveId": "clmno9876543210zyxw",
    "startLocation": {
      "latitude": 37.7749,
      "longitude": -122.4194
    },
    "startedAt": "2026-04-13T18:22:00Z",
    "timestamp": "2026-04-13T18:22:00Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | **P0** | |
| `driveId` | `string` (cuid) | **P0** | Matches the eventual persisted `Drive.id` at drive completion. |
| `startLocation.latitude` | `number` | **P1** | Encrypted at rest inside `Drive.routePoints` ([`data-classification.md`](data-classification.md) §1.5); plaintext on the wire under WSS (NFR-3.22). |
| `startLocation.longitude` | `number` | **P1** | Same as above. |
| `startedAt` | `string` (ISO 8601 UTC) | **P0** | **This is the v1 home of `tripStartTime`** per DV-13. Value is `StartedAt` from the drive detector's `DriveStartedEvent`. It is repeated alongside `timestamp` (below) rather than replacing it to make the SDK binding obvious: `DriveStarted.startedAt` is the semantic drive-lifecycle timestamp; `timestamp` is the envelope-equivalent event time. SDKs MUST treat `startedAt` as the authoritative `tripStartTime` for consumers. |
| `timestamp` | `string` (ISO 8601 UTC) | **P0** | Envelope-level event time. For `drive_started`, this is always equal to `startedAt`; the two are separate fields purely to keep every `server->client` message shape uniform on `timestamp`. |

**Note on DV-13 (`tripStartTime`).** NFR-3.1 originally listed `tripStartTime` as a member of the *navigation* atomic group. There is no Tesla field for `tripStartTime` -- it is derived from the drive detector's `started_at` timestamp in [`internal/drives/`](../../internal/drives/). Semantically it belongs with the drive, not with nav: forcing it into the nav group would require a cross-subsystem join that Tesla's 500 ms bucket floor (§3.2.1) cannot deliver atomically, and a vehicle can have no nav but still have an active drive. v1 therefore relocates `tripStartTime` from the `navigation` group to the `drive` group, where it is carried as `drive_started.payload.startedAt` on the wire. Amending the NFR-3.1 literal in `requirements.md` is a follow-up tracked as **DV-13** in §10.

Source: [`internal/drives/transitions.go`](../../internal/drives/transitions.go) publishes `events.DriveStartedEvent`; [`internal/ws/broadcaster.go:handleDriveStarted`](../../internal/ws/broadcaster.go) line 99 resolves the VIN to a vehicleId and marshals the frame.

### 4.3 `drive_ended`

> **Anchored:** FR-3.1, FR-3.4 (with explicit scope note).
> **Drive lifecycle target:** `driving -> ended` (DR-3)
> **Schema:** [`schemas/ws-messages.schema.json#/$defs/DriveEndedPayload`](schemas/ws-messages.schema.json)
> **Fixture:** `drive_ended.json` (planned)

**The `drive_ended` wire payload is a SUMMARY.** It carries only the handful of fields an SDK consumer needs to render an immediate "drive finished" toast / card / complication. The full FR-3.4 drive record is retrieved via REST `GET /drives/{id}`, which is the authoritative source for every field FR-3.4 lists. See the scope note below for the full contract.

```jsonc
{
  "type": "drive_ended",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "driveId": "clmno9876543210zyxw",
    "distance": 12.4,
    "durationSeconds": 1458,
    "avgSpeed": 30.5,
    "maxSpeed": 65.2,
    "timestamp": "2026-04-13T18:46:18Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` | **P0** | |
| `driveId` | `string` | **P0** | Opaque drive cuid -- the input to `fetchDrive(driveId)` (see "SDK consumption" below). |
| `distance` | `number` (miles) | **P0** | Haversine sum from `DriveStats.Distance`. |
| `durationSeconds` | `number` (seconds) | **P0** | Drive duration in seconds, as a JSON number (`double`). Server-side value is `DriveStats.Duration.Seconds()`. **The earlier Go `time.Duration.String()` format (e.g. `"24m18s"`) is not part of the v1 wire contract** -- it was dropped before v1 ship because there are no pre-v1 consumers and the Go-native string would force every SDK to write a parser for a format it otherwise never sees. See DV-12 (RESOLVED). TypeScript consumers construct a `Date` delta or `Temporal.Duration` from this field; Swift consumers map directly to `Duration(secondsComponent:attosecondsComponent:)` or `TimeInterval`. |
| `avgSpeed` | `number` (mph) | **P0** | `DriveStats.AvgSpeed` |
| `maxSpeed` | `number` (mph) | **P0** | `DriveStats.MaxSpeed` |
| `timestamp` | `string` (ISO 8601 UTC) | **P0** | `EndedAt` from the drive detector. |

Source: [`internal/ws/broadcaster.go:handleDriveEnded`](../../internal/ws/broadcaster.go) line 140. Server emits `DriveStats.Duration.Seconds()` as `durationSeconds` (DV-12 RESOLVED by MYR-32).

**SDK consumption.** The v1 SDK surface is:

- **TypeScript**: `client.onDriveEnded(cb)` for the live summary, `await client.fetchDrive(id)` for the full FR-3.4 record, plus a `useDrive(id)` React hook that wraps the fetch + local cache.
- **Swift**: `client.onDriveEnded { summary in ... }` for the live summary, `try await client.fetchDrive(summary.driveId)` for the full FR-3.4 record.

Neither SDK auto-fetches the full record on `drive_ended`: doing so would burn cellular bandwidth on every idle consumer (especially bad for watchOS, per NFR-3.36). Consumers that need the full record call `fetchDrive` explicitly when the UI renders a detail view.

> **Scope note (FR-3.4 vs wire payload).** `requirements.md` FR-3.4 lists the full drive record as `{distance, duration, avgSpeed, maxSpeed, energyUsed, fsdMiles, interventions, startChargeLevel, endChargeLevel, startLocation+address, endLocation+address}`. The **wire** `drive_ended` payload contains only the summary fields above. The remaining FR-3.4 fields are available via REST `GET /drives/{id}` (see [`rest-api.md`](rest-api.md)) and are persisted in the `Drive` table ([`data-classification.md`](data-classification.md) §1.4). The canonical references for this split are [`state-machine.md`](state-machine.md) §3.3 (drive lifecycle -> `ended`) and [`state-machine.md`](state-machine.md) §4.1 (`drive_ended` event), plus [`data-lifecycle.md`](data-lifecycle.md) for the persistence side. This is not a divergence -- the split between "real-time fast summary" and "post-hoc full record" is intentional for NFR-3.1 latency reasons and was confirmed by both SDK agents (Option A: summary on wire + explicit `fetchDrive(driveId)` helper). Tracked as **DV-11** (RESOLVED) in §10 for audit-trail continuity.

> **Micro-drive filter:** Drives that fail the micro-drive filter (default 2 minutes / 0.1 miles, [`state-machine.md`](state-machine.md) §3.5) NEVER produce a `drive_ended` frame. The SDK relies on `WS_DISCONNECTED` (DR-4) or an extended absence of route updates as the only signal that an in-progress drive was suppressed. This is documented behavior, not a divergence.

### 4.4 `connectivity`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/ConnectivityPayload`](schemas/ws-messages.schema.json)
> **Fixtures:** `connectivity.online.json`, `connectivity.offline.json` (planned)

```jsonc
{
  "type": "connectivity",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "online": true,
    "timestamp": "2026-04-13T18:22:00Z"
  }
}
```

| Field | Type | Classification |
|-------|------|----------------|
| `vehicleId` | `string` | **P0** |
| `online` | `boolean` | **P0** |
| `timestamp` | `string` (ISO 8601 UTC) | **P0** |

**Important distinction:** `connectivity.online` reports the **vehicle<->server** (Tesla mTLS) connection status, NOT the **SDK client<->server** (WebSocket) status. The latter is implicit in the WebSocket connection itself -- an absent connection IS the disconnected state.

Per [`state-machine.md`](state-machine.md) §4.2, `connectivity` does NOT directly transition `connectionState` -- the SDK already knows its WebSocket is open, since it just received a frame on it. The signal is informational: the UI may show "Vehicle offline" while continuing to display cached data. When the server emits `connectivity.online: false`, the broadcaster also clears any pending nav accumulator state for that VIN to prevent stale nav data on reconnect ([`broadcaster.go:handleConnectivity`](../../internal/ws/broadcaster.go) line 227).

### 4.5 Per-vehicle ownership filtering

> **Anchored:** NFR-3.21.

`Hub.Broadcast(vehicleID, msg)` ([`internal/ws/hub.go`](../../internal/ws/hub.go) line 70) iterates every connected client and calls `client.hasVehicle(vehicleID)`. Only clients whose `vehicleIDs` slice (populated at handshake time from `Authenticator.GetUserVehicles`) contains the target vehicle ID receive the frame. The dev-mode `NoopAuthenticator` returns the `WildcardVehicleID` sentinel ([`auth.go`](../../internal/ws/auth.go)) from `GetUserVehicles`; the handshake translates that into `Client.allVehicles = true` and `hasVehicle` short-circuits to `true` for those clients. Production `Authenticator` implementations MUST NOT emit the sentinel, so `allVehicles` is always `false` on production: an empty `vehicleIDs` slice means deny-all per NFR-3.21.

The SDK can rely on the following contract: **an authenticated production client will NEVER receive a `vehicle_update`, `drive_started`, `drive_ended`, or `connectivity` frame for a vehicle it does not own at handshake time.**

> **Divergence (DV-09 — reduced):** Vehicle-deletion ownership changes are now propagated mid-connection (MYR-73, 2026-05-09): a Postgres `vehicle_deleted` LISTEN/NOTIFY trigger drives a hub-side `RemoveVehicle` call that closes affected clients with code 4002 within seconds of the Next.js commit. **Role mutations** (e.g., promoting a viewer to owner via the Invite flow) are still stale until the client reconnects — that path needs an Invite-state notify mechanism that does not exist today. See §10 DV-09 for the remaining open scope.

`heartbeat` frames are broadcast to ALL clients regardless of vehicle ownership via [`Hub.BroadcastAll`](../../internal/ws/hub.go) line 90 -- they carry no vehicle-scoped data.

### 4.6 Per-role projection at broadcast (NFR-3.19)

> **Anchored:** NFR-3.19, NFR-3.20, FR-5.4. Per-resource mask matrix is canonical in [`rest-api.md`](rest-api.md) §5.2; the same matrix governs both transports.

NFR-3.19 requires every WebSocket broadcast to be projected through the recipient's role mask before sending. To satisfy this without per-client marshaling cost, the hub MUST pre-marshal **once per role per frame**, then fan out the role-appropriate bytes to each subscribed client based on that client's per-vehicle role:

```
EventBus -> Hub.Broadcast(vehicleId, plaintext)
  framesByRole := { for each role in v1Roles:
      role -> marshal(mask.Apply(plaintext, mask.For(resourceType, role)))
  }
  for each client subscribed to vehicleId:
      role := client.vehicleRoles[vehicleId]   // resolved at handshake (§2.2)
      client.enqueue(framesByRole[role])
```

Marshal cost is `O(|roles|)` per frame, fan-out is `O(|clients|)`. With v1's two-role matrix (`owner`, `viewer`) this is essentially free; FR-5.5's future `limited_viewer` makes it `O(3)`.

The mask matrix is the **same matrix used by the REST handler layer** (`rest-api.md` §5.2) — a single source-of-truth per resource type. The Go implementation lives in `internal/mask/` and is consumed by both the WS hub and the REST handlers (`rest-api.md` §5.1 handler-layer rule). This keeps owner/viewer behavior identical across transports and gives `contract-guard` a single set of mask tables to validate against the schema.

`Client.vehicleRoles map[VehicleID]Role` is populated at handshake time alongside `Client.vehicleIDs`. The `Authenticator.ResolveRole(ctx, userId, vehicleId)` interface method returns the role for each owned vehicle. Like `vehicleIDs`, `vehicleRoles` is a snapshot — see DV-09 below for the mid-connection refresh gap, which extends to role downgrade (e.g., a viewer being upgraded to owner mid-connection does not reflect until reconnection).

**Empty-payload suppression.** If a role's mask projection yields a payload with no remaining fields (every field in the original frame was masked away), the hub MUST omit the frame for that role rather than send an empty `vehicle_update`. Sending an empty broadcast would leak "something happened on this vehicle" to a viewer who shouldn't even know the field existed.

---

## 5. Client -> server message catalog

> **Anchored:** FR-6.1, FR-6.2, NFR-3.21.

### Catalog summary

| `type` | Status | Implementation | Notes |
|--------|--------|----------------|-------|
| `auth` | **Implemented** | [`handler.go:authenticateClient`](../../internal/ws/handler.go) | The ONLY client->server frame the server reads today. |
| `subscribe` | **Implemented** ([MYR-46](https://linear.app/myrobotaxi/issue/MYR-46)) | [`control_frames.go:handleSubscribeFrame`](../../internal/ws/control_frames.go) | Adds the vehicle to the active subscription set after an ownership check. Non-owned target → typed `vehicle_not_owned` error + close 4002. `sinceSeq` parsed but ignored (DV-02). |
| `unsubscribe` | **Implemented** ([MYR-46](https://linear.app/myrobotaxi/issue/MYR-46)) | [`control_frames.go:handleUnsubscribeFrame`](../../internal/ws/control_frames.go) | Removes the vehicle from the active subscription set without closing the socket. Idempotent. |
| `ping` | **Implemented** ([MYR-46](https://linear.app/myrobotaxi/issue/MYR-46)) | [`control_frames.go:handlePingFrame`](../../internal/ws/control_frames.go) | Server responds with `pong` echoing the nonce. RFC 6455 transport-level PING/PONG continues to be handled transparently by `coder/websocket`. |

As of MYR-46 the server's `readPump` ([`client.go:readPump`](../../internal/ws/client.go)) dispatches `subscribe`, `unsubscribe`, and `ping` frames via [`control_frames.go:handleClientFrame`](../../internal/ws/control_frames.go). Unknown frame types are logged-and-ignored so a future SDK introducing a new client→server frame does not poison an otherwise-healthy connection.

### 5.1 `auth`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/AuthPayload`](schemas/ws-messages.schema.json)
> **Fixture:** `auth.json` (planned)

```jsonc
{
  "type": "auth",
  "payload": {
    "token": "<opaque session token>"
  }
}
```

| Field | Classification | Notes |
|-------|----------------|-------|
| `token` | **P1** | Never log. See [`data-classification.md`](data-classification.md) §2.2 and the `AuthPayload.token` row in the JSON Schema (`x-classification: P1`). |

See §2.2 for full handshake semantics.

### 5.2 `subscribe`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/SubscribePayload`](schemas/ws-messages.schema.json)
> **Implementation:** [`control_frames.go:handleSubscribeFrame`](../../internal/ws/control_frames.go)

Implemented by [MYR-46](https://linear.app/myrobotaxi/issue/MYR-46) (DV-07 RESOLVED minus `sinceSeq`). At handshake time, the server seeds the active subscription set from `Authenticator.GetUserVehicles(userID)` so a client that never sends `subscribe`/`unsubscribe` keeps receiving every owned vehicle (backwards-compatible with pre-MYR-46 SDK consumers). After handshake, an explicit `subscribe` adds a vehicle to the active set after an ownership check; a non-owned target yields a typed `vehicle_not_owned` error frame plus close code 4002 (§6.1.1, §6.2). The `sinceSeq` field is parsed but ignored — the snapshot-resume path depends on DV-02 (envelope `seq`) landing first.

```jsonc
{
  "type": "subscribe",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef",
    "sinceSeq": 4271
  }
}
```

When DV-02 lands, the server response to a `subscribe` carrying `sinceSeq` will be either:

- A normal `vehicle_update` stream starting at `sinceSeq + 1`, OR
- An `error` frame with `code: snapshot_required` indicating the client must perform a full REST snapshot fetch (NFR-3.11) and reconnect.

Today the server ignores `sinceSeq` and continues the existing live stream from the current state.

### 5.3 `unsubscribe`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/UnsubscribePayload`](schemas/ws-messages.schema.json)
> **Implementation:** [`control_frames.go:handleUnsubscribeFrame`](../../internal/ws/control_frames.go)

Implemented by [MYR-46](https://linear.app/myrobotaxi/issue/MYR-46). Removes the vehicle from the per-client active subscription set without closing the underlying WebSocket. Idempotent: removing an already-absent vehicleId is a no-op. Does NOT require ownership — a subscribed-but-since-revoked vehicle is still removable so the client can drain the set on logout.

```jsonc
{
  "type": "unsubscribe",
  "payload": {
    "vehicleId": "clxyz1234567890abcdef"
  }
}
```

### 5.4 `ping`

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/PingPayload`](schemas/ws-messages.schema.json)
> **Implementation:** [`control_frames.go:handlePingFrame`](../../internal/ws/control_frames.go)

Implemented by [MYR-46](https://linear.app/myrobotaxi/issue/MYR-46). The server responds with a `pong` frame echoing the nonce so the client can compute round-trip latency. Application-level `ping` is reserved for platforms where the WebSocket library does not expose RFC 6455 PING/PONG — specifically watchOS extended-runtime sessions and iOS background sockets per NFR-3.36 / NFR-3.36a-d. Browser/Node clients still rely on transport-level RFC 6455 PING/PONG (handled transparently by [`coder/websocket`](https://github.com/coder/websocket)) and the server's outbound `heartbeat` frames (§7.4) for liveness.

```jsonc
{
  "type": "ping",
  "payload": {
    "nonce": "<opaque round-trip ID>"
  }
}
```

The server response is a `pong` frame echoing the nonce. `pong` is canonical in [`schemas/ws-messages.schema.json#/$defs/PongPayload`](schemas/ws-messages.schema.json); the server-side write path is `control_frames.go:writePong`.

---

## 6. Errors and close codes

> **Anchored:** FR-7.1, FR-7.3, NFR-3.10, NFR-3.21.

### 6.1 Error frame

> **Schema:** [`schemas/ws-messages.schema.json#/$defs/ErrorPayload`](schemas/ws-messages.schema.json)
> **Fixtures:** `error.auth_failed.json`, `error.auth_timeout.json` (planned)

```jsonc
{
  "type": "error",
  "payload": {
    "code": "auth_failed",
    "message": "invalid token"
  }
}
```

| Field | Type | Classification |
|-------|------|----------------|
| `code` | `string` (enum) | **P0** |
| `message` | `string` | **P0** (MUST NOT contain P1 values; see §6.3 + [`data-classification.md`](data-classification.md) §2.2) |

Per FR-7.1, consumer SDKs MUST map `code` to typed error values and branch on the typed value, NEVER on the human-readable `message`. The `message` is intended for logs and developer tooling only.

#### 6.1.1 Error code catalog

| Code | Today | Direction | Reconnect policy | Description |
|------|-------|-----------|------------------|-------------|
| `auth_failed` | **Implemented** ([`wserrors.ErrCodeAuthFailed`](../../internal/wserrors/wserrors.go)) | server->client (handshake) | Surface to UI; **do not auto-retry**. Consumer must re-auth. | Token signature/issuer/audience/expiry check failed, or `GetUserVehicles` failed. |
| `auth_timeout` | **Implemented** ([`wserrors.ErrCodeAuthTimeout`](../../internal/wserrors/wserrors.go)) | server->client (handshake) | **Auto-retry with backoff** (NFR-3.10) | Client did not send the auth frame within `HandlerConfig.AuthTimeout` (default 5 s). Treated as transient. |
| `permission_denied` | **PLANNED** — generic 403 surface; the vehicle-specific case is now `vehicle_not_owned` (MYR-46). | server->client | Surface to UI; do not auto-retry the same vehicle | Reserved for future non-vehicle-scoped permission failures (e.g., role downgrade mid-session). |
| `vehicle_not_owned` | **Implemented** ([MYR-46](https://linear.app/myrobotaxi/issue/MYR-46)) — emitted on a `subscribe` to a vehicle outside the caller's ownership set, paired with close code 4002. | server->client | Surface to UI; do not auto-retry the same vehicleId | The specific case of `permission_denied` for an explicit `subscribe` (§5.2). |
| `rate_limited` | **Implemented for pre-auth per-IP cap** ([`handler.go`](../../internal/ws/handler.go) emits the typed REST envelope on HTTP 429 — MYR-47); **PLANNED** for the post-auth per-user breach (DV-08) | server->client (per-user breach, paired with close 4003) OR HTTP 429 on upgrade (per-IP breach) | Auto-retry with **extended** backoff -- see pseudocode below. | Caller-facing signal: a concurrent-connection cap was breached. Per-user breach (post-auth, close 4003) and per-IP breach (pre-auth, HTTP 429) are both surfaced to the caller as the same typed error `rate_limited`; the SDK may inspect the underlying transport status for diagnostic logging but MUST NOT branch consumer-visible behavior on it. See §1.3 for the cap defaults, enforcement points, and the `rate_limited.device_cap` sub-code for the per-user breach. |
| `internal_error` | **PLANNED** | server->client | Auto-retry with backoff | Catch-all for unexpected server failures during a live session. |
| `snapshot_required` | **PLANNED** (DV-02) | server->client | Run the reconnect sequence (§7.2): re-fetch REST snapshot and restart live stream. | Server cannot satisfy the client's `subscribe.sinceSeq` request because the gap is too large. Requires DV-02 (`seq`) to ship. |

The PLANNED codes are reserved in the AsyncAPI spec and JSON Schemas today so SDKs can match against them once the server emits them. The schema enum is the canonical list -- when a new code is added, the enum, this table, and the contract-guard rules MUST all be updated in the same PR.

##### `rate_limited` reconnect pseudocode

```
// On receipt of error.code == "rate_limited" (or HTTP 429 on upgrade),
// the SDK MUST use this backoff curve instead of the §7.1 default.
onRateLimited(source, subCode):
    // source ∈ {per_user_close_4003, per_ip_http_429}
    // subCode ∈ {device_cap, null}
    if subCode == "device_cap":
        // Terminal-ish: device 6+ on a 5-cap. Do NOT retry automatically;
        // surface a typed UI signal so the user can sign out another device.
        emit UI event "device_cap_reached" { userVisible: true }
        return DO_NOT_RETRY

    // All other rate_limited variants: extended exponential backoff.
    attempt       += 1
    baseDelay     := §7.1 curve[attempt]           // e.g. 1s, 2s, 4s, 8s, ...
    minDelay      := max(baseDelay, 2 * §7.1 curve[1])  // skip the 1s slot
    elapsedSince  := now - firstRateLimitedAt

    if source == per_user_close_4003 AND elapsedSince < 60s:
        // First minute after a per-user cap breach: pin to the §7.1 max delay
        // to avoid pounding the cap while another device releases its slot.
        delay := §7.1 maxDelay
    else:
        delay := minDelay + jitter(±20%)

    scheduleReconnect(delay)
```

> **`rate_limited.device_cap` sub-code.** When the server emits `error.code == "rate_limited"` specifically because the caller breached the **per-user** cap (§1.3, default 5 concurrent connections per user), the error frame MUST carry an additional `subCode: "device_cap"` field. SDKs MUST surface this as a typed UI signal (e.g., `DeviceCapReachedError`) distinct from generic rate-limiting, so consumer apps can render an actionable message ("Too many devices signed in -- sign out of another device to continue") instead of a misleading "Network busy, retrying..." toast. Per-IP breaches (HTTP 429 on upgrade) do NOT carry a sub-code because the SDK cannot distinguish a NAT-mate flood from user intent. This is the single deviation from the "SDK MUST NOT branch consumer-visible behavior on cap source" rule: a `device_cap` sub-code is an explicit opt-in for per-user clarity.

### 6.2 WebSocket close codes

> **Anchored:** RFC 6455 §7.4. Application-specific codes use the 4000-4999 range.

The Go server uses [`coder/websocket`](https://github.com/coder/websocket) status constants. Today the server explicitly closes the socket with the following codes:

| Code | Name | Today | Source (Go) | When | SDK reconnect policy |
|------|------|-------|-------------|------|----------------------|
| `1001` | Going Away | **Implemented** | [`client.go:writePump`](../../internal/ws/client.go) line 56 (`websocket.StatusGoingAway`) | Hub closed the client's send channel (server shutdown) | Auto-reconnect with backoff (C-6 + D-4) |
| `1008` | Policy Violation | **Implemented** | [`handler.go:handleUpgrade`](../../internal/ws/handler.go) line 93 (`websocket.StatusPolicyViolation`) | Authentication failed (sent immediately after the `error` frame) | Branches on the preceding `error.code` -- see below |
| `1000` | Normal Closure | Tolerated | n/a | Client closed the socket cleanly | n/a (client-initiated) |

> **Divergence (DV-06):** Close code 1008 is emitted for BOTH `auth_failed` (terminal, do-not-retry) and `auth_timeout` (transient, auto-retry). The SDK has to disambiguate by reading the preceding `error.code`, which is fragile if the error frame fails to arrive. Target: map `auth_timeout` to its own close code (proposal: 4001 "Auth Token Expired" or a dedicated 40xx value once DV-06 is resolved).

In addition, RFC 6455 reserves codes 4000-4999 for **application-specific** usage. The following application-specific codes are **PLANNED** (reserved by this contract for future server emission). SDKs SHOULD recognize them but MUST NOT panic on receipt of any 4xxx code they don't know.

| Code | Name | When | SDK reconnect policy |
|------|------|------|----------------------|
| `4001` | Auth Token Expired | Server detected mid-session token expiry (e.g., JWT `exp` passed) OR client needs to refresh before reconnect (DV-06 target) | Refresh token via `getToken()`, reconnect |
| `4002` | Permission Revoked | Vehicle ownership revoked while connected (e.g., invite removed) | Surface to UI; do not auto-retry the same vehicle (DV-09) |
| `4003` | Server Overload | Per-vehicle or per-user backpressure cap exceeded | Auto-reconnect with extended backoff |
| `4004` | Protocol Violation | Client sent a malformed frame or violated the atomic-group contract | Surface to UI as a bug; do not auto-retry |
| `4005` | Snapshot Required | Server cannot satisfy the requested `subscribe.sinceSeq` (gap too large) -- client must re-fetch the REST snapshot (paired with `error.code = snapshot_required`) | Run the standard reconnect sequence (§7.2) |

The mapping between `error.payload.code` and the close code, when both are emitted:

| `error.code` | Following close code today | Target close code |
|--------------|---------------------------|-------------------|
| `auth_failed` | `1008` Policy Violation | `1008` (no change) |
| `auth_timeout` | `1008` Policy Violation | `4001` Auth Token Expired (DV-06) |
| `permission_denied` | n/a today | `4002` Permission Revoked (DV-07) |
| `vehicle_not_owned` | n/a today | `4002` Permission Revoked (DV-07) |
| `rate_limited` | HTTP 429 on upgrade (DV-08) | `4003` Server Overload (DV-07/DV-08) |
| `snapshot_required` | n/a today | `4005` Snapshot Required (DV-02) |
| `internal_error` | n/a today | `1011` Internal Error |

### 6.3 No P1 in error messages

Per [`data-classification.md`](data-classification.md) §2.2 and Rule CG-DC-2: the `error.payload.message` field is **P0**, but error message construction sites MUST NOT include P1 values (no GPS, no addresses, no tokens, no email, no full VINs). Use opaque IDs (vehicleId, driveId, userId) for correlation, or `redactVIN()` for VINs that absolutely must appear.

The current implementation in [`handler.go:sendError`](../../internal/ws/handler.go) emits only static strings (`"invalid token"`, `"failed to load vehicles"`) and is therefore compliant. Future error codes MUST preserve this property; contract-guard Rule CG-DC-2 blocks PRs that introduce P1 values into error construction sites.

---

## 7. Heartbeat, reconnect, and snapshot resume

> **Anchored:** NFR-3.10, NFR-3.11, NFR-3.12, NFR-3.13, FR-8.1.

### 7.1 Reconnect backoff parameters (NFR-3.10)

These are the canonical values from [`state-machine.md`](state-machine.md) §1.4 and MUST be implemented identically in both SDKs:

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Initial delay | 1 second | First reconnect attempt after disconnect |
| Backoff multiplier | 2x | Each subsequent attempt doubles the delay |
| Maximum delay | 30 seconds | Cap regardless of attempt count |
| Jitter | ± 25% of computed delay | Prevents thundering herd at the 5K concurrent client target (NFR-3.6) |
| Maximum retries | Unlimited (default) | SDK retries indefinitely unless `USER_STOPPED` or consumer configures a limit |

```
delay          = min(initialDelay * 2^(attempt - 1), maxDelay)
jitter         = delay * random(-0.25, +0.25)
effectiveDelay = delay + jitter
```

### 7.2 Reconnect sequence

The full sequence diagram is in [`state-machine.md`](state-machine.md) §5. The protocol-relevant invariants:

1. **Snapshot before stream (NFR-3.11).** On reconnect, the SDK MUST re-fetch the REST snapshot ([`rest-api.md`](rest-api.md) `GET /vehicles/{id}/snapshot`) BEFORE processing any new WebSocket frames. The snapshot is the cold-load source of truth; the WebSocket stream resumes from the consistent baseline.
2. **All groups -> loading.** When the SDK begins the reconnect, every `dataState` group transitions to `loading` (state-machine D-7). Cached values remain visible per NFR-3.12 / NFR-3.13.
3. **No forced reload (NFR-3.12, NFR-3.13).** The reconnect is entirely SDK-internal. The UI is never asked to refresh. Cached state remains visible indefinitely.
4. **Ordering guarantee.** Live frames received during the snapshot fetch are queued and applied AFTER the snapshot, NEVER before (CG-SM-4).
5. **Idempotent.** Multiple rapid disconnect/reconnect cycles MUST NOT cause duplicate snapshot fetches. The SDK cancels any in-flight fetch when a new reconnect begins.

The reconnect handshake replays §2 verbatim: open WSS, send `auth` frame, await live frames.

### 7.3 Snapshot-resume semantics (NFR-3.11)

NFR-3.11 says: "On reconnect, SDK MUST re-fetch the DB snapshot and resume live stream without user intervention."

Two valid implementations, the contract supports both:

1. **REST-snapshot resume (current v1.0 implementation).** Reconnect always fetches the full REST snapshot. No wire-level sequence numbers. Trade-off: extra HTTP round-trip on every reconnect, gaps within a single connection are invisible.
2. **Sequence-resume (PLANNED, v1.x, DV-02).** When the server emits envelope `seq`, the client passes its highest-seen `seq` as `subscribe.sinceSeq` (§5.2). The server replays missed frames OR responds with `error.code: snapshot_required` to fall back to mode 1. Trade-off: requires server-side per-connection retention of recent frames.

The SDK contract today is mode 1. The wire surface for mode 2 is reserved so v1.x can ship without a breaking change.

### 7.4 Heartbeat / keepalive

> **Anchored:** NFR-3.10 (reconnect cadence constraint).

| Direction | Cadence | Wire form | Source (Go) |
|-----------|---------|-----------|-------------|
| Server -> client | Default 15 seconds (configurable via `WebSocketConfig.HeartbeatInterval`; validated `> 0` in `config/validate.go` line 104) | Bare envelope `{"type":"heartbeat"}` (no `payload` key -- `omitempty`) | [`heartbeat.go:RunHeartbeat`](../../internal/ws/heartbeat.go) |
| Client -> server | On-demand `ping` (no fixed cadence; reserved for watchOS extended-runtime and iOS background sessions per NFR-3.36, MYR-46) | `{"type":"ping","payload":{"nonce":"…"}}` → server `pong` echo | [`control_frames.go:handlePingFrame`](../../internal/ws/control_frames.go) |
| Transport-level (RFC 6455 PING/PONG) | Handled transparently by `coder/websocket` library | Binary control frames | Library internals |

The server pre-marshals the heartbeat message once at init (`heartbeatMessage = mustMarshal(wsMessage{Type: msgTypeHeartbeat})`) and broadcasts it via [`Hub.BroadcastAll`](../../internal/ws/hub.go) line 90 to ALL connected clients regardless of vehicle ownership.

#### 7.4.1 SDK liveness watchdog

The SDK uses the heartbeat as a positive liveness signal:

- Reset a watchdog timer on EVERY received frame (heartbeat, vehicle_update, anything).
- If the watchdog fires (no frame for `2 * heartbeatInterval`, default 30 s), the SDK treats it as a silent disconnect and triggers `WS_CLOSED` -> `connecting` (C-6 -> C-9).

Per Rule CG-SM-1 ([`state-machine.md`](state-machine.md) §7), the watchdog MUST NOT be used to mark `dataState` as `stale`. `dataState` transitions to `stale` only when the WebSocket actually closes (NFR-3.7, NFR-3.8b).

**Two-watchdog model.** The liveness watchdog described above is the **post-`auth_ok`** mechanism: it is armed on receipt of `auth_ok` and reset on every subsequent frame. Before `auth_ok` arrives, a separate **pre-`auth_ok` timer** is in effect (§2.3 rule 4) — a 6-second bound starting when the SDK hands the `auth` frame to the socket. The two timers never overlap: the pre-`auth_ok` timer is cancelled the moment `auth_ok` arrives or C-3 fires, whichever happens first; the liveness watchdog is armed at that same moment. Any "no frame" window between `connecting` and `connected` is covered by the pre-`auth_ok` timer; every "no frame" window after `connected` is covered by the liveness watchdog. Together they bound the end-user "Connecting..." banner to at most 6 s on degraded paths and bound silent data-plane stalls to at most 30 s.

#### 7.4.2 SDK MUST NOT use heartbeat for freshness

Per NFR-3.7, freshness is event-driven and not time-based. The SDK MUST NOT:

- Mark fields stale because no `vehicle_update` for that field arrived in the last N heartbeats.
- Use heartbeat cadence to derive any `dataState` transition.

The only legitimate uses of heartbeat in the SDK are: (a) reset the liveness watchdog, (b) update an internal "last frame received" timestamp for debug telemetry.

### 7.5 Apple platform suspend/resume (Swift SDK only)

> **Anchored:** NFR-3.36, NFR-3.36a-d. Detailed bindings in [`swift-lifecycle.md`](swift-lifecycle.md).

When the iOS / iPadOS / watchOS / visionOS / macOS process is suspended by the OS, `URLSessionWebSocketTask` does not deliver a close frame; the socket falls silent. The Swift SDK MUST detect liveness via the heartbeat-watchdog timeout in §7.4.1 and transition `connected -> disconnected` with a typed reason of `heartbeat_timeout` -- NOT `transport_close`. This is the only path by which a suspended-then-resumed app reaches `disconnected`.

The SDK is UI-framework-agnostic per NFR-3.35 and MUST NOT observe scene transitions itself. Consumers forward foreground transitions by calling `MyRoboTaxiClient.handleForegroundTransition()` from their app's lifecycle observer (SwiftUI `@Environment(\.scenePhase)`, UIKit `UIScene.willEnterForegroundNotification`, WatchKit `WKApplicationDidBecomeActiveNotification`, or AppKit `NSApplication.didBecomeActiveNotification`). On receiving that call the SDK MUST execute the §7.2 reconnect sequence with a **first-attempt backoff bypass** (NFR-3.36a): reset retry counter to 0 and immediately attempt reconnect; if it fails, normal §7.1 backoff resumes from attempt 1.

Per-platform consumer wiring (notifications → SDK method) is enumerated in [`swift-lifecycle.md`](swift-lifecycle.md) §2 and §3.2.

For background-driven snapshot refresh (no foreground transition required), the Swift SDK MUST expose `performBackgroundSnapshotRefresh()` and `performBackgroundDriveRoutePrefetch(maxDrives:)` async methods per NFR-3.36b. Consumers register the platform-specific background-task identifiers (`BGAppRefreshTask` / `BGProcessingTask` on iOS / iPadOS / macOS / visionOS; `WKApplicationRefreshBackgroundTask` on watchOS) themselves and call those SDK methods from inside the registered handler. The SDK observes `Task.checkCancellation()` so consumer-supplied expiration handlers can cancel in-flight work cleanly.

Browser and Node consumers (TypeScript SDK) have no analogous lifecycle: `document.visibilitychange` is consumer-controlled and explicitly OUT of v1 SDK scope, and the Node event loop never suspends mid-task. This section binds Apple platforms only.

---

## 8. Cross-references

| Topic | Document |
|-------|----------|
| Atomic group definitions and consistency predicates | [`vehicle-state-schema.md`](vehicle-state-schema.md) §2, §3 |
| Per-field types, units, classification, schemas | [`vehicle-state-schema.md`](vehicle-state-schema.md) §1 + [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) |
| Data classification (P0/P1/P2) | [`data-classification.md`](data-classification.md) |
| Data lifecycle (DB vs WebSocket source-of-truth, retention) | [`data-lifecycle.md`](data-lifecycle.md) |
| connectionState / dataState / drive lifecycle state machines | [`state-machine.md`](state-machine.md) |
| Reconnect sequence diagram | [`state-machine.md`](state-machine.md) §5 |
| REST snapshot endpoint | [`rest-api.md`](rest-api.md) |
| AsyncAPI 3.0 spec (machine-readable) | [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) |
| Envelope JSON Schema | [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json) |
| Per-message JSON Schemas | [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) |
| Fixture index (planned) | [`fixtures/README.md`](fixtures/README.md) |
| Server implementation entry points | [`internal/ws/handler.go`](../../internal/ws/handler.go), [`internal/ws/broadcaster.go`](../../internal/ws/broadcaster.go), [`internal/ws/nav_broadcast.go`](../../internal/ws/nav_broadcast.go), [`internal/ws/route_broadcast.go`](../../internal/ws/route_broadcast.go), [`internal/ws/atomic_groups.go`](../../internal/ws/atomic_groups.go), [`internal/ws/group_accumulator.go`](../../internal/ws/group_accumulator.go), [`internal/ws/route_accumulator.go`](../../internal/ws/route_accumulator.go), [`internal/ws/heartbeat.go`](../../internal/ws/heartbeat.go), [`internal/ws/field_mapping.go`](../../internal/ws/field_mapping.go) |
| Functional / non-functional requirements | [`docs/architecture/requirements.md`](../architecture/requirements.md) |

---

## 9. Type generation targets

### 9.1 TypeScript (via `@myrobotaxi/contracts`)

WebSocket message types are generated alongside `VehicleState` by the [contracts repo](https://github.com/myrobotaxi/contracts) — see [`vehicle-state-schema.md` §6.1](./vehicle-state-schema.md#61-typescript-via-myrobotaxicontracts) for the toolchain and codegen mechanics. Consumers get the discriminated envelope and all eight `$defs` message payload types via:

```ts
import type { WebSocketEnvelope, VehicleUpdatePayload, ErrorPayload } from '@myrobotaxi/contracts/types';
```

The previously-planned in-repo `gen-ts-ws-types` Makefile target is OBSOLETE; the contracts repo's `scripts/codegen.mjs` consumes `ws-messages.schema.json` + `ws-envelope.schema.json` directly. Schema-touching PRs in this repo must pair with a contracts-repo PR per the paired-PR convention until MYR-95 collapses schema authoring into the contracts repo.

### 9.2 Swift (AsyncAPI -> Codable structs)

Per NFR-3.34, the Swift SDK uses `Codable`/`Sendable` structs. A code generator (PLANNED) will produce one struct per `$defs` entry in [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) plus the envelope from [`schemas/ws-envelope.schema.json`](schemas/ws-envelope.schema.json). The discriminator is implemented as an enum-with-associated-values per Swift idiom.

---

## 10. Code ↔ spec divergences

This section is the canonical catalogue of every known gap between this contract and the current `internal/ws/` implementation (or between this contract and `requirements.md`). Every entry has a Linear follow-up title; contract-tester treats any un-catalogued divergence as a failing contract violation. The divergence IDs (DV-NN) are stable -- new divergences take the next free number; closed divergences retain their ID in the change log.

### Status legend

Read this legend before scanning the catalogue. A row's **Status** column classifies the action it requires:

| Status | Meaning | Blocks merge of this PR? | Action |
|--------|---------|--------------------------|--------|
| **RESOLVED** | Contract frozen AND implementation matches. | No | Audit-trail only; no further action. |
| **RESOLVED (target documented; wiring still pending)** | Contract frozen; server/SDK wiring in flight. | No | Follow-up implementation issue referenced in the row. |
| **Requirement amendment pending** | Contract text is correct but `requirements.md` literal disagrees. | No | Separate `requirements.md` amendment PR -- this contract ships ahead of that PR by design (§10 rule 4). |
| **Open** | Known gap; neither contract nor implementation has landed the target. | No | Follow-up implementation issue referenced in the row. |
| **Open (reduced)** | A sub-slice of a larger `Open` divergence has been pulled out and resolved; the remainder is still open. | No | Same as `Open`; see the row for scope. |
| **New** | Divergence added in the same PR that introduces this section's row. | No | Same as `Open`. |

`contract-guard` treats any undocumented divergence as a failing contract violation -- this legend exists so a reader can tell at a glance which rows are informational (audit trail) and which are actionable (follow-ups).

### Catalogue

| ID | Status | Topic | Current behavior | Target behavior | Anchor | Proposed Linear issue title |
|----|--------|-------|------------------|-----------------|--------|------------------------------|
| **DV-01** | **Requirement amendment pending** | Nav debounce 200 ms (NFR-3.2 literal) vs 500 ms (implementation + Tesla floor) | `defaultGroupFlushInterval = 500 * time.Millisecond` ([`group_accumulator.go`](../../internal/ws/group_accumulator.go)). Tesla emits field batches in **500 ms buckets on the vehicle side**, and Fleet API `interval_seconds` has a **1-second minimum** -- sub-second emission cannot even be requested. MYR-14 (2026-04-29) re-confirmed this divergence after deciding the 500 ms window for the same reason; NFR-3.2 amendment to `requirements.md` remains pending. | **Amend NFR-3.2 in [`docs/architecture/requirements.md`](../architecture/requirements.md) from 200 ms to 500 ms.** This is a **requirement-drift divergence**, not an implementation-drift divergence: the server is correct and the requirement was authored without knowledge of Tesla's 500 ms batch floor. See §3.2.1 for the full justification. MYR-11 ships with DV-01 still marked "amendment pending"; a separate NFR-3.2 amendment PR is required to fully close DV-01. | NFR-3.2, §3.2.1 | `MYR-XX Amend NFR-3.2: nav debounce window 200 ms -> 500 ms (Tesla 500 ms bucket floor)` |
| **DV-02** | Open | Envelope `seq` + `ts` not emitted | Server has no per-connection sequence counter; no envelope-level timestamp. Payload-internal `timestamp` is the only time source. | Server adds a `nextSeq` counter to `Client`, increments on every `Hub.Broadcast`/`BroadcastAll`, and includes it plus `ts` (server `time.Now().UTC()`) in every envelope. SDK tolerates absence today; consumes once emitted. | NFR-3.11, §3.1, §3.3 | `MYR-XX Add monotonic per-connection seq + envelope ts to WebSocket frames (NFR-3.11)` |
| **DV-03** | **RESOLVED** | `chargeState` in charge group | Live WS wiring landed in [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) (2026-04-22); source proto corrected from 2 to 179 by [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42) (2026-04-23) — see DV-19 for the empirical capture. `FleetFieldDetailedChargeState` is in `DefaultFieldConfig` at 30s; `tpb.Field_DetailedChargeState → FieldChargeState` in `fieldMap`; `convertChargeState` handles `Value_DetailedChargeStateValue` primary + `Value_ChargingValue` pre-2024.44.25 fallback. Wire field passes through `internal/ws/field_mapping.go` unchanged (internal == wire name). REST `/snapshot` DB persistence landed in [MYR-41](https://linear.app/myrobotaxi/issue/MYR-41) (2026-04-25): cross-repo Prisma migration adds `chargeState String?` to `Vehicle`, Go writer applier persists every event, `vehicle_repo.go` SELECT/UPDATE round-trips the column. | **v1 ships `chargeState` as a member of the `charge` atomic group** sourced from proto 179 `DetailedChargeState` (enum: `Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`). DB persistence path is now closed end-to-end. See §4.1.4 for the full wire contract. | NFR-3.1, §4.1.4 | [`MYR-41`](https://linear.app/myrobotaxi/issue/MYR-41) — Persist chargeState + timeToFull to Prisma Vehicle table |
| **DV-04** | **RESOLVED** | `timeToFull` in charge group | Live WS wiring landed in [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) (2026-04-22): `FleetFieldTimeToFullCharge` added to `DefaultFieldConfig` at 30s, `Field_TimeToFullCharge` is in `fieldMap` with internal name `timeToFull`, `convertNumericOrString` handles the `DoubleValue` hours passthrough. The empirical unit verification DV-17 is closed (see the [MYR-25 comment](https://linear.app/myrobotaxi/issue/MYR-25#comment-4f1dcee9-ab10-4039-acc5-9e7ef25c3762) — 1.0667h observed at chargeLevel 68 during home L2 charging on 2026-04-22). REST `/snapshot` DB persistence landed in [MYR-41](https://linear.app/myrobotaxi/issue/MYR-41) (2026-04-25) alongside DV-03: Prisma migration adds `timeToFull Float?` to `Vehicle`, Go writer applier persists every event, `vehicle_repo.go` SELECT/UPDATE round-trips the column. | **v1 ships `timeToFull` as a member of the `charge` atomic group with unit `hours` (decimal double).** DB persistence path is now closed end-to-end. See §4.1.4 for the full wire contract. Note that Tesla also exposes `EstimatedHoursToChargeTermination` (proto field 190) as a related "simple" ETA that always targets `ChargeLimitSoc`; proto 43 is the v1 source because it is trip-aware (time-to-trip-ready during Trip Planner sessions) -- delineation and decision rationale resolved by [MYR-28](https://linear.app/myrobotaxi/issue/MYR-28) on 2026-04-21, see `vehicle-state-schema.md` §7.1 for the full comparison and citations. Proto 190 stays held out of `fieldMap` pending the MYR-28 flip-condition Trip Planner capture tracked in MYR-25. | NFR-3.1, §4.1.4 | [`MYR-41`](https://linear.app/myrobotaxi/issue/MYR-41) — Persist chargeState + timeToFull to Prisma Vehicle table |
| **DV-05** | **RESOLVED** | Fixture files for every message type | **RESOLVED by MYR-13.** 35 canonical fixtures authored across `websocket/`, `rest/`, `atomic-groups/`, and `edge-cases/` directories. Every `type` in §4 / §5 has at least one happy-path fixture. Edge cases cover nav clear, 0,0 GPS sentinel, null gear, spec-only nulls, active charging, micro-drive, and device_cap. Includes `auth_ok.json` and the four-field charge group. `contract-tester` wiring is a follow-up. | (Resolved -- see [`fixtures/README.md`](fixtures/README.md) for the full index.) | NFR-3.45, §4 catalog | `MYR-13 Create canonical contract fixtures for conformance testing` |
| **DV-06** | Open | `auth_timeout` close code conflated with `auth_failed` | Both errors close with `websocket.StatusPolicyViolation` (1008). SDK has to read the `error.code` to decide retry policy; fragile if the error frame races the close. | Map `auth_timeout` to a dedicated close code (proposed: `4001` Auth Token Expired) so SDKs can branch on the close code alone. Requires updating `handler.go:handleUpgrade` error path + SDK close-code switch. | FR-7.1, FR-7.3, §6.2 | `MYR-XX Emit distinct close code for auth_timeout vs auth_failed` |
| **DV-07** | **RESOLVED minus `sinceSeq` (which is DV-02)** | Client control frames (`subscribe`/`unsubscribe`/`ping`/`pong`) and typed `permission_denied` | `readPump` previously ignored all post-auth client frames. | **Resolved by [MYR-46](https://linear.app/myrobotaxi/issue/MYR-46).** `readPump` now dispatches `subscribe`, `unsubscribe`, and `ping` frames via `internal/ws/control_frames.go`. Each `Client` carries a `subscribed` set seeded from `vehicleIDs` at handshake (so pre-MYR-46 SDK consumers that never send `subscribe`/`unsubscribe` keep receiving every owned vehicle). `subscribe` to a non-owned vehicle emits a typed `error` frame with `code: vehicle_not_owned` and closes the socket with **close code 4002**. `unsubscribe` removes the vehicle from the active set without closing. `ping` echoes the nonce back as `pong`. The `sinceSeq` snapshot-resume sub-row stays open under DV-02 (envelope sequence numbers). | FR-8.1, NFR-3.21, §5 | [`MYR-46`](https://linear.app/myrobotaxi/issue/MYR-46) — Per-vehicle subscribe/unsubscribe + permission_denied error frame |
| **DV-08** | **RESOLVED (target documented; wiring still pending)** | Per-IP and per-user connection caps | `HandlerConfig.MaxConnectionsPerIP` exists in [`handler.go`](../../internal/ws/handler.go) line 33 and `handleUpgrade` checks it, but [`cmd/telemetry-server/main.go`](../../cmd/telemetry-server/main.go) line 178 constructs `HandlerConfig` without populating it. `WebSocketConfig.MaxConnectionsPerUser` (default **5**, [`internal/config/defaults.go`](../../internal/config/defaults.go) line 67) exists but is not threaded into the handler either. | **Ship both caps:** per-IP **64** (pre-auth, breach -> HTTP 429, no WS handshake), per-user **5** (post-auth, breach -> `error` frame `code="rate_limited"` + close **4003 Server Overload**). See §1.3 for enforcement points and rationale and §6.1.1 for the `rate_limited` reconnect policy. The wiring change is a follow-up implementation issue. | NFR-3.6, §1.3, §6.1.1, §6.2 | `MYR-XX Wire MaxConnectionsPerIP (64) + MaxConnectionsPerUser (5) into HandlerConfig with asymmetric enforcement` |
| **DV-09** | **Open (reduced)** | Vehicle ownership AND role snapshot stale mid-connection | **Vehicle-deletion sub-case RESOLVED by [MYR-73](https://linear.app/myrobotaxi/issue/MYR-73) (2026-05-09):** the Postgres `vehicle_deleted` LISTEN/NOTIFY pipeline (Phase 1 trigger in `react-frontend`, Phase 2 listener in `internal/store/notify_listener.go`) drives `Hub.RemoveVehicle` on the deletion event and closes every subscribed client with code 4002 reason `vehicle access revoked`. **Remaining open:** role-mutation drift (Invite-driven owner↔viewer transitions) still requires reconnect — `Client.vehicleRoles` is still a handshake snapshot, and there is no Invite-state notify channel today. | Add an Invite-state notify channel (Postgres trigger or app-layer signal) that maps to a hub-side role refresh hook OR a targeted close. The deletion path is already wired and can be the template. | NFR-3.19, NFR-3.21, FR-5.3, §4.5, §4.6 | `MYR-XX Refresh WebSocket client role on Invite-state change (DV-09 remainder)` |
| **DV-10** | Open | `speed` not in GPS atomic group | `requirements.md` NFR-3.1 lists `speed` in the GPS group. [`vehicle-state-schema.md`](vehicle-state-schema.md) §7.1 resolves `speed` as ungrouped (2 s cadence vs. 10 m GPS delta filter). Server delivers `speed` independently. | Amend NFR-3.1 in `requirements.md` to reflect the resolved decision. No wire change needed. | NFR-3.1, §4.1.7 | `MYR-XX Amend NFR-3.1 to remove speed from GPS atomic group` |
| **DV-11** | **RESOLVED** | `drive_ended` wire payload is summary-only (FR-3.4 scope split) | Server emits summary fields only; full FR-3.4 fields are persisted in `Drive` and fetched via REST `GET /drives/{id}`. | **v1 ships the summary on the wire + an explicit `fetchDrive(driveId)` SDK helper** (unanimous recommendation from sdk-typescript and sdk-swift). Target SDK API: `client.onDriveEnded(cb)` + `await client.fetchDrive(id)` + TS `useDrive(id)` React hook + Swift `client.fetchDrive(_:)` async method. REST endpoint is `GET /drives/{id}` -- authoritative reference is [`rest-api.md`](rest-api.md) (placeholder until that doc is authored; see README index). No wire change; documented in §4.3 and [`state-machine.md`](state-machine.md) §3.3 / §4.1. | FR-3.1, FR-3.4, §4.3 | (No implementation issue needed for server; TS/Swift SDK issues implement `fetchDrive`.) |
| **DV-12** | **RESOLVED** | `drive_ended.duration` string format dropped | **Resolved by [MYR-32](https://linear.app/myrobotaxi/issue/MYR-32).** Server now emits `durationSeconds` (float64, `DriveStats.Duration.Seconds()`) on the `drive_ended` wire frame. The Go `time.Duration.String()` format is no longer emitted. `messages.go` struct field renamed from `Duration string` to `DurationSeconds float64` with JSON tag `"durationSeconds"`. `broadcaster.go:handleDriveEnded` calls `.Seconds()` instead of `.String()`. Tests verify the JSON key name and float64 roundtrip. | (Resolved.) | FR-3.4 ergonomics, §4.3 | [`MYR-32`](https://linear.app/myrobotaxi/issue/MYR-32) — Emit drive_ended.durationSeconds (replace duration string) |
| **DV-13** | **Requirement amendment pending** | `tripStartTime` atomic group membership | `requirements.md` NFR-3.1 lists `tripStartTime` as a member of the *navigation* atomic group. There is no Tesla field for `tripStartTime`; it is derived from the drive detector's `started_at` timestamp in [`internal/drives/`](../../internal/drives/). | **Relocate `tripStartTime` from the `navigation` group to the `drive` group** in NFR-3.1. The drive group is delivered via `drive_started` (not `vehicle_update`), so `tripStartTime` is naturally carried as `drive_started.payload.startedAt` -- see §4.2. MYR-11 ships with DV-13 still marked "amendment pending"; a separate NFR-3.1 amendment PR is required to fully close DV-13. (Bundling with DV-01 is fine if the architect prefers one amendment PR for both.) | NFR-3.1, §4.2 | `MYR-XX Amend NFR-3.1: relocate tripStartTime from navigation group to drive group` |
| **DV-14** | **New** | Slow-auth attack mitigation | Neither the per-IP nor the per-user cap (DV-08 target) defends against a slow-auth attack where each TCP connection sits under the per-IP cap but holds the 5 s `AuthTimeout` window. An attacker can still saturate the upgrade path by opening connections just below the concurrent cap and letting them idle through the auth deadline. | Add EITHER (a) a dedicated pre-auth rate-limit on upgrade *attempts* (token-bucket over a 1-minute window, independent of the concurrent-connection count), OR (b) a shortened `AuthTimeout` under load (e.g. drop from 5 s to 1 s when `hub.ipConnectionCount` for the source IP exceeds a soft threshold). Architect + security to decide which. This is a secondary mitigation to DV-08 and does not block v1 ship, but must be tracked so it is not forgotten. | NFR-3.6, §1.3 | `MYR-XX Add slow-auth attack mitigation (pre-auth upgrade rate limit OR adaptive AuthTimeout)` |
| **DV-15** | **RESOLVED** | state-machine.md C-3 trigger alignment | [`state-machine.md`](state-machine.md) §1.3 previously defined C-3 (`connecting -> connected`) as triggered by "first data frame OR heartbeat arrives", because it was authored before `auth_ok` was pulled into v1. **Owner:** [MYR-31](https://linear.app/myrobotaxi/issue/MYR-31) (`Agent/sdk-architect`). | **RESOLVED by MYR-31.** `state-machine.md` §1.3 C-3 now reads `AUTH_OK_RECEIVED` with guard "Server sends `auth_ok` frame". §1.1 Mermaid diagram, §4.1 message catalog, §4.2 event-to-transition mapping, and §5.1 reconnect sequence diagram all updated. Both docs now agree: C-3 is triggered by receipt of `auth_ok`. | FR-8.1, §2.3, §2.4 | [`MYR-31`](https://linear.app/myrobotaxi/issue/MYR-31) — Amend state-machine.md C-3 trigger: first-frame -> auth_ok receipt |
| **DV-16** | **RESOLVED** | `auth_ok` frame emission | **Resolved by [MYR-33](https://linear.app/myrobotaxi/issue/MYR-33).** Server now emits `auth_ok` as the first server-to-client frame after `Hub.Register` succeeds. Implementation: [`handler.go:sendAuthOk`](../../internal/ws/handler.go) called from `handleUpgrade` on the success path, before the readPump/writePump handoff. Wire shape matches §2.3 (`userId`, `vehicleCount`, `issuedAt`). Tests assert `auth_ok` is the first frame on success and is NOT emitted on failure paths. | (Resolved.) | FR-6.1, §2.3, §2.4 | [`MYR-33`](https://linear.app/myrobotaxi/issue/MYR-33) — Emit auth_ok frame from handler.go:authenticateClient on success |
| **DV-17** | **RESOLVED** | Empirical `timeToFull` unit verification | **Resolved 2026-04-22 via live capture** against the prod telemetry server during a home Level-2 charging session on VIN ending `3795`. Observed value: `1.066666841506958` as a `DoubleValue` at chargeLevel 68%. Cross-checked against the in-car "time to full" display and the per-hour kWh math (~9 kWh remaining × ~8-10 kW = ~0.9-1.1 hours). **Unit is hours (decimal double)** — the [tesla-fleet-telemetry-sme skill](../../.claude/skills/tesla-fleet-telemetry-sme/) and legacy Tesla REST `time_to_full_charge` descriptions are correct; the briefly-introduced "seconds" label caught by the MYR-11 post-freeze audit was factually wrong. Full capture + methodology in the [MYR-25 comment](https://linear.app/myrobotaxi/issue/MYR-25#comment-4f1dcee9-ab10-4039-acc5-9e7ef25c3762). The empirical fixture `1.066666841506958` is pinned in `TestDecoder_DecodePayload_TimeToFull` (MYR-40) to guard against future regressions. | (Resolved.) | NFR-3.1, §4.1.4 | [`MYR-25`](https://linear.app/myrobotaxi/issue/MYR-25) — Verify TimeToFullCharge unit empirically against charging vehicle |
| **DV-18** | **RESOLVED** | `FieldChargeState` internal constant collision | **Resolved by [MYR-26](https://linear.app/myrobotaxi/issue/MYR-26).** The existing `FieldChargeState` constant (which mapped to proto 179 `DetailedChargeState`) has been renamed to `FieldDetailedChargeState` with internal name `"detailedChargeState"`. A new `FieldChargeState` constant now correctly maps to proto field 2 (`Field_ChargeState`) with internal name `"chargeState"`. Both constants are in `fieldMap`; the new `FieldChargeState` is intentionally not yet in `DefaultFieldConfig` — fleet API configuration is added by the DV-03 implementation PR. **Note:** [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42) subsequently removed the `FieldDetailedChargeState` constant entirely (proto 179 now populates `FieldChargeState` directly), so only `FieldChargeState` remains. The collision resolution in MYR-26 stays valid; it was the right interim step. | (Resolved.) | NFR-3.1, §4.1.4 | [`MYR-26`](https://linear.app/myrobotaxi/issue/MYR-26) — Resolve FieldChargeState constant collision before DV-03 wiring |
| **DV-19** | **RESOLVED** | `chargeState` re-sourced from proto 179 (proto 2 deprecated on recent firmware) | [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) wired `chargeState` from Tesla proto field **2** (`ChargeState`) based on the documented proto schema. On 2026-04-23, empirical capture against the prod server showed that Tesla firmware ≥ 2024.44.25 (on VIN ending `3795`) accepts proto 2 in `fleet_telemetry_config` (API returns `synced: true` with `ChargeState` listed at `interval_seconds: 30`) but the vehicle **never actually emits proto 2** — even across plug/unplug transitions observed over multi-minute charging sessions. Proto 179 `DetailedChargeState` fires on the same transitions with identical enum string values. Evidence: two frames captured during an unplug/plug cycle at 03:37-03:38 UTC showed `proto179="Stopped"` then `proto179="Charging"` with `proto2="—"` (absent) in both. Tesla's public proto schema documents proto 2 as a valid field but recent firmware has silently stopped populating it in favor of proto 179. The `tesla-fleet-telemetry-sme` skill was not aware of this deprecation. | **Re-source `chargeState` from proto 179 (`Field_DetailedChargeState`)** via the `Value_DetailedChargeStateValue` oneof variant (primary) with `Value_ChargingValue` fallback for pre-2024.44.25 firmware. Wire-level behavior unchanged — enum string values are identical (`Unknown`, `Disconnected`, `NoPower`, `Starting`, `Charging`, `Complete`, `Stopped`). Resolved by [MYR-42](https://linear.app/myrobotaxi/issue/MYR-42) on 2026-04-23: `Field_ChargeState` removed from `fieldMap`, `FleetFieldChargeState` removed from `DefaultFieldConfig`, `Field_DetailedChargeState → FieldChargeState` added to `fieldMap`, `convertChargeState` consolidated to handle both oneof variants. Contract docs updated: `vehicle-state-schema.md` §1.1/§2.2/§7.1, `schemas/vehicle-state.schema.json` (`x-tesla-proto-field` 2 → 179), `data-classification.md` §1.3. Generalizable lesson captured in the `project_tesla_proto2_deprecated.md` user memory: Tesla's proto schema listing a field is NOT proof the vehicle populates it on current firmware — empirical verification required before promoting to `fieldMap`. | NFR-3.1, §4.1.4 | [`MYR-42`](https://linear.app/myrobotaxi/issue/MYR-42) — Re-source chargeState wire field from proto 179 (DetailedChargeState) |

### Divergence management rules

1. **One-way door for the catalogue.** A new divergence MUST be added to this table in the same PR that introduces the gap. `contract-guard` treats an undocumented divergence between this doc and `internal/ws/` as merge-blocking drift.
2. **Closing a divergence.** When a follow-up PR fully resolves a divergence, mark the row's **Status** column as **RESOLVED** and add a one-line entry in the change log (§11) referencing the resolving PR and Linear issue. RESOLVED rows are **retained** in the table for audit-trail continuity. Do NOT reuse a DV-NN number even if its row is later deleted.
3. **RESOLVED-with-implementation-pending.** A divergence may be marked RESOLVED at the contract level (the target shape is locked and documented here) while the implementation follow-up is still in flight. In that case, the Status column carries the qualifier "RESOLVED (target documented; wiring still pending)" and the row references the implementation issue. Once the implementation lands, the qualifier is dropped.
4. **Amendment divergences.** DV entries that propose to amend `requirements.md` (currently DV-01, DV-10, DV-13) carry the status "**Requirement amendment pending**" until the amendment PR lands. They MUST be resolved in one of two ways: ship the change, or land a `requirements.md` amendment PR that updates the NFR literal. "Leave it as drift forever" is not an option. MYR-11 is explicitly permitted to merge with DV-01 and DV-13 in "amendment pending" state because (a) the wire contract is already consistent with the correct values, and (b) the amendment PRs are out-of-scope for MYR-11's change footprint.

---

## 11. Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-13 | Initial full draft (closed PR #166): handshake, envelope, server->client and client->server catalogs, error/close-code matrix, heartbeat/reconnect/snapshot semantics, AsyncAPI 3.0 spec, sibling JSON Schemas, §10 open questions. Authored by a general-purpose agent role-playing `sdk-architect`; closed by the user for re-do with the real subagent. | general-purpose (role-played) |
| 2026-04-13 | **Authoritative rewrite by the registered `sdk-architect` subagent.** (1) Corrected the anchored-requirements table: fixed FR-8.1/FR-8.2 labels, added FR-7.3, NFR-3.9, NFR-3.12, NFR-3.13 anchors. (2) Fixed the `MaxConnectionsPerIP` wiring claim: it is **not** populated by `main.go` today, so the per-IP cap is unwired. Recorded as DV-08. (3) Added the `charge`/`gps`/`gear` non-accumulator server flow explanation to §3.2 (only nav has a dedicated debounce). (4) Replaced §10 "open questions" with a formal "Code ↔ spec divergences" catalogue with stable DV-NN IDs and divergence-management rules. (5) Added new divergences: DV-03 (`chargeState`), DV-04 (`timeToFull`), DV-06 (`auth_timeout` close-code conflation), DV-10 (`speed` ungrouped), DV-11 (`drive_ended` FR-3.4 scope split), DV-12 (`duration` string format), DV-13 (`tripStartTime`). (6) Tightened §6.1.1 to separate today/planned + reconnect policy columns, added `snapshot_required` reserved code. (7) Added forward-compat open-object rule to §3.1 for FR-1.3. | sdk-architect |
| 2026-04-13 | **v1 contract freeze (second pass).** Specialist decisions from `tesla-telemetry`, `security`, `sdk-typescript`, `sdk-swift` applied: **DV-01** recast as requirement-drift (NFR-3.2 literal 200 ms is wrong; Tesla 500 ms bucket + 1 s `interval_seconds` minimum is the floor; NFR-3.2 amendment pending). **DV-03, DV-04 RESOLVED** -- `chargeState` (proto field 2, enum) and `timeToFull` (proto field 43, double seconds) added to the v1 charge atomic group; `vehicle-state-schema.md` line 333 factually-wrong "not available from Tesla Fleet Telemetry" claim corrected. **DV-08 RESOLVED** (target documented; wiring still pending): both caps ship with asymmetric enforcement -- per-IP 64 (pre-auth, HTTP 429) + per-user 5 (post-auth, error frame + close 4003). **DV-11 RESOLVED** -- summary on wire + `fetchDrive(driveId)` SDK helper (unanimous SDK recommendation). **DV-12 RESOLVED** -- `duration` string dropped, `durationSeconds` number replaces it. **DV-13** recast as requirement-drift (`tripStartTime` relocated from navigation group to drive group; carried as `drive_started.payload.startedAt`; NFR-3.1 amendment pending). **auth_ok** pulled out of DV-07 and made v1-required (new §2.3 content; C-3 trigger is now `auth_ok` receipt; rest of DV-07 remains open for `subscribe`/`unsubscribe`/`ping`/`pong` + typed `permission_denied`). **New divergences added:** **DV-14** (slow-auth attack mitigation follow-up to DV-08) and **DV-15** (`state-machine.md` C-3 trigger alignment follow-up). Cross-contract updates to `vehicle-state-schema.md` §2.2 and §7.1. AsyncAPI spec + `ws-envelope.schema.json` + `ws-messages.schema.json` updated in lockstep. | sdk-architect |
| 2026-04-13 | **PR #167 review pass.** Addresses Claude Review warnings (3) and ux-audit + Claude Review nice-to-haves (7). (1) Updated DV-03, DV-04, DV-12 Status column to "RESOLVED (target documented; wiring still pending)" to match the §10 Rule 3 qualifier convention that DV-08 already uses. (2) Added **DV-16** for the `auth_ok` frame emission gap: server has zero `auth_ok` references in `internal/ws/` today, so an SDK that implements §2.3 rule 4 literally will hit its 6-second pre-`auth_ok` timer on every connection. DV-16 status is "RESOLVED (target documented; wiring still pending)". (3) Updated the §4 catalog `auth_ok` row Source cell from the clever-paren rationale to a plain `(target; see DV-16)` reference. (4) Added a paragraph after §3.1's open-object rule clarifying that `additionalProperties: false` in the JSON Schemas is the contract-tester invariant, not a runtime SDK rule. (5) Added a perceptual-smoothness scope note to §3.2.1 explaining the 500 ms debounce applies only to the nav group and that GPS/position updates arrive on Tesla's per-field cadence, independent of nav. (6) Added a C-3 inline gloss to §2.3 rule 1 so a reader does not have to tab-switch to state-machine.md. (7) Added SDK type-gen guidance (`string | null` / `Optional<String>`) and null-placeholder UI prose (`--`, not a spinner) for `chargeState` / `timeToFull` in §4.1.4. (8) Added a `rate_limited` reconnect pseudocode block + a `rate_limited.device_cap` typed sub-code to §6.1.1 so per-user cap breaches surface an actionable "too many devices" UI signal instead of a generic rate-limit toast. `ErrorPayload.subCode` added to `ws-messages.schema.json` with the `device_cap` enum. (9) Added a status legend above the §10 catalogue so a reader can classify rows as `RESOLVED` / `RESOLVED (wiring pending)` / `Requirement amendment pending` / `Open` / `New` at a glance. (10) Added an audit-trail footnote under `data-classification.md` §6 "By tier" summary recording MYR-11 as the source of the P0 count bump 83 -> 85. | sdk-architect |
| 2026-04-15 | **DV-16 RESOLVED by [MYR-33](https://linear.app/myrobotaxi/issue/MYR-33).** Server now emits `auth_ok` as the first frame after `Hub.Register` succeeds (`handler.go:sendAuthOk`). §4 catalog row updated from `(target; see DV-16)` to `handler.go:sendAuthOk`. §10 DV-16 status flipped from "RESOLVED (target documented; wiring still pending)" to "RESOLVED". | go-engineer |
| 2026-04-14 | **Tesla SME audit corrections.** After the MYR-11 freeze commit landed, a trust-but-verify audit by the `tesla-telemetry` subagent against the `tesla-fleet-telemetry-sme` skill and the vendored `vehicle_data.proto` found three errors, all stemming from the same "claim-without-citation" failure mode that caused the original MYR-8 `timeToFull` incident. **Fix 1 — `timeToFull` unit (CRITICAL):** §4.1.4 and every cross-contract reference labeled the unit as "seconds". The SME skill documents it as **hours (decimal)** and the legacy Tesla REST API `time_to_full_charge` is also in hours. Corrected across §4.1.4, `vehicle-state-schema.md` §1.1/§2.2/§7.1, `data-classification.md` §1.3, `vehicle-state.schema.json` (added `chargeState` and `timeToFull` field definitions — the JSON Schema had NOT been updated in the previous freeze pass, a separate drift the audit also caught), and the AsyncAPI example (5400 seconds → 1.5 hours). **Fix 2 — fabricated protobuf identifier:** §3.2 and `vehicle-state-schema.md` §2.2 referenced a type name `VehicleTelemetryEvent.Fields` that does not exist in the Tesla proto. Tesla's actual top-level message is `Payload` with repeated `Datum` entries. Corrected to reference the real type. **Fix 3 — sourceless `interval_seconds` claim:** §3.2.1 claimed Tesla enforces a 1-second minimum and cited our own `fleet_api_fields.go` lines as authority. The 1-second value is OUR highest-cadence request, not a published Tesla floor. Reworded to distinguish REQUESTED cadence from the DELIVERED-as-one-message cadence (500 ms Tesla vehicle-side bucket). **New divergences added:** **DV-17** — empirical unit verification of `TimeToFullCharge` via charging-vehicle protobuf capture, required before any SDK build generates types against `timeToFull`. **DV-18** — `FieldChargeState` internal constant collision: `internal/telemetry/fields.go` already uses that name for proto 179 (`DetailedChargeState`); the DV-03 implementation PR must rename the existing constant before adding a new one for proto 2. Flagged as a Go-side trap the contract doc cannot fix on its own. | sdk-architect |
| 2026-04-15 | **DV-15 RESOLVED** by MYR-31. `state-machine.md` §1.3 C-3 trigger amended from "first data frame OR heartbeat" to "receipt of `auth_ok`". Both docs now agree on the canonical C-3 trigger. | sdk-architect |
| 2026-04-15 | **DV-12 RESOLVED by [MYR-32](https://linear.app/myrobotaxi/issue/MYR-32).** Server now emits `durationSeconds` (float64) instead of `duration` (Go string) on `drive_ended` frames. `messages.go` field renamed, `broadcaster.go` calls `.Seconds()` instead of `.String()`. §10 DV-12 status flipped from "RESOLVED (target documented; wiring still pending)" to "RESOLVED". | go-engineer |
| 2026-04-15 | **DV-18 RESOLVED by [MYR-26](https://linear.app/myrobotaxi/issue/MYR-26).** Renamed `FieldChargeState` (proto 179) to `FieldDetailedChargeState`; added new `FieldChargeState` for proto field 2 (`Field_ChargeState`). The naming collision that would have blocked DV-03 wiring is eliminated. §10 DV-18 status flipped from "New (implementation trap)" to "RESOLVED". | go-engineer |
| 2026-04-15 | **MYR-27: Rename `fsdMilesToday` to `fsdMilesSinceReset`.** Wire field name in §3.2 ungrouped list, §4.1 rename table, and §4.1.7 ungrouped field list updated. Tesla's `SelfDrivingMilesSinceReset` does not reset daily; the cosmetic label was wrong. | sdk-architect |
| 2026-04-21 | **MYR-28: Delineate `TimeToFullCharge` (proto 43) vs `EstimatedHoursToChargeTermination` (proto 190).** Research confirmed proto 43 is trip-aware (reports time-to-trip-ready during Trip Planner sessions, time-to-`ChargeLimitSoc` otherwise) while proto 190 always reports the simple time-to-`ChargeLimitSoc`. Decision: keep proto 43 as the `timeToFull` source because trip-awareness matches the product UX "when will my car be done charging?". Updated §10 DV-04 to cross-reference the resolved delineation in `vehicle-state-schema.md` §7.1 (the canonical location for the full comparison + citations). No wire, schema, fixture, or Go code changes — proto 43 was already the source; this PR is pure documentation closing a research question MYR-11 left open. Empirical side-by-side capture folded into DV-17 (MYR-25). | sdk-architect |
| 2026-04-22 | **MYR-40: Wire `chargeState` (proto 2) + `timeToFull` (proto 43) into the live WS path.** Added `FleetFieldChargeState` to `DefaultFieldConfig`, promoted `Field_TimeToFullCharge` from MYR-25 observation-only into `fieldMap`, added `convertChargeState` routing proto 2's `Value_ChargingValue` oneof through the existing `chargingStateString` helper. `timeToFull` routes through `convertNumericOrString` (handles `DoubleValue` → `float64` hours). §4.1.4 prose updated: "Today the server requests only two of four charge fields" wording replaced with "all four fields in `DefaultFieldConfig`." DV-03 and DV-04 flipped from "RESOLVED (target documented; wiring still pending)" to "RESOLVED (wire wiring live; REST snapshot DB persistence pending)" — the DB-persistence follow-up needs a cross-repo Prisma PR in `../react-frontend` since `Vehicle` is Prisma-owned. DV-17 flipped from "New (research)" to "RESOLVED" — empirical 1.0667h capture on 2026-04-22 confirmed the hours unit. Proto 190 (`EstimatedHoursToChargeTermination`) remains held out of `fieldMap` pending the MYR-28 §7.1 flip-condition Trip Planner capture tracked in MYR-25. | go-engineer + sdk-architect |
| 2026-04-23 | **MYR-42: Re-source `chargeState` from proto 179 `DetailedChargeState` (proto 2 deprecated in recent firmware).** Empirical capture on 2026-04-23 against the prod server showed Tesla firmware ≥ 2024.44.25 accepts proto 2 in `fleet_telemetry_config` (`synced: true`) but never actually emits it. Proto 179 fires on the same transitions with identical enum string values. §4.1.4 wire-table source column updated from "proto field 2 (`ChargeState`)" → "proto field 179 (`DetailedChargeState`)". Added **DV-19** as a new resolved divergence capturing the firmware-deprecation finding and switch. DV-03 resolution cell updated to reference the new source proto. Cross-contract updates to `vehicle-state-schema.md` §1.1/§2.2/§7.1, `schemas/vehicle-state.schema.json`, and `data-classification.md` §1.3. Wire-level behavior unchanged — enum strings identical across proto 179's `DetailedChargeStateValue` enum and the legacy `ChargingState` enum. `Field_ChargeState` removed from `fieldMap`; `FleetFieldChargeState` removed from `DefaultFieldConfig`; `convertChargeState` consolidated to handle both oneof variants (`DetailedChargeStateValue` primary + `ChargingValue` pre-2024.44.25 fallback). | go-engineer + sdk-architect |
| 2026-05-09 | **MYR-73 Phase 2: vehicle-deletion sub-case of DV-09 RESOLVED.** Added `Hub.RemoveVehicle(vehicleID)` driven by a Postgres `vehicle_deleted` LISTEN/NOTIFY pipeline. The Next.js trigger (cross-repo) fires on every Vehicle DELETE; the Go listener republishes as `VehicleDeletedEvent` on the in-process bus; a dispatcher fans out to the hub (close subscribed clients with 4002), the Tesla mTLS receiver (close active inbound stream), the VIN cache (evict stale identifiers), and the JWT user-existence cache (1s singleflight TTL — bounded staleness for deleted users). Receiver also rejects inbound frames for unknown VINs with HTTP 403 and the `tesla_inbound_rejected_total{reason="vehicle_not_authorized"}` counter. Role-mutation drift remains open under DV-09 (reduced). | go-engineer |
