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
| **NFR-3.20** | REST responses are projected through the caller's role mask before encoding (handler-layer mask; the underlying DB read returns plaintext and is role-agnostic) | §5 RBAC field masks |
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
   0. `GET /api/vehicles` (list)
   1. `GET /api/vehicles/{vehicleId}/snapshot`
   2. `GET /api/vehicles/{vehicleId}/drives`
   3. `GET /api/drives/{driveId}`
   4. `GET /api/drives/{driveId}/route`
   5. Invite endpoints (3 operations)
   6. `DELETE /api/users/me`
   7. `GET /api/users/me/export`
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

> **Divergence (DV-20 — RESOLVED):** All Go-server-owned SDK-surface REST endpoints in §6 / §7 are now mounted. `GET /api/vehicles` (§7.0) landed first (MYR-91, 2026-05-10); `GET /api/vehicles/{vehicleId}/snapshot` (§7.1) and `GET /api/vehicles/{vehicleId}/drives` (§7.2) landed in MYR-133 (2026-06-03); `GET /api/drives/{driveId}/route` (§7.4) landed in PR #260 (DV-20); `GET /api/drives/{driveId}` (§7.3) landed in MYR-130 (2026-07-02). See §10.

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

> **Why an HTTP header for REST but an in-band frame for WebSocket?** Browsers cannot set arbitrary headers on a WebSocket upgrade request, so the WS path pushes the token into the first WebSocket frame for portability (`websocket-protocol.md` §2.3 rationale). REST has no such constraint -- the standard `Authorization: Bearer <token>` header is universally supported by every HTTP client in the v1 client matrix (browser `fetch`, Node `undici`, Swift `URLSession` — including watchOS and visionOS variants) and is the least-surprising choice.

### 3.2 Server-side validation

The server's REST middleware MUST:

1. Parse the `Authorization` header; reject requests without it with `401 auth_failed`.
2. Reject malformed headers (missing `Bearer ` prefix, empty token) with `401 auth_failed`.
3. Validate the token via the same `Authenticator` instance used by the WebSocket handler ([`internal/ws/auth.go`](../../internal/ws/auth.go)) -- in production this is `internal/auth.NewJWTAuthenticator`, which checks signature, issuer, audience, and expiry against `AuthConfig`.
4. Resolve the authenticated `userId`.
5. For vehicle-scoped endpoints, resolve the user's vehicle ownership set via `Authenticator.GetUserVehicles(ctx, userID)` and verify the requested `vehicleId` is in the set. On mismatch, return `403 vehicle_not_owned`.
6. Emit observability signals using the same slog / Prometheus / OTel conventions as the WebSocket handler (§9).

The entire REST auth middleware is PLANNED; no REST auth middleware exists in the current server -- see §10 DV-19.

**Dual-algorithm validation (MYR-193, ADR-001).** As of MYR-193 the single `Authenticator` accepts tokens signed with EITHER algorithm, pinned per token by the header `alg` + `kid` with a strict allowlist:

- **HS256** — the legacy shared-secret tokens minted by the Next.js app / testbench with `AUTH_SECRET` (unchanged; the whole live app depends on them).
- **ES256** — the new asymmetric access tokens minted by the Go server's own identity module (`internal/identity`), verified against the local keypair's public keys resolved by the token's `kid`. The public keys are published at `GET /api/auth/.well-known/jwks.json` (§7.10).

Both carry `iss=myrobotaxi`, `aud=telemetry`, `sub=<user CUID>`, so issuer/audience/expiry checks are identical across algorithms. The key callback is type-driven — an HMAC-typed token only ever receives the shared secret, an ECDSA-typed token only ever receives an EC public key — so algorithm-confusion attacks (e.g. signing an HS256 token with the published ES256 public key as the HMAC key) are rejected. The fail-closed user-existence check (§3.2 step 4, FR-10.1) now accepts a `sub` present in EITHER the Prisma `"User"` table OR the Go-owned `go_users` table (Apple-native users have no legacy Prisma row).

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
| `error.subCode` | `string` (enum) \| `null` | No | P0 | Optional typed sub-code for branching consumer UI when the primary code is ambiguous across carriers. v1 enum: `device_cap` (WS-only, shared with the WS ErrorPayload — REST does not emit it; declared on the REST envelope for shared-type compatibility) and `reauth_required` (REST-only, emitted by §7.6 / §7.7 when the recent-login re-auth gate fails — see §4.1.1 `auth_failed`). The wire shape is **always present**, serialized as JSON `null` when the carrier emits no sub-code (see §4.1 envelope JSON example above). |

Two rules are non-negotiable for every error response:

1. **Consumers MUST branch on `error.code`, never on `error.message`.** The message is a free-form English string for developer tooling and is subject to change without a protocol version bump. Per FR-7.1, the stable enum is the contract, not the prose.
2. **`error.message` MUST NOT contain any P1 value.** No GPS coordinates, no addresses, no location names, no tokens, no email addresses, no raw VINs (VIN appears only as `***XXXX` last-4 via `redactVIN()`). See [`data-classification.md`](data-classification.md) §2.2. Error construction sites in the REST handler MUST use opaque IDs (`vehicleId`, `driveId`, `userId`) for correlation, never the underlying sensitive values. `contract-guard` Rule CG-DC-2 blocks PRs that introduce P1 values into error construction sites.

#### 4.1.1 REST error code catalog

The REST catalog is a superset of the WebSocket catalog in [`websocket-protocol.md`](websocket-protocol.md) §6.1.1 and [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) `ErrorPayload.code`. The shared codes map directly to the same typed error values in the SDK's `CoreError` union, so consumer code branches on a single enum across both transports. REST adds three codes that have no WebSocket equivalent, flagged in the "Carrier" column as REST-only.

| Code | HTTP | Carrier | Status | Reconnect/retry policy | Description |
|------|------|---------|--------|------------------------|-------------|
| `auth_failed` | 401 | Shared (WS + REST) | Implemented (MYR-47); `subCode: reauth_required` on REST §7.6 / §7.7 (MYR-76) | Surface to UI; refresh token via `getToken()`; retry once (FR-6.2). A second `auth_failed` is terminal for the operation. **`subCode: reauth_required` is NOT eligible for the `getToken()` retry path** — the SDK MUST surface it to the consumer's auth layer, which is responsible for triggering a fresh interactive sign-in flow (e.g., a NextAuth `signIn()` redirect) before retrying. A silent `getToken()` refresh cannot satisfy the precondition because the `auth_time` claim only advances on a fresh OAuth round-trip. | Token signature/issuer/audience/expiry check failed, or the `Authorization` header was missing/malformed. **`subCode: reauth_required` (REST-only, §7.6 / §7.7):** the bearer token is valid but the user's most recent fresh OAuth sign-in is older than the recent-auth window (default 300 s; configurable via `REAUTH_MAX_AGE_SEC`). The SDK MUST trigger an interactive sign-in flow and retry. |
| `auth_timeout` | 401 | Shared (WS + REST) | Implemented (WS); REST path not yet exercised (server-side ValidateToken has no deadline-only branch in v1) | Auto-retry once with fresh token; NFR-3.10-style backoff on subsequent attempts. | Rare REST path: server-side token validation exceeded its internal deadline. Treated as transient. |
| `permission_denied` | 403 | Shared (WS + REST, PLANNED on WS per DV-07) | PLANNED — emitted alongside MYR-46 per-vehicle subscribe | Surface to UI; do not auto-retry the same operation. | Authenticated user attempted a resource they do not own or a role they do not have (e.g., viewer calling an invite endpoint). |
| `vehicle_not_owned` | 403 | Shared (WS + REST, PLANNED on WS per DV-07) | Implemented on REST (MYR-47); PLANNED on WS per DV-07 | Surface to UI; do not auto-retry the same vehicleId. | Specific case of `permission_denied` for a vehicle-scoped endpoint whose `vehicleId` path param is not in the caller's ownership set. |
| `not_found` | 404 | **REST-only** | Implemented (MYR-47) | Surface to UI; do not retry. The resource either does not exist or is filtered out by ownership / role mask. | Unknown `vehicleId`, `driveId`, or `inviteId`. The SDK cannot distinguish "never existed" from "revoked access" -- this is intentional, so the server never leaks the existence of resources the caller cannot see. |
| `invalid_request` | 400 | **REST-only** | Implemented (MYR-47) | Surface to UI as a developer error; do not retry. | Request body, path params, or query string failed server-side validation (malformed cursor, `limit` out of range, malformed email on invite creation, etc.). |
| `conflict` | 409 | **REST-only** | Implemented (MYR-174) | Surface to UI; **do not auto-retry the same mutation** — the ride is not in a state that permits it. | A ride-request state mutation is illegal from the row's current lifecycle state (e.g. cancelling a `completed` ride, accepting a `cancelled` one). The legal-transition matrix is §7.8. Member of the shared `ErrorPayload.code` enum for single-union SDK typing, but never emitted over the WS transport. |
| `ride_active` | 409 | **REST-only** | Implemented (MYR-230) | **Do not auto-retry the create.** Adopt the returned `activeRideRequest` into the pending/tracking UI — this is the ride the rider should be looking at. | The caller tried to create a second **instant** ride request while already holding an OPEN one (status `requested`/`accepted` or any in-progress state — everything short of terminal `completed`/`declined`/`cancelled`). Only one active instant ride per rider is allowed; **scheduled rides are exempt** and never trigger this. Distinct from `conflict` (an illegal *transition* on a known ride) — this rejects the *creation* of a second concurrent ride. The 409 body carries the existing open request under `activeRideRequest` (same shape as `GET /api/ride-requests/{id}`) so the client adopts it. See §7.8. |
| `rate_limited` | 429 | Shared (WS + REST) | Implemented for WS pre-auth per-IP cap (MYR-47); REST per-user request cap PLANNED (DV-22); WS post-auth per-user cap PLANNED (DV-08) | Auto-retry with extended backoff (§4.1.2). SDK MAY set `Retry-After` header as backoff hint. | Two distinct caps share the same typed code. WS emits `rate_limited` with `subCode: device_cap` for **concurrent-session cap** breaches (too many simultaneous WebSocket connections per user, see `websocket-protocol.md` §6.1.1 and DV-08). REST emits `rate_limited` (no sub-code in v1) for **request-rate cap** breaches (>120 req/min per authenticated user, see §4.1.2 and DV-22). Consumers distinguish the two via the carrier transport and the presence of `subCode`. |
| `internal_error` | 500 | Shared (WS + REST) | Implemented on REST (MYR-47); PLANNED on WS | Auto-retry with exponential backoff (NFR-3.10 curve from `websocket-protocol.md` §7.1), cap at 3 REST attempts before surfacing. | Catch-all for unexpected server failures: panics, DB errors, downstream timeouts. |
| `service_unavailable` | 503 | **REST-only, PLANNED** | PLANNED (DV-21) | Auto-retry with exponential backoff; honor `Retry-After` header if present. | Reserved for maintenance windows and graceful-shutdown states. The server MAY return `503` during rolling deployments; v1 does not yet emit this code. Added to the REST catalog so SDK consumers can write forward-compatible handlers. |
| `snapshot_required` | -- | **WS-only** (close code 4005 + error frame) | PLANNED (DV-02) | n/a for REST | WS-only. REST has no analogue because REST is already the snapshot channel (the "fall back to snapshot fetch" signal IS a REST call). Listed here for completeness; REST clients never receive this code. |
| `key_not_paired` | 403 | **REST-only** | Implemented (MYR-180) | Surface to UI; **do not auto-retry** — needs owner action. | The application's virtual key is not enrolled on the vehicle (owner has not completed the `tesla.com/_ak/<domain>` pairing, MYR-115), or the command-signing transport is not configured. This is the default outcome for every signer-required command in §7.9 until pairing happens. The SDK prompts the owner to pair. |
| `vehicle_asleep` | 503 | **REST-only** | Implemented (MYR-180) | Auto-retry with backoff (§4.1.2 curve). SDK MAY honor `Retry-After`. | The target vehicle was asleep/offline and did not come online within the executor's bounded wake+retry budget (§7.9). Transient — the executor already woke + retried internally before surfacing this. |
| `command_failed` | 502 | **REST-only** | Implemented (MYR-180) | Surface to UI; do not blindly auto-retry (the vehicle rejected the action). | A vehicle command (§7.9) failed for a non-scope, non-pairing reason: the vehicle returned `result:false`, a signing-session/counter error survived re-handshake, or the Fleet API/proxy failed. Collapses the several vehicle-side failure modes into one typed code in v1. |

##### 4.1.1.a REST-only codes added to the shared catalog

Eight codes are REST-only extensions of the shared catalog: `not_found`, `invalid_request`, `service_unavailable`, `conflict` (MYR-174), `ride_active` (MYR-230), and `key_not_paired` / `vehicle_asleep` / `command_failed` (MYR-180).

- `conflict` (HTTP 409) is emitted only over REST when a ride-request lifecycle mutation is illegal from the row's current state (§7.8 transition matrix). It has no WS analogue because the ride-request mutations are request-oriented REST endpoints; the WS transport carries only the summary `ride_status_changed` broadcast of a *successful* transition. It is a member of the shared `ErrorPayload.code` enum (added by MYR-174) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks it REST-only-on-the-wire.
- `ride_active` (HTTP 409) is emitted only over REST when `POST /api/ride-requests` is called for an **instant** ride while the caller already holds an OPEN instant ride (§7.8). Like `conflict` it is request-oriented and has no WS analogue — the WS path never carries a create. It is a member of the shared `ErrorPayload.code` enum (added by MYR-230) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks it REST-only-on-the-wire. **Response-body note:** unlike every other error, the `ride_active` 409 body adds a sibling `activeRideRequest` field alongside the standard `error` envelope — the rider's existing open ride (`$defs.RideRequest`, byte-for-byte the `GET /api/ride-requests/{id}` shape) — so the client adopts it rather than surfacing a decline. The nested `error` object is unchanged; consumers that ignore the extra field still get a well-formed envelope.
- `not_found` is not emitted over the WebSocket because the WS path enforces ownership via silent filtering in `Hub.Broadcast` (see `websocket-protocol.md` §4.5) -- a client simply does not receive frames for vehicles it does not own, and there is no equivalent "the resource does not exist" signal because the WS is stream-oriented, not request-oriented. On REST, every vehicle-scoped path param MUST return `404 not_found` for unknown IDs.
- `invalid_request` exists only because REST accepts structured request bodies and query params that can be malformed independently of auth. The WS protocol has no v1 client->server frames that take structured payloads beyond `auth`, so malformed-body errors cannot arise there.
- `service_unavailable` is RESERVED for the REST contract so the SDK can write forward-compatible handlers before the server begins emitting it during maintenance windows.
- `key_not_paired`, `vehicle_asleep`, and `command_failed` are the MYR-180 vehicle-command codes (§7.9). They are REST-only because the WS transport is stream-oriented and carries no command requests — there is no WS surface on which a command could be issued or rejected. They are members of the shared enum (added by MYR-180) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks them REST-only-on-the-wire.

