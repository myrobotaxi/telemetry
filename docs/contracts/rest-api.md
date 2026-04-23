# REST API Contract

**Status:** Draft -- v1
**Target artifact:** OpenAPI 3.1 specification at [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml)
**Owner:** `sdk-architect` agent
**Last updated:** 2026-04-14

## Purpose

Defines every HTTP REST endpoint the telemetry server exposes to the MyRoboTaxi TypeScript and Swift SDKs. This contract is the authoritative source for:

- The non-streaming half of the SDK surface: cold-load snapshots, paginated drive history, per-drive detail, per-drive route playback, sharing/invite lifecycle, and user-initiated data deletion
- The authentication scheme (bearer token from `getToken()`) shared with the WebSocket contract
- The typed error envelope and the REST extensions to the shared error code catalog
- Cursor-based pagination semantics
- Role-based field masks applied server-side to every response
- The split between the real-time WebSocket surface and the snapshot-or-lifecycle REST surface

The markdown is the human source of truth. Its machine-readable twin is [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml). Payload shapes reuse [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) via `$ref` -- they are NOT re-declared. REST-only shapes (drive summary, drive detail, drive route, invite, error envelope, pagination wrapper) are declared inline in the OpenAPI document under `components/schemas` until a follow-up issue extracts them to sibling JSON Schemas. Drift between this doc, the OpenAPI spec, and the server implementation is a CI failure ([`contract-guard`](../../CLAUDE.md#merge-policy-non-negotiable)).

Known, **accepted** divergences between this contract and the current `internal/server/` / `internal/store/` implementation are catalogued in §10. Every such entry has a proposed Linear follow-up title. A divergence that is not listed in §10 is contract drift and MUST be fixed, not added.

## Anchored requirements

Every FR/NFR listed here is anchored in at least one section of this doc. The tag in the "Where" column is the exact section the requirement lands in.

| ID | Requirement | Where it lands |
|----|-------------|----------------|
| **FR-3.2** | Paginated drive history (list of past drives with basic metadata) | §7.2 `GET /vehicles/{vehicleId}/drives` |
| **FR-3.3** | Per-drive route playback (full GPS trail) | §7.4 `GET /drives/{driveId}/route` |
| **FR-3.4** | Per-drive stats (distance, duration, energy, FSD, interventions, start/end loc+addr) | §7.3 `GET /drives/{driveId}` |
| **FR-5.1** | Invite creation (owner -> recipient) | §7.5 `POST /vehicles/{vehicleId}/invites` |
| **FR-5.2** | Viewer list for owners | §7.5 `GET /vehicles/{vehicleId}/invites` |
| **FR-5.3** | Revoke viewer access | §7.5 `DELETE /invites/{inviteId}` |
| **FR-5.4** | Role model: owner + viewer, static masks server-side | §5 RBAC field masks |
| **FR-5.5** | Architecture supports a third role without schema changes | §5 RBAC field masks (extension seam) |
| **FR-6.1** | SDK accepts a `getToken()` callback; SDK never stores credentials | §3 Authentication |
| **FR-6.2** | SDK calls `getToken()` on initial connect and on every auth error | §3.3 Auth failure and retry |
| **FR-7.1** | Typed error codes (no string-matching on message) | §4.1 Error envelope |
| **FR-7.3** | Only terminal errors surface to UI; transient errors auto-retry | §4.1 error catalog (reconnect policy column) |
| **FR-9.1** | One-shot paginated fetch + reactive subscription for recency | §4.2 Pagination; §7.2 cross-reference to `drive_ended` |
| **FR-9.2** | Completed drive appears in the live drives list without a re-fetch | §7.2 FR-9.1/FR-9.2 pairing note |
| **FR-10.1** | User-initiated deletion of all user data | §7.6 `DELETE /users/me` |
| **FR-10.2** | Deletion writes an immutable audit log entry | §7.6 + cross-ref to [`data-lifecycle.md`](data-lifecycle.md) §3.1 |
| **NFR-3.5** | Snapshot must contain enough data to render the full UI (no per-field spinners) | §7.1 `GET /vehicles/{vehicleId}/snapshot` |
| **NFR-3.11** | Reconnect re-fetches DB snapshot before resuming live stream | §7.1 + cross-ref to `websocket-protocol.md` §7.2 reconnect sequence |
| **NFR-3.19** | Every WS broadcast projected through recipient's role mask; no raw fan-out | §5 RBAC field masks (applied to REST too) |
| **NFR-3.20** | Persisted DB reads respect viewer's role-based field visibility | §5 RBAC field masks |
| **NFR-3.21** | Vehicle ownership enforced on every API call | §3 Authentication + §5 RBAC |
| **NFR-3.22** | TLS in transit for all external connections | §2.1 Transport |
| **NFR-3.23** | AES-256-GCM application-level encryption for P1 fields | §7.4 drive route transport note; §8 resource schemas |
| **NFR-3.27** | Drives retained for 1 year rolling window | §7.2 pagination ordering + cross-ref to `data-lifecycle.md` §2.2 |
| **NFR-3.29** | Audit logs retained indefinitely | §7.6 + cross-ref to `data-lifecycle.md` §2.3 |

---

## 1. Table of contents

1. Table of contents (this section)
2. Transport and base URL
3. Authentication
4. Common conventions (error envelope, pagination, versioning, headers, idempotency)
5. RBAC and field masks
6. Endpoint catalog summary
7. Endpoint reference
   1. `GET /api/vehicles/{vehicleId}/snapshot`
   2. `GET /api/vehicles/{vehicleId}/drives`
   3. `GET /api/drives/{driveId}`
   4. `GET /api/drives/{driveId}/route`
   5. Invite endpoints (3 operations)
   6. `DELETE /api/users/me`
8. Resource schemas
9. Observability
10. Code <-> spec divergences
11. Change log

---

## 2. Transport and base URL

> **Anchored:** NFR-3.22, FR-6.1.

### 2.1 Servers

REST endpoints are served from the same host as the WebSocket channel. The server list mirrors [`specs/websocket.asyncapi.yaml`](specs/websocket.asyncapi.yaml) `servers`:

| Environment | Base URL | Scheme | Notes |
|-------------|----------|--------|-------|
| Production | `https://api.myrobotaxi.com/api` | `https` (TLS, NFR-3.22) | Browser clients originate from `https://app.myrobotaxi.com`. TLS termination at the Fly.io edge. |
| Development | `http://localhost:8080/api` | `http` | Local dev only. Plain HTTP is allowed ONLY when the server is bound to loopback. |

The base path is `/api` to match the existing `/api/ws` WebSocket path ([`internal/ws/handler.go`](../../internal/ws/handler.go) line 43) and the existing `/api/vehicle-status/{vin}` + `/api/fleet-config/{vin}` REST endpoints already registered in [`cmd/telemetry-server/main.go`](../../cmd/telemetry-server/main.go) lines 190 and 277. Adopting `/api` for the SDK's REST surface keeps the mount point consistent across channels.

> **Divergence (DV-20):** None of the SDK-surface REST endpoints in §6 / §7 are mounted by the Go server today. The `/api` prefix is correct; the routes under it are not yet wired. See §10.

### 2.2 Content type

All request and response bodies are `application/json; charset=utf-8`. Clients MUST set `Content-Type: application/json` on every request that carries a body and SHOULD set `Accept: application/json`. The server replies with `Content-Type: application/json` on every non-empty response.

### 2.3 Method semantics

| Method | Used for |
|--------|----------|
| `GET` | Snapshot fetch, drive list, drive detail, drive route, invite list |
| `POST` | Invite creation |
| `DELETE` | Invite revocation, user self-deletion |

`PUT` and `PATCH` are **NOT used** in v1. Mutations are restricted to explicit creation (POST) and deletion (DELETE); there is no endpoint that updates an existing resource in-place. This simplifies idempotency semantics (see §4.5) and reduces the surface area of the contract.

---

## 3. Authentication

> **Anchored:** FR-6.1, FR-6.2, NFR-3.21, NFR-3.22.

### 3.1 Bearer token in the `Authorization` header

Every REST endpoint requires authentication. The client MUST send:

```
Authorization: Bearer <token>
```

The token is the **same opaque session token** that the SDK passes in the WebSocket `auth` frame (see [`websocket-protocol.md`](websocket-protocol.md) §2.2). Both transports resolve the token from the consumer's `getToken()` callback (FR-6.1), so the SDK maintains a single credential surface and never stores the token itself.

> **Why an HTTP header for REST but an in-band frame for WebSocket?** Browsers cannot set arbitrary headers on a WebSocket upgrade request, so the WS path pushes the token into the first WebSocket frame for portability (`websocket-protocol.md` §2.3 rationale). REST has no such constraint -- the standard `Authorization: Bearer <token>` header is universally supported by every HTTP client (browser `fetch`, Node `undici`, Swift `URLSession`, React Native `fetch`) and is the least-surprising choice.

### 3.2 Server-side validation

The server's REST middleware MUST:

1. Parse the `Authorization` header; reject requests without it with `401 auth_failed`.
2. Reject malformed headers (missing `Bearer ` prefix, empty token) with `401 auth_failed`.
3. Validate the token via the same `Authenticator` instance used by the WebSocket handler ([`internal/ws/auth.go`](../../internal/ws/auth.go)) -- in production this is `internal/auth.NewJWTAuthenticator`, which checks signature, issuer, audience, and expiry against `AuthConfig`.
4. Resolve the authenticated `userId`.
5. For vehicle-scoped endpoints, resolve the user's vehicle ownership set via `Authenticator.GetUserVehicles(ctx, userID)` and verify the requested `vehicleId` is in the set. On mismatch, return `403 vehicle_not_owned`.
6. Emit observability signals using the same slog / Prometheus / OTel conventions as the WebSocket handler (§9).

The entire REST auth middleware is PLANNED; no REST auth middleware exists in the current server -- see §10 DV-19.

### 3.3 Auth failure and retry (FR-6.2)

When the SDK receives an HTTP response whose status code is `401`:

1. The SDK MUST NOT retry the failing request with the same token.
2. The SDK MUST call `getToken()` again to obtain a fresh token.
3. The SDK MUST retry the original request **exactly once** with the new token.
4. If the retry also returns `401`, the SDK surfaces the error to the consumer as a typed `auth_failed` error (FR-7.1) and MUST NOT retry further. The consumer's auth layer is responsible for triggering re-authentication (sign-in flow).

This matches the WebSocket auth refresh flow in [`websocket-protocol.md`](websocket-protocol.md) §6.1.1 (`auth_failed` reconnect policy) -- one refresh attempt, then surface to UI.

### 3.4 TLS

Production REST traffic MUST use TLS (NFR-3.22). The server is served behind a TLS-terminating edge (Fly.io). The SDK MUST NOT permit plaintext HTTP against `api.myrobotaxi.com` in production. Local development on `localhost:8080` is exempt by policy -- the SDK MAY accept `http://localhost:*` URLs when a dev flag is set, but MUST refuse any non-loopback HTTP host.

### 3.5 Token redaction

The token is **P1** per [`data-classification.md`](data-classification.md) §1.2 (`AuthPayload.token` row, reused for REST). The server MUST NOT log the token in any structured log field, error message, metric label, or crash report. The `Authorization` header is stripped before the request is written to the slog `http request` line ([`internal/server/middleware.go:requestLogger`](../../internal/server/middleware.go)) -- this exclusion is PLANNED alongside the REST auth middleware (DV-19).

---

## 4. Common conventions

### 4.1 Error envelope and typed error codes

> **Anchored:** FR-7.1, FR-7.3.

All non-2xx responses carry a JSON body with this envelope:

```json
{
  "error": {
    "code": "auth_failed",
    "message": "invalid token",
    "subCode": null
  }
}
```

| Field | Type | Required | Classification | Notes |
|-------|------|----------|----------------|-------|
| `error.code` | `string` (enum) | Yes | P0 | Stable typed code. Consumers branch on this value per FR-7.1. |
| `error.message` | `string` | Yes | P0 (never contains P1) | Human-readable description for logs and developer tooling. Safe to display in developer-mode banners; not intended for end-user UI. |
| `error.subCode` | `string` (enum) \| `null` | No | P0 | Optional typed sub-code for branching consumer UI when the primary code is ambiguous across carriers. Currently only `device_cap` (shared with the WS ErrorPayload). |

Two rules are non-negotiable for every error response:

1. **Consumers MUST branch on `error.code`, never on `error.message`.** The message is a free-form English string for developer tooling and is subject to change without a protocol version bump. Per FR-7.1, the stable enum is the contract, not the prose.
2. **`error.message` MUST NOT contain any P1 value.** No GPS coordinates, no addresses, no location names, no tokens, no email addresses, no raw VINs (VIN appears only as `***XXXX` last-4 via `redactVIN()`). See [`data-classification.md`](data-classification.md) §2.2. Error construction sites in the REST handler MUST use opaque IDs (`vehicleId`, `driveId`, `userId`) for correlation, never the underlying sensitive values. `contract-guard` Rule CG-DC-2 blocks PRs that introduce P1 values into error construction sites.

#### 4.1.1 REST error code catalog

The REST catalog is a superset of the WebSocket catalog in [`websocket-protocol.md`](websocket-protocol.md) §6.1.1 and [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) `ErrorPayload.code`. The shared codes map directly to the same typed error values in the SDK's `CoreError` union, so consumer code branches on a single enum across both transports. REST adds three codes that have no WebSocket equivalent, flagged in the "Carrier" column as REST-only.

| Code | HTTP | Carrier | Status | Reconnect/retry policy | Description |
|------|------|---------|--------|------------------------|-------------|
| `auth_failed` | 401 | Shared (WS + REST) | Implemented (WS); PLANNED (REST, DV-19) | Surface to UI; refresh token via `getToken()`; retry once (FR-6.2). A second `auth_failed` is terminal for the operation. | Token signature/issuer/audience/expiry check failed, or the `Authorization` header was missing/malformed. |
| `auth_timeout` | 401 | Shared (WS + REST) | Implemented (WS); PLANNED (REST, DV-19) | Auto-retry once with fresh token; NFR-3.10-style backoff on subsequent attempts. | Rare REST path: server-side token validation exceeded its internal deadline. Treated as transient. |
| `permission_denied` | 403 | Shared (WS + REST, PLANNED on WS per DV-07) | PLANNED | Surface to UI; do not auto-retry the same operation. | Authenticated user attempted a resource they do not own or a role they do not have (e.g., viewer calling an invite endpoint). |
| `vehicle_not_owned` | 403 | Shared (WS + REST, PLANNED on WS per DV-07) | PLANNED | Surface to UI; do not auto-retry the same vehicleId. | Specific case of `permission_denied` for a vehicle-scoped endpoint whose `vehicleId` path param is not in the caller's ownership set. |
| `not_found` | 404 | **REST-only** | PLANNED (DV-20) | Surface to UI; do not retry. The resource either does not exist or is filtered out by ownership / role mask. | Unknown `vehicleId`, `driveId`, or `inviteId`. The SDK cannot distinguish "never existed" from "revoked access" -- this is intentional, so the server never leaks the existence of resources the caller cannot see. |
| `invalid_request` | 400 | **REST-only** | PLANNED (DV-20) | Surface to UI as a developer error; do not retry. | Request body, path params, or query string failed server-side validation (malformed cursor, `limit` out of range, malformed email on invite creation, etc.). |
| `rate_limited` | 429 | Shared (WS + REST) | PLANNED (WS DV-08; REST DV-22) | Auto-retry with extended backoff (§4.1.2). SDK MAY set `Retry-After` header as backoff hint. | Two distinct caps share the same typed code. WS emits `rate_limited` with `subCode: device_cap` for **concurrent-session cap** breaches (too many simultaneous WebSocket connections per user, see `websocket-protocol.md` §6.1.1 and DV-08). REST emits `rate_limited` (no sub-code in v1) for **request-rate cap** breaches (>120 req/min per authenticated user, see §4.1.2 and DV-22). Consumers distinguish the two via the carrier transport and the presence of `subCode`. |
| `internal_error` | 500 | Shared (WS + REST) | PLANNED | Auto-retry with exponential backoff (NFR-3.10 curve from `websocket-protocol.md` §7.1), cap at 3 REST attempts before surfacing. | Catch-all for unexpected server failures: panics, DB errors, downstream timeouts. |
| `service_unavailable` | 503 | **REST-only, PLANNED** | PLANNED (DV-21) | Auto-retry with exponential backoff; honor `Retry-After` header if present. | Reserved for maintenance windows and graceful-shutdown states. The server MAY return `503` during rolling deployments; v1 does not yet emit this code. Added to the REST catalog so SDK consumers can write forward-compatible handlers. |
| `snapshot_required` | -- | **WS-only** (close code 4005 + error frame) | PLANNED (DV-02) | n/a for REST | WS-only. REST has no analogue because REST is already the snapshot channel (the "fall back to snapshot fetch" signal IS a REST call). Listed here for completeness; REST clients never receive this code. |

##### 4.1.1.a REST-only codes added to the shared catalog

Three codes are REST-only extensions of the shared catalog: `not_found`, `invalid_request`, and `service_unavailable`.

- `not_found` is not emitted over the WebSocket because the WS path enforces ownership via silent filtering in `Hub.Broadcast` (see `websocket-protocol.md` §4.5) -- a client simply does not receive frames for vehicles it does not own, and there is no equivalent "the resource does not exist" signal because the WS is stream-oriented, not request-oriented. On REST, every vehicle-scoped path param MUST return `404 not_found` for unknown IDs.
- `invalid_request` exists only because REST accepts structured request bodies and query params that can be malformed independently of auth. The WS protocol has no v1 client->server frames that take structured payloads beyond `auth`, so malformed-body errors cannot arise there.
- `service_unavailable` is RESERVED for the REST contract so the SDK can write forward-compatible handlers before the server begins emitting it during maintenance windows.

Both `not_found` and `invalid_request` MUST be added to the shared `ErrorPayload.code` enum in [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) even though the WS never emits them, so the SDK's `CoreError` union is a single enum across both transports. This is not a drift -- the WS contract explicitly lists them as "REST-only" in the catalog description. Tracked as DV-20.

**`service_unavailable` is intentionally REST-only and is NOT promoted to the shared enum** in DV-20. The WS equivalent of a 503 maintenance window is a connection-refused close code (4003/1011), not a typed `service_unavailable` error frame. Keeping `service_unavailable` out of the shared enum preserves transport-appropriate error semantics: REST clients retry on 503+`service_unavailable`, WS clients retry on close 4003/1011 per `websocket-protocol.md` §7.1.

#### 4.1.2 Rate limiting

> **Anchored:** FR-7.1, NFR-3.6.

In v1 REST endpoints are protected by a per-user request-rate limit. The default is a PLANNED **120 requests/minute per authenticated user** (approximately two requests per second sustained, with bursts permitted via a token bucket). This cap is PLANNED and not enforced today -- tracked as DV-22.

When the cap is breached the server returns `429 rate_limited` with the standard error envelope. The response SHOULD include a `Retry-After: <seconds>` header indicating the minimum delay the SDK should wait before retrying. SDKs MUST apply exponential backoff on successive `429`s using the curve from [`websocket-protocol.md`](websocket-protocol.md) §7.1 (initial 1 s, multiplier 2x, max 30 s, +/- 25% jitter).

The REST rate limit is **independent** of the WebSocket `MaxConnectionsPerUser` per-user concurrent-connection cap (WS DV-08). A user may have 5 open WebSocket sessions AND 120 REST requests/min simultaneously. This is intentional: the two limits protect different exhaustion modes (concurrent holdings vs request flood).

Unlike the WebSocket `rate_limited` error, REST `rate_limited` in v1 does **not** emit a `subCode: device_cap` -- `device_cap` is specific to the per-user concurrent-connection cap on the WS path. REST rate-limit breaches surface to the UI as a generic "too many requests" signal; per-device UX messaging is not part of v1 REST.

### 4.2 Pagination

> **Anchored:** FR-9.1, FR-9.2.

#### 4.2.1 Cursor-based

REST list endpoints use **cursor-based pagination**, not offset-based. Cursors are opaque base64-encoded strings. The SDK and any other client MUST treat the cursor as an opaque token: parse it, mutate it, or infer anything from its contents and the server is free to break your code on the next deployment. The server reserves the right to change the cursor encoding (key type, signing scheme, version prefix) without a contract version bump.

Clients request pagination via query parameters:

| Parameter | Type | Default | Constraints | Description |
|-----------|------|---------|-------------|-------------|
| `limit` | integer | `20` | min 1, max 100 | Maximum number of items to return in this page. Requests with `limit` outside `[1, 100]` return `400 invalid_request`. |
| `cursor` | string | absent | opaque base64 | Page anchor returned by a prior request's `nextCursor`. Absent on the first page. Malformed cursors return `400 invalid_request`. |

The server responds with a wrapped list:

```json
{
  "items": [...],
  "nextCursor": "eyJsYXN0SWQiOiJjbHh5ejEyMzQ1Njc4OTBkcnYwMDEifQ==",
  "hasMore": true
}
```

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `items` | array | Yes | Zero or more items of the resource type. Empty array when there are no results (NOT `null`). |
| `nextCursor` | string \| null | Yes | Cursor to pass on the next request to retrieve the following page. `null` when the final page has been reached. |
| `hasMore` | boolean | Yes | `true` iff more pages exist (i.e., `nextCursor` is non-null). Provided as a redundant convenience so callers don't have to null-check `nextCursor`. |

#### 4.2.2 Stable ordering

Paginated endpoints MUST return items in a deterministic order. For drives, the ordering is:

```
ORDER BY startTime DESC, id DESC
```

Ordering by `startTime DESC` alone is ambiguous when multiple drives share the same millisecond boundary (rare but possible when a simulator replay creates bulk records). The secondary `id DESC` tiebreaker is a compound key that guarantees a total order, which is required for cursor stability -- a cursor encodes `(startTime, id)` so pagination can resume from a known position without skipping or repeating items.

Drives older than 365 days are pruned by the background retention job per NFR-3.27 (see [`data-lifecycle.md`](data-lifecycle.md) §5). A paginated scan that started before a prune and resumed after it will observe items disappearing from the tail of the list -- this is acceptable. `hasMore` and `nextCursor` continue to reflect the current state of the table.

#### 4.2.3 One-shot + reactive pairing (FR-9.1, FR-9.2)

The REST drive-history endpoint is the "one-shot paginated fetch" half of FR-9.1. Its pair is the WebSocket `drive_ended` reactive subscription (`websocket-protocol.md` §4.3 and `schemas/ws-messages.schema.json` `DriveEndedPayload`). SDK consumers render the drive list by:

1. Fetching the first page via `GET /api/vehicles/{vehicleId}/drives` on cold load (REST).
2. Paginating backwards on scroll via `nextCursor` (REST).
3. Prepending newly-completed drives to the in-memory list when a `drive_ended` WebSocket frame arrives (WS -- no re-fetch required). This satisfies FR-9.2.
4. For a drive the consumer wants to inspect in detail (tap-through), calling `GET /api/drives/{driveId}` and `GET /api/drives/{driveId}/route` on demand.

This pattern is the blueprint for every "history + live update" surface in the SDK. It avoids re-fetching the full list after every live event (which would waste cellular bandwidth on watchOS per NFR-3.36) while keeping the UI consistent across the REST snapshot boundary.

### 4.3 Versioning

The REST surface is mounted at `/api` with no version prefix in v1. This matches the `/api/ws` WebSocket path: neither surface embeds a version in its URL.

**No simultaneous protocol versions.** Per NFR-3.40, protocol-level multi-versioning (v1 and v2 served simultaneously) is a v3+ concern, not v1. When a breaking change is required in v2, the server will introduce a versioned prefix (e.g., `/api/v2/...`) alongside the existing unversioned `/api/...` path and deprecate the latter on the [`NFR-3.37`](../architecture/requirements.md) schedule. v1 SDKs pointed at the v1 path continue to function while v2 SDKs adopt the v2 path.

**Deprecation signal.** When an endpoint is deprecated, the server MUST return a `Deprecation: true` response header (RFC 8594) and a `Sunset: <HTTP-date>` header indicating the earliest date the endpoint may be removed. No endpoints are deprecated in v1.

### 4.4 Request and response headers

| Header | Direction | Required | Notes |
|--------|-----------|----------|-------|
| `Authorization: Bearer <token>` | client -> server | Yes | §3 |
| `Content-Type: application/json; charset=utf-8` | both | On bodies | §2.2 |
| `Accept: application/json` | client -> server | SHOULD | Clients should signal JSON preference explicitly. |
| `X-Request-ID` | both | Optional | If the client sends a request ID, the server echoes it back on the response and includes it in every slog / OTel span emitted during that request. Enables end-to-end correlation across the SDK, the REST middleware, and the store layer. If the client does not send one, the server generates a random request ID. |
| `Retry-After: <seconds>` | server -> client | On 429 / 503 | Advisory backoff hint. |
| `Deprecation: true` + `Sunset: <HTTP-date>` | server -> client | On deprecated endpoints | §4.3. No endpoints are deprecated in v1. |

No consumer-facing headers beyond these are part of the v1 contract. Standard observability headers (e.g., `traceparent` for W3C Trace Context) flow through the middleware as documented in §9.

### 4.5 Idempotency

v1 REST uses HTTP method semantics as the idempotency boundary. `GET` is always idempotent. `DELETE` is idempotent in the "equivalent final state" sense (see below). `POST` is NOT idempotent by default.

| Method + Endpoint | Idempotency | Notes |
|-------------------|-------------|-------|
| `GET /api/vehicles/{vehicleId}/snapshot` | Yes (naturally) | Always returns current state. |
| `GET /api/vehicles/{vehicleId}/drives` | Yes (naturally) | Paginated; stable ordering means a repeat call returns equivalent pages modulo new drives arriving at the head of the list. |
| `GET /api/drives/{driveId}` | Yes (naturally) | Immutable record once the drive completes. |
| `GET /api/drives/{driveId}/route` | Yes (naturally) | Immutable payload. |
| `GET /api/vehicles/{vehicleId}/invites` | Yes (naturally) | Read-only. |
| `POST /api/vehicles/{vehicleId}/invites` | **No** | Without client-supplied deduplication, a retry after a network blip MAY create two invites for the same email. The consumer is responsible for handling this (show an error UI, let the user retry manually, de-duplicate on the UI side). A server-side `Idempotency-Key` header is NOT part of v1 -- tracked as a future enhancement if usage warrants it. |
| `DELETE /api/invites/{inviteId}` | Yes (equivalent final state) | Deleting an already-deleted invite returns `404 not_found` on the second call. Clients that need "delete or already deleted" semantics SHOULD treat `404 not_found` on a DELETE as an acceptable terminal state rather than an error. |
| `DELETE /api/users/me` | Yes (equivalent final state) | After the first successful call the user's token is invalidated; the second call returns `401 auth_failed` because the token no longer resolves to a valid user. The final state is "account deleted" in both cases. |

**Why no `Idempotency-Key` in v1.** The only non-idempotent endpoint is `POST /api/vehicles/{vehicleId}/invites`. In v1 the cost of a double-invite (two rows in the Invite table) is bounded: the owner can revoke either via `DELETE /api/invites/{inviteId}`. The cost of shipping a server-side idempotency key store (Redis or a dedicated table) is higher than the cost of the occasional duplicate invite at v1 scale (NFR-3.6: 1,000 users). If the invite UX suffers, a follow-up can add an `Idempotency-Key` header following RFC draft conventions.

---

## 5. RBAC and field masks

> **Anchored:** NFR-3.19, NFR-3.20, FR-5.4, FR-5.5.

v1 defines two roles:

| Role | Read | Write | Reference |
|------|------|-------|-----------|
| `owner` | Full | Full (create/delete invites, delete account) | FR-5.4 |
| `viewer` | Full read of the vehicle's live state, drive history, and route playback | None | FR-5.4 |

The third architectural slot `limited_viewer` is NOT a v1 role but is kept available as an extension seam per FR-5.5. The masking machinery below is defined as a static per-role projection applied at the store layer, so adding a third role is a one-file change (a new mask entry) rather than an architectural change.

### 5.1 Masking rule

Every REST response MUST be projected through the caller's role mask **server-side** before being written to the response body. No raw fan-out to callers (NFR-3.19 is about the WS path; NFR-3.20 extends the same rule to REST). The mask is applied in the store layer -- the REST handler receives an already-masked object from the repository.

### 5.2 Per-resource masks

#### 5.2.1 Vehicle snapshot (`GET /api/vehicles/{vehicleId}/snapshot`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` | All fields in [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) | Including GPS, nav, charge, gear -- the full v1 `VehicleState` shape. |
| `viewer` | All fields EXCEPT `licensePlate` | **Note:** `licensePlate` is a Prisma-owned column per [`data-classification.md`](data-classification.md) §1.3 and is NOT currently a member of `vehicle-state.schema.json`, so this mask rule is **forward-looking**: it codifies the behavior the first time `licensePlate` is surfaced over the SDK. Viewers retain full GPS, nav, and charge visibility because the whole point of sharing is to watch the vehicle in real time (FR-5.1, FR-5.4). |
| `limited_viewer` (FR-5.5 future slot) | All fields EXCEPT `licensePlate`, `navRouteCoordinates`, `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`; `latitude`/`longitude` reduced to a coarse-grained hash (city-block resolution) | Documented here as the extension seam for FR-5.5. NOT implemented in v1. The mask is a static per-role projection; adding the `limited_viewer` row is a one-file store-layer change. |

#### 5.2.2 Drive list (`GET /api/vehicles/{vehicleId}/drives`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner`, `viewer` | `id`, `vehicleId`, `startTime`, `endTime`, `date`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `startChargeLevel`, `endChargeLevel`, `createdAt` | The field set is identical for both roles. Viewers are read-only (FR-5.4); they observe the same data as owners but cannot create, delete, or modify any drive record. |

`startAddress`, `startLocation`, `endAddress`, and `endLocation` are **deliberately omitted from the list payload** and are only returned by the drive detail endpoint. Rationale: list payloads are the most frequently fetched resource, and keeping them lean reduces the bytes on the wire per page, reduces the P1 surface area per request (addresses are P1 per `data-classification.md` §1.4), and keeps the drive list response under ~5 KB per page at typical drive counts.

#### 5.2.3 Drive detail (`GET /api/drives/{driveId}`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner`, `viewer` | All FR-3.4 stats: `id`, `vehicleId`, `startTime`, `endTime`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `energyUsedKwh`, `startChargeLevel`, `endChargeLevel`, `fsdMiles`, `fsdPercentage`, `interventions`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `createdAt` | Both roles see the full record including P1 start/end location and address. Rationale: the owner expects their own data, and the viewer has explicit consent via the invite they accepted. Denying viewers the start/end address would defeat the sharing use case (FR-5.1) -- knowing "the drive ended at the airport" is the point. |

Does NOT include `routePoints` -- those are returned by the separate `GET /api/drives/{driveId}/route` endpoint (heavy payload; see §7.4 for the lazy-fetch rationale).

#### 5.2.4 Drive route (`GET /api/drives/{driveId}/route`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner`, `viewer` | Full `routePoints` array | Both roles see the full polyline. The whole sharing use case is watching someone drive home; a partial polyline would defeat FR-5.1. |

#### 5.2.5 Invite endpoints

| Endpoint | Role access | Notes |
|----------|-------------|-------|
| `GET /api/vehicles/{vehicleId}/invites` | `owner` only | Viewers who call this receive `403 permission_denied`. |
| `POST /api/vehicles/{vehicleId}/invites` | `owner` only | Same. |
| `DELETE /api/invites/{inviteId}` | `owner` only (of the vehicle the invite targets) | Same. |

Rationale: FR-5.2 and FR-5.3 assign the viewer list and revocation to owners explicitly. v1 does not support viewers inviting additional viewers.

Note on the Invite response shape: `email` is **P1** per `data-classification.md` §1.6. The response returns it to the owner (who already knows who they invited), but any future `limited_viewer` who gains read access to invite metadata would have this field masked out. Since v1 only owners can hit invite endpoints at all, this masking is moot today; it is documented here for FR-5.5 readiness.

#### 5.2.6 Account deletion

| Endpoint | Role access | Notes |
|----------|-------------|-------|
| `DELETE /api/users/me` | Self only | The authenticated user can delete only their own account. There is no admin deletion, no cross-user deletion, no "delete all viewers of my vehicle" operation. |

### 5.3 Extension seam for a third role (FR-5.5)

The RBAC masking machinery is implemented as a static lookup table keyed by `(resourceType, role)` at the store layer. Adding a new role is a three-step change:

1. Add the role name to the `Role` enum in the store layer.
2. Add mask entries for each resource type that the new role should see (or inherit from `viewer` with a diff).
3. Wire the role into the `Authenticator.ResolveRole(userId, vehicleId)` call site.

No contract changes are required for the new role's wire shape (the REST response schemas already cover every field; the new role simply sees fewer of them). This satisfies FR-5.5's "architecture MUST support adding a third role without schema changes."

---

## 6. Endpoint catalog summary

| Method | Path | Purpose | Auth | Anchored FRs/NFRs |
|--------|------|---------|------|-------------------|
| `GET` | `/api/vehicles/{vehicleId}/snapshot` | Cold-load full VehicleState | Bearer + owner-or-viewer of vehicleId | FR-1.1, FR-1.2, FR-2.1, NFR-3.5, NFR-3.11 |
| `GET` | `/api/vehicles/{vehicleId}/drives` | Paginated drive history for vehicle | Bearer + owner-or-viewer of vehicleId | FR-3.2, FR-9.1, FR-9.2 |
| `GET` | `/api/drives/{driveId}` | Single drive detail (FR-3.4 stats + start/end addresses) | Bearer + owner-or-viewer of drive's vehicle | FR-3.4, FR-9.1 |
| `GET` | `/api/drives/{driveId}/route` | Full GPS polyline for drive playback | Bearer + owner-or-viewer of drive's vehicle | FR-3.3, NFR-3.23 |
| `POST` | `/api/vehicles/{vehicleId}/invites` | Create sharing invite | Bearer + owner of vehicleId | FR-5.1 |
| `GET` | `/api/vehicles/{vehicleId}/invites` | List viewers + pending invites | Bearer + owner of vehicleId | FR-5.2 |
| `DELETE` | `/api/invites/{inviteId}` | Revoke invite | Bearer + owner of invite's vehicle | FR-5.3 |
| `DELETE` | `/api/users/me` | Delete own account + all data | Bearer (self only) | FR-10.1, FR-10.2, NFR-3.29 |

All paths are PLANNED; none are mounted by the Go server today (DV-20). See §10.

---

## 7. Endpoint reference

### 7.1 `GET /api/vehicles/{vehicleId}/snapshot`

> **Anchored:** NFR-3.5, NFR-3.11, FR-1.1, FR-1.2, FR-2.1.

#### Purpose

Returns the full current `VehicleState` for a single vehicle. This is the cold-load snapshot the SDK fetches on initial page render (target: < 500 ms end-to-end per the latency table in `requirements.md` §3.1) and on every reconnect per NFR-3.11 (see `websocket-protocol.md` §7.2 reconnect sequence). The snapshot is the DB source-of-truth (see [`data-lifecycle.md`](data-lifecycle.md) §1.2); the WebSocket is the real-time channel. An SDK built on this contract never shows a per-field loading spinner -- the snapshot response is always complete enough to render the full UI (NFR-3.5, NFR-3.6).

#### Request

```
GET /api/vehicles/{vehicleId}/snapshot HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

| Parameter | Location | Type | Required | Notes |
|-----------|----------|------|----------|-------|
| `vehicleId` | path | string (cuid) | Yes | Opaque DB ID (FR-4.2). Never the VIN. |

#### Response -- 200 OK

The body is a `VehicleState` object whose shape is defined by [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json). The OpenAPI spec at [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml) references this schema via `$ref` -- it is NOT re-declared in this doc.

Example:

```json
{
  "vehicleId": "clxyz1234567890abcdef",
  "name": "Stumpy",
  "model": null,
  "year": null,
  "color": null,
  "status": "parked",
  "speed": 0,
  "heading": 180,
  "latitude": 10.0,
  "longitude": 20.0,
  "locationName": null,
  "locationAddress": null,
  "gearPosition": "P",
  "chargeLevel": 78,
  "chargeState": "Disconnected",
  "estimatedRange": 245,
  "timeToFull": null,
  "interiorTemp": 68,
  "exteriorTemp": 55,
  "odometerMiles": 12458,
  "fsdMilesSinceReset": null,
  "destinationName": null,
  "destinationAddress": null,
  "destinationLatitude": null,
  "destinationLongitude": null,
  "originLatitude": null,
  "originLongitude": null,
  "etaMinutes": null,
  "tripDistanceRemaining": null,
  "navRouteCoordinates": null,
  "lastUpdated": "2026-04-13T18:22:01Z"
}
```

Spec-only fields (MYR-24) are returned as `null` per [`vehicle-state-schema.md`](vehicle-state-schema.md) §1.1. The charge-group fields `chargeState` and `timeToFull` may be returned as `null` until the cross-repo Prisma DB-persistence follow-up to [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) lands (the Prisma-owned `Vehicle` table does not yet have these columns). MYR-40 shipped the live WS wire path for both fields on 2026-04-22, so `vehicle_update` WebSocket frames carry real values even while this snapshot remains transitional-null. See `websocket-protocol.md` §4.1.4 and §10 DV-03 / DV-04.

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `vehicle_not_owned` | Caller is not the owner or an accepted viewer of `vehicleId` |
| 404 | `not_found` | `vehicleId` does not exist (or is not visible to the caller -- intentionally indistinguishable) |
| 429 | `rate_limited` | REST rate limit breached (§4.1.2) |
| 500 | `internal_error` | Store-layer error, decryption failure, etc. |

#### RBAC

See §5.2.1. Owners see the full `VehicleState`; viewers see all current fields (licensePlate mask is forward-looking because it is not yet a member of `VehicleState`).

#### Implementation notes

- The server MUST NOT return `undefined` for missing fields -- all fields specified as required in `vehicle-state.schema.json` are always present. Nullable fields are present with an explicit `null`.
- Decryption of P1 coordinate columns (lat/lng, destination lat/lng, origin lat/lng, navRouteCoordinates) happens in the store layer per NFR-3.25 before the handler sees the object. The SDK never sees ciphertext.
- Latency target: p95 < 500 ms from auth-resolved to response-written per `requirements.md` §3.1.

---

### 7.2 `GET /api/vehicles/{vehicleId}/drives`

> **Anchored:** FR-3.2, FR-9.1, FR-9.2.

#### Purpose

Returns a paginated list of completed drives for a vehicle, newest first, suitable for the SDK's drive-history scroll view. This is the "one-shot paginated fetch" half of FR-9.1; the reactive half is the WebSocket `drive_ended` subscription.

#### Request

```
GET /api/vehicles/{vehicleId}/drives?limit=20&cursor=<opaque> HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

| Parameter | Location | Type | Required | Default | Notes |
|-----------|----------|------|----------|---------|-------|
| `vehicleId` | path | string (cuid) | Yes | -- | Opaque DB ID (FR-4.2). |
| `limit` | query | integer | No | 20 | 1-100 inclusive. `400 invalid_request` on out-of-range. |
| `cursor` | query | string (opaque base64) | No | absent | Returned by a prior response's `nextCursor`. Absent on first page. `400 invalid_request` on malformed. |

#### Response -- 200 OK

```json
{
  "items": [
    {
      "id": "clmno9876543210zyxw0001",
      "vehicleId": "clxyz1234567890abcdef",
      "startTime": "2026-04-13T18:22:00Z",
      "endTime": "2026-04-13T18:46:18Z",
      "date": "2026-04-13",
      "distanceMiles": 12.4,
      "durationSeconds": 1458,
      "avgSpeedMph": 30.5,
      "maxSpeedMph": 65.2,
      "startChargeLevel": 82,
      "endChargeLevel": 76,
      "createdAt": "2026-04-13T18:46:19Z"
    },
    {
      "id": "clmno9876543210zyxw0002",
      "vehicleId": "clxyz1234567890abcdef",
      "startTime": "2026-04-13T08:14:00Z",
      "endTime": "2026-04-13T08:29:07Z",
      "date": "2026-04-13",
      "distanceMiles": 5.1,
      "durationSeconds": 907,
      "avgSpeedMph": 20.2,
      "maxSpeedMph": 42.0,
      "startChargeLevel": 85,
      "endChargeLevel": 82,
      "createdAt": "2026-04-13T08:29:08Z"
    }
  ],
  "nextCursor": "eyJzdGFydFRpbWUiOiIyMDI2LTA0LTEzVDA4OjE0OjAwWiIsImlkIjoiY2xtbm85ODc2NTQzMjEwenl4dzAwMDIifQ==",
  "hasMore": true
}
```

Each `items[i]` is a `DriveSummary` object as defined in §8 and in the OpenAPI spec. The summary deliberately omits `startAddress`, `startLocation`, `endAddress`, `endLocation`, `energyUsedKwh`, `fsdMiles`, `fsdPercentage`, `interventions`, and `routePoints` -- those are available via drive detail (§7.3) and drive route (§7.4). See §5.2.2 for the RBAC rationale.

#### Ordering

Drives are ordered `startTime DESC, id DESC` per §4.2.2. The cursor encodes both fields so pagination is stable across concurrent writes.

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | `limit` out of range or malformed `cursor` |
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `vehicle_not_owned` | Caller has no access to `vehicleId` |
| 404 | `not_found` | `vehicleId` does not exist (or is not visible) |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error |

#### RBAC

See §5.2.2. Owners and viewers see the same field set.

#### FR-9.1 / FR-9.2 pairing

The SDK's drive-history UI is hydrated by:

1. An initial `GET /api/vehicles/{vehicleId}/drives` call (REST, this endpoint) to populate the first page.
2. Subsequent paginated scrolls that call this endpoint again with `nextCursor`.
3. A WebSocket subscription to `drive_ended` messages that the SDK prepends to the in-memory list as new drives complete (see [`websocket-protocol.md`](websocket-protocol.md) §4.3).

The SDK MUST NOT re-fetch the list when a `drive_ended` frame arrives -- the frame carries enough data to synthesize a `DriveSummary` for prepending, and the SDK can later call `GET /api/drives/{driveId}` on tap-through to fetch the full record lazily. This is the FR-9.1 / FR-9.2 contract: snapshot + reactive subscription, no redundant fetches.

#### Retention

Drives older than 365 days are pruned by the background retention job per NFR-3.27 (see [`data-lifecycle.md`](data-lifecycle.md) §5). A cursor scan that straddles a prune event MAY observe items disappearing from the tail -- this is acceptable.

---

### 7.3 `GET /api/drives/{driveId}`

> **Anchored:** FR-3.4, FR-9.1.

#### Purpose

Returns the full FR-3.4 stats for a single completed drive. This is the endpoint invoked by the SDK's `fetchDrive(driveId)` helper paired with the `drive_ended` WebSocket message (see `websocket-protocol.md` §4.3 and the DV-11 resolution). The drive-ended wire payload is deliberately a summary; the full record lives here.

#### Request

```
GET /api/drives/{driveId} HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

| Parameter | Location | Type | Required | Notes |
|-----------|----------|------|----------|-------|
| `driveId` | path | string (cuid) | Yes | Matches the `driveId` carried by `drive_started` and `drive_ended` WebSocket frames. |

#### Response -- 200 OK

```json
{
  "id": "clmno9876543210zyxw0001",
  "vehicleId": "clxyz1234567890abcdef",
  "startTime": "2026-04-13T18:22:00Z",
  "endTime": "2026-04-13T18:46:18Z",
  "date": "2026-04-13",
  "distanceMiles": 12.4,
  "durationSeconds": 1458,
  "avgSpeedMph": 30.5,
  "maxSpeedMph": 65.2,
  "energyUsedKwh": 4.2,
  "startChargeLevel": 82,
  "endChargeLevel": 76,
  "fsdMiles": 8.1,
  "fsdPercentage": 65.3,
  "interventions": 1,
  "startLocation": "Location A",
  "startAddress": "synthetic-start-address",
  "endLocation": "Location B",
  "endAddress": "synthetic-end-address",
  "createdAt": "2026-04-13T18:46:19Z"
}
```

This is a `DriveDetail` object as defined in §8 and in the OpenAPI spec. It contains every FR-3.4 field EXCEPT `routePoints`, which is returned by the separate `GET /api/drives/{driveId}/route` endpoint.

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `vehicle_not_owned` | Caller has no access to the drive's vehicle |
| 404 | `not_found` | `driveId` does not exist (or is not visible) |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error, decryption failure |

#### RBAC

See §5.2.3. Owners and viewers see the same field set including start/end location and address, because the viewer has explicit consent via the invite they accepted.

---

### 7.4 `GET /api/drives/{driveId}/route`

> **Anchored:** FR-3.3, NFR-3.23.

#### Purpose

Returns the full GPS polyline for a drive as an array of `RoutePoint` records suitable for rendering on a map. The polyline is encrypted at rest (NFR-3.23) via AES-256-GCM column-level encryption on `Drive.routePoints` (see [`data-classification.md`](data-classification.md) §1.5), decrypted in the store layer per NFR-3.25 before the handler sees it, and transported plaintext over TLS (NFR-3.22). The SDK never sees ciphertext.

#### Request

```
GET /api/drives/{driveId}/route HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

| Parameter | Location | Type | Required | Notes |
|-----------|----------|------|----------|-------|
| `driveId` | path | string (cuid) | Yes | |

#### Response -- 200 OK

```json
{
  "driveId": "clmno9876543210zyxw0001",
  "routePoints": [
    { "lat": 10.0000, "lng": 20.0000, "speed": 0,  "heading": 180, "timestamp": "2026-04-13T18:22:00Z" },
    { "lat": 10.0002, "lng": 20.0003, "speed": 15, "heading": 175, "timestamp": "2026-04-13T18:22:03Z" },
    { "lat": 10.0005, "lng": 20.0007, "speed": 22, "heading": 170, "timestamp": "2026-04-13T18:22:06Z" }
  ]
}
```

Each `RoutePoint` matches the `RoutePointRecord` shape from [`data-classification.md`](data-classification.md) §1.5: `{lat, lng, speed, heading, timestamp}`. The `lat` and `lng` fields are classified P1; the sub-fields `speed`, `heading`, and `timestamp` are P0 in isolation but are encrypted at rest alongside the parent polyline column (NFR-3.23).

#### Payload size and lazy-fetch guidance

A 60-minute drive captured at 1 Hz is approximately 3,600 points, which serializes to roughly 200-300 KB of JSON. This is well below any mobile OS memory-pressure threshold -- even watchOS can hold a single drive's polyline in memory without issue.

The lazy-fetch guidance below is **about cellular bandwidth and perceived latency, not heap pressure**:

- The SDK SHOULD fetch this endpoint lazily (on user tap of a drive's detail view), not eagerly for every drive in the list.
- Eager pre-fetching of every drive's route would waste cellular bandwidth on every drive-list render, which is particularly bad on watchOS per NFR-3.36 (aggressive lifecycle handling, short-lived launches, incremental state hydration).
- The SDK MAY fetch the route for the top 1-3 drives as an optimistic prefetch when the drive list is cold-loaded on WiFi, and MUST NOT prefetch on cellular.

This is explicitly NOT an OOM concern -- a single drive's polyline fits in any v1 target runtime. The recommendation exists purely to protect data plans and perceived latency on low-bandwidth networks.

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `vehicle_not_owned` | Caller has no access to the drive's vehicle |
| 404 | `not_found` | `driveId` does not exist (or is not visible) |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error, decryption failure |

#### RBAC

See §5.2.4. Owners and viewers see the full polyline; denying viewers would defeat FR-5.1.

---

### 7.5 Invite endpoints

> **Anchored:** FR-5.1, FR-5.2, FR-5.3, FR-5.4.

The Invite table is **Prisma-owned** per [`data-classification.md`](data-classification.md) §1.6 and [`data-lifecycle.md`](data-lifecycle.md) §1.4. No `InviteRepo` exists in `internal/store/` today. The three invite endpoints below are PLANNED for v1 -- see §10 DV-23. There are two implementation paths:

1. **Go telemetry server owns the endpoints.** The telemetry server adds an `InviteRepo` to `internal/store/` that reads/writes the existing Prisma-managed Invite table, and mounts the three endpoints under `/api/...` alongside snapshot/drives/drive-route.
2. **Next.js app owns the endpoints.** The Next.js app serves the invite endpoints directly (Prisma already owns the table). The SDK still hits `https://api.myrobotaxi.com/api/...` -- the edge router proxies invite paths to the Next.js app and snapshot/drives paths to the Go telemetry server.

Either choice is compatible with this contract: **the REST contract is the SDK's source of truth regardless of where the handler runs**. The handler location is an implementation detail that the SDK does not observe. For clarity, the Linear follow-up issue (DV-23) will lock the decision and any routing configuration.

#### 7.5.1 `POST /api/vehicles/{vehicleId}/invites`

##### Purpose

Creates a sharing invite that grants the recipient (identified by email) read access to the vehicle as a `viewer`. The recipient accepts the invite out-of-band (via the Next.js app's invite-acceptance flow) and becomes an active viewer upon acceptance.

##### Request

```
POST /api/vehicles/{vehicleId}/invites HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Content-Type: application/json; charset=utf-8
Accept: application/json

{
  "label": "Viewer",
  "email": "invitee-a@example.com",
  "permission": "live_history"
}
```

| Field | Type | Required | Classification | Notes |
|-------|------|----------|----------------|-------|
| `label` | string | Yes | P0 | Display name the owner chose for the invite (e.g., "Viewer", "Shared user"). Max 64 characters. |
| `email` | string (RFC 5322 email) | Yes | P1 | Invitee's email address. Server-side validation on format. |
| `permission` | string (enum) | Yes | P0 | `live` (live state only) or `live_history` (live state + drive history). Matches the `InvitePermission` enum in `data-classification.md` §1.6. |

##### Response -- 201 Created

```json
{
  "id": "clxyz1234567890invite01",
  "vehicleId": "clxyz1234567890abcdef",
  "senderId": "clxyz1234567890userid",
  "label": "Viewer",
  "email": "invitee-a@example.com",
  "status": "pending",
  "permission": "live_history",
  "sentDate": "2026-04-14T10:00:00Z",
  "acceptedDate": null,
  "lastSeen": null,
  "isOnline": false,
  "createdAt": "2026-04-14T10:00:00Z",
  "updatedAt": "2026-04-14T10:00:00Z"
}
```

The response is a full `Invite` object as defined in §8. The `email` field is returned to the owner (who already knows who they invited); any future `limited_viewer` role would have it masked out (§5.2.5).

##### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Malformed JSON, missing required field, invalid email, invalid permission enum, label too long |
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `permission_denied` | Caller is not the owner of `vehicleId` (e.g., caller is a viewer or a non-owner) |
| 404 | `not_found` | `vehicleId` does not exist (or is not visible) |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error |

##### Idempotency

`POST` is NOT idempotent in v1 (§4.5). A retry after a network blip MAY create two invites for the same email; the owner can revoke either via `DELETE /api/invites/{inviteId}`.

#### 7.5.2 `GET /api/vehicles/{vehicleId}/invites`

##### Purpose

Returns the list of active viewers and pending invites for a vehicle.

##### Request

```
GET /api/vehicles/{vehicleId}/invites HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

##### Response -- 200 OK

```json
{
  "items": [
    {
      "id": "clxyz1234567890invite01",
      "vehicleId": "clxyz1234567890abcdef",
      "senderId": "clxyz1234567890userid",
      "label": "Viewer A",
      "email": "invitee-a@example.com",
      "status": "accepted",
      "permission": "live_history",
      "sentDate": "2026-04-01T10:00:00Z",
      "acceptedDate": "2026-04-01T11:23:00Z",
      "lastSeen": "2026-04-14T09:45:00Z",
      "isOnline": true,
      "createdAt": "2026-04-01T10:00:00Z",
      "updatedAt": "2026-04-14T09:45:00Z"
    },
    {
      "id": "clxyz1234567890invite02",
      "vehicleId": "clxyz1234567890abcdef",
      "senderId": "clxyz1234567890userid",
      "label": "Viewer B",
      "email": "invitee-b@example.com",
      "status": "pending",
      "permission": "live_history",
      "sentDate": "2026-04-14T10:00:00Z",
      "acceptedDate": null,
      "lastSeen": null,
      "isOnline": false,
      "createdAt": "2026-04-14T10:00:00Z",
      "updatedAt": "2026-04-14T10:00:00Z"
    }
  ]
}
```

**Not paginated in v1.** The response is a simple `{items: Invite[]}` object without `nextCursor` / `hasMore`. Rationale: typical viewer counts per vehicle are small (1-10), well below any reasonable page size. If a future use case requires pagination, an additive change (adding `nextCursor` and `hasMore` fields, unused by v1 clients) can introduce it without breaking compatibility.

##### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `permission_denied` | Caller is not the owner of `vehicleId` |
| 404 | `not_found` | `vehicleId` does not exist (or is not visible) |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error |

#### 7.5.3 `DELETE /api/invites/{inviteId}`

##### Purpose

Revokes a sharing invite. If the invite was in `pending` state, it is deleted and the recipient cannot accept it. If the invite was in `accepted` state, the corresponding viewer immediately loses read access to the vehicle.

Per [`websocket-protocol.md`](websocket-protocol.md) §10 DV-09, the mid-connection ownership snapshot is stale on the WS path today -- a revoked viewer who is currently connected over the WS continues to receive broadcasts until they reconnect. Closing DV-09 is the mechanism that wires this REST endpoint's effect into the live WebSocket path. Until DV-09 ships, SDK consumers should assume that revocation takes effect on the next WS reconnect, not immediately.

##### Request

```
DELETE /api/invites/{inviteId} HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
```

| Parameter | Location | Type | Required | Notes |
|-----------|----------|------|----------|-------|
| `inviteId` | path | string (cuid) | Yes | |

##### Response -- 204 No Content

Empty body on success.

##### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `permission_denied` | Caller is not the owner of the vehicle this invite targets |
| 404 | `not_found` | `inviteId` does not exist or has already been revoked |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error |

##### Idempotency

`DELETE` is idempotent in the "equivalent final state" sense (§4.5). A second DELETE after success returns `404 not_found`; clients MAY treat this as a successful terminal state.

---

### 7.6 `DELETE /api/users/me`

> **Anchored:** FR-10.1, FR-10.2, NFR-3.29.

#### Purpose

Deletes the authenticated user and all associated data per the cascade defined in [`data-lifecycle.md`](data-lifecycle.md) §3. This is the SDK's single entry point for user-initiated data deletion per FR-10.1. The endpoint writes an immutable audit log entry before the destructive operation per FR-10.2 and the data-lifecycle contract, and the audit log entry is retained indefinitely per NFR-3.29.

#### Request

```
DELETE /api/users/me HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
```

No request body.

#### Response -- 200 OK

The deletion is executed as a **single database transaction** per [`data-lifecycle.md`](data-lifecycle.md) §3.1, with the audit log INSERT as step 1 before the cascading DELETE. v1 returns a synchronous `200 OK` response after the transaction commits:

```json
{
  "deleted": true,
  "auditLogId": "claud0g123456789deletion"
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `deleted` | boolean | P0 | Always `true` on a successful 200 response. |
| `auditLogId` | string (cuid) | P0 | Opaque ID of the AuditLog row written per FR-10.2 / NFR-3.29 / `data-lifecycle.md` §3.1 / §4. The SDK (and the web test bench) can cross-reference this ID against the audit log store to verify the write. The row itself is P0 (`data-lifecycle.md` §4.4). |

**Why synchronous (not async with a `202 Accepted` + polling).** The cascade defined in `data-lifecycle.md` §3 is a single database transaction, not a long-running background job. Returning `200 OK` after the transaction commits is simpler and avoids the complexity of a polling endpoint for a workflow that is already atomic. This decision is recorded in §7.6 of this doc and is the canonical v1 behavior; any future move to an async pipeline (e.g., to support deferred external cleanup of reverse-geocoded cache entries) would require an additive change that falls back to 200 for existing clients.

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token. Also the expected response on a second DELETE attempt after a successful first call -- the token has been invalidated. |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Transaction rolled back (the cascade failed and no data was deleted per `data-lifecycle.md` §3.4) |

**Note on 403:** This endpoint has no 403 path because it operates on `/users/me` -- the authenticated user is always "owner" of their own account. There is no cross-user deletion in v1.

#### Idempotency

Idempotent in the "equivalent final state" sense (§4.5). A second call after success returns `401 auth_failed` because the token no longer resolves to a valid user. The final state is "account deleted" in both cases.

#### Cascade reference

The full deletion cascade (User -> Account, Vehicle, Invite, Settings; Vehicle -> Drive, TripStop, Invite) and the transactional guarantees are defined normatively in [`data-lifecycle.md`](data-lifecycle.md) §3. This section does NOT re-specify them; the data-lifecycle contract is the single source of truth for the cascade and the audit log row shape.

#### Implementation notes

- The audit log row is written BEFORE the cascading DELETE, in the same transaction, per `data-lifecycle.md` §3.1.
- If the Invite endpoints end up being served from the Next.js app rather than the Go telemetry server (see §7.5 rationale), the account deletion endpoint may also run in the Next.js app layer -- the Go telemetry server today has no User repository. The SDK is unaware of which process handles the request.
- WebSocket session cleanup (`data-lifecycle.md` §3.5): after the transaction commits, the telemetry server detects the vehicle deletion on its next DB read cycle and terminates any active WebSocket connections for those vehicles. The SDK observes this as a close code 1008 or 1001 on the WS, and the next `getToken()` call will fail.

---

## 8. Resource schemas

The canonical v1 `VehicleState` schema is [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json). The REST snapshot endpoint returns that shape directly via `$ref` in the OpenAPI spec -- it is NOT re-declared.

REST-only resource shapes are declared inline in [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml) under `components/schemas` for v1. The following shapes are defined there:

| Schema | Used by | Notes |
|--------|---------|-------|
| `DriveSummary` | `GET /api/vehicles/{vehicleId}/drives` item | Subset of FR-3.4 for list rendering. |
| `DriveDetail` | `GET /api/drives/{driveId}` response | Full FR-3.4 record minus `routePoints`. |
| `DriveRoute` | `GET /api/drives/{driveId}/route` response | `{driveId, routePoints[]}`. |
| `RoutePoint` | `DriveRoute.routePoints[]` item | `{lat, lng, speed, heading, timestamp}` matching `RoutePointRecord` from [`data-classification.md`](data-classification.md) §1.5. |
| `Invite` | All three invite endpoints | Full row from `data-classification.md` §1.6. |
| `CreateInviteRequest` | `POST /api/vehicles/{vehicleId}/invites` request body | `{label, email, permission}`. |
| `ErrorEnvelope` | All non-2xx responses | `{error: {code, message, subCode?}}`. |
| `PaginatedDrives` | `GET /api/vehicles/{vehicleId}/drives` response | `{items, nextCursor, hasMore}` wrapper. |
| `PaginatedInvites` | `GET /api/vehicles/{vehicleId}/invites` response | `{items}` without cursor (unpaginated in v1). |
| `DeleteUserResponse` | `DELETE /api/users/me` response | `{deleted, auditLogId}`. |

Every field in every inline schema carries an `x-classification` annotation (P0, P1, P2, or `mixed`) matching the convention in `schemas/ws-messages.schema.json` and `vehicle-state.schema.json`. This is non-negotiable -- `contract-guard` CG-DC-1 runs against these shapes too.

A follow-up issue (to be filed by DV-23's resolver) will extract these shapes to sibling JSON Schemas under `schemas/` so they can be `$ref`'d by both the OpenAPI spec and any future SDK code generator without embedding them in the YAML. The v1 scope keeps them inline to ship MYR-12 tightly.

---

## 9. Observability

> **Anchored:** FR-11.x (SDK side; REST endpoints share the same emission points as the WS surface).

REST endpoints emit the same slog / Prometheus / OpenTelemetry signals as the WebSocket surface. The existing `requestLogger` middleware in [`internal/server/middleware.go`](../../internal/server/middleware.go) already records `method`, `path`, `status`, `duration`, and `remote_addr` on every request; this is the emission point for REST observability. Additional middleware hooks PLANNED as part of the REST handler implementation (DV-19):

| Signal | Type | Labels / attributes | Notes |
|--------|------|---------------------|-------|
| `http_requests_total` | counter | `method`, `path_template`, `status_class`, `role` | Prometheus counter. `path_template` is the pattern (e.g., `/api/vehicles/:id/snapshot`), not the concrete path, to avoid cardinality explosion. `role` is `owner`, `viewer`, or `unauthenticated`. |
| `http_request_duration_seconds` | histogram | same | Prometheus histogram. SLO targets derive from `requirements.md` §3.1 latency table. |
| `http_errors_total` | counter | `method`, `path_template`, `error_code` | Cardinality bounded by the error code enum in §4.1.1. |
| slog `http request` | structured log | `method`, `path`, `status`, `duration`, `remote_addr`, `user_id`, `request_id`, `role` | Existing emission, extended to include `user_id`, `request_id`, and `role` after auth middleware resolves them. `user_id` is P0 (opaque cuid). NEVER log the `Authorization` header or any P1 field. |
| OTel span | trace | `http.method`, `http.route`, `http.status_code`, `user.id`, `vehicle.id` | W3C Trace Context propagated via the `traceparent` header. |

`contract-guard` Rule CG-DC-2 blocks any PR that introduces P1 values into log fields, error messages, or metric labels. The `vehicleId` / `driveId` / `userId` / `inviteId` IDs are P0 and are log-safe; the underlying sensitive values (GPS, addresses, tokens, emails) are P1 and MUST NOT appear in any observability output.

---

## 10. Code <-> spec divergences

This section is the canonical catalogue of every known gap between this contract and the current `internal/server/` / `internal/store/` implementation. Every entry has a proposed Linear follow-up title. `contract-guard` treats any un-catalogued divergence as a failing contract violation. The divergence IDs (DV-NN) are stable -- new divergences take the next free number; closed divergences retain their ID in the change log. Divergence IDs DV-01 through DV-18 are owned by [`websocket-protocol.md`](websocket-protocol.md) §10; MYR-12 adds DV-19 through DV-23.

### Status legend

See [`websocket-protocol.md`](websocket-protocol.md) §10 status legend -- same meanings (RESOLVED, RESOLVED (wiring pending), Requirement amendment pending, Open, New, Open (reduced)).

### Catalogue

| ID | Status | Topic | Current behavior | Target behavior | Anchor | Proposed Linear issue title |
|----|--------|-------|------------------|-----------------|--------|------------------------------|
| **DV-19** | **New** | REST auth middleware | [`internal/server/server.go`](../../internal/server/server.go) wires a `requestLogger` middleware across the client mux but has NO authentication middleware for REST endpoints. The existing `/api/vehicle-status/{vin}` and `/api/fleet-config/{vin}` handlers perform their own ad-hoc validation. The SDK's REST surface needs a shared middleware that parses `Authorization: Bearer <token>`, calls the same `Authenticator` used by the WS handler, resolves the user's vehicle ownership set, and emits observability signals. | Add a `restAuthMiddleware(Authenticator)` in `internal/server/middleware.go` (or a new file) that: (1) parses the header, (2) validates via `Authenticator.ValidateToken`, (3) loads the user's vehicles via `GetUserVehicles`, (4) puts `userId` and `vehicleIDs` in the request context, (5) returns `401 auth_failed` / `401 auth_timeout` on failure with the error envelope from §4.1, (6) strips the Authorization header from the slog `http request` line. Wire this middleware in front of every `/api/...` handler except the existing Tesla-owned endpoints. | FR-6.1, FR-6.2, NFR-3.21, §3 | `MYR-XX Add REST auth middleware + error envelope to internal/server` |
| **DV-20** | **New** | SDK-surface REST endpoints not yet mounted | None of the six endpoints in §7 are mounted by the Go server. `cmd/telemetry-server/main.go` registers only `/api/ws`, `/api/vehicle-status/{vin}`, and `/api/fleet-config/{vin}`. The snapshot / drives / drive-detail / drive-route / invite / user-deletion paths return `404 not found` from the placeholder catch-all handler today. | Implement handlers for each endpoint in §6 / §7. Order of implementation: (1) `GET /api/vehicles/{vehicleId}/snapshot` (simplest -- existing VehicleRepo + decryption), (2) `GET /api/drives/{driveId}` (existing DriveRepo.GetByID), (3) `GET /api/drives/{driveId}/route` (existing routePoints column + decryption), (4) `GET /api/vehicles/{vehicleId}/drives` (requires new DriveRepo.List method with cursor-based pagination), (5) Invite endpoints (depend on DV-23 decision), (6) `DELETE /api/users/me` (depends on Next.js-app decision per §7.6). Also add the REST-only error codes `not_found` and `invalid_request` to the shared `ErrorPayload.code` enum in `schemas/ws-messages.schema.json` even though the WS never emits them, so the SDK's `CoreError` is a single union. | FR-3.2, FR-3.3, FR-3.4, FR-5.x, FR-10.1, NFR-3.5, §6, §7 | `MYR-XX Mount SDK-surface REST endpoints (snapshot, drives, invites, user deletion)` |
| **DV-21** | **New** | `service_unavailable` code reserved but not emitted | v1 does not emit `503 service_unavailable`. The code is reserved in this contract for forward-compat. | Server begins emitting `503 service_unavailable` during maintenance windows and graceful-shutdown states, with a `Retry-After` header. SDK error catalog already recognizes the code from day one. | NFR-3.10, §4.1.1 | `MYR-XX Emit 503 service_unavailable during graceful shutdown + maintenance` |
| **DV-22** | **New** | REST rate limit not enforced | No per-user REST rate limit is configured in [`internal/config/defaults.go`](../../internal/config/defaults.go) or wired through the server. The 120 req/min target in §4.1.2 is a PLANNED default, not an enforced value. | Add `WebSocketConfig.RestRateLimitPerMinutePerUser` (default 120) in `internal/config/defaults.go`. Implement a token-bucket rate limiter in the REST middleware keyed by `userId`. Breach returns `429 rate_limited` with a `Retry-After` header. Independent of `MaxConnectionsPerUser` (which governs concurrent WS sessions, not REST rps). | NFR-3.6, §4.1.2 | `MYR-XX Implement per-user REST rate limit (120 req/min default)` |
| **DV-23** | **New** | Invite endpoints handler location + `InviteRepo` | The Invite table is Prisma-owned per `data-classification.md` §1.6; `internal/store/` has no `InviteRepo`. The three invite endpoints (§7.5) are PLANNED with two compatible implementation paths: (1) add an `InviteRepo` to the Go telemetry server that reads the Prisma-managed Invite table, or (2) serve the invite endpoints from the Next.js app with edge routing. | Lock the implementation path in a dedicated Linear issue. Either path is compatible with this contract -- the SDK calls `https://api.myrobotaxi.com/api/vehicles/{id}/invites` regardless of where the handler runs. Recommendation: option (1) so all SDK-surface REST lives in one process, but (2) is acceptable if the Next.js app already has invite lifecycle UI. Document the choice as a RESOLVED row and file the implementation issue. | FR-5.1, FR-5.2, FR-5.3, §7.5 | `MYR-XX Decide Invite endpoint handler location (Go InviteRepo vs Next.js) + implement` |

### Divergence management rules

Same as [`websocket-protocol.md`](websocket-protocol.md) §10 divergence management rules (one-way door, closing rules, RESOLVED-with-implementation-pending, amendment divergences). DV-NN IDs are globally unique across both catalogues -- MYR-12 intentionally starts at DV-19 to avoid collision with the DV-01 through DV-18 IDs owned by the WebSocket contract.

---

## 11. Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-14 | Initial full draft (MYR-12): §2 transport, §3 auth, §4 conventions (error envelope, pagination, versioning, headers, idempotency), §5 RBAC with forward-looking `limited_viewer` extension seam, §6 catalog summary, §7 per-endpoint reference (snapshot, drives list, drive detail, drive route, 3 invite ops, user self-deletion), §8 resource-schema index cross-referencing the inline OpenAPI components, §9 observability, §10 divergences DV-19 through DV-23 (REST auth middleware, unmounted SDK endpoints, reserved `503 service_unavailable`, REST rate limit, invite handler location decision). Adds REST-only error codes `not_found`, `invalid_request`, `service_unavailable` to the shared catalog with a note that the `ErrorPayload.code` enum in `schemas/ws-messages.schema.json` must be extended in the DV-20 follow-up. Canonical machine-readable twin is `specs/rest.openapi.yaml`. | sdk-architect |