Both `not_found` and `invalid_request` are now members of the shared `ErrorPayload.code` enum in [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) (added by MYR-98, the DV-20 enum slice) even though the WS never emits them, so the SDK's `CoreError` union is a single enum across both transports. This is not a drift -- the WS contract explicitly lists them as "REST-only" in the catalog description and the schema enum description marks them REST-only-on-the-wire. The shared-enum subCode `reauth_required` was added in the same change (REST-only; see §4.1). DV-20's remaining open scope is the endpoint-mounting work (§10), not the enum.

#### 4.1.1.b Server-side emission audit (MYR-47)

The closed Go enum lives in [`internal/wserrors/wserrors.go`](../../internal/wserrors/wserrors.go) and is the single source of truth for every error code the server emits. `sendError` (WS) and `writeError` / `WriteErrorEnvelope` (REST) take the typed `ErrorCode` as a parameter, so the compiler refuses string literals at the call site — every error construction path pulls a value from the enum. The reachability matrix in [`internal/wserrors/wserrors_test.go`](../../internal/wserrors/wserrors_test.go) walks the closed enum and asserts every code has either a constructed-scenario test or a documented blocker.

Per-site audit (REST surface in `internal/telemetry/`, WS surface in `internal/ws/`):

| Site | HTTP / WS | Code (typed) | Scenario |
|------|-----------|--------------|----------|
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — invalid VIN | 400 | `ErrCodeInvalidRequest` | Malformed `{vin}` path param |
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — missing Authorization | 401 | `ErrCodeAuthFailed` | Header omitted |
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — invalid token | 401 | `ErrCodeAuthFailed` | `ValidateToken` rejects |
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — vehicle not found | 404 | `ErrCodeNotFound` | `GetVehicleOwner` returns `sdk.ErrNotFound` |
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — vehicle ownership mismatch | 403 | `ErrCodeVehicleNotOwned` | Caller's `userID` ≠ vehicle's `ownerID` |
| [`vehicle_status_handler.go`](../../internal/telemetry/vehicle_status_handler.go) — DB lookup error | 500 | `ErrCodeInternalError` | `GetVehicleOwner` returns non-`ErrNotFound` |
| [`vehicle_status_mask.go`](../../internal/telemetry/vehicle_status_mask.go) — role resolution error | 500 | `ErrCodeInternalError` | `ResolveRole` returns error |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — wrong method | 405 | `ErrCodeInvalidRequest` | Method is neither `GET` (config status) nor `POST` (re-push) |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — invalid VIN | 400 | `ErrCodeInvalidRequest` | Malformed `{vin}` path param |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — missing/invalid Authorization | 401 | `ErrCodeAuthFailed` | Header omitted or `ValidateToken` fails |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — vehicle ownership mismatch | 403 | `ErrCodeVehicleNotOwned` | Caller's `userID` ≠ vehicle's `ownerID` |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — Tesla token expired (no refresher) | 401 | `ErrCodeAuthFailed` | Token expired and refresh path unavailable |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — Tesla token refresh failed | 401 | `ErrCodeAuthFailed` | Tesla refresh endpoint rejects |
| [`fleet_config_handler.go`](../../internal/telemetry/fleet_config_handler.go) — Fleet API skipped vehicle | 409 | `ErrCodeInvalidRequest` | Fleet API replies "skipped" with reason |
| [`fleet_config_errors.go`](../../internal/telemetry/fleet_config_errors.go) — Tesla token not found | 401 | `ErrCodeAuthFailed` | User has not linked a Tesla account |
| [`fleet_config_errors.go`](../../internal/telemetry/fleet_config_errors.go) — Tesla token lookup failure | 500 | `ErrCodeInternalError` | DB failure on `tokens.GetTeslaToken` |
| [`fleet_config_errors.go`](../../internal/telemetry/fleet_config_errors.go) — vehicle lookup not found | 404 | `ErrCodeNotFound` | `GetVehicleOwner` returns `sdk.ErrNotFound` |
| [`fleet_config_errors.go`](../../internal/telemetry/fleet_config_errors.go) — vehicle lookup failure | 500 | `ErrCodeInternalError` | DB failure on `GetVehicleOwner` |
| [`fleet_config_errors.go`](../../internal/telemetry/fleet_config_errors.go) — Fleet API server / client / network error | 502 | `ErrCodeInternalError` | Upstream Fleet API issue (collapsed to one typed code in v1) |
| [`handler.go`](../../internal/ws/handler.go) — pre-auth per-IP cap | HTTP 429 (envelope) | `ErrCodeRateLimited` | `MaxConnectionsPerIP` breached before WS upgrade |
| [`handler.go`](../../internal/ws/handler.go) — auth failed | WS frame | `ErrCodeAuthFailed` | `ValidateToken` rejects after upgrade |
| [`handler.go`](../../internal/ws/handler.go) — auth timeout | WS frame | `ErrCodeAuthTimeout` | Client did not send `auth` within `AuthTimeout` |
| [`handler.go`](../../internal/ws/handler.go) — `GetUserVehicles` failure | WS frame | `ErrCodeAuthFailed` | DB failure when loading vehicle ownership |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — missing/invalid Authorization | 401 | `ErrCodeAuthFailed` | Header omitted or `ValidateToken` fails |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — malformed/unknown-field body, bad place, out-of-range coord, bad `scheduledFor`, bad `limit`/`cursor` | 400 | `ErrCodeInvalidRequest` | Create body / list query failed validation (`additionalProperties:false`) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — vehicle not found on create | 404 | `ErrCodeNotFound` | `GetByID` returns `sdk.ErrNotFound` |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — vehicle access denied on create | 403 | `ErrCodeVehicleNotOwned` | Caller's `userID` ≠ vehicle's owner (v1 owner-only access; §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — second open instant ride on create (pre-check) | 409 | `ErrCodeRideActive` | Rider already holds an OPEN instant ride; body carries `activeRideRequest` (MYR-230, §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — second open instant ride on create (unique-index race backstop) | 409 | `ErrCodeRideActive` | Concurrent instant create rejected by `uq_go_ride_requests_active_instant_rider` (23505); winner re-read into `activeRideRequest` (MYR-230, §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — unknown / non-party ride | 404 | `ErrCodeNotFound` | `GetByID` miss, or caller is neither rider nor owner (no existence leak) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — owner attempts cancel (rider-only) | 403 | `ErrCodePermissionDenied` | A party, but wrong role for the action |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — illegal lifecycle transition | 409 | `ErrCodeConflict` | Cancel from a non-`{requested,accepted}` state (§7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — store failure | 500 | `ErrCodeInternalError` | DB error on create / status update / list |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — rider attempts accept/decline (owner-only) | 403 | `ErrCodePermissionDenied` | A party, but wrong role for the decision |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — accept/decline from a non-`requested` state | 409 | `ErrCodeConflict` | Illegal lifecycle transition (§7.8 matrix) |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — incoming feed store failure | 500 | `ErrCodeInternalError` | DB error on the owner list |

Sites NOT in this audit (intentional carve-outs):

- [`receiver.go`](../../internal/telemetry/receiver.go) — Tesla mTLS endpoint. Not consumer-facing; protobuf agents only.
- [`debug_fields_handler.go`](../../internal/telemetry/debug_fields_handler.go) — dev-only debug stream. Not part of the SDK contract surface (handler doc-comment).
- [`server.go`](../../internal/server/server.go) `/healthz` — healthcheck, has its own response shape distinct from the typed-error contract.

**`service_unavailable` is intentionally REST-only and is NOT promoted to the shared enum** in DV-20. The WS equivalent of a 503 maintenance window is a connection-refused close code (4003/1011), not a typed `service_unavailable` error frame. Keeping `service_unavailable` out of the shared enum preserves transport-appropriate error semantics: REST clients retry on 503+`service_unavailable`, WS clients retry on close 4003/1011 per `websocket-protocol.md` §7.1.

#### 4.1.2 Rate limiting

> **Anchored:** FR-7.1, NFR-3.6.

In v1 REST endpoints are protected by a per-user request-rate limit. The default is a PLANNED **120 requests/minute per authenticated user** (approximately two requests per second sustained, with bursts permitted via a token bucket). This cap is PLANNED and not enforced today -- tracked as DV-22.

When the cap is breached the server returns `429 rate_limited` with the standard error envelope. The response SHOULD include a `Retry-After: <seconds>` header indicating the minimum delay the SDK should wait before retrying. SDKs MUST apply exponential backoff on successive `429`s using the curve from [`websocket-protocol.md`](websocket-protocol.md) §7.1 (initial 1 s, multiplier 2x, max 30 s, +/- 25% jitter).

The REST rate limit is **independent** of the WebSocket `MaxConnectionsPerUser` per-user concurrent-connection cap (WS DV-08). A user may have 5 open WebSocket sessions AND 120 REST requests/min simultaneously. This is intentional: the two limits protect different exhaustion modes (concurrent holdings vs request flood).

Unlike the WebSocket `rate_limited` error, REST `rate_limited` in v1 does **not** emit a `subCode: device_cap` -- `device_cap` is specific to the per-user concurrent-connection cap on the WS path. REST rate-limit breaches surface to the UI as a generic "too many requests" signal; per-device UX messaging is not part of v1 REST.

**Pre-auth cap on `/api/auth/*` (MYR-193, IMPLEMENTED).** The identity endpoints (§7.10) cannot use the per-user cap above — they run *before* a user is authenticated. They are instead protected by a **per-IP token-bucket** limit (default 30 req/min, burst 10; configurable via `identity.auth_rate_limit_per_minute` / `identity.auth_rate_limit_burst`), keyed on the leftmost `X-Forwarded-For` entry (the edge appends). On breach the server returns `429 rate_limited` with a `Retry-After` header — the same envelope and SDK backoff behaviour as above. This guards the sign-in / refresh surface against credential spraying and refresh-token brute force.

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

The ride-request list endpoints (§7.8) use the same total-order shape over their own timestamp: `ORDER BY createdAt DESC, id DESC`, with the cursor encoding `(createdAt, id)`. The keyset resume predicate is `(created_at, id) < (:cursorCreatedAt, :cursorId)` — a Postgres row-value comparison over the same compound key — so pagination is stable across concurrent inserts. `createdAt` travels inside the opaque cursor as an RFC 3339 nanosecond string and round-trips losslessly into the `timestamptz` comparison.

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

The third architectural slot `limited_viewer` is NOT a v1 role but is kept available as an extension seam per FR-5.5. The masking machinery below is defined as a static per-role projection applied at the handler layer (see §5.1) via `internal/mask/`, so adding a third role is a one-file change (a new mask entry) rather than an architectural change.

### 5.1 Masking rule

Every REST response MUST be projected through the caller's role mask **server-side** before being written to the response body. No raw fan-out to callers (NFR-3.19 is about the WS path; NFR-3.20 extends the same rule to REST).

The mask is applied at the **handler layer**: the store returns plaintext, fully-decrypted objects, and each REST handler invokes `mask.Apply(obj, role)` from `internal/mask/` before encoding. Three reasons the store stays role-agnostic:

1. **Store reuse.** The same `VehicleRepo.Get` is called from cron jobs (drive pruning per `data-lifecycle.md` §5), audit-log writers, and admin / ops tooling that have no role concept. Making the store role-aware would force every non-handler caller to fabricate a fake role.
2. **Test isolation.** Store tests don't need to set up role plumbing; they exercise the persistence layer in isolation.
3. **Single point of consistency with the WS path.** The WebSocket broadcaster reads from the event bus, not the store. If the mask were attached to the store layer, the WS path would bypass it entirely. Anchoring the mask at the handler layer keeps it adjacent to the WS hub's per-role pre-projection (`websocket-protocol.md` §4.6) — both paths consume the same `internal/mask/` matrix.

**Output shape: absent, not nulled.** The mask MUST remove denied fields from the JSON entirely (no key emitted) rather than emit them with a `null` value. Emitting `null` would leak the existence of the field to the viewer, which is itself information. The Go implementation projects through `map[string]any` rather than relying on `omitempty` (which only suppresses zero values, not access-control denials).

### 5.2 Per-resource masks

#### 5.2.0 Vehicles list (`GET /api/vehicles`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` | All `VehicleSummary` fields: `vehicleId`, `name`, `model`, `year`, `color`, `vinLast4`, `status`, `chargeLevel`, `estimatedRange`, `lastUpdated`, `role` | Full catalog visibility. `role` is always `owner` for the row's caller-vehicle relationship. |
| `viewer` | All `VehicleSummary` fields EXCEPT `name` | The user-assigned nickname (P1, owner-curated) is owner-only. Viewers still see model/year/color so they can identify the vehicle in their list, but the nickname stays with the owner. Forward-looking — see the §7.0 implementation note about the v1 viewer pathway being PLANNED. |

#### 5.2.1 Vehicle snapshot (`GET /api/vehicles/{vehicleId}/snapshot`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` | All fields in [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) | Including GPS, nav, charge, gear -- the full v1 `VehicleState` shape. |
| `viewer` | All fields EXCEPT `licensePlate` | **Note:** `licensePlate` is a Prisma-owned column per [`data-classification.md`](data-classification.md) §1.3 and is NOT currently a member of `vehicle-state.schema.json`, so this mask rule is **forward-looking**: it codifies the behavior the first time `licensePlate` is surfaced over the SDK. Viewers retain full GPS, nav, and charge visibility because the whole point of sharing is to watch the vehicle in real time (FR-5.1, FR-5.4). |
| `limited_viewer` (FR-5.5 future slot) | All fields EXCEPT `licensePlate`, `navRouteCoordinates`, `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`; `latitude`/`longitude` reduced to a coarse-grained hash (city-block resolution) | Documented here as the extension seam for FR-5.5. NOT implemented in v1. The mask is a static per-role projection; adding the `limited_viewer` row is a one-file handler-layer change in `internal/mask/`. |

#### 5.2.2 Drive list (`GET /api/vehicles/{vehicleId}/drives`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner`, `viewer` | `id`, `vehicleId`, `startTime`, `endTime`, `date`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `startChargeLevel`, `endChargeLevel`, `fsdMiles`, `fsdPercentage`, `createdAt`, `startLocation`, `startAddress`, `endLocation`, `endAddress` | The field set is identical for both roles. Viewers are read-only (FR-5.4); they observe the same data as owners but cannot create, delete, or modify any drive record. |

`startLocation`, `startAddress`, `endLocation`, `endAddress` are P1 per [`data-classification.md`](data-classification.md) §1.4 and are included on the list payload as of [MYR-145](https://linear.app/myrobotaxi/issue/MYR-145) (2026-06-06). Rationale: the drive-history scroll view needs origin and destination labels alongside the lightweight stats to render a meaningful list row — fetching them per-row via `GET /drives/{driveId}` would force one extra REST call per visible row. The four columns are short reverse-geocoded TEXT values (street address / place name, a few hundred bytes each at most) so the per-page payload remains comfortably under the ~5 KB-per-page list budget. The handler omits any of the four keys whose store value is empty (drive in progress with no end-side geocoding yet, or zero-GPS at drive start) per the §7.2 nullable-field convention.

`fsdMiles` and `fsdPercentage` are P0 per [`data-classification.md`](data-classification.md) §1.4 (FSD distance / ratio — aggregate, non-identifying) and are included on the list payload as of [MYR-152](https://linear.app/myrobotaxi/issue/MYR-152). Rationale: the drive-history view shows FSD usage per drive, and fetching it per-row via `GET /drives/{driveId}` would force one extra REST call per visible row. Both are small `double` columns already populated by the drive-write path, so the per-page payload stays well under the ~5 KB-per-page list budget. Unlike the location fields they are always present (non-nullable, default `0`). `routePoints`, `energyUsedKwh`, and `interventions` stay out of the projection per the heavy-payload / drive-detail rule below.

#### 5.2.3 Drive detail (`GET /api/drives/{driveId}`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner`, `viewer` | All FR-3.4 stats: `id`, `vehicleId`, `startTime`, `endTime`, `date`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `energyUsedKwh`, `startChargeLevel`, `endChargeLevel`, `fsdMiles`, `fsdPercentage`, `interventions`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `createdAt` | Both roles see the full record including P1 start/end location and address. Rationale: the owner expects their own data, and the viewer has explicit consent via the invite they accepted. Denying viewers the start/end address would defeat the sharing use case (FR-5.1) -- knowing "the drive ended at the airport" is the point. |

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

### 5.3 Audit log sampling for masked responses

> **Anchored:** NFR-3.20, FR-10 (audit infrastructure), `data-lifecycle.md` §4.

Every REST response and every WebSocket frame whose mask projection removed at least one field MUST be audit-logged at a **1% sampling rate**, computed deterministically by hash. The hash inputs differ per channel because the WS audit emit is per-`(vehicleId, role, frame)` at the hub (not per-client), while the REST audit emit is per-request:

```
REST: shouldAudit := hash(userId || requestId || resourceId) mod 100 == 0
WS:   shouldAudit := hash(vehicleId || role || frameSeq)    mod 100 == 0
```

`requestId` is the `X-Request-ID` echoed per §4.4. `frameSeq` is the envelope sequence number once DV-02 lands; until then, the hub uses an in-process monotonic per-vehicle counter.

Hash-based sampling rather than a counter avoids concentrating samples on bursty vehicles (a counter samples every 100th regardless of source; hash-based sampling distributes uniformly across the active vehicle set, which is essential for incident triage).

Audit entries land in the existing `AuditLog` table per `data-lifecycle.md` §4 with:

| Column | Value |
|---|---|
| `action` | `mask_applied` (NEW enum value — see `data-lifecycle.md` §4.2) |
| `targetType` | `rest_response` for REST, `ws_broadcast` for WebSocket |
| `targetId` | The `vehicleId` or `driveId` whose response was masked |
| `initiator` | `user` (the consumer's request triggered the response) |
| `metadata` | `{ "role": "viewer", "channel": "rest" \| "ws", "fieldsMasked": ["licensePlate", ...], "endpoint": "/api/vehicles/{id}/snapshot" }` |

`metadata.fieldsMasked` is a list of column names. Column names are P0 (they appear in this contract and in `data-classification.md` §1) — the audit log MUST NOT contain any actual masked field values, only their names.

Audit-log entries for masked responses are themselves P0 (per `data-classification.md` §2.3 already-classified rules) and follow the same indefinite retention as all other audit entries (NFR-3.29).

For WebSocket broadcasts, the audit emit happens **once per (vehicleId, role, frame)** at the hub layer, not per client — keeping the audit volume proportional to vehicle activity, not to viewer count.

### 5.4 Extension seam for a third role (FR-5.5)

The RBAC masking machinery is implemented as a static lookup table keyed by `(resourceType, role)` in `internal/mask/`, consumed by both the REST handler layer (§5.1) and the WebSocket hub's per-role pre-projection (`websocket-protocol.md` §4.6). Adding a new role is a three-step change:

1. Add the role name to the `Role` enum in `internal/auth/`.
2. Add mask entries for each resource type that the new role should see (or inherit from `viewer` with a diff).
3. Wire the role into the `Authenticator.ResolveRole(userId, vehicleId)` call site.

No contract changes are required for the new role's wire shape (the REST response schemas already cover every field; the new role simply sees fewer of them). This satisfies FR-5.5's "architecture MUST support adding a third role without schema changes."

---

## 6. Endpoint catalog summary

| Method | Path | Purpose | Auth | Anchored FRs/NFRs |
|--------|------|---------|------|-------------------|
| `GET` | `/api/vehicles` | List the caller's vehicles (catalog only, no telemetry detail) | Bearer (self) | FR-4.x, FR-5.4, NFR-3.21 |
| `GET` | `/api/vehicles/{vehicleId}/snapshot` | Cold-load full VehicleState | Bearer + owner-or-viewer of vehicleId | FR-1.1, FR-1.2, FR-2.1, NFR-3.5, NFR-3.11 |
| `GET` | `/api/vehicles/{vehicleId}/drives` | Paginated drive history for vehicle | Bearer + owner-or-viewer of vehicleId | FR-3.2, FR-9.1, FR-9.2 |
| `GET` | `/api/drives/{driveId}` | Single drive detail (FR-3.4 stats + start/end addresses) | Bearer + owner-or-viewer of drive's vehicle | FR-3.4, FR-9.1 |
| `GET` | `/api/drives/{driveId}/route` | Full GPS polyline for drive playback | Bearer + owner-or-viewer of drive's vehicle | FR-3.3, NFR-3.23 |
| `POST` | `/api/vehicles/{vehicleId}/invites` | Create sharing invite | Bearer + owner of vehicleId | FR-5.1 |
| `GET` | `/api/vehicles/{vehicleId}/invites` | List viewers + pending invites | Bearer + owner of vehicleId | FR-5.2 |
| `DELETE` | `/api/invites/{inviteId}` | Revoke invite | Bearer + owner of invite's vehicle | FR-5.3 |
| `DELETE` | `/api/users/me` | Delete own account + all data | Bearer (self only) | FR-10.1, FR-10.2, NFR-3.29 |
| `GET` | `/api/users/me/export` | GDPR Art. 15 / 20 portability export of every Prisma row owned by the caller | Bearer (self only) | FR-10, NFR-3.29 |
| `POST` | `/api/ride-requests` | Create a ride request (P10 ride-hailing) | Bearer + vehicle access | FR-9.3, NFR-3.21, NFR-3.23 |
| `GET` | `/api/ride-requests` | Rider's own ride-request history (paginated) | Bearer (self as rider) | FR-9.1, FR-9.3 |
| `GET` | `/api/ride-requests/incoming` | Owner's feed of open (`requested`) ride requests across their vehicles (paginated) | Bearer (self as owner) | FR-9.1, FR-9.3 |
| `GET` | `/api/ride-requests/{id}` | Single ride-request detail | Bearer + party (rider or owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/cancel` | Rider cancels a requested/accepted ride | Bearer (rider) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/accept` | Owner accepts a `requested` ride (emits the MYR-176 dispatch seam) | Bearer (owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/decline` | Owner declines a `requested` ride | Bearer (owner) | FR-9.3 |
| `POST` | `/api/vehicles/{vehicleId}/command/{name}` | Send a Tesla vehicle command (P11 actuation + P10 dispatch) | Bearer + owner of vehicleId | FR-11.x, NFR-3.21 |
| `POST` | `/api/auth/apple` | Native Sign in with Apple → ES256 access + refresh pair | None (pre-auth; per-IP rate-limited) | FR-6.1, MYR-193 |
| `POST` | `/api/auth/refresh` | Single-use refresh-token rotation | Refresh token in body (pre-auth; per-IP rate-limited) | FR-6.2, MYR-193 |
| `POST` | `/api/auth/revoke` | Revoke a refresh-token family (sign-out) | Refresh token in body (pre-auth; per-IP rate-limited) | MYR-193 |
| `GET` | `/api/auth/.well-known/jwks.json` | Public ES256 verification keys (JWKS) | None (public) | MYR-193 |

The `POST`/`GET /api/ride-requests[...]` rows (§7.8) are the P10 ride-hailing surface: the four rider-facing endpoints are mounted as of MYR-174 and the three owner-facing endpoints (incoming feed + accept/decline) as of MYR-175. The dispatch/live-tracking transitions remain with MYR-176/177 and the reschedule endpoints with MYR-192.

The `/api/auth/*` rows (§7.10) are the identity module's auth surface (MYR-193, ADR-001 `docs/architecture/adr-001-identity-module.md`): native Sign in with Apple, ES256 access-token minting with a published JWKS, and rotating refresh tokens. Unlike every other row, these are **pre-authentication** endpoints (they mint or rotate the very credential the others require), so they are not Bearer-gated — they are protected by a per-IP token-bucket rate limit instead (§4.1.2).

`GET /api/vehicles` (§7.0) is mounted by the Go server as of MYR-91 (2026-05-10). `GET /api/vehicles/{vehicleId}/snapshot` (§7.1) and `GET /api/vehicles/{vehicleId}/drives` (§7.2) are mounted as of MYR-133 (2026-06-03). `GET /api/drives/{driveId}/route` (§7.4) is mounted as of PR #260 (DV-20), and `GET /api/drives/{driveId}` (§7.3) as of MYR-130 (2026-07-02) — DV-20 is fully RESOLVED (see §10). `GET /api/users/me/export` (and the §7.6 / §7.5 endpoints) is served by the Next.js app per §10 DV-23 and is NOT in scope for the Go server's DV-20 mount.

---

## 7. Endpoint reference

### 7.0 `GET /api/vehicles`

> **Anchored:** FR-4.x (vehicle catalog), FR-5.4 (owner/viewer roles), NFR-3.21 (ownership enforcement).

#### Purpose

Returns a thin catalog of vehicles the authenticated caller is allowed to see. This is the SDK's vehicle-enumeration entry point — the answer to "what are my cars?" Without it, every SDK consumer would have to read the Prisma `Vehicle` table directly, bypassing the contract.

The response is deliberately a **catalog**, not telemetry. Live state (charge level, speed, location, navigation) lives in `§7.1 /snapshot` and is fetched per-vehicle once the consumer knows which vehicles to ask about. Pre-MYR-91 the only way to enumerate vehicles was the WebSocket `auth_ok` frame's `vehicleCount`, which carried a count but not the IDs needed to call other endpoints.

#### Request

```
GET /api/vehicles HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
```

No request body, no query parameters in v1. Pagination (`cursor`, `limit`) is reserved for future use but not required — most users have ≤ 3 cars and the response is bounded.

#### Response -- 200 OK

```json
{
  "items": [
    {
      "vehicleId": "clxyz1234567890abcdef",
      "name": "Stumpy",
      "model": "Model 3",
      "year": 2024,
      "color": "Midnight Silver Metallic",
      "vinLast4": "0001",
      "status": "parked",
      "chargeLevel": 78,
      "estimatedRange": 245,
      "lastUpdated": "2026-05-10T17:45:00Z",
      "role": "owner"
    }
  ]
}
```

##### VehicleSummary fields

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | P0 | Opaque DB cuid. The SDK uses this in every other endpoint's path parameter (`/api/vehicles/{vehicleId}/snapshot`, etc.). |
| `name` | `string` | P1 | User-assigned vehicle name. P1 because it's commonly a recognizable nickname; owner-visible only (see RBAC below). |
| `model` | `string` | P0 | Tesla model: `Model 3`, `Model S`, `Model X`, `Model Y`, `Cybertruck`, etc. |
| `year` | `integer` | P0 | Model year. |
| `color` | `string` | P0 | Display color. |
| `vinLast4` | `string` | P0 | Last 4 characters of the VIN — full VIN is never emitted per `data-classification.md` §1.5 + `redactVIN()`. |
| `status` | `string` (enum) | P0 | One of `driving`, `parked`, `charging`, `offline`, `in_service`. Mirrors `VehicleStatus` Prisma enum. |
| `chargeLevel` | `integer` | P0 | Battery state of charge, 0–100. Lightweight indicator for the catalog view; full charge group lives in `/snapshot`. |
| `estimatedRange` | `integer` | P0 | Estimated remaining range in miles. Same lightweight rationale. |
| `lastUpdated` | `string` (ISO 8601) | P0 | Timestamp of the last telemetry write to this vehicle. The catalog uses it to render "last seen N minutes ago." |
| `role` | `string` (enum) | P0 | `owner` or `viewer`. The caller's relationship to the vehicle. See RBAC below. |

##### Excluded from the list response

The list is deliberately a thin catalog. The following are NOT in `VehicleSummary` and require `/snapshot` to fetch:

- GPS coordinates (`latitude`, `longitude`, `heading`)
- Navigation atomic group (`destination*`, `origin*`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`)
- Speed, gear, climate, odometer, FSD-miles, full charge group (`chargeState`, `timeToFull`)
- Location name / address

The rationale is "the list is what you see; the snapshot is what you drill into." The SDK consumer pattern is: call `/api/vehicles` once on cold load, render the list, then call `/snapshot` for the active vehicle the user clicks into.

#### Response -- error

| HTTP | `error.code` | `subCode` | When |
|------|--------------|-----------|------|
| 401 | `auth_failed` | `null` | Missing/malformed/invalid token. |
| 401 | `auth_failed` | `reauth_required` | The MYR-79 recent-login re-auth gate (`rest-api.md` §4.1.1) is NOT applied to this endpoint in v1 — `/api/vehicles` is non-destructive catalog data, not a deletion / bulk-export surface. The row is listed here as forward-looking: if a future iteration adds catalog-level operations (e.g., bulk un-link), the same carve-out as §7.6 / §7.7 would apply. v1 returns `null` subCode only. |
| 429 | `rate_limited` | `null` | REST rate limit breached (§4.1.2). |
| 500 | `internal_error` | `null` | Underlying DB failure during `VehicleRepo.ListByUser` or invite-merge join. |

**Note on 403 / 404:** The list never returns 403 or 404 for the *list itself* — an authenticated user always sees at minimum an empty `items: []` array. Per-vehicle existence is hidden behind RBAC at the list level: vehicles the caller has no relationship to are silently absent (the same "silent existence-hiding" rule as `/snapshot` per §4.1.1). The list will never include a vehicle the caller can't see; conversely, a vehicle the caller can't see is indistinguishable from one that doesn't exist.

#### RBAC

See `§5.2.0` below for the per-role `VehicleSummary` mask. v1 behavior:

- **Owner** sees every `VehicleSummary` field for every vehicle where `Vehicle.userId == callerId`.
- **Viewer** sees every `VehicleSummary` field **EXCEPT** `name` (P1 — owner-curated nickname) for every vehicle where an accepted `Invite` row exists for the caller's email + the vehicle.
- v1 implementation note: **owner-only is the only path currently reachable on the Go server.** The viewer-merged behavior is forward-looking — it requires the Go server to read the Prisma-owned `Invite` table, which lands as a follow-up ticket. Until then, viewer-tier callers receive an empty list. The mask matrix for `VehicleSummary` is wired now (in `internal/mask/tables.go`) so the viewer pathway is data-ready when the invite-read pathway lands.

#### Idempotency

`GET` is naturally idempotent. The catalog is read-only — a repeat call returns equivalent data modulo any new vehicle the user just linked.

#### Implementation notes

- The handler lives in `internal/telemetry/vehicles_list_handler.go` (analogous to `vehicle_status_handler.go`). It calls `VehicleRepo.ListByUser(ctx, userId)` for the owner slice. Viewer-shared vehicles are PLANNED (see RBAC v1 note above).
- The mask projection is applied at the handler layer using `mask.For(mask.ResourceVehicleSummary, role)` — same plumbing as the snapshot endpoint. Owners and viewers branch on the role returned by `authenticator.ResolveRole(ctx, userId, vehicleId)` for each list item.
- No audit-log row is emitted for the list itself (reads are not P1+ per `data-lifecycle.md` §4.2). The per-row mask projection's `fieldsMasked` count is observable via the existing REST mask-audit hook (1% sample) if any viewer-mask field ever strips, but in v1 (owner-only path) the projection is the identity.

#### Forward-looking: pagination

When a single user can plausibly own > 100 vehicles (fleet operators?), this endpoint will gain `cursor` + `limit` query params per `§4.2` cursor-based pagination. v1 returns the full list in one response and omits the `nextCursor` / `hasMore` envelope — the response is a bare `{ items: [...] }`. SDK consumers that handle the future paginated shape can branch on the absence of `nextCursor` to know they're talking to a v1 server.

---

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
  "model": "Model 3",
  "year": 2024,
  "color": "Midnight Silver Metallic",
  "status": "parked",
  "speed": 0,
  "heading": 180,
  "latitude": 10.0,
  "longitude": 20.0,
  "locationName": "Home",
  "locationAddress": "123 Market St, San Francisco, CA",
  "gearPosition": "P",
  "chargeLevel": 78,
  "chargeState": "Disconnected",
  "estimatedRange": 245,
  "timeToFull": null,
  "interiorTemp": 68,
  "exteriorTemp": 55,
  "odometerMiles": 12458,
  "fsdMilesSinceReset": 412.7,
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

The seven former spec-only catalog fields (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `destinationAddress`) were promoted out of spec-only status by [MYR-24](https://linear.app/myrobotaxi/issue/MYR-24) on 2026-04-23 — the Go `internal/store.Vehicle` read path now loads them from the Prisma-owned `Vehicle` table. Six are non-nullable on snapshot (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`); `destinationAddress` remains nullable because the Prisma column is `String?`. The charge-group fields `chargeState` and `timeToFull` are persisted to the Prisma-owned `Vehicle` table as of [MYR-41](https://linear.app/myrobotaxi/issue/MYR-41) on 2026-04-25; both columns are nullable (`String?` / `Float?`) so a vehicle that has never charged surfaces `null` on the snapshot. [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) shipped the live WS wire path for both fields on 2026-04-22; the cold-load snapshot now reads back the same values. See `websocket-protocol.md` §4.1.4 and §10 DV-03 / DV-04.

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
      "startLocation": "Home",
      "startAddress": "742 Evergreen Terrace, San Francisco, CA 94107",
      "endLocation": "Whole Foods Market",
      "endAddress": "399 4th Street, San Francisco, CA 94107",
      "distanceMiles": 12.4,
      "durationSeconds": 1458,
      "avgSpeedMph": 30.5,
      "maxSpeedMph": 65.2,
      "startChargeLevel": 82,
      "endChargeLevel": 76,
      "fsdMiles": 8.1,
      "fsdPercentage": 65.3,
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
      "fsdMiles": 0,
      "fsdPercentage": 0,
      "createdAt": "2026-04-13T08:29:08Z"
    }
  ],
  "nextCursor": "eyJzdGFydFRpbWUiOiIyMDI2LTA0LTEzVDA4OjE0OjAwWiIsImlkIjoiY2xtbm85ODc2NTQzMjEwenl4dzAwMDIifQ==",
  "hasMore": true
}
```

Each `items[i]` is a `DriveSummary` object as defined in §8 and in the OpenAPI spec. The summary includes `startLocation`, `startAddress`, `endLocation`, `endAddress` ([MYR-145](https://linear.app/myrobotaxi/issue/MYR-145), 2026-06-06) so SDK consumers can render origin / destination labels without a per-row drive-detail fetch (see §5.2.2 for the rationale). Each of those four fields is **nullable on the wire**: when the underlying Drive row carries an empty value (drive still in progress with no end-side geocoding; zero-GPS at drive start; reverse-geocode lookup failed), the handler **omits the JSON key entirely** rather than emitting `""` or `null`. The second item in the example above (`clmno9876543210zyxw0002`) illustrates this — the cold-start drive has no geocoded labels yet, so all four keys are absent. `fsdMiles` and `fsdPercentage` are also included on the list payload as of [MYR-152](https://linear.app/myrobotaxi/issue/MYR-152) (P0, see §5.2.2); unlike the location fields they are **always present** (non-nullable, default `0` — the second item shows a drive with no FSD usage). `energyUsedKwh`, `interventions`, and `routePoints` are still deliberately omitted from the list payload — those remain drive detail (§7.3) and drive route (§7.4) fields per the heavy-payload + per-row stats rule.

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
- Eager pre-fetching of every drive's route would waste cellular bandwidth on every drive-list render, which is particularly bad on watchOS per NFR-3.36c (aggressive lifecycle handling: extended-runtime sessions, short-lived launches, REST-snapshot rehydration).
- The SDK MAY fetch the route for the top 1-3 drives as an optimistic prefetch when the drive list is cold-loaded on WiFi, and MUST NOT prefetch on cellular. On Apple platforms the Swift SDK MUST honor `URLSessionConfiguration.allowsExpensiveNetworkAccess = false` and `allowsConstrainedNetworkAccess = false` for prefetch requests, satisfying this rule against Low Data Mode and metered cellular without ad-hoc reachability checks.

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

The Invite table is **Prisma-owned** per [`data-classification.md`](data-classification.md) §1.6 and [`data-lifecycle.md`](data-lifecycle.md) §1.4. No `InviteRepo` exists in `internal/store/` and none will be added: per §10 DV-23 (RESOLVED 2026-05-08, MYR-69), the **Next.js app serves all three invite endpoints directly** against its existing Prisma-owned Invite table, with the public API hostname (`https://api.myrobotaxi.com/api/...`) proxying invite paths to the Next.js app and snapshot/drives/drive-route paths to the Go telemetry server. The REST contract is the SDK's source of truth regardless of where the handler runs; the handler location is an implementation detail the SDK does not observe.

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

> **Anchored:** FR-10.1, FR-10.2, NFR-3.29; GDPR Art. 17 recent-auth corollary.

#### Purpose

Deletes the authenticated user and all associated data per the cascade defined in [`data-lifecycle.md`](data-lifecycle.md) §3. This is the SDK's single entry point for user-initiated data deletion per FR-10.1. The endpoint writes an immutable audit log entry before the destructive operation per FR-10.2 and the data-lifecycle contract, and the audit log entry is retained indefinitely per NFR-3.29.

#### Re-auth precondition (MYR-76)

In addition to the standard Bearer-token check (§3), this endpoint requires that the caller's most recent **fresh OAuth sign-in** occurred within the recent-auth window. The default window is 300 s (5 minutes); operators MAY override via the `REAUTH_MAX_AGE_SEC` environment variable on the Next.js handler.

The precondition is checked before the deletion transaction is entered. Sessions whose `auth_time` (Unix-seconds, mirroring the OIDC `auth_time` claim and surfaced as `session.user.authTime` in NextAuth) is older than the window — or sessions that lack the claim entirely (legacy sessions predating the rollout) — are rejected with `401 auth_failed` carrying `subCode: reauth_required`.

Rationale: v1's Bearer token alone is sufficient to permanently destroy the account and cascade-delete every owned vehicle, drive, invite, and setting per [`data-lifecycle.md`](data-lifecycle.md) §3. A long-lived stolen token would therefore let an attacker erase the user's data before they notice. The recent-auth corollary of GDPR Art. 17 — the right to erasure does not license unauthorized erasure — motivates a fresh-OAuth-step requirement on the destructive path. The same precondition applies symmetrically to §7.7 `GET /api/users/me/export`.

**Behavior on rejection:**

1. The handler returns `401 auth_failed` with `subCode: reauth_required` and message `"recent re-authentication required"`.
2. The SDK MUST NOT swallow this with a silent `getToken()` retry (see the §4.1.1 row for `auth_failed`); it MUST surface the typed `subCode` so the consumer's auth layer can trigger an interactive sign-in (e.g., NextAuth `signIn()` redirect).
3. After the user completes a fresh sign-in, the JWT `authTime` claim advances and the next `DELETE /api/users/me` call passes the gate.

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

| HTTP | `error.code` | `subCode` | When |
|------|--------------|-----------|------|
| 401 | `auth_failed` | `null` | Missing/malformed/invalid token. Also the expected response on a second DELETE attempt after a successful first call -- the token has been invalidated. |
| 401 | `auth_failed` | `reauth_required` | Recent-login re-auth gate failed: the session is valid but the `auth_time` claim is older than `REAUTH_MAX_AGE_SEC` (default 300 s), or the claim is absent (legacy session). The SDK MUST trigger interactive re-authentication; see Re-auth precondition above. |
| 429 | `rate_limited` | `null` | REST rate limit breached |
| 500 | `internal_error` | `null` | Transaction rolled back (the cascade failed and no data was deleted per `data-lifecycle.md` §3.4) |

**Note on 403:** This endpoint has no 403 path because it operates on `/users/me` -- the authenticated user is always "owner" of their own account. There is no cross-user deletion in v1.

#### Idempotency

Idempotent in the "equivalent final state" sense (§4.5). A second call after success returns `401 auth_failed` because the token no longer resolves to a valid user. The final state is "account deleted" in both cases.

#### Cascade reference

The full deletion cascade (User -> Account, Vehicle, Invite, Settings; Vehicle -> Drive, TripStop, Invite) and the transactional guarantees are defined normatively in [`data-lifecycle.md`](data-lifecycle.md) §3. This section does NOT re-specify them; the data-lifecycle contract is the single source of truth for the cascade and the audit log row shape.

#### Implementation notes

- The audit log row is written BEFORE the cascading DELETE, in the same transaction, per `data-lifecycle.md` §3.1.
- **The Next.js app serves this endpoint** per §10 DV-23 (RESOLVED 2026-05-08, MYR-69). The Go telemetry server has no User repository and no `DELETE /api/users/me` handler; the public API hostname proxies this path to the Next.js app, which executes the Prisma `$transaction` defined in `data-lifecycle.md` §3.1. The SDK is unaware of which process handles the request.
- WebSocket session cleanup (`data-lifecycle.md` §3.5): after the transaction commits, the telemetry server detects the vehicle deletion on its next DB read cycle and terminates any active WebSocket connections for those vehicles. The SDK observes this as a close code 1008 or 1001 on the WS, and the next `getToken()` call will fail.

---

### 7.7 `GET /api/users/me/export`

> **Anchored:** FR-10 (data-export companion to FR-10.1 deletion), GDPR Art. 15 (right of access), GDPR Art. 20 (portability), NFR-3.29; GDPR Art. 17 recent-auth corollary (symmetric with §7.6).

#### Purpose

Returns a JSON archive of every Prisma row owned by the authenticated user — the SDK's single entry point for GDPR Art. 15 / Art. 20 portability exports. The endpoint is the export companion to `DELETE /api/users/me` (§7.6); together they implement the data-export-then-delete flow GDPR requires before erasure. Phase A implementation: [myrobotaxi/react-frontend#259](https://github.com/myrobotaxi/react-frontend/pull/259) (Next.js handler).

The handler runs in the Next.js app per the same DV-23 routing decision that places `DELETE /api/users/me` and the §7.5 invite endpoints there: the public API hostname (`https://api.myrobotaxi.com/api/...`) proxies `/api/users/me/*` paths to the Next.js app. The Go telemetry server has no User repository and no export handler. The SDK is unaware of which process serves the request.

#### Re-auth precondition (MYR-76)

Symmetric with §7.6: this endpoint also requires that the caller's most recent fresh OAuth sign-in occurred within `REAUTH_MAX_AGE_SEC` (default 300 s). The decision to apply the gate to the export endpoint as well as the deletion endpoint was made because both endpoints surface the full ownership graph — a stolen Bearer token used against `/api/users/me/export` exfiltrates every owned vehicle, drive, GPS trail, and invite even though it cannot destroy them; the recent-auth corollary applies symmetrically. The mechanism and rejection behavior are identical to §7.6 (see that section); the SDK MUST surface `401 auth_failed` / `subCode: reauth_required` to the consumer's auth layer rather than swallowing it with `getToken()`.

#### Request

```
GET /api/users/me/export HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Accept: application/json
```

Authentication is the standard NextAuth session — Bearer token in the `Authorization` header (§3.1) or NextAuth session cookie. No request body, no path or query parameters.

#### Response — 200 OK

The response body is a JSON archive of every Prisma row owned by the caller, with all P1 columns decrypted at the crypto boundary (per [`data-classification.md`](data-classification.md) §3 — the encryption is transparent at the export boundary just as it is at the WebSocket boundary, NFR-3.25). OAuth credentials (`Account.access_token`, `Account.refresh_token`) are explicitly excluded — exporting the user's Tesla OAuth tokens would let an attacker impersonate the user against Tesla's Fleet API after the export, so the export contract treats them as a privileged credential boundary that the export does NOT cross.

The high-level shape is documented here; the canonical field-level schema lives in the Phase A implementation. Consumers MUST treat any field not enumerated here as advisory and branch on its presence, not its specific value.

| Top-level key | Description | Source |
|---------------|-------------|--------|
| `user` | The caller's User row (`id`, `email`, `name`, `image`, `createdAt`, `updatedAt`, `emailVerified`). | Prisma `User` |
| `settings` | The caller's Settings row, if present. | Prisma `Settings` |
| `vehicles` | Array of vehicles owned by the caller. Each entry includes the full `Vehicle` row with P1 GPS / destination / nav-route columns decrypted. | Prisma `Vehicle` |
| `drives` | Array of completed drives across all owned vehicles, with `routePoints` (JSONB GPS polyline) decrypted per NFR-3.23. | Prisma `Drive` |
| `tripStops` | Array of trip stops for owned vehicles. | Prisma `TripStop` |
| `invitesSent` | Invites the caller created (caller is `senderId`). | Prisma `Invite` |
| `invitesAccepted` | Invites the caller accepted (caller's email matches `email` and `status = accepted`). | Prisma `Invite` |
| `auditLogs` | AuditLog rows where `userId` equals the caller's user ID. | Prisma `AuditLog` |

**Explicitly excluded** from the export:

| Excluded | Why |
|----------|-----|
| `Account.access_token`, `Account.refresh_token` | Tesla OAuth credentials. Exporting them would extend the user's authorization surface beyond their session. The user can revoke and re-link their Tesla account through the normal account-settings flow. |
| `Account.expires_at`, `Account.token_type`, `Account.scope` | Companion fields tied to the OAuth credentials above. |
| Any field not owned by the caller | Cross-user data is filtered at the Prisma query boundary by `userId` / `senderId` / vehicle ownership. The export never includes another user's drives, invites, or audit rows. |

The response body's audit-log side effect is documented in [`data-lifecycle.md`](data-lifecycle.md) §4.2 as the `data_exported` action: one row per export with `targetType: user`, `targetId: <callerUserId>`, `initiator: user`, and `metadata: {vehicleCount, driveCount, inviteCount, auditCount}` (P0 counts only per Rule CG-DL-5; never PII / GPS / tokens). The audit row is retained indefinitely per NFR-3.29.

#### Response — error

Per the §4.1 error envelope:

| HTTP | `error.code` | `subCode` | When |
|------|--------------|-----------|------|
| 401 | `auth_failed` | `null` | Missing/malformed/invalid token. Same error mode as §7.6. |
| 401 | `auth_failed` | `reauth_required` | Recent-login re-auth gate failed (see Re-auth precondition above; symmetric with §7.6). |
| 429 | `rate_limited` | `null` | REST rate limit breached |
| 500 | `internal_error` | `null` | Decryption failure, DB read failure, or audit-log write failure. The export and the audit row are written in the same transaction; if the audit row fails, the response is `500` and no archive is returned. |

**Note on 403:** Same as §7.6 — the endpoint operates on `/users/me`, the authenticated user is always the owner of their own data, and there is no cross-user export in v1.

#### Idempotency

`GET` is naturally idempotent. Multiple successive calls return identical archives (modulo any updates the user made between calls), and each call writes a new `data_exported` audit row — that is intentional per the audit-log contract: the audit log records every export attempt that succeeded, not just the first one.

#### Implementation notes

- The handler runs in the Next.js app per DV-23 (RESOLVED 2026-05-08, MYR-69). The Go telemetry server has no User repository.
- The audit-log row MUST be written in the same Prisma `$transaction` as the export read. If the audit insert fails, the entire export is rolled back per the same atomicity rule that governs `DELETE /api/users/me` in `data-lifecycle.md` §3.4.
- Decryption of P1 columns at the crypto boundary uses the same `ENCRYPTION_KEY` and AES-256-GCM scheme as the WebSocket and REST snapshot paths (NFR-3.23, NFR-3.25). The export is NOT a separate decryption code path; it reuses the existing one.
- **Re-auth gate (RESOLVED 2026-05-10, MYR-76):** The recent-login re-auth precondition is now enforced symmetrically with §7.6. The Next.js handler reads `session.user.authTime` (the OIDC-style `auth_time` claim stamped by the NextAuth `jwt` callback on every fresh sign-in) and rejects with `401 auth_failed` / `subCode: reauth_required` when the claim is missing or older than `REAUTH_MAX_AGE_SEC` (default 300 s). See the Re-auth precondition subsection above and the resolution note in [`../architecture/requirements.md`](../architecture/requirements.md) §2.10.

---

### 7.8 Ride requests (P10 ride-hailing)

> **Anchored:** FR-9.1, FR-9.3, NFR-3.21, NFR-3.23.
> **Schema:** [`schemas/ride-request.schema.json`](schemas/ride-request.schema.json) — `RideRequest`, `RideRequestCreateRequest`, `RideRequestsListResponse`.
> **Persisted:** Go-owned `go_ride_requests` table (pickup/dropoff coordinates AES-256-GCM encrypt-only — [`data-classification.md`](data-classification.md) §1.9).
> **Reactive pair:** the WS `ride_request_created` / `ride_status_changed` summary frames ([`websocket-protocol.md`](websocket-protocol.md) §4.7–4.8), unicast to the two parties only.

The rider-facing surface (mounted as of **MYR-174**) plus the owner-facing surface (incoming feed + accept/decline, mounted as of **MYR-175**). All endpoints validate a bearer token first (`401 auth_failed` on missing/invalid) and return the `RideRequest` object (`$defs.RideRequest`) as a bare object on the single-resource paths, or the `RideRequestsListResponse` envelope on the list path. Field names/enums are byte-for-byte the schema.

#### Authorization model (v1 enforced vs deferred)

- **Create** derives `ownerId` from the target vehicle's owner and enforces a **vehicle-access check identical to `/snapshot` and `/drives`**: the caller must be able to see the vehicle (v1: `vehicle.userId == caller`). So in v1 a rider may only request a ride on a vehicle they own, and `ownerId == riderId`. **Broader shared-viewer requests (rider ≠ owner) are DEFERRED to the app-side sharing tiers**: they light up automatically when the server's vehicle-access set gains the viewer-merge pathway (`GetUserVehicles`, PLANNED MYR-91) — no change to this handler is required, only a wider access set. Unknown vehicle → `404 not_found`; visible-but-not-accessible → `403 vehicle_not_owned`. A rider may hold only **one OPEN instant ride** (MYR-230): a second **instant** create while one is open is `409 ride_active` and returns the existing ride for the client to adopt; **scheduled** creates are exempt (see the `POST` section below).
- **Detail (`GET {id}`)** is **party-only**: rider OR vehicle owner. A caller who is neither gets `404 not_found` (not `403`) so the server never confirms the existence of a ride the caller has no relation to.
- **Cancel** is **rider-only**. The owner is a party but cannot cancel → `403 permission_denied`. A non-party → `404`.
- **Accept / decline** are **owner-only**. The rider is a party but cannot decide → `403 permission_denied`. A non-party → `404`. (MYR-175)
- **Incoming feed** is scoped to the authenticated owner (`owner_id == JWT sub`) — no cross-owner reads are expressible. (MYR-175)
- `riderId` is always the JWT `sub` (never client-supplied); `id`, `status`, and all timestamps are server-assigned.

#### `POST /api/ride-requests`

Body is `RideRequestCreateRequest` (`vehicleId`, `pickup`, `dropoff` required; `passengerName`, `passengerPhone`, `scheduledFor` optional). The body is decoded **strictly** — unknown keys are `400 invalid_request` (schema `additionalProperties:false`). `pickup`/`dropoff` are validated as `RidePlace` (lat ∈ [-90,90], lng ∈ [-180,180], non-empty `label`). Responds **`201 Created`** with the full `RideRequest` and unicasts `ride_request_created` to the rider + owner.

**One active instant ride per rider (MYR-230).** An **instant** create (`scheduledFor` absent) is refused **`409 ride_active`** when the rider already holds an OPEN instant ride — one whose status is not terminal (`requested`, `accepted`, or any in-progress/tracking state; NOT `completed`/`declined`/`cancelled`). The 409 body carries that existing ride so the client adopts it instead of stacking a duplicate — see "409 `ride_active` body" below. **Scheduled creates (`scheduledFor` present) are EXEMPT**: a future reservation is not an active ride, so a rider may hold any number of scheduled rides plus one open instant ride, and an open scheduled ride never blocks a new instant one. The guard is enforced authoritatively by a **partial unique index** (`uq_go_ride_requests_active_instant_rider` on `rider_id WHERE scheduled_for IS NULL AND status IN (open states)`, migration 0004): two concurrent instant creates serialize in Postgres — exactly one INSERT wins, the loser's `23505` maps to the same `409 ride_active`. This is the create-side analogue of the guarded-transition race discipline (`UpdateStatusFrom`, below).

> **One-time dedup at migration (deploy caveat).** The guard postdates the create endpoint, so databases that predate it can hold riders with several concurrently-open instant rides (stale pre-guard test debris). Migration 0004 is self-cleaning: **before** creating the index it transitions every OLDER open instant ride per rider to `cancelled`, keeping only the MOST RECENT one (`ORDER BY created_at DESC, id DESC` — the list endpoints' total order; keeping newest matches rider expectation). The dedup mirrors the store's `UpdateStatusFrom` timestamp discipline (entering `cancelled` stamps neither `acceptedAt` nor `completedAt`; `updatedAt` is touched). **No `ride_status_changed` WS frames fire for these rows** — migrations run with no event bus; clients refetch via REST (FR-9.1/FR-9.2 reconciliation) and observe the stale rides as `cancelled`.

Errors: `400 invalid_request` (malformed/unknown-field body, bad place, bad `scheduledFor`), `401 auth_failed`, `403 vehicle_not_owned`, `404 not_found` (unknown vehicle), `409 ride_active` (rider already has an open instant ride), `500 internal_error`.

##### 409 `ride_active` body

Unlike every other error response, the `ride_active` 409 augments the standard error envelope (§4.1) with a sibling `activeRideRequest` object — the rider's existing open instant ride, byte-for-byte the `RideRequest` shape `GET /api/ride-requests/{id}` returns — so the SDK can adopt it into the pending/tracking UI rather than showing a decline:

```json
{
  "error": { "code": "ride_active", "message": "you already have an active ride request", "subCode": null },
  "activeRideRequest": { "id": "crr…", "riderId": "…", "status": "accepted", "pickup": { … }, "dropoff": { … }, "createdAt": "…", "updatedAt": "…" }
}
```

The nested `error` object is unchanged; a consumer that ignores `activeRideRequest` still receives a well-formed envelope and can re-fetch the rider's open ride via `GET /api/ride-requests`. In the extraordinarily rare case where the blocking ride reaches a terminal state between the rejected insert and the server's re-read, the server returns `409 ride_active` **without** `activeRideRequest`; the SDK re-syncs its list and MAY retry the create.

#### `GET /api/ride-requests`

The authenticated rider's own requests, newest first, cursor-paginated per §4.2 (`limit` default 20, max 100; opaque `cursor`). Returns the `RideRequestsListResponse` envelope (`items` always present — `[]` never `null`; `nextCursor` null on the final page). Ordering `createdAt DESC, id DESC` (§4.2.2).

Errors: `400 invalid_request` (`limit` out of range / malformed `cursor`), `401 auth_failed`, `500 internal_error`.

#### `GET /api/ride-requests/{id}`

Party-only (rider or vehicle owner). Returns the bare `RideRequest`. Errors: `401 auth_failed`, `404 not_found` (unknown id OR non-party — indistinguishable), `500 internal_error`.

#### `POST /api/ride-requests/{id}/cancel`

Rider-only. Legal only from `requested` or `accepted` → `cancelled`; any other current status is `409 conflict`. Responds `200 OK` with the updated `RideRequest` and unicasts `ride_status_changed`. Errors: `401 auth_failed`, `403 permission_denied` (owner/non-rider party), `404 not_found` (unknown id / non-party), `409 conflict` (illegal transition), `500 internal_error`.

#### `GET /api/ride-requests/incoming`

The owner's feed of **open** requests across their vehicles — status `requested` only, covering BOTH the on-demand and scheduled variants (a scheduled request is `requested` with `scheduledFor` set, not a separate status; the owner sheet forks on `scheduledFor` presence). Newest first, cursor-paginated per §4.2 with the same `RideRequestsListResponse` envelope and `(createdAt, id)` cursor as the rider list. Decided rows (accepted/declined/…) leave the feed by construction.

Errors: `400 invalid_request` (`limit` out of range / malformed `cursor`), `401 auth_failed`, `500 internal_error`.

> **Routing note:** the literal `/incoming` segment takes precedence over the `GET /api/ride-requests/{id}` wildcard in Go's `ServeMux`, so both routes coexist; a regression test pins this.

#### `POST /api/ride-requests/{id}/accept`

Owner-only. Legal only from `requested` → `accepted`; any other current status is `409 conflict`. Responds `200 OK` with the updated `RideRequest` (now carrying `acceptedAt`, stamped first-entry-only by the store) and unicasts `ride_status_changed` to both parties. **Dispatch seam (MYR-176):** a successful accept also publishes an internal `ride.accepted` event on the process event bus carrying the pickup/dropoff places and the booked-for passenger contact — the input the nav-dispatch pipeline subscribes to for the Tesla `navigation_gps_request` push. The event is internal-only: it never reaches the WS broadcast path, and no Tesla call happens **synchronously on this endpoint** — the push runs asynchronously off the bus.

Errors: `401 auth_failed`, `403 permission_denied` (rider/non-owner party), `404 not_found` (unknown id / non-party), `409 conflict`, `500 internal_error`.

#### Dispatch outcome (MYR-176)

When an owner accepts, the server asynchronously pushes the rider's **pickup** into the vehicle's Tesla navigation (an unsigned `navigation_gps_request` with `order: 1` — Tesla's 1-based remote-nav order integer where `1` replaces the current trip so the pickup becomes the active destination — via the §7.9 command Executor). This does **not** change the main lifecycle status — `accepted` stays `accepted` (the `accepted → enroute` transition remains MYR-177 live-tracking territory). Instead the outcome is recorded as an **orthogonal, optional annotation** on the `RideRequest`, surfaced on the party-only detail (`GET /api/ride-requests/{id}`):

- **`dispatchStatus`** — `"sent"` (the vehicle accepted the nav push), `"failed"` (terminal error, or exhausted the bounded retry — e.g. `key_not_paired`, `permission_denied`, or transport/asleep after retries), or `"skipped"` (the `DISPATCH_ENABLED` kill-switch was off). Absent until the push resolves.
- **`dispatchedAt`** — RFC 3339 instant the dispatch was attempted (also the exactly-once latch: a re-delivered accept never double-pushes). Absent until claimed.

Both fields are **optional and additive** (older clients ignore them); the internal failure reason code is not exposed on the wire. There is **no** new WS frame for dispatch — clients that render it refetch the REST detail after the `accepted` `ride_status_changed`.

> **Internal failure reason codes (`dispatch_error`, server-side only — NOT on the wire).** When `dispatchStatus` is `"failed"` the server records an opaque reason code in the `dispatch_error` column (data-classification.md §1.9) for operability. The set is the typed command codes (`key_not_paired`, `permission_denied`, `vehicle_asleep`, `command_failed`, `invalid_request`, `internal_error`) plus dispatch-local codes:
> - `vehicle_unresolved` — the vehicle's VIN could not be resolved (row gone, or transient lookup failure exhausted the bounded retry).
> - `token_expired` — the owner has a Tesla token on file but it is expired and could not be refreshed; **the owner must re-link** their Tesla account. Distinct from `token_unavailable` so this actionable case is not conflated with "never linked".
> - `token_unavailable` — no usable Tesla token could be obtained (account **never linked**, or a transient lookup failure exhausted retries).
> - `transport_unconfigured` — the tesla-http-proxy command transport is not configured; a permanent misconfiguration (not retried).
> - `dispatch_canceled` — the per-event context was canceled/timed out mid-resolution.
> - `dispatch_interrupted` — the dispatch was claimed (`dispatched_at` stamped) but the process died (crash/SIGTERM) before recording an outcome; the startup reconciler resolved the orphaned row (see below).
>
> Resolution steps (VIN + token) run under the same bounded retry policy as the command: transient failures are retried; only well-identified permanent conditions (`token_expired`, `token_unavailable` on a never-linked account, `vehicle_unresolved` not-found, `transport_unconfigured`) short-circuit.

> **Startup reconciliation of interrupted dispatches.** The `dispatched_at` claim latch is stamped BEFORE the nav push runs, so a crash or SIGTERM in the claim→record window leaves a row with `dispatched_at` set and `dispatch_status` NULL — stuck "claimed but unresolved" forever, invisible to monitoring, and never re-attempted (the exactly-once latch blocks a second claim). On startup the dispatcher runs a one-shot reconciliation pass (`internal/dispatch` `Reconcile`, wired in `cmd/telemetry-server/dispatch_wiring.go`) that finds every such row **older than the per-event OverallTimeout** (so a genuinely in-flight dispatch is never touched) and records it `failed` / `dispatch_interrupted`. **We resolve-as-failed rather than re-dispatch on purpose:** the process died at an unknown point (the push may or may not have reached the car), the accept is likely stale by restart, and a late nav push to a car that has since moved is worse than an honest, alertable "interrupted" outcome. The reconciler is best-effort (log-and-continue; a failure never blocks server startup).

#### `POST /api/ride-requests/{id}/decline`

Owner-only. Legal only from `requested` → `declined`; any other current status is `409 conflict`. Responds `200 OK` with the updated `RideRequest` and unicasts `ride_status_changed`. Same error catalog as accept (minus the dispatch seam).

#### Lifecycle transition matrix

The main `RideRequestStatus` lifecycle is monotonic; the reschedule negotiation is a separate sub-state (`rescheduleStatus`, MYR-192). Every mutation endpoint enforces legality in the **handler** (the store stays a dumb persistence layer) and rejects an illegal transition with `409 conflict`. Rows are the current status; a cell names the endpoint that performs the transition (and the story that owns it), or `409` when no legal transition exists from that state.

| From \ To | `accepted` | `declined` | `enroute` | `arrived` | `completed` | `cancelled` |
|-----------|-----------|-----------|-----------|-----------|-------------|-------------|
| `requested` | `accept` (owner, MYR-175) | `decline` (owner, MYR-175) | `409` | `409` | `409` | `cancel` (rider, MYR-174) |
| `accepted` | — | `409` | dispatch (MYR-176) | `409` | `409` | `cancel` (rider, MYR-174) |
| `enroute` | `409` | `409` | — | live-tracking (MYR-177) | `409` | `409` |
| `arrived` | `409` | `409` | `409` | — | live-tracking (MYR-177) | `409` |
| `declined` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |
| `completed` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |
| `cancelled` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |

- **Atomicity / race semantics (MYR-174/175):** every transition executes as a single guarded UPDATE (`WHERE id = … AND status = ANY(<legal-from>)` — `store.RideRequestRepo.UpdateStatusFrom`), so concurrent conflicting mutations serialize in the database: **exactly one wins; every loser receives `409 conflict`** even if its pre-check read saw a legal state (e.g. rider-cancel racing owner-decline, or an owner double-tapping accept from two devices). The WS `ride_status_changed` frame and the `ride.accepted` dispatch event are published only by the winning write — the dispatch seam is exactly-once per accept by construction.
- **MYR-174 (this story)** implements only the two `→ cancelled` transitions. Cancel from `enroute`/`arrived` (ride in progress) and from any terminal state is `409` — cancel is legal only from `{requested, accepted}`.
- **MYR-175** implements `requested → accepted` / `requested → declined` (owner-only endpoints above). Accepting or declining a ride already past `requested` — including one the rider cancelled while the owner sheet was open — is the race the `409` protects.
- **Reschedule confirm/decline (owner)** is NOT part of MYR-175: the rider-side propose endpoint (`ProposeReschedule`) has no HTTP surface yet, so an owner resolve endpoint would be unreachable dead code. The whole reschedule negotiation (propose + resolve, `rescheduleStatus` sub-state) ships together in **MYR-192**; the store layer (`ResolveReschedule`) is already in place.
- **MYR-176** performs the nav push on accept but records it as an orthogonal `dispatchStatus`/`dispatchedAt` annotation (see "Dispatch outcome" above) — it does **not** advance the main lifecycle. The `accepted → enroute → arrived → completed` transitions remain **MYR-177** live-tracking territory; until it lands, those endpoints do not exist and the states are unreachable from the server.
- Every transition that succeeds emits a `ride_status_changed` summary frame to the two parties.

---

### 7.9 `POST /api/vehicles/{vehicleId}/command/{name}` (Tesla vehicle commands)

> **Anchored:** FR-11.x (vehicle actuation), NFR-3.21 (ownership enforcement), NFR-3.22 (TLS in transit). Implemented by MYR-180.

#### Purpose

The owner-only actuation surface. It sends a signed Tesla Fleet vehicle command (lock, climate, charge, trunk, remote start, horn, lights) or an unsigned navigation/dispatch command to the caller's vehicle. It is the foundation for P11 (per-command issues MYR-181–183) and P10 dispatch (MYR-176 uses `navigation_gps_request`).

#### Transport / architecture

Modern Fleet API vehicle commands require end-to-end command signing with a virtual key the owner enrolls in the car. The server does **not** embed the signing library in-process: it forwards commands to the **tesla-http-proxy sidecar** it already runs for `fleet_telemetry_config` pushes (`TESLA_PROXY_URL`). The proxy signs signer-required commands with the P-256 virtual key (which lives ONLY in the proxy's config, never in this process) and forwards unsigned commands (`navigation_request`) straight to the Fleet API. Session caching, re-handshake, and wake are the proxy's job. Full decision record + ops runbook: [`docs/operations/vehicle-commands.md`](../operations/vehicle-commands.md).

#### Request

```
POST /api/vehicles/{vehicleId}/command/{name} HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
Content-Type: application/json

{ ...command parameters (may be empty)... }
```

- `{vehicleId}` is the cuid (NOT the VIN); the server resolves it to the VIN server-side.
- `{name}` is the command name (identical to the Tesla Fleet command name).
- The body is the command's typed parameters; parameterless commands take an empty body.

#### Command catalog

| `{name}` | Scope | Signed | Params |
|----------|-------|--------|--------|
| `door_lock` | `vehicle_cmds` | yes | — |
| `door_unlock` | `vehicle_cmds` | yes | — |
| `auto_conditioning_start` | `vehicle_cmds` | yes | — |
| `auto_conditioning_stop` | `vehicle_cmds` | yes | — |
| `set_temps` | `vehicle_cmds` | yes | `driver_temp` (°C, required), `passenger_temp` (°C, optional; mirrors driver) |
| `charge_start` | `vehicle_charging_cmds` | yes | — |
| `charge_stop` | `vehicle_charging_cmds` | yes | — |
| `set_charge_limit` | `vehicle_charging_cmds` | yes | `percent` (int 50–100) |
| `actuate_trunk` | `vehicle_cmds` | yes | `which_trunk` (`"front"` \| `"rear"`) |
| `remote_start_drive` | `vehicle_cmds` | yes | — |
| `honk_horn` | `vehicle_cmds` | yes | — |
| `flash_lights` | `vehicle_cmds` | yes | — |
| `navigation_gps_request` | `vehicle_cmds` | no | `lat`, `lon` (required), `order` (int, default 1) — the lat/long dispatch command (MYR-176) |
| `navigation_request` | `vehicle_cmds` | no | `value` (string address or maps URL) — text/share destination |

There is no `vehicle_remote_start` Fleet scope (that is the legacy Owner API); `remote_start_drive` is a `vehicle_cmds` command. `navigation_request`/`navigation_gps_request` are UNSIGNED: Tesla processes them server-side, so the proxy forwards them to the Fleet API rather than signing them.

#### Scope gating

Each command maps to a Fleet OAuth scope. The server parses the granted scopes from the owner's Tesla access-token JWT (`scp` claim). If the token demonstrably lacks the required scope, the server returns `403 permission_denied` **without calling Tesla**. If the scopes cannot be parsed, enforcement is deferred to Tesla.

#### Wake + session policy

If the vehicle is asleep/offline the executor issues a wake and retries the command with a bounded budget (default: 3 wake+retry attempts, ~2 s backoff each). If the budget is exhausted it returns the transient `503 vehicle_asleep`, which the SDK retries with backoff. A signing-session counter/anti-replay error triggers one silent re-handshake retry (the proxy owns the session cache); a second counter error surfaces as `502 command_failed`.

#### Rate limiting

Per-vehicle command cooldown (token bucket, default ~1 command / 2 s with a small burst). Breaches return `429 rate_limited` with `Retry-After`.

#### Success response — `200 OK`

```json
{ "status": "applied", "command": "door_lock", "vin": "***0001" }
```

The VIN is redacted to the last 4 (P0 rule; the full VIN never leaves the server). Command parameters (e.g. navigation coordinates, P1) are never persisted and never logged.

#### Errors

| HTTP | code | Cause |
|------|------|-------|
| 400 | `invalid_request` | Unknown command name, malformed body, or a parameter that failed validation (bad range, wrong type). |
| 401 | `auth_failed` | Missing/invalid caller bearer token, or the owner's Tesla token is expired and cannot be refreshed. |
| 403 | `vehicle_not_owned` | Caller is not the vehicle's owner. |
| 403 | `permission_denied` | The owner's Tesla token lacks the command's scope (preflight), or Tesla rejected for access. |
| 403 | `key_not_paired` | Virtual key not enrolled on the vehicle (pre-pairing default), or signing transport not configured. |
| 404 | `not_found` | Unknown `vehicleId`. |
| 429 | `rate_limited` | Per-vehicle command cooldown breached. |
| 502 | `command_failed` | Vehicle returned `result:false`, counter error survived re-handshake, or Fleet/proxy failure. |
| 503 | `vehicle_asleep` | Vehicle did not wake within the retry budget (retry with backoff). |

#### Pre-pairing behavior (MYR-115 not yet done)

Until the owner pairs the virtual key, every **signer-required** command resolves to `403 key_not_paired`. The endpoint is always mounted (never a 404) and nothing crashes when the proxy/key is absent; when `TESLA_PROXY_URL` is unset the server logs a clear "signing disabled" line at startup and returns `key_not_paired`.
### 7.10 Authentication (identity module — MYR-193)

> **Anchored:** FR-6.1, FR-6.2, MYR-193. Design record: `docs/architecture/adr-001-identity-module.md`.

The identity module (`internal/identity`) is the server's own token issuer. These endpoints are **pre-authentication** (no Bearer header) and are protected by a per-IP rate limit (§4.1.2). All bodies are `application/json`. Errors use the standard envelope (§4.1); auth failures collapse to `401 auth_failed` with a generic message — there is no reuse/linkage oracle and no PII in any error.

The endpoints are mounted only when an ES256 signing key is configured (`AUTH_ES256_PRIVATE_KEY`); `POST /api/auth/apple` additionally requires `APPLE_NATIVE_CLIENT_ID` and otherwise returns `404 not_found`.

#### 7.10.1 `POST /api/auth/apple`

Validate a native Sign in with Apple identity token and issue a token pair.

**Request**

```
POST /api/auth/apple
Content-Type: application/json
```

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `identityToken` | string | Yes | The Apple identity token (JWT) from `ASAuthorizationAppleIDCredential.identityToken`. |
| `fullName` | string | No | Apple returns the name only on the first authorization; the client forwards it so it can be persisted on first sign-in. |
| `email` | string | No | Advisory only — the server links on the token's verified-email claim, never this field. |
| `nonce` | string | No | If present, must equal the token's `nonce` claim. |

Server validation: RS256 signature against Apple's JWKS (`https://appleid.apple.com/auth/keys`, cached with `kid` rotation), `iss=https://appleid.apple.com`, `aud=APPLE_NATIVE_CLIENT_ID`, `exp`/`iat`. The user is resolved (and bound on first sign-in) per ADR-001 §4: existing `apple_sub` binding → config bootstrap override → verified-email match against `"User"` → fresh `go_users` mint.

**Response — 200 OK**

```json
{
  "accessToken": "<ES256 JWT>",
  "expiresIn": 3600,
  "refreshToken": "<opaque>",
  "user": { "id": "cmmgr4b1p0005l104ifpctlg8", "name": "Ada Lovelace", "email": "ada@example.com" }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `accessToken` | string | P1 | ES256 JWT, `sub`=user CUID, `iss=myrobotaxi`, `aud=telemetry`, ~1h. Use as the Bearer token everywhere else. |
| `expiresIn` | integer | P0 | Access-token lifetime in seconds. |
| `refreshToken` | string | P1 | Opaque single-use token; store securely (Keychain), send only to `/api/auth/refresh` \| `/revoke`. |
| `user.id` | string | P0 | User CUID. |
| `user.name` / `user.email` | string | P1 | Present when known; omitted otherwise. |

**Response — error**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `identityToken`, malformed / unknown-field body |
| 401 | `auth_failed` | Apple token invalid (signature/iss/aud/exp/nonce) |
| 404 | `not_found` | Apple sign-in not enabled (`APPLE_NATIVE_CLIENT_ID` unset) |
| 429 | `rate_limited` | Per-IP cap exceeded |

#### 7.10.2 `POST /api/auth/refresh`

Rotate a refresh token (single-use). Presenting a spent or revoked token is treated as theft: the whole family is revoked (`401`).

**Request** — `{ "refreshToken": "<opaque>" }`

**Response — 200 OK** — same shape as §7.9.1 (a fresh `accessToken` + a new `refreshToken`; `user` carries at least `id`). The previous refresh token is now invalid.

**Response — error**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `refreshToken` / malformed body |
| 401 | `auth_failed` | Unknown, expired, spent, or revoked token (reuse revokes the family) |
| 429 | `rate_limited` | Per-IP cap exceeded |

#### 7.10.3 `POST /api/auth/revoke`

Revoke the refresh-token family a token belongs to (sign-out on this device lineage).

**Request** — `{ "refreshToken": "<opaque>" }`

**Response — 204 No Content** — always, for a well-formed request, whether or not the token existed (no existence oracle). `400 invalid_request` for a missing field; `429 rate_limited` on cap breach.

#### 7.10.4 `GET /api/auth/.well-known/jwks.json`

Public JSON Web Key Set (RFC 7517) of the ES256 verification keys. Public, cacheable (`Cache-Control: public, max-age=3600`). Consumers resolve an access token's `kid` header to a key here to verify the signature.

**Response — 200 OK**

```json
{ "keys": [ { "kty": "EC", "crv": "P-256", "x": "<b64url>", "y": "<b64url>", "use": "sig", "alg": "ES256", "kid": "<thumbprint>" } ] }
```

All JWKS fields are P0 (public by definition). The `kid` is the RFC 7638 thumbprint of the key.

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
| **DV-20** | **RESOLVED** | SDK-surface REST endpoints not yet mounted on the Go server | **RESOLVED — all four Go-server-owned §7 endpoints are mounted.** `GET /api/vehicles` (§7.0) landed in MYR-91 (2026-05-10); `GET /api/vehicles/{vehicleId}/snapshot` (§7.1) and `GET /api/vehicles/{vehicleId}/drives` (§7.2) landed in MYR-133 (2026-06-03); `GET /api/drives/{driveId}/route` (§7.4) landed in PR #260 (`DriveRouteHandler`, existing routePoints column + decryption); `GET /api/drives/{driveId}` (§7.3) landed in MYR-130 (2026-07-02) via `internal/telemetry/drive_detail_handler.go` backed by `DriveRepo.GetByID`. (The §7.5 invite endpoints and §7.6 `DELETE /api/users/me` are NOT in DV-20's scope -- per DV-23 (RESOLVED 2026-05-08, MYR-69) they are served by the Next.js app and are tracked under MYR-70 / MYR-71 / MYR-73 instead. The Go server returning 404 on those paths is the terminal behavior, not a transitional one.) | (Resolved.) All handlers enforce bearer auth + ownership + role-based `internal/mask` projection at the handler layer per §5.1; the shared-enum slice (REST-only `not_found` + `invalid_request` on `ErrorPayload.code`, plus `reauth_required` on `subCode`) was completed by MYR-98 on 2026-05-15. A route-surface regression test (`cmd/telemetry-server/wiring_routes_test.go`) guards against a contract route silently losing its mount. | FR-3.3, FR-3.4, NFR-3.5, §6, §7 | (Resolved — see MYR-91 / MYR-133 / PR #260 / MYR-130.) |
| **DV-21** | **New** | `service_unavailable` code reserved but not emitted | v1 does not emit `503 service_unavailable`. The code is reserved in this contract for forward-compat. | Server begins emitting `503 service_unavailable` during maintenance windows and graceful-shutdown states, with a `Retry-After` header. SDK error catalog already recognizes the code from day one. | NFR-3.10, §4.1.1 | `MYR-XX Emit 503 service_unavailable during graceful shutdown + maintenance` |
| **DV-22** | **New** | REST rate limit not enforced | No per-user REST rate limit is configured in [`internal/config/defaults.go`](../../internal/config/defaults.go) or wired through the server. The 120 req/min target in §4.1.2 is a PLANNED default, not an enforced value. | Add `WebSocketConfig.RestRateLimitPerMinutePerUser` (default 120) in `internal/config/defaults.go`. Implement a token-bucket rate limiter in the REST middleware keyed by `userId`. Breach returns `429 rate_limited` with a `Retry-After` header. Independent of `MaxConnectionsPerUser` (which governs concurrent WS sessions, not REST rps). | NFR-3.6, §4.1.2 | `MYR-XX Implement per-user REST rate limit (120 req/min default)` |
| **DV-23** | **RESOLVED** | Invite endpoints + `DELETE /api/users/me` handler location | The Invite table is Prisma-owned per `data-classification.md` §1.6; `internal/store/` has no `InviteRepo`. The three invite endpoints (§7.5) and the user self-deletion endpoint (§7.6) were PLANNED with two compatible implementation paths: (1) add an `InviteRepo` (and a User-deletion path) to the Go telemetry server that reads the Prisma-managed tables, or (2) serve these endpoints from the Next.js app with edge routing. | **RESOLVED 2026-05-08 (MYR-69): Next.js app owns `DELETE /api/users/me` and the §7.5 invite endpoints; Go telemetry server has Insert-only AuditLog access via raw pgx.** Rationale: aligns with the existing normative statement in `data-lifecycle.md` §3.4 that the Next.js app layer initiates the deletion transaction; sidesteps cross-process token/session invalidation; matches the Prisma-owned-table precedent (Vehicle, Drive, Account); avoids introducing a second migration toolchain in the Go repo just for the AuditLog table. The public API hostname (`https://api.myrobotaxi.com/api/...`) proxies invite + user-deletion paths to the Next.js app and snapshot/drives/drive-route paths to the Go telemetry server. The SDK calls a single base URL regardless of which process serves the request. Implementation follow-ups: MYR-70 (Next.js handler for invites), MYR-71 (Next.js handler for `DELETE /api/users/me`), MYR-72 (AuditLog Prisma model + Insert-only Go pgx writer), MYR-73 (edge routing config). | FR-5.1, FR-5.2, FR-5.3, FR-10.1, FR-10.2, NFR-3.29, §7.5, §7.6 | (Resolved -- see MYR-70 / MYR-71 / MYR-72 / MYR-73 for implementation follow-ups.) |

### Divergence management rules

Same as [`websocket-protocol.md`](websocket-protocol.md) §10 divergence management rules (one-way door, closing rules, RESOLVED-with-implementation-pending, amendment divergences). DV-NN IDs are globally unique across both catalogues -- MYR-12 intentionally starts at DV-19 to avoid collision with the DV-01 through DV-18 IDs owned by the WebSocket contract.

---

## 11. Change log

| Date | Change | Author |
| 2026-07-10 | **One active instant ride per rider — `POST /api/ride-requests` `409 ride_active` guard ([MYR-230](https://linear.app/myrobotaxi/issue/MYR-230)).** An instant create is now refused `409 ride_active` when the rider already holds an OPEN instant ride (status not terminal); **scheduled rides are exempt** (a rider may hold many scheduled rides plus one open instant ride, and an open scheduled ride never blocks a new instant one). The guard is authoritative via a **partial unique index** (`uq_go_ride_requests_active_instant_rider` on `rider_id WHERE scheduled_for IS NULL AND status IN ('requested','accepted','enroute','arrived')`, migration 0004) — two concurrent instant creates serialize in Postgres, exactly one wins, the loser's `23505` maps to the same 409 (create-side analogue of the `UpdateStatusFrom` race discipline). **Migration 0004 is self-cleaning:** before creating the index it cancels every OLDER open instant ride per rider (pre-guard production debris would otherwise abort the `CREATE UNIQUE INDEX`), keeping the most recent by `created_at DESC, id DESC`; the dedup fires no `ride_status_changed` WS frames (migrations run with no event bus — clients refetch via REST). The 409 body augments the standard error envelope with a sibling **`activeRideRequest`** (`$defs.RideRequest`, the `GET /api/ride-requests/{id}` shape) so the client adopts the existing ride instead of surfacing a decline (§7.8 new "409 `ride_active` body"). New **REST-only** shared-catalog code `ride_active` (409) added to §4.1.1, §4.1.1.a, the §4.1.1.b emission audit, `wserrors` + the reachability matrix, and the `schemas/ws-messages.schema.json` `ErrorPayload.code` enum (REST-only-on-the-wire). Implementation: `internal/store/ride_request_{repo,queries,errors}.go` (partial index + `GetActiveInstantByRider` + 23505→`ErrRideRequestActive`), `internal/telemetry/ride_request_{handler,types}.go` (pre-check + race backstop + `activeRideRequest` body), `cmd/telemetry-server/ride_request_adapters.go`. No data-classification change (adopted-ride coordinates go only to the ride's own rider, mirroring GET; never logged). **Contracts follow-up:** the shared error-envelope enum in `myrobotaxi/contracts` needs an additive `ride_active` entry (tolerant enum, patch tag) — see PR body. | go-engineer |
|------|--------|--------|
| 2026-07-10 | **Mounted the Tesla vehicle-command surface — P11 actuation + P10 dispatch foundation ([MYR-180](https://linear.app/myrobotaxi/issue/MYR-180)).** New §7.9 `POST /api/vehicles/{vehicleId}/command/{name}` (owner-only): a signed Tesla Fleet vehicle-command proxy seeded with the P11 commands (door_lock/unlock, auto_conditioning_start/stop, set_temps, charge_start/stop, set_charge_limit, actuate_trunk, remote_start_drive, honk_horn, flash_lights) and the P10 dispatch commands (navigation_gps_request for lat/long + navigation_request for text/share). §6 catalog gains a row; §7.9 documents the command catalog, scope gating, wake+session policy, per-vehicle cooldown, and pre-pairing behavior. Three new **REST-only** shared-catalog codes — `key_not_paired` (403), `vehicle_asleep` (503, transient), `command_failed` (502) — added to §4.1.1, §4.1.1.a, `wserrors`, the reachability matrix, and the `schemas/ws-messages.schema.json` `ErrorPayload.code` enum (REST-only-on-the-wire); `permission_denied` flips from PLANNED to Implemented (scope gating). **Transport decision:** the server reuses the tesla-http-proxy sidecar it already runs for `fleet_telemetry_config` (`TESLA_PROXY_URL`) rather than embedding the vehicle-command library in-process — the P-256 command key stays ONLY in the proxy config, never in this process (decision record + ops runbook: [`../operations/vehicle-commands.md`](../operations/vehicle-commands.md)). **Merge-safe before pairing (MYR-115):** the route is always mounted, every signer-required command resolves to `key_not_paired` until the owner pairs, and startup never crashes when the proxy/key is absent. Implementation: `internal/commands/` (registry, executor with wake/counter retry, scope parsing, ProxyTransport), `internal/telemetry/vehicle_command_handler.go` (+ token/cooldown), wired in `cmd/telemetry-server/wiring.go`. No stored-data / data-classification change (command params are transit-only, never persisted; VIN redacted, params never logged). | go-engineer |
| 2026-07-10 | **Nav-dispatch on accept — P10 ride-hailing ([MYR-176](https://linear.app/myrobotaxi/issue/MYR-176), stacked on MYR-175/180).** A new async pipeline (`internal/dispatch`) subscribes to the internal `ride.accepted` seam and pushes the rider's **pickup** into the vehicle's Tesla navigation (unsigned `navigation_gps_request`, `order: 1` — replace-trip, Tesla's 1-based remote-nav order — via the §7.9 command Executor). §7.8 gains a **"Dispatch outcome"** subsection: two **optional, additive** `RideRequest` fields — `dispatchStatus` (`sent`/`failed`/`skipped`) and `dispatchedAt` — surfaced on the party-only detail. **No new lifecycle status** (`accepted` stays `accepted`; matrix note updated so MYR-176 is an annotation, not the `accepted → enroute` transition, which stays MYR-177) and **no new WS frame** (clients refetch detail). Policies: exactly-once per ride (`dispatched_at` claim latch), bounded retry (2× w/ backoff on transport/asleep; `key_not_paired`/`permission_denied` terminal), `DISPATCH_ENABLED` kill-switch (→ `skipped`), one redacted-VIN audit line per attempt. New nullable columns `dispatch_status`/`dispatched_at`/`dispatch_error` on `go_ride_requests` (migration 0005; data-classification.md §1.9, all P0). Contracts: optional fields added to `ride-request.schema.json` `$defs.RideRequest`. Implementation: `internal/dispatch/*`, `internal/store/ride_request_dispatch.go`, `internal/telemetry/tesla_token_resolver.go`, wired in `cmd/telemetry-server/dispatch_wiring.go`. | go-engineer |
| 2026-07-09 | **Mounted the owner-facing ride-request surface — P10 ride-hailing ([MYR-175](https://linear.app/myrobotaxi/issue/MYR-175), stacked on MYR-174).** §7.8 extended with `GET /api/ride-requests/incoming` (owner's open-`requested` feed — on-demand + scheduled variants — same envelope + `(createdAt, id)` cursor as the rider list; literal segment wins over the `{id}` wildcard, regression-tested) and `POST /api/ride-requests/{id}/accept` / `/decline` (owner-only; legal only from `requested`, everything else `409 conflict` per the §7.8 matrix — matrix note updated from planned to implemented). Accept publishes the internal `ride.accepted` **dispatch seam** event (pickup/dropoff places + booked-for passenger contact) that MYR-176 subscribes to for the Tesla `navigation_request` push — internal-only, never broadcast, no Tesla calls in this story. Both decisions unicast `ride_status_changed` to the two parties. **Reschedule confirm/decline deliberately deferred to MYR-192**: the rider-side propose endpoint has no HTTP surface yet, so an owner resolve endpoint would be unreachable; the store's `ResolveReschedule` is in place. §6 catalog + §4.1.1.b emission audit extended (`permission_denied` rider-attempts-decision row, `conflict` owner-decision row). Implementation: `internal/telemetry/ride_request_owner_handler.go`, `mutateStatus` refactor in `ride_request_handler.go`, routes in `cmd/telemetry-server/wiring.go`. No schema changes. | go-engineer |
| 2026-07-09 | **Mounted the rider-facing ride-request surface — P10 ride-hailing ([MYR-174](https://linear.app/myrobotaxi/issue/MYR-174)).** New §7.8: `POST /api/ride-requests` (create; strict `additionalProperties:false` body, derives `ownerId` + vehicle-access check identical to `/snapshot`, `201` + full `RideRequest`), `GET /api/ride-requests` (rider's own cursor-paginated list — `createdAt DESC, id DESC`, keyset cursor over `(createdAt, id)`; §4.2.2 updated), `GET /api/ride-requests/{id}` (party-only; non-party → `404` to avoid existence leak), `POST /api/ride-requests/{id}/cancel` (rider-only; `requested`/`accepted` → `cancelled`, else `409 conflict`). Added the full lifecycle **transition matrix** (§7.8) including the MYR-175 accept/decline and MYR-176/177 dispatch rows (unimplemented transitions return `409`). New shared-catalog code **`conflict`** (HTTP 409; §4.1.1, §4.1.1.a) — REST-only, added to `wserrors` + the reachability matrix; never emitted over WS. §6 endpoint catalog + §4.1.1.b emission audit extended. **Authorization enforced vs deferred:** v1 requires the caller to own the vehicle (so `ownerId == riderId`); broader shared-viewer requests (rider ≠ owner) are deferred to the app-side sharing tiers and light up when `GetUserVehicles` gains viewer-merge (PLANNED MYR-91) with no handler change. Reactive pair is the WS `ride_request_created` / `ride_status_changed` summary frames (websocket-protocol.md §4.7–4.8), per-party unicast. Implementation: `internal/telemetry/ride_request_{types,wire,handler,read_handler}.go`, `internal/store/ride_request_repo_page.go`, `internal/ws/{ride_broadcast.go,hub.go SendToUsers}`, `internal/events/ride_events.go`, wired in `cmd/telemetry-server`. No `ride-request.schema.json` change — the shapes landed in contracts v0.9.0. | go-engineer |
| 2026-07-02 | **Mounted `GET /api/drives/{driveId}` on the Go server — DV-20 fully RESOLVED ([MYR-130](https://linear.app/myrobotaxi/issue/MYR-130)).** Closes the last unmounted DV-20 endpoint (drive detail, FR-3.4). Implemented in `internal/telemetry/drive_detail_handler.go` + `drive_detail_types.go`, backed by the wide `DriveRepo.GetByID` read (appropriate for a detail endpoint) via a new `driveDetailAdapter` in `cmd/telemetry-server/adapters.go`; wired at `GET /api/drives/{driveId}` in `wiring.go` with the same bearer-auth + ownership (`VehicleRepo.GetByID`) + role-based `DriveDetail` mask flow as the drive-route endpoint. Response is the `DriveDetail` object per §7.3 / §8 — every FR-3.4 field EXCEPT `routePoints` (served by §7.4). Error catalog: 400 `invalid_request` (empty driveId), 401 `auth_failed`, 403 `vehicle_not_owned`, 404 `not_found`, 500 `internal_error`. **Mask/OpenAPI drift fix:** `date` (required in the OpenAPI `DriveDetail` component and present in the §7.3 / fixture bodies) was missing from the §5.2.3 mask allow-list — masking would have stripped it and broken conformance; `driveDetailFields` in `internal/mask/tables.go` and the §5.2.3 table are updated in lockstep to include `date` (P0). Added `Server.ClientHandler()` accessor (`internal/server/server.go`) and a route-surface regression test (`cmd/telemetry-server/wiring_routes_test.go`) that asserts every SDK-contract REST route is mounted (unauthenticated request returns 401/400, never 404). §2.1 DV-20 callout, §6 endpoint-catalog status note, and the §10 DV-20 row flip to RESOLVED. No wire-shape / envelope / OpenAPI changes beyond the additive `date` mask fix — the DriveDetail contract was locked at MYR-12. | go-engineer |
| 2026-06-07 | **`GET /api/vehicles/{vehicleId}/drives` items now include `fsdMiles` / `fsdPercentage` ([MYR-152](https://linear.app/myrobotaxi/issue/MYR-152)).** §5.2.2 mask table extended with the two P0 FSD stats (owner + viewer both see them — they already appear on `DriveDetail`); §7.2 response example updated to show a drive with FSD usage and a second drive with `0`. Unlike the location fields, both are **always present** (non-nullable, default `0`). OpenAPI `DriveSummary` schema gains `fsdMiles` + `fsdPercentage` (`required`, `x-classification: P0`), matching their `DriveDetail` siblings; the schema description no longer lists them among omitted fields. Additive, non-breaking → minor contract bump. Implementation: `internal/store/queries.go` `driveSummarySelectColumns` extended; `internal/store/drive_repo_list.go` `DriveSummaryRow` + `scanDriveSummaryRow` extended; `internal/telemetry/vehicle_drives_types.go` `DriveListItem` + `driveSummary` + `toMaskMap` + `buildDriveSummary` extended; `internal/mask/tables.go` `driveSummaryFields` extended; `cmd/telemetry-server/adapters.go` `driveListerAdapter` extended. No store-write-path changes — `DriveRepo.Create` / `Complete` already populate the columns. Pairs with [MYR-151](https://linear.app/myrobotaxi/issue/MYR-151) (FSD baseline fix) which makes the values non-zero. | go-engineer |
| 2026-06-06 | **`GET /api/vehicles/{vehicleId}/drives` items now include `startLocation` / `startAddress` / `endLocation` / `endAddress` ([MYR-145](https://linear.app/myrobotaxi/issue/MYR-145)).** §5.2.2 mask table extended with the four P1 location fields (owner + viewer both see them — consistent with the FR-5.1 sharing use case already in effect for `DriveDetail`). §7.2 response example updated to show one drive with all four fields populated and a second drive with all four omitted, illustrating the nullable-on-the-wire convention: the handler drops the key entirely when the underlying Drive column is empty (drive still in progress, zero-GPS at start, or reverse-geocode failure) rather than emitting `""` or `null`. OpenAPI `DriveSummary` schema gains the four optional properties with the same `x-classification: P1` annotation as their `DriveDetail` siblings. Lean-projection rule is preserved: `routePoints` / `energyUsedKwh` / `fsdMiles` / `fsdPercentage` / `interventions` remain drive-detail-only. Implementation: `internal/store/queries.go` `driveSummarySelectColumns` extended; `internal/store/drive_repo_list.go` `DriveSummaryRow` + `scanDriveSummaryRow` extended; `internal/telemetry/vehicle_drives_types.go` `DriveListItem` + `driveSummary` + `toMaskMap` extended; `internal/mask/tables.go` `driveSummaryFields` extended; `cmd/telemetry-server/adapters.go` `driveListerAdapter` extended. No store-write-path changes — `DriveRepo.Create` / `Complete` already populate the four columns. No cursor / ordering / OpenAPI envelope shape changes. | go-engineer |
| 2026-06-03 | **Mounted `GET /api/vehicles/{vehicleId}/snapshot` and `GET /api/vehicles/{vehicleId}/drives` on the Go server ([MYR-133](https://linear.app/myrobotaxi/issue/MYR-133)).** Closes two of the four DV-20 PENDING endpoints. Snapshot returns the full v1 `VehicleState` shape from [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) via `internal/telemetry/vehicle_snapshot_handler.go`, backed by `VehicleRepo.GetByID` with GPS dual-read + nav-route blob decryption already applied at the store layer (NFR-3.23 / NFR-3.25). Drives returns the paginated `DriveSummary` envelope via `internal/telemetry/vehicle_drives_handler.go`, backed by a new lean `DriveRepo.ListByVehicleID` projection (no `routePoints` blob; ordered `startTime DESC, id DESC` per §4.2.2) with opaque base64-JSON cursor encoding the `(startTime, id)` anchor. Both handlers enforce ownership via `VehicleRepo.GetByID` (`vehicleId` cuid path param, NOT VIN), project responses through the role-based `internal/mask` matrix at the handler layer per §5.1, and emit 1%-sampled `mask_applied` audit rows per §5.3 when a role mask strips a field. Error catalog: 400 `invalid_request` (drives list only — out-of-range limit / malformed cursor), 401 `auth_failed`, 403 `vehicle_not_owned`, 404 `not_found`, 500 `internal_error`. The §10 DV-20 row reduces to two remaining endpoints (`GET /api/drives/{driveId}`, `GET /api/drives/{driveId}/route`); the §2.1 divergence callout, §6 endpoint catalog status note, and DV-20 catalogue row are updated accordingly. No wire shape, OpenAPI, or schema changes — the contract for both endpoints was already locked at MYR-12. | go-engineer |
| 2026-05-10 | **New `GET /api/vehicles` list endpoint ([MYR-91](https://linear.app/myrobotaxi/issue/MYR-91)).** Added a thin catalog endpoint to enumerate the signed-in caller's vehicles — closes the gap where the SDK had no way to answer "what are my cars?" without bypassing the contract via direct Prisma reads. Inserted as §7.0 (preserving existing §7.1–§7.7 numbering); §6 endpoint catalog grew a row; §5.2.0 declares the `VehicleSummary` per-role mask (owner sees all fields, viewer sees all minus `name`); OpenAPI spec gains the path + `VehicleSummary` component schema. Implementation: Go server mounts the handler at `GET /api/vehicles` (`internal/telemetry/vehicles_list_handler.go`) reading from `VehicleRepo.ListByUser`. v1 returns owner-owned vehicles only; the viewer-merged pathway is documented but PLANNED — depends on the Go server reading the Prisma-owned `Invite` table in a follow-up. No pagination in v1 (response is bounded; reserved query params for future). Test bench (P6 MYR-88) and SDK `client.vehicles.list()` (P3 MYR-80) consume this endpoint. | sdk-architect |
| 2026-05-10 | **Recent-login re-auth gate documented ([MYR-79](https://linear.app/myrobotaxi/issue/MYR-79); implementation [MYR-76](https://linear.app/myrobotaxi/issue/MYR-76)).** §7.6 (`DELETE /api/users/me`) and §7.7 (`GET /api/users/me/export`) gain a **Re-auth precondition** subsection requiring the caller's most recent fresh OAuth sign-in to be within `REAUTH_MAX_AGE_SEC` (default 300 s); rejection returns `401 auth_failed` with the new `subCode: reauth_required`. The gate applies symmetrically to deletion (destructive) and export (full-graph exfiltration) per the GDPR Art. 17 recent-auth corollary — both endpoints surface the entire ownership graph, so a stolen Bearer token must not satisfy either path alone. §4.1.1 `auth_failed` row updated to document the new subCode and the **explicit carve-out from the `getToken()` retry path**: SDKs MUST surface `reauth_required` to the consumer's auth layer for an interactive sign-in flow rather than swallowing it with a silent token refresh, because the `auth_time` claim only advances on a fresh OAuth round-trip. The §7.7 deferral note is replaced with the RESOLVED reference. Cross-contract: [`../architecture/requirements.md`](../architecture/requirements.md) §2.10 flipped from Deferred to Resolved. No wire/OpenAPI/schema-shape changes — `subCode` was already an existing field on the §4.1 error envelope. | sdk-architect |
| 2026-05-09 | **GDPR readiness pack docs ([MYR-75](https://linear.app/myrobotaxi/issue/MYR-75) Phase B).** Adds §7.7 `GET /api/users/me/export` to the endpoint reference (Phase A handler shipped in [myrobotaxi/react-frontend#259](https://github.com/myrobotaxi/react-frontend/pull/259)) — JSON archive of every Prisma row owned by the caller, P1 columns decrypted at the crypto boundary, OAuth credentials explicitly excluded; audit-log side effect documented as the new `data_exported` action in [`data-lifecycle.md`](data-lifecycle.md) §4.2 with `metadata: {vehicleCount, driveCount, inviteCount, auditCount}` (P0 counts only per Rule CG-DL-5). §1 TOC and §6 endpoint catalog summary updated. The optional recent-login re-auth gate from MYR-75's three-piece scoping is deferred to a follow-up issue and noted in §7.7 implementation notes + [`../architecture/requirements.md`](../architecture/requirements.md) §2.10. Companion runbook: [`../operations/backup-retention.md`](../operations/backup-retention.md) (Supabase backup window, redelete-on-restore procedure honoring GDPR Art. 17, legal-basis-for-retention boundary). | sdk-architect |
| 2026-05-08 | **DV-23 RESOLVED by [MYR-69](https://linear.app/myrobotaxi/issue/MYR-69).** Locked the FR-10 deletion + §7.5 invite-endpoint architecture to **Option 2 -- Next.js app owns `DELETE /api/users/me` and the three invite endpoints**, with the Go telemetry server holding **Insert-only** access to the Prisma-owned `AuditLog` table via raw pgx. §7.5 preamble rewritten from "two implementation paths" to a single locking sentence. §7.6 implementation notes' "may also run in the Next.js app layer" hedge replaced with a definitive Next.js-owns statement. §10 DV-23 row flipped from **New** to **RESOLVED** with resolution date, rationale, and pointers to MYR-70 / MYR-71 / MYR-72 / MYR-73 implementation follow-ups. **DV-20 row reduced in scope from six endpoints to four**: invite + user-deletion 404s on the Go server are now the terminal behavior (served by Next.js per DV-23), not transitional Go-server work; implementation order steps (5)/(6) and the FR-5.x / FR-10.1 anchors removed. Cross-contract update: [`data-lifecycle.md`](data-lifecycle.md) §1.4 adds an `AuditLog` row noting the telemetry server has Insert-only access; §4 preamble locks `AuditLog` ownership to the Next.js Prisma schema with the Go server as Insert-only writer (responsibility per §3.4). No wire / OpenAPI / SDK API changes -- the SDK still calls the single `https://api.myrobotaxi.com/api/...` base URL. | sdk-architect |
| 2026-04-14 | Initial full draft (MYR-12): §2 transport, §3 auth, §4 conventions (error envelope, pagination, versioning, headers, idempotency), §5 RBAC with forward-looking `limited_viewer` extension seam, §6 catalog summary, §7 per-endpoint reference (snapshot, drives list, drive detail, drive route, 3 invite ops, user self-deletion), §8 resource-schema index cross-referencing the inline OpenAPI components, §9 observability, §10 divergences DV-19 through DV-23 (REST auth middleware, unmounted SDK endpoints, reserved `503 service_unavailable`, REST rate limit, invite handler location decision). Adds REST-only error codes `not_found`, `invalid_request`, `service_unavailable` to the shared catalog with a note that the `ErrorPayload.code` enum in `schemas/ws-messages.schema.json` must be extended in the DV-20 follow-up. Canonical machine-readable twin is `specs/rest.openapi.yaml`. | sdk-architect |
