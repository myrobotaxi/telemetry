# REST API Contract

**Status:** Draft -- v1
**Target artifact:** OpenAPI 3.1 specification at [`specs/rest.openapi.yaml`](specs/rest.openapi.yaml)
**Owner:** `sdk-architect` agent
**Last updated:** 2026-08-01

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
   10. Authentication (identity module — MYR-193)
   11. In-app Tesla account link (MYR-246)
   12. `DELETE /api/tesla/vehicles/{vehicleId}` (owner car offboarding — MYR-258)
   13. `POST /api/tesla/vehicles/{teslaVehicleId}/re-add` (owner deliberate re-add — MYR-262)
   14. `PUT /api/tesla/vehicles/{vehicleId}/plate` (owner license-plate entry — MYR-286)
   15. `POST /api/tesla/vehicles/{vehicleId}/refresh` (owner on-demand state refresh — MYR-315)
   16. `PUT /api/tesla/vehicles/{vehicleId}/service-window` (owner expected-back entry — MYR-316)
   17. Push notification device registry (2 operations — MYR-186)
   18. `PUT /api/tesla/vehicles/{vehicleId}/ride-share` (owner ride-share pause toggle — MYR-342)
   19. Notification preferences (2 operations — MYR-349; the owner hears the rider's start — MYR-462)
   20. Saved places (3 operations — MYR-321)
   21. Live Activity token registration (2 operations — MYR-172; content-state progress track — MYR-398; the lifecycle banner defers to the island — MYR-413; the sixth island expansion rides an update, not the end — MYR-418; a rescued reservation may register again — MYR-461)
   22. `GET /api/vehicles/{vehicleId}/booked-windows` (schedule-picker conflict read — MYR-385)
   23. `POST /api/tesla/vehicles/{vehicleId}/complete-setup` (zero-touch setup completion — MYR-505)
   24. `POST /api/ride-requests/join` (group-ride join — MYR-540)
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
| `PATCH` | Editing one accepted share grant's capabilities in place — §7.5.7, the only `PATCH` in v1 |
| `PUT` | Whole-object replacement on the settings-shaped surfaces — §7.14, §7.16, §7.17.1, §7.18, §7.19.2, §7.20.2 |
| `DELETE` | Invite revocation, user self-deletion |

**`PATCH` has exactly ONE use in v1** — the owner-only `PATCH /api/invites/{inviteId}` (§7.5.7), added by [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369). The original rule this section stated still governs every other resource here, and for the original reasons: mutations restricted to explicit creation (`POST`) and deletion (`DELETE`), with nothing updating a resource in place, keeps idempotency semantics simple (see §4.5) and keeps the contract's surface area small. What made this one the exception is what it edits — a **partial** update of **two independent** owner-editable flags (`allowRides`, `suspended`) on a live access grant, where an ABSENT field must mean *leave it alone*. An owner turning ride requests off has no business restating whether the grant is suspended, and a `PUT` would force exactly that: the client would have to resend state it may never have read, so a client working from a stale or partial view would silently overwrite the flag it did not mean to touch. On an access-control surface that is not a style preference — the failure mode of the `PUT` shape is **un-suspending somebody by omission**. The idempotency argument survives intact, because this endpoint is idempotent in effect anyway (§7.5.7). `PUT` itself is no longer unused either: the settings-shaped surfaces above are whole-object replacements, where resending the entire object IS the intent, which is the case `PATCH` is not.

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
| `error.subCode` | `string` (enum) \| `null` | No | P0 | Optional typed sub-code for branching consumer UI when the primary code is ambiguous across carriers. v1 enum: `device_cap` (WS-only, shared with the WS ErrorPayload — REST does not emit it; declared on the REST envelope for shared-type compatibility), `reauth_required` (REST-only, emitted by §7.6 / §7.7 when the recent-login re-auth gate fails — see §4.1.1 `auth_failed`) `reservation_expired` (REST-only, emitted by §7.21.1 on a `409 conflict` when a Live Activity registration is refused because the ride's reservation lapsed — the ride's own status is still `accepted`, so the primary code alone cannot tell the client what happened) and `time_conflict` (REST-only, emitted by §7.8 create/accept on a `409 vehicle_unavailable` when the target vehicle is already promised to another open ride within 45 minutes of the requested `scheduledFor` — MYR-383. It exists because the code's three other carriers are conditions of the car RIGHT NOW, which a client answers with "try again later", while this one is a property of the TIME the rider picked, which a client answers by returning them to the picker). The wire shape is **always present**, serialized as JSON `null` when the carrier emits no sub-code (see §4.1 envelope JSON example above). |

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
| `vehicle_unavailable` | 409 | **REST-only** | Implemented (MYR-277, MYR-266, MYR-342, MYR-383) | Surface to UI; **do not auto-retry** while the vehicle stays in service / offline / paused / on another ride — retry once the owner brings it back. **`subCode: time_conflict` is different: do NOT retry at all** — the car is fine and the *slot* is taken, so the client returns the rider to the time picker with the conflicting instant from the message. | The target vehicle cannot serve this ride, for one of **four** capability reasons: **(a)** its persisted status is `in_service` or `offline` — MYR-277, **accept only**; **(b)** it is already **committed to another active instant ride** (`accepted`/`arrived`/`enroute`) — the per-vehicle one-active-ride guard MYR-266, **accept only**; **(c)** its owner has **PAUSED ride sharing** — MYR-342, on **create AND accept**; **(d)** **`subCode: time_conflict`** — it is already promised to another open ride within **45 minutes** of the requested `scheduledFor`, the per-vehicle window gate MYR-383, on **create AND accept**, **SCHEDULED rides only**. Cases (a) and (b) apply to **INSTANT rides only** — a ride with `scheduledFor` skips the status gate (MYR-313) and is outside the per-vehicle index's `scheduled_for IS NULL` predicate (MYR-266). Case (d) is the mirror image: it applies to **reservations only** and never to an instant ride. **The MYR-313 exemption is therefore narrower than "reservations are never refused `vehicle_unavailable`"** — it exempts a reservation from the *status* gate (a), not from (c) or (d), which ask about the reservation instant rather than about the car today. In every case the ride is still legally acceptable/creatable, so this is **not** `conflict` (an illegal lifecycle *transition*) — it is a capability gate on the vehicle. Arbitration: (a)/(c) read the current persisted row; (b) is the partial unique index `uq_go_ride_requests_active_instant_vehicle` (23505) on the guarded write; (d) is a per-vehicle **advisory transaction lock** held across probe-and-write, so two conflicting bookings resolve to exactly one winner. **Decline is never gated by any of them.** Member of the shared `ErrorPayload.code` enum for single-union SDK typing, but never emitted over the WS transport. See §7.8. |
| `rate_limited` | 429 | Shared (WS + REST) | Implemented for WS pre-auth per-IP cap (MYR-47); REST per-user request cap PLANNED (DV-22); WS post-auth per-user cap PLANNED (DV-08) | Auto-retry with extended backoff (§4.1.2). SDK MAY set `Retry-After` header as backoff hint. | Two distinct caps share the same typed code. WS emits `rate_limited` with `subCode: device_cap` for **concurrent-session cap** breaches (too many simultaneous WebSocket connections per user, see `websocket-protocol.md` §6.1.1 and DV-08). REST emits `rate_limited` (no sub-code in v1) for **request-rate cap** breaches (>120 req/min per authenticated user, see §4.1.2 and DV-22). Consumers distinguish the two via the carrier transport and the presence of `subCode`. |
| `internal_error` | 500 | Shared (WS + REST) | Implemented on REST (MYR-47); PLANNED on WS | Auto-retry with exponential backoff (NFR-3.10 curve from `websocket-protocol.md` §7.1), cap at 3 REST attempts before surfacing. | Catch-all for unexpected server failures: panics, DB errors, downstream timeouts. |
| `service_unavailable` | 503 | **REST-only, PLANNED** | PLANNED (DV-21) | Auto-retry with exponential backoff; honor `Retry-After` header if present. | Reserved for maintenance windows and graceful-shutdown states. The server MAY return `503` during rolling deployments; v1 does not yet emit this code. Added to the REST catalog so SDK consumers can write forward-compatible handlers. |
| `snapshot_required` | -- | **WS-only** (close code 4005 + error frame) | PLANNED (DV-02) | n/a for REST | WS-only. REST has no analogue because REST is already the snapshot channel (the "fall back to snapshot fetch" signal IS a REST call). Listed here for completeness; REST clients never receive this code. |
| `key_not_paired` | 403 | **REST-only** | Implemented (MYR-180) | Surface to UI; **do not auto-retry** — needs owner action. | The application's virtual key is not enrolled on the vehicle (owner has not completed the `tesla.com/_ak/<domain>` pairing, MYR-115), or the command-signing transport is not configured. This is the default outcome for every signer-required command in §7.9 until pairing happens. The SDK prompts the owner to pair. |
| `vehicle_asleep` | 503 | **REST-only** | Implemented (MYR-180) | Auto-retry with backoff (§4.1.2 curve). SDK MAY honor `Retry-After`. | The target vehicle was asleep/offline and did not come online within the executor's bounded wake+retry budget (§7.9). Transient — the executor already woke + retried internally before surfacing this. |
| `command_failed` | 502 | **REST-only** | Implemented (MYR-180) | Surface to UI; do not blindly auto-retry (the vehicle rejected the action). | A vehicle command (§7.9) failed for a non-scope, non-pairing reason: the vehicle returned `result:false`, a signing-session/counter error survived re-handshake, or the Fleet API/proxy failed. Collapses the several vehicle-side failure modes into one typed code in v1. |

##### 4.1.1.a REST-only codes added to the shared catalog

Nine codes are REST-only extensions of the shared catalog: `not_found`, `invalid_request`, `service_unavailable`, `conflict` (MYR-174), `ride_active` (MYR-230), `vehicle_unavailable` (MYR-277), and `key_not_paired` / `vehicle_asleep` / `command_failed` (MYR-180).

- `conflict` (HTTP 409) is emitted only over REST when a ride-request lifecycle mutation is illegal from the row's current state (§7.8 transition matrix). It has no WS analogue because the ride-request mutations are request-oriented REST endpoints; the WS transport carries only the summary `ride_status_changed` broadcast of a *successful* transition. It is a member of the shared `ErrorPayload.code` enum (added by MYR-174) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks it REST-only-on-the-wire.
- `ride_active` (HTTP 409) is emitted only over REST when `POST /api/ride-requests` is called for an **instant** ride while the caller already holds an OPEN instant ride (§7.8). Like `conflict` it is request-oriented and has no WS analogue — the WS path never carries a create. It is a member of the shared `ErrorPayload.code` enum (added by MYR-230) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks it REST-only-on-the-wire. **Response-body note:** unlike every other error, the `ride_active` 409 body adds a sibling `activeRideRequest` field alongside the standard `error` envelope — the rider's existing open ride (`$defs.RideRequest`, byte-for-byte the `GET /api/ride-requests/{id}` shape) — so the client adopts it rather than surfacing a decline. The nested `error` object is unchanged; consumers that ignore the extra field still get a well-formed envelope.
- `vehicle_unavailable` (HTTP 409) is emitted only over REST when the target vehicle cannot serve a ride — because it is `in_service`/`offline` (MYR-277, accept), already committed to another active instant ride (the per-vehicle one-active-ride guard MYR-266, accept), its owner has paused ride sharing (MYR-342, create + accept), or it is already promised to another open ride inside the 45-minute booking window (**`subCode: time_conflict`**, MYR-383, create + accept) (§7.8). Like `conflict` it is request-oriented and has no WS analogue — the WS path never carries a create or an accept. It is a member of the shared `ErrorPayload.code` enum (added by MYR-277) so the SDK's `CoreError` union stays one enum across transports; the schema enum description marks it REST-only-on-the-wire. Distinct from `conflict` (an illegal lifecycle *transition* on a known ride): the ride IS acceptable/creatable — the *vehicle* just cannot serve it. **Decline is never gated** by any of the four. The per-vehicle busy guard is the exact analogue of the per-rider `ride_active` guard (MYR-230) applied to the vehicle, and scheduled rides are exempt from both — **but "exempt from those two" is not "never refused `vehicle_unavailable`"**: the MYR-342 pause and the MYR-383 window gate both apply to reservations, because they ask about the reservation instant rather than about the car right now. `time_conflict` is in fact **reservation-only** and never fires for an instant ride.
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
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — malformed/unknown-field body, bad place, out-of-range coord, bad `scheduledFor`, `scheduledFor` earlier than the vehicle's `serviceEstimatedEndAt`, `scheduledFor` more than 30 days out, bad `limit`/`cursor` | 400 | `ErrCodeInvalidRequest` | Create body / list query failed validation (`additionalProperties:false`). Two of these are bounds rather than syntax, both deliberately the SAME code with no new one: the MYR-316 service-window bound (null estimate ⇒ no bound; equal allowed; instant rides unaffected, §7.8) and the MYR-372 future-horizon bound, `scheduledFor must be no more than 30 days in the future` — **CREATE only**, equal allowed, and the PAST is deliberately unbounded because accept-after-due semantics depend on it (§7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — vehicle not found on create | 404 | `ErrCodeNotFound` | `GetByID` returns `sdk.ErrNotFound` |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — vehicle access denied on create | 403 | `ErrCodeVehicleNotOwned` | Caller's `userID` ≠ vehicle's owner (v1 owner-only access; §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — second open instant ride on create (pre-check) | 409 | `ErrCodeRideActive` | Rider already holds an OPEN instant ride; body carries `activeRideRequest` (MYR-230, §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — second open instant ride on create (unique-index race backstop) | 409 | `ErrCodeRideActive` | Concurrent instant create rejected by `uq_go_ride_requests_active_instant_rider` (23505); winner re-read into `activeRideRequest` (MYR-230, §7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — unknown / non-party ride | 404 | `ErrCodeNotFound` | `GetByID` miss, or caller is neither rider nor owner (no existence leak) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — owner cancel outside its window (MYR-522; this row was a blanket 403 while cancel was rider-only) | 409 | `ErrCodeConflict` | The owner's cancel is legal from `accepted`/`arrived` only; the refusal for `requested` names decline, and for `enroute` names dropped-off |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — illegal lifecycle transition | 409 | `ErrCodeConflict` | Cancel from a non-`{requested,accepted}` state (§7.8) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — store failure | 500 | `ErrCodeInternalError` | DB error on create / status update / list |
| [`ride_share_gate.go`](../../internal/telemetry/ride_share_gate.go) — ride-request create OR owner accept against a vehicle whose owner has PAUSED ride sharing (MYR-342) | 409 | `ErrCodeVehicleUnavailable` | `Ride sharing is paused for this vehicle`. A CAPABILITY refusal, sharing the code with the MYR-266 busy guard and the MYR-277 in-service/offline gate — the request is well-formed and the caller authorised, the car simply cannot serve it. **Applies to SCHEDULED rides too**, unlike the MYR-313 exemption (§7.8): a service visit ends, an owner's pause does not |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — scheduled accept whose `scheduledFor` precedes the vehicle's `serviceEstimatedEndAt` | 400 | `ErrCodeInvalidRequest` | MYR-316 service-window bound on accept, same code as the create-side check (§7.8). The vehicle read this needs **fails CLOSED** (MYR-372): a transient read failure is `500 internal_error` and the reservation is NOT accepted, and a vehicle that no longer exists is the permanent `409 vehicle_unavailable` in the row below. It used to fail open — unreadable ⇒ unbounded — which silently granted reservations that could sit inside a service visit |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — accept whose target vehicle no longer exists | 409 | `ErrCodeVehicleUnavailable` | `Vehicle is no longer available` (MYR-372). PERMANENT, so it must not wear the retryable `500` a transient store failure gets; and not a `404`, whose meaning on this endpoint is "unknown ride / non-party" and which would tell an owner their own request had vanished |
| [`ride_request_owner_decision.go`](../../internal/telemetry/ride_request_owner_decision.go) — rider attempts accept/decline (owner-only) | 403 | `ErrCodePermissionDenied` | A party, but wrong role for the decision (`loadOwnerParty`, shared by accept and decline) |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — accept from a non-`requested` state | 409 | `ErrCodeConflict` | Illegal lifecycle transition (§7.8 matrix) |
| [`ride_request_owner_decision.go`](../../internal/telemetry/ride_request_owner_decision.go) — decline from a state outside the ride's declinable set | 409 | `ErrCodeConflict` | Illegal lifecycle transition (§7.8 matrix). The set is `{requested}` for an INSTANT ride and `{requested, accepted}` for a SCHEDULED one (MYR-360), so an accepted instant ride still conflicts |
| [`ride_request_upcoming_handler.go`](../../internal/telemetry/ride_request_upcoming_handler.go) — malformed `upcomingForVehicle` | 400 | `ErrCodeInvalidRequest` | Empty, whitespace-only or oversized value on the owner incoming feed (MYR-360, §7.8). An id that is well-formed but UNKNOWN or owned by somebody else is **not** an error — it returns an empty `200` page, so the param cannot oracle whether a vehicle exists |
| [`ride_request_upcoming_handler.go`](../../internal/telemetry/ride_request_upcoming_handler.go) — upcoming-reservations store failure | 500 | `ErrCodeInternalError` | DB error on the `upcomingForVehicle` slice |
| [`ride_request_active_handler.go`](../../internal/telemetry/ride_request_active_handler.go) — malformed `activeForVehicle` | 400 | `ErrCodeInvalidRequest` | Empty, whitespace-only or oversized value on the owner incoming feed (MYR-400, §7.8). Same rule and the same shared validator as `upcomingForVehicle`: an id that is well-formed but UNKNOWN or owned by somebody else is **not** an error — it returns an empty `200` page, so neither param can oracle whether a vehicle exists |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — `upcomingForVehicle` **and** `activeForVehicle` both present | 400 | `ErrCodeInvalidRequest` | The two vehicle filters are mutually exclusive (MYR-400, §7.8). They name different predicates, orderings **and** cursor anchor columns, so no single page can honour both requests; refusing beats a precedence rule that would silently answer a question the client did not ask. (The refusal does **not** rest on the slices being disjoint — §7.8 states the overlap set) |
| [`ride_request_active_handler.go`](../../internal/telemetry/ride_request_active_handler.go) — active-rides store failure | 500 | `ErrCodeInternalError` | DB error on the `activeForVehicle` slice |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — accept for an in-service / offline vehicle | 409 | `ErrCodeVehicleUnavailable` | Target vehicle's persisted status is `in_service`/`offline` and cannot fulfill the ride (MYR-277, §7.8) |
| [`ride_window_conflict.go`](../../internal/telemetry/ride_window_conflict.go) — scheduled ride-request CREATE **or** owner ACCEPT whose target vehicle is already promised inside the 45-minute booking window (MYR-383) | 409 | `ErrCodeVehicleUnavailable` + `SubCodeTimeConflict` | The ONE response composer both landing sites share (§7.8 "Per-vehicle ride-window conflict"). A CAPABILITY refusal, sharing the code with the MYR-266/MYR-277/MYR-342 gates; the sub-code is what tells a client the *slot* is taken rather than the *car* unavailable. **Applies to SCHEDULED rides only** — the mirror image of (a)/(b), and NOT covered by the MYR-313 exemption, which exempts reservations from the status gate alone. Message names the conflicting INSTANT and nothing else: the other party's ride is P1 (§4.1 rule 2) |
| [`ride_request_handler.go`](../../internal/telemetry/ride_request_handler.go) — accept for a vehicle already on another active ride (unique-index race backstop) | 409 | `ErrCodeVehicleUnavailable` | Guarded `requested`→`accepted` write rejected by `uq_go_ride_requests_active_instant_vehicle` (23505); the car is already committed to an `accepted`/`arrived`/`enroute` ride (MYR-266, §7.8) |
| [`ride_request_owner_progress_handler.go`](../../internal/telemetry/ride_request_owner_progress_handler.go) — owner `picked-up` on a DORMANT reservation | 409 | `ErrCodeConflict` | `a scheduled ride cannot be picked up before its dispatch`. MYR-376: the ride is legally `accepted`, but it is a SCHEDULED one that is neither dispatched (`dispatchStatus == "sent"`) nor yet due (`scheduledFor <= now()`) — the car has been told nothing, so there is no pickup to confirm (§7.8). **Deliberately the SAME code as the illegal-transition conflict**, no new one; only the message distinguishes them. Instant rides are unaffected, and the refusal lifts at the due instant so an expired reservation can still proceed manually. Enforced inside the guarded write (`UpdateStatusFromDispatched`), never as a pre-check |
| [`ride_request_owner_handler.go`](../../internal/telemetry/ride_request_owner_handler.go) — incoming feed store failure | 500 | `ErrCodeInternalError` | DB error on the owner list |
| [`vehicle_service_window_handler.go`](../../internal/telemetry/vehicle_service_window_handler.go) — missing `{vehicleId}`, malformed JSON body, unparseable or non-future `expectedEndAt` | 400 | `ErrCodeInvalidRequest` | §7.16 owner write. A malformed timestamp reports `expectedEndAt must be an RFC 3339 date-time`; a past/now value reports `expectedEndAt must be in the future`. Clearing is NOT an error — absent key, explicit `null`, empty/whitespace string and an empty body all clear |
| [`vehicle_service_window_handler.go`](../../internal/telemetry/vehicle_service_window_handler.go) — non-`PUT` method | 405 | `ErrCodeInvalidRequest` | §7.16, same shape as §7.14 |

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

**One ride-request view orders differently, deliberately (MYR-360).** `GET /api/ride-requests/incoming?upcomingForVehicle={vehicleId}` (§7.8) orders `scheduledFor ASC, id ASC` — **soonest first** — with the cursor encoding `(scheduledFor, id)` and the ASCENDING resume predicate `(scheduled_for, id) > (:cursorScheduledFor, :cursorId)`. Same total-order shape, same row-value keyset discipline, opposite direction over a different column. The reason is that the view's whole purpose is to name the **next** reservation, and under `createdAt DESC` a paginated cut could omit the soonest one entirely (a reservation booked long ago for tomorrow sorts last). Every row in that view has a non-null `scheduled_for` by construction, so the row-value comparison never meets a `NULL`. **The wire cursor format is unchanged and there is still exactly ONE of it** — the same opaque base64 `{timestamp, id}` pair; only which column the timestamp names differs per view, which clients never see because the cursor is opaque (§4.2.1).

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

**Why no `Idempotency-Key` in v1.** The only non-idempotent endpoint is `POST /api/vehicles/{vehicleId}/invites`. The cost of a double-invite (two extra `go_vehicle_shares` rows carrying a second live code) is bounded: the owner cancels either via `DELETE /api/invites/{inviteId}`, and both codes expire in 7 days regardless. (`POST /api/invites/redeem` looks like the other candidate but is not one — it is idempotent per redeemer by construction, backed by the partial-unique accepted-grant index.) The cost of shipping a server-side idempotency key store (Redis or a dedicated table) is higher than the cost of the occasional duplicate invite at v1 scale (NFR-3.6: 1,000 users). If the invite UX suffers, a follow-up can add an `Idempotency-Key` header following RFC draft conventions.

---

## 5. RBAC and field masks

> **Anchored:** NFR-3.19, NFR-3.20, FR-5.4, FR-5.5.

v1 defines two roles:

| Role | Read | Write | Reference |
|------|------|-------|-----------|
| `owner` | Full | Full (create/delete invites, delete account) | FR-5.4 |
| `viewer` | Full read of the vehicle's **live** state (catalog row, snapshot, WebSocket stream), plus ride requests when the grant carries `allowRides`. **Drive history and route playback are owner-only** as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) — see §7.5.0 | None | FR-5.4 |

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
| `owner` | All `VehicleSummary` fields: `vehicleId`, `name`, `model`, `year`, `color`, `vinLast4`, `status`, `chargeLevel`, `estimatedRange`, `lastUpdated`, `role`, `hasActiveRide`, `licensePlate`, `serviceEstimatedEndAt`, `rideShareEnabled`, `setupState`, `trimLabel`, `location` | Full catalog visibility. `role` is always `owner` for the row's caller-vehicle relationship. `hasActiveRide` ([MYR-233](https://linear.app/myrobotaxi/issue/MYR-233)) is P0 derived operational state — same tier as `status` — and is in BOTH role allow-lists: a rider needs it to render Busy and route to the scheduling flow. `licensePlate` ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286)) is P1 but likewise in BOTH allow-lists — see the viewer row. `serviceEstimatedEndAt` ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316)) is P0 operational timing — again the same tier as `status` — and is in BOTH allow-lists for the same reason `hasActiveRide` is: it is what floors the rider's scheduling picker, so a rider who cannot see it cannot book correctly. `rideShareEnabled` ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342)) is P0 owner-set operational availability — the same tier again — and is in BOTH allow-lists for a reason stronger than either: the viewer is the party the value is ABOUT. A rider who cannot see that a shared car is paused discovers it only from a `409 vehicle_unavailable` after composing a whole request. Added to `vehicleSummaryOwnerFields` in `internal/mask/tables.go`; the viewer list is a copy of the owner list plus `sharePermission`, so it lands in both by construction. `setupState` ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491)) is P0 derived operational state — the same tier again, and the field that qualifies `status` itself — and is in BOTH allow-lists for the sharpest version of the `rideShareEnabled` argument: a never-streamed shared car sits at `status: "offline"` **permanently**, because that column is a write-once schema default until the first mTLS stream event ([MYR-437](https://linear.app/myrobotaxi/issue/MYR-437)), so a viewer's picker today shows a healthy car as dead with no explanation. The viewer is the party who needs "still being set up" most, and the picker reads THIS row, not the snapshot. `trimLabel` ([MYR-507](https://linear.app/myrobotaxi/issue/MYR-507)) is P0 and in BOTH allow-lists, and it is the one entry on this row justified by IDENTITY rather than operational state: it is an equipment fact of exactly the tier of `model` / `year` / `color`, which have been viewer-visible since v1, and all four are legible through a windshield from the kerb — the badge on the boot lid IS the trim. It was previously withheld from the catalog as "MYR-320 detail-sheet only", and the cost landed entirely on the viewer, who never fetches a snapshot: with no trim, no model and no plate, a shared car's descriptor degraded to the raw colour token and a rider's lock screen read `UltraRed`. Note the deliberate SPLIT from its MYR-320 sibling `fsdVersion`, which stays detail-sheet-only — a software designation with no catalog consumer — and from `trim`, the raw badge code (`p74d`), which is not display-safe for either role and is on neither surface's catalog. Pinned by `TestMYR320DetailFieldsSplitOnTheVehiclesList` (`internal/mask/tables_details_test.go`) so a later change cannot carry `fsdVersion` along by analogy. `location` ([MYR-515](https://linear.app/myrobotaxi/issue/MYR-515)) is **P1** — the first on this resource besides `licensePlate` — and is nevertheless in BOTH allow-lists, on the narrowest argument available: **the viewer already receives this exact value, at this exact precision, on the streaming path**, because `vehicleStateViewerFields` retains the whole Speed/GPS atomic group and MYR-435 deliberately did not narrow it. Same value, different transport, same entitled party. The difference the catalog does introduce is **aggregation** — N positions in one response versus one per socket subscription — which changes convenience rather than reach, since the grant is per-vehicle and a viewer could subscribe to each car in turn. Coarsening was considered and rejected: with the socket serving full precision to the same viewer, rounding the catalog is theatre that costs the short-hop pickup ETA the field exists for; a future coarsening policy must cover the stream and this row together or the weaker surface sets the real bound. **MYR-435 note:** the vehicles-list row is unaffected by the viewer-mask narrowing — this thin catalog has never carried media, cabin or controls state, and `TestVehiclesListCarriesNoRemovedField` now guards that for BOTH roles so a later "show a lock icon in the catalog" cannot quietly re-open what §5.2.1 closed. |
| `viewer` | All `VehicleSummary` fields, PLUS `sharePermission` | **The viewer list subtracts NOTHING from the owner list.** `name` — the user-assigned nickname — is **viewer-visible as of [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184)**, reversing the original owner-only rule for two reasons. **Product:** the rider UI renders a shared car as "{Owner}'s {Vehicle}", so the nickname is the label a viewer reads; a vehicle nickname is P1-acceptable for viewers under the same policy that puts the owner's first name in the push payloads and in `RedeemShareInviteResponse.ownerFirstName`. **Contract:** `name` is in the `required` list of [`schemas/vehicle-summary.schema.json`](schemas/vehicle-summary.schema.json), so stripping it made **every** viewer row — the §7.0 merge and the §7.5.5 redeem response alike — invalid against the very shape its consumer decodes, while each individual field assertion still passed. **Standing rule: any field this mask removes MUST be OPTIONAL in that schema** — enforced by `TestViewerMaskKeepsEverySchemaRequiredField` (`internal/telemetry/vehicles_list_viewer_schema_test.go`), which walks the schema's own `required` list against the real projection. `licensePlate` ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286)) is deliberately NOT owner-only despite also being P1: the entire purpose of the plate is that a rider can identify the correct car at pickup, which fails if only the owner can see it. That is a product decision, not an oversight. `serviceEstimatedEndAt` ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316)) is likewise NOT owner-only, and needs no separate rule: there are no subtractions at all, so every field added to the owner list arrives here automatically (`internal/mask/tables.go` copies the owner slice and appends). That is the intended outcome, not an accident of the derivation — the window floors the rider's picker. The catalog never carries the full `vin` for either role (`vinLast4` only). **[MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) added `sharePermission` to the VIEWER list only** — the first field whose asymmetry runs this direction. It carries the access the caller holds over a car they do NOT own (§7.5.0), which is meaningless on an owner row (an owner holds no grant), so it is deliberately ABSENT from the owner allow-list rather than emitted empty — a consumer told to read an absent value as the LOWEST would otherwise conclude an owner has `live` access to their own car. **[MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) made it DERIVED** rather than a stored tier: `allowRides` → `rides`, otherwise `live`, and never `live_history`. A **suspended** grant produces no viewer row at all, so there is nothing for this mask to project — the vehicle is absent from the response, not present with a reduced value. P0, the same tier as its sibling `role`. Because the viewer list is a copy of the owner list, this one field is appended explicitly. `rideShareEnabled` ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342)) arrives here by the same no-subtractions derivation, and again deliberately: it is the field a rider most needs before composing a request, so withholding it would break the very consumer it was added for. **This row is no longer forward-looking:** MYR-184 delivered the viewer merge, so viewer rows are real and this projection runs on every list call from anyone who has redeemed an invite. |

#### 5.2.1 Vehicle snapshot (`GET /api/vehicles/{vehicleId}/snapshot`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` | All fields in [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json) | Including GPS, nav, charge, gear -- the full v1 `VehicleState` shape. Includes the MYR-252 cabin-control read-back fields (`locked`, `hvacPower`, `isClimateOn`, `fanSpeed`, `driverTempSetting`, `passengerTempSetting`, `hvacAutoMode`, `hvacAcEnabled`, `seatHeaterLeft`/`Right`, `seatHeaterRearLeft`/`Center`/`Right`, `seatCoolerLeft`/`Right`, `seatVentEnabled`, `chargePortDoorOpen`, `frunkOpen`, `trunkOpen`, `mediaPlaybackStatus`, `mediaVolume`) — all P0, all in `internal/mask/tables.go` `vehicleStateOwnerFields`. **Delivery caveat (updated by [MYR-298](https://linear.app/myrobotaxi/issue/MYR-298)):** **20 of the 21** cabin read-backs are now persisted (Go-owned `go_vehicle_control_state` side table, LEFT-joined into `VehicleRepo.GetByID`) and **returned on this DB-backed `/snapshot`** for non-streaming cars (NFR-3.5) — the five owner controls `locked`, `frunkOpen`, `trunkOpen`, `isClimateOn`, `chargePortDoorOpen` ([MYR-269](https://linear.app/myrobotaxi/issue/MYR-269)), the eleven cabin-setting levels ([MYR-273](https://linear.app/myrobotaxi/issue/MYR-273)), the two climate-mode read-backs ([MYR-274](https://linear.app/myrobotaxi/issue/MYR-274)), and `seatVentEnabled` + `mediaPlaybackStatus` ([MYR-298](https://linear.app/myrobotaxi/issue/MYR-298)). Only `hvacPower` stays WS-live-only — its server-derived `isClimateOn` boolean IS persisted, so nothing owner-facing is lost. Every persisted read-back is nullable and surfaces as an explicit `null` when never read (honest-unknown, never a fabricated value). This closes the [MYR-253](https://linear.app/myrobotaxi/issue/MYR-253) hydration. **Extended by [MYR-303](https://linear.app/myrobotaxi/issue/MYR-303) + [MYR-308](https://linear.app/myrobotaxi/issue/MYR-308):** the owner projection additionally carries the media now-playing block (`mediaNowPlayingTitle`, `mediaNowPlayingArtist`, `mediaNowPlayingAlbum`, `mediaNowPlayingStation`, `mediaPlaybackSource`, `mediaNowPlayingDurationMs`, `mediaNowPlayingElapsedMs`, `mediaVolumeMax`) and `seatCoolingCapable` — all nine persisted to the same side table (migration 0015) and returned here. The five free-text fields are **P1**, not P0 like the rest of the cabin block: they are user content whose accumulation reveals listening habits, so they are redacted in logs (`data-classification.md` §1.13). `seatCoolingCapable` is REST-sourced (`vehicle_config.has_seat_cooling`) and **snapshot-only** — Tesla does not stream it, so it never appears on a `vehicle_update` frame. A `/snapshot` omitting optional fields is contract-valid (§7.1). **Extended by [MYR-316](https://linear.app/myrobotaxi/issue/MYR-316):** the owner projection also carries `serviceEstimatedEndAt` — added to `vehicleStateOwnerFields` in `internal/mask/tables.go`, P0 like the sibling `status` it is gated on. It is neither streamed nor a cabin read-back: it is `COALESCE(service_etc, service_expected_end_at)` over two migration-**0017** columns on the same side table (Tesla's estimate outranking the owner's §7.16 entry), emitted only while the vehicle is `in_service` and `null` otherwise. **Extended by [MYR-342](https://linear.app/myrobotaxi/issue/MYR-342):** the owner projection also carries `rideShareEnabled` — added to `vehicleStateOwnerFields`, P0 like `status`. It is neither streamed nor Tesla-sourced and never will be: it is the OWNER's switch on their own car, a non-null boolean on the same side table (migration **0021**), written only by §7.18. **Extended by [MYR-491](https://linear.app/myrobotaxi/issue/MYR-491):** the owner projection also carries `setupState` — added to `vehicleStateOwnerFields`, P0 like `status`, which it exists to qualify. It is the first field on this shape sourced from a SECOND Go-owned side table, `go_fleet_config_attempts` (migrations **0031** + **0036**), via its own LEFT JOIN, and it is DERIVED rather than projected: the four joined columns are evaluated together with `status` and `lastUpdated` by one server-side function shared with the §7.0 catalog row, so the two surfaces cannot disagree. Never streamed, and by construction a car in any of these states is not sending frames at all. |
| `viewer` | **An explicit allow-list — see the enumeration below.** Location/heading/speed, navigation + route, vehicle identity, charge, availability, and freshness. **NOT** media, cabin climate, or any vehicle-controls state. | **REWRITTEN BY [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435) (client decision, 2026-08-02: *"Remove media/cabin and any vehicle controls."*).** This row used to read "all fields EXCEPT the full `vin`" — a SUBTRACTION mask. The [MYR-427](https://linear.app/myrobotaxi/issue/MYR-427) privacy audit found the shape itself is the defect, not just its contents: under subtraction every field added to the owner list reaches viewers **by default**, decided by whoever did not think about it. That is how a viewer came to receive the owner's now-playing track title and cabin temperature. The viewer arm is therefore now an **explicit allow-list** (`vehicleStateViewerFields` in `internal/mask/tables.go`), with a companion `vehicleStateOwnerOnlyFields` enumerating every withheld field; `TestVehicleStateRoleListsPartitionOwnerFields` requires the two to partition the owner list exactly, so a newly added field fails the build until it is consciously classified. **Both delivery surfaces read this one table** — the WS hub (`BroadcastMasked` / `enqueueSnapshotFrame`, [`websocket-protocol.md`](websocket-protocol.md) §4.6) and this REST endpoint — so there is no second matrix to keep in sync. Each surface is pinned by a test that drives it end-to-end and iterates `mask.OwnerOnlyVehicleStateFields()`: `TestVehicleSnapshotHandler_ViewerSnapshotOmitsEveryOwnerOnlyField` (this endpoint), `TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing` (WS live broadcast) and `TestHub_SendSnapshot_ViewerReplayOmitsEveryOwnerOnlyField` (WS connect-time replay). **Retained rationale:** viewers keep full GPS, nav and charge visibility because watching the vehicle in real time is the entire point of sharing (FR-5.1, FR-5.4), and `licensePlate` stays viewer-visible ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286)) because a rider identifies the car at pickup by reading it. `serviceEstimatedEndAt` ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316)) and `rideShareEnabled` ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342)) stay for the strongest version of that argument — the viewer is the party those values are ABOUT — but note they now stay by **explicit listing**, no longer "automatically, by the derivation". `setupState` ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491)) is listed for viewers on the same basis and with the strongest form of that argument: a shared car that has never streamed reads `status: "offline"` forever ([MYR-437](https://linear.app/myrobotaxi/issue/MYR-437) — the column is a write-once schema default until the first mTLS stream event), so without this field a viewer literally cannot tell a broken car from an unfinished one. It carries no VIN, no coordinate, no credential and no user content, so nothing in the MYR-427 audit's reasoning reaches it. The full 17-char `vin` remains owner-only ([MYR-279](https://linear.app/myrobotaxi/issue/MYR-279)); it is no longer the *only* owner-only field. |
| `limited_viewer` (FR-5.5 future slot) | The `viewer` allow-list above, MINUS `licensePlate` and the whole navigation group (`navRouteCoordinates`, `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`), with `latitude`/`longitude` reduced to a coarse-grained hash (city-block resolution) | Documented here as the extension seam for FR-5.5. NOT implemented in v1. **Simplified by MYR-435:** this row used to carry a standing instruction to exclude the five free-text media fields when the tier is built. That instruction is now redundant — MYR-435 removed media, cabin and controls state for **every** non-owner role, so `limited_viewer` inherits those exclusions by construction and only needs to subtract further from the narrowed viewer list. It still excludes `licensePlate` even though the full `viewer` role receives it (MYR-286): an invited viewer waiting at a pickup needs the plate, whereas `limited_viewer` is the deliberately-degraded tier for someone who is not being picked up. Adding the row is a one-file change in `internal/mask/tables.go`. |

##### 5.2.1.1 The viewer allow-list, enumerated (MYR-435)

Stated as an **allow-list** rather than as a set of subtractions, per the MYR-427 audit finding that subtraction masks rot. This is the complete set a `viewer` receives on `/snapshot` **and** on every masked `vehicle_update` frame — the two surfaces share one table.

| Group | Fields | Why a viewer needs it |
|-------|--------|----------------------|
| Identity | `vehicleId`, `name`, `model`, `year`, `color`, `trim`, `trimLabel`, `softwareVersion`, `fsdVersion`, `licensePlate` | Recognizing the right car at the curb. The full `vin` is **excluded** (MYR-279). |
| Location | `latitude`, `longitude`, `heading`, `speed`, `locationName`, `locationAddress` | The shared live map — the feature itself (FR-5.1). |
| Navigation | `destinationName`, `destinationAddress`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`, plus the §7.1 snapshot aliases `navDestinationName`, `navDestinationLocation`, `navOriginLocation`, `navEtaMinutes`, `navTripDistanceRemaining` | Where the car is going, and when it arrives. |
| Live trail | `driveTrailCoordinates` | The polyline behind the car on the live map. Distinct from the DRIVES resource, which [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369) made owner-only: this is the trip being watched, not stored history. |
| Charge | `chargeLevel`, `chargeState`, `estimatedRange`, `timeToFull` | A rider deciding whether the car can make the trip. Note the boundary against `chargePortDoorOpen`, which is **excluded**: "is it charging" is trip planning, "is the flap open" is an owner control tile. |
| Motion / availability | `status`, `gearPosition`, `rideShareEnabled`, `serviceEstimatedEndAt`, `setupState` | Whether the car is available and moving. `gearPosition` is `status`'s `x-atomic-group: gear` sibling and travels with it; it is motion state, not an actuable control. `setupState` ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491)) belongs to this group because it is the field that explains `status` for a car that has never streamed — withholding it from viewers would leave the rider-side picker with "offline" and nothing else, which is the MYR-437 defect. |
| Counters | `odometerMiles`, `fsdMilesSinceReset` | Neither media, cabin, nor a control — out of MYR-435's scope, and both are `required` in the schema. |
| Freshness | `lastUpdated` | Telling live from stale. |

**Withheld from viewers** (`vehicleStateOwnerOnlyFields`), grouped as the client described them:

| Group | Fields | Reasoning |
|-------|--------|-----------|
| Identity | `vin` | Predates MYR-435 ([MYR-279](https://linear.app/myrobotaxi/issue/MYR-279)): identifies the physical car and links to its location history ([`data-classification.md`](data-classification.md) §1.3, §2.1). |
| Media / now-playing | `mediaNowPlayingTitle`, `mediaNowPlayingArtist`, `mediaNowPlayingAlbum`, `mediaNowPlayingStation`, `mediaPlaybackSource`, `mediaPlaybackStatus`, `mediaVolume`, `mediaVolumeMax`, `mediaNowPlayingDurationMs`, `mediaNowPlayingElapsedMs` | The audit's sharpest example. The five free-text fields are **P1** user content whose accumulation reveals listening habits. The prior justification — "a rider in the car can already hear it" — is **retired**: a share grant is durable and REMOTE, so a viewer is frequently not in the car at all, and the mask cannot tell the difference. The P0 numerics go with the block because elapsed + duration is a live "someone is listening right now" occupancy signal even without the track name. |
| Cabin climate | `interiorTemp`, `exteriorTemp`, `hvacPower`, `isClimateOn`, `fanSpeed`, `driverTempSetting`, `passengerTempSetting`, `hvacAutoMode`, `hvacAcEnabled`, `seatHeaterLeft`/`Right`/`RearLeft`/`RearCenter`/`RearRight`, `seatCoolerLeft`/`Right`, `seatVentEnabled`, `seatCoolingCapable` | The cabin is the owner's private space; `interiorTemp` is a live occupancy signal. **`exteriorTemp` is the AMBIGUOUS case, resolved toward removal** — ambient air is closer to weather than to privacy, but a rider next to the car already knows the weather and no viewer surface renders it, so for a viewer it is data with no consumer (issue rule: default to REMOVE in doubt). `seatCoolingCapable` is an equipment fact that would otherwise sit with `trim`, but its only consumer is the seat-cooling control tile. **`interiorTemp` and `exteriorTemp` were moved OUT of the schema's `required` list in the same change** — see the standing rule below. |
| Vehicle controls | `locked`, `chargePortDoorOpen`, `frunkOpen`, `trunkOpen`, `virtualKeyPaired` | Everything that exists to drive the OWNER's control tiles. A viewer cannot actuate any of it (commands are owner-only at the routing layer), so the state renders a control they do not have — while telling them whether the owner's car is standing unlocked. `virtualKeyPaired` gates whether commands can be sent at all, so it is controls infrastructure. |

**Standing rule (unchanged, and newly load-bearing): a field this mask removes MUST be OPTIONAL in the resource's schema.** MYR-435 hit this directly — `interiorTemp` and `exteriorTemp` were in `required` in [`schemas/vehicle-state.schema.json`](schemas/vehicle-state.schema.json), so masking them would have made **every viewer snapshot invalid against the shape its own consumer decodes**, exactly the [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) `name` defect. Both were moved out of `required` in the same commit. Consumers with generated non-optional bindings must regenerate before this ships — see the §5.2.1.2 note.

##### 5.2.1.2 Consumer migration note (MYR-435)

Any client that decodes `VehicleState` from a **generated** binding compiled against the pre-MYR-435 schema will treat `interiorTemp` / `exteriorTemp` as non-optional and **fail to decode a viewer snapshot entirely** — not degrade, fail. The failure mode is a viewer whose live map never populates. Fields dropped from the *frame* path are safe either way, because `vehicle_update.fields` is a sparse map by contract (§4.1) — `VehicleUpdatePayload.fields` declares no `required` properties.

**The hazard set is exactly two fields, and it is closed.** Of the 34 fields MYR-435 withholds from viewers, only `interiorTemp` and `exteriorTemp` were in the schema's `required` list, and only those two are non-optional in the generated Swift `VehicleState` (`public var interiorTemp: Int` / `public var exteriorTemp: Int`). The other 31 — every media field, every other cabin field, `locked` / `chargePortDoorOpen` / `frunkOpen` / `trunkOpen`, and `vin` — are already `Optional` and decode to `nil` when absent. `virtualKeyPaired` is not declared on the generated `VehicleState` at all. No other generated type declares either temp.

**This repo's `schemas/` copy is NOT the artifact consumers build against.** The relaxation in this PR lands only in telemetry's `docs/contracts/schemas/`. The published package is **`myrobotaxi/contracts`** (SwiftPM, `@myrobotaxi/contracts`), whose `main` still marks both temps `required` as of **v0.28.0** — the latest tag. Because SwiftPM resolves by **git tag**, a commit on that repo's `main` is not enough:

1. Relax `required` in `myrobotaxi/contracts` `schemas/vehicle-state.schema.json` and regenerate `Sources/MyRobotaxiContracts/Generated/VehicleState.swift` so both temps become `Int?`.
2. Bump the package version **0.28.0 → 0.29.0** and **tag `v0.29.0`** — an untagged commit is invisible to SwiftPM.
3. Bump the iOS pin, which currently sits at **0.13.0** in `Packages/MyRoboTaxiKit/Package.resolved`, to a range that admits v0.29.0.
4. Only then deploy a telemetry server carrying this change.

Steps 1–3 are a separate PR in a separate repo and are **not** included here.

#### 5.2.2 Drive list (`GET /api/vehicles/{vehicleId}/drives`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` only | `id`, `vehicleId`, `startTime`, `endTime`, `date`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `startChargeLevel`, `endChargeLevel`, `fsdMiles`, `fsdPercentage`, `createdAt`, `startLocation`, `startAddress`, `endLocation`, `endAddress` | **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369).** There is no `viewer` row here because no grant of any shape opens this surface — including a legacy grant still carrying the retired `live_history` preset (§7.5.0). The gate is `vehicleAccessForOwnerOnly` (`internal/telemetry/vehicle_share_access.go`), which takes no share reader and issues no grant query at all, so there is nothing for a future edit to widen by passing a different capability. A viewer gets `403 vehicle_not_owned`. |

`startLocation`, `startAddress`, `endLocation`, `endAddress` are P1 per [`data-classification.md`](data-classification.md) §1.4 and are included on the list payload as of [MYR-145](https://linear.app/myrobotaxi/issue/MYR-145) (2026-06-06). Rationale: the drive-history scroll view needs origin and destination labels alongside the lightweight stats to render a meaningful list row — fetching them per-row via `GET /drives/{driveId}` would force one extra REST call per visible row. The four columns are short reverse-geocoded TEXT values (street address / place name, a few hundred bytes each at most) so the per-page payload remains comfortably under the ~5 KB-per-page list budget. The handler omits any of the four keys whose store value is empty (drive in progress with no end-side geocoding yet, or zero-GPS at drive start) per the §7.2 nullable-field convention.

`fsdMiles` and `fsdPercentage` are P0 per [`data-classification.md`](data-classification.md) §1.4 (FSD distance / ratio — aggregate, non-identifying) and are included on the list payload as of [MYR-152](https://linear.app/myrobotaxi/issue/MYR-152). Rationale: the drive-history view shows FSD usage per drive, and fetching it per-row via `GET /drives/{driveId}` would force one extra REST call per visible row. Both are small `double` columns already populated by the drive-write path, so the per-page payload stays well under the ~5 KB-per-page list budget. Unlike the location fields they are always present (non-nullable, default `0`). `routePoints`, `energyUsedKwh`, and `interventions` stay out of the projection per the heavy-payload / drive-detail rule below.

#### 5.2.3 Drive detail (`GET /api/drives/{driveId}`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` only | All FR-3.4 stats: `id`, `vehicleId`, `startTime`, `endTime`, `date`, `distanceMiles`, `durationSeconds`, `avgSpeedMph`, `maxSpeedMph`, `energyUsedKwh`, `startChargeLevel`, `endChargeLevel`, `fsdMiles`, `fsdPercentage`, `interventions`, `startLocation`, `startAddress`, `endLocation`, `endAddress`, `createdAt` | **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369).** The previous rule gave viewers the identical record, including the P1 start/end location and address, on the reasoning that a viewer had explicit consent via the invite they accepted and that withholding "the drive ended at the airport" defeated FR-5.1. **That reasoning retired with the `live_history` tier itself** (§7.5.0): the product no longer offers trip history to a viewer at any grant shape, so there is no consent to read it from. What a grant conveys now is live watching (§7.0, §7.1, the WebSocket stream) and, with `allowRides`, rides. Same `vehicleAccessForOwnerOnly` gate as §5.2.2; a viewer gets `403 vehicle_not_owned`. |

Does NOT include `routePoints` -- those are returned by the separate `GET /api/drives/{driveId}/route` endpoint (heavy payload; see §7.4 for the lazy-fetch rationale).

#### 5.2.4 Drive route (`GET /api/drives/{driveId}/route`)

| Role | Visible fields | Notes |
|------|----------------|-------|
| `owner` only | Full `routePoints` array | **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369).** The polyline is trip history, so it retires alongside §7.2 and §7.3 rather than separately from them. The old note read "the whole sharing use case is watching someone drive home" — that use case is served by the **live** surfaces (§7.0, §7.1, the WebSocket stream), which a viewer keeps in full; replaying where the car has already been is the part the product withdrew. Same `vehicleAccessForOwnerOnly` gate as §5.2.2; a viewer gets `403 vehicle_not_owned`. |

#### 5.2.5 Vehicle-sharing endpoints

| Endpoint | Role access | Notes |
|----------|-------------|-------|
| `POST /api/vehicles/{vehicleId}/invites` | `owner` only | A non-owner — INCLUDING a viewer of that vehicle at any tier — receives `403 vehicle_not_owned`. A viewer who could mint an invite would be re-sharing a car that is not theirs. |
| `GET /api/vehicles/{vehicleId}/invites` | `owner` only | Same. A viewer who could list would read the owner's private `label` for every other person they invited. |
| `DELETE /api/invites/{inviteId}` | `owner` only (`owner_user_id` match, enforced in the SQL `WHERE`) | A row that belongs to another owner is `404 not_found`, not 403 — indistinguishable from one that does not exist, so the endpoint is not an oracle for invite ids. |
| `PATCH /api/invites/{inviteId}` | `owner` only (same SQL predicate, carried by the `UPDATE` itself rather than a pre-read) | **ACCEPTED grants only.** A pending or revoked row updates zero rows: pending is `409 conflict` (there is no grant yet to edit), revoked joins missing-and-foreign under the same 404 rule. A viewer cannot reach this endpoint at all — they would otherwise be editing the controls their own access is governed by. |
| `POST /api/invites/{inviteId}/resend` | `owner` only (same SQL predicate) | Same 404 rule. |
| `POST /api/invites/redeem` | any authenticated caller | The one RIDER-facing route. It is not vehicle-scoped and has no role gate — the submitted code IS the authorization — but it is rate-limited per user (§7.5.5) and refuses `409` when the caller owns a target vehicle. |

Rationale: FR-5.2 and FR-5.3 assign the viewer list and revocation to owners explicitly. v1 does not support viewers inviting additional viewers, and MYR-184 did not change that.

**`ShareInvite` mask (`ResourceInvite` / `inviteOwnerFields` in `internal/mask/tables.go`):** `inviteId`, `vehicleId`, `label`, `permission`, `status`, `allowRides`, `suspended`, `code`, `shareUrl`, `createdAt`, `expiresAt`, `acceptedAt`. The `viewer` role has **no entry at all** for this resource, so the fail-closed lookup produces deny-all — a viewer who somehow reached the projection would get an empty object rather than an owner's labels and a live code.

**`allowRides` and `suspended` ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)) are the per-grant flags, and both are P0** — an authorization capability and an authorization state, the same tier as the `permission` and `status` they sit beside ([`data-classification.md`](data-classification.md) §1.15). They appear on **accepted rows only**; a pending invite has no grant for them to describe. Both are **owner-only by the same construction as every other entry here** — the viewer role has no entry in this resource at all — and that construction is load-bearing rather than incidental: a viewer able to read `suspended` could read the controls their own access is governed by, and a suspended viewer would learn they had been suspended, which §7.5.0 deliberately never tells them. `shareUrl` ([MYR-368](https://linear.app/myrobotaxi/issue/MYR-368)) is the signed join link and carries the same P1 bearer handling as the `code` it embeds — pending rows only, never logged (§7.5.6).

**MYR-184 rebuilt this allow-list.** It previously described a shape this server never served — `id`, `email`, `status`, `createdAt`, `acceptedAt`, `revokedAt` — modelled on the retired Prisma `Invite` table. Three corrections worth naming:

- **`email` is gone with no replacement.** This contract is code-based; no address is collected. Its stand-in is `label`, an owner-typed memo (P1, a person's name) that is never resolved to an account.
- **`revokedAt` is gone.** Revoked rows are tombstones and are never serialized at all, so a field describing when it happened has no wire moment to appear in. Keeping it implied revoked rows are returned.
- **`code` is new**, and is the one entry here that is a live **credential** rather than a description of one. Owner-only by construction, additionally omitted by the handler from any row that is not `pending`, and never logged.

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
| `metadata` | `{ "role": "viewer", "channel": "rest" \| "ws", "fieldsMasked": ["vin", ...], "endpoint": "/api/vehicles/{id}/snapshot" }` |

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
| `GET` | `/api/vehicles/{vehicleId}/drives` | Paginated drive history for vehicle | Bearer + **owner** of vehicleId (owner-only since MYR-369) | FR-3.2, FR-9.1, FR-9.2 |
| `GET` | `/api/drives/{driveId}` | Single drive detail (FR-3.4 stats + start/end addresses) | Bearer + **owner** of drive's vehicle (owner-only since MYR-369) | FR-3.4, FR-9.1 |
| `GET` | `/api/drives/{driveId}/route` | Full GPS polyline for drive playback | Bearer + **owner** of drive's vehicle (owner-only since MYR-369) | FR-3.3, NFR-3.23 |
| `POST` | `/api/vehicles/{vehicleId}/invites` | Mint a sharing code (one code, N vehicles) — §7.5.1 | Bearer + owner of vehicleId | FR-5.1 |
| `GET` | `/api/vehicles/{vehicleId}/invites` | List pending invites + accepted viewers — §7.5.2 | Bearer + owner of vehicleId | FR-5.2 |
| `DELETE` | `/api/invites/{inviteId}` | Cancel a pending invite / revoke an accepted grant (tombstone, 204, idempotent) — §7.5.3 | Bearer + owner of the invite | FR-5.3 |
| `PATCH` | `/api/invites/{inviteId}` | Edit one **accepted** grant's capabilities in place — `allowRides` / `suspended`, partial, accepted-only (409 on pending) — §7.5.7 | Bearer + owner of the invite | FR-5.1, FR-5.3, MYR-369 |
| `POST` | `/api/invites/{inviteId}/resend` | Re-mint the code + reset the 7-day expiry across **every** pending row of the invite (pending only) — §7.5.4 | Bearer + owner of the invite | FR-5.1, MYR-184 |
| `POST` | `/api/invites/redeem` | Rider-side join: accept every row backing a code, atomically — §7.5.5 | Bearer (self); rate-limited 10/min | FR-5.1, FR-5.4, MYR-184 |
| `DELETE` | `/api/users/me` | Delete own account + all data | Bearer (self only) | FR-10.1, FR-10.2, NFR-3.29 |
| `GET` | `/api/users/me/export` | GDPR Art. 15 / 20 portability export of every Prisma row owned by the caller | Bearer (self only) | FR-10, NFR-3.29 |
| `POST` | `/api/ride-requests` | Create a ride request (P10 ride-hailing) | Bearer + vehicle access | FR-9.3, NFR-3.21, NFR-3.23 |
| `GET` | `/api/ride-requests` | Rider's own ride-request history (paginated) | Bearer (self as rider) | FR-9.1, FR-9.3 |
| `GET` | `/api/ride-requests/incoming` | Owner's feed of open (`requested`) ride requests across their vehicles (paginated). Two mutually exclusive params select different slices of the same feed: `?upcomingForVehicle={vehicleId}` — the owner's **accepted, still-future reservations** for one car, soonest first (MYR-360); `?activeForVehicle={vehicleId}` — the ride that car is **serving right now** (MYR-400) | Bearer (self as owner) | FR-9.1, FR-9.3 |
| `GET` | `/api/ride-requests/{id}` | Single ride-request detail | Bearer + party (rider or owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/cancel` | Party-aware cancel (MYR-522): the RIDER from `requested`/`accepted`, the OWNER from `accepted`/`arrived` | Bearer + party (rider or owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/picked-up` | Owner confirms pickup — rider is aboard (accepted→arrived) | Bearer (owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/start` | Rider starts the ride (arrived→enroute; triggers the leg-2 dropoff nav push) | Bearer (rider) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/dropped-off` | Owner confirms dropoff — ride complete (enroute→completed) | Bearer (owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/accept` | Owner accepts a `requested` ride (emits the MYR-176 dispatch seam) | Bearer (owner) | FR-9.3 |
| `POST` | `/api/ride-requests/{id}/decline` | Owner declines a `requested` ride, or an **`accepted` SCHEDULED** one (MYR-360) | Bearer (owner) | FR-9.3 |
| `POST` | `/api/vehicles/{vehicleId}/command/{name}` | Send a Tesla vehicle command (P11 actuation + P10 dispatch) | Bearer + owner of vehicleId | FR-11.x, NFR-3.21 |
| `POST` | `/api/tesla/vehicles/{vehicleId}/refresh` | On-demand state refresh: wake if needed, one `vehicle_data` read (§7.15) | Bearer + owner of vehicleId | FR-1.1, FR-2.1, NFR-3.21 |
| `PUT` | `/api/tesla/vehicles/{vehicleId}/service-window` | Owner's "expected back" fallback for `serviceEstimatedEndAt` — the value Tesla's own estimate outranks (§7.16) | Bearer + owner of vehicleId | FR-9.3, NFR-3.21 |
| `PUT` | `/api/tesla/vehicles/{vehicleId}/ride-share` | Owner's ride-sharing switch: pause or resume ride requests for one car (§7.18) | Bearer + owner of vehicleId | FR-9.3, NFR-3.21 |
| `PUT` | `/api/push/devices` | Register or refresh this installation's APNs device token (§7.17.1) | Bearer (self only) | FR-9.3, NFR-3.21, MYR-186 |
| `DELETE` | `/api/push/devices` | Unregister an APNs device token on sign-out (§7.17.2) | Bearer (self only) | FR-9.3, NFR-3.21, MYR-186 |
| `GET` | `/api/users/me/push-prefs` | Read the caller's five notification-category switches (§7.19.1) | Bearer (self only) | FR-9.3, NFR-3.21, MYR-349 |
| `PUT` | `/api/users/me/push-prefs` | Change some of them — partial body, echoes the whole set (§7.19.2) | Bearer (self only) | FR-9.3, NFR-3.21, MYR-349 |
| `GET` | `/api/users/me/places` | Read the caller's saved Home/Work places — 0–2 rows, `[]` when none (§7.20.1) | Bearer (self only) | FR-9.3, NFR-3.21, NFR-3.23, MYR-321 |
| `PUT` | `/api/users/me/places/{kind}` | Set or replace one saved place — whole-object upsert, echoes the stored row (§7.20.2) | Bearer (self only) | FR-9.3, NFR-3.21, NFR-3.23, MYR-321 |
| `DELETE` | `/api/users/me/places/{kind}` | Forget one saved place — 204, idempotent (§7.20.3) | Bearer (self only) | FR-9.3, NFR-3.21, MYR-321 |
| `POST` | `/api/ride-requests/{id}/activity-token` | Register (or rotate) the ActivityKit push token for the rider's Live Activity on this ride — upsert on `(ride, rider)`, clears any end tombstone (§7.21.1) | Bearer + **rider** of the ride (owner → 403; non-party → 404) | FR-9.3, NFR-3.21, MYR-172 |
| `DELETE` | `/api/ride-requests/{id}/activity-token` | End the Live Activity registration when the Activity ends on the phone — 200 `{ended}`, idempotent (§7.21.2) | Bearer + **rider** of the ride (owner → 403; non-party → 404) | FR-9.3, NFR-3.21, MYR-172 |
| `GET` | `/api/vehicles/{vehicleId}/booked-windows` | The vehicle's blocked time windows so a rider's schedule picker can dim conflicting slots BEFORE submitting — the read side of the §7.8 MYR-383 gate, derived from the same predicate and constant (§7.22) | Bearer + **owner of vehicleId, or a viewer whose grant carries `allowRides`** — byte-for-byte the ride-CREATE gate | FR-9.1, FR-9.3, NFR-3.21, MYR-385 |
| `POST` | `/api/ride-requests/join` | Redeem a group ride's shared link: join the ride as a member and receive the full `RideRequest` with yourself in `members` — idempotent per joiner, code in the BODY (§7.24) | Bearer (self); rate-limited 10/min | FR-9.3, NFR-3.21, NFR-3.23, MYR-540 |
| `POST` | `/api/tesla/vehicles/{vehicleId}/complete-setup` | Finish onboarding without a human trigger: wake, confirm the virtual key with Tesla, force the fleet-config re-push, and answer with the resulting `setupState` (§7.23) | Bearer + owner of vehicleId | FR-1.1, NFR-3.21, MYR-505 |
| `POST` | `/api/auth/apple` | Native Sign in with Apple → ES256 access + refresh pair | None (pre-auth; per-IP rate-limited) | FR-6.1, MYR-193 |
| `POST` | `/api/auth/refresh` | Single-use refresh-token rotation | Refresh token in body (pre-auth; per-IP rate-limited) | FR-6.2, MYR-193 |
| `POST` | `/api/auth/revoke` | Revoke a refresh-token family (sign-out) | Refresh token in body (pre-auth; per-IP rate-limited) | MYR-193 |
| `GET` | `/api/auth/.well-known/jwks.json` | Public ES256 verification keys (JWKS) | None (public) | MYR-193 |

The `POST`/`GET /api/ride-requests[...]` rows (§7.8) are the P10 ride-hailing surface: the four rider-facing endpoints are mounted as of MYR-174, the owner-facing incoming feed + accept/decline as of MYR-175. **MYR-270 replaced the MYR-265 auto-leg model with an owner-driven handshake** (the rider-`board` endpoint and the drive-end auto-completion are retired): the owner confirms **picked-up** (`accepted → arrived`) and **dropped-off** (`enroute → completed`); the **rider** confirms **start** (`arrived → enroute`), which is what fires the leg-2 dropoff nav push. The pickup nav push landed with MYR-176 and the dropoff nav push moved to the rider **start** endpoint with MYR-270. The reschedule endpoints remain with MYR-192.

The `/api/auth/*` rows (§7.10) are the identity module's auth surface (MYR-193, ADR-001 `docs/architecture/adr-001-identity-module.md`): native Sign in with Apple, ES256 access-token minting with a published JWKS, and rotating refresh tokens. Unlike every other row, these are **pre-authentication** endpoints (they mint or rotate the very credential the others require), so they are not Bearer-gated — they are protected by a per-IP token-bucket rate limit instead (§4.1.2).

`GET /api/vehicles` (§7.0) is mounted by the Go server as of MYR-91 (2026-05-10). `GET /api/vehicles/{vehicleId}/snapshot` (§7.1) and `GET /api/vehicles/{vehicleId}/drives` (§7.2) are mounted as of MYR-133 (2026-06-03). `GET /api/drives/{driveId}/route` (§7.4) is mounted as of PR #260 (DV-20), and `GET /api/drives/{driveId}` (§7.3) as of MYR-130 (2026-07-02) — DV-20 is fully RESOLVED (see §10). The §7.5 vehicle-sharing endpoints are mounted on the Go server as of MYR-184 (2026-07-29), which SUPERSEDES the §7.5 half of DV-23. `DELETE /api/users/me` (§7.6) is mounted on the Go server as of **MYR-355** (2026-07-30), which SUPERSEDES the remaining half of DV-23 — DV-23 is now superseded in full. `GET /api/users/me/export` (§7.7) is the one path still assigned to the Next.js app.

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
      "role": "owner",
      "hasActiveRide": false,
      "licensePlate": "ABC 1234",
      "serviceEstimatedEndAt": null,
      "rideShareEnabled": true,
      "setupState": null,
      "trimLabel": "Performance",
      "location": { "latitude": 37.7749, "longitude": -122.4194 }
    },
    {
      "vehicleId": "clsetup491newownercar",
      "name": "Amruth's X Plaid",
      "model": "Model X",
      "year": 2025,
      "color": "",
      "vinLast4": "6683",
      "status": "offline",
      "chargeLevel": 0,
      "estimatedRange": 0,
      "lastUpdated": "2026-08-09T02:10:00Z",
      "role": "owner",
      "hasActiveRide": false,
      "licensePlate": "",
      "serviceEstimatedEndAt": null,
      "rideShareEnabled": true,
      "setupState": {
        "state": "awaiting_virtual_key",
        "since": "2026-08-09T02:10:00Z"
      },
      "trimLabel": null,
      "location": null
    }
  ]
}
```

The second row is the [MYR-503](https://linear.app/myrobotaxi/issue/MYR-503) scenario verbatim: a car linked minutes ago whose owner has not paired the virtual key, so Tesla refused the telemetry config with `missing_key`, `status` is still the write-once `offline` schema default, and every signed command against it will be rejected. Without `setupState` that row is indistinguishable from a dead car — which is exactly how a tester came to tap a Lock button that could never work.

##### VehicleSummary fields

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | P0 | Opaque DB cuid. The SDK uses this in every other endpoint's path parameter (`/api/vehicles/{vehicleId}/snapshot`, etc.). |
| `name` | `string` | P1 | User-assigned vehicle name. P1 because it's commonly a recognizable nickname. **RBAC: BOTH roles.** This cell said "owner-visible only" until MYR-435 corrected it — [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) had already removed the `name` subtraction from the viewer mask (§5.2.0), both because the rider UI renders "{Owner}'s {Vehicle}" and because `name` is `required` in `vehicle-summary.schema.json`, so stripping it made every viewer row invalid against its own schema. |
| `model` | `string` | P0 | Tesla model: `Model 3`, `Model S`, `Model X`, `Model Y`, `Cybertruck`, etc. **Populated for EVERY car since [MYR-507](https://linear.app/myrobotaxi/issue/MYR-507)** — the field, the type and the masks are all unchanged; only the PROVENANCE is new, exactly as MYR-320 did one column over for `color`. Until then this was filled only for vehicles linked through the **Prisma web flow**: the Go server's own provisioning INSERT (`queryUpsertOwnedVehicle`) seeded the empty placeholder `''` under a comment promising "the web sync / streaming pipeline fills real values later", and no such writer has ever existed on either side — so a car linked through the Go path reported an **empty model forever**. That is half of what produced the MYR-507 field report. **Source:** position 4 of the VIN, the model line (`internal/vin`), written at link time by the provisioning INSERT and backfilled for existing rows by `store.VehicleRepo.FillVehicleIdentity` on the same non-waking connectivity-edge / periodic in-service read that carries the colour write — the **fifth** §1.4 carve-out in `data-lifecycle.md`. Deliberately NOT from Tesla: the VIN needs no token, no awake car and no network call, so a car is identified from the instant it is linked rather than waiting on an edge it may never produce. **FILL-IF-EMPTY, NEVER OVERWRITE** — a populated column is left exactly as it is, because the Prisma flow writes a richer value for some cars (`Model 3 Performance`) than a VIN position can encode (`Model 3`), and a blind overwrite would trade this bug for a subtler one. **Empty-value convention** unchanged: the key is ALWAYS present, and `''` means "not determined" — a VIN whose model code this server does not recognise, which it will never guess at. Keep a fallback label for `''` and never render an empty model fragment in a descriptor. |
| `year` | `integer` | P0 | Model year. **Populated for EVERY car since [MYR-507](https://linear.app/myrobotaxi/issue/MYR-507)** — the same placeholder problem (seeded `0`, never filled for a Go-provisioned car), the same fix and the same fill-if-empty rule as the sibling `model` above, which see. **Source:** position 10 of the VIN, the ISO 3779 model-year code. This one has no alternative: **Tesla's Fleet API carries no model year at any path** — not on `vehicle_data`, not on `vehicle_config`, nowhere — so the VIN is the platform's only source of a year, and once the year comes from the VIN the model comes from the same 17 characters for free. Only the 2010–2030 letter window is recognised; a digit is ambiguous between `200x` and `203x` until 2031 and is deliberately not read. **Zero-value convention:** the key is ALWAYS present, and `0` means "not determined". **`0` is never a real model year** — treat it exactly as an empty `model` and drop the fragment from the descriptor rather than rendering "Year 0". |
| `color` | `string` | P0 | Display color, read off the Prisma-owned `Vehicle.color` column as an identity-row field alongside `name`/`licensePlate`. **Tesla-populated since [MYR-320](https://linear.app/myrobotaxi/issue/MYR-320)** — the column, the type, and the masks are all unchanged; only the PROVENANCE is new. The value was never actually filled before (the MYR-257 provisioning INSERT seeds `''`) and now comes from REST `vehicle_data.vehicle_config.exterior_color` (live-verified `"Quicksilver"`), written by a single-column owner-scoped UPDATE (`store.VehicleRepo.UpdateVehicleColor`, the fourth §1.4 carve-out in `data-lifecycle.md`) on the non-waking connectivity-edge / periodic in-service read. **An empty Tesla value NEVER overwrites a good one**, so a partial payload cannot blank the colour. **Empty-value convention:** this server ALWAYS emits the key and uses an empty string for "not read yet" — the same convention `licensePlate` matches. No WebSocket delta; it refreshes on the next list fetch. |
| `vinLast4` | `string` | P0 | Last 4 characters of the VIN — full VIN is never emitted per `data-classification.md` §1.5 + `redactVIN()`. |
| `status` | `string` (enum) | P0 | One of `driving`, `parked`, `charging`, `offline`, `in_service`. Mirrors `VehicleStatus` Prisma enum. `in_service` is derived + persisted as of MYR-259 (`ServiceMode` proto 159 push OR Tesla REST `in_service`; see `vehicle-state-schema.md` §2.4) — additive, always in the enum. |
| `chargeLevel` | `integer` | P0 | Battery state of charge, 0–100. Lightweight indicator for the catalog view; full charge group lives in `/snapshot`. |
| `estimatedRange` | `integer` | P0 | Estimated remaining range in miles. Same lightweight rationale. |
| `lastUpdated` | `string` (ISO 8601) | P0 | Timestamp of the last telemetry write to this vehicle. The catalog uses it to render "last seen N minutes ago." |
| `role` | `string` (enum) | P0 | `owner` or `viewer`. The caller's relationship to the vehicle. See RBAC below. |
| `hasActiveRide` | `boolean` | P0 | **OPTIONAL** ([MYR-233](https://linear.app/myrobotaxi/issue/MYR-233)). `true` iff the vehicle currently has an OPEN INSTANT ride request. **Derivation:** a `go_ride_requests` row with `scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')` — character-for-character the predicate of the per-vehicle partial unique index `uq_go_ride_requests_active_instant_vehicle` (migration 0013, [MYR-266](https://linear.app/myrobotaxi/issue/MYR-266)), so the flag and the accept guard can never disagree (flag true ⇒ an accept would 409; an accept 409s ⇒ flag true). The index also bounds the match at one row per vehicle. Scheduled rides are EXEMPT (a reservation never makes the car busy) and `requested` does NOT count (many riders may hold pending requests against one idle car). Computed in the list query as a correlated `EXISTS` — one statement for the whole catalog, no N+1. **v1 no-WS-push caveat:** derivation is REST-read-time only; §8 pushes no `hasActiveRide` frame to non-party viewers, so a rider's Busy badge refreshes on the next list fetch, not live. **Absence semantics:** this server version always emits the field (`true`/`false`); a missing key means the server predates MYR-233 — consumers MUST read that as "availability unknown → treat as available" and MUST NEVER render Busy from absence. |
| `licensePlate` | `string` | **P1** | **OPTIONAL** ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286)). The owner-entered license plate, same value and semantics as `VehicleState.licensePlate` (§7.1). Read off the Prisma-owned `Vehicle.licensePlate` column as an identity-row field alongside `name`/`color` — **not telemetry** and **not from Tesla** (the Fleet API exposes no plate anywhere); it exists only because the owner typed it via §7.14. Already normalized on write (trim, uppercase, ≤ 10 chars, charset `[A-Z0-9 -]`) — consumers MUST NOT re-normalize before display. **Empty-value convention:** this server ALWAYS emits the key and uses an **empty string** for "no plate set", matching the sibling `color` exactly; an ABSENT key means a pre-MYR-286 server. Neither ever means "we could not read the plate" — keep the `VIN ····xxxx` fallback (built from `vinLast4`) for both, and never render an empty plate in a catalog row. **RBAC:** in BOTH role allow-lists (§5.2.0) despite being P1 — the plate exists so a rider can identify the correct car at pickup. **v1 no-WS-push caveat:** no WebSocket delta; the plate refreshes on the next list fetch. |
| `serviceEstimatedEndAt` | `string` (RFC 3339, UTC) or `null` | P0 | **OPTIONAL** ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316), contracts v0.17.0). When this vehicle's current service visit is estimated to END — the same value and the same semantics as `VehicleState.serviceEstimatedEndAt` (§7.1), resolved by one shared helper so the catalog row and the snapshot can never disagree. **Resolution:** `COALESCE(service_etc, service_expected_end_at)` over two nullable columns on the Go-owned `go_vehicle_control_state` side table (migration **0017**), where `service_etc` is Tesla's own estimate (Fleet API `GET /api/1/vehicles/{vin}/service_data`) and `service_expected_end_at` is what the owner typed via §7.16 — Tesla wins, the owner is the fallback. **Emitted only while `status` is `in_service`;** every other status is `null`, and the monitor additionally CLEARS both columns when it observes the car leaving service, so the gate is belt-and-braces and a stale window can never outlive the visit. **`null` is the common case, not a failure:** Tesla returns an all-null `service_data` body for a visit with no appointment record, so a car can be legitimately in service with no estimate at all. **Consumer rule:** floor the scheduling picker here; when the value is `null` or the key is ABSENT there is **NO BOUND** and scheduling stays fully open — never block a booking on missing data. **RBAC:** BOTH roles (§5.2.0) — a rider needs the window for the same reason the owner does, because it floors the picker. **v1 no-WS-push caveat:** not streamed; a `vehicle_update` NEVER carries it, so it refreshes on the next list fetch. |
| `rideShareEnabled` | `boolean` | P0 | **OPTIONAL** ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342), contracts v0.20.0). Whether this vehicle's OWNER currently accepts ride requests against it — the same value and the same semantics as `VehicleState.rideShareEnabled` (§7.1), read from the same column by the same expression so the catalog row and the snapshot can never disagree. **`true` is the ordinary state** and the state every vehicle starts in. **`false` means the owner has PAUSED ride requests for this car:** the vehicle may be online, parked, charged and idle — nothing about it has changed — but the server refuses new ride requests against it and refuses to accept outstanding ones (`409 vehicle_unavailable`, §7.8). **Source:** `ride_share_enabled BOOLEAN NOT NULL DEFAULT true` on the Go-owned `go_vehicle_control_state` side table (migration **0021**), emitted as `COALESCE(gcs.ride_share_enabled, TRUE)` — a car with no side-table row at all (the common case) reads `true`. **OWNER INTENT, NOT VEHICLE STATE:** unlike the siblings `status` and `hasActiveRide`, nothing the car does sets or clears it and no timer expires it. It moves only through §7.18, and only for the owner. That is why the pause applies to SCHEDULED rides too — a deliberate deviation from the [MYR-313](https://linear.app/myrobotaxi/issue/MYR-313) exemption, argued in §7.8. **Absence semantics:** this server always emits the field (`true`/`false`); a missing key means the server predates MYR-342 and consumers MUST read it as **ENABLED**. Absence NEVER means paused — never hide the request affordance, and never fail closed, on a missing key. **RBAC:** BOTH roles (§5.2.0) — the viewer is the party this value is *about*, and a rider who cannot see it learns the car is paused only from a 409 after composing a whole request. **v1 no-WS-push caveat:** not streamed; a `vehicle_update` NEVER carries it, so it refreshes on the next list fetch, the next `/snapshot`, or from the echo §7.18 returns. |
| `setupState` | `object` or `null` | P0 | **OPTIONAL** ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491), contracts v0.24.0). What is still standing between this vehicle and live telemetry, or `null` when there is nothing to say — **the same object, the same members and the same semantics as `VehicleState.setupState` (§7.1)**, produced by the SAME server-side derivation over the same inputs, so a catalog row and a snapshot for one vehicle can never disagree. Members: `state` (`awaiting_virtual_key` \| `configuring` \| `token_failed`) and `since` (RFC 3339 UTC). Read §7.1's entry for the derivation rules, the honesty guarantees, and the per-member owner action. **Carried on the catalog as well as the snapshot for a DIFFERENT consumer:** the owner's card is snapshot-fed, but the rider-side picker reads list rows, and today it renders a never-streamed shared car as permanently "offline" ([MYR-437](https://linear.app/myrobotaxi/issue/MYR-437)) — `Vehicle.status` is `NOT NULL DEFAULT 'offline'` and nothing writes it until the car's first mTLS stream event. This field is what lets the picker say "still being set up", greyed with a reason, instead of omitting the car or lying about it, and it cannot fetch a `/snapshot` per row to find out. (Contrast `hasActiveRide`, which is list-only; `setupState` is on both surfaces because both surfaces have a consumer.) **`null` is NOT `ready`:** it means the server makes no claim — a healthy streaming car, a car nobody has examined, or an expired claim all read `null`, and a consumer MUST NOT paint a positive "set up ✓" badge from it. **Absence semantics:** this server always emits the key; a missing key means a server predating MYR-491 and MUST be read exactly like `null`. **RBAC:** BOTH roles (§5.2.0) — P0 operational state about the car, the same tier as the sibling `status` it qualifies, carrying no VIN, no coordinate, no credential and no user content, so it is log-safe in full. **v1 no-WS-push caveat:** not streamed; a `vehicle_update` NEVER carries it, and by construction a car in any of these states is not sending frames to attach it to. It refreshes on the next list fetch or `/snapshot`. |
| `trimLabel` | `string` or `null` | P0 | **OPTIONAL** ([MYR-507](https://linear.app/myrobotaxi/issue/MYR-507), contracts v0.31.0). The DISPLAY-READY trim of this vehicle — `Plaid`, `Performance`, `Long Range` — and **the same value, the same column and the same semantics as `VehicleState.trimLabel` (§7.1, [MYR-320](https://linear.app/myrobotaxi/issue/MYR-320))**. There is nothing derived here and nothing to keep in step: the snapshot does not compute this field either, it reads it, and the catalog now reads the same column, so the two surfaces cannot name one car two different things. **Source:** `trim_label` on the Go-owned `go_vehicle_control_state` side table (migration **0018**), written from Tesla REST `vehicle_data.vehicle_config.performance_package` on the non-waking connectivity-edge / periodic in-service read. It rides the `go_vehicle_control_state` join the MYR-316 window and the MYR-342 switch already pay for, so it costs one short text column and no new index probe. **On the catalog as well as the snapshot for a consumer the snapshot cannot serve:** a **viewer never fetches a `/snapshot`** — owners get model and trim from §7.1, riders have only this row. With the trim withheld as "detail-sheet only", a shared car's descriptor ladder had nothing identifying left and degraded to the raw colour token: a lock-screen Live Activity subline reading `UltraRed`, and a review-sheet row whose model slot rendered the bare make `Tesla`. Together with the siblings `year`, `model` and `color`, a row now carries everything the client's target descriptor needs — `2026 Model X Plaid · Ultra Red`. **NOT the raw badge code:** `VehicleState.trim` carries Tesla's `trim_badging` (`p74d`), which is explicitly not display-safe and is absent from the catalog under **both** roles; only the label may be rendered. **`null` is NOT the empty string** and is the ordinary state for a car Tesla has not been asked about yet — it means "no trim known", and a descriptor builder MUST drop the fragment entirely rather than emit a stray separator. **Absence semantics:** this server always emits the key; a missing key means a server predating MYR-507 and MUST be read exactly like `null`. **RBAC:** BOTH roles (§5.2.0) — an equipment fact of exactly the tier of its identity siblings `model` / `year` / `color`, all three of which have been in both allow-lists since v1, and all four of which are legible through a windshield from the kerb (the badge on the boot lid IS the trim). There is nothing here for masking to protect, and the viewer is in fact the party the field is *for*. **v1 no-WS-push caveat:** not streamed; a `vehicle_update` NEVER carries it, so it refreshes on the next list fetch or `/snapshot`. |
| `location` | `object` or `null` | **P1** | **OPTIONAL** ([MYR-515](https://linear.app/myrobotaxi/issue/MYR-515), contracts v0.31.0). Where this vehicle was last known to be — `{ "latitude": number, "longitude": number }`, or `null` when the server holds no fix. **The same coordinate pair, at the same precision, as `VehicleState.latitude` / `longitude` (§7.1)** and as the live `vehicle_update` frame: read from the same AES-256-GCM-encrypted columns by the same resolver, so the catalog and the snapshot cannot disagree about where a car is. **Why it is on the catalog at all** — the one exception to the thin-catalog rule, argued in "Excluded from the list response" below: the rider's picker renders a **per-row pickup ETA** and has no other input for a row it is not watching. A client holds exactly ONE coordinate (the watched car's, from the single-vehicle WebSocket subscription), and a viewer's per-row `/snapshot` is **403 by design** (MYR-432/449), so without this field the picker can only rank the car the user already opened. **A NULLABLE OBJECT, not two nullable scalars:** `latitude`/`longitude` are an ATOMIC GROUP (§5.4) and splitting one is itself a contract violation, so nesting makes "both or neither" structural rather than conventional — the half-state two flat fields would permit has no representation here. Consumers MUST treat the object as indivisible and MUST NOT carry one axis forward from an earlier read. **`null` is the ONLY "no fix" spelling on this surface, and that is a DELIBERATE DIVERGENCE from §7.1.** `VehicleState`'s columns are `NOT NULL`, so that surface cannot express absence and uses the `(0, 0)` sentinel (`vehicle-state-schema.md` §2.3). This surface *can* express absence, so it does: the server collapses an exact `(0, 0)` to `null` before emitting, and **a consumer will never receive `0,0` here**. Do not port a Gulf-of-Guinea guard to this field, and do not treat `{latitude: 0, longitude: 0}` as reachable — a picker handed the sentinel does not fail loudly, it confidently renders an ETA to a point in the Atlantic. **RBAC: BOTH roles**, and this needs the heaviest justification on the row because it is the FIRST P1 field on this resource other than `licensePlate`. A viewer **already receives** `latitude`/`longitude` at full precision on the **streaming** path for exactly these vehicles — `vehicleStateViewerFields` retains the whole Speed/GPS atomic group ("the entire point of a shared live map") and [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435)'s narrowing deliberately left it — so this is the same value over a different transport to a party already entitled to it, not a new disclosure. **The honest caveat:** the catalog hands a viewer N positions at once where the socket hands out one per subscription. That is an **aggregation** difference, not an entitlement one — the grant is per-vehicle and a viewer could subscribe to each car in turn for the same data — so what changes is convenience, not reach. A row exists only because a live accepted grant produced it (the shared-catalog join IS the access check), so a suspended or revoked grant yields no row and therefore no coordinate. **On coarsening: considered and deliberately NOT applied.** The platform has no precision convention to follow, and rounding this row while the socket serves the exact value to the same viewer would protect nothing while measurably degrading the short-hop pickup ETA the field exists for. If a future policy coarsens shared positions it MUST coarsen the stream and this row together — splitting them would leave the weaker surface setting the real privacy bound. **P1 handling binds in full:** never logged (`data-classification.md` §2.2), never emitted outside the vehicle's party, encrypted at rest. **v1 no-WS-push caveat:** REST-read-time only — no `vehicle_update` frame carries a `location` object under this name (the live position arrives as the flat `VehicleState` fields), so this value is only as fresh as the last write to the vehicle row and a consumer actively watching a car SHOULD prefer the socket's coordinate over this one. |

##### Excluded from the list response

The list is deliberately a thin catalog. The following are NOT in `VehicleSummary` and require `/snapshot` to fetch:

- `heading`, and the reverse-geocoded location name / address
- Navigation atomic group (`destination*`, `origin*`, `etaMinutes`, `tripDistanceRemaining`, `navRouteCoordinates`)
- Speed, gear, climate, odometer, FSD-miles, full charge group (`chargeState`, `timeToFull`)
- `fsdVersion` and the raw `trim` badge code (contrast the display-safe `trimLabel`, which IS on the row — see §5.2.0)

**The car's own coordinates were on this list until [MYR-515](https://linear.app/myrobotaxi/issue/MYR-515)** and are now carried as the nullable `location` object. That is the single exception to the thin-catalog rule, and it is granted because the rationale below has a gap the rest of the list does not: *"drill into the one you clicked"* presumes the consumer can drill in at all. The rider's picker must rank **every** row by pickup ETA **before** the user has clicked anything, it holds exactly one coordinate (the watched car's, from the single-vehicle WebSocket subscription), and a viewer's per-row `/snapshot` is **403 by design** (MYR-432/449). There is no drill-in to defer to. The `heading` and the geocoded labels stay off because nothing on the picker needs them, and the destination / origin pairs and the route blob stay off because they are the expensive part and no catalog row emits them.

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
- **Viewer** sees every `VehicleSummary` field, **PLUS** `sharePermission`, for every vehicle where an ACCEPTED `go_vehicle_shares` grant names the caller. (This bullet said "EXCEPT `name`" until MYR-435 corrected it — [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184) removed that subtraction, and the field table above now says so too.)
- **[MYR-435](https://linear.app/myrobotaxi/issue/MYR-435) did NOT narrow this resource.** The client's removals — media, cabin climate, vehicle-controls state — are all in the "Excluded from the list response" set above and have never been on a catalog row; the narrowing applies to `/snapshot` and to WS frames (§5.2.1.1). `TestVehiclesListCarriesNoRemovedField` now guards this for BOTH roles, so a later "show a lock icon in the catalog" fails a test rather than quietly re-opening what §5.2.1 closed.
- **Implementation note (updated by [MYR-184](https://linear.app/myrobotaxi/issue/MYR-184), 2026-07-29): the viewer merge is LIVE.** This bullet previously said owner-only was the only reachable path, that the merge required reading the Prisma-owned `Invite` table, and that viewer-tier callers received an empty list. None of that is true any more: sharing is Go-owned (`go_vehicle_shares`, migration 0020) and a rider who redeemed a code sees exactly the cars they were granted. The response is **owner rows first, then shared rows** — the two come from separate lean queries so the owner path's index plan is untouched. A failure of the shared-vehicle read is logged and swallowed, degrading to the caller's own cars rather than 500-ing their whole garage; the degraded set is always strictly SMALLER, never wider. **Extended by [MYR-540](https://linear.app/myrobotaxi/issue/MYR-540): a THIRD source follows the shared rows — the vehicles of LIVE group rides the caller has JOINED** (`go_ride_members`, non-terminal statuses only — character-for-character the WS access set's membership leg, so the catalog and the handshake name the same cars). Member rows are `role: "viewer"` projections carrying the ZERO grant (no ride capability — riding in a car buys watching it, never booking it), deduplicated against the two earlier halves: an owner keeps their owner row, a share keeps its capability row, and the member row appears only when membership is the caller's only relationship to the car. Same swallow-and-degrade failure rule; access ends with the ride because the join's status predicate IS the lifetime — no revocation step exists or is needed.

#### Idempotency

`GET` is naturally idempotent. The catalog is read-only — a repeat call returns equivalent data modulo any new vehicle the user just linked.

#### Implementation notes

- The handler lives in `internal/telemetry/vehicles_list_handler.go` (analogous to `vehicle_status_handler.go`). It calls `VehicleRepo.ListSummariesByUser(ctx, userId)` for the owner slice and `VehicleRepo.ListSharedSummariesByUser(ctx, userId)` for the viewer slice (`internal/telemetry/vehicles_list_viewer.go`), the latter joining `go_vehicle_shares` on `accepted_by_user_id` + `status='accepted'` so the grant IS the filter.
- The mask projection is applied at the handler layer using `mask.For(mask.ResourceVehicleSummary, role)` — same plumbing as the snapshot endpoint. Owners and viewers branch on the role returned by `authenticator.ResolveRole(ctx, userId, vehicleId)` for each list item.
- No audit-log row is emitted for the list itself (reads are not P1+ per `data-lifecycle.md` §4.2). The per-row mask projection's `fieldsMasked` count is observable via the existing REST mask-audit hook (1% sample); since MYR-184 the viewer projection genuinely strips (`name`), so this is no longer always the identity.

#### Forward-looking: pagination

When a single user can plausibly own > 100 vehicles (fleet operators?), this endpoint will gain `cursor` + `limit` query params per `§4.2` cursor-based pagination. v1 returns the full list in one response and omits the `nextCursor` / `hasMore` envelope — the response is a bare `{ items: [...] }`. SDK consumers that handle the future paginated shape can branch on the absence of `nextCursor` to know they're talking to a v1 server.

---

### 7.1 `GET /api/vehicles/{vehicleId}/snapshot`

> **Anchored:** NFR-3.5, NFR-3.11, FR-1.1, FR-1.2, FR-2.1.

> **MYR-269 — owner-control hydration (NFR-3.5).** The snapshot now returns the five owner-control read-backs `locked`, `frunkOpen`, `trunkOpen`, `isClimateOn`, `chargePortDoorOpen` for a **non-streaming** car (in service / asleep / offline), sourced from the Go-owned `go_vehicle_control_state` side table (LEFT-joined by `VehicleRepo.GetByID`). Persisted on both the live stream path and the MYR-260 `/vehicle_data` backfill; each is nullable and omitted/null when never read (an honest "unavailable", never a fabricated on/off). Wire names match the live WS `vehicle_update` fields so the client reconciles REST and WS against one field set. `Vehicle.status` remains persisted independently of these controls. The remaining MYR-252 cabin fields stay WS-live-only pending MYR-253 — see `vehicle-state-schema.md` §1.1.

> **MYR-298 — seat-vent + media hydration completes the cabin set (NFR-3.5).** The snapshot additionally returns `seatVentEnabled` (`boolean` or `null`) and `mediaPlaybackStatus` (`string` enum `Stopped`/`Playing`/`Paused`, or `null`), the last two contracted `vehicle_update` fields that were neither persisted nor emitted here. Before this, a client that missed the live WS frame — a backgrounded phone, a sleeping car, any socket drop — could **never** learn them. Both come from the same `go_vehicle_control_state` LEFT JOIN, are nullable, and carry the sibling absent-vs-null semantics exactly: **the key is always present on the owner projection**, and a never-read value is an explicit `null` (honest-unknown) rather than an omitted key or a fabricated `false`/`"Stopped"`. A streamed `mediaPlaybackStatus` of `"Unknown"`/empty persists NULL and never overwrites a known status, mirroring MYR-274's handling of `"Unknown"` `hvacAutoMode`. **Neither field is written by the MYR-260 `/vehicle_data` backfill** — Tesla's cached `vehicle_data` climate subset carries neither value, so the stream is their only source; this also keeps them clear of the backfill-overwrites-fresher-stream issue tracked in [MYR-300](https://linear.app/myrobotaxi/issue/MYR-300). With this, 20 of the 21 MYR-252 cabin read-backs are snapshot-backed; only `hvacPower` remains WS-live-only (its derived `isClimateOn` is persisted). See `vehicle-state-schema.md` §1.1 and `data-classification.md` §1.13.

> **MYR-303 — the media now-playing block (NFR-3.5).** The snapshot returns eight further media fields: `mediaNowPlayingTitle`, `mediaNowPlayingArtist`, `mediaNowPlayingAlbum`, `mediaNowPlayingStation`, `mediaPlaybackSource` (all `string` or `null`), `mediaNowPlayingDurationMs` and `mediaNowPlayingElapsedMs` (`integer` ms, or `null`), and `mediaVolumeMax` (`number` or `null`). All eight stream live on `vehicle_update` (Tesla's Media group, on change) AND are persisted to the same `go_vehicle_control_state` side table (migration **0015**), so a car that is asleep, offline or in service surfaces its last-known now-playing block instead of an empty panel. The key is ALWAYS present on both role projections; a never-observed field is an explicit `null`.
>
> **Empty string is NOT null — the one place this block diverges from its siblings.** For the five free-text fields an empty string means "observed, and nothing is playing" (the track ended), while `null` means "never observed". The server persists an empty value as `''` and lets it OVERWRITE a known title, deliberately unlike the MYR-298 `mediaPlaybackStatus`, whose `"Unknown"`/empty is dropped. The reasoning is that `mediaPlaybackStatus` is an ENUM whose `Unknown` member means "we could not read this", whereas an empty title is a real report about the world — and dropping it would pin a finished track forever, leaving the app advertising a song that stopped playing an hour ago. **Consumers MUST render `""` and `null` differently** and MUST NOT coalesce them.
>
> **Classification.** The five text fields are **P1**, deliberately stricter than the P0 `mediaPlaybackStatus`/`mediaVolume` they sit beside: they are free-text user content, and an accumulated title/artist/album/station/source stream reveals listening habits. Redact in logs (presence/length only); never emit outside the vehicle's party; never retain as a listening history. Both roles receive them — see §5.2.1 for why, and for the `limited_viewer` exclusion. The three numerics are P0. **Not written by the MYR-260 `/vehicle_data` backfill** (Tesla's cached payload carries no now-playing block), so they are clear of the [MYR-300](https://linear.app/myrobotaxi/issue/MYR-300) stale-overwrite issue. Note `mediaNowPlayingDurationMs` may carry Tesla's **18000000** (5h) radio sentinel verbatim — the server stores it as-is so clients can distinguish "radio" from "never observed"; rendering it as "no duration" is the client's job. See `schemas/vehicle-state.schema.json` and `data-classification.md` §1.13.

> **MYR-308 — `seatCoolingCapable` (REST-sourced, snapshot-only).** The snapshot returns `seatCoolingCapable` (`boolean` or `null`): whether the car is EQUIPPED WITH ventilated front seats. This is a SPEC fact, not a runtime state — contrast the sibling `seatVentEnabled` (proto 254), which is the on/off of that equipment; a car can be `seatCoolingCapable: true` with `seatVentEnabled: false`. Sourced ONLY from Tesla REST `vehicle_data.vehicle_config.has_seat_cooling`, read on the existing ServiceStatusMonitor connectivity-edge path (no `endpoints=` parameter is needed — Tesla's default `/vehicle_data` response already carries `vehicle_config`, which is how MYR-279's `trim_badging` arrives) and persisted to `go_vehicle_control_state` (migration 0015). **It carries NO live WebSocket delta: a `vehicle_update` frame NEVER contains it.**
>
> Because Tesla has no proto for it, it has no `fieldMap` entry — and that is exactly what carries it past the MYR-300 stream-recency gate, which drops only fields derived from `fieldMap`. A busily-streaming car still acquires it, and an **in-service car, which never streams at all, acquires it on the ordinary connectivity-edge read** — the case MYR-308 exists for. Same REST-only path as `trim` (MYR-279).
>
> **`null` does NOT mean "no seat cooling".** It means the server predates MYR-308 or has never completed a vehicle-config read for this car; clients MUST fall back to the existing telemetry-presence heuristic (treat the car as capable if `seatCoolerLeft`/`seatCoolerRight` have ever been non-null) rather than hiding the control. An explicit `false` is AUTHORITATIVE and outranks that heuristic: clients MUST NOT offer seat-cooling controls at all — no greyed-out row, no disabled slider implying the hardware exists. P0 both roles (an equipment fact, tiered like `trim`). See `data-classification.md` §1.13.

> **MYR-316 — `serviceEstimatedEndAt`, the service window (REST-derived, snapshot + list only).** The snapshot returns `serviceEstimatedEndAt` (`string` RFC 3339 or `null`): when the car's CURRENT service visit is estimated to end. It answers the one question an "In Service" badge cannot — *when do I get my car back?* — and it is what floors the rider's scheduling picker, so it is P0 operational timing about the car, the same tier as the sibling `status`, and in BOTH role masks (a rider needs the floor for exactly the reason the owner does).
>
> **Two columns, one emitted value — and the two-column split is the design, not an accident.** Migration **0017** adds `service_etc` (Tesla's own estimate, read from the Fleet API `GET /api/1/vehicles/{vin}/service_data`) and `service_expected_end_at` (what the owner typed via the new §7.16) to the Go-owned `go_vehicle_control_state` side table, both nullable `TIMESTAMPTZ`. The wire value is `COALESCE(service_etc, service_expected_end_at)` — **Tesla wins, the owner is the fallback** — resolved by one shared helper (`internal/telemetry/service_window.go`) used by this endpoint, §7.0, and the §7.8 scheduler bound, so the three readers can never drift. Collapsing them into one column would have been cheaper and wrong: a Tesla estimate arriving late would ERASE what the owner typed, and a withdrawn Tesla estimate would fall back to `null` instead of back to the owner's answer. Both columns also sit OUTSIDE the shared COALESCE control-state upsert the rest of this table uses — that upsert cannot express a NULL write, and clearing is a first-class operation here — so they have dedicated writers in `internal/store/vehicle_service_window.go`.
>
> **Emitted ONLY while `status` is `in_service`; `null` otherwise.** The monitor additionally CLEARS both columns when it observes the car leaving service, so the in-service gate is belt-and-braces: a stale window can neither outlive the visit nor be resurrected by a status flip. Consumers therefore never age this field out themselves.
>
> **`null` is COMMON AND NORMAL.** Tesla returns an ALL-NULL `service_data` body for a visit with no appointment record, so a car can be genuinely in service with no estimate at all. `null` is not an error, not a fetch failure, and not a claim that the car is back — and it means **NO BOUND**: scheduling stays fully open, and neither the picker nor the server may block on missing data (§7.8). The read itself is non-fatal on error and leaves the last-known estimate in place.
>
> **Delivery: NOT STREAMED.** Tesla has no proto for a service ETC, so there is no `fieldMap` entry, **no fleet-config change**, and a `vehicle_update` frame NEVER carries `serviceEstimatedEndAt`. It is REST-derived and reaches clients on the next read of this endpoint or §7.0. The Tesla read piggybacks on the ServiceStatusMonitor's existing connectivity-edge path for an in-service car, sharing the SAME per-VIN 45 s read debounce as the `GET /api/1/vehicles/{vin}` edge read and the MYR-260 `/vehicle_data` backfill and reusing the token that read already resolved — one more read on a path that was already firing, not a new poller. See `vehicle-state-schema.md` §2.4 and `data-classification.md` §1.13.

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
  "licensePlate": "ABC 1234",
  "serviceEstimatedEndAt": null,
  "rideShareEnabled": true,
  "setupState": null,
  "lastUpdated": "2026-04-13T18:22:01Z"
}
```

This car is set up and streaming, so `setupState` is `null`. A car that is not would carry, for example, `{"state": "awaiting_virtual_key", "since": "2026-08-09T02:10:00Z"}`.

The seven former spec-only catalog fields (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`, `destinationAddress`) were promoted out of spec-only status by [MYR-24](https://linear.app/myrobotaxi/issue/MYR-24) on 2026-04-23 — the Go `internal/store.Vehicle` read path now loads them from the Prisma-owned `Vehicle` table. Six are non-nullable on snapshot (`model`, `year`, `color`, `fsdMilesSinceReset`, `locationName`, `locationAddress`); `destinationAddress` remains nullable because the Prisma column is `String?`. The charge-group fields `chargeState` and `timeToFull` are persisted to the Prisma-owned `Vehicle` table as of [MYR-41](https://linear.app/myrobotaxi/issue/MYR-41) on 2026-04-25; both columns are nullable (`String?` / `Float?`) so a vehicle that has never charged surfaces `null` on the snapshot. [MYR-40](https://linear.app/myrobotaxi/issue/MYR-40) shipped the live WS wire path for both fields on 2026-04-22; the cold-load snapshot now reads back the same values. See `websocket-protocol.md` §4.1.4 and §10 DV-03 / DV-04.

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `licensePlate` | `string` | **P1** | **OPTIONAL** ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286), contracts v0.15.0). The owner-entered license plate, read off the Prisma-owned `Vehicle.licensePlate` column as an identity-row field alongside `name`/`color` — **not telemetry**. **Not sourced from Tesla:** the Fleet API exposes no plate on any endpoint, telemetry field, or proto, so the value exists only because the owner typed it via §7.14 `PUT /api/tesla/vehicles/{vehicleId}/plate`. **Already normalized** on write (trimmed, uppercased, ≤ 10 chars, charset `[A-Z0-9 -]`) — consumers MUST NOT re-normalize or re-validate before display. **Empty-value convention:** this server ALWAYS emits the key and uses an **empty string** for "no plate set", exactly matching its sibling `color` (a plain non-`omitempty` string). The wire contract additionally tolerates an ABSENT key, which means the server predates MYR-286; neither ever means "this car has a plate we could not read". Consumers keep their existing `VIN ····xxxx` fallback for both cases and MUST NEVER render an empty plate. **RBAC:** visible to BOTH roles (§5.2.1) — unlike the owner-only `vin`. **Delivery:** no WebSocket delta in v1 — a `vehicle_update` frame NEVER carries `licensePlate` and a plate edit fires no push, so the value reaches clients on the next read (this endpoint or §7.0). |
| `serviceEstimatedEndAt` | `string` (RFC 3339, UTC) or `null` | P0 | **OPTIONAL** ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316), contracts v0.17.0). When the car's CURRENT service visit is estimated to end — the answer an "In Service" badge alone cannot give, and the FLOOR the rider's scheduling picker builds on. **Server-computed, never client-supplied on this shape.** Resolved as `COALESCE(service_etc, service_expected_end_at)` over two nullable `TIMESTAMPTZ` columns on the Go-owned `go_vehicle_control_state` side table (migration **0017**): Tesla's own estimate (`service_data.service_etc`, Fleet API `GET /api/1/vehicles/{vin}/service_data`) OUTRANKS the owner-entered "expected back" value written by §7.16, which is the fallback — kept as two columns precisely so a late Tesla estimate does not erase what the owner typed and a withdrawn one falls back rather than to `null`. **Gated on status:** emitted only while `status` is `in_service`; anything else is `null`, and the monitor physically clears both columns when the car leaves service, so the gate is belt-and-braces. **`null` is common and normal** — Tesla returns an all-null `service_data` body for a visit with no appointment record — and means **NO BOUND**, not "unknown, refuse to schedule" (§7.8). **RBAC:** BOTH roles (§5.2.1); P0 operational timing, the same tier as `status`, so it is log-safe. **Delivery:** NOT STREAMED — no proto, no `fieldMap` entry, no fleet-config change, and a `vehicle_update` NEVER carries it; it reaches clients on the next read (this endpoint or §7.0). |
| `rideShareEnabled` | `boolean` | P0 | **OPTIONAL** ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342), contracts v0.20.0). Whether this vehicle's OWNER currently accepts ride requests against it — the same value and semantics as `VehicleSummary.rideShareEnabled` (§7.0). **Carried on the snapshot as well as the catalog row on purpose:** the owner's ride-sharing toggle lives on the vehicle detail surface, and a control whose current position can only be learned from a *different* endpoint renders wrong on a cold open. (Contrast `hasActiveRide`, which is list-only — nothing on the detail sheet reads it.) **Server-stored, never client-supplied on this shape:** `ride_share_enabled BOOLEAN NOT NULL DEFAULT true` on the Go-owned `go_vehicle_control_state` side table (migration **0021**), read as `COALESCE(gcs.ride_share_enabled, TRUE)` so a car with no side-table row reads `true`. Written ONLY by §7.18, and only by the owner — it is deliberately absent from `ControlStateUpdate`, so no telemetry frame can reach it. **`false` means PAUSED**, and the server then refuses ride-request creates and accepts for this car with `409 vehicle_unavailable` (§7.8), for SCHEDULED rides as well as instant ones. **ABSENT means ENABLED** (a server predating MYR-342) — never paused. **RBAC:** BOTH roles (§5.2.1); P0 operational availability, the same tier as `status`, so it is log-safe. **Delivery:** NOT STREAMED — no proto, no `fieldMap` entry, no fleet-config change, and a `vehicle_update` NEVER carries it. |
| `setupState` | `object` or `null` | P0 | **OPTIONAL** ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491), contracts v0.24.0). **What is still standing between this car and live telemetry, or `null` when there is nothing to say.** Members: `state` — one of `awaiting_virtual_key`, `configuring`, `token_failed` — and `since`, the RFC 3339 UTC instant the evidence behind it was recorded. <br><br>**WHY IT EXISTS.** [MYR-489](https://linear.app/myrobotaxi/issue/MYR-489) taught the server to heal a car that was linked but never streamed and deliberately left the wire untouched; the next night [MYR-503](https://linear.app/myrobotaxi/issue/MYR-503) showed the cost — a new owner opened the app, saw a card with no location and no charge, tapped Lock, and got a dead button, because nothing said his virtual key was never paired. The client's direction was explicit: pairing guidance must NOT hang off a failed control (*"does this make sense to the end user to have retry associated with lock?"* — no). This field is the vehicle-level answer, rendered BEFORE any command is attempted, naming the ONE thing left to do. <br><br>**THE MEMBERS AND THE ACTION EACH IMPLIES.** `awaiting_virtual_key` — Tesla refused to apply the telemetry config because the owner's virtual key is not enrolled on the car (Tesla's own `missing_key` / `not_paired` skip). THE OWNER MUST ACT: pair the key in the Tesla app; every signed command is rejected until they do, and nothing the server does heals it. `configuring` — the configuration side is in hand and the server is waiting for the car to open its telemetry session, including after a MYR-489 forced re-push. NO OWNER ACTION IS REQUIRED; copy should say so, and may suggest waking the car's screen once it persists. Note this member does NOT by itself assert that pairing was detected (it is also reached when the server has just issued the car a brand-new config), so copy asserting *"pairing detected"* overstates it. `token_failed` — the owner's Tesla authorization can no longer be resolved or refreshed, so neither config pushes nor commands can reach the car. THE OWNER MUST ACT: reconnect the Tesla account. **Consumers MUST treat an unrecognised `state` as "setup is incomplete, reason unknown" and MUST NOT fail the decode** — new members may be added in a minor version. <br><br>**SOURCE AND DERIVATION.** The Go-owned `go_fleet_config_attempts` row (`last_outcome`, `last_attempt_at`, `signed_command_at`, `forced_repush_at`; migrations **0031** + **0036**), LEFT JOINed onto this read and evaluated together with the row's own `status` and `lastUpdated`. **DERIVED SERVER-SIDE, EXACTLY ONCE**, in one function that BOTH this endpoint and the §7.0 catalog row call — consumers MUST NOT re-derive it, and are guaranteed the two surfaces cannot disagree for one vehicle. The link-time config push seeds `awaiting_virtual_key` the moment Tesla answers `missing_key`, so a brand-new owner's card is populated within seconds of onboarding rather than after the reconciler's first pass. <br><br>**HONESTY GUARANTEES** — worth knowing because they bound what the copy may claim. (1) **Absence of evidence yields `null`:** a vehicle with no schedule row never carries a state, and the table is self-draining, so a successful config push removes the row and with it the card. Presence alone does not license a card either ([MYR-517](https://linear.app/myrobotaxi/issue/MYR-517)): a row may be a SEED written at link time or by an applied signed command, whose empty `last_outcome` yields `null` exactly as no row does. (2) **`configuring` EXPIRES** 24 hours after its evidence, falling back to `null` — never to `awaiting_virtual_key`, because a car whose pairing has been proven is not awaiting a key and telling its owner to re-pair would send them to fix something that is not broken. Nothing here says "connecting…" forever. (3) **A car that is currently STREAMING never carries `configuring` or `token_failed`**, so a stale schedule row cannot put a setup card on a live vehicle. (4) **A car that streamed once and later went quiet is the FRESHNESS story** (`lastUpdated`), not a setup state, and is deliberately excluded — `setupState` is about cars that have never come up, not cars that have gone away. <br><br>**NOT derived from `key_not_paired` command rejections**, which is a deliberate exclusion rather than an oversight: that error code is overloaded — the server returns the very same code when ITS OWN signing proxy is unconfigured — so it cannot distinguish an unpaired owner from a broken deployment, and a card built on it would tell the whole fleet to re-pair whenever our proxy config broke. Tesla's `missing_key` on the config push carries no such ambiguity. <br><br>**`null` is NOT `ready`:** there is deliberately no `ready` member. `null` means the server makes no claim, which also covers a car nobody has examined and an expired claim, so a consumer MUST NOT render a positive "set up ✓" state from it. **ABSENT** means a server predating MYR-491 and MUST be read exactly like `null`. **RBAC:** BOTH roles (§5.2.1) — P0 operational state about the car, the same tier as the sibling `status` it exists to qualify; no VIN, no coordinate, no credential, no user content, so it is log-safe in full. **Delivery:** NOT STREAMED — no proto, no `fieldMap` entry, and a `vehicle_update` NEVER carries it; by construction a vehicle in any of these states is not sending frames to attach it to. |
| `trimLabel` | `string` or `null` | P0 | **OPTIONAL** ([MYR-320](https://linear.app/myrobotaxi/issue/MYR-320), contracts v0.18.0). The HUMAN-READABLE trim / performance designation (e.g. `"Performance"`), **display-ready as delivered**. Sourced ONLY from REST `vehicle_data.vehicle_config.performance_package` (live-verified against the owner's own car), persisted to `go_vehicle_control_state.trim_label` (migration **0018**, nullable `TEXT`) and LEFT-joined in by `VehicleRepo.GetByID`. **Contrast the sibling `trim`**, which stays the RAW badge code from `vehicle_config.trim_badging` (e.g. `p74d`) and is NOT display-safe: both are kept, neither replaces the other, and `trimLabel` is the ONLY one of the two a consumer may render. **Consumer rule:** compose the display model name as `<year> <model> <trimLabel>` (e.g. "2026 Model Y Performance") and OMIT THE LABEL ENTIRELY — falling back to `<year> <model>` — when the value is `null` or the key is ABSENT; never substitute `trim`, never re-case or reformat it, never emit a dangling separator. **`null` is common and normal:** it means the server predates MYR-320, no vehicle-config read has completed for this car, or the car's configuration carries no performance designation — never an error. **RBAC:** BOTH roles (§5.2.1) — the same treatment as `trim` and `softwareVersion`. **Delivery:** NOT STREAMED and **snapshot-only** — no proto, no `fieldMap` entry (which is also what carries it past the MYR-300 stream-recency gate), a `vehicle_update` NEVER carries it, and it is deliberately NOT on the §7.0 list row: this is a details-sheet field. |
| `fsdVersion` | `string` or `null` | P0 | **OPTIONAL** ([MYR-320](https://linear.app/myrobotaxi/issue/MYR-320), contracts v0.18.0). The car's CURRENT FSD software DESIGNATION exactly as Tesla names it (e.g. `"FSD (Supervised) v14.3.5"`). Sourced ONLY from the **TITLE of the NEWEST entry** returned by the Fleet API `GET /api/1/vehicles/{vin}/release_notes` — **no `vehicle_data` field and no proto carries this**, so the release-notes call is the only source Tesla exposes; persisted to `go_vehicle_control_state.fsd_version` (migration **0018**, nullable `TEXT`) and LEFT-joined in by `VehicleRepo.GetByID`. **Distinct from `softwareVersion`**, which is the INSTALLED FIRMWARE BUILD (e.g. `2026.20.1 9a8b7c6`) — the two strings move independently and NEITHER CAN BE DERIVED FROM THE OTHER. **Free-form, passed through VERBATIM:** the shape is Tesla's and may change (parenthetical qualifiers, the `v` prefix, point-release depth), so the server never parses or normalizes it and consumers MUST NOT parse it, compare it ordinally, or gate a feature on a version number extracted from it. **Consumer rule:** render it as its own row in the details sheet and OMIT THE ROW ENTIRELY when the value is `null` or the key is ABSENT — no "Unknown", no empty row, no placeholder dash. **`null` does NOT mean the car lacks FSD:** it means the server predates MYR-320 or no release-notes read has completed. **RBAC:** BOTH roles (§5.2.1), matching `softwareVersion`. **Delivery:** NOT STREAMED and **snapshot-only** — a `vehicle_update` NEVER carries it, and it is deliberately NOT on the §7.0 list row. |

#### Response -- error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid token |
| 403 | `vehicle_not_owned` | Caller is not the owner or an accepted viewer of `vehicleId` |
| 404 | `not_found` | `vehicleId` does not exist (or is not visible to the caller -- intentionally indistinguishable) |
| 429 | `rate_limited` | REST rate limit breached (§4.1.2) |
| 500 | `internal_error` | Store-layer error, decryption failure, etc. |

#### RBAC

See §5.2.1. Owners see the full `VehicleState`; viewers see everything except the full `vin` (MYR-279). `licensePlate` is visible to BOTH roles (MYR-286) — a rider identifies the car at pickup from it. `serviceEstimatedEndAt` is visible to BOTH roles too (MYR-316) — it floors the rider's scheduling picker, so a viewer who cannot see it cannot book against the car correctly. So is `rideShareEnabled` (MYR-342): it tells a rider whether the car is taking requests at all. So is `setupState` (MYR-491): it is the only thing that distinguishes a shared car that is broken from one that is merely not finished, and `status` alone cannot — a never-streamed car reads `offline` forever (MYR-437).

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

See §5.2.2. **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)** — there is no viewer projection of this endpoint any more, because no grant of any shape opens it, including a legacy one still carrying the retired `live_history` preset (§7.5.0). The handler gates on `vehicleAccessForOwnerOnly`, which consults no grant at all; a viewer gets `403 vehicle_not_owned`.

#### FR-9.1 / FR-9.2 pairing

The SDK's drive-history UI is hydrated by:

1. An initial `GET /api/vehicles/{vehicleId}/drives` call (REST, this endpoint) to populate the first page.
2. Subsequent paginated scrolls that call this endpoint again with `nextCursor`.
3. A WebSocket subscription to `drive_ended` messages that the SDK prepends to the in-memory list as new drives complete (see [`websocket-protocol.md`](websocket-protocol.md) §4.3).

The SDK MUST NOT re-fetch the list when a `drive_ended` frame arrives -- the frame carries enough data to synthesize a `DriveSummary` for prepending, and the SDK can later call `GET /api/drives/{driveId}` on tap-through to fetch the full record lazily. This is the FR-9.1 / FR-9.2 contract: snapshot + reactive subscription, no redundant fetches.

#### Retention

Drives older than 365 days are pruned by the background retention job per NFR-3.27 (see [`data-lifecycle.md`](data-lifecycle.md) §5). **Implemented and running daily since [MYR-439](https://linear.app/myrobotaxi/issue/MYR-439) (2026-08-02);** before that the policy existed with nothing enforcing it. No drive has yet been pruned and none will be until **2027-03-08** — the oldest production drive dates to 2026-03-08, so the platform is still younger than its own retention window.

A cursor scan that straddles a prune event MAY observe items disappearing -- this is acceptable, and it cannot corrupt the scan: the keyset cursor is a pure value comparison that never dereferences its anchor row, so a cursor naming a since-pruned drive still returns the correct next page.

> Earlier revisions of this note said items disappear "from the tail". That is slightly stronger than the schema guarantees: the prune orders by `createdAt` while this list orders by `startTime` (a `String`, compared lexicographically), so for rows with clock skew the two orders can diverge and an item may vanish from the middle of a scan. Either way the client sees a gap, never an error.

A pruned drive simply stops appearing here. In-window drives are entirely unaffected -- the pruner touches nothing inside the window.

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
| 404 | `not_found` | `driveId` does not exist (or is not visible) -- **including a drive pruned by the 365-day retention job** |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error, decryption failure |

#### Retention

This endpoint is the tap-through target for a `drive_ended` frame the SDK cached earlier, so a client can hold a drive id for arbitrarily long. Once that drive passes 365 days it is deleted (NFR-3.27, [`data-lifecycle.md`](data-lifecycle.md) §5 and §5.9) and this endpoint returns `404 not_found` -- the ordinary not-found path, not a distinct error. Note the 404 is evaluated **before** the ownership check, so a pruned drive 404s rather than 403s. Clients MUST treat 404 here as "this drive is gone", not as a transient failure to retry.

#### RBAC

See §5.2.3. **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369).** The previous rule — that viewers saw the same field set including start/end location and address, on the strength of the consent carried by the invite they accepted — retired with the `live_history` tier itself (§7.5.0); there is no longer a grant that conveys trip history for that consent to attach to. Same `vehicleAccessForOwnerOnly` gate as §7.2; a viewer gets `403 vehicle_not_owned` whatever their grant says.

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
| 404 | `not_found` | `driveId` does not exist (or is not visible) -- **including a drive pruned by the 365-day retention job** |
| 429 | `rate_limited` | REST rate limit breached |
| 500 | `internal_error` | Store-layer error, decryption failure |

#### Retention

The GPS trail this endpoint serves is the single most sensitive artifact the platform stores, and it is deleted with its parent drive at 365 days (NFR-3.27, [`data-lifecycle.md`](data-lifecycle.md) §5). There is no separate route-blob table and no independent route retention policy: `routePoints` and its ciphertext shadow are columns on the drive row, so they die exactly when it does. Requests for a pruned drive's route return `404 not_found` on the same path as any unknown id.

#### RBAC

See §5.2.4. **Owner-only, unconditionally, as of [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369).** Denying viewers no longer defeats FR-5.1: the sharing use case is served by the live surfaces a viewer keeps in full — the §7.0 catalog row, the §7.1 snapshot, the WebSocket stream, and rides where the grant carries `allowRides` — not by replaying a route already driven (§7.5.0). Same `vehicleAccessForOwnerOnly` gate as §7.2; a viewer gets `403 vehicle_not_owned`.

---

### 7.5 Vehicle sharing (invites + redeem)

> **Anchored:** FR-5.1, FR-5.2, FR-5.3, FR-5.4.
> **Schema:** [`schemas/vehicle-sharing.schema.json`](schemas/vehicle-sharing.schema.json) — `ShareInvite`, `SharePermission`, `CreateShareInviteRequest`, `RedeemShareInviteRequest`, `RedeemShareInviteResponse`, `PatchShareInviteRequest`, `ShareInviteListResponse` (contracts v0.23.0 — `ShareInvite.shareUrl` added by MYR-368 §7.5.6; `ShareInvite.allowRides` / `ShareInvite.suspended` and `PatchShareInviteRequest` added by MYR-369 §7.5.7).
> **Persisted:** Go-owned `go_vehicle_shares` table (migration 0020 — [`data-classification.md`](data-classification.md) §1.15). No foreign keys to the sibling schema (CG-DL-9).

Mounted on the **Go telemetry server** as of **MYR-184** (2026-07-29). This **SUPERSEDES §10 DV-23**, which had assigned invites to the now-deprecated Next.js app: `react-frontend` is retired, the email-keyed Prisma `Invite` table is retired **unused**, and sharing is Go-owned end to end. See the DV-23 row in §10 for the full supersession note.

**Codes, not emails.** MyRoboTaxi has no email infrastructure and riders are Apple-native, so the owner sends a server-minted **6-character code** out-of-band through the iOS system share sheet. Nothing in this family accepts, stores, or resolves an email address. The pre-MYR-184 shape documented here — `email`, `senderId`, `sentDate`, `isOnline` — never shipped and is gone.

**Links, not dictation (MYR-368).** Every pending row also carries `shareUrl`, a complete **signed** join URL that embeds the code. It is what the share sheet actually sends; the bare `code` remains on the wire as the fallback and as what the redeem endpoint consumes. The signature is verified **statically by the web join shell**, against a public key compiled into it — no database, no round trip, so a forged or tampered link is bounced before it can turn the join page into a code oracle. See **§7.5.6** for the full format, the canonical signed payload, key management, and rotation.

**Six endpoints, two audiences.** Five are OWNER-facing and speak in `ShareInvite` — the revoke being the one that answers with no body at all; one is RIDER-facing and returns `RedeemShareInviteResponse`. `ShareInvite` is **never** delivered to an invited party: it carries the owner-typed `label`, the owner's per-grant controls (§7.5.7), and, while pending, the live `code`.

| Endpoint | Audience | Returns |
|----------|----------|---------|
| `POST /api/vehicles/{vehicleId}/invites` | owner | `ShareInvite` (201) |
| `GET /api/vehicles/{vehicleId}/invites` | owner | `ShareInviteListResponse` (200) |
| `DELETE /api/invites/{inviteId}` | owner | no body (204) |
| `PATCH /api/invites/{inviteId}` | owner | `ShareInvite` (200) — the updated **accepted** grant |
| `POST /api/invites/{inviteId}/resend` | owner | `ShareInvite` (200) |
| `POST /api/invites/redeem` | any authenticated caller | `RedeemShareInviteResponse` (200) |

#### 7.5.0 Grant capabilities (MYR-369 — the tier is retired)

**The cumulative tier is gone.** Until MYR-369 a grant carried one `SharePermission` on a total order (`live` < `live_history` < `rides`), every gate compared with a `>=` over it, and the value was **fixed for the life of the grant** — changing somebody's access meant revoking and re-inviting them. An accepted grant now carries **independent, owner-editable flags** on `go_vehicle_shares` (migration 0024), edited in place through **§7.5.7 `PATCH /api/invites/{inviteId}`**:

| Column | Wire field | What it does | Server gate |
|--------|-----------|--------------|-------------|
| — (the grant existing and being live) | — | The vehicle appears in the viewer's §7.0 list; §7.1 snapshot readable under the viewer mask; WS vehicle subscription allowed. | `internal/telemetry/vehicle_share_access.go` `vehicleAccessFor(..., capBase)` |
| `allow_rides` | `ShareInvite.allowRides` | Creating a §7.8 ride request as a rider who is **not** the vehicle's owner. | `vehicleAccessFor(..., capRides)` — create, owner accept, and reservation dispatch |
| `suspended_at` | `ShareInvite.suspended` | **Gates everything** — see below. | The access-set predicate, `suspended_at IS NULL` |

##### The suspension invariant

**A suspended grant is excluded from the viewer-merge access set.** Not hidden in the catalog, not refused at one gate — *excluded from the set*, which is the single thing the §7.0 vehicle list, the §7.1 snapshot, the WebSocket handshake and the §7.8 rides surfaces all resolve through. (§7.2–§7.4 do not appear on that list because they no longer consult a grant at all — they are owner-only, below.) One predicate, `suspended_at IS NULL`, kills all of them together, and **no capability flag can out-vote it**: `allowRides: true` on a suspended grant allows nothing, because by the time any capability is read the grant has already failed to appear.

**But it is ONE predicate written SIX TIMES.** It is one rule, and it reads like one place; it is not one place. The honest inventory, so that the seventh statement is written by somebody who knows there are already six — **six live occurrences across five files in two packages**:

| # | Site | What it gates |
|---|------|---------------|
| 1 | [`internal/auth/queries.go`](../../internal/auth/queries.go) — `queryUserVehicleIDs` | The **merged access set** (vehicles owned UNION vehicles shared with you). The WebSocket subscribed set and its cache, `GET /api/vehicles`, and every per-vehicle "can this caller see this car" resolve through this one. |
| 2 | [`internal/auth/vehicle_access.go`](../../internal/auth/vehicle_access.go) — `queryShareGrant` | Per-vehicle capability resolution behind `ResolveVehicleAccess`. |
| 3 | [`internal/store/vehicle_share_access_queries.go`](../../internal/store/vehicle_share_access_queries.go) — `queryAcceptedShareGrant` | The REST snapshot gate (`ShareGrantFor`). |
| 4 | *same file* — `queryRiderMayRequestRides` | The reservation-dispatch probe used by the sweeper. |
| 5 | [`internal/store/vehicle_repo_list_shared.go`](../../internal/store/vehicle_repo_list_shared.go) — `sharedSummaryJoin` | The viewer catalog. The predicate sits in the **JOIN**, not a `WHERE`, because the join IS the access check. |
| 6 | [`internal/store/vehicle_share_queries.go`](../../internal/store/vehicle_share_queries.go) — `queryAcceptedSharesByCodeAndUser` | The idempotent re-redeem lookup — which is what makes re-redeeming a suspended grant's code answer 404. |

**The repetition is structurally forced, not sloppy.** The `auth` / `store` halves cannot be collapsed into one statement: `internal/auth` may not import `internal/store`, because the dependency rule runs the other way — `internal/auth` is a dependency of `internal/ws`, never the reverse — so the two packages necessarily carry their own copies of the access-set predicate. Within `internal/store` the repetition is a different thing again: each statement serves a different surface with a different **shape** — a join, an `EXISTS` probe, a code lookup, a flags lookup — and there is no single statement all four could share without becoming a worse version of each.

**The honest consequence.** This is an invariant maintained by **repetition**, which means it is enforced by convention plus tests, not by construction. A seventh statement that reads `go_vehicle_shares` for access and omits the term is a suspended viewer keeping access, and nothing in the type system will say so — which is precisely why the inventory is written down here and why each of the six sites carries the reason in a comment beside it.

The consequences a consumer must know:

- A suspended grant produces **no `VehicleSummary` row at all** — the vehicle is absent, not present with a reduced `sharePermission`. There is deliberately no "suspended" marker for a viewer to render.
- Re-redeeming the code of a suspended grant answers **404**, indistinguishably from an unknown or expired code (§7.5.5). The server does not announce a suspension to the person it was applied to.
- The grant **still serializes on the owner's own §7.5.2 listing** — that is the whole point; the owner has to see it to lift it.
- Suspension is the **reversible** alternative to §7.5.3 revoke. The row and its flags survive, and one PATCH restores exactly what it held; a revoke is a permanent tombstone the viewer can only escape through a fresh invite.
- Because suspension works by removing the grant from the access set, it is enforced at the **WebSocket HANDSHAKE** — which on its own would leave a viewer holding a **live, open** socket receiving broadcasts until they reconnected. **[MYR-373](https://linear.app/myrobotaxi/issue/MYR-373) closed that gap (2026-08-02): a suspending PATCH now also tears the socket down.** After committing and after busting the grantee's cache, the handler publishes `share.access_revoked`; the WS hub cuts that grantee's sessions for that vehicle off synchronously and closes them with `4002` / `vehicle access revoked` (see [`websocket-protocol.md`](websocket-protocol.md) §4.5.1). Sub-millisecond on the instance that served the PATCH; a 60s revalidation sweep backstops it. **Scoped to the suspended grantee** — the owner's own live map and every other viewer's are untouched. An owner whose mental model is "location stops now" is now right about every surface, including an already-open socket. The residual is the same per-process-cache caveat §7.5.3(a) records: on a multi-machine deployment an instance that did not serve the PATCH catches up within its 5-minute cache TTL plus one sweep. `allowRides` is unaffected either way — it has no WebSocket effect, so patching it closes nothing.

##### `live_history` is retired

The **"Live + history"** capability is **removed from the product**. §7.2 drives, §7.3 drive detail and §7.4 drive route are **owner-only again, unconditionally** — no grant of any shape opens them, including a legacy grant created at the `live_history` preset.

The enum member is **kept, not removed**. Dropping it would break every installed client whose decoder still lists it and would make contracts 0.23.0 a major bump. It is therefore **decodable and never emitted**:

| Where | Behaviour |
|-------|-----------|
| §7.5.1 create request | **Still accepted** so an un-updated send-invite sheet keeps working — and **persisted as `live`**. The 201 response carries `permission: "live"`, so a client MUST read the created row rather than assume its input round-trips. |
| Any response `permission` | Never emitted. A pending row's stored preset is normalized on the way out; an accepted row's `permission` is derived (below). |
| `VehicleSummary.sharePermission` | Never emitted. A legacy `live_history` grant derives `live`, which is exactly what it now conveys. |

##### `permission` is now derived on accepted rows

`ShareInvite.permission` has two separate lives, and consumers must not conflate them:

- **Pending row** — the invite-time **preset** the owner chose. Redemption maps it onto the new grant's flags (`rides` → `allowRides: true`, everything else → `false`). That mapping is the only thing the preset ever does; **pending invites keep tier-at-redeem**, so an invite minted before MYR-369 redeems to exactly the capabilities its preset always implied.
- **Accepted row** — **derived from the flags on every read** (`allowRides` → `"rides"`, otherwise `"live"`). Never a stored tier, never an input to a decision. It is the compatibility projection a pre-MYR-369 client reads, and it changes when the owner PATCHes the grant — which is how such a client learns the access moved.

The same derivation produces `VehicleSummary.sharePermission` (§5.2.0, §7.0). A consumer that understands `allowRides` MUST prefer it. The old instruction to compare with a cumulative `>=` is **obsolete**: the model is a set of independent flags, not a total order — though a `>=` over the two values actually emitted happens to give the same answer, which is what keeps an un-updated client correct.

**Sharing never grants writes.** Commands (§7.9), plate (§7.14), refresh (§7.15), service window (§7.16), and teardown (§7.12) stay owner-only under every capability, as does this entire §7.5 family — a viewer cannot re-share a car that is not theirs, nor read the owner's private labels for other people, nor read the controls their own access is governed by.

#### 7.5.1 `POST /api/vehicles/{vehicleId}/invites`

##### Purpose

Mints ONE code and creates one `go_vehicle_shares` row per vehicle in the requested set. Owner-only.

##### Request

```
POST /api/vehicles/{vehicleId}/invites HTTP/1.1
Authorization: Bearer <token>
Content-Type: application/json; charset=utf-8

{
  "label": "Mira Chen",
  "permission": "live",
  "vehicleIds": ["clxyz1234567890abcdef", "clxyz1234567890abcdeg"]
}
```

| Field | Type | Required | Classification | Notes |
|-------|------|----------|----------------|-------|
| `label` | string | Yes | P1 | Owner-typed recipient name — a memo for the owner's own list, **never** resolved to an account and **not** an email. Max 120 characters. Log-redacted. |
| `permission` | string (enum) | Yes | P0 | `live` \| `live_history` \| `rides` — the invite-time **preset** redemption maps onto the grant's flags (§7.5.0). Applied identically to every row a multi-vehicle create mints; per-vehicle presets are not expressible at create time, but the capabilities they produce **are** editable per vehicle afterwards via §7.5.7. **`live_history` is accepted and persisted as `live`** (MYR-369) — read the created row's `permission` rather than assuming your input round-trips. |
| `vehicleIds` | string[] | No | P0 | Multi-vehicle invite. **MUST include the path vehicle** (400 otherwise — the path vehicle is what authorizes the call). Every id must be owned by the caller (403 otherwise). Omitting it is exactly equivalent to `[<path vehicleId>]`. Max 20. Duplicates are collapsed. |

##### Response — 201 Created

A single `ShareInvite`: **the row for the PATH vehicle**. Sibling rows are not returned; the client learns them by listing each vehicle's invites, and the `code` on the returned row is the one to hand out for all of them.

```json
{
  "inviteId": "csh0123456789abcdef0123456789abcd",
  "vehicleId": "clxyz1234567890abcdef",
  "label": "Mira Chen",
  "permission": "live",
  "status": "pending",
  "code": "RBO246",
  "shareUrl": "https://myrobotaxi.app/join/RBO246?k=1.1785942245.mHRTPwZlrUFqzQ9k1p8O_5xkzXQ9dHTh5rHhNaeJ0OQz3n0XmL4vJ8ptKQC1cO8bZ5MPKB6h0nlFmVLbUqEQAg&from=Alex&to=Mira",
  "createdAt": "2026-07-29T15:04:05Z",
  "expiresAt": "2026-08-05T15:04:05Z"
}
```

`code`, `shareUrl` and `expiresAt` are present because the row is `pending`; `acceptedAt` is omitted for the same reason. The 7-day expiry is computed by the **database** (`NOW() + INTERVAL '7 days'`), the same clock the redeem predicate reads — and it is that exact value, in unix seconds, that `shareUrl` signs (§7.5.6).

**All-or-nothing.** The whole create is one transaction and ownership of every requested vehicle is verified inside it. A set containing one car the caller does not own mints nothing at all — a partial grant would hand out a code that grants less than the owner believes.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Malformed JSON; missing/blank `label`; label over 120 chars; unknown `permission`; empty `vehicleIds`; `vehicleIds` omitting the path vehicle; over 20 vehicles |
| 401 | `auth_failed` | Missing/invalid token |
| 403 | `vehicle_not_owned` | Caller does not own the path vehicle |
| 403 | `permission_denied` | Some vehicle in `vehicleIds` is not owned by the caller |
| 404 | `not_found` | Path `vehicleId` does not exist |
| 500 | `internal_error` | Store-layer error |

##### Idempotency

`POST` is not idempotent (§4.5). A retry after a network blip mints a second code; the owner cancels either with `DELETE /api/invites/{inviteId}`.

#### 7.5.2 `GET /api/vehicles/{vehicleId}/invites`

##### Purpose

The owner's sharing screen for one vehicle: pending invites and accepted viewer grants. Owner-only.

##### Response — 200 OK

```json
{
  "invites": [
    {
      "inviteId": "csh0123456789abcdef0123456789abcd",
      "vehicleId": "clxyz1234567890abcdef",
      "label": "Mira Chen",
      "permission": "live",
      "status": "pending",
      "code": "RBO246",
      "shareUrl": "https://myrobotaxi.app/join/RBO246?k=1.1785942245.mHRTPwZlrUFqzQ9k1p8O_5xkzXQ9dHTh5rHhNaeJ0OQz3n0XmL4vJ8ptKQC1cO8bZ5MPKB6h0nlFmVLbUqEQAg&from=Alex&to=Mira",
      "createdAt": "2026-07-29T15:04:05Z",
      "expiresAt": "2026-08-05T15:04:05Z"
    },
    {
      "inviteId": "csh0123456789abcdef0123456789abce",
      "vehicleId": "clxyz1234567890abcdef",
      "label": "Roommate",
      "permission": "rides",
      "status": "accepted",
      "allowRides": true,
      "suspended": false,
      "createdAt": "2026-07-01T10:00:00Z",
      "acceptedAt": "2026-07-01T11:23:00Z"
    }
  ]
}
```

The accepted row carries the MYR-369 flags and the pending row does not: a pending invite has no grant yet, so there is nothing for the flags to describe, and a client reading `suspended: false` on an unredeemed code would reasonably conclude it is live access. Its `permission` is **derived** from `allowRides` — not the preset stored on the row.

A **suspended** grant still appears in this list, carrying `suspended: true`. That is deliberate and is the only surface on which it appears at all: the owner has to see a suspension to lift it (§7.5.0).

Three rules the response body encodes, all of them server-enforced:

1. **The envelope key is `invites`, not `items`.** This surface is deliberately **unpaginated** — an owner's per-vehicle invite set is small and bounded, there is no cursor and no `hasMore` — and the distinct key keeps an SDK pagination helper from mistaking it for a page. Always an array, never `null`.
2. **Revoked rows are NEVER serialized.** Revocation is a tombstone flip kept server-side for audit; the wire `status` enum has no `revoked` member, so a revoked row simply disappears from this list. Consumers never need to filter it.
3. **`code` appears only on `pending` rows**, and `shareUrl` and `expiresAt` with it; `acceptedAt` appears only on `accepted` rows. Order is newest first (`created_at DESC`). `shareUrl` is minted in the same branch and from the same two values as `code`, so a row that has one always has the other — an accepted row carrying a link would resurrect the credential the `code` rule just withheld.

**Expiry is not a status.** An expired invite stays `pending` with an `expiresAt` in the past and simply stops redeeming. A client that wants an "Expired" affordance derives it by comparing that value to the current time.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/invalid token |
| 403 | `vehicle_not_owned` | Caller does not own `vehicleId` — including a **viewer** of that vehicle |
| 404 | `not_found` | `vehicleId` does not exist |
| 500 | `internal_error` | Store-layer error |

#### 7.5.3 `DELETE /api/invites/{inviteId}`

##### Purpose

Cancels a pending invite or revokes an accepted grant. Owner-only.

The row is **tombstoned** (`status` → `revoked`, `revoked_at` stamped), never hard-deleted: an access grant that vanishes leaves no way to answer "who had access to this car in June".

##### Response — 204 No Content

Empty body.

**Idempotent.** Revoking an already-revoked invite that belongs to the caller is also 204, so a client retrying a dropped response never sees a spurious 404. An invite that does not exist and one that belongs to another owner both answer **404**, indistinguishably — this endpoint is not an oracle for other people's invite ids.

**Revocation takes effect immediately on this instance.** The server busts the revoked viewer's cached access set (`auth.JWTAuthenticator.InvalidateVehicles`) as part of the request, so the next REST call and the next WS handshake both see the narrowed set. **And it ends a LIVE socket, not just the next handshake** ([MYR-373](https://linear.app/myrobotaxi/issue/MYR-373), 2026-08-02): after the bust, the handler publishes `share.access_revoked` and the WS hub cuts the revoked viewer's sessions for that vehicle off synchronously, then closes them with `4002` / `vehicle access revoked` — see [`websocket-protocol.md`](websocket-protocol.md) §4.5.1. The teardown is keyed on the revoked viewer, so the owner's own stream and other viewers' streams are untouched. One caveat survives: (a) the cache and the in-process notification are both **per-process**, so on a multi-machine deployment only the machine that served the revoke clears its cache and closes its own sockets; the others lapse on the 5-minute TTL and are swept within a further 60s. The app runs a single Fly machine today. Caveat (b) — "a viewer holding a live WebSocket keeps receiving broadcasts until they reconnect" — **no longer applies**; DV-09 is closed for the access-loss direction.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/invalid token |
| 404 | `not_found` | `inviteId` does not exist, or belongs to another owner |
| 500 | `internal_error` | Store-layer error |

#### 7.5.4 `POST /api/invites/{inviteId}/resend`

##### Purpose

Mints a **new** code and resets `expiresAt` to a full 7 days from now, invalidating the previous code. Owner-only, **pending-only**.

**The re-mint covers EVERY row of the invite, atomically — not only the row named in the path.** A multi-vehicle invite (§7.5.1) is **one code backing one row per vehicle**, so the sibling set *is* the invite. Re-minting a single row would leave the previous code **live and pending** on the siblings for the remainder of its 7-day TTL, which defeats the one reason an owner presses resend (a code that leaked), and would **split the invite in two**: the new code would grant a single car and the old code would still grant the rest. The server therefore locks every pending row sharing the target row's current code (same owner) and writes the same new code plus a fresh `expiresAt` to all of them in one transaction — so after a resend the old code redeems **nothing** (`404`), and the new code grants **every** vehicle the invite covered.

##### Response — 200 OK

The updated `ShareInvite` **for the path row** — one row, the invite the caller named, even when the re-mint touched several. `inviteId` is unchanged (a client holding it keeps working) and `createdAt` is unchanged (the owner's "sent {ago}" line still refers to the original send). Sibling rows carry the same new `code` and `expiresAt`; the owner sees them on the next §7.5.2 listing of their vehicles.

**The link is RE-SIGNED, not reissued.** `shareUrl` is derived from the code, the expiry, and the two display names, all three of which a resend refreshes — so the returned URL is entirely new and the previous one stops redeeming along with the code it embeds. There is no separate "link revocation": killing the code kills the link.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/invalid token |
| 409 | `conflict` | The invite has already been **accepted**. Changing who holds an accepted grant is a revoke plus a fresh invite — silently re-opening a live grant for redemption by a different person would be a quiet transfer of access |
| 404 | `not_found` | `inviteId` does not exist, belongs to another owner, or is a revoked tombstone |
| 500 | `internal_error` | Store-layer error |

#### 7.5.5 `POST /api/invites/redeem`

##### Purpose

The rider-side join. Accepts **every** pending, unexpired row backing the submitted code, atomically, on behalf of the authenticated caller. This is the only sharing endpoint an invited party ever calls and the only response they ever see.

##### Request

```
POST /api/invites/redeem HTTP/1.1
Authorization: Bearer <token>
Content-Type: application/json; charset=utf-8

{ "code": "RBO246" }
```

| Field | Type | Required | Classification | Notes |
|-------|------|----------|----------------|-------|
| `code` | string | Yes | P1 | 6 characters, `^[A-Z0-9]{6}$`. The server normalizes exactly as the client entry field does (upper-case, strip everything outside `[A-Z0-9]`), so a code pasted with a stray space or hyphen still works. A live bearer credential — never logged, never echoed into an error body. |

The redeeming account is the JWT subject and is never client-supplied.

##### Response — 200 OK

```json
{
  "ownerFirstName": "Alex",
  "vehicles": [
    {
      "vehicleId": "clxyz1234567890abcdef",
      "name": "Alex's Model 3",
      "model": "Model 3",
      "year": 2024,
      "color": "Pearl White",
      "licensePlate": "8ABC123",
      "vinLast4": "0001",
      "status": "parked",
      "chargeLevel": 72,
      "estimatedRange": 210,
      "lastUpdated": "2026-07-29T15:04:05Z",
      "hasActiveRide": false,
      "serviceEstimatedEndAt": null,
      "rideShareEnabled": true,
      "role": "viewer",
      "sharePermission": "rides"
    }
  ]
}
```

- `ownerFirstName` — the sharing owner's **first name only**, resolved with the same ladder `RideRequest.requesterName` uses (display name → email local-part → the literal `"Owner"`). No surname, no email, no user id: a redeemer learns the minimum needed to recognize whose car they just joined. P1, log-redacted. Always non-empty.
- `vehicles` — ordinary `VehicleSummary` rows, identical in shape to §7.0, so the client can seed its catalog from this one response. Always present and **never empty**: a redemption that granted nothing answers 404 or 409 instead. Length > 1 for a multi-vehicle invite. Every row carries `role: "viewer"` and a `sharePermission`, and is the **viewer-masked** projection — the same mask §7.0 applies to a viewer-merged row, so a redeemer sees exactly what their catalog will show them a second later, `name` included (§5.2.0). It is a complete `VehicleSummary`: every field that schema marks `required` is present, which is what lets a client seed its catalog straight from this response.

##### Atomicity and idempotency

The whole redemption is one transaction with the candidate rows held under `FOR UPDATE`:

- **Two people racing one code**: the second blocks, re-reads no pending rows, and gets 404. Never a subset granted, never two grants.
- **The caller owns one of the target vehicles**: 409, with **nothing** written — a multi-vehicle code that includes one of your own cars grants you none of the others rather than a confusing partial set.
- **The same account retrying** after a dropped response: 200 with the same body. Enforced by the partial-unique index over one accepted grant per `(user, vehicle)`.

On success the server busts the redeemer's cached access set, so the granted vehicles appear on their very next `GET /api/vehicles` rather than after the cache TTL.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | The code is malformed after normalization (wrong length or illegal characters). Deliberately **not** 404 — "you sent nonsense" and "that code grants you nothing" are different answers |
| 401 | `auth_failed` | Missing/invalid token |
| 404 | `not_found` | The code is unknown, **expired**, or already consumed by a different account. All three answer with an **identical body**: the server does not tell an enumerating caller which one it hit |
| 409 | `conflict` | The caller owns one of the target vehicles, or already holds a grant on one of them through a different invite |
| 429 | `rate_limited` | Per-user redemption cap exceeded (10 attempts/minute) |
| 500 | `internal_error` | Store-layer error |

##### Rate limiting

The code space is only 36^6 (~2.2 billion), so this endpoint is rate-limited **per authenticated user** at 10 attempts per minute — every attempt counted, successes included, so an attacker cannot interleave a known-good code with guesses to stay under the cap. The counter is **in-process**, which is exact only because the app runs a single Fly machine (`fly.toml` declares one `[[vm]]` with no scale-out); a second machine would make the effective cap N × the limit, and this must move to a shared store **before** any scale-out, not after.

#### 7.5.7 `PATCH /api/invites/{inviteId}` — edit one grant's capabilities (MYR-369)

> **Anchored:** FR-5.1, FR-5.3.
> **Added in contracts v0.23.0.** Additive — `ShareInvite.allowRides` / `ShareInvite.suspended` are optional, and `PatchShareInviteRequest` is a new `$def`.

##### Purpose

The owner changing what ONE accepted grant conveys, **in place**. This is what replaces the pre-MYR-369 rule that a grant's access was fixed for its life and the only way to change it was revoke-plus-reinvite — which forced the invited person to redeem a new code just because the owner turned one capability off.

**Applies to ONE ROW.** A multi-vehicle invite is N rows sharing one code, and patching one changes that vehicle's grant only — so an owner may now hold *different capabilities per vehicle for the same person*, which the fixed tier could not express.

> **AMENDED BY [MYR-451](https://linear.app/myrobotaxi/issue/MYR-451) — the write is now DATED and the decision is now LOGGED.** Nothing about the request or response shape changes; what changes is what survives the request.
>
> An external-beta owner reported that a grantee whose Rides toggle was OFF had still requested and received a ride. The grant row was recovered intact and read `permission = 'rides'` beside `allow_rides = false` — which **proves the patch committed**, because redemption derives the flag from the preset, so a row that redeemed as `rides` and now reads `false` can only have been narrowed afterwards. The create gate is correct and refuses exactly that grant — the gate's decision is covered at handler level by `TestEndpointGrantMatrix`, and the statements it resolves through are covered against a real database by `TestMYR451_WithdrawnRideCapabilityIsRefusedEverywhere` (which closes the persistence half that previously sat below a fake share reader; no single test yet spans both halves).
>
> What could not be established was **when** the flag moved — and therefore whether the rides taken preceded the toggle (correct) or followed it (a live bypass). Those are opposite conclusions and the system could not distinguish them, because:
>
> 1. the row had **no `updated_at`** — `suspended_at` recorded WHEN for suspension, while `allow_rides` sat beside it as a bare boolean recording only the verdict;
> 2. the patch log line carried the invite id and the resulting flags, but **not the grantee, the vehicle, or what was asked for**, so it joined to nothing; and
> 3. the create path logged **denials only**, so a create that sailed through the gate and a create that never happened emitted identical silence.
>
> All three are closed. `updated_at` (migration 0032) is stamped by the same statement that moves the flags; the patch line names `user_id` / `vehicle_id` / `requested_*` beside the resulting state; and a **permitted non-owner create** is logged on the same `(user_id, vehicle_id)` key. The three lines reconstruct the ordering from logs alone. **This is observability, not enforcement** — no gate changed, and none needed to.
>
> **Still open, and NOT fixed here.** The grantee's client is never told that its ride capability was withdrawn: the cache bust below is server-side, and the socket teardown is deliberately limited to suspension because `allowRides` has no WebSocket effect. So a rider app that has already loaded its vehicle list keeps rendering `"Location + rides"` and keeps offering the booking flow until it independently re-reads — which is what the beta reporters saw and described as "I can still request the ride". The server refuses at the final step; the client walks the user to it. Closing that needs a client-visible signal and a client change, tracked separately.

##### Request

`PatchShareInviteRequest` — `{"allowRides"?: boolean, "suspended"?: boolean}`.

**PARTIAL BY DESIGN.** Only the properties **present** are written; an absent property leaves that capability exactly as it was, and is **NOT** the same as sending `false`. The server distinguishes the two with pointer-valued fields (`nil` = absent), which is the one bug this endpoint most easily has — a partial update that silently clears the field it did not mention would be an access-control failure in both directions.

**At least one property is required** (`minProperties: 1`). An empty body is `400 invalid_request`, never a successful no-op: on an access-control surface, "I turned it off and it said OK" is the worst available failure mode, so a client bug must not be able to present as an applied edit.

`permission` is deliberately **not patchable** — on an accepted row it is derived output, not state (§7.5.0).

##### Semantics

- `allowRides` — the §7.8 ride capability. Setting it `false` takes effect on the next request the viewer makes; it does **not** cancel ride requests they have already had accepted. Setting it `true` does **not** un-suspend.
- `suspended` — the whole grant, on or off. `true` removes the vehicle from the viewer's access set entirely (§7.5.0 suspension invariant); `false` restores **exactly** the capabilities the row already carried — nothing is re-derived and nothing is lost. Re-suspending an already-suspended grant refreshes `suspended_at`; the endpoint is idempotent in effect.

##### Response — success

`200 OK` with the updated `ShareInvite`. It is an accepted row, so it carries `allowRides`, `suspended` and the **derived** `permission`, and carries neither `code` nor `shareUrl`.

##### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Malformed body, or a body that changes nothing |
| 401 | `auth_failed` | Missing/invalid token |
| 404 | `not_found` | The invite does not exist, belongs to **another owner**, or is a revoked tombstone. All three answer with an **identical body** — the same non-oracle rule §7.5.3 and §7.5.4 follow, so this endpoint cannot be used to probe for other people's invite ids |
| 409 | `conflict` | The invite is still **pending**. A pending row has no grant to edit — its access is decided at redemption from the invite's preset — so the owner cancels it and sends a new one. Told plainly rather than hidden, because the caller demonstrably owns the row |
| 500 | `internal_error` | Store-layer error |

##### Enforcement notes

- **Owner-only, enforced in the SQL.** `owner_user_id = $n` rides the `UPDATE` itself, on the row it changes. The handler does **not** pre-read the row to check ownership: a read-then-write would add a window and a second thing to disagree, and would buy nothing the predicate does not already give.
- **Atomic.** One conditional `UPDATE ... RETURNING`; the response echoes the row the database now holds rather than a Go-side reconstruction of the request, so a concurrent edit is reported honestly.
- **Cache bust targets the GRANTEE**, not the owner — the MYR-184 bust-on-mutation pattern. The cached access set belongs to the person whose access changed, and for a suspension the bust is a **security property**: that cache is what the WebSocket handshake and every per-vehicle handler consult, so a stale entry *is* a live grant for up to the TTL. The bust is **unconditional** on every successful patch, not only the suspending one — a bust conditional on which field moved is a rule the next person to add a field can get wrong.

##### Three enforcement layers for the ride capability

Because the capability is now editable, an owner can withdraw it *after* a request exists. Three gates cover the three windows, the same shape as the MYR-342 pause:

| Layer | Where | Window it covers |
|-------|-------|------------------|
| Create | §7.8 `POST /api/ride-requests` | The request being made at all |
| Owner accept | §7.8 `POST /api/ride-requests/{id}/accept` | The capability moving while the request sat unanswered in the owner's queue |
| Reservation dispatch | `internal/dispatch/reservation_worker.go` | The capability moving after a **reservation was already accepted** — which may be days before it fires |

The dispatch layer **holds** rather than expires, and sits before the irreversible claim, for the same reason the pause probe does: holding is free and the next tick re-decides, so an owner who restores access inside the lateness window still gets the dispatch they meant to allow. An unreadable grant holds too. The accept layer **fails closed** on an unreadable grant, deliberately unlike the MYR-313 fail-open on an unreadable *vehicle*: dispatching a car to somebody who may have just been suspended is not recoverable the way a retried accept is.

#### 7.5.6 Signed invite links (`ShareInvite.shareUrl` — MYR-368)

> **Anchored:** FR-5.1, FR-5.2, NFR-3.23.
> **Added in contracts v0.22.0.** Additive and optional — a consumer that finds `code` with no `shareUrl` shares the bare code.

##### Why the link is signed

An owner shares a URL, not six dictated characters. That URL necessarily contains the code, which means the join page is now reachable by anyone who can type an address — and a join page that answers "is `RBO247` real?" is an **oracle** against a 36^6 space that the §7.5.5 rate limit only protects on the API side.

So the shell answers nothing. Every link carries an Ed25519 signature that the **web join shell verifies statically**, against a public key **compiled into the shell**: no database, no API call, no server round trip. A link that is unsigned, forged, or edited is bounced by JavaScript that never learned whether the code exists.

What the signature does **not** prove is that the invite is live. A link can verify perfectly and still be expired, cancelled, or already redeemed. Redemption remains the only authority: the shell hands the code to §7.5.5, which decides. The signature buys exactly one thing — the shell can discard obvious garbage without asking.

##### Format

```
https://myrobotaxi.app/join/{code}?k={keyId}.{expUnix}.{sigB64url}&from={from}&to={to}
```

| Part | Value |
|------|-------|
| path segment | The 6-character `code`, verbatim. |
| `k` — part 1 | **Key id**, one character. `1` today. |
| `k` — part 2 | Expiry as **unix seconds** — the row's actual `expires_at`, the value the redeem predicate reads. |
| `k` — part 3 | The 64-byte Ed25519 signature, **base64url, unpadded** (RFC 4648 §5). |
| `from` | The **owner's** first name, sanitized (below). **Omitted entirely** when nothing survives sanitization. |
| `to` | The **recipient's** first name — the first token of this row's owner-typed `label` — sanitized. Omitted on the same rule. |

Parameter order is `k`, `from`, `to`. Nothing in a well-formed link needs percent-encoding: base64url and the sanitized name alphabet are both URL-safe.

##### Canonical signed payload

The signature covers this exact ASCII string:

```
join:{code}:{expUnix}:{from}:{to}
```

**Five fields, always four colons.** `{from}` and `{to}` are the **sanitized** values, and an omitted parameter is signed as the **empty string** — so "no name" and "a name that sanitized to nothing" are the same thing, and a verifier reconstructs the payload from the query exactly as it finds it. No field can contain a `:`: the code alphabet is `[A-Z0-9]`, the expiry is decimal digits, and the names are ASCII letters. The `join:` prefix **domain-separates** this signature, so nothing signed by this key can ever be replayed as a signature over something else.

##### Name sanitization

Applied server-side to both names; a verifier does **not** re-apply it, it simply reads the query values as given.

1. Take the **first whitespace-separated token** (`strings.Fields`, so any run of Unicode whitespace collapses). This is the P1 **first-names-only** policy that governs `RideRequest.requesterName` and the push payloads — a link arriving by text message must not carry somebody's surname.
2. Strip to **ASCII letters** `[A-Za-z]`. One rule removes every injection question a more permissive filter would have to answer case by case: no markup, no separators, no percent-encoding, no bidi or zero-width tricks. The cost is honest — accented and non-Latin names lose characters or vanish.
3. **Cap at 20 characters**, a bound on a value that originates as owner-typed input.
4. If nothing survives, **omit the parameter** and sign the empty string. Consumers MUST render generic copy for an absent name — never a placeholder built from the code.

Worked examples: `"Mira Chen"` → `Mira`; `"  Ada   Lovelace "` → `Ada`; `"O'Brien-Smith"` → `OBrienSmith`; `"José"` → `Jos`; `"🙂"` → omitted; `"123"` → omitted.

**Tampering with a name breaks the signature**, which is the reason the names are in the payload at all: without it, somebody holding a genuine link could rewrite `from` to any name the recipient would trust.

##### Key management

The private half is an Ed25519 **seed**, 32 bytes, base64, in the Fly secret **`INVITE_LINK_SIGNING_KEY`**. It exists nowhere else.

```bash
# 1. Generate the seed
openssl rand -base64 32

# 2. Derive the PUBLIC key to paste into the web join shell's constant
INVITE_LINK_SIGNING_KEY='<seed>' go run ./cmd/ops invite-link public-key

# 3. Set the secret and deploy
fly secrets set INVITE_LINK_SIGNING_KEY='<seed>'
```

`ops invite-link public-key` needs no database, no network, and no deployed process — it is arithmetic on the seed, so it can be run before the secret is ever set. It is deliberately the boring option: an authenticated admin endpoint would have meant a new route and a new auth surface to print a constant.

The server also logs the public key at startup (`invite-link signing key loaded`, with `public_key_b64`). Public by definition and safe to log — and it answers the one question the subcommand cannot: **which key the running process actually loaded**. Use the subcommand to obtain the value, the log line to confirm the deploy took it.

**Startup FAILS FAST** when `INVITE_LINK_SIGNING_KEY` is absent outside `--dev`, and on a malformed value in **any** mode. This is the kill-switch precedent (`DISPATCH_ENABLED`, `PUSH_ENABLED`, `SERVICE_REPOLL_ENABLED`) applied to a security control: without the key the server runs perfectly well and simply stops emitting `shareUrl`, which is exactly the failure worth refusing because it is **invisible** — every owner's share sheet quietly degrades to dictating six characters and nothing in the running system says why. Under `--dev` with no seed the server generates an **ephemeral** key per process, so a local link can never be mistaken for one production would honour.

##### Rotation

The key id is why it is in the URL. To rotate:

1. Teach the shell key `2` **alongside** `1` — it verifies against whichever id the link names.
2. Set the new seed on the server; it signs everything new as `2`.
3. Links signed under `1` keep verifying until the last of them expires, at most 7 days later. Then drop `1` from the shell.

Without the id the shell could hold only one key at a time and a rotation would break every unredeemed invite the moment it happened.

##### Classification

`shareUrl` is **P1 and bearer**, identical to `code` — it contains it. Never logged, never in an error envelope, never on a non-owner surface, and present only on `pending` rows. It is in the owner allow-list in `internal/mask/tables.go` (`inviteOwnerFields`); a wire field missing from that list is silently dropped from the response. No new persisted column: the URL is derived per request from `code`, `expires_at`, and the two names — [`data-classification.md`](data-classification.md) §1.15 is unchanged.

---

### 7.6 `DELETE /api/users/me`

> **Anchored:** FR-10.1, FR-10.2, NFR-3.29; App Store Review Guideline 5.1.1(v) (an app offering account creation MUST offer in-app account deletion).
>
> **REWRITTEN BY [MYR-355](https://linear.app/myrobotaxi/issue/MYR-355) (2026-07-30).** The previous version of this section assigned the endpoint to the **Next.js app** (per §10 DV-23) and specified a `200 OK` body plus a NextAuth-specific recent-auth gate. All three parts are superseded. See "What changed and why" at the end of this section — the old text is not merely restated here, because a contract that describes a process no client reaches is worse than no contract.

#### Purpose

Deletes the authenticated user and every piece of data the platform holds about them, per the sequence defined normatively in [`data-lifecycle.md`](data-lifecycle.md) §3. This is the SDK's single entry point for user-initiated data deletion (FR-10.1), and it is what makes the iOS app shippable: Apple rejects any app that creates accounts and offers no way to delete one.

**The Go telemetry server serves this endpoint.** The native iOS client (P9) is the only consumer, it never talks to the Next.js app, and most of what must be deleted now lives in Go-owned tables no Prisma cascade reaches (`go_ride_requests`, `go_vehicle_shares`, `go_push_devices`, `go_refresh_tokens`, `go_users`, `go_identity_apple`, `go_removed_vehicles`). The `account_deleted` AuditLog row (FR-10.2) moves with the endpoint and is written by `store.AccountDeleter.DeleteIdentity` inside the same transaction as the identity delete (CG-DL-3).

#### Request

```
DELETE /api/users/me HTTP/1.1
Host: api.myrobotaxi.com
Authorization: Bearer <token>
```

**No request body.** The endpoint takes none and reads none. There is nothing to scope: a caller may delete exactly one account, their own, in full.

#### Response — 204 No Content

Empty body. There is no account left to describe, and a body would only tempt a client to render something to a user it is already signing out. Clients MUST NOT attempt to decode the response.

The previous `{ "deleted": true, "auditLogId": … }` body is withdrawn: `auditLogId` existed so the web test bench could cross-reference the audit row, a surface the iOS client has no access to and no use for, and returning an internal row id to a caller whose account has just ceased to exist is not a capability worth keeping.

#### Response — error

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/expired token, or a token whose user no longer resolves. Also the expected response on a call made *after* a successful deletion once the server's user-existence cache has evicted. |
| 405 | `invalid_request` | Any method other than DELETE. |
| 429 | `rate_limited` | REST rate limit breached. |
| 500 | `internal_error` | A step of the sequence failed. **Partial deletion is possible and expected** — see Idempotency. |

There is no 403: the endpoint operates on `/users/me`, so the caller is always the owner of the account in question. There is no cross-user deletion in v1.

#### Idempotency and the re-run contract

This is the part clients must implement against, and it differs from every other destructive endpoint in this document.

The deletion is **NOT one transaction**, and cannot be. The per-vehicle teardown it composes (§7.12, `store.OwnerTeardown`) is already its own transaction — it takes `FOR UPDATE` locks over the owner's vehicle set and fires the `vehicle_deleted` NOTIFY, whose consumers must not observe uncommitted work — and the ride-cancellation step publishes push notifications. Wrapping N of those in an outer transaction would deadlock against the locks or notify people about work a rollback then undid.

What is guaranteed instead:

1. **Every step is idempotent.** Re-running affects zero rows for work already done.
2. **The whole sequence is RE-RUNNABLE.** A `500` means the deletion stopped part-way; calling `DELETE /api/users/me` again resumes it.
3. **The identity rows are deleted LAST**, so a mid-sequence failure never leaves an account that cannot authenticate to finish its own deletion. This ordering is the contract, not an implementation detail.
4. **Exactly one `account_deleted` audit row** is ever written, however many times the endpoint is called: the final transaction writes it only when it finds identity rows still present.
5. **A second call after success is `204`**, not an error, until the caller's access token stops validating.

Clients SHOULD therefore offer a plain "try again" on failure, and MUST NOT sign the user out on a `500` — they need their token to retry.

#### What is deleted, and in what order

Normative in [`data-lifecycle.md`](data-lifecycle.md) §3.1. Summary:

| # | Step | Notes |
|---|------|-------|
| 1 | Count the user's drives | Audit metadata only; a failure here is logged and ignored — a missing statistic never blocks erasure |
| 2 | **Revoke the Tesla OAuth grant at Tesla** | `POST https://auth.tesla.com/oauth2/v3/revoke` with the stored `refresh_token` + `client_id`. **Best-effort and non-fatal** — see "Tesla grant revocation" below. MUST run before step 3, which deletes the token it needs |
| 3 | Tear down every owned vehicle | One existing §7.12 `owner_teardown` transaction per car: `Vehicle` + cascade, that car's `go_ride_requests`, every sharing grant ON the car revoked, the VIN tombstoned, and on the last car the Tesla `Account` tokens cleared + `Settings` reset |
| 4 | Revoke shares RECEIVED | Every accepted grant the user redeemed → `revoked` tombstone |
| 5 | **Scrub the owner-typed label off shares RECEIVED** (MYR-447) | Step 4 tombstones the ACCESS; this erases the NAME. `label` is free text the CAR OWNER typed for their own list, so on a redeemed grant it is the departing person's name in somebody else's row. Keyed on the person, not the status, so it also reaches grants that were ALREADY revoked — which step 4, being idempotency-guarded, skips. Scrubbed to `''`. No wire effect: a revoked grant is never serialized |
| 6 | Cancel open rides as RIDER | Through the guarded §7.8 transition, publishing `ride_status_changed` so affected owners get the standard lifecycle push |
| 7 | Delete push devices | Whole `go_push_devices` address book for the user |
| 8 | Delete saved places | The person's `go_saved_places` Home/Work rows (MYR-321, §7.20) — personal effects with no counterparty |
| 9 | Revoke refresh tokens | `go_refresh_tokens` marked revoked with `reason='account_deleted'` |
| 10 | Delete identity + write audit | ONE transaction: the `account_deleted` row, then `go_identity_apple`, `go_users`, and the Prisma `"User"` row **if one exists** |
| 11 | Invalidate auth caches | The caller's unexpired access token stops validating immediately rather than at the cache TTL |

**Dual-source identity.** Step 10 handles both account shapes with the same statements: an Apple-native user has no `"User"` row (that DELETE affects zero rows) and a legacy web user has no `go_users` row. Neither case is special-cased and neither is an error.

**A ride physically in progress (`enroute` / `arrived`) is NOT cancelled by the deletion teardown.** Since MYR-537 those states ARE rider-cancellable interactively — a person aboard making a deliberate choice, with the owner pushed about it — but a DELETION is bookkeeping, and cancelling a car mid-trip from under its owner is a worse outcome than letting the ride reach its own terminal state. The teardown keeps its own narrower from-set (`requested`/`accepted`, the pre-MYR-537 pair, deliberately decoupled from the rider's interactive window); an in-progress ride closes on its own and afterwards renders as former-rider history like any other completed ride.

#### Tesla grant revocation

> **Added by [MYR-366](https://linear.app/myrobotaxi/issue/MYR-366).** Before MYR-366 the deletion merely DELETEd the stored Tesla tokens; the grant itself stayed listed on the owner's tesla.com third-party-apps page until they removed it by hand.

Step 2 now actively revokes the grant: a form-encoded `POST` to Tesla's OAuth2 revocation endpoint `https://auth.tesla.com/oauth2/v3/revoke` (RFC 7009, same auth host as authorize/token) carrying `token` = the stored **refresh** token, `token_type_hint=refresh_token`, and `client_id`. No client secret is sent. Presenting the refresh token invalidates the whole grant, access tokens included.

Three properties are contractual:

1. **It runs BEFORE anything deletes the tokens.** Step 3's last-vehicle arm deletes the `Account` row and step 10's `User` cascade takes any that survives; after either, the refresh token is gone and revocation is impossible. This ordering is normative — see [`data-lifecycle.md`](data-lifecycle.md) §3.1.
2. **It is best-effort and NEVER blocks the deletion.** A Tesla 5xx, a timeout, a network failure, an already-invalid token, or no Tesla account at all are each logged and stepped past. The response is unchanged: still `204`, still no body. **A client cannot observe whether revocation succeeded**, and deliberately so — the account is deleted either way, and a partial-success signal would be a state no client could act on.
3. **A re-run skips it cleanly.** The second call finds no stored token and makes no request to Tesla.

Skipped entirely on a deployment with no Tesla OAuth `client_id` configured, in which case the pre-MYR-366 behaviour stands.

#### Data retention — ride history is NOT deleted

Terminal ride rows (`completed` / `declined` / `cancelled`) in which the deleted user was the **rider** are **kept**. They are the *owner's* record of their own car, and erasing them would delete a second person's data to satisfy the first person's request.

The rows are kept **whole and unmodified** — `rider_id` still holds the deleted user's cuid. No column was added and no row was rewritten. The mechanism is the identity resolution that already exists: `requesterIdentitySelect` (MYR-229/MYR-264) probes all three identity sources with `requester_exists`, finds none, and the projection **OMITS `requesterName`** rather than degrading it to the `"Rider"` literal (which means "a rider with no name on file", a different fact). An omitted `requesterName` on the live path therefore means precisely *"this account was deleted"*, and the iOS client renders it as **"Former rider"**.

This is pinned by `TestAccountDeletion_RideHistorySurvivesAsFormerRider`, which asserts a real first name before the deletion and an omitted one after.

**One asymmetry, stated rather than hidden:** an OWNER's deletion runs the §7.12 teardown per car, and that teardown deletes the car's `go_ride_requests` rows outright — so riders lose their history of rides in that owner's car. That is pre-existing MYR-258 behaviour (those rows carry P1 encrypted pickup/dropoff GPS for a vehicle that is leaving the platform) and MYR-355 deliberately did not change it. The retention rule above is therefore precise: **a rider's deletion preserves the owner's history; an owner's deletion does not preserve the rider's.**

#### What is NOT deleted

| Record | Reason |
|--------|--------|
| `AuditLog` rows | Retained indefinitely per NFR-3.29. No FK to `User` — the orphaned `userId` is intentional |
| Terminal ride rows where the user was the rider | Counterparty records — see Data retention above |
| Revoked share tombstones | The owner's audit trail of who could see their car outlives the viewer's account |
| Revoked refresh-token rows | Rotation lineage is reuse-detection evidence (hash-only; the raw token was never stored) |
| The Tesla virtual key | No Fleet API path exists; only the owner can remove it, from the car's touchscreen (§7.12) |
| The Tesla-side grant, **only when revocation fails** | Since MYR-366 the deletion actively revokes it (see "Tesla grant revocation" above), so it normally DOES go. The call is best-effort: when Tesla refuses or is unreachable the deletion still completes and the owner-confirmed consent page (§7.12) is the fallback |

#### Audit row (FR-10.2)

`action='account_deleted'`, `targetType='user'`, `targetId`=the caller's own cuid, `initiator='user'`. `metadata` is **P0 counts only** per CG-DL-5: `{vehicleCount, driveCount, ridesCancelled, sharesRevoked, shareLabelsScrubbed, pushDevicesDeleted, savedPlacesDeleted, refreshTokensRevoked, hadPrismaUser}`. No name, no email, no VIN, no coordinate, no token.

#### What changed and why

| Was (pre-MYR-355) | Now | Why |
|---|---|---|
| Served by the Next.js app (§10 DV-23) | Served by the Go telemetry server | The only client is the native iOS app, which never reaches the Next.js app; and the Prisma cascade cannot reach the Go-owned tables that hold most of the data |
| `200 OK` + `{deleted, auditLogId}` | `204 No Content`, no body | Nothing left to describe; `auditLogId` served a web test bench the iOS client cannot use |
| One Prisma `$transaction` | A sequence of independently-atomic, idempotent, re-runnable steps | The composed per-vehicle teardown is already a transaction with locks and a NOTIFY; see Idempotency above |
| Recent-auth gate (`auth_time` ≤ `REAUTH_MAX_AGE_SEC`, `subCode: reauth_required`) | **Not implemented in this round** | The gate was specified against the NextAuth `auth_time` claim. The Go identity module's ES256 access token (§7.10) carries no `auth_time` claim, so there is nothing to check — implementing it requires minting the claim in `identity.TokenMinter` and threading it through the refresh rotation, which is its own change. The client-side control that DOES ship is a **two-step explicit confirmation** in the iOS app. **This is a deliberate, recorded gap, not an oversight**: the GDPR Art. 17 recent-auth rationale in the previous text still stands, and re-instating the gate on the Go token is tracked as follow-up work |

---

### 7.7 `GET /api/users/me/export`

> **Anchored:** FR-10 (data-export companion to FR-10.1 deletion), GDPR Art. 15 (right of access), GDPR Art. 20 (portability), NFR-3.29; GDPR Art. 17 recent-auth corollary (symmetric with §7.6).

#### Purpose

Returns a JSON archive of every Prisma row owned by the authenticated user — the SDK's single entry point for GDPR Art. 15 / Art. 20 portability exports. The endpoint is the export companion to `DELETE /api/users/me` (§7.6); together they implement the data-export-then-delete flow GDPR requires before erasure. Phase A implementation: [myrobotaxi/react-frontend#259](https://github.com/myrobotaxi/react-frontend/pull/259) (Next.js handler).

The handler runs in the Next.js app. It is the last path DV-23 still assigns there: the §7.5 half was SUPERSEDED by MYR-184 (sharing moved to the Go server) and the §7.6 half by MYR-355 (deletion moved with it), neither of which disturbs this one. Note the asymmetry that leaves: **deletion is served by Go and export by Next.js**, so the export companion this section describes is not reachable from the iOS client at all. The public API hostname (`https://api.myrobotaxi.com/api/...`) proxies `/api/users/me/*` paths to the Next.js app. The Go telemetry server has no User repository and no export handler. The SDK is unaware of which process serves the request.

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

#### Requester name (MYR-229)

The `RideRequest` object carries an **optional** `requesterName` string so an owner sees who asked for the ride instead of a bare `riderId` cuid. It is **populated server-side** from the requester's (`riderId`) identity — the requester is never allowed to set it — resolved READ-ONLY (CG-DL-9) from **three** identity sources in precedence order (MYR-264): the sibling-schema `"User"` row, then the Apple first-consent name in `go_identity_apple`, then `go_users`. The third leg matters more since MYR-184: a rider who joined via an invite code may be Apple-native and have no `"User"` row at all, and a `"User"`-only lookup would show their owner a placeholder. The resolution ladder within a source is:

1. **First name** — the first whitespace-separated token of the user's display `name` (e.g. `"Ada Lovelace"` → `"Ada"`).
2. **Email local-part** — the part before `@` when there is no usable name (e.g. `"grace.hopper@navy.mil"` → `"grace.hopper"`).
3. **`"Rider"`** — a stable literal when the user row exists but carries neither a usable name nor email.

The field is **OMITTED** (never an empty string) in exactly one case: the rider has **no `"User"` row at all**. It is surfaced on the party-only detail (`GET /api/ride-requests/{id}`) **and every list item** (rider list, owner incoming feed). Every read, list item, and mutation return resolves the requester **inline via a correlated subselect in the same statement** as the ride row (no separate lookup, no N+1, no after-commit window) — so a status transition and `Create` return the name atomically, and an unresolvable identity never fails the ride operation. The same value rides the reactive WS `ride_request_created` / `ride_status_changed` summary frames (websocket-protocol.md §4.7–4.8). **Classification:** P1 PII, party-only surface, never logged (data-classification.md §1.9).

#### Authorization model (v1 enforced vs deferred)

- **Create** derives `ownerId` from the target vehicle's owner and enforces a vehicle-access check: the caller must be the vehicle's **owner**, OR hold an accepted share at the **`rides`** tier (§7.5). A rider ≠ owner request is therefore normal as of **MYR-184**, and `ownerId` is still the VEHICLE's owner — that asymmetry is what routes the accept/decline to the right person. **CORRECTION (MYR-184, 2026-07-29):** this bullet previously said shared-viewer requests would "light up automatically" when `GetUserVehicles` gained the viewer merge, with "no change to this handler required". **That was wrong.** The create-time check is a SEPARATE code path from the read-side access set (`internal/telemetry/ride_request_handler.go`, an owner-equality test on the `GetByID` row): widening the access set put shared cars in a viewer's list and let them read the snapshot, but this check still refused every one of their ride requests. Granting `rides` required changing it, and MYR-184 did. The identical claim in `internal/telemetry/ride_request_handler.go`'s type doc has been corrected in lockstep. **AMENDED BY [MYR-369](https://linear.app/myrobotaxi/issue/MYR-369):** the gate no longer reads a tier. It reads the grant's **`allow_rides` flag**, which the owner can switch off in place through §7.5.7 without revoking the grant, and which a **suspended** grant never satisfies. The check is therefore live state rather than something fixed at invite time — and it is now backed by two more layers, the owner-accept backstop and the reservation-dispatch probe (§7.5.7). Unknown vehicle → `404 not_found`; visible-but-not-accessible, and any viewer whose grant lacks the ride capability → `403 vehicle_not_owned`. A rider may hold only **one OPEN instant ride** (MYR-230): a second **instant** create while one is open is `409 ride_active` and returns the existing ride for the client to adopt; **scheduled** creates are exempt (see the `POST` section below).
- **Detail (`GET {id}`)** is **party-only**: rider OR vehicle owner. A caller who is neither gets `404 not_found` (not `403`) so the server never confirms the existence of a ride the caller has no relation to.
- **Cancel** is **party-aware** (MYR-522; rider-only before it). The RIDER may cancel from ANY live status — `requested`/`accepted`/`arrived`/`enroute` ([MYR-537](https://linear.app/myrobotaxi/issue/MYR-537), client directive: "allow rider to cancel ride at anytime even after it's started"; before it, `requested`/`accepted` only). The OWNER may cancel from `accepted`/`arrived`; a `requested` ride keeps DECLINE as the owner's one exit (409 naming it), and once the rider is aboard (`enroute`) there is no owner cancel — ending early is what dropped-off already is (409 naming it). A non-party → `404`. A self-ride caller (owner == rider) takes the rider path. Both paths stamp `cancelledBy` ("rider" | "owner") in the same guarded write.
- **Accept / decline** are **owner-only**. The rider is a party but cannot decide → `403 permission_denied`. A non-party → `404`. (MYR-175)
- **Owner-driven handshake (MYR-270).** **picked-up** (`accepted → arrived`) and **dropped-off** (`enroute → completed`) are **owner-only** (the rider is a party but cannot confirm → `403`); **start** (`arrived → enroute`) is **rider-only** (the owner is a party but cannot start → `403`). A non-party → `404` on all three.
- **Incoming feed** is scoped to the authenticated owner (`owner_id == JWT sub`) — no cross-owner reads are expressible. (MYR-175)
- `riderId` is always the JWT `sub` (never client-supplied); `id`, `status`, and all timestamps are server-assigned.

#### `POST /api/ride-requests`

Body is `RideRequestCreateRequest` (`vehicleId`, `pickup`, `dropoff` required; `passengerName`, `passengerPhone`, `scheduledFor` optional). The body is decoded **strictly** — unknown keys are `400 invalid_request` (schema `additionalProperties:false`). `pickup`/`dropoff` are validated as `RidePlace` (lat ∈ [-90,90], lng ∈ [-180,180], non-empty `label`). Responds **`201 Created`** with the full `RideRequest` and unicasts `ride_request_created` to the rider + owner.

**One active instant ride per rider (MYR-230).** An **instant** create (`scheduledFor` absent) is refused **`409 ride_active`** when the rider already holds an OPEN instant ride — one whose status is not terminal (`requested`, `accepted`, or any in-progress/tracking state; NOT `completed`/`declined`/`cancelled`). The 409 body carries that existing ride so the client adopts it instead of stacking a duplicate — see "409 `ride_active` body" below. **Scheduled creates (`scheduledFor` present) are EXEMPT**: a future reservation is not an active ride, so a rider may hold any number of scheduled rides plus one open instant ride, and an open scheduled ride never blocks a new instant one. The guard is enforced authoritatively by a **partial unique index** (`uq_go_ride_requests_active_instant_rider` on `rider_id WHERE scheduled_for IS NULL AND status IN (open states)`, migration 0004): two concurrent instant creates serialize in Postgres — exactly one INSERT wins, the loser's `23505` maps to the same `409 ride_active`. This is the create-side analogue of the guarded-transition race discipline (`UpdateStatusFrom`, below).

> **One-time dedup at migration (deploy caveat).** The guard postdates the create endpoint, so databases that predate it can hold riders with several concurrently-open instant rides (stale pre-guard test debris). Migration 0004 is self-cleaning: **before** creating the index it transitions every OLDER open instant ride per rider to `cancelled`, keeping only the MOST RECENT one (`ORDER BY created_at DESC, id DESC` — the list endpoints' total order; keeping newest matches rider expectation). The dedup mirrors the store's `UpdateStatusFrom` timestamp discipline (entering `cancelled` stamps neither `acceptedAt` nor `completedAt`; `updatedAt` is touched). **No `ride_status_changed` WS frames fire for these rows** — migrations run with no event bus; clients refetch via REST (FR-9.1/FR-9.2 reconciliation) and observe the stale rides as `cancelled`.

Errors: `400 invalid_request` (malformed/unknown-field body, bad place, bad `scheduledFor`, a `scheduledFor` more than **30 days** in the future — the MYR-372 horizon bound, see below — or a `scheduledFor` EARLIER than the vehicle's `serviceEstimatedEndAt` — the MYR-316 service-window bound, see the accept section below), `401 auth_failed`, `403 vehicle_not_owned`, `404 not_found` (unknown vehicle), `409 ride_active` (rider already has an open instant ride), `409 vehicle_unavailable` (the vehicle's owner has PAUSED ride sharing — MYR-342, see below; **or, with `subCode: time_conflict`, the target vehicle is already promised to another open ride within 45 minutes of the requested `scheduledFor` — the MYR-383 window gate, see below**), `500 internal_error`.

**Future-horizon bound on `scheduledFor` (MYR-372).** A create is refused **`400 invalid_request`** — the existing shape, deliberately not a new error code, exactly as the MYR-316 service-window bound refuses — when `scheduledFor` is more than **30 days** after the server's current instant, with the message `scheduledFor must be no more than 30 days in the future`. Before this the field was validated only for RFC 3339 syntax, so a pickup could be booked arbitrarily far out. A reservation is not a calendar entry: it is a standing instruction that the reservation sweeper re-evaluates every 30 seconds until it fires, held against an availability question nobody can answer for an instant years away. Three rules:

- **EQUAL is ALLOWED, and the bound is a FIXED 720-HOUR DURATION, not calendar arithmetic.** The server compares `scheduledFor` against `now + 720h` in UTC; it does not add 30 calendar days in anybody's local zone, and it cannot, because it does not know the rider's zone. Both instants are UTC so no DST transition affects the *comparison* — but a client that renders "30 days from now" in a local zone that crosses a DST boundary can produce an instant up to an hour past the limit and be refused. Clients SHOULD therefore compute the offer in UTC, SHOULD offer at most 30 days, and MUST still handle the `400`: the two clocks are not the same clock. Exactly at the limit passes (strictly "later than"), the same reading the service-window bound takes.
- **The PAST is deliberately UNBOUNDED.** There is no lower bound here. The accept-after-due semantics below let an owner accept a reservation whose instant has already passed, and the sweeper's 30-minute lateness ceiling is what resolves those honestly; a lower bound would refuse rides this contract already promises to take.
- **The bound applies at CREATE only.** `scheduledFor` is client-supplied nowhere else — accept carries no instant of its own — so there is no second layer to enforce, and a reservation booked before this bound existed is not retroactively invalidated.

**Ride-sharing pause gate on create (MYR-342).** A create is refused **`409 vehicle_unavailable`** with the message `Ride sharing is paused for this vehicle` when the target vehicle's `rideShareEnabled` (§7.0 / §7.1) is `false` — the owner has switched the car out of ride-hailing through §7.18. The typed code is deliberately the one already shared with the MYR-266 per-vehicle busy guard and the MYR-277 in-service/offline gate: all three are **capability** refusals — the request is well-formed and the caller authorised, the car simply cannot serve it — not the `conflict` that means an illegal lifecycle transition, and not `permission_denied`, which would tell a legitimate rider they are the wrong person.

Three rules, all load-bearing:

- **It applies to SCHEDULED creates too — a DELIBERATE DEVIATION from the MYR-313 exemption.** MYR-313 lets a *reservation* through for a car that is `in_service` or `offline` today, because a service visit **ENDS**: the car's state today says nothing about the reservation instant, and refusing strands the owner over a condition that will have cleared. An owner's pause has no such horizon — nothing ends it but the owner reaching for the switch again — so "the condition will have cleared by then" is a hope, not a fact. Honouring the exemption here would let a rider book a car its owner has withdrawn indefinitely, and would leave the request sitting in the owner's queue: the exact hand-declining treadmill §7.18 exists to end. The same deviation applies on **accept** and in the **reservation sweeper**, for the same reason.
- **The gate sits AFTER the vehicle-access check.** A caller with no access to the vehicle gets the access denial they would have got anyway (`403`/`404`), never the pause `409` — the gate must not become an oracle answering questions about a stranger's car.
- **The flag is read off the SAME `GetByID` row that resolved the owner**, not a second lookup, so access and availability come from one statement and cannot disagree. **TOCTOU, honestly stated:** there is a window between that read and the row insert, bounded by the handler's own latency, in which an owner who pauses may still see one request land. It is accepted rather than closed, because the insert is guarded by the per-rider and per-vehicle unique indexes, neither of which can carry a vehicle-level predicate. The backstops are the point of having **three** layers: a request that slips past create is refused at accept, and a reservation that slips past accept is held at dispatch.

##### Per-vehicle ride-window conflict — a vehicle cannot promise two rides in one window ([MYR-383](https://linear.app/myrobotaxi/issue/MYR-383))

A **scheduled** create is refused **`409 vehicle_unavailable`** with **`subCode: time_conflict`** when the target vehicle is already promised to another open ride too close to the requested `scheduledFor`. The same gate is enforced again on **accept** (see below) — the two-layer shape of the MYR-316 service-window bound.

> **THE MODEL, in one sentence.** Every OPEN ride on a vehicle **OCCUPIES a window** of ±**45 minutes** around its ride instant — `scheduledFor` for a reservation, **now** for an active instant ride — and a reservation whose instant falls **inside** one of those windows is refused.

Two arms, because a ride's instant has two spellings:

| Arm | What occupies the window | Statuses that count | Window is around |
|-----|--------------------------|---------------------|------------------|
| **Reservation** | another reservation on the same car | `accepted` / `arrived` / `enroute` always; `requested` **on create only** (see below) | that reservation's `scheduledFor` |
| **Active instant** | an instant ride the car is committed to | `accepted` / `arrived` / `enroute` | **now** — a car mid-ride cannot also promise a pickup 20 minutes out |

**This closes a documented v1 boundary.** "Reservation ↔ reservation" and "instant ↔ reservation" were both listed as accepted v1 boundaries under "Reservation-time dispatch" (MYR-179), on the reasoning that collision detection belongs at BOOKING time rather than in the dispatcher. This is that booking-time gate. The dispatch-side boundaries themselves are **unchanged and still real** — the sweeper's busy predicate still excludes scheduled rides — but they are now much harder to reach, because the colliding pair can no longer be created. The v1-boundaries note has been annotated accordingly rather than deleted: pre-existing double-bookings and the ungated instant direction (below) can still produce one.

Six rules, all load-bearing:

- **The window is a single constant, and 45 minutes is a PRODUCT GUESS.** Roughly "a ride plus the drive to the next pickup" for the single-city fleet v1 serves. It appears in exactly one place in the server (`store.RideConflictWindow`), is passed to SQL as a bind parameter rather than baked into an interval literal or an index, and is **not** encoded in any schema, enum, or client. Changing it is a one-line change with no migration and no contract bump. Clients MUST NOT hard-code it; the refusal names the conflicting instant, which is what a picker needs. **As of [MYR-385](https://linear.app/myrobotaxi/issue/MYR-385) a picker does not have to wait for the refusal to learn this** — §7.22 reports the vehicle's blocked windows as CONCRETE instants, resolved server-side against this same constant, which is what keeps the constant off the wire while still letting the picker dim correctly.
- **The comparison is STRICTLY inside the window.** Two rides exactly 45 minutes apart are **allowed** — that is a legal back-to-back booking, and an off-by-one refusing it would look like a broken rule. 44 minutes 59 seconds is refused.
- **`requested` counts on CREATE but NOT on ACCEPT — deliberately, and the asymmetry is the design.** A pending reservation is a **claim** on a slot but not a **commitment** of the car, and the two endpoints ask different questions of it. *Create* asks "is this slot already claimed?" — a rider must not be handed a 3:20 PM booking that is going to collide with somebody's pending 3:00 PM request, because resolving that costs the owner a hand-decline (the treadmill §7.18 exists to end). *Accept* asks "is the car already COMMITTED at this hour?" — an owner's accept is precisely **how** a contested slot is decided, so counting the peer request would refuse **both** sides of a legacy double-booking and leave the owner unable to confirm either, which is the same stranding this gate exists to prevent, arrived at from the other side. It would also make the refusal claim a slot is "booked" when nobody has booked it. Peer requests lose the moment one of them is accepted: the winner becomes committed, and the loser's accept is then refused truthfully.
- **Terminal rows free their window immediately.** A `declined`/`cancelled`/`completed` reservation is outside both arms, so the refusal is a **DEFERRAL**, never a permanent hold on a slot: decline the holder and the identical booking succeeds. Nothing about a refused request changes on the server — no row, no event, no notification.
- **The refusal names the WINDOW, never the other ride.** The message carries the conflicting instant and, when the claim is only pending, says so. It carries **no** `activeRideRequest`-style sibling object and no id, rider, requester name, pickup or dropoff: the caller is not a party to that ride, those fields are P1 (data-classification.md §1.9), and a booking probe must not become a way to enumerate a stranger's calendar. The instant alone is P0 operational timing — the same tier as `status`, and the same disclosure the MYR-316 refusal already makes. Three messages, each saying only what is true:
  - `Vehicle is already booked for a ride at <RFC3339>` — a committed reservation.
  - `Vehicle already has a ride request for <RFC3339>` — a pending claim (create only).
  - `Vehicle is already on a ride and can't also be booked for that time` — the active-instant arm, which has no scheduled instant to name.
- **INSTANT creates and INSTANT accepts are NOT gated by this rule** (v1a). An instant create inserts at `requested`, which occupies no window; an instant **accept** deliberately skips the probe, so a reservation 20 minutes out does **not** refuse an instant accept even though the reverse holds. That direction is a **remaining boundary**, narrowed but not closed: it is the accept-side half of the MYR-179 "instant ↔ reservation (one-directional)" boundary. MYR-385 (§7.22) shipped the picker-annotation read surface but deliberately did **not** touch this — a read surface reports the rule, it does not widen it.

**Two other remaining boundaries, stated plainly.** (1) A **confirmed reschedule** moves a reservation's `scheduledFor` through its own unconditional write and is **not** window-gated, so a negotiation can still land a reservation inside another's window; the accept-side layer does not re-run, because the ride is already `accepted`. (2) A pair of reservations **already accepted** before this gate shipped stays as it is — the accept layer catches a legacy pair only if one is accepted *after* the other, so an already-accepted pair remains a dispatch-time collision (see the MYR-179 v1-boundaries note). Both are follow-ups, not properties of the rule.

**Why `vehicle_unavailable` and not a new code.** This is a **capability** refusal — the request is well formed and the caller authorised, the vehicle simply cannot serve this ride — exactly like the MYR-277 in-service/offline gate, the MYR-342 pause and the MYR-266 per-vehicle busy guard, all of which already carry this code. It is **not** `conflict`, which means an illegal lifecycle *transition*: the ride being booked is perfectly legal. A new top-level code would also have to be added to the shared `ErrorPayload.code` enum every SDK compiles against, for a refusal an existing code already describes.

**Why the `time_conflict` sub-code is required.** The three sibling carriers of `vehicle_unavailable` are all conditions of the car **right now** — a client answers them with "try again later". This one is a property of the **time the rider picked**: the car is fine, the slot is taken, and the client should send the rider back to the picker. Consumers MUST branch on `subCode`, never on the message (§4.1 rule 1). A consumer that ignores `subCode` still gets a well-formed, correctly-coded 409.

**Race construction: a per-vehicle advisory transaction lock, NOT an index.** Every other guard on this table is a partial unique index (0004 per-rider, 0013 per-vehicle), and an exclusion constraint over a time range was the obvious next reach. It does not fit, for three independent reasons: **(1)** half the predicate is relative to **now** — the active-instant arm occupies a window around the current time, which no *immutable* index expression can hold, so an `EXCLUDE` could cover only one arm and the rule would need two mechanisms; **(2)** a constraint cannot be created over data that violates it, so the migration would have to **cancel live production reservations** to install itself — the 0004/0013 dedup pattern applied to rows a human is expecting a car for; **(3)** `btree_gist` is an extension, and `CREATE EXTENSION` from an application migration role is a deploy-time gamble the gate does not need to take. So both landing sites run

```
BEGIN;  pg_advisory_xact_lock(<vehicle>);  <window probe>;  <INSERT | guarded UPDATE>;  COMMIT
```

Two conflicting bookings for one car **serialize on the lock**; the loser's probe runs after the winner has committed and sees it. Bookings for different cars never contend. The lock is released by COMMIT/ROLLBACK — including on a panic or a dropped connection — so there is nothing to leak and nothing to unlock. **Every accept takes the lock**, including an instant accept that skips the probe, which is what makes an instant accept and a reservation accept for the same car serialize. **Migration 0026** adds `idx_go_ride_requests_vehicle_window`, a **plain partial index** over `(vehicle_id, scheduled_for)` whose predicate is the reservation arm's static conjuncts; it makes the probe an index probe and **enforces nothing**, so — unlike 0004/0013 — **no production reservation is cancelled to install it**. The active-instant arm reuses `uq_go_ride_requests_active_instant_vehicle` (0013), which is already exactly that predicate. **The probe's range conjuncts must stay written against the BARE `scheduled_for` column** (`scheduled_for > $2 − W AND scheduled_for < $2 + W`, with the half-width on the constant side) — moving `W` onto the anchor is algebraically identical and costs the index: Postgres cannot match an expression to a plain btree column, the range degrades from Index Cond to a heap Filter, and the probe reads every open reservation on the car while holding the booking lock. `internal/store/ride_request_window_index_plan_test.go` EXPLAINs the real statement and fails if that happens.

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

**Party-aware** (MYR-522 — client decision 2026-08-12; rider-only before it). Which party is calling decides the legality window, and both windows are enforced by the same guarded single-statement UPDATE:

| Caller | Legal from | Refusals |
|--------|-----------|----------|
| Rider (including a self-ride owner) | `requested`, `accepted` | any other status → `409 conflict` |
| Owner | `accepted`, `arrived` | `requested` → `409 conflict` naming **decline** (that moment's owner verb); `enroute` → `409 conflict` naming **dropped-off** (the rider is aboard); any other status → `409 conflict` |

The winning write also stamps **`cancelledBy`** (`"rider"` \| `"owner"`) into the same UPDATE, first writer wins — so who cancelled is arbitrated by exactly the race discipline that decides whether the cancel wins. The stamp is surfaced on the party-only `RideRequest` payload as optional `cancelledBy`, and consumers MUST treat absence (any row cancelled before the field existed) as "initiator unknown", never guess. Responds `200 OK` with the updated `RideRequest` and unicasts `ride_status_changed` through the same delivery every other transition uses (nav stand-down and Live Activity teardown ride the same event).

**Push (MYR-522):** an OWNER's cancel notifies the RIDER — `{vehicle nickname} had to cancel your ride` (scheduled variant: `… your scheduled ride`, naming no time per the standing rule), body `Try booking another car.` — under the `ride_lifecycle` category. The rider's own cancel, and any event with no `cancelledBy`, stays silent, which keeps every pre-MYR-522 publisher behaving exactly as before. The owner's name never travels on a lock-screen surface (payload policy).

Errors: `401 auth_failed`, `404 not_found` (unknown id / non-party), `409 conflict` (illegal transition / wrong door, see table), `500 internal_error`. The pre-MYR-522 `403 permission_denied` for an owner cancel is RETIRED.

#### Owner-driven dispatch handshake (MYR-270)

The autonomous ride runs as a **two-leg, owner-driven handshake** over the existing `RideRequestStatus` enum (no new status values). MYR-270 **supersedes the MYR-265 model** (the rider `board` endpoint and the drive-end `enroute → completed` auto-completion are **retired**). The four steps:

| Step | Endpoint | Actor | Transition | Nav effect |
|------|----------|-------|-----------|-----------|
| Accept | `POST .../accept` | Owner | `requested → accepted` | pushes the **pickup** nav (MYR-176) |
| Picked up | `POST .../picked-up` | **Owner** | `accepted → arrived` | none |
| Start | `POST .../start` | **Rider** | `arrived → enroute` | pushes the **leg-2** nav — the trip's FIRST STOP when it has stops ([MYR-539](https://linear.app/myrobotaxi/issue/MYR-539)), the **dropoff** otherwise |
| Dropped off | `POST .../dropped-off` | **Owner** | `enroute → completed` | none |

Meaning of each state: `accepted` = leg 1, car en route to the **pickup**; `arrived` = rider picked up, awaiting the rider's start; `enroute` = leg 2, car en route to the **dropoff**; `completed` = dropped off. Every successful transition unicasts a `ride_status_changed` summary to both parties (websocket-protocol.md §4.8). All three new endpoints are guarded (`UpdateStatusFrom` `WHERE status = ANY(<legal-from>)`) and **idempotent** — a repeat of the destination state is a `200 OK` no-op that re-returns the current `RideRequest` and re-publishes nothing; every other current status is `409 conflict`.

**Pickup carries one precondition beyond its from-status ([MYR-376](https://linear.app/myrobotaxi/issue/MYR-376)).** For a **SCHEDULED** ride, `accepted → arrived` is additionally refused `409 conflict` while the reservation is **dormant** — neither dispatched nor yet due. A reservation sits in `accepted` from the moment the owner confirms it until `scheduledFor`, and until then the car has been told nothing, so there is no pickup to confirm. **INSTANT rides are unaffected.** The full rule, and why it lifts at the due instant rather than at a successful dispatch, is under `POST .../picked-up` below; the transition matrix cell is annotated accordingly.

#### `POST /api/ride-requests/{id}/picked-up`

**Owner-only** (the rider is a party but cannot confirm → `403 permission_denied`; a non-party → `404`). The owner tapping "Picked up" flips `accepted → arrived` (rider is aboard, awaiting the rider's start). **No Tesla nav is pushed.** An optional `picked_up_at` audit timestamp is stamped server-side first-entry-only (P0, off-wire — data-classification.md §1.9), mirroring `accepted_at`/`completed_at`.

Legal only from `accepted → arrived`. Responds `200 OK` with the updated `RideRequest` (now `arrived`) and unicasts `ride_status_changed`. **Idempotency:** already-`arrived` is a `200 OK` no-op; every other status is `409 conflict`. Errors: `401 auth_failed`, `403 permission_denied` (rider/non-owner party), `404 not_found` (unknown id / non-party), `409 conflict` (illegal from-status, **or a dormant reservation — see below**), `500 internal_error`.

##### Reservation dormancy — a reservation cannot be picked up before its dispatch ([MYR-376](https://linear.app/myrobotaxi/issue/MYR-376))

A **SCHEDULED** ride's pickup is refused **`409 conflict`** — the SAME code, message `a scheduled ride cannot be picked up before its dispatch` — while the reservation is **dormant**. The rule, exactly:

> A reservation is **DORMANT** from the moment it is accepted until the **earlier** of (a) its leg-1 dispatch resolving `sent` and (b) its `scheduledFor` instant arriving. `accepted → arrived` is refused for the whole of that interval and legal outside it.
>
> Equivalently, the pickup requires `scheduledFor` **absent** OR `dispatchStatus == "sent"` OR `scheduledFor <= now()`.

**The defect this closes.** MYR-179 moved a reservation's pickup nav push from accept time to `scheduledFor`, which left a reservation sitting in `accepted` — indistinguishable, to the lifecycle, from an instant ride whose car is already rolling. In production an owner accepted a ride due the **next day** and immediately tapped "Picked up". The ride reached `arrived` with `dispatchStatus` still absent, the car never dispatched and in service; from `arrived` the rider's `start` would have pushed a real **dropoff** nav to that vehicle, and the ride had no legal exit (the rider's cancel is illegal from `arrived`, per the matrix below; the owner's `arrived` cancel arrived later, with MYR-522). The gate makes "picked up" mean what it says: the car was actually sent.

**Why dormancy ends at the DUE INSTANT and not at a successful dispatch.** Requiring `dispatchStatus == "sent"` would contradict the reservation-expiry contract stated in the transition-matrix notes below and under "Reservation-time dispatch": a reservation the sweeper failed (`failed` / `reservation_expired`), skipped (kill-switch), or never reached stays `accepted`, and its parties "may still **cancel or proceed manually**". Under a `sent`-only gate every such ride would have **cancel as its only exit** — the same stranding the defect produced, arrived at from the other direction. Past `scheduledFor`, the owner is at the car and the ride is owed: a manual pickup **is** the documented recovery path, dispatch or no dispatch. What is refused is strictly the **pre-due** pickup, which is the defect and nothing more.

- **INSTANT rides are entirely unaffected**, including ones whose nav push resolved `failed` or `skipped`. They carry no `scheduledFor`, the car is already at the kerb, and gating their pickup on a dispatch outcome would break a working ride to punish a failed notification.
- **A dispatched reservation is live whatever the clock says.** `dispatchStatus == "sent"` carries the pickup on its own, so an accept-after-due reservation swept early (see "Accept-after-due semantics") is pickup-able immediately.
- **The comparison is inclusive** (`scheduledFor <= now()`): a ride due this second is already out of dormancy.
- **The refusal is a DEFERRAL, not a dead end.** The identical request succeeds once either arm opens; no client state needs clearing and nothing about the row changes on a refusal (no `picked_up_at`, no `ride_status_changed`, no status move).
- **No new error code.** The dormancy refusal and the illegal-from-status refusal share `conflict`; only the message distinguishes them, so a client switching on `code` needs no change. A client MAY surface the message verbatim.
- **Enforced inside the guarded write, never as a pre-check.** The predicate rides the same single `UPDATE` as the `WHERE status = ANY(<legal-from>)` guard (`store.RideRequestRepo.UpdateStatusFromDispatched`), so a pickup tapped while the sweeper is dispatching — or while the clock crosses `scheduledFor` — is arbitrated by the database on exactly the terms every other transition race is. **No pickup can commit against a row that is dormant at the instant of the write**, however stale the caller's read was; the invariant `status = 'arrived'` ⇒ the reservation was live is guaranteed by construction rather than by ordering.
- **A confirmed reschedule that moves the instant later re-imposes dormancy**, correctly — the ride is genuinely due later now. (`scheduledFor`'s *presence* remains immutable for a ride's lifetime; only its value moves.)
- **`start` needs no gate of its own.** `arrived → enroute` is already legal only from `arrived`, which a dormant reservation can no longer reach.

##### Auto-arrival — the server confirms the pickup when the car is observed at it ([MYR-538](https://linear.app/myrobotaxi/issue/MYR-538), CLIENT-DIRECTED)

> Client: *"If car reaches destination you shouldn't require owner to mark as arrived we should be able to figure it out."*

`accepted → arrived` has a **second writer**. Alongside the owner's tap, the server advances the ride itself once the car's own telemetry shows it has reached the pickup. **No REST surface changes**: no new endpoint, no new status, no new error code, no field added to any payload, no contracts version bump. What changes is that the transition can now happen without a request — and a client that already handles `ride_status_changed` handles it with no change at all.

**The rule.** For a ride that is `accepted` with leg-1 `dispatchStatus == "sent"`, the server marks it `arrived` when its vehicle is:

> within **~80 m** of the ride's pickup coordinate (haversine), **stopped** (reported speed ≤ 1 mph, or gear `P` when the frame carries no speed), continuously, for **~20 s**.

All three are required and the dwell is measured on the **frame's own timestamp**, so both telemetry sources count equally — the live stream at ~1 s and the MYR-394 REST position poll at ~25 s (which is the only source a non-streaming car has, and therefore the only way this can work for one).

**It is an additional writer through the SAME guard, not a parallel path.** The detector calls `store.RideRequestRepo.UpdateStatusFromDispatched` with the same allowed-from set (`accepted`) the `picked-up` endpoint passes — the identical dormancy-guarded single `UPDATE`. Everything the §7.8 race semantics promise therefore holds unchanged:

- **The owner's tap stays, and wins ties.** Detector and tap contend on one statement; the database picks a winner. If the owner taps first the detector's write matches no row and it **no-ops silently** — no retry, no event, nothing logged above debug. If the detector wins first, the owner's tap finds the ride already `arrived` and gets its usual idempotent `200 OK` no-op. Neither party can observe a difference in the endpoint's contract.
- **The published events are byte-identical to the tap's.** The winning write publishes the same summary `ride_status_changed` (`status: "arrived"`), so the rider's push, the Live Activity phase and the WS frame fire exactly as they always have. An additional **internal-only** `ride.waypoint_arrived` seam is published alongside it, carrying ids and the waypoint label `"pickup"` — never broadcast, never on the wire. It exists because "the ride was picked up" and "the car is physically at the kerb" are different facts and only the second one may drive the car.
- **The arrival also flashes the lights ×3** ([MYR-542](https://linear.app/myrobotaxi/issue/MYR-542), CLIENT-DECIDED). The seam's first and only subscriber sends Tesla's signed `flash_lights` three times, ~1.5 s apart (no horn), so the rider can pick the car out at the kerb. **No REST surface, no wire change, nothing a client can observe or request** — it is a server-side gesture at the car, and the owner's manual tap cannot trigger it, because the tap does not publish this seam. Bounded and silent: at most three commands per waypoint, a failed flash is never retried and abandons the remainder, and every failure is logged only — a missed courtesy is not actionable by any party and is never surfaced to one. Gated by `ARRIVAL_FLASH_ENABLED` (default **true**), separate from `AUTO_ARRIVAL_ENABLED` so headlights can be silenced without giving up arrival detection.
- **Dormancy is unaffected.** A candidate must already have `dispatchStatus == "sent"`, which is one of the dormancy predicate's own escape hatches, so auto-arrival can never advance a ride the MYR-376 gate would refuse.

**Deliberately conservative, because the two errors are not symmetric.** A false arrival wakes the rider with "Your car is here", waves their Live Activity to the arrived phase, and **cannot be undone** — the lifecycle has no `arrived → accepted` edge. A late arrival costs nothing: the owner's tap is still there and the rider is standing next to the car. So the detector fails towards *doing nothing* in every ambiguous case: an interrupted dwell **resets** rather than resumes, a frame with no evidence of stillness is not counted, a vehicle carrying two live leg-1 rides is skipped rather than guessed at, and a candidate set the server cannot refresh goes stale and then **empty** — detection suspends rather than running on possibly-cancelled rides or possibly-edited pickups.

**The car's own `milesToArrival` is corroboration only, and never a trigger.** It is the dash's distance to whatever the dash currently believes its destination is, and [MYR-527](https://linear.app/myrobotaxi/issue/MYR-527) is the standing proof those can differ: a rider spent three hours on a trip whose car was still navigating to the pickup, with the app faithfully rendering the dash's "4 min away". Position-vs-pickup is the only input to the decision; the car's number is written to the arrival log line for whoever reads it later and read by nothing else.

**Operational.** Gated by `AUTO_ARRIVAL_ENABLED` (default **true**; anything `strconv.ParseBool` rejects fails the whole config load). False means the detector is not constructed at all — no telemetry subscription, no candidate reads — and `accepted → arrived` has exactly one writer again, which is the pre-MYR-538 behaviour. Nothing queues while it is off and nothing replays when it returns, so it is safe to throw mid-ride. Candidates come from one lean indexed read (`ListArrivalCandidates`, served by `idx_go_ride_requests_active_poll` from migration 0028 — **no new index, no migration**) cached ~15 s; there is no per-frame query.

#### `POST /api/ride-requests/{id}/start`

**Rider-only** (the owner is a party but cannot start → `403 permission_denied`; a non-party → `404`). This is the **leg-2 trigger**: the rider tapping "Start ride" flips `arrived → enroute` (car en route to the **dropoff**). The rider **cannot** start before the owner confirms pickup — start is legal **only from `arrived`**, so a still-`accepted` ride is a `409 conflict`.

On success the endpoint publishes an internal `ride.started` event carrying the **dropoff** place — the input the nav-dispatch pipeline subscribes to for the Tesla `navigation_request` share push (the car's new destination). Like `ride.accepted` this event is **internal-only**: it never reaches the WS broadcast path, and no Tesla call happens synchronously on this endpoint (the push runs asynchronously off the bus).

Legal only from `arrived → enroute`. Responds `200 OK` with the updated `RideRequest` (now `enroute`) and unicasts `ride_status_changed` to both parties. **Idempotency:** a start when the ride is **already `enroute`** is a `200 OK` no-op that re-returns the current `RideRequest` and does **NOT** re-publish the dropoff dispatch seam (so the dropoff nav is never pushed twice by a client retry). Every other current status — including `accepted` — is `409 conflict`. Under a concurrent double-tap the guarded `UpdateStatusFrom` (`WHERE status = 'arrived'`) lets exactly one write win — only the winner publishes the `ride.started` seam, so **the dropoff nav push is exactly-once per start** (the accept→`ride.accepted` discipline, applied to leg 2).

Errors: `401 auth_failed`, `403 permission_denied` (owner/non-rider party), `404 not_found` (unknown id / non-party), `409 conflict` (not startable from the current status), `500 internal_error`.

##### Leg-2 dispatch outcome (MYR-270)

The dropoff push outcome is recorded on its **own** triple of columns — `dropoff_dispatch_status` / `dropoff_dispatched_at` / `dropoff_dispatch_error` (migration 0007) — a mirror of the leg-1 (`dispatch_*`) columns so the two legs' histories never clobber each other (a failed dropoff push must not erase the record that the pickup push succeeded). Exactly-once is enforced by BOTH the start transition guard (only the winning `arrived → enroute` write publishes the seam) and, belt-and-suspenders, the `dropoff_dispatched_at` claim latch (a re-delivered `ride.started` finds it set and skips). The value sets, retry policy, and internal reason codes are identical to the leg-1 "Dispatch outcome (MYR-176)" contract above. **These leg-2 columns are P0 and NOT exposed on the wire** (data-classification.md §1.9); there is no new WS frame for the dropoff dispatch — the client-visible start signal is the `enroute` `ride_status_changed`. **Interrupted-dispatch reconciliation is leg-1-only in v1:** a crash in the dropoff claim→record window leaves `dropoff_dispatched_at` set / `dropoff_dispatch_status` NULL; impact is low and a follow-up can extend the startup reconciler if leg-2 interruptions prove material.

#### `POST /api/ride-requests/{id}/dropped-off`

**Owner-only** (the rider is a party but cannot confirm → `403 permission_denied`; a non-party → `404`). The owner tapping "Dropped off" flips `enroute → completed` (ride finished). **Completion is owner-confirmed** — MYR-270 removed the MYR-265 drive-end auto-completion, so a car parking no longer closes a ride. **No Tesla nav is pushed.** `completed_at` is stamped server-side first-entry-only.

Legal only from `enroute → completed`. Responds `200 OK` with the updated `RideRequest` (now `completed`) and unicasts `ride_status_changed`. **Idempotency:** already-`completed` is a `200 OK` no-op; every other status is `409 conflict`. Errors: `401 auth_failed`, `403 permission_denied` (rider/non-owner party), `404 not_found` (unknown id / non-party), `409 conflict`, `500 internal_error`.

#### `GET /api/ride-requests/incoming`

The owner's feed of **open** requests across their vehicles — status `requested` only, covering BOTH the on-demand and scheduled variants (a scheduled request is `requested` with `scheduledFor` set, not a separate status; the owner sheet forks on `scheduledFor` presence). Newest first, cursor-paginated per §4.2 with the same `RideRequestsListResponse` envelope and `(createdAt, id)` cursor as the rider list. Decided rows (accepted/declined/…) leave the feed by construction.

Errors: `400 invalid_request` (`limit` out of range / malformed `cursor` / malformed `upcomingForVehicle` / malformed `activeForVehicle` / both vehicle filters at once), `401 auth_failed`, `500 internal_error`.

> **Routing note:** the literal `/incoming` segment takes precedence over the `GET /api/ride-requests/{id}` wildcard in Go's `ServeMux`, so both routes coexist; a regression test pins this.

##### `?upcomingForVehicle={vehicleId}` — the owner's confirmed reservations for one car (MYR-360)

**ONE optional query parameter, no new endpoint and no new route.** When present it selects a **different slice of this same owner-scoped feed** — not a different resource:

| Facet | Default feed | `upcomingForVehicle={vehicleId}` |
|-------|--------------|----------------------------------|
| Owner scope | `owner_id` = JWT sub | **unchanged** — `owner_id` = JWT sub |
| Vehicle | *(none — all the owner's cars)* | `vehicle_id = {vehicleId}` |
| Status | `requested` | `accepted` |
| Reservation | either variant | `scheduled_for IS NOT NULL AND scheduled_for > now()` |
| Order | `createdAt DESC, id DESC` | **`scheduledFor ASC, id ASC` — soonest first** |
| Cursor | `(createdAt, id)`, descending | `(scheduledFor, id)`, **ascending** |
| Envelope | `RideRequestsListResponse` | **unchanged** — same envelope, same `limit` default 20 / max 100 |

**Why this question needed a new slice at all.** The default feed cannot answer it and never could: it is hardcoded to status `requested` — decided rows leave the feed by construction, as stated above — and it has no vehicle filter. `GET /api/ride-requests` is the *rider's* own list, not the owner's. The client asks this immediately before an owner pauses ride sharing for a car (§7.18), to warn them which **confirmed** reservations the pause would silently break, and to offer an immediate decline (`POST /api/ride-requests/{id}/decline`, extended by the same story).

**`scheduled_for > now()` is STRICT, on purpose.** A reservation already due is not something the owner can still spare the rider from — the reservation sweeper owns it from that instant, and the hold-then-expire backstop is what resolves it. Warning an owner about a reservation they can no longer get ahead of would be noise.

**The soonest-first ordering is load-bearing, not cosmetic.** It is a deliberate departure from the feed's `createdAt DESC` default because the dialog this feeds names "the **next** reservation": under `createdAt DESC` a paginated cut could omit the soonest one entirely (a reservation booked long ago for tomorrow sorts last). The cursor therefore anchors on `(scheduledFor, id)` and resumes with `(scheduled_for, id) > (:cursorScheduledFor, :cursorId)` — the same Postgres row-value keyset discipline as every other cursor here, just ascending (§4.2.2). **The opaque wire cursor format is unchanged**: still one base64 `{timestamp, id}` pair, so a cursor is still opaque and there is still exactly one encoding to reason about.

**`requesterName` is resolved exactly as on every other list item** — the same in-statement correlated subselect (see "Requester name" above), because the dialog shows the rider's first name.

**An unknown or unowned `vehicleId` returns an EMPTY page — `200` with `items: []` — NEVER a `403` or `404`.** The vehicle filter runs on the owner's *own* feed, so it needs no separate ownership check to be safe: a car the caller does not own simply matches no rows. But it must not become an **existence oracle** either — a `404` for "no such vehicle" versus a `200` for "yours, but nothing booked" would let any authenticated caller probe for other people's cars. Both answer `200 { "items": [] }`, indistinguishably. A **malformed** value (empty, whitespace-only, or over 64 characters) is `400 invalid_request`, exactly like a bad `limit`/`cursor`.

**Absent the parameter this endpoint is byte-identical to before MYR-360** — same `requested` filter, same `createdAt DESC` ordering, same bytes. A golden-body test pins it.

##### `?activeForVehicle={vehicleId}` — the ride the car is serving RIGHT NOW ([MYR-400](https://linear.app/myrobotaxi/issue/MYR-400))

**A SECOND optional query parameter on the same feed, following the grammar `upcomingForVehicle` established — no new endpoint and no new route.** It answers the one question no owner surface could: *what ride is my vehicle serving right now?*

| Facet | Default feed | `upcomingForVehicle={vehicleId}` | `activeForVehicle={vehicleId}` |
|-------|--------------|----------------------------------|--------------------------------|
| Owner scope | `owner_id` = JWT sub | **unchanged** | **unchanged** — `owner_id` = JWT sub |
| Vehicle | *(none)* | `vehicle_id = {vehicleId}` | `vehicle_id = {vehicleId}` |
| Status | `requested` | `accepted` | `accepted` / `arrived` / `enroute` |
| Reservation | either variant | `scheduled_for IS NOT NULL AND scheduled_for > now()` | **not dormant** — see below |
| Order | `createdAt DESC, id DESC` | `scheduledFor ASC, id ASC` | **`createdAt DESC, id DESC` — unchanged from the default** |
| Cursor | `(createdAt, id)`, descending | `(scheduledFor, id)`, ascending | **`(createdAt, id)`, descending — the ordinary one** |
| Envelope | `RideRequestsListResponse` | **unchanged** | **unchanged** — same envelope, same `limit` default 20 / max 100 |

**Why the gap existed, and what it cost.** Both existing owner surfaces are about rides that are *not* live: the default feed is `requested` only (decided rows leave it by construction, as stated above), and `upcomingForVehicle` is `accepted` **and strictly future**. A ride in flight fell between them. iOS worked around it with a **device-persisted ride-id pointer** plus the party-only `GET /api/ride-requests/{id}` ([MYR-396](https://linear.app/myrobotaxi/issue/MYR-396)) — but a pointer only remembers what *that device* did. A ride accepted on a **second device**, or before a reinstall, was unrecoverable, and a `ride_status_changed` frame about a ride the client did not hold had to be **dropped**, because there was no read that could fetch it. With real beta testers on more than one device that is a routine, not an edge case.

**The predicate: a COMMITTED status, minus a DORMANT reservation.**

> `status IN ('arrived','enroute') OR (status = 'accepted' AND (scheduled_for IS NULL OR dispatch_status = 'sent' OR scheduled_for <= NOW()))`

- **The committed set is `accepted`/`arrived`/`enroute`** — deliberately the same three the per-vehicle one-active-ride index `uq_go_ride_requests_active_instant_vehicle` (migration 0013) is partial over, because those are exactly the states in which a car is *promised* to a ride. `requested` is excluded (the owner has not committed the car, and many riders may hold pending requests to one car — that is the default feed); terminal states fall out because the car is free again. A new committed state would belong in this predicate, in index 0013 and in the transition matrix **together**.
- **Dormancy is [MYR-376](https://linear.app/myrobotaxi/issue/MYR-376)'s predicate reused VERBATIM**, not restated — this is its third caller, after the pickup transition and the Live Activity progress gate (`store.rideNotDormantPredicate`). Two spellings of "is this ride live" would be two predicates that **drift silently**: each would keep passing its own tests while disagreeing about whether a car is on its way. So an accepted reservation enters this slice at exactly the instant it becomes pickup-able, and not one tick earlier — including the dispatch arm, so **a reservation the sweeper already dispatched is live whatever the clock says**, matching what the pickup gate already promises.
- **Dormancy gates `accepted` ONLY.** A ride that has been picked up is live *by definition* — a rider is in the car. Dormancy asks a question about a reservation that has not started, so it has no meaning past `arrived`, and applying it there would be actively harmful: a confirmed reschedule can move `scheduledFor` later, and a uniform predicate would then drop a ride with a passenger aboard out of the one read that exists to recover it.

**`active` and `upcoming` are COMPLEMENTARY.** A future, dormant reservation is in `upcoming` and **not** in `active`; the same reservation moves into `active` once it goes live. That boundary is the point of having two params rather than one fuzzy "for this vehicle" filter.

**They are NOT disjoint by construction, and this document does not claim they are.** The overlap set is exactly `status = 'accepted' AND scheduled_for > now() AND dispatch_status = 'sent'` — a reservation the sweeper has **dispatched** whose instant is **still future**, which satisfies `upcoming` (accepted + future) and `active` (the dormancy dispatch arm) at the same time. **Today that set is empty in production**, but by a property of the *sweeper*, not of these predicates: the sweep only claims `scheduled_for <= now()`, so nothing can be dispatched and future at once, and the only row of this shape in the test suite is planted with raw SQL. **It becomes reachable when [MYR-192](https://linear.app/myrobotaxi/issue/MYR-192) (reschedule) ships**, because resolve-reschedule moves `scheduled_for` forward without clearing the dispatch columns.

> **Requirement on MYR-192.** Resolve-reschedule MUST clear `dispatch_status` and `dispatched_at` when it moves the instant. This is not tidiness: §7.8 already promises that a confirmed reschedule moving the instant later **re-imposes dormancy**, and while the dispatch columns survive the move the `dispatch_status = 'sent'` arm *defeats* that promise — the ride would stay pickup-able, and `active`, for a reservation that is days out again. Clearing them restores both the documented dormancy and the disjointness of these two slices.

**Asking for both params at once is `400 invalid_request`.** The refusal does **not** rest on disjointness: the two params name different predicates, different orderings **and** different cursor anchor columns, so no single page can honour both requests. A precedence rule would silently answer a question the client did not ask.

**It returns a LIST, and the list can legitimately hold more than one row.** Index 0013 guarantees at most one active *instant* ride per vehicle, but it is **partial on `scheduled_for IS NULL`**, so due reservations are exempt from it: a reservation the sweeper is holding (car busy, owner paused, grant withdrawn, or — since [MYR-372](https://linear.app/myrobotaxi/issue/MYR-372) — the car **in service** at dispatch time) stays `accepted` past its due instant and is live by the manual-pickup contract even while an instant ride occupies the car. The common case is 0 or 1 rows; the pagination is not decoration. Clients should treat "more than one" as reachable rather than as corruption.

**A reservation the sweeper EXPIRED stays in this slice, on purpose.** `reservation_expired` resolves the *dispatch*, not the *ride*: the row stays `accepted` and the transition matrix's notes promise its parties "may still cancel or **proceed manually**". Excluding it would invent a lifecycle policy inside a read — and would hide exactly the kind of orphaned, still-owed ride that has no other owner-scoped surface at all.

**What a client can and cannot tell apart here — stated precisely, because it is narrower than it looks.** The wire projection carries `dispatchStatus` and `dispatchedAt`; it does **not** carry `dispatchError`, which is **server-side only and deliberately absent** from the projection (`RideRequestData.DispatchError` — adding it would be a contract change, not a refactor). So:

| The client sees | It can conclude |
|-----------------|-----------------|
| `dispatchStatus: "sent"` | The car **was** told — the reservation is progressing normally |
| `dispatchStatus` absent | The car has **not** been told yet — undispatched (the sweeper has not claimed it, or is holding it) |
| `dispatchStatus: "failed"` | The car was **not** successfully told. **Ambiguous beyond that** |

That last row is the limit. **A sweeper-EXPIRED reservation and an ordinary nav-push FAILURE on a ride that is still genuinely happening are indistinguishable on the wire today** — both read `status: "accepted"`, `dispatchStatus: "failed"`, `dispatchedAt` set. The discriminator is `dispatchError == "reservation_expired"`, and the server itself must read **both** columns to tell them apart (`isReservationExpired`) precisely because `dispatchStatus` alone cannot. No wire heuristic recovers it: `dispatchedAt` is set in both cases and `scheduledFor` timing does not separate them.

**Consequence for clients:** render `failed` as the honest, unambiguous thing it supports — *accepted, no car currently rolling* — and let the owner decide (proceed manually, or cancel). Do **not** infer "expired" from `dispatchStatus` alone. **Surfacing a derived discriminator on the wire is a contracts change and is deliberately NOT part of MYR-400**; it is a follow-up decision.

**The ordering does NOT depart from the default, unlike `upcomingForVehicle`.** Soonest-first was load-bearing there because that dialog names "the *next* reservation"; here the rows are already live and the ordering only has to be a stable total order for the keyset to be sound. Keeping `createdAt DESC, id DESC` means this slice reuses the **ordinary descending `(createdAt, id)` cursor** and adds **no third cursor type** — §4.2.2's "one encoding, two typed views" is unchanged.

**Authorization is `incoming`'s, exactly.** Bearer, owner-scoped to the JWT subject, which the query string cannot widen — there is no cross-owner read to express and no separate ownership check to forget. **An unknown or unowned `vehicleId` returns an EMPTY page — `200` with `items: []` — NEVER a `403` or `404`**, for the identical existence-oracle reason spelled out for `upcomingForVehicle` above; both params share one validator so they cannot drift on what a well-formed vehicle id is. A **malformed** value (empty, whitespace-only, or over 64 characters) is `400 invalid_request`.

**`requesterName` is resolved exactly as on every other list item**, and **absent both parameters the endpoint is still byte-identical** — the golden-body test that pinned MYR-360 pins MYR-400 too.

#### `POST /api/ride-requests/{id}/accept`

Owner-only. Legal only from `requested` → `accepted`; any other current status is `409 conflict`. Responds `200 OK` with the updated `RideRequest` (now carrying `acceptedAt`, stamped first-entry-only by the store) and unicasts `ride_status_changed` to both parties.

**Vehicle-availability gate (MYR-277; scheduled exempt, MYR-313).** An **instant** accept (`scheduledFor` absent) is additionally refused **`409 vehicle_unavailable`** when the ride's target vehicle cannot currently be dispatched — its persisted status is `in_service` (the owner put it into service) or `offline` (unreachable). This is a **capability gate distinct from the lifecycle `409 conflict`**: the ride is still legally `requested`, but the vehicle can't fulfill it. `parked`/`driving`/`charging` are dispatchable and accept normally. The gate reads the **current** persisted status at accept time (via the same vehicle read the create path uses), so a status flip between the incoming feed and the accept is honored, and a check-then-accept race resolves against the freshly-read status. On a blocked accept **no status transition happens** and **neither the `ride_status_changed` frame nor the `ride.accepted` dispatch seam fires**. If the vehicle's status cannot be read the accept **fails closed** with `500 internal_error` (the server never accepts a ride it cannot confirm the vehicle can serve). **Decline is never gated** — an owner may always decline regardless of vehicle status.

**Scheduled accepts are EXEMPT from the availability GATE (MYR-313) — but the vehicle IS read.** The exemption is about the `in_service`/`offline` *verdict*, not about the read: since MYR-316 the service-window bound needs the vehicle's current `serviceEstimatedEndAt`, so a scheduled accept performs the same lookup an instant one does, and since **MYR-372 that read FAILS CLOSED** (`500 internal_error`, ride not accepted; a vehicle that no longer exists is `409 vehicle_unavailable`). The paragraph below and the ⚠ note under the service-window bound carry the full argument. What the exemption still means: "can this car be dispatched right now?" is only the question an instant accept is asking; a reservation days out says nothing about the car's status today, and refusing it left owners unable to confirm reservations for a car that was in service that afternoon. This makes the gate consistent with the two guards it is the analogue of — the per-rider `ride_active` index and the per-vehicle one-active-ride index (`uq_go_ride_requests_active_instant_vehicle`), both partial on `scheduled_for IS NULL` — and with §4.1.1's statement that scheduled rides are exempt from both. **Availability at the reservation instant is still required**, but that is the scheduled-dispatch machinery's precondition (it must re-check the vehicle when the reservation comes due); accepting a reservation is not dispatching it. **Since MYR-372 the sweeper actually does this** — see the availability bullet under "Reservation-time dispatch" below. Until then the exemption was a promise nobody kept: a car that entered service between the accept and the due instant was nav-dialled anyway. The exemption is a **DEFERRAL of the question, not a waiver of it.**

**Service-window bound on `scheduledFor` (MYR-316).** A **scheduled** accept is refused **`400 invalid_request`** — deliberately NOT a new error code and NOT a `409` — when the ride's `scheduledFor` is EARLIER than the target vehicle's current `serviceEstimatedEndAt`, with the message `scheduledFor must not be earlier than the vehicle's estimated service end (<RFC3339>)`. The same bound is enforced on **create** (`POST /api/ride-requests`), so a reservation cannot be booked past the guard and then confirmed through it, and the resolved value is computed by the same `internal/telemetry/service_window.go` helper that §7.0 / §7.1 emit from — one definition of the window, three readers, no drift. This is a **request-validity** failure, not a lifecycle or capability conflict: the rider asked for a time the car provably cannot serve, and the honest fix is to pick a later time, which is what a `400` tells a client to do. Three rules, all load-bearing:

- **A null estimate means NO BOUND.** When `serviceEstimatedEndAt` is null — the vehicle is not `in_service`, or it is but Tesla returned the all-null `service_data` body a visit with no appointment record produces — scheduling stays **fully open**. Missing data never narrows what a rider may book; the server must no more block on absence than the picker must.
- **EQUAL is ALLOWED.** The bound is strictly "earlier than", so a `scheduledFor` exactly at `serviceEstimatedEndAt` passes. A rider booking the car for the minute it is due back is asking a legitimate question, and an off-by-one that rejected it would be indistinguishable from a broken estimate.
- **INSTANT rides are unaffected.** They carry no `scheduledFor` to compare, and they are already gated by MYR-277's in-service `409 vehicle_unavailable` — a car in the shop cannot be dispatched right now, which is the instant path's own question.

**Consequence for MYR-313: a scheduled accept DOES read the vehicle.** MYR-313 short-circuited the availability gate *before* the vehicle read; the bound needs the vehicle's current `serviceEstimatedEndAt`, so the read happens again. **MYR-313's exemption from the AVAILABILITY gate is unchanged**: an `in_service`/`offline` scheduled accept still succeeds, because "can this car be dispatched right now?" remains the wrong question for a reservation. The two guards ask different things — the availability gate asks about *now*, the service-window bound asks whether the requested *future* instant clears a known service end.

> **⚠ CHANGED in MYR-372 — that read now FAILS CLOSED for scheduled accepts too.** It previously failed open: an unreadable vehicle left the reservation **unbounded** rather than refused, on the argument that refusing it would resurrect the MYR-313 stranding. The two failures are not alike. **MYR-313 was a PERMANENT `409`** — the car was in service, so every retry that day refused and the owner had no way through. **A lookup failure is a TRANSIENT `500`** — the owner retries and it succeeds. Trading one retry for the silent grant of a reservation that may sit squarely inside a service visit is the worse trade, sharply so with external beta riders on the other end and nobody on hand to explain it. **Unknown now blocks, on every path, for every ride**, and the instant and scheduled paths finally agree. Two consequences follow, because the pause check and the grant backstop both ride this same read: an **unknown pause state** and an **unreadable grant** now block a scheduled accept as well. Clients MUST treat a `500` here as retryable — it is a read failure, not a verdict about the car.
>
> **§7.22's advisory read is deliberately NOT affected.** That endpoint answers a *picker*, not a gate; it describes a calendar and refuses nothing, and its client-side fail-open behaviour is documented there and unchanged. Fail-closed is the rule for surfaces that GRANT a ride.

**Per-vehicle ride-window backstop (MYR-383).** A **scheduled** accept is refused **`409 vehicle_unavailable`** with **`subCode: time_conflict`** when the ride's target vehicle is already promised to another **committed** open ride within 45 minutes of this ride's `scheduledFor`. The rule, the two arms, the messages and the race construction are stated once under `POST /api/ride-requests` above — this is the same gate, at its second layer.

Three things are specific to the accept side:

- **`requested` peers do NOT count here.** Unlike create, the accept probe ignores still-undecided reservations, because the owner's accept is *how* a contested slot is decided. Refusing both sides of a collision would leave a legacy double-booking unconfirmable in either direction. The full argument is in the create section.
- **It is not redundant with the create gate.** Two things reach this layer and not the other: reservations booked **before this gate existed** (including the pair that produced the client report), and the **active-instant arm**, which is time-relative — a car idle when a reservation was booked may be mid-ride by the time the owner confirms it.
- **INSTANT accepts are not window-gated in v1a.** They skip the probe entirely, so their behaviour is unchanged; the per-vehicle one-active-ride index (MYR-266) remains their guard. See the remaining-boundary note in the create section.

**How this narrows the MYR-313 exemption — precisely which gate is which.** There are now **three distinct gates** on a scheduled accept, and MYR-313 exempts a reservation from exactly one of them:

| Gate | Question it asks | Scheduled accept |
|------|------------------|------------------|
| **Availability** (MYR-277) — vehicle `in_service`/`offline` | "Can this car be dispatched **right now**?" | **EXEMPT** (MYR-313) — but **DEFERRED, not waived**: the sweeper asks the identical question at the due instant (MYR-372) |
| **Service-window bound** (MYR-316) — `scheduledFor` before `serviceEstimatedEndAt` | "Can the car serve the **requested future instant**?" | **Applies** → `400 invalid_request` |
| **Ride-window conflict** (MYR-383) — this section | "Is the car **already promised** at that instant?" | **Applies** → `409 vehicle_unavailable` / `time_conflict` |

MYR-313's exemption is **not weakened**: an `in_service`/`offline` scheduled accept still succeeds, because "can this car be dispatched right now?" remains the wrong question for a reservation. What the new gate refuses is a different fact entirely — not the car's *condition*, but a *promise the car has already made*, which is as durable as the reservation itself and says nothing about today's status. A reservation for a car that is in service today and already booked at that hour is refused by MYR-383 and exempt from MYR-277 **at the same time**, and both are correct. The pattern is MYR-316's, one step further: MYR-313 exempts reservations from the *status* gate only, and each new gate that asks about the reservation instant applies on its own terms.

**Dispatch seam (MYR-176):** a successful accept also publishes an internal `ride.accepted` event on the process event bus carrying the pickup/dropoff places and the booked-for passenger contact — the input the nav-dispatch pipeline subscribes to for the Tesla `navigation_request` share push. The event is internal-only: it never reaches the WS broadcast path, and no Tesla call happens **synchronously on this endpoint** — the push runs asynchronously off the bus.

**Scheduled accepts DEFER the dispatch to `scheduledFor` (MYR-179).** The `ride.accepted` seam still fires for a reservation, but the nav-dispatch pipeline **does not push** when the ride carries `scheduledFor`: accepting a reservation is not dispatching it, and dialing a rider's pickup into the car hours or days early would strand the wrong destination in its navigation. The pickup push instead fires at the reservation instant, driven by the reservation sweeper (see "Reservation-time dispatch" below). Observable consequence on the wire: after a **scheduled** accept, `dispatchStatus` and `dispatchedAt` stay **absent** — the same pending shape an instant ride has between accept and push resolution — until `scheduledFor` arrives. **Instant accepts are unchanged** (push on accept). Nothing about the *lifecycle* changes: the ride is `accepted` either way, and `dispatchStatus` remains an orthogonal annotation.

Errors: `400 invalid_request` (SCHEDULED accepts only — `scheduledFor` is earlier than the target vehicle's `serviceEstimatedEndAt`, the MYR-316 service-window bound; no bound applies when that value is null, and an equal instant is allowed), `401 auth_failed`, `403 permission_denied` (rider/non-owner party), `403 vehicle_not_owned` (the RIDER on this request no longer holds a grant carrying the ride capability — the MYR-369 accept-time backstop, the second of the three layers in §7.5.7; an unreadable grant fails closed with `500 internal_error` instead), `404 not_found` (unknown id / non-party), `409 conflict` (illegal lifecycle transition), `409 vehicle_unavailable` (INSTANT accepts only — target vehicle `in_service`/`offline`, MYR-277; scheduled accepts are exempt from THAT gate, MYR-313 — **but see the MYR-342 pause below and the MYR-383 window conflict above, neither of which is exempt**; the window conflict is the one carrying `subCode: time_conflict`, and it applies to SCHEDULED accepts only), `500 internal_error`.

**Ride-sharing pause backstop on accept (MYR-342).** An accept is refused **`409 vehicle_unavailable`** with the message `Ride sharing is paused for this vehicle` when the target vehicle's `rideShareEnabled` is `false`. This is the second of three enforcement layers; it exists because the create gate cannot reach backwards to a request that was already outstanding when the owner reached for the switch.

- **It applies to SCHEDULED accepts too, unlike the availability gate beside it** — the same deliberate deviation from MYR-313 argued on create. An owner's pause is open-ended where a service visit is not.
- **The check runs BEFORE the MYR-313 instant-only short-circuit but AFTER the vehicle read.** ⚠ **CHANGED in MYR-372:** that read now fails CLOSED for scheduled accepts, so **an UNKNOWN pause state BLOCKS** with `500 internal_error` where it previously sailed past on the early return. That was never a decision about pauses — it was a side effect of the fail-open shape the check happened to sit behind — and an owner's pause should not be defeatable by a database blip.
- **`decline` is never gated by the pause.** An owner may always decline. Gating it would trap requests on a car its owner has withdrawn — the opposite of what the pause is for.

#### Dispatch outcome (MYR-176)

When an owner accepts, the server asynchronously pushes the rider's **pickup** into the vehicle's Tesla navigation (an unsigned `navigation_request` share whose `value` is the pickup's full-precision `"<lat>,<lon>"` coordinate pair — text the car's nav geocoder resolves — via the §7.9 command Executor). **Why `navigation_request`, not `navigation_gps_request` (MYR-245):** the tesla-http-proxy's `ExtractCommandAction` has no case for `navigation_gps_request`, so the proxy returns HTTP 400 `invalid_command` **locally, before the car is dialed** — the Jul 18 2026 outage. Only `navigation_request` is proxy-forwardable (the proxy returns `ErrCommandUseRESTAPI` and relays it to the Fleet REST API). This does **not** change the main lifecycle status — `accepted` stays `accepted` (the pickup handshake `accepted → arrived → enroute` is owner-driven, MYR-270; see the handshake section above). Instead the outcome is recorded as an **orthogonal, optional annotation** on the `RideRequest`, surfaced on the party-only detail (`GET /api/ride-requests/{id}`):

- **`dispatchStatus`** — `"sent"` (the vehicle accepted the nav push), `"failed"` (terminal error, or exhausted the bounded retry — e.g. `key_not_paired`, `permission_denied`, or transport/asleep after retries), or `"skipped"` (the push was deliberately not made: the `DISPATCH_ENABLED` kill-switch was off, or the leg was **superseded** by a later leg of the same ride — see `nav_superseded` below). Absent until the push resolves.
- **`dispatchedAt`** — RFC 3339 instant the dispatch was attempted (also the exactly-once latch: a re-delivered accept never double-pushes). Absent until claimed.

Both fields are **optional and additive** (older clients ignore them); the internal failure reason code is not exposed on the wire. There is **no** new WS frame for dispatch — clients that render it refetch the REST detail after the `accepted` `ride_status_changed`.

> **Internal failure reason codes (`dispatch_error`, server-side only — NOT on the wire).** When `dispatchStatus` is `"failed"` — or `"skipped"` for the one deliberate-non-push code, `nav_superseded` — the server records an opaque reason code in the `dispatch_error` column (data-classification.md §1.9) for operability. The set is the typed command codes (`key_not_paired`, `permission_denied`, `vehicle_asleep`, `command_failed`, `invalid_request`, `internal_error`) plus dispatch-local codes:
> - `vehicle_unresolved` — the vehicle's VIN could not be resolved (row gone, or transient lookup failure exhausted the bounded retry).
> - `token_expired` — the owner has a Tesla token on file but it is expired and could not be refreshed; **the owner must re-link** their Tesla account. Distinct from `token_unavailable` so this actionable case is not conflated with "never linked".
> - `token_unavailable` — no usable Tesla token could be obtained (account **never linked**, or a transient lookup failure exhausted retries).
> - `transport_unconfigured` — the tesla-http-proxy command transport is not configured; a permanent misconfiguration (not retried).
> - `dispatch_canceled` — the per-event context was canceled/timed out mid-resolution.
> - `dispatch_interrupted` — the dispatch was claimed (`dispatched_at` stamped) but the process died (crash/SIGTERM) before recording an outcome; the startup reconciler resolved the orphaned row (see below).
> - `nav_superseded` (MYR-526) — recorded with `dispatchStatus: "skipped"`, **not** `"failed"`: nothing went wrong, the ride had simply moved on to its next leg. The leg was refused (or stopped mid-flight) because a **later leg of the same ride** had already taken the car's navigation. See "Leg ordering" below.
> - `reservation_expired` (MYR-179) — a **scheduled** ride was still undispatched past its **lateness ceiling** (`scheduledFor + 30 min`), so it was resolved `failed` **without** a nav push. Two causes produce it and the outcome is identical either way, so the code is one generalised value rather than a per-cause family: the vehicle stayed committed to another active ride for the whole window, or dispatch itself was unavailable (downtime, or `RESERVATION_DISPATCH_ENABLED=false`). The distinguishing detail rides the structured log, not the column. Reservation-only: an instant ride can never record it. See "Reservation-time dispatch" below.
>
> Resolution steps (VIN + token) run under the same bounded retry policy as the command: transient failures are retried; only well-identified permanent conditions (`token_expired`, `token_unavailable` on a never-linked account, `vehicle_unresolved` not-found, `transport_unconfigured`) short-circuit.

> **Startup reconciliation of interrupted dispatches.** The `dispatched_at` claim latch is stamped BEFORE the nav push runs, so a crash or SIGTERM in the claim→record window leaves a row with `dispatched_at` set and `dispatch_status` NULL — stuck "claimed but unresolved" forever, invisible to monitoring, and never re-attempted (the exactly-once latch blocks a second claim). On startup the dispatcher runs a one-shot reconciliation pass (`internal/dispatch` `Reconcile`, wired in `cmd/telemetry-server/dispatch_wiring.go`) that finds every such row **older than the per-event OverallTimeout** (so a genuinely in-flight dispatch is never touched) and records it `failed` / `dispatch_interrupted`. **We resolve-as-failed rather than re-dispatch on purpose:** the process died at an unknown point (the push may or may not have reached the car), the accept is likely stale by restart, and a late nav push to a car that has since moved is worse than an honest, alertable "interrupted" outcome. The reconciler is best-effort (log-and-continue; a failure never blocks server startup).

#### Leg ordering: the car's nav destination only moves forwards (MYR-526)

The vehicle's navigation destination is a **single last-write-wins resource**, but a ride's two nav legs arrive as **independent** bus deliveries onto a shared worker pool — leg 1 (pickup) on `ride.accepted`, leg 2 (dropoff) on `ride.started`. Nothing ordered the two Tesla calls, and the claim latches are stamped **before** the resolve-and-command work, so a leg-1 push that stalls (an asleep car costs a wake plus the executor's spaced retries, on top of the dispatcher's own bounded retry) can still be in flight when the rider taps **start**.

That is the MYR-526 production failure (2026-08-09, 12.4 s between accept and start): leg 2's drop-off share reached the car **first**, leg 1's pickup share landed **on top of it**, and the dash sat at the kerb showing "At Destination / End Trip" for the pickup. Both legs recorded `sent`, correctly — Tesla accepted both shares — so the ride row, the app, and the audit line all agreed the drop-off had been pushed while the car disagreed. **No amount of typed errors or command retries would have caught it: there was no error.**

The dispatcher therefore serializes nav pushes **per vehicle** and enforces monotonic leg order **per ride**:

- At most one nav push per vehicle is in flight at a time, so two legs can no longer finish out of order. Different vehicles are unaffected — the gate never serializes the fleet behind one asleep car.
- A **higher** leg of the same ride arriving while a lower one is in flight **takes over**, cancelling it. Leg 2 must be able to overtake a stalled leg 1 (the rider is already moving); it just must not be overtaken back.
- A leg that finds a higher leg of its **own** ride already delivered is refused outright and records `skipped` / `nav_superseded` rather than dialing a stale destination. The reachable case is a reservation whose sweeper claims leg 1 after the ride was picked up past due and already started.

Supersession is keyed by **ride**, never by vehicle: the *next* ride's pickup is a lower leg ordinal than the *previous* ride's drop-off, and refusing it would strand every car pointing at its last drop-off. Ordering is in-process, which is sufficient — both legs of a ride are dispatched by the same server, and a process that dies mid-leg leaves the claimed-but-unresolved shape the startup reconciler already owns.

#### Reservation-time dispatch (MYR-179)

A **scheduled** ride's pickup nav fires at its **leave-time**, not at accept. This changes only **WHEN** the existing leg-1 (pickup) push runs — there is **no new lifecycle status, no new wire field, no wire-shape change, and no migration**. The same three columns carry the outcome (`dispatchStatus` / `dispatchedAt` / `dispatch_error`), and `dispatchedAt` remains the exactly-once latch.

**`scheduledFor` is an ARRIVAL instant, and the leave-time precedes it by a computed dispatch lead ([MYR-535](https://linear.app/myrobotaxi/issue/MYR-535), client decision 2026-08-12).** As shipped by MYR-179 the sweeper dispatched AT `scheduledFor`, so the car was told to leave at the very instant it was booked to arrive — measured in production: scheduled `21:00:00Z`, dispatched `21:00:08Z`, and the client's own words for it, "the owner should leave before 4PM". The lead is

> `lead = clamp(estimated drive time from the car's current position to the pickup + 3 min buffer, 5 min, 30 min)`

and the car is dispatched once `now >= scheduledFor − lead`. The estimate is the platform's one closed form (great-circle × 1.3 route factor ÷ 24 mph — deliberately the same arithmetic as the iOS `TripEstimate`, and the same trade: a city-hop average overstates highway drive time, which errs toward leaving early, bounded by the 30-minute cap, never late). A car with **no usable position** — never streamed, the `(0, 0)` no-fix sentinel, or an unreadable row — falls to the **floor lead** (5 min) rather than holding: the lead only tunes *when* a car that should go actually leaves, so degradations are deliberately asymmetric with the hold-on-error posture of the probes below, which guard whether it may go at all. A candidate already **at or past** `scheduledFor` never consults the position at all — the answer is "now" whatever the estimate says — so a position-read outage can delay a dispatch by at most the lead it would have granted and can never hold one past its pickup instant. **No client change was needed**: reservation dormancy (MYR-376, `POST .../picked-up` above) already lifts on `dispatchStatus == "sent"` — "a dispatched reservation is live whatever the clock says" — so an early-dispatched reservation is immediately pickup-able, which is correct: the car really is rolling.

**The two halves.**

| | Instant ride (`scheduledFor` absent) | Scheduled ride (`scheduledFor` set) |
|---|---|---|
| On accept | Claims the latch, pushes the pickup | **Does nothing** — latch unclaimed, `dispatchStatus` absent |
| Pickup pushed | At accept | At the leave-time (`scheduledFor − lead`, MYR-535), by the reservation sweeper |

Leaving the row **untouched** at accept is load-bearing, not an omission: latch-unclaimed + outcome-absent is precisely the shape the sweeper selects on. The accept deliberately does **not** record `skipped` — `skipped` means "the kill-switch was off and we will never push this ride", which would both misreport a reservation that is going to dispatch on schedule and latch the row out of the sweep.

**The sweeper.** A background ticker (default **30 s**, `internal/dispatch` `ReservationSweeper`, wired in `cmd/telemetry-server/reservation_wiring.go`) **selects** reservations that are `scheduled_for IS NOT NULL AND status = 'accepted' AND dispatched_at IS NULL AND scheduled_for <= now + 30 min` — the **dispatch horizon**, now plus the maximum lead, so a candidate is visible for the whole window it could legally leave in (MYR-535) — oldest reservation first, `LIMIT 25`, then hands each candidate to one of its **own** small pool of workers (2). A candidate still before its computed leave-time is **WAITING**: nothing probed, nothing claimed, nothing logged per row, and the next tick re-decides with the car's newer position — so the leave-time tracks the car right up until it is due to go. Each worker claims the **same** `dispatched_at` latch the instant path uses and then runs the **same** push pipeline (VIN + token resolution, `navigation_request`, bounded retry, outcome record). Reusing both is what gives scheduled dispatch the identical guarantees rather than a parallel set:

- **Exactly-once, across replicas.** The latch admits one winner for the row's lifetime, so every server may sweep concurrently with no coordination: a reservation is pushed once no matter how many sweepers or ticks see it.
- **Just-in-time claiming.** The sweep pass claims **nothing**. The latch is stamped inside the worker, *after* the vehicle-busy re-check and *immediately before* the push, so claim → outcome is bounded by the same per-dispatch `OverallTimeout` (2 min) an instant ride gets. That bound is what the startup reconciler's 5-minute age floor depends on: a reservation can never sit claimed-but-unpushed long enough to be mistaken for an orphan, and a deploy mid-pass hands its unreached candidates back rather than burning them (they are simply re-selected on the next process's first tick). Because nothing is lost by not reaching a candidate, `LIMIT 25` bounds work per tick rather than rationing dispatches.
- **Pool isolation.** The sweeper's worker budget is its own, separate from the dispatcher's instant-dispatch pool. A backlog of reservations whose cars are asleep (each push up to 2 min) can therefore never queue in front of an instant accept's pickup push — **instant-ride dispatch behaviour is unchanged by MYR-179 under any reservation load**.
- **Crash-safe.** A crash between claim and outcome leaves `dispatched_at` set / `dispatch_status` NULL — the identical orphan shape the **existing leg-1 startup reconciler** already resolves to `failed` / `dispatch_interrupted`. That reconciler's query filters on the latch columns only and has never mentioned `scheduled_for`, so scheduled rows were already in scope; **it required no widening**.
- **Kill-switches.** `RESERVATION_DISPATCH_ENABLED=false` stops the sweeper entirely — due reservations stay accepted, unclaimed and outcome-absent, so re-enabling dispatches the ones still inside their lateness window and honestly fails the rest. `DISPATCH_ENABLED=false` still applies to reservations too, recording `skipped` with no Tesla call exactly as for an instant accept.
- **Punctuality.** A reservation whose leave-time falls just after a tick waits at most one sweep interval, so the push lands within ~30 s of `scheduledFor − lead`.
- **Indexed.** Migration 0016 adds `idx_go_ride_requests_reservation_due`, a partial index over `scheduled_for` whose predicate is exactly the query's three static conjuncts, so the every-30s pass stays an index probe as `go_ride_requests` accumulates terminal rows. No schema change — an index only.

**Lateness ceiling (30 min).** A reservation that is still undispatched more than 30 minutes past `scheduledFor` is **expired**, regardless of what the vehicle is doing: `dispatchStatus: "failed"` with `dispatch_error = reservation_expired`, **without** pushing nav. Expiry is evaluated **first**, before every hold probe below — so a long-dead reservation resolves honestly instead of being held forever behind a car that is in service and may never leave. A late nav push dials a car whose rider gave up — the same stance MYR-176 takes for interrupted dispatches ("a late nav push to a car that has since moved is worse than an honest, alertable outcome"), applied to the sweeper's much larger lateness surface: after downtime or a kill-switch window, stale reservations are failed rather than dispatched hours late. Claiming on this path is the point — it converts a silently-pending row into a resolved, alertable outcome on the existing `dispatchStatus` surface and stops the sweeper re-evaluating it forever. The deadline is anchored on `scheduledFor` itself, not on when the sweeper first saw the row, so a restart mid-hold resumes the same deadline rather than resetting it and downtime cannot buy a stale reservation a fresh window.

**Accept-after-due semantics.** `scheduledFor` is not bounded BELOW at create or accept time — only above, by the MYR-372 30-day horizon, which cannot refuse a past instant. A reservation accepted *after* its `scheduledFor` therefore dispatches on the next sweep tick (≤ 30 s later) **provided it is still inside the 30-minute ceiling**; accepted more than 30 minutes past `scheduledFor`, it resolves `failed` / `reservation_expired` on that first tick without a push. Clients that want a just-accepted stale reservation to actually dispatch must reschedule it rather than rely on the accept.

**Dispatch-time hold policy.** Availability is re-checked at the reservation instant — the precondition MYR-313 deferred here when it exempted scheduled accepts from the `vehicle_unavailable` gate. Inside the lateness window, and immediately before claiming, the worker asks **every** "may this car be dispatched right now?" question and holds if any answers no. Chief among them is whether the vehicle is currently committed to an active **instant** ride (`scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')` — character-for-character the `uq_go_ride_requests_active_instant_vehicle` predicate, shared in code with the `hasActiveRide` catalog flag so the two can never disagree):

- **Busy** → **hold**: no claim, no outcome, no push. The row is left exactly as it was and retried on the next tick, so the moment the car finishes its ride the reservation dispatches normally. Re-pointing the navigation of a car that is carrying another rider is the outcome this exists to prevent.
- **Busy state unreadable** (database error) → **hold**, never claim. A held reservation is recoverable next tick; a push that hijacks a live ride is not.
- **Vehicle `in_service`** ([MYR-372](https://linear.app/myrobotaxi/issue/MYR-372)) → **hold**. This is the check MYR-313 promised and nothing performed: a car that entered service in the days between the accept and the due instant was nav-dialled anyway, sending a rider out to meet a car that is on a lift. The status arm comes from the same enumeration the instant-accept gate's `409 vehicle_unavailable` is built from — one definition in the server, so the sweeper cannot decide a car is fine that the accept path would refuse. **Hold, not expire, and that is the whole semantics:** the reservation stays `accepted`, nothing is claimed and nothing is recorded, and the next tick re-decides — so a car back from service *inside* the lateness window still takes the ride a few minutes late. A car that never returns crosses the 30-minute ceiling and resolves honestly as `failed` / `reservation_expired`. **No new wire status was needed** for any of it: `accepted`-and-retrying and `failed`/`reservation_expired` are both already in this section, so a client can render this today with nothing new to learn.

    > **⚠ `offline` is DELIBERATELY NOT a hold, even though accept refuses it.** The asymmetry is load-bearing and reversing it strands cars. `Vehicle.status` is `NOT NULL DEFAULT 'offline'`, the owned-vehicle provisioning INSERT never sets the column, and **nothing writes the value back for a car that has never streamed**: every status writer requires a live telemetry-stream event, the MYR-320 periodic re-poll selects `in_service` rows only, and the §7.15 refresh writes every other field. ([MYR-454](https://linear.app/myrobotaxi/issue/MYR-454) narrowed this claim, which used to read "nothing in this server ever writes the value back": the telemetry fold now heals `offline` → `parked`/`driving` on the first frame that arrives. That **strengthens** the argument below rather than undermining it — the stranded population is now exactly the never-streamed cars, and for those nothing has changed.) A key-paired car that has not yet streamed — every new owner, for their first days — therefore sits at `offline` with nothing scheduled to correct it, so a hold on it would repeat every 30 seconds until the 30-minute ceiling and expire the reservation **every time**. It would also suppress the one thing that clears the condition: nav dispatch never depended on the stream, the command layer classifies offline/asleep alike and runs a **wake ladder** within a bounded budget, and asleep-vehicle pushes are ordinary steady state here. `offline` is consequently left to the dispatcher's existing arbitration — push, wake, and record an honest outcome if the car truly cannot be raised. `in_service` is different precisely because the MYR-320 re-poll maintains it independently, which is what makes waiting on it a bounded, self-resolving wait. **Accept's `409` on `offline` for INSTANT rides is untouched** — a different question, asked with a human waiting on the answer.

- **Vehicle state unreadable** (database error) → **hold**, matching every other probe. We cannot tell whether pushing would dial a car on a lift, and a held reservation is recoverable where a wrong push is not. A vehicle row that cannot be found at all is an error too — never a fabricated "dispatchable" — and the ceiling resolves it.
- **ONE READ serves both the service arm and the pause arm below.** They come from a single joined statement, matching the discipline the create and accept paths already keep (the pause is read off the very row that resolved ownership, never a second lookup), so the two facts cannot disagree and the sweeper costs one round trip per due reservation rather than two.
- **The RIDER no longer holds the ride capability** ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)) → **hold**. The third and last enforcement layer of the per-grant ride flag (§7.5.7), and the only one outside a request: neither the create gate nor the accept backstop can reach a reservation that was already accepted when its owner suspended the grant or switched `allowRides` off, and a reservation may sit `accepted` for days. An owner's own reservation always passes. **An unreadable grant holds**, on the same terms as the probes above. *(This bullet documents behaviour shipped with MYR-369; it was previously described only in the §11 changelog row.)*
- **Ride sharing PAUSED for the vehicle** ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342)) → **hold**, on exactly the same terms. This is the THIRD and last enforcement layer of the owner's pause, and the only one that runs outside a request: the create gate and the accept backstop cannot reach a reservation that was already accepted when the owner reached for the switch, so without this layer a paused car would still be dialled days later. The probe sits BESIDE the busy check and BEFORE the claim, for the reason the claim ordering already gives — the claim is irreversible, so a reservation claimed and then not pushed is burnt permanently, while holding is free. **There is deliberately NO expiry logic of its own:** a reservation whose owner never re-enables the car crosses the 30-minute lateness ceiling above and is resolved honestly as `failed` / `reservation_expired`, the same outcome a permanently-busy car already produces. An owner who un-pauses *inside* the window still gets the dispatch they meant to allow.
- **Pause state unreadable** (database error) → **hold**. Since MYR-372 this is the same condition as "vehicle state unreadable" above — one read serves both arms — and every probe on this path fails CLOSED. So does the accept path's vehicle read now, so the two surfaces no longer disagree about what "unknown" means; the difference is only in the shape of the refusal, a retryable `500` where a human is waiting versus a silent re-decide 30 seconds later where nobody is.
- Held rows are additionally filtered **out of the selection window** in SQL (a `NOT EXISTS` on the busy predicate), so an old reservation waiting on a long trip cannot head-of-line block a younger, dispatchable one out of the `LIMIT`. A row past its lateness ceiling is exempt from that filter and always surfaces — it must be resolvable whatever the car is doing.

**Claim-time re-validation.** The reservation claim re-checks `status = 'accepted'` (and `scheduled_for IS NOT NULL`) in the same guarded `UPDATE` that stamps the latch. A ride cancelled by the rider, or advanced by the owner's picked-up, between the sweep's `SELECT` and the claim therefore **loses**: the claim matches no row and the sweeper does nothing at all — no push, no outcome, no `ride.due`. (The instant path's claim is deliberately left unguarded and byte-identical to MYR-176; its window is milliseconds.)

**The picked-up half of that race survives MYR-376 — and MYR-535 — unchanged.** The dormancy gate (§7.8, `POST .../picked-up`) refuses a pickup while the reservation is neither dispatched nor due. Since MYR-535 the sweeper touches rows up to 30 minutes *before* `scheduledFor`, but the interleaving is still safe from both sides: a pickup tapped before the sweep's claim resolves is refused by the gate's own guarded write (still dormant — not yet dispatched, not yet due), and one tapped after the dispatch resolves `sent` is legal because the gate's dispatch arm has opened — "a dispatched reservation is live whatever the clock says". An owner who advances a ride out from under an in-flight sweep at or past `scheduledFor` is still the intended manual-recovery outcome, and the claim's `status = 'accepted'` re-check is what makes it safe.

**`ride.due` seam.** When a reservation's pickup push actually resolves **`sent`**, the sweeper publishes an internal `ride.due` event (ids + `scheduledFor` + the `dueAt` instant it was picked up for dispatch). Publishing follows the push, not the claim, precisely because the topic's meaning is "your car is on the way": a `skipped` (kill-switch), token-failed or command-failed reservation emits **nothing**, and neither does the expiry path — the car never came. Since the latch admits one winner, a false event could never be corrected by a later one, which is why the seam is pinned to the delivered outcome. It is **internal-only** — never broadcast to WS clients, no REST surface. Its consumer is the push notifier, which fans out to **both parties** (MYR-186 gave the rider "heading your way"; MYR-535 adds the owner's "time to head out" — see the server-side sends table in §7.17). Since MYR-535 `dueAt` ordinarily **precedes** `scheduledFor` by the dispatch lead, which is exactly what makes both pushes true at send time: the car has the route and the pickup is still ahead. The publish is fire-and-forget and drop-safe: a bus failure is logged and the dispatch stands.

**Nav-apply verification ([MYR-527](https://linear.app/myrobotaxi/issue/MYR-527)).** `dispatchStatus: "sent"` records that Tesla's share endpoint answered success — which it does whether or not the car ever applies the destination. First live casualty 2026-08-12: a rider aboard a 3-hour trip whose dash still navigated to the pickup, with the rider surface faithfully rendering the wrong nav (MYR-485 reads the car's own ETA/route by design). The dispatcher therefore closes the loop after every `sent` leg: it watches the CAR'S OWN reported destination (`DestinationLocation`, streamed at 1s/30s-resend and persisted to the vehicle row) converge on the shared target within ~250 m and a 90-second window; if it never does, it **re-shares once** through the nav sequencer (which admits a re-share of the leg's own order and still refuses anything lower — MYR-526's guarantee holds for re-shares by construction), and if it STILL never does, it publishes the internal `ride.nav_unapplied` seam, whose consumer is the OWNER's push — *"«Vehicle» may not have the route / Check the dash and set the destination manually if needed."* **Nothing on the wire changes**: the outcome row keeps `sent` (true — Tesla accepted it), the re-share touches no claim and no column, and the verification is best-effort by construction (only the claim's winner verifies; a process death mid-verification costs a warning, never a dispatch; a leg that was superseded, a ride that moved on, or an unreadable status all abandon silently — the alert fires only for a live leg whose target never appeared in the car's own reporting).

**v1 boundaries.**

- **Reservation ↔ reservation. CLOSED AT BOOKING TIME by [MYR-383](https://linear.app/myrobotaxi/issue/MYR-383)** — the dispatch-side statement below is unchanged and still accurate, but the situation it describes can no longer be *created*. The busy predicate still excludes scheduled rides (mirroring the index), so one reservation coming due still does not mark the car busy for another, and two overlapping reservations would still both dispatch. What changed is that overlapping reservations are now **refused at booking**: a `scheduledFor` within 45 minutes of another open ride on the same car is `409 vehicle_unavailable` / `time_conflict` at **create** and again at **accept** (§7.8 "Per-vehicle ride-window conflict"). This boundary said collision detection "belongs with booking-time conflict detection, not dispatch" — that is exactly where it now lives. **Residual:** pairs booked *before* that gate shipped can still both be `accepted` and would still both dispatch; the accept-side layer catches them only if one is accepted after the other, so a pair already accepted remains a dispatch-time collision. Nothing in the sweeper changed.
- **Instant ↔ reservation (one-directional).** The busy model runs in one direction only. The sweeper refuses to re-point a car that is mid-**instant** ride, but nothing protects a car mid-**reservation** from an instant accept: a dispatched reservation is still `status = 'accepted'` with `scheduled_for` set, so it is outside `activeInstantRidePredicate` — the vehicle reports `hasActiveRide: false`, the MYR-277 accept gate (which reads vehicle status, not rides) passes, and a new instant accept pushes its own pickup over the reservation's destination. The reverse direction — an instant accept landing between the sweeper's busy re-check and its push — is now a **seconds-wide** window (both run adjacent in the same worker) rather than the minutes the pass-start check allowed, but it is not zero: closing it entirely requires the busy predicate to arbitrate the *accept* as well, which is a change to the instant path and out of scope for v1. **PARTIALLY NARROWED by [MYR-383](https://linear.app/myrobotaxi/issue/MYR-383), in one direction only.** A car mid-**instant**-ride now *does* refuse a new **reservation** inside 45 minutes of it (the window gate's active-instant arm, §7.8), so the booking cannot be made in the first place. The reverse — the direction described above, an **instant accept** re-pointing a car that is mid-reservation — is **still open**: the instant accept deliberately skips the window probe in v1a, because gating it is a change to the instant path with its own product questions (an owner standing at the car being told they may not accept a ride now). It remains an accepted boundary, on the same follow-up as the picker annotation.

#### Multi-stop trips ([MYR-539](https://linear.app/myrobotaxi/issue/MYR-539))

**CLIENT-DIRECTED, and the shape is the whole model: exactly ONE pickup → N ordered stops → ONE destination.** The endpoints are NOT members of the list — they stay `RideRequest.pickup` / `RideRequest.dropoff` — so a stop is always the middle of a trip. Contracts **v0.34.0** adds `RideRequest.stops` (`$defs.RideStop`: `place`, `id`, `status`) and `RideRequestTripPatch.stops` (`$defs.RideStopPatch`). Persisted in the Go-owned **`go_ride_stops`** table (migration **0039**), whose coordinates are AES-256-GCM **encrypt-only** exactly like the endpoints' (data-classification.md §1.9) and which **cascades** with its ride so the MYR-447 retention sweep cannot strand P1 location data.

- **OPTIONAL AND ADDITIVE.** `stops` is **omitted** when a trip has none, so every ride written before MYR-539 serializes byte-identically. A consumer MUST read absence (or `[]`) as "no stops", **never** as "unknown".
- **THE ARRAY INDEX IS THE TRAVEL ORDER** — pickup → `stops[0]` → … → dropoff. There is no ordinal field on the wire, so a reorder is just a differently ordered list.
- **`id` AND `status` ARE SERVER-OWNED.** The id is minted on first write and is **stable across reorders and place edits** — it is how a client says "keep this one", and how two people editing at once end up talking about the same rows rather than about list positions. The status (`upcoming` | `current` | `completed`) advances **only** on arrival detection.
- **A STATUS ADVANCE IS NOT A TRIP EDIT.** It does **not** bump `tripVersion`, because the trip's SHAPE did not change; a client reads leg progress off the stop statuses on the refetch the `ride_status_changed` frame already triggers. There is **one** `tripVersion` for the whole trip shape — pickup, drop-off and stops together — not one per part.
- **AT MOST ONE STOP IS `current`.** A ride under way with stops ahead of it always has exactly one, promoted by the same guarded write that made it the car's target: the rider's Start promotes the first, the leg advance promotes the next, and an edit that gives an already-`enroute` ride its first stop promotes it on the spot.

**LEG ADVANCE — how a stop is passed.** The car reaching a stop is observed the same way MYR-538 observes a pickup (`internal/arrival`: parked inside the arrival radius for the dwell window), and the detector's candidate set now includes **`enroute`** rides measured against their **current** stop, or against the destination when none is left ahead. Each arrival publishes the internal `ride.waypoint_arrived` seam with `waypoint` = `"pickup"`, `"stop:{id}"`, or `"destination"`. A **stop** arrival is acted on: one guarded write marks it `completed`, promotes the next `upcoming` stop to `current`, and — **only for the writer that won** — the car's nav is re-shared to the new target through the MYR-526 sequencer and verified per MYR-527, so a re-delivered event re-dials nothing. **The DESTINATION advances nothing**: `dropped-off` remains the owner's tap, because ending a ride has consequences (billing, history, the Live Activity's dismissal) a false positive cannot be walked back from — the event exists there only for the MYR-542 arrival flash. **No new status, no new error code, no lifecycle-matrix change.**

#### `PATCH /api/ride-requests/{id}/trip`

**Party-scoped trip-shape edit ([MYR-541](https://linear.app/myrobotaxi/issue/MYR-541), CLIENT-DIRECTED: "allow the users to change the pick up or drop off at anytime. Obv if pick up has past they can only change drop-off"; extended with the stop list by [MYR-539](https://linear.app/myrobotaxi/issue/MYR-539)).** Body is contracts `$defs.RideRequestTripPatch` (v0.34.0): `pickup?`, `dropoff?` and/or `stops?` (at least one, else `400 invalid_request`) plus the REQUIRED `tripVersion` the editor holds. Answers `200` with the updated `RideRequest` (whose `tripVersion` incremented by exactly one) — including its full `stops` list, whether or not this edit touched it.

- **THE WINDOW is the client's rule**: the PICKUP is editable from `requested`/`accepted` only — once `arrived` the refusal is a `409` that NAMES the door still open ("the pickup has passed — only the drop-off can be changed"); the DROP-OFF is editable through every live status, `enroute` included. A terminal ride refuses everything ("the ride has ended"). The window rides the guarded UPDATE's own `WHERE`, so a concurrent picked-up beats a pickup edit in the database, never in a pre-check.
- **THE ACL is the participant set** — rider and owner both (the client's collaboration decision: every edit applies INSTANTLY, no approval gate anywhere, including the owner changing the final drop-off mid-ride), and every joined group member once MYR-540's membership lands. Non-party → `404`.
- **CONCURRENCY is `tripVersion`**: one UPDATE carries the status window, the version match and the place writes; a stale editor gets `409 conflict` and refetches. Two people editing at once resolve to exactly one winner — never a silent clobber. **The stop list rides the SAME decision (MYR-539).** Stops are rows rather than columns, so they cannot join that statement; they join its **transaction**, applied only after the guard has already admitted the edit, and the version bump the endpoints' UPDATE performs is the stop list's bump too. A stop refusal rolls the WHOLE edit back, endpoints included — the client sent one desired trip, not three independent writes.
- **STOPS ARE REPLACE SEMANTICS, NOT DIFF OPS (MYR-539).** The client sends the full desired list: an entry **with** an `id` keeps that existing stop (its `place` may have moved), an entry **without** one is new, and an existing stop left out is **removed**. **OMIT the key** to leave the list exactly as it is (an edit that only moves the drop-off never restates the stops); send **`[]`** to clear every stop back to a plain two-endpoint trip. Their window is the **DROP-OFF's** — stops are the trip *ahead* of the car — and what protects the trip *behind* it is not the status window but the stop rules below, which hold whatever the ride's status.
- **THE TWO STOP REFUSALS ARE DIFFERENT CLASSES, on purpose.** A replacement that **removes or reorders** a stop already `completed` or `current` — including inserting a new stop *ahead* of one, which renumbers the road already travelled — is **`409 conflict`**: the road behind the car cannot be rewritten, and the client refetches and re-applies, the same loop a stale `tripVersion` runs. So the client sends what it holds and lets the guard decide rather than pre-filtering. An `id` **the ride does not have** is **`400 invalid_request`** (never silently treated as a new stop): no retry of the same body could fix it. A frozen stop's **`place`** is simply **not written** rather than refused — every entry restates its place by construction, so treating a byte-difference on a stop nobody is editing as a `409` would refuse edits to the rest of the trip for no reason a user could act on.
- **What follows a winning edit**: the OTHER participants are pushed ("«FirstName» changed the drop-off" / "Your pickup was changed" / "Your stops were changed", and "trip" when one edit moved more than one part — the edited PART is named, the PLACE never travels on a lock-screen surface); every live surface converges off the TripEdit-marked `ride_status_changed` frame, whose `tripVersion` is the refetch signal (the frame stays summary-only — no places); and when the edited endpoint is the CURRENT leg's target (pickup edited while `accepted`+dispatched, drop-off edited while `enroute`, **or the STOP LIST edited while `enroute` — MYR-539, where the server computes the current leg's new target because once a trip has stops it is no longer derivable from the endpoints, and that arm SUPERSEDES the drop-off arm so one edit is one re-share**) the dispatcher RE-SHARES the car's nav through the sequencer and then VERIFIES it end-to-end per MYR-527 — re-share once more on a miss, owner's "may not have the route" push on two. A drop-off edited before the rider's Start re-shares nothing: leg 2 has not dispatched, and the Start dispatches the NEW drop-off for free. A pickup edited on an undispatched reservation likewise: the sweeper re-reads the pickup every tick, so the MYR-535 dispatch lead re-computes on its own.

#### `POST /api/ride-requests/{id}/decline`

Owner-only. Legal from `requested` → `declined` for any ride, **and from `accepted` → `declined` for a SCHEDULED ride** (MYR-360); any other current status is `409 conflict`. Responds `200 OK` with the updated `RideRequest` and unicasts `ride_status_changed`. Same error catalog as accept **minus the dispatch seam and the `vehicle_unavailable` gate** — an owner may always decline a request regardless of the vehicle's status, and regardless of the MYR-342 ride-sharing pause.

**Declining a CONFIRMED reservation (MYR-360).** An owner may decline a ride that is already `accepted` **when it carries `scheduledFor`**. This exists for one situation: the owner pauses ride sharing for a car (§7.18) that has a confirmed future reservation on it. Before this, that reservation had no humane exit — the reservation sweeper **holds** at due time (its pause check runs before the claim), retries, and the ride resolves `reservation_expired` at the 30-minute lateness ceiling, so the rider learns nobody is coming **half an hour after their pickup time**. The client now reads `?upcomingForVehicle=` (above) before pausing, names the next reservation, and offers this decline. **The hold-then-expire backstop stays** — it is what covers a crash or an offline pause, where nobody was present to decline anything; this only makes the common path humane.

- **SCHEDULED ONLY. An accepted INSTANT ride still returns `409 conflict`.** A car already dispatched to a rider standing on a sidewalk is a different situation with different consequences (the nav is already in the car, the rider is already outside) and is out of scope. The narrowing is expressed as a property of the row — `scheduled_for IS NOT NULL` — which is **immutable for a ride's lifetime**: the reschedule negotiation moves the reservation *instant* but never converts a reservation into an instant ride or back.
- **A reservation already PAST its `scheduledFor` is still declinable.** Deliberate: the ride is still `accepted`, the owner is still the decider, and an explicit decline is strictly better for the rider than the silent `reservation_expired` they would otherwise receive. There is **no race with the sweeper**: its claim re-checks `status = 'accepted'` in the same guarded `UPDATE` that stamps its latch, so a declined reservation matches no row and the sweeper does nothing at all — no push, no outcome, no `ride.due`.
- **Race discipline is unchanged.** The allowed-from set handed to the guarded `UpdateStatusFrom` write is the ride's full **legal** from-set (`{requested}` for an instant ride, `{requested, accepted}` for a scheduled one), so a status that moves between the pre-check read and the write is still arbitrated by the database: two racing declines yield exactly one `200`, one `409 conflict`, and one published `ride_status_changed`.
- **Accept is untouched.** `accepted → accepted` is still not a transition, and accepting a ride already past `requested` is still `409`.
- **The rider's push copy forks.** `ride.status.changed` → rider fires exactly as it does for every other `declined` transition, but the copy for a **scheduled** ride reads "*&lt;Vehicle&gt; can't make your scheduled ride*" rather than "*…can't take this ride*", which would read as a reply to a request the rider had just made. Per the standing rule it **omits the time** — the server holds `scheduledFor` in UTC and knows no client time zone.

#### Group rides (MYR-540, contracts v0.35.0 — CLIENT-DIRECTED)

A **group ride** is a ride other people may join, through a signed link the server mints when the owner accepts. Three additive, optional fields on `RideRequest` carry it — `groupRide`, `shareUrl`, `members` — plus one new endpoint, `POST /api/ride-requests/join` (§7.24).

Client decisions, all four (2026-08-13): joiners get the **full rider view**, joining requires **app + sign-in**, members have **full collaboration** (a member edits the trip exactly as the requester does), and there is **no cap** on group size — the group sorts out seats themselves.

**THE FLAG IS THE ONLY CLIENT-SUPPLIED PART.** `groupRide` is set at create from `RideRequestCreateRequest.groupRide` and nothing changes it afterwards (the trip PATCH carries trip SHAPE only). It is **optional and additive: absent means false** — every ride written before MYR-540 and every ride booked with the toggle off — and a consumer MUST read absence as false, never as "unknown". `groupRide: true` with no `shareUrl` and no `members` is the ordinary state of a group ride still waiting for the owner's decision.

**THE LINK IS MINTED AT ACCEPT, ONCE.** The owner's `requested → accepted` transition mints a 6-character join code (`^[A-Z0-9]{6}$`) and its expiry in the SAME guarded write that performs the transition, so only the winning accept mints and a repeated or re-delivered accept mints nothing — the ride keeps the code it already published, because a re-mint would silently kill every link already sent. A request the owner **declines** never mints one, and a ride that is not a group ride never mints one at all.

`shareUrl` is the complete signed link and is **indivisible**: clients share it VERBATIM as the single activity item of the share sheet (the MYR-359 one-link rule), never parse it, never rebuild one out of parts. Shape, informative and not a licence to parse:

```
https://myrobotaxi.app/ride/{code}?k={keyId}.{expUnix}.{sigBase64url}
```

The `k` triple is §7.5.6's, unchanged — one-character key id so a rotation can serve both keys at once, expiry as unix seconds, base64url-unpadded 64-byte Ed25519 signature — over the canonical ASCII payload `ride:{code}:{expUnix}`. **The `ride:` prefix is DOMAIN SEPARATION**, and it is what makes it safe for both link kinds to share one private key and one compiled-in public key in the landing shell: nothing signed for one kind can be replayed as the other. Unlike an invite link it carries **no display names**, because a group link addresses nobody in particular.

**PRESENCE RULES for `shareUrl`, in the order the server applies them:**

1. Group rides only.
2. **Accepted or later** — enforced by there being no code before the mint, not by a status test.
3. Inside the code's own expiry (7 days, the invite link's TTL and for the same reason: it bounds a link that escaped into a group chat, and exists mainly for the row that never reaches a terminal status).
4. **Terminal status plus a five-minute linger.**

Rule 4 needs stating precisely, because the two halves differ. **THE CODE DIES AT TERMINAL WITH NO GRACE AT ALL**: the redeem predicate excludes `completed`/`declined`/`cancelled`, so a link redeemed one second after the drop-off answers `404` like any other dead code. The **linger governs only whether the FIELD is emitted**, and it exists for a rendering reason — a ride's ending and the requester's share sheet are not coordinated, and a control that blanks mid-interaction reads as a fault rather than as an ending. Five minutes is deliberately the same window MYR-421 holds a completed ride's Live Activity open for, so there is one number rather than two. Emitting a link that no longer redeems is safe precisely because the contract already says so: **the signature proves the link came from the server, not that the ride is still joinable** — redemption is authoritative. The linger is measured from `updatedAt`, not `completedAt`, because only one of the three terminal statuses stamps a completion instant.

**ABSENCE OF `shareUrl` IS NEVER A STATEMENT ABOUT WHETHER THIS IS A GROUP RIDE.** `groupRide` is that field and the only one.

**`members` IS THE JOINERS, AND THE REQUESTER IS NOT ONE OF THEM.** Each entry is `{userId, firstName}` (`$defs.RideMember`) — the smallest shape that lets a surface render the group. The requester is `riderId` and is never duplicated into the list; the owner is not in it either (a party to the ride, not a passenger). That reading is what keeps every pre-MYR-540 row byte-identical: **absent means no joiners**, so an old solo ride needs no `members: []`. Rendering "N riding" is `members.length + 1`. Order is server-defined (currently ascending join time). `firstName` is server-derived through the SAME identity ladder `requesterName` uses (first name → email local-part → a generic fallback), so it is always a non-empty string, and **first names only** — the same P1 policy that governs `requesterName` and the push payloads.

**MEMBERSHIP IS RIDE-SCOPED AND SELF-EXPIRING.** It buys viewer-tier access to the ride and its VEHICLE for the ride's lifetime and dies with the ride, leaving no standing grant to revoke:

- **Vehicle access.** A live membership is a THIRD source of vehicle access beside ownership and an accepted share — the narrowest of the three. It resolves to `viewer` with the **zero-value capability set**, so riding along never becomes permission to request rides in the car, and every viewer-facing surface (the catalog row, the `GET .../snapshot`, the WebSocket subscription) applies the ordinary MYR-435 viewer mask.
- **It ENDS at terminal status**, in the access-set query itself, with nothing to revoke and nothing to sweep. A member's next WebSocket handshake after the ride ends resolves an access set without the vehicle and is refused for it.
- **LIVE SOCKETS AGE OUT; THEY ARE NOT SEVERED.** The handshake access set is frozen for the session, and the only mechanism that narrows it mid-session is the MYR-373 revocation nudge, which fires on SHARE mutations and not on a ride ending. So a member connected when the ride completes keeps that socket until the 60-second `AccessRevalidator` backstop sweep re-derives their set and closes the session with `4002` — the same bound, and the same mechanism, that already governs a rider whose access ends. Bounded, and in the safe direction.
- **A member who joins mid-session must RECONNECT.** The handshake set only ever narrows; nothing widens it in place. A joiner's app is reconnecting anyway (it has just navigated to the tracking surface), so this is invisible in practice — but a client that joined on an already-open socket and did not reconnect would see no telemetry until it did.

**WHICH VERBS THE WIDENED PARTY SET REACHES.** `loadForParty` — the party check behind every `/api/ride-requests/{id}` surface — admits a joined member, so a member gets the §7.8 detail read, sees the ride in their own `GET /api/ride-requests` list, and may `PATCH .../trip` (the full-collaboration decision; the trip PATCH's ACL was always "the participant set", and this is that set). Two verbs deliberately did **not** widen:

| Verb | Member | Why |
|------|--------|-----|
| `GET /api/ride-requests/{id}`, `GET /api/ride-requests` | ✅ | The full rider view is the client's decision. |
| `PATCH /api/ride-requests/{id}/trip` | ✅ | Full collaboration — a member edits exactly as the requester does, under the same `tripVersion` guard. |
| `POST .../activity-token` (both) | ✅ | A member is a passenger and holds their own Live Activity. Registration is keyed `(ride, user)` per §7.21, so a member is another row on the same ride and the ETA ticker — which fans out per REGISTRATION — picks it up unchanged. |
| `POST .../cancel` | ❌ **403** | MYR-537 gave the cancel to the requester and the owner. **A member leaving is not a cancel**, and there is deliberately no leave endpoint in v1. |
| `POST .../start` | ❌ **403** | "We're in, go" is one person's call, and it is the requester's. |
| `POST .../accept`, `.../decline`, `.../picked-up`, `.../dropped-off` | ❌ **403** | Owner-only, unchanged. |

Both refusals are **403, not 404**, and the difference is deliberate: this caller is a party looking at the ride in their own app, so answering "not found" would read as a bug. A **non-party** still gets `404` — the widening is membership, not a hole in the scoping — and an unreadable membership fails closed to the same `404`.

**PUSH.** Every ride-lifecycle push a rider receives, the members receive too, in the RIDER's words verbatim — same alert, same category, same island-alert marking. Forking the copy would mean inventing a second voice for one fact. The editor of a trip change is excluded, as always. **Joining itself sends nobody anything** (client decision): a notification per joiner would be a group chat's worth of banners, and the requester finds out by looking at the ride.

**NO WS FRAME FOR MEMBERSHIP CHANGES.** `ride_status_changed` stays summary-only and a client picks up a new member on the refetch it already performs. `shareUrl` and `members` are delivered on the **party-scoped REST surfaces only** — never on a broadcast frame, the same delivery rule `requesterName` follows.

**ACCOUNT DELETION.** A deleted account's memberships are DELETED, not left inert. The `ON DELETE CASCADE` covers only the direction that needs no help (a ride's rows die with the ride); a member's own deletion is the other direction, and their rows would otherwise stand on somebody else's live ride — a deleted account named in a `members` array and, more to the point, present in the access set that admits a WebSocket to that ride's vehicle. The step runs beside the open-ride cancellation it completes: that one ends the rides the person BOOKED, this one ends the ones they merely JOINED.

---

#### Lifecycle transition matrix

The main `RideRequestStatus` lifecycle is monotonic; the reschedule negotiation is a separate sub-state (`rescheduleStatus`, MYR-192). Every mutation endpoint enforces legality in the **handler** (the store stays a dumb persistence layer) and rejects an illegal transition with `409 conflict`. Rows are the current status; a cell names the endpoint that performs the transition (and the story that owns it), or `409` when no legal transition exists from that state.

The owner-driven handshake makes the lifecycle a strict linear chain
`requested → accepted → arrived → enroute → completed` (MYR-270). Column = target state.

A cell naming an endpoint means the transition is **legal from that status**; it does not mean every row in that status can take it *right now*. Two cells carry an additional row-level precondition, both on the `accepted` row and both narrowed by `scheduled_for`: `accepted → declined` is SCHEDULED-only (MYR-360), and `accepted → arrived` is refused for a SCHEDULED ride that is still **dormant** (MYR-376 — see the bullets below).

| From \ To | `accepted` | `declined` | `arrived` | `enroute` | `completed` | `cancelled` |
|-----------|-----------|-----------|-----------|-----------|-------------|-------------|
| `requested` | `accept` (owner, MYR-175) | `decline` (owner, MYR-175) | `409` | `409` | `409` | `cancel` (rider, MYR-174) |
| `accepted` | — | `decline` (owner, MYR-360 — **SCHEDULED rides only**; instant stays `409`) | `picked-up` (owner, MYR-270 — **`409` for a SCHEDULED ride that is neither dispatched nor yet due**, MYR-376) | `409` | `409` | `cancel` (rider, MYR-174; **owner, MYR-522**) |
| `arrived` | `409` | `409` | — | `start` (rider, MYR-270) | `409` | `cancel` (rider, **MYR-537**; owner, MYR-522) |
| `enroute` | `409` | `409` | `409` | — | `dropped-off` (owner, MYR-270) | `cancel` (**rider only**, MYR-537 — the owner's exit stays dropped-off, `409` naming it) |
| `declined` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |
| `completed` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |
| `cancelled` (terminal) | `409` | `409` | `409` | `409` | `409` | `409` |

- **Atomicity / race semantics (MYR-174/175):** every transition executes as a single guarded UPDATE (`WHERE id = … AND status = ANY(<legal-from>)` — `store.RideRequestRepo.UpdateStatusFrom`), so concurrent conflicting mutations serialize in the database: **exactly one wins; every loser receives `409 conflict`** even if its pre-check read saw a legal state (e.g. rider-cancel racing owner-decline, or an owner double-tapping accept from two devices). The WS `ride_status_changed` frame and the `ride.accepted` dispatch event are published only by the winning write — the dispatch seam is exactly-once per accept by construction.
- **MYR-174 (this story)** implements only the two `→ cancelled` transitions. For the RIDER, cancel from `enroute`/`arrived` (ride in progress) and from any terminal state is `409` — the rider's cancel is legal only from `{requested, accepted}`. **MYR-522 adds the OWNER's cancel** from `{accepted, arrived}` (never `requested` — decline is that moment's owner verb — and never `enroute`: the rider is aboard, and ending early is dropped-off). Both stamp `cancelledBy` in the same guarded write.
- **MYR-175** implements `requested → accepted` / `requested → declined` (owner-only endpoints above). Accepting or declining a ride already past `requested` — including one the rider cancelled while the owner sheet was open — is the race the `409` protects.
- **Reschedule confirm/decline (owner)** is NOT part of MYR-175: the rider-side propose endpoint (`ProposeReschedule`) has no HTTP surface yet, so an owner resolve endpoint would be unreachable dead code. The whole reschedule negotiation (propose + resolve, `rescheduleStatus` sub-state) ships together in **MYR-192**; the store layer (`ResolveReschedule`) is already in place.
- **MYR-176** performs the leg-1 (pickup) nav push on accept but records it as an orthogonal `dispatchStatus`/`dispatchedAt` annotation (see "Dispatch outcome" above) — it does **not** advance the main lifecycle.
- **MYR-179** changes only **when** that leg-1 push fires for a **scheduled** ride — at `scheduledFor` rather than at accept (see "Reservation-time dispatch" above). The matrix is untouched: a reservation still goes `requested → accepted` on accept and sits in `accepted` until the owner confirms pickup, exactly as before; only the orthogonal `dispatchStatus` annotation resolves later. A reservation failed by the lateness ceiling (`reservation_expired`) likewise stays `accepted` — the dispatch failed, the ride did not, and the owner/rider may still cancel or **proceed manually**. **That manual proceed remains available under MYR-376 precisely because the pickup gate is time-aware:** dormancy lifts at `scheduledFor`, and an expired reservation is by definition past it, so `picked-up` is open to it. A gate keyed on `dispatchStatus == "sent"` would have quietly converted this sentence into "may cancel" — which is why it is not.
- **MYR-376** adds a **row-level precondition to the `accepted → arrived` cell** and changes nothing else: the cell stays legal, and the refusal reuses `409 conflict` (no new code, no new status, no schema change). A **SCHEDULED** ride is refused while it is **dormant** — neither dispatched (`dispatchStatus == "sent"`) nor yet due (`scheduledFor <= now()`). This is the same shape as the MYR-360 bullet below it — a cell narrowed by a property of the row rather than by its status — except that the narrowing here is partly **temporal**, so the same row answers differently before and after its due instant. INSTANT rides are unaffected. Like every other transition here the precondition is evaluated **inside** the guarded `UPDATE` (`UpdateStatusFromDispatched`), so it is arbitrated by the database against a concurrently dispatching sweeper rather than by a pre-check read. `start` (`arrived → enroute`) needs no gate of its own: a dormant reservation can no longer reach `arrived`. Full rule and rationale in §7.8 under `POST .../picked-up`.
- **MYR-277** gates **accept only** on vehicle availability: `requested → accepted` is refused `409 vehicle_unavailable` when the target vehicle's persisted status is `in_service` or `offline`. This does **not** change the transition matrix (the transition stays legal for a dispatchable `parked`/`driving`/`charging` vehicle) — it is an orthogonal capability precondition on the accept endpoint, evaluated against the current persisted status and never applied to decline. **MYR-313** narrows it to **instant** rides: a `scheduledFor` ride skips the gate entirely (see §7.8), matching the per-rider and per-vehicle active-ride guards, which are both partial on `scheduled_for IS NULL`. **MYR-383** adds a SECOND, independent capability precondition on the same cell, pointing the other way: `requested → accepted` is refused `409 vehicle_unavailable` / `subCode: time_conflict` for a **SCHEDULED** ride whose target vehicle is already promised to another committed open ride within 45 minutes of its `scheduledFor`. It likewise does **not** change the matrix — the transition stays legal whenever the window is free — and it is **not** covered by the MYR-313 exemption, which exempts reservations from the *status* gate alone: this one asks about the reservation *instant*, not about the car today. **INSTANT accepts are not window-gated in v1a.** Decline is gated by neither.
- **MYR-342** adds the **ride-sharing pause** on **create AND accept**, and it too leaves the matrix untouched: `requested → accepted` stays legal, and the refusal is `409 vehicle_unavailable` — a capability conflict, the same class as MYR-266 and MYR-277, because the car cannot serve the ride. **Unlike MYR-313's availability exemption, it applies to SCHEDULED rides as well as instant ones**, deliberately: a service visit ends on its own, an owner's pause does not. The reservation sweeper enforces the same fact a third time, as a **hold** rather than a refusal, and the lateness ceiling resolves anything the owner never un-pauses.
- **MYR-316** adds a **service-window bound** on `scheduledFor`, on **create AND accept**, and it too leaves the matrix untouched: `requested → accepted` stays legal, and the refusal is a `400 invalid_request` (not a new code, not a `409`) because a reservation for a time the car provably cannot serve is a bad *request*, not an illegal transition or a capability conflict. Bound: `scheduledFor` earlier than the vehicle's resolved `serviceEstimatedEndAt` (§7.0/§7.1) is refused; **null estimate ⇒ no bound at all**, **equal is allowed** (strictly "earlier than"), and instant rides are unaffected (no `scheduledFor`, already gated by MYR-277). Note the interaction with the bullet above: the bound means a scheduled accept **does** read the vehicle again. Since **MYR-372** that read **fails CLOSED** — a transient failure is `500 internal_error` and the reservation is not accepted; a vehicle that no longer exists is `409 vehicle_unavailable`. It used to fail open, leaving the reservation unbounded rather than refused, on the argument that refusing protected against MYR-313's stranding; that argument does not hold, because MYR-313 was a permanent `409` with no way through while a read failure is a transient error the owner retries out of. MYR-313's exemption from the *availability* gate is unchanged — and since MYR-372 it is a genuine DEFERRAL, because the reservation sweeper now asks the in-service question at the due instant.
- **MYR-360** adds the **only new legal edge** in this matrix since MYR-270: `accepted → declined`, and unlike the MYR-277/313/342/316 bullets above it **does** change a cell — from `409` to the `decline` endpoint. It is narrowed to **SCHEDULED rides**; an accepted **instant** ride still returns `409 conflict`, because a car already dispatched to a rider on a sidewalk is a different situation and out of scope. The narrowing rests on a row property that is **immutable for a ride's lifetime** (`scheduled_for IS NOT NULL` — the reschedule negotiation moves the reservation *instant*, never its existence), which is what makes it safe to derive the guarded write's allowed-from set from a pre-check read: the set handed to `UpdateStatusFrom` is the ride's full **legal** from-set, so the database still arbitrates every race and two concurrent declines still yield exactly one winner. Declining a reservation **already past** its `scheduledFor` is allowed on purpose — the ride is still `accepted`, the owner is still the decider, and an explicit decline beats the silent `reservation_expired` at the lateness ceiling; the sweeper's claim re-checks `status = 'accepted'`, so a declined reservation simply matches no row. Accept, the owner-only rule, and decline's freedom from every capability gate are all unchanged.
  - **MIGRATION-INDEX AUDIT — no migration and no index change is required, and here is why.** The standing rule is that a new state in the active-ride predicate must update the migration 0013 index **in the same PR**. This change **adds no new status value at all**: `declined` has been a member of `go_ride_requests_status_check` since 0002. It adds one legal *edge* into an existing **terminal** state. `declined` appears in **neither** partial index — 0004 `uq_go_ride_requests_active_instant_rider` covers `status IN ('requested','accepted','enroute','arrived')` and 0013 `uq_go_ride_requests_active_instant_vehicle` covers `status IN ('accepted','enroute','arrived')` — so the transition only ever moves a row **out of** 0004's predicate and, for an instant ride, out of 0013's; it can never move one in. Independently, the new edge is **scheduled-only**, and both indexes are partial on `scheduled_for IS NULL`, so every row this edge can touch is outside both indexes entirely. Both arguments are individually sufficient. No new column, no new value, no predicate change.
- **MYR-270** replaces the retired MYR-265 auto-leg model with an **owner-driven handshake** over the three remaining transitions: `accepted → arrived` (owner **picked-up**), `arrived → enroute` (rider **start** — which fires the leg-2 dropoff nav push), and `enroute → completed` (owner **dropped-off**). Completion is now **owner-confirmed**: the MYR-265 drive-end auto-completion (`internal/ridecomplete`) and the rider `board` endpoint are removed, so a car parking no longer closes a ride and the rider can no longer advance the ride before the owner confirms pickup. Each transition is guarded + idempotent and emits the usual `ride_status_changed` summary. (The `enroute_at` column stamped on `arrived → enroute` remains as a lifecycle timestamp; it no longer feeds any drive-end correlation.)
- **MYR-538** adds the first transition in this matrix with a writer that is **not an endpoint**: `accepted → arrived` is also performed by the server when telemetry shows the car parked at the pickup (see "Auto-arrival" under `POST .../picked-up`). The cell is unchanged and so is the endpoint — the detector runs the **same** guarded `UpdateStatusFromDispatched` with the same allowed-from set, so it is simply another contender in the race the first bullet describes, and the one that loses (usually it) is silent. No new status, no new code, no schema change.
- **MYR-539** adds multi-stop trips and changes **no cell in this matrix**. A stop's `upcoming → current → completed` progression is a SEPARATE, per-stop lifecycle that lives on `RideStop.status` (see "Multi-stop trips" above): it advances on the same MYR-538 waypoint evidence, through its own guarded exactly-once write, and it neither reads nor writes `RideRequestStatus`. In particular the car reaching the final DESTINATION does **not** complete the ride — `enroute → completed` still has exactly one writer, the owner's `dropped-off` tap. The only thing multi-stop changes about this table is what `start` (`arrived → enroute`) SENDS THE CAR TO: the first stop when the trip has stops, the drop-off otherwise. No new status, no new error code, no new edge.
- **MYR-540** adds group rides and changes **no cell in this matrix**. Nothing about a group ride is a status: the flag is set at create, the link is minted as a SIDE EFFECT of the existing `requested → accepted` write (inside that write's own transaction, so only the winner mints and a repeat mints nothing), and joining writes a membership row without touching `status` at all. What DOES change is who may take some of the existing cells: a joined member reaches the trip PATCH and the two read surfaces, and deliberately reaches neither `cancel` nor `start` — see "Group rides" above for the full table and for why both refusals are `403` rather than `404`. No new status, no new error code, no new edge.
- Every transition that succeeds emits a `ride_status_changed` summary frame to the two parties.

---

### 7.9 `POST /api/vehicles/{vehicleId}/command/{name}` (Tesla vehicle commands)

> **Anchored:** FR-11.x (vehicle actuation), NFR-3.21 (ownership enforcement), NFR-3.22 (TLS in transit). Implemented by MYR-180.

#### Purpose

The owner-only actuation surface. It sends a signed Tesla Fleet vehicle command (lock, climate, charge, charge-port door, seat heater/cooler, media, trunk, remote start, horn, lights) or an unsigned navigation/dispatch command to the caller's vehicle. It is the foundation for P11 (per-command issues MYR-181–183) and P10 dispatch (MYR-176/MYR-245 use `navigation_request` — see the note below on why `navigation_gps_request` is not proxy-usable).

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
| `navigation_gps_request` | `vehicle_cmds` | no | `lat`, `lon` (required), `order` (int, default 1) — lat/long destination. **NOT proxy-usable** (see below); reachable only via a direct Fleet REST call, which the server does not make. Registered for API completeness. |
| `navigation_request` | `vehicle_cmds` | no | `value` (string) — a text destination the car's nav geocoder resolves: a street address, a `"<lat>,<lon>"` coordinate pair (what MYR-245 dispatch sends), or a maps URL. This is the command dispatch uses. |
| `charge_port_door_open` | `vehicle_charging_cmds` | yes | — |
| `charge_port_door_close` | `vehicle_charging_cmds` | yes | — |
| `remote_seat_heater_request` | `vehicle_cmds` | yes | `seat_position` (int 0–8; 0 front-left, 1 front-right, then second/third rows), `level` (int 0–3: off/low/med/high) |
| `remote_seat_cooler_request` | `vehicle_cmds` | yes | `seat_position` (int; **1 front-left, 2 front-right** — front seats only, a different numbering from the heater), `seat_cooler_level` (**int 1–4**: 1 off, 2 low, 3 med, 4 high) |
| `media_toggle_playback` | `vehicle_cmds` | yes | — |
| `media_next_track` | `vehicle_cmds` | yes | — |
| `media_prev_track` | `vehicle_cmds` | yes | — |
| `adjust_volume` | `vehicle_cmds` | yes | `volume` (float 0–11) |

There is no `vehicle_remote_start` Fleet scope (that is the legacy Owner API); `remote_start_drive` is a `vehicle_cmds` command. Both nav commands are UNSIGNED (Tesla processes them server-side, so neither needs the virtual key), **and NEITHER routes through the tesla-http-proxy in this deployment**: `navigation_gps_request` is not in the proxy's `ExtractCommandAction` switch (HTTP 400 `invalid_command` locally — the car is never dialed; the Jul 18 2026 outage), and the deployed proxy (v0.4.1) also mis-forwards `navigation_request` (double-written HTTP 400: `"command requires using the REST API"` + `"upstream internal error"` — the Jul 20 2026 recurrence, MYR-245). Unsigned commands therefore POST **directly to the Fleet REST API** (`FLEET_API_BASE_URL`), routed per the registry's `SignerRequired` flag; signed commands keep the proxy path.

The charge-port, seat-climate, and media commands (MYR-249) are all **signed**, so they keep the proxy path. Each was verified present in the **pinned** tesla-http-proxy (v0.4.1) `ExtractCommandAction` switch, so none 400s locally as `invalid_command` before the car is dialed (the MYR-245 failure mode). Two Tesla-side quirks the registry mirrors: `charge_port_door_open`/`close` sit under `vehicle_charging_cmds` (with the other `charge_*` commands), not `vehicle_cmds`; and the seat `seat_position` index differs between the heater (0-based, all seats) and the cooler (1 front-left / 2 front-right, front seats only). The two seat commands are also asymmetric on level: the heater `level` is **0–3** (0 off, passed straight to `vehicle.Level`), while the cooler `seat_cooler_level` is **1–4** (1 off … 4 high) — the HTTP value maps 1:1 onto Tesla's `HvacSeatCoolerLevel` enum because the proxy's `Level(x-1)` and `SetSeatCooler`'s `+1` cancel. Body params (seat position/level, volume) are **transit-only — never persisted, never logged** (same classification as the existing command params); no `data-classification.md` change.

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

#### Naming the rejection reason on `command_failed` (MYR-329)

> **Non-normative — no schema, code, or field change.** `error.message` is free text per §4.1 and stays free text; this only pins down what the server chooses to put there so clients can rely on it.

A `502 command_failed` means we reached the car and it refused. The tesla-http-proxy relays the car's own refusal as `{"response":{"result":false,"reason":"car could not execute command: <car prose>"}}` (vehicle-command v0.4.1: `pkg/vehicle/infotainment.go:37-42` reads `ActionStatus.ResultReason.PlainText`, `pkg/proxy/proxy.go:146` serializes it; VCSEC refusals use a `vcsec could not execute command: ` prefix). That prose is **firmware text** — unstable, untranslated, and not a contract — so it is never forwarded.

Instead the server matches it against a closed allow-list and emits its own canonical token:

```
"message": "vehicle command failed: vehicle_in_service"
```

| Token | Meaning |
|-------|---------|
| `vehicle_in_service` | Car is in service mode; Tesla refuses most remote commands. |
| `requires_user_acknowledgement` | Car wants a confirmation on its own touchscreen. |
| `user_not_present` | Command needs someone in the driver's seat. |
| `remote_access_disabled` | Remote/mobile access is switched off in the car's settings. |
| `low_battery` | Pack too low for the requested action. |
| `vehicle_busy` | Car is mid-something and declined to queue. Transient. |

Rules for consumers:

- **Unrecognized reason ⇒ no token.** The message is exactly `vehicle command failed`, as before this issue. Clients show their generic copy.
- **Match the token, never the prose.** The set is append-only and grows; a client that does not recognize a token MUST fall back to its generic copy, so old clients keep working. This is the one place a client may read `error.message` — and only to look for a token from this closed set (cf. FR-7.1).
- The wire message is assembled purely from server-side constants, so no upstream bytes reach a client. The full sanitized upstream reason is kept server-side only, on the `vehicle command rejected` log line.

Implementation: `internal/commands/reject_reason.go`.

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

**Response — 200 OK** — same shape as §7.10.1 (a fresh `accessToken` + a new `refreshToken`). As of [MYR-243](https://linear.app/myrobotaxi/issue/MYR-243), `user` is enriched the same way as sign-in — `name`/`email` are populated best-effort from the stored profile (`go_identity_apple` binding, falling back to `go_users`) so installs that lost their local profile cache still get a full projection on refresh, not just `id`. A profile-lookup failure is fail-open: the refresh still succeeds and `user` degrades to `id`-only rather than the request failing. The previous refresh token is now invalid.

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

### 7.11 In-app Tesla account link (owner onboarding — MYR-246)

Two owner-facing endpoints that let a signed-in user link their Tesla Fleet
account **from inside the native app** (iOS `ASWebAuthenticationSession`),
replacing the developer-only localhost `ops auth link` flow. They mint the
Tesla authorize URL server-side (so the Tesla `client_secret` never reaches the
device) and complete the code→token exchange on the server, persisting the
tokens through the existing encrypted `Account` dual-write path
(`store.AccountRepo.UpdateTeslaToken`, §MYR-62). The OAuth primitives are shared
with the ops CLI via `internal/teslaauth`; the endpoints live in
`internal/teslalink`.

**Enablement.** Mounted only when `TESLA_LINK_REDIRECT_BASE_URL` **and** the
Tesla OAuth credentials (`AUTH_TESLA_ID` / `AUTH_TESLA_SECRET`) are configured;
otherwise the routes are not registered (the app's "Link your Tesla" button has
no backend). This mirrors the fleet-config endpoint's optional-mount pattern.

**End-to-end flow.**

1. App (authenticated) → `POST /api/tesla/link/start` → receives the Tesla
   `authorizeUrl`.
2. App opens `authorizeUrl` in `ASWebAuthenticationSession` with
   `callbackURLScheme = "myrobotaxi"`.
3. User consents on `auth.tesla.com`. Tesla redirects the browser to the
   backend `redirect_uri` = `TESLA_LINK_REDIRECT_BASE_URL` +
   `/api/tesla/link/callback`.
4. The callback validates `state`, exchanges the `code` for tokens, persists
   them for the calling user, and issues a `302` to the app deep link
   `myrobotaxi://tesla-linked?status=…`.
5. `ASWebAuthenticationSession` intercepts the `myrobotaxi://` redirect and
   returns control to the app.

**Scopes.** The authorize URL always requests the full Fleet scope set —
`openid offline_access user_data vehicle_device_data vehicle_location
vehicle_cmds vehicle_charging_cmds` — and always sends
`prompt_missing_scopes=true` (MYR-242), so an already-linked owner re-consents
to any newly requested scope instead of silently keeping the old set. Verify the
**granted** JWT `scp` claim after linking, not the request.

#### 7.11.1 `POST /api/tesla/link/start`

Owner-authenticated. Creates a short-lived (30 min — MYR-517; it was 10, and a
real first consent outran it), single-use PKCE + `state` session bound to the
caller's user id and returns the Tesla authorize URL.

**Auth:** `Authorization: Bearer <access token>` (ES256 or HS256, §3). Missing/
invalid → `401 auth_failed`.

**Request body:** none.

**Response — 200 OK**

```json
{
  "authorizeUrl": "https://auth.tesla.com/oauth2/v3/authorize?client_id=…&redirect_uri=…&scope=…&state=…&code_challenge=…&code_challenge_method=S256&prompt_missing_scopes=true&response_type=code",
  "state": "<base64url-32B CSRF nonce>"
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `authorizeUrl` | `string` | P0 | Public Tesla authorize URL; contains no secret (client_secret is never included). |
| `state` | `string` | P0 | CSRF nonce and the server-side session key. Echoed for optional client correlation; the server is the authority on validation. |

Errors: `401 auth_failed` (missing/invalid bearer), `500 internal_error`
(randomness/PKCE failure).

#### 7.11.2 `GET /api/tesla/link/callback`

**Unauthenticated** (Tesla redirects a browser here — there is no bearer token).
The caller is recovered from the single-use `state` session created at `/start`,
so a forged callback cannot bind tokens onto an arbitrary account. Query params
are Tesla's standard OAuth redirect: `code`, `state`, and on denial `error` /
`error_description`.

On every outcome the handler responds `302 Found` with `Location:
myrobotaxi://tesla-linked?status=<success|error>[&reason=<code>][&detail=<code>]`
(plus a tiny HTML fallback page with a tappable link + meta-refresh). **No
tokens or PII ever appear in the redirect URL or logs.** Outcome
classification:

| `status` | `reason` | Cause |
|----------|----------|-------|
| `success` | — | Tokens exchanged and persisted for the caller. |
| `error` | `tesla_denied` | Tesla returned an `error` param (user declined / scope refusal). |
| `error` | `invalid_state` | No matching session — unknown, replayed, or expired `state`. Carries an additive `detail` param (`session_expired` \| `session_unknown`, MYR-517) that a client may branch on; `reason` itself is unchanged. |
| `error` | `missing_code` | No authorization `code` in the redirect. |
| `error` | `exchange_failed` | Tesla rejected the code→token exchange. |
| `error` | `account_not_provisioned` | **Legacy (MYR-246).** No longer emitted on the happy path — provisioning (MYR-257) creates the `Account`. Retained as a defined reason for backward-compat. |
| `error` | `persist_failed` | Provisioning failed: Tesla `userinfo` lookup, token encryption, or the DB transaction failed. |

**Implementation notes.**

- **Self-serve provisioning (MYR-257).** On a successful code→token exchange the
  callback PROVISIONS the caller's minimal Prisma owner rows before persisting
  tokens, via the sanctioned `store.OwnerProvisioner` (a single, audited,
  idempotent, transactional path — NOT the identity module). In one transaction it
  first **resolves the canonical Prisma user** — (a) the existing owner of the
  Tesla `Account`(provider, providerAccountId) if any (its `userId` is authoritative
  and **never rewritten**; the caller's Apple identity converges onto it via a
  `go_identity_apple` re-point), else (b) an existing `"User"` matched by our
  **Apple-verified** email only (adopted, not re-inserted — prevents a
  `User.email @unique` collision for a returning web user; the Tesla-account
  email is never a merge anchor), else (c) INSERT a fresh
  `"User"` on the go_users id — then upserts `"Settings"`(`teslaLinked=true`) and
  `"Account"` (`ON CONFLICT ("provider","providerAccountId") DO UPDATE` tokens
  only, **never `userId`**; dual-write-encrypted; `providerAccountId` = Tesla OIDC
  `sub` from a `userinfo` call) for that canonical user. This makes a brand-new
  go_users-native Apple user a working owner with **no ops step** (the
  `AUTH_APPLE_BOOTSTRAP` + pre-seeded-`Account` MVP steps are gone) while never
  duplicating a user or reassigning a Tesla account/vehicle across users. Every
  outcome is audited (opaque cuids + outcome; no PII/tokens). A denied/failed link
  provisions nothing (no orphan `"User"`). Design + ownership rationale + the
  required cross-repo schema gate (incl. `User.email @unique`):
  [`../architecture/self-serve-onboarding.md`](../architecture/self-serve-onboarding.md).
- **Server-side stream setup (best-effort).** After provisioning, the callback
  triggers a best-effort hook that lists the owner's vehicles (Fleet API
  `GET /api/1/vehicles`), seeds their `"Vehicle"` identity rows **for owned
  vehicles only** (`access_type == "OWNER"`; shared-driver vehicles are skipped +
  audited, never attached), and — only when the tesla-http-proxy is configured at
  runtime — pushes the fleet-telemetry config per VIN so the car streams without
  an ops `fleet-config push`. A `teslaVehicleId` already owned by a different user
  is skipped (never reassigned). Any failure here is logged and never fails the
  link. The virtual-key approval stays a Tesla-app user action.
- Sessions are in-memory, single-use, and TTL-bounded (**30 min**, MYR-517 —
  see the expiry note below), and capped at **three in-flight sessions per
  user**, the oldest evicted when the cap binds. A denied callback (`error`
  param) also burns the session so it cannot be replayed within its TTL. A
  process restart drops in-flight sessions (the user simply retries the link —
  nothing is persisted until the callback succeeds).
- **The expiry that a slow consent flow must survive is OURS, not Tesla's**
  (MYR-517). Tesla mints the authorization `code` at the END of consent and the
  callback exchanges it within one round-trip, so no amount of time spent on
  Tesla's sign-in, MFA and scope screens is charged against the code's lifetime.
  Every second of it is charged against the `state` session created at `/start`.
  At 10 minutes that clock ran out on a real first-time owner mid-flow and
  answered `invalid_state`; 30 minutes is the honest budget for a Tesla sign-in
  the owner may not have credentials for, plus an MFA challenge and a
  six-checkbox permissions screen on a phone. The security of the flow rests on
  PKCE, the single-use state, and the per-user binding — not on the TTL, which
  bounds an in-memory map.
- **`detail` sub-classifies `invalid_state`** (MYR-517), as an ADDITIVE query
  parameter on the deep link. `reason` is unchanged and remains the field to
  branch on; `detail` is present only alongside `reason=invalid_state` and is
  safe to ignore. `session_expired` — the session existed and its TTL elapsed,
  which means the owner did everything right and the correct client behaviour is
  a silent new `/start` rather than an error the owner has to read and act on.
  `session_unknown` — no session for that state at all: a replay, a forgery, or
  a process restart. The server also logs the expiry as its own greppable event
  (`tesla_link_session_expired`, Info, with the configured TTL).
- `TESLA_LINK_REDIRECT_BASE_URL`, when set, is validated at startup — it must be
  an absolute `http`/`https` origin with a host — so a typo fails fast rather
  than silently producing a callback URI Tesla never redirects to.
- **Ops must register the exact callback URI** (`TESLA_LINK_REDIRECT_BASE_URL` +
  `/api/tesla/link/callback`) as an Allowed Redirect URI on the Tesla Fleet app,
  alongside the existing web (`https://myrobotaxi.app/api/auth/callback/tesla`)
  and CLI (`http://localhost:8765/callback`) URIs.

---

### 7.12 `DELETE /api/tesla/vehicles/{vehicleId}` (owner car offboarding — MYR-258)

Owner-authenticated "Remove this car" full teardown — the reverse of the
in-app link/onboarding flow (§7.11). The caller MUST own the vehicle. It removes
the car from our backend **authoritatively and immediately**, does the
automatable Tesla-side cleanup **best-effort**, and returns the two Tesla-side
steps only the owner can complete. Design + Fleet-API verification:
[`../architecture/car-offboarding.md`](../architecture/car-offboarding.md).

**Enablement.** The route is ALWAYS mounted (the authoritative teardown is a
local DB transaction that needs no proxy). The best-effort Tesla stream-config
delete is wired only when the tesla-http-proxy (`TESLA_PROXY_URL`) is configured;
otherwise it is skipped and the local teardown still completes.

**Request.**

```
DELETE /api/tesla/vehicles/{vehicleId}
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
```

`{vehicleId}` is the Prisma cuid (NOT a VIN — browsers never receive a full VIN).

**Behavior / sequence** (car-offboarding.md §5.1):

1. Validate the bearer → `userId`; resolve `vehicleId` → (VIN, owner) via
   `VehicleRepo.GetByID`. Unknown vehicle → `404 not_found` (indistinguishable
   from ownership-filtered — never leaks existence). A real ownership mismatch →
   `403 vehicle_not_owned`. No teardown runs on either.
2. Resolve the owner's Tesla token (best-effort, with auto-refresh). If absent,
   skip step 3.
3. Best-effort `DELETE …/fleet_telemetry_config` at Tesla (`FleetAPIClient.DeleteTelemetryConfig`,
   which rides the same forwarded/unsigned proxy path as the GET status read, NOT
   the JWS-signed POST push). Any failure/404 is **non-fatal** — logged, then
   `streamConfigDeleted:false`; the local teardown still runs.
4. Authoritative local teardown transaction (`store.OwnerTeardown`, owner-scoped).
   It first `SELECT … FOR UPDATE`-locks the owner's vehicle set (serializing
   concurrent same-owner teardowns so the last-vehicle decision is race-safe),
   DELETEs the Go-owned `go_ride_requests` rows for the vehicle (P1 encrypted
   pickup/dropoff GPS + passenger PII — NO FK to `Vehicle`, so the cascade never
   reaches them; a complete removal deletes them explicitly), then DELETEs the
   `Vehicle` (cascades `Drive`/`TripStop`/vehicle-scoped `Invite` + encrypted
   route blobs; fires the `vehicle_deleted` NOTIFY that closes WS/mTLS streams +
   evicts caches — data-lifecycle.md §3.5). On the owner's **last** vehicle it
   also DELETEs the Tesla `Account` tokens and resets `Settings`
   (`teslaLinked=false`, `virtualKeyPaired=false`). Writes the user-initiated
   `vehicle_deleted` AuditLog row in the same transaction (CG-DL-3). Since
   **MYR-261**, when the car has a `teslaVehicleId`, the same transaction also
   writes a **removed-vehicle tombstone** into `go_removed_vehicles` so a later
   Tesla re-link's best-effort vehicle sync cannot resurrect the still-Tesla-
   owned VIN (data-lifecycle.md §1.4.1); `vehicle_deleted.metadata.tombstoned`
   records whether one was written.
5. Respond `200` with the honest post-state + owner-action items.

**Idempotent (§4.5, "equivalent final state").** A repeated DELETE of an
already-removed car returns **`404 not_found`** — clients MAY treat that as a
successful terminal state. No duplicate audit row is written on a re-remove.
(The writer's zero-rows `AlreadyGone` success path exists only for the narrow
race where the row is deleted between the ownership lookup and the teardown.)

**Removed-vehicle tombstone & re-add (MYR-261).** Because the teardown does not
revoke access at Tesla, the removed car stays visible to the Fleet API and a
subsequent Tesla re-link would otherwise re-provision it. The tombstone written
in step 4 makes the removal durable: the post-link sync
(`store.OwnerProvisioner.UpsertOwnedVehicle`) skips any tombstoned
`(owner, teslaVehicleId)`, so **a passive re-link never resurrects a removed
car**. The passive bulk sync cannot distinguish a deliberate re-add from an
incidental re-link, so tombstone-wins is the default and a **deliberate re-add
is an explicit action**: it must first clear the tombstone via
`store.RemovedVehicleRegistry.ClearTombstone` (which writes a
`vehicle_readd_allowed` audit row), after which the next sync provisions the car
normally. Since **MYR-262** the sanctioned deliberate-re-add route is §7.13
`POST /api/tesla/vehicles/{teslaVehicleId}/re-add` (owner-authenticated), which
clears the tombstone then best-effort re-provisions the car; an operator stopgap
`ops vehicles re-add --user-id <id> --tesla-vehicle-id <id>` clears a tombstone
out-of-band over the same registry.

**OAuth revoke — server-side since [MYR-366](https://linear.app/myrobotaxi/issue/MYR-366), on a LAST-VEHICLE removal only.** On the removal that takes the owner's last car — the one whose teardown deletes the `Account` row — the server now `POST`s the stored refresh token to `https://auth.tesla.com/oauth2/v3/revoke` **before** the local transaction clears it, killing the grant outright. A **mid-fleet** removal does NOT revoke: the owner keeps other cars, the link must keep working for them, and revoking would break every one. The call is **best-effort and non-fatal** — a Tesla failure, an unreachable host or an already-dead token is logged and the authoritative local teardown proceeds unchanged, exactly like the stream-config delete above.

The revoke outcome is deliberately **not** on the wire. `revokeUrl` is still returned unchanged and still points at the owner-confirmed consent page: revocation may have failed, so the honest client behaviour — offer the owner the manual step — is the same either way, and a `grantRevoked` field would only invite clients to hide a step that is still sometimes necessary. Virtual-key removal has no Fleet API and no deep link (§1.3) — it is returned as **instructions only**.

**Response `200`** (`application/json`):

```json
{
  "removed": true,
  "wasLastVehicle": true,
  "teslaTokensCleared": true,
  "streamConfigDeleted": true,
  "revokeUrl": "https://auth.tesla.com/user/revoke/consent?back_url=myrobotaxi%3A%2F%2Ftesla-unlinked&revoke_client_id=<CLIENT_ID>",
  "virtualKeyRemoval": {
    "required": true,
    "automatable": false,
    "steps": [
      "Open the Tesla app or your car's touchscreen",
      "Go to Controls → Locks",
      "Tap the “MyRoboTaxi” key",
      "Tap Remove/Delete Key",
      "Authenticate by tapping a key card on the center console"
    ]
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `removed` | `boolean` | P0 | Authoritative: the car is gone from the app (true on both a fresh removal and an already-gone no-op). |
| `wasLastVehicle` | `boolean` | P0 | True when this was the owner's last linked vehicle (Account tokens cleared + Settings reset). |
| `teslaTokensCleared` | `boolean` | P0 | Mirrors `wasLastVehicle`: our stored Tesla tokens were deleted. Removes OUR access, not the grant. |
| `streamConfigDeleted` | `boolean` | P0 | Whether the best-effort Tesla `fleet_telemetry_config` delete succeeded. `false` on skip/failure — non-fatal. |
| `revokeUrl` | `string` | P0 | Owner-confirmed consent-revoke deep link (public URL, no secret). Omitted when no Tesla `client_id` is configured. |
| `virtualKeyRemoval` | `object` | P0 | Owner-manual key-removal instructions. `automatable` is always `false` (no API, no deep link). |

**Errors:** `401 auth_failed` (missing/invalid bearer), `403 vehicle_not_owned`
(caller does not own the vehicle), `404 not_found` (unknown vehicle),
`405 invalid_request` (non-DELETE method), `500 internal_error` (local teardown
transaction failed — atomic: nothing deleted, no audit row; retryable).

---

### 7.13 `POST /api/tesla/vehicles/{teslaVehicleId}/re-add` (owner deliberate re-add — MYR-262)

Owner-authenticated "Add this car back" — the sanctioned un-trap for the MYR-261
removed-vehicle tombstone. A car the owner removed via §7.12 gets a
`go_removed_vehicles` tombstone, and the passive post-link bulk sync
(`ownerStreamHook.AfterLink` → `store.OwnerProvisioner.UpsertOwnedVehicle`) skips
any tombstoned `(owner, teslaVehicleId)` so an incidental Tesla re-link can never
resurrect it (§7.12 "tombstone-wins"). That made a removed car a permanent trap;
this endpoint is the **only** runtime path that clears a tombstone.

**Deliberate-vs-passive seam.** This endpoint clears the tombstone
(`store.RemovedVehicleRegistry.ClearTombstone`) **before** provisioning; the
passive `AfterLink` sync **never** clears one. Everything after the clear (Fleet
list → owner filter → `UpsertOwnedVehicle` → stream-config push) is the shared
provisioning path, so the car returns through exactly the code the passive sync
uses.

**Enablement.** The route is ALWAYS mounted (clearing the tombstone — the durable
un-trap — is a local DB transaction that needs no proxy). The inline re-provision
is best-effort and only pushes stream config when the tesla-http-proxy
(`TESLA_PROXY_URL`) is configured; otherwise the tombstone is still cleared and
the car is provisioned by the next Tesla link's passive sync.

**Request.**

```
POST /api/tesla/vehicles/{teslaVehicleId}/re-add
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
```

`{teslaVehicleId}` is the **Tesla vehicle id** (NOT a Prisma cuid like §7.12): at
re-add time the local `Vehicle` row has been deleted, so the tombstone's
`(user_id, tesla_vehicle_id)` composite is the only stable handle for a removed
car.

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{teslaVehicleId}` → `400 invalid_request`.
2. **Clear the tombstone** for the caller's OWN `(userId, teslaVehicleId)`
   (`ClearTombstone`, scoped `WHERE user_id = <caller>`). Idempotent — an absent
   tombstone is a clean no-op (`wasTombstoned:false`). On an actual clear it
   writes a `vehicle_readd_allowed` audit row in the same transaction (§4.2).
3. **Best-effort re-provision** the single owned car matching `teslaVehicleId`:
   resolve the owner's Tesla token (with auto-refresh), list the caller's
   Fleet-API vehicles, and provision only an **OWNER-access** match
   (`UpsertOwnedVehicle`, owner-scoped and cross-user-safe) + push its stream
   config. Any failure is non-fatal (`provisioned:false`) — the tombstone clear is
   the durable un-trap; the next link's passive sync provisions the car.
4. Respond `200` with the honest post-state.

**Ownership — fail-closed at two layers, neither trusting the path param.**
`ClearTombstone` is owner-scoped, so a caller can only clear their **own**
tombstone (never another user's). The re-provision lists the **caller's** fleet
and provisions only an OWNER-access match, so a caller can never re-add a car they
do not own even with a guessed `teslaVehicleId`.

**Idempotent.** Re-adding a car with no tombstone is a clean `200`
(`wasTombstoned:false`); the post-state ("no tombstone blocks this car") is the
same either way.

**Response `200`** (`application/json`):

```json
{
  "readded": true,
  "wasTombstoned": true,
  "provisioned": true
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `readded` | `boolean` | P0 | Authoritative: after this call no tombstone blocks the car, so it is eligible to return (true on both a real clear and an idempotent no-op). |
| `wasTombstoned` | `boolean` | P0 | Whether a tombstone was actually cleared (false when the car had no tombstone — a clean no-op). |
| `provisioned` | `boolean` | P0 | Whether the car was re-provisioned inline. `false` means the un-trap succeeded but the car will be provisioned by the next Tesla link's passive sync (e.g. tokens were cleared on a last-vehicle removal and must be re-linked first). |

**Errors:** `401 auth_failed` (missing/invalid bearer), `400 invalid_request`
(missing `teslaVehicleId`), `405 invalid_request` (non-POST method),
`500 internal_error` (tombstone clear transaction failed — atomic: nothing
cleared, no audit row, no provision; retryable).

### 7.14 `PUT /api/tesla/vehicles/{vehicleId}/plate` (owner license-plate entry — MYR-286)

Owner-authenticated "what plate is on this car?" — the **only** way
`Vehicle.licensePlate` is ever populated, and therefore the write half of the
read surfaces in §7.0 and §7.1.

**Why this endpoint has to exist.** The plate is **not a Tesla value**. The
Fleet API exposes no license plate on any endpoint, in any telemetry field, or
in any proto — there is nothing to sync, backfill, or decode. The column can be
populated only by the owner typing it, and no Next.js/Prisma surface writes it
either, so the Go server owns the write.

**Enablement.** The route is ALWAYS mounted. No tesla-http-proxy, no Tesla
token, and no Tesla call is involved at any point — this is purely a local
owner-scoped DB write.

**Request.**

```
PUT /api/tesla/vehicles/{vehicleId}/plate
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
Content-Type: application/json

{ "plate": "abc 1234" }
```

`{vehicleId}` is the Prisma cuid (the same key as §7.12 — NOT a VIN, and NOT the
Tesla vehicle id used by §7.13; the local `Vehicle` row exists by definition
here).

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `plate` | `string` | **P1** | The plate as the owner typed it. Normalized server-side before validation and storage (see below). An **empty string CLEARS** the stored plate. An omitted key is treated as the empty string. |

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{vehicleId}` → `400 invalid_request`. Non-`PUT` method → `405 invalid_request`.
2. **Normalize, then validate — in that order, and the order is the contract.**
   Normalization is `trim` (leading/trailing whitespace) then `uppercase`, and
   nothing else: interior spacing is preserved verbatim, because `ABC 1234` and
   `ABC1234` are different plates in some jurisdictions and collapsing them
   would silently rewrite the owner's answer. Validation then runs on the
   **normalized** value: at most **10 characters**, drawn only from
   `^[A-Z0-9 -]*$`. So `"  abc 1234  "` is ACCEPTED (it normalizes to
   `"ABC 1234"`), where validating the raw input would have rejected it for
   lowercase and counted its surrounding spaces against the cap. A violation →
   `400 invalid_request`, and nothing is written.
3. Resolve `vehicleId` → owner via `VehicleRepo.GetByID`. Unknown vehicle →
   `404 not_found` (indistinguishable from ownership-filtered — never leaks
   existence). A real ownership mismatch → `403 vehicle_not_owned`. **Identical
   semantics to §7.12.** No write runs on either.
4. Write the normalized plate. The `UPDATE` is itself scoped
   `WHERE "id" = … AND "userId" = <caller>`, so ownership is enforced twice and
   the write is fail-closed regardless of step 3. A zero-row result (the car was
   deleted in the gap) is reported as `404 not_found`, never a false `200`.
5. Respond `200` with the **normalized stored value**.

**Empty string clears.** `{"plate": ""}` — or any whitespace-only submission —
writes the empty string, which is the "no plate set" value on both the column
(`TEXT NOT NULL DEFAULT ''`) and the wire. A clear is an ordinary write, not a
separate `DELETE` verb and not a validation failure.

**Idempotent (§4.5).** PUTting the same plate twice yields the same `200` and
the same stored value. PUTting a value that only differs by case or surrounding
whitespace is also idempotent, because both normalize to the same stored string.

**No migration.** The `Vehicle.licensePlate` column already exists
(`TEXT NOT NULL DEFAULT ''`). This endpoint adds a **narrow Go-side UPDATE
carve-out** on the Prisma-owned `Vehicle` table — the third such carve-out after
the MYR-257 provision and the MYR-258 teardown, and since MYR-320 one of four
(the fourth being the Tesla-sourced `Vehicle.color` write) — documented in
[`data-lifecycle.md`](data-lifecycle.md) §1.4. CG-DL-9 constrains Go *migration*
SQL and does not apply: this ships no migration, and an application-runtime
Prisma UPDATE is the sanctioned class (same as `store.OwnerProvisioner`).

**No WebSocket push.** A plate edit fires **no** `vehicle_update` frame, and a
`vehicle_update` NEVER carries `licensePlate`. A client that needs the new value
immediately either adopts this response optimistically or re-reads §7.0 / §7.1.

**Response `200`** (`application/json`):

```json
{
  "vehicleId": "clxyz1234567890abcdef",
  "licensePlate": "ABC 1234"
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | P0 | Echo of the path parameter. |
| `licensePlate` | `string` | **P1** | The **normalized** value now stored. Empty string when the plate was cleared. Echoed so a client can adopt it without a re-read — there is no WS delta for this field. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `{vehicleId}`, malformed JSON body, or a plate that violates the charset or the 10-character cap **after** normalization. The rejected value is P1 and is NEVER echoed in the message — the envelope describes the rule only. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 403 | `vehicle_not_owned` | Caller does not own the vehicle (matches §7.12). |
| 404 | `not_found` | Unknown vehicle, or ownership-filtered — intentionally indistinguishable (matches §7.12). Also returned when the row disappears between the ownership check and the write. |
| 405 | `invalid_request` | Non-`PUT` method. |
| 500 | `internal_error` | Store-layer failure. Atomic: nothing written; retryable. |

**Observability.** The P1 plate value is NEVER logged. The success line carries
the P0 `vehicle_id` / `user_id` plus a `cleared` boolean — enough to debug
without putting an identifying value in the log stream (§9, CG-DC-2).

---

### 7.15 `POST /api/tesla/vehicles/{vehicleId}/refresh` (owner on-demand state refresh — MYR-315)

Owner-authenticated "bring this car up to date now" — the pull half of an
otherwise entirely push-based surface.

**Why this endpoint has to exist.** Every other read surface is **passive**. The
WebSocket carries only what the car volunteers, and a car that is asleep, in
service, or merely quiet volunteers nothing — so §7.1 keeps returning the last
frame it ever saw, however old. The §7.12/§7.13 backfills fire on *connectivity
edges*, which a parked car does not produce. Without this endpoint a user
staring at stale data has no action available to them at all.

**Enablement.** The route is ALWAYS mounted. The `vehicle_data` read is an
UNSIGNED authenticated read against the direct Fleet API, so it needs no
tesla-http-proxy; only the *wake* uses the command transport (proxy-preferred,
Fleet REST fallback). With no wake transport configured, an already-awake car
still refreshes normally and a sleeping one resolves to `502 command_failed`.

**Request.**

```
POST /api/tesla/vehicles/{vehicleId}/refresh
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
```

`{vehicleId}` is the Prisma cuid (the same key as §7.12 / §7.14 — NOT a VIN, and
NOT the Tesla vehicle id used by §7.13). There is **no request body.**

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{vehicleId}` → `400 invalid_request`. Non-`POST` method → `405 invalid_request`.
2. Resolve `vehicleId` → owner. Unknown vehicle → `404 not_found`
   (indistinguishable from ownership-filtered — never leaks existence). A real
   ownership mismatch → `403 vehicle_not_owned`. **Identical semantics to §7.12
   and §7.14.** Neither touches Tesla.
3. **Freshness short-circuit.** If a LIVE streamed frame has arrived for this
   VIN within the **120 s** stream-authoritative window (the same MYR-300 window
   that gates the REST backfill), respond `200` with `status: "fresh"` and the
   arrival time of that frame. **No Tesla call is made, and the cooldown in
   step 4 is not consumed** — a car that is working perfectly must never be
   rate-limited for it, and refreshing data that is already current would only
   cost the user battery.
4. **Cooldown.** Otherwise, a per-vehicle limiter permits one Tesla-hitting
   refresh per **60 s**. Over the limit → `429 rate_limited` with `Retry-After: 60`.
5. **Wake.** Probe Tesla's cheap vehicle object; if `state` is not `online`,
   wake and retry under the **same bounded wake budget vehicle commands use**
   (§7.9) — 3 attempts, 2 s backoff. Probe-first, so an *online but quiet* car
   (the common case) costs one cheap read and **no wake at all**. Budget spent →
   `503 vehicle_asleep`.
6. **One read.** Exactly ONE `vehicle_data` read, republished through the same
   MYR-260 backfill mapping the connectivity-edge path uses — so the values land
   on the identical broadcast + persist route a streamed frame takes, and a
   client with a live socket sees the resulting `vehicle_update` as usual.
   Respond `200` with `status: "refreshed"`.

**`seatCoolingCapable` comes along free.** The `vehicle_data` read includes the
`vehicle_config` sub-object, so a refresh also re-acquires the REST-only spec
facts `trim` (MYR-279) and `seatCoolingCapable` (MYR-308) — the two fields the
stream can never source. An owner whose car has never produced a connectivity
edge since MYR-308 shipped can therefore populate the capability bit by tapping
refresh, with no drive and no reconnect required.

**Cooldown is in-memory and per-process.** It resets on restart and is not
shared across replicas, so the effective limit under N replicas is up to N
refreshes per minute per vehicle. This is deliberate: the backstop that actually
protects Tesla is the bounded wake budget and the single-read shape, and a
durable counter would cost a DB round trip on every tap to enforce a limit whose
only job is to damp a stuck finger. The limiter is consumed on the path that
dials Tesla, **including** when that path then fails — a `503` does not refund
the token, so a client retrying a sleeping car backs off rather than re-waking
it every second.

**Not idempotent in the §4.5 sense, but safe to repeat.** Two refreshes inside
the cooldown do not produce two Tesla calls; two refreshes outside it produce
two reads and two `lastUpdated` values, which is the point.

**Response `200`** (`application/json`):

```json
{
  "status": "refreshed",
  "lastUpdated": "2026-07-28T12:00:00Z"
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `status` | `string` enum | P0 | `"fresh"` — the live stream already held current truth and no Tesla call was made. `"refreshed"` — the car was woken if necessary and one `vehicle_data` read was published. |
| `lastUpdated` | `string` (RFC 3339, UTC) | P0 | The instant the data now reflects: the arrival time of the last live frame when `fresh`, the completion time of the REST read when `refreshed`. Matches the `lastUpdated` semantics of §7.0 / §7.1. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `{vehicleId}`. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer, or the owner has no usable Tesla token (account not linked, or expired beyond refresh — re-link required). |
| 403 | `vehicle_not_owned` | Caller does not own the vehicle (matches §7.12 / §7.14). |
| 404 | `not_found` | Unknown vehicle, or ownership-filtered — intentionally indistinguishable (matches §7.12 / §7.14). |
| 405 | `invalid_request` | Non-`POST` method. |
| 429 | `rate_limited` | This vehicle was refreshed within the last 60 s. Carries `Retry-After: 60`. Never returned for a `fresh` short-circuit. |
| 502 | `command_failed` | The Fleet API read failed for a reason a wake cannot fix, the publish failed, or no wake transport is configured for a car that needs waking. Retryable. |
| 503 | `vehicle_asleep` | The wake budget was exhausted, or the car dropped back to sleep between the wake probe and the read (Tesla `408`). Transient — the SDK backs off. |
| 500 | `internal_error` | Store-layer failure during the ownership lookup. Nothing was read or written. |

**Observability.** VINs are redacted to last-4 in every log line (§9, CG-DC-2).
The `vehicle_data` payload is P1 and is NEVER logged, not even on a decode
failure — only the redacted VIN and the HTTP status.

---

### 7.16 `PUT /api/tesla/vehicles/{vehicleId}/service-window` (owner expected-back entry — MYR-316)

Owner-authenticated "when do you expect this car back?" — the **fallback** half of
the `serviceEstimatedEndAt` read surface in §7.0 / §7.1, and the only value on
that field a human ever types.

**Why this endpoint has to exist.** Tesla's own estimate is the better answer and
the server always prefers it — but Tesla very often has no answer at all. The
Fleet API's `GET /api/1/vehicles/{vin}/service_data` returns a body whose fields
are **all nullable**, and an **all-null body is the normal shape for a visit with
no appointment record**: the car is genuinely in service and Tesla simply does
not know when it will be done. Without this endpoint that case is permanently
`null` — the owner is staring at an "In Service" badge that cannot say *when*,
and the rider's scheduling picker has no floor to build on even though the owner
was told "Thursday afternoon" at the counter. The owner is the only remaining
source of that fact, so the Go server owns the write.

**Enablement.** The route is ALWAYS mounted. No tesla-http-proxy, no Tesla token,
and no Tesla call is involved at any point — this is purely a local owner-scoped
DB write against the Go-owned `go_vehicle_control_state` side table.

**Request.**

```
PUT /api/tesla/vehicles/{vehicleId}/service-window
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
Content-Type: application/json

{ "expectedEndAt": "2026-07-29T18:00:00Z" }
```

`{vehicleId}` is the Prisma cuid (the same key as §7.12 / §7.14 / §7.15 — NOT a
VIN, and NOT the Tesla vehicle id used by §7.13).

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `expectedEndAt` | `string` (RFC 3339) or `null` | P0 | When the owner expects the car back. MUST be in the FUTURE. **Four accepted spellings CLEAR the value** — see below. Written to `service_expected_end_at`; it is the FALLBACK, so Tesla's `service_etc` still outranks it on the next read. |

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{vehicleId}` → `400 invalid_request`. Non-`PUT` method → `405 invalid_request`.
2. Decode the body. **Clearing has four accepted spellings, all equivalent:** the
   key ABSENT, an explicit `null`, an EMPTY or whitespace-only string, and an
   EMPTY BODY. All four write SQL `NULL` to `service_expected_end_at`. There is
   deliberately no separate `DELETE` verb: "I no longer know when it's coming
   back" is an ordinary answer to the same question, and a client that renders a
   cleared text field should not have to switch HTTP methods to submit it.
3. Otherwise parse as RFC 3339. Unparseable → `400 invalid_request`, message
   `expectedEndAt must be an RFC 3339 date-time`. **The value must be in the
   FUTURE** — a past or present instant is `400 invalid_request` with
   `expectedEndAt must be in the future`. A service window that has already
   elapsed is not information, and accepting one would silently install a
   scheduling floor (§7.8) that every future booking already clears.
4. Resolve `vehicleId` → owner via `VehicleRepo.GetByID`. Unknown vehicle →
   `404 not_found` (indistinguishable from ownership-filtered — never leaks
   existence). A real ownership mismatch → `403 vehicle_not_owned`. **Identical
   semantics to §7.12 and §7.14.** No write runs on either.
5. Write `service_expected_end_at` through the dedicated writer in
   [`internal/store/vehicle_service_window.go`](../../internal/store/vehicle_service_window.go)
   — **not** the shared per-field COALESCE control-state upsert the rest of that
   table uses, which cannot express a NULL write and so could never clear.
   Store failure → `500 internal_error`; nothing written, retryable.
6. Respond `200` with the value now stored in the owner column.

**The ownership check is load-bearing here in a way it is not for §7.14.** The
plate endpoint writes the Prisma-owned `Vehicle` table, whose `UPDATE` re-scopes
`WHERE "userId" = <caller>` — a belt-and-braces second enforcement that makes a
zero-row write fail closed even if the check above were wrong. The
`go_vehicle_control_state` side table has **no `userId` column** (CG-DL-9: no
Prisma FKs), so there is no owner-scoped SQL predicate to fall back on and step 4
is the *only* thing standing between a caller and another user's car. Treat it
as such: it is not a duplicate of the read-path filter.

**Precedence — this endpoint writes the FALLBACK, Tesla wins.** The emitted
`serviceEstimatedEndAt` is `COALESCE(service_etc, service_expected_end_at)`, so
a `200` from this endpoint does **not** guarantee the value will appear on the
next `/snapshot`: if Tesla's own `service_etc` is (or later becomes) non-null it
takes precedence. That is why the response echoes the **owner column** rather
than the resolved field — echoing the resolved value would make a client believe
its write had been overruled when it has merely been outranked, and would leave
it with no way to display the value the owner just typed. A client that needs the
resolved window re-reads §7.0 / §7.1.

**Auto-clear when the car leaves service.** The ServiceStatusMonitor physically
clears BOTH columns when it observes the vehicle leaving `in_service`, so an
owner's entry is scoped to the visit it was typed for and can never leak into the
next one. Combined with the in-service emission gate (a non-`in_service` vehicle
reads `null` regardless of column contents), a stale window is impossible in both
directions. The consequence worth stating plainly: **an owner may need to re-enter
the value for a subsequent visit** — that is intended, not a bug.

**Idempotent (§4.5).** PUTting the same instant twice yields the same `200` and
the same stored value; so does clearing twice.

**No WebSocket push.** A service-window edit fires **no** `vehicle_update` frame,
and a `vehicle_update` NEVER carries `serviceEstimatedEndAt` — Tesla has no proto
for it, so there is nothing to stream and **no fleet-config change** was involved.
A client that needs the new value immediately either adopts this response
optimistically or re-reads §7.0 / §7.1.

**Response `200`** (`application/json`):

```json
{
  "vehicleId": "clxyz1234567890abcdef",
  "expectedEndAt": "2026-07-29T18:00:00Z"
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | P0 | Echo of the path parameter. |
| `expectedEndAt` | `string` (RFC 3339, UTC) or `null` | P0 | The value now stored in the OWNER column `service_expected_end_at` — **not** the resolved `serviceEstimatedEndAt`. `null` after a clear. Tesla's `service_etc` may still outrank it on the next read; see "Precedence" above. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `{vehicleId}`; malformed JSON body; `expectedEndAt` not parseable as RFC 3339 (`expectedEndAt must be an RFC 3339 date-time`); or `expectedEndAt` not in the future (`expectedEndAt must be in the future`). Clearing is never an error. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 403 | `vehicle_not_owned` | Caller does not own the vehicle (matches §7.12 / §7.14). |
| 404 | `not_found` | Unknown vehicle, or ownership-filtered — intentionally indistinguishable (matches §7.12 / §7.14). |
| 405 | `invalid_request` | Non-`PUT` method. |
| 500 | `internal_error` | Store-layer failure. Atomic: nothing written; retryable. |

**Observability.** The success line carries the P0 `vehicle_id` / `user_id` plus
a `cleared` boolean, mirroring §7.14. The timestamp itself is P0 operational
timing — the same tier as `Vehicle.status` — so it is log-safe and needs no
redaction; VINs still redact to last-4 (§9, CG-DC-2). Handler:
[`internal/telemetry/vehicle_service_window_handler.go`](../../internal/telemetry/vehicle_service_window_handler.go);
wiring `cmd/telemetry-server/wiring_vehicle_service_window.go`.

---

### 7.17 Push notification device registry (MYR-186)

The address book behind ride-lifecycle push notifications: two user-scoped
operations on one path, letting a signed-in phone say "reach me here" and
"stop reaching me here."

**Why this exists.** Every client-facing signal before MYR-186 travelled the
WebSocket, which means it only arrived while the app was in the foreground
holding a live socket. That is exactly the wrong assumption for the ride
lifecycle: the owner who needs to see an incoming request has their phone in a
pocket, and the rider who needs to know their car has arrived is not staring at
a map. APNs is the only channel that reaches a locked screen, and APNs needs a
per-installation device token that only the device itself can supply.

**Both operations are user-scoped and there is no vehicle in the path**, so —
unlike §7.12 / §7.14 / §7.16 — there is no ownership check to perform: the JWT
subject *is* the resource owner, the same shape as the §7.6 / §7.7 `/api/users/me`
surface.

**Always mounted.** The routes exist whether or not the deployment carries APNs
credentials. A client must be able to register before the secrets are set, so
that the first deploy carrying them can reach phones immediately instead of
waiting for every installed app to relaunch.

**Classification.** `deviceToken` is **P1** — a device identifier and a
capability (anyone holding it plus the team's APNs key can push to that phone).
It is stored raw in the Go-owned `go_push_devices` table (migration 0019) and
protected by log redaction rather than app-level encryption; see
`data-classification.md` §1.14 and §3.2. Consequences that show up in this
contract: the token is **never echoed** in a success response or an error
envelope, and only an 8-character prefix ever reaches a log line.

#### 7.17.1 `PUT /api/push/devices` (register / refresh)

**Request.**

```
PUT /api/push/devices
Authorization: Bearer <app session JWT>        # device owner = JWT sub
Content-Type: application/json

{ "deviceToken": "aabbccdd…", "sandbox": false }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `deviceToken` | `string` | **P1** | The APNs token for this app installation. Required. Trimmed of surrounding whitespace; must be non-empty, at most 256 characters, and free of embedded whitespace or control characters. |
| `sandbox` | `boolean` | P0 | `true` when the token was minted by a development or TestFlight build. Optional, defaults to `false`. **Not cosmetic:** a sandbox token is only valid against `api.sandbox.push.apple.com` and a release token only against `api.push.apple.com`; sending to the wrong gateway returns `BadDeviceToken`, which the server treats as a permanent rejection and deletes the row. |

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Non-`PUT`
   method → `405 invalid_request`.
2. Strict-decode the body (unknown keys are a `400`, matching §7.14 — a typo'd
   key must fail loudly rather than silently registering an empty token).
   Malformed JSON or a token that violates the rule above → `400 invalid_request`,
   describing the RULE and **never echoing the value**.
3. Upsert on `device_token`, stamping `last_seen_at`.

**Idempotent (§4.5), and re-parenting is the point.** The conflict target is the
token alone, **not** `(userId, deviceToken)`. A token identifies a physical
installation, so when a second person signs in on the same phone the row must
transfer to them. Keying on the pair would instead leave two rows claiming one
device and keep delivering the previous occupant's ride notifications to the new
occupant's lock screen. The same upsert refreshes `sandbox`, because a device can
move between a TestFlight and an App Store build.

**Response `200`** (`application/json`):

```json
{ "registered": true, "sandbox": false }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `registered` | `boolean` | P0 | Always `true` on a `200`. |
| `sandbox` | `boolean` | P0 | The flavour now stored. **The response deliberately does NOT echo `deviceToken`** — it is P1, the caller already knows the value it sent, and echoing it would put the token in every client log and proxy trace for no benefit. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Malformed JSON; unknown key; missing/empty `deviceToken`; token longer than 256 characters; token containing whitespace or control characters. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`PUT` method. |
| 500 | `internal_error` | Store-layer failure. Nothing written; retryable. |

#### 7.17.2 `DELETE /api/push/devices` (unregister)

**Request.** Same path, same body shape; `sandbox` is ignored (a token is
unregistered by identity, not by flavour).

```
DELETE /api/push/devices
Authorization: Bearer <app session JWT>
Content-Type: application/json

{ "deviceToken": "aabbccdd…" }
```

**Caller-scoped by SQL, not just by check.** The `DELETE` carries
`WHERE user_id = <caller>`, so one person can never unregister another's phone
even though the token alone would identify the row.

**Response `200`** (`application/json`):

```json
{ "unregistered": true }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `unregistered` | `boolean` | P0 | Whether a registration was actually removed. **`false` covers BOTH "already gone" and "registered to somebody else", deliberately indistinguishable** — otherwise the endpoint would be an oracle for whether an arbitrary token is registered, and to whom. Sign-out is idempotent: a miss is still a `200`. |

**Errors:** identical to §7.17.1, except `405 invalid_request` is a non-`DELETE`
method.

#### Server-side sends (informative — no client contract)

The registry exists to feed three internal bus seams. There is **no REST or
WebSocket surface for the notifications themselves**; this subsection documents
the behavior a client can rely on.

| Bus topic | Audience | Notification title | Body |
|-----------|----------|--------------------|------|
| `ride.request.created` (on-demand) | vehicle **owner** | `«FirstName» wants a ride` (`New ride request` when unresolved) | `Open MyRoboTaxi to accept or decline.` |
| `ride.request.created` (scheduled) | vehicle **owner** | `«FirstName» requested a scheduled ride` (`New scheduled ride request`) | `Open MyRoboTaxi to accept or decline.` |
| `ride.status.changed` → `accepted` | **rider** | `Your ride is confirmed` | `Open MyRoboTaxi to see the details.` |
| `ride.status.changed` → `declined` | **rider** | `«Vehicle» can't take this ride` (`Your car can't take this ride`) | `Try booking another car.` |
| `ride.status.changed` → `arrived` | **rider** | `Your car is here — your turn to start` | `Open MyRoboTaxi to start the ride.` |
| `ride.due` | **rider** | `«Vehicle» is heading your way` (`Your car is heading your way`) | `Your scheduled pickup is coming up.` |
| `ride.due` (MYR-535) | vehicle **owner** (suppressed on a self-ride — one phone, one fact, the rider push alone survives) | `«FirstName»'s pickup is coming up` (`Your rider's pickup is coming up`) | `Your car has the route — time to head out.` |
| `ride.status.changed` → `cancelled` **by the OWNER** (MYR-522, `cancelledBy == "owner"`) | **rider** | `«Vehicle» had to cancel your ride` (scheduled: `«Vehicle» had to cancel your scheduled ride`; fallback `Your car …`) | `Try booking another car.` |
| `ride.status.changed` → `cancelled` **by the RIDER of a committed ride** (MYR-537, `cancelledBy == "rider"` and the pre-check status was `accepted`/`arrived`/`enroute` — a `requested` cancel stays silent, and so does an event carrying no previous status) | vehicle **owner** (suppressed on a self-ride) | `«FirstName» cancelled the ride` (`Your rider cancelled the ride`) | `No need to continue — the ride has ended.` |

Every other transition sends nothing — each is either the recipient's own
action or invisible to them. The two `cancelled` forks are keyed on the same
stamp and are disjoint: an OWNER's cancel wakes the rider (MYR-522), a RIDER's
cancel of a COMMITTED ride wakes the owner (MYR-537), and a rider's cancel of
a still-`requested` ride wakes nobody. An un-upgraded publisher omits
`cancelledBy` (and `PreviousStatus`) entirely, which reads as the silent arm —
exactly the pre-MYR-522/537 behaviour.

**Payload policy — first names and vehicle nicknames ONLY.** A notification
renders on a **locked screen**, to whoever is holding the phone. Pickup and
dropoff labels, street addresses, coordinates and passenger phone numbers are P1
(§1.9 of `data-classification.md`) and **never** appear in a notification. The
APNs `userInfo` carries exactly one key, `{"rideId": "…"}`; a client that needs
anything more refetches §7.8 detail over authenticated REST.

**Scheduled rides do not name the time.** The server holds `scheduledFor` in UTC
and knows nothing about the rider's or the owner's time zone, so any absolute
rendering here would be either wrong (`5:30 PM` in the wrong zone) or unreadable
(`Jul 31, 5:30 PM UTC`). The notification says only that the request is
scheduled; correct local rendering belongs to the client, which knows the
device's zone.

**At-most-once is NOT guaranteed (v1).** The bus makes no exactly-once promise,
and `ride.status.changed` is published on every lifecycle mutation — including a
reschedule sub-state change, which re-publishes the ride's *unchanged* main
status. So an accepted ride that is later rescheduled can produce a second
"Your ride is confirmed". This is accepted for v1: a duplicate notification is a
minor annoyance to a human, whereas a missed one is a rider standing on a
sidewalk. `ride.due` has no such exposure — its publisher holds a one-winner
latch for the ride's whole lifetime (§7.8's reservation-due seam).

**Self-healing registry.** APNs answers `410 Unregistered` (and `400
BadDeviceToken`) for a token that will never accept another push; the sender
deletes that row. Since `go_push_devices` carries no Prisma FK (CG-DL-9),
this feedback loop — not a cascade — is what keeps the table from accumulating
dead installations.

**Kill-switch and keyless operation.** `PUSH_ENABLED=false` stops all sending;
so does a deployment with no `APNS_KEY_P8` / `APNS_KEY_ID`. In both states the
endpoints stay mounted, registrations still persist, and each would-be
notification is logged as `push skipped` — the service runs normally, it simply
does not reach phones. See `docs/deployment.md`.

**Observability.** The registration lines carry the P0 `user_id`, a `sandbox`
flag, and the **8-character token prefix only** — never a whole token
(`data-classification.md` §3.2). The send line carries `topic`, `ride_id`,
`user_id` and device counts, and deliberately **not** the notification copy,
which embeds a first name. Handlers:
[`internal/push/devices_handler.go`](../../internal/push/devices_handler.go);
consumers `internal/push/{notifier,notifier_send,copy}.go`; sender
`internal/push/{apns,token}.go`; store
[`internal/store/push_device_repo.go`](../../internal/store/push_device_repo.go);
wiring `cmd/telemetry-server/wiring_push.go`.

---

### 7.18 `PUT /api/tesla/vehicles/{vehicleId}/ride-share` (owner ride-share pause toggle — MYR-342)

Owner-authenticated "am I lending this car out right now?" — the **only** writer
of the `rideShareEnabled` read surface in §7.0 / §7.1, and the switch behind the
three ride-request gates in §7.8.

**Why this endpoint has to exist.** Before it, an owner had exactly two ways to
stop riders booking their car and **neither was a switch**. They could mark it
`in_service` — a lie about where the car is, and one MYR-313 deliberately lets
SCHEDULED rides through anyway, so it does not even work. Or they could decline
every incoming request by hand, forever, which is not a state at all but a chore.
Nothing in the system could express *"the car is fine; I am simply not lending it
out at the moment"* — a normal, temporary, open-ended intention that has nothing
to do with the vehicle's condition. This endpoint is that expression.

**Enablement.** The route is ALWAYS mounted. No tesla-http-proxy, no Tesla token,
and no Tesla call is involved at any point — this is purely a local owner-scoped
DB write against the Go-owned `go_vehicle_control_state` side table, and there is
no Tesla concept of "this owner is lending their car out" for it to talk to.
Unconditional mounting is also the **fail-safe** direction, which is worth stating
because it runs against the usual instinct: a route gated on some capability, with
the gate off, would leave an owner unable to pause a car the rider-facing catalog
still shows as available. A route that is always present cannot strand an owner
that way.

**Request.**

```
PUT /api/tesla/vehicles/{vehicleId}/ride-share
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
Content-Type: application/json

{ "enabled": false }
```

`{vehicleId}` is the Prisma cuid (the same key as §7.12 / §7.14 / §7.15 / §7.16 —
NOT a VIN, and NOT the Tesla vehicle id used by §7.13).

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `enabled` | `boolean` | P0 | **REQUIRED.** `true` resumes ride requests for this vehicle; `false` PAUSES them. Written to `ride_share_enabled`. There is no third state and no "clear" — see below. |

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{vehicleId}` → `400 invalid_request`. Non-`PUT` method → `405 invalid_request`.
2. Decode the body strictly (`additionalProperties:false` — unknown keys are a
   `400`). **`enabled` is REQUIRED, and this is the one place §7.18 deliberately
   diverges from its §7.16 template.** The service-window body treats an absent
   key, an explicit `null`, an empty string and an empty body as four spellings of
   the same *clear*. This field has no clear: **both values are decisions.** A body
   that names neither is a client bug, and guessing would either un-pause a car its
   owner paused or withdraw a car its owner never touched — so all four of those
   spellings are `400 invalid_request` here.
3. Resolve `vehicleId` → owner via `VehicleRepo.GetByID`. Unknown vehicle →
   `404 not_found` (indistinguishable from ownership-filtered — never leaks
   existence). A real ownership mismatch → `403 vehicle_not_owned`. **Identical
   semantics to §7.12 / §7.14 / §7.16.** No write runs on either.
4. Write `ride_share_enabled` through the dedicated writer in
   [`internal/store/vehicle_ride_share.go`](../../internal/store/vehicle_ride_share.go)
   — **not** the shared per-field COALESCE control-state upsert the rest of that
   table uses. Store failure → `500 internal_error`; nothing written, retryable.
5. Respond `200` with the resolved stored value.

**No share tier grants this toggle, at any level.** Every other rider-adjacent
surface admits a viewer at some tier; this one admits none. A viewer holding even
the top `rides` tier (§7.5.0) arrives here legitimately authenticated and is
refused `403 vehicle_not_owned` — the pause is the **owner's** switch, and a rider
able to flip it would invert the feature. The handler consults no share reader at
all, deliberately, because there is no tier that should grant it.

**The ownership check is load-bearing here in exactly the way §7.16's is.** The
`go_vehicle_control_state` side table has **no `userId` column** (CG-DL-9: no
Prisma FKs), so unlike the §7.14 plate write there is no owner-scoped SQL
predicate underneath to fail closed if the check above were wrong. Step 3 is the
*only* thing standing between a caller and another user's car.

**Why the write bypasses the shared control-state upsert.** Two reasons, and the
second is a security property rather than a style preference:

1. The shared `queryUpsertControlState` writes `col = COALESCE(EXCLUDED.col,
   existing.col)`, in which a `NULL` means *leave alone*. That form is right for
   telemetry — a frame that does not carry a field must not erase it — but it
   cannot express a write of `false`, which is the entire point of this field:
   `false` is not the absence of an opinion, it is the owner's decision.
2. `ControlStateUpdate` is fed by **telemetry**. If this column had a slot there,
   any routine frame from the car could silently re-enable ride sharing on a
   vehicle its owner had paused. Keeping the write on its own statement, reachable
   only from this owner-authenticated handler, means the pause can be lifted by
   exactly one actor: the owner. The property is asserted, not merely commented —
   `TestVehicleRepo_RideShareIsNotReachableFromTelemetry`.

**Not nullable, and the default is load-bearing.** `ride_share_enabled` is
`BOOLEAN NOT NULL DEFAULT true` (migration **0021**) — alone among the columns on
that side table, every one of which is otherwise honest-unknown-nullable. There is
no unknown state to be honest about here: a car whose owner has never touched the
toggle **is** accepting rides. The `DEFAULT` does double duty: it backfills every
pre-MYR-342 row to the correct prior behaviour (all cars were shareable, because
there was no way not to be), and the read path spells the same default a second
time as `COALESCE(gcs.ride_share_enabled, TRUE)` because a car with no side-table
row at all must be indistinguishable from an explicitly-enabled one.

**What a pause actually does — three enforcement layers (§7.8).** The flag is not
advisory. While `rideShareEnabled` is `false`:

| Layer | Where | Effect |
|-------|-------|--------|
| Create | `POST /api/ride-requests` | `409 vehicle_unavailable`, `Ride sharing is paused for this vehicle`. Instant **and** scheduled. |
| Accept | `POST /api/ride-requests/{id}/accept` | Same `409`, same message — the backstop for requests already outstanding when the switch moved. Instant **and** scheduled. `decline` is never gated. |
| Dispatch | Reservation sweeper (MYR-179) | A due reservation is **HELD**, not claimed and not pushed. Re-decided every tick; the 30-minute lateness ceiling expires it honestly if the owner never re-enables. |

**The scheduled deviation, stated once and plainly.** MYR-313 exempts SCHEDULED
rides from the `vehicle_unavailable` availability gate. **The pause does NOT
inherit that exemption, on any layer.** MYR-313's argument is that a reservation
days out says nothing about the car's status today, because a service visit
**ENDS** — the car will be back, so refusing strands the owner over a condition
that will have cleared by the time it matters. An owner's pause is **open-ended**:
nothing ends it but the owner reaching for the switch again. Applying the
exemption would let a rider book a car withdrawn indefinitely, and would leave the
request sitting in the owner's queue — the treadmill this endpoint exists to end.
The lateness ceiling is what keeps that strictness safe rather than merely harsh:
a reservation nobody re-enables resolves itself, honestly and visibly, instead of
hanging forever.

**Warning the owner BEFORE the pause (MYR-360).** The dispatch layer above is a
backstop, not a courtesy: a CONFIRMED reservation on a paused car is held, then
expired as `reservation_expired`, so the rider finds out nobody is coming **half
an hour after their pickup time**. The client therefore asks
`GET /api/ride-requests/incoming?upcomingForVehicle={vehicleId}` (§7.8) *before*
calling this endpoint, names the next accepted reservation to the owner, and
offers an immediate `POST /api/ride-requests/{id}/decline` — which MYR-360
extended to accept `accepted → declined` for scheduled rides. **Nothing about
this endpoint changes**: it does not read reservations, does not block on them,
and does not decline anything on the owner's behalf. The hold-then-expire
backstop stays exactly as described above, because it is what covers the pauses
no dialog ever sees (a crash, an offline client, a pause made elsewhere).

**Idempotent (§4.5).** PUTting `false` twice yields the same `200` and the same
stored value; so does `true`. The toggle is not a one-way door in either
direction.

**No WebSocket push.** A ride-share edit fires **no** `vehicle_update` frame, and
a `vehicle_update` NEVER carries `rideShareEnabled` — the value is owner intent,
not telemetry, so Tesla has no proto for it and **no fleet-config change** was
involved. A client that needs the new value immediately adopts this response;
everyone else sees it on the next read of §7.0 or §7.1.

**Response `200`** (`application/json`):

```json
{
  "vehicleId": "clxyz1234567890abcdef",
  "enabled": false
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` (cuid) | P0 | Echo of the path parameter. |
| `enabled` | `boolean` | P0 | The RESOLVED stored value. This server writes exactly what was asked, so it always equals the request; it is echoed anyway because the contract's rule is that the client adopts the **server's** answer, which keeps a future server free to refuse or coerce without breaking clients. Unlike §7.16 there is no precedence to worry about — nothing outranks the owner here. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `{vehicleId}`; malformed JSON; unknown body keys; or `enabled` absent, `null`, or not a boolean (`enabled is required`). Unlike §7.16, silence is NOT a valid answer. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 403 | `vehicle_not_owned` | Caller does not own the vehicle — including a viewer at ANY share tier (matches §7.12 / §7.14 / §7.16). |
| 404 | `not_found` | Unknown vehicle, or ownership-filtered — intentionally indistinguishable (matches §7.12 / §7.14 / §7.16). |
| 405 | `invalid_request` | Non-`PUT` method. |
| 500 | `internal_error` | Store-layer failure. Atomic: nothing written; retryable. **Never reported as success** — a `200` over a failed write would leave an owner believing their car is paused while it is still taking requests. |

**Observability.** The success line carries the P0 `vehicle_id` / `user_id` plus
an `enabled` boolean, mirroring §7.14 / §7.16. The value is P0 operational
availability — the same tier as `Vehicle.status` — so it is log-safe and needs no
redaction; VINs still redact to last-4 (§9, CG-DC-2). Handler:
[`internal/telemetry/vehicle_ride_share_handler.go`](../../internal/telemetry/vehicle_ride_share_handler.go);
gate helper `internal/telemetry/ride_share_gate.go`; wiring
`cmd/telemetry-server/wiring_vehicle_ride_share.go`.

---

### 7.19 Notification preferences (MYR-349)

Two user-scoped operations on one path, letting a signed-in person say which
categories of notification may wake their phones — and, for the first time,
making the app's Settings switches mean something.

**Why this exists: the switches were a LIE.** Both Settings screens have shipped
a Notifications card since MYR-170. Every switch on it was backed by a private
client-side struct: it moved, it animated, it persisted nowhere and it gated
nothing. MYR-186 then built the whole delivery pipeline — §7.17's registry, the
APNs sender, the ride-lifecycle notifier — with **no preference layer at all**,
so on the day push became real an owner who had switched "Charging complete" off
kept receiving it and a rider who had switched "Pick-up & arrival alerts" off
kept being woken at pickup. This endpoint is the storage those switches always
implied, and `internal/push` consults it before every send.

**Both operations are user-scoped and there is no vehicle in the path**, so —
like §7.6 / §7.7 / §7.17 — there is no ownership check to perform: the JWT
subject *is* the resource. There is deliberately **no `userId` anywhere** in the
request or the response; a body carrying one is rejected by the strict decode
rather than ignored.

**Always mounted**, for the same reason §7.17 is: a person must be able to
silence a category whether or not this deployment carries APNs credentials, and
the answer has to be stored before the notifier that honours it is wired.

**These are DELIVERY preferences, not authorization.** A category that is off is
a silence, not a denial: it stops this service waking a phone and changes
nothing about what the account may read over REST or the telemetry socket. Do
not use it as an access control.

#### 7.19.0 The five categories

| Wire key | Column | Covers |
|----------|--------|--------|
| `rideLifecycle` | `ride_lifecycle` | The **whole** ride status class — a new request and the rider's `enroute` start reaching an OWNER, and `accepted` / `declined` / `arrived` / `completed` / `expired` reaching a RIDER. |
| `driveStarted` | `drive_started` | The owner's car began a drive. |
| `driveCompleted` | `drive_completed` | The owner's car finished a drive. |
| `chargingComplete` | `charging_complete` | The owner's car finished charging. |
| `viewerJoined` | `viewer_joined` | Somebody redeemed an invite to the owner's car (§7.5.5). |

**Owner-only and rider concepts share ONE row.** A person is not an owner or a
rider — MYR-343 established that an account can be both at once, and the mode
switch lets one person be either within a session. Keying preferences by
`(user, role)` would give the same human two answers to "should this phone
ring", and the phone is the same phone. **The notifier decides** which column
applies to a given send, because only it knows which side of a ride the
recipient is standing on.

**`rideLifecycle` is ONE category on purpose**, and this is the open design
question worth naming. The rider's Settings screen renders TWO switches over it
("Request accepted / declined", "Pick-up & arrival alerts"), because **no send
site distinguishes them**: `arrived` and `accepted` travel the same handler, the
same alert builder and the same fan-out. Minting a second column would create a
preference the server could store and would never honour differently — the exact
class of lie this issue exists to remove. The two rows therefore move together
on the client. If they must become independent, the send sites have to split
first, not the column.

**One event can now wake BOTH parties, and `rideLifecycle` gates each of them
against its own row.** MYR-462 added the first owner-facing status transition:
`arrived → enroute` — the rider pressing Start ride — is the owner's only signal
that their car has left the kerb with somebody aboard, and before it existed the
owner learned the trip had started only by opening the app and waiting for a
refresh. `ride.status.changed` therefore fans out to the RIDER for
`accepted` / `declined` / `arrived` and to the OWNER for `enroute`, and the gate
reads the recipient's row in each case, never the other party's. The two never
both fire on one transition, and the owner-side send is **suppressed entirely
when the owner IS the rider** — a self-ride must not report the rider's own tap
back to them.

**Four of the five categories have no sender yet.** `internal/push` has exactly
four fan-out sites and **all four are `rideLifecycle`** — there is no
drive-started, drive-completed, charging-complete or viewer-joined notifier in
this service at all (see the audit in the MYR-349 PR). Those four columns are
created now anyway, because their switches are already on the owner's screen:
storing the answer is what stops the toggle being a lie a second time, and it
means whichever notifier eventually sends them is born gated rather than
retrofitted.

**Storage.** Go-owned `go_push_prefs` (migration 0022), primary key `user_id`,
five `BOOLEAN NOT NULL DEFAULT TRUE` columns, no Prisma FK (CG-DL-9).

**Classification: P0 throughout.** `user_id` is an opaque cuid and the five
values are booleans about delivery — not identifying, no location, no ride
detail, no capability. Unlike §7.17's `deviceToken` there is nothing here to
redact, so the values appear in full in the success log line. See
`data-classification.md` §1.16.

#### 7.19.1 `GET /api/users/me/push-prefs`

**Request.**

```
GET /api/users/me/push-prefs
Authorization: Bearer <app session JWT>
```

**Response `200`** (`application/json`):

```json
{
  "rideLifecycle": true,
  "driveStarted": true,
  "driveCompleted": true,
  "chargingComplete": true,
  "viewerJoined": true
}
```

**ALL FIVE KEYS ARE ALWAYS PRESENT.** No field is optional and none carries
`omitempty`. `omitempty` on a boolean drops `false` — which is the *interesting*
half of every one of these values — and a client reading an absent key as its
own default would render a switch in the opposite position to the one the person
just chose. Clients SHOULD declare these as non-optional booleans so that a
renamed or missing key fails the decode loudly; an optional property would
decode a wrong key to `nil` silently, which is exactly how MYR-362 shipped.

**An account with no stored row is not an error and not an unknown** — it reads
as every category `true`. That is the state every account is in until somebody
moves a switch, so the missing row is the COMMON path, and "no row", "row whose
column was defaulted" and "explicitly enabled" are indistinguishable by design.

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `rideLifecycle` | `boolean` | P0 | Required, never omitted. |
| `driveStarted` | `boolean` | P0 | Required, never omitted. |
| `driveCompleted` | `boolean` | P0 | Required, never omitted. |
| `chargingComplete` | `boolean` | P0 | Required, never omitted. |
| `viewerJoined` | `boolean` | P0 | Required, never omitted. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`GET` method on this handler. |
| 500 | `internal_error` | Store-layer failure. **Deliberately NOT a fabricated all-true default** — answering "everything on" after a database failure would tell somebody their switches had been reset. Retryable. |

#### 7.19.2 `PUT /api/users/me/push-prefs`

**Request.** A **PARTIAL** object: only the categories the client is changing.

```
PUT /api/users/me/push-prefs
Authorization: Bearer <app session JWT>
Content-Type: application/json

{ "chargingComplete": false }
```

**Partial is the contract, not a convenience.** A settings screen changes one
switch at a time. A whole-object PUT would let a second phone — or a stale
screen — write back four values its user never touched, which for this surface
is the difference between a stale toggle and a notification somebody explicitly
silenced arriving anyway. **An omitted key means LEAVE ALONE**, never "set to
`false`".

`{}` is legal and is a plain read-after-write.

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`.
   Non-`PUT` method → `405 invalid_request`.
2. Strict-decode the body — **unknown keys are a `400`**, matching §7.14 / §7.17.
   This matters more here than anywhere else on the surface: on a body where
   every key is optional, a typo'd or renamed category would otherwise return
   `200` having changed **nothing**, the client would show the switch in its new
   position, and the notification would keep arriving. That is the MYR-349 lie
   again, one layer down.
3. Upsert on `user_id`, assigning `COALESCE($n, existing)` per column. On the
   INSERT arm an omitted category takes the same all-on default a missing row
   would have produced; on the UPDATE arm it keeps whatever the person last
   chose.

**Idempotent (§4.5).** Re-sending the same body is a no-op producing the same
response.

**Response `200`** — the **WHOLE set as stored, read back after the write**:

```json
{
  "rideLifecycle": true,
  "driveStarted": false,
  "driveCompleted": true,
  "chargingComplete": false,
  "viewerJoined": true
}
```

**Clients MUST adopt this echo rather than the booleans they sent.** The echo is
produced by the same statement that performed the write (`RETURNING`), so it
carries the four categories the request never mentioned — including any changed
on another device in the meantime. A client that flipped its switch optimistically
and then kept its own value would resurrect a preference somebody switched off
elsewhere. On failure the client MUST roll the optimistic flip back **and say
so**: leaving it up manufactures the exact false belief this endpoint exists to
prevent — an owner walking away believing charging alerts are off while they are
still firing.

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Malformed JSON; **any unknown key**, including a snake_case spelling of a real category or a `userId`. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`PUT` method. |
| 500 | `internal_error` | Store-layer failure. Nothing written; retryable. |

#### 7.19.3 Enforcement, and which way it fails

The gate lives in exactly ONE place — `Notifier.allowed`, called by the single
fan-out that every send site reaches. Each site names its category at the call
site; there is no unset value meaning "always send".

**IT FAILS OPEN, in all four of the ways it can fail**: no preference store
wired, a lookup error, an account with no row, and a category the model has no
column for. All four resolve to every-category-on, i.e. the exact pre-MYR-349
behaviour.

That direction is the product decision, not a shortcut. This package's standing
rule (§7.17) is that a duplicate notification is a minor annoyance to a human
whereas a missed one is a rider standing on a sidewalk — and because nothing
about a ride waits on push, a gate that failed CLOSED would convert a transient
database hiccup into platform-wide silence with no error surfacing anywhere. The
cost of failing open is bounded and visible: somebody who silenced a category
might occasionally receive it.

**The gate reads the RECIPIENT's row, never the other party's.** A ride has two
people and they can disagree: `ride.request.created` wakes the OWNER while
`ride.status.changed` and `ride.due` wake the RIDER.

**A silenced category never reaches the device lookup.** The check runs first, so
a suppressed notification logs `push suppressed by preference` rather than
`push sent … delivered=0`, which in production reads like a delivery failure.

**This is no longer the only gate on the fan-out ([MYR-413](https://linear.app/myrobotaxi/issue/MYR-413)).** A second, narrower one runs immediately after it: a ride-lifecycle banner is skipped when the recipient already has a running Live Activity whose island alert carries the same news — see **§7.21.6** for the rule, its exclusions, and its per-user bound. The two are deliberately ordered preference-first, so a muted category costs one read rather than two, and they are deliberately separate: **this** gate is a person saying they do not want to hear it, while that one is the same person already being told by a better surface. Collapsing them would make "I muted ride updates" and "my lock screen is already showing this" the same fact, and they fail in opposite directions when the Activity ends.

**Observability.** `push suppressed by preference` carries `topic`, `category`
and the P0 `user_id`; a failed lookup logs at ERROR and says it is sending
anyway. The write line carries the resulting five booleans in full (P0, nothing
to redact). Handler:
[`internal/push/prefs_handler.go`](../../internal/push/prefs_handler.go);
categories + gate model `internal/push/prefs.go`; enforcement
`internal/push/notifier_send.go`; store
[`internal/store/push_prefs_repo.go`](../../internal/store/push_prefs_repo.go);
wiring `cmd/telemetry-server/wiring_push.go`.

---

### 7.20 Saved places (MYR-321)

> **Anchored:** FR-9.3 (user-scoped resources), NFR-3.9 (data tiers), NFR-3.21 (self-scoped surfaces), NFR-3.23 (encryption at rest).

The account's Home and Work places — the two shortcut chips the ride-request sheet offers instead of making somebody search for their own house.

**Why this exists.** The same shape of gap §7.19 closed: a lie, not a missing feature. The chips have shipped since the ride sheet landed, backed by a private client-side `@State` struct. They rendered, they tapped, they persisted nowhere. "Home" meant one address on a person's iPhone and a different one on their iPad, and reinstalling the app forgot both. This is the missing storage.

**Account-scoped, not vehicle-scoped, and not device-scoped.** A saved place belongs to a PERSON, so it follows them across every car they own, every car they are a viewer of, and every device they sign in on. Keying it by car would mean re-entering a home address per vehicle and would put it on a surface a co-owner can read; keying it by device is exactly the bug being fixed.

**Self-scoped like §7.6 / §7.7 / §7.19.** There is no vehicle in the path, so there is no ownership check to make: the JWT subject IS the resource. There is no `userId` in any path or body and there must never be one — a `userId` key in the `PUT` body is rejected `400` as an unknown field rather than ignored.

**Never visible to a counterparty.** Sharing a car grants access to the CAR, never to the other person's address book. These rows reach the account that saved them and nobody else — not a co-owner, not a viewer, not the owner of a car this person rides in. There is no viewer-masked projection of this resource because there is no non-self reader of it.

**Two fixed slots, not a favourites list.** `kind` is a closed set of exactly `home` and `work`. They are PEERS, not tiers — unlike `SharePermission` there is no ordering and neither implies the other. A user-named collection would need ordering, renaming, a create endpoint and a per-person cap, none of which the two chips need. Adding a third slot later is an additive contract enum member plus a migration widening the `CHECK`.

**No row means not set.** There is no tombstone and no null-coordinate placeholder: a kind never set and a kind deleted are indistinguishable, and both read as absent. This is why the list is a SPARSE array of 0–2 rows rather than a fixed pair with nullable members — a half-populated place is not a weaker place, it is not a place.

**Storage.** Go-owned `go_saved_places` (migration 0023), `PRIMARY KEY (user_id, kind)`, no Prisma FK (CG-DL-9). Coordinates are stored ENCRYPT-ONLY as base64 AES-256-GCM ciphertext in `lat_enc` / `lng_enc`, following the `go_ride_requests` precedent exactly (NFR-3.23): the table is new, so there is no plaintext column, no dual-write window and no backfill. The repository requires an `Encryptor` at construction (panics on nil) and treats a decrypt failure as a hard read error — there is no fallback column to fall back to.

**Classification: P1, and the highest-value location the platform holds.** See `data-classification.md` §1.17. A ride coordinate is where somebody went once; a saved home coordinate is where they SLEEP, it is durable, and it is re-read on every app launch. `label` is P1 log-redacted but not app-level encrypted — the same tier split `go_ride_requests` makes between `pickup_lat_enc` and `pickup_label`. **Nothing in this surface is ever logged:** no log line carries a coordinate or a label, on success or on failure, and no error envelope echoes the rejected input.

**No §5.2 mask entry**, for the same reason §7.19 has none: the resource is self-scoped, the JWT subject is the only reader, and there is no role dimension to mask across (Rule CG-DC-5 satisfied by this statement).

**Contract:** [`schemas/saved-places.schema.json`](schemas/saved-places.schema.json) (contracts v0.21.0) — `SavedPlaceKind`, `SavedPlace`, `SavedPlacesResponse`, `PutSavedPlaceRequest`.

#### 7.20.0 The two kinds

| Wire value | Column value | Meaning |
|------------|--------------|---------|
| `home` | `'home'` | Where the person lives. |
| `work` | `'work'` | Where the person works. |

Matched **case-sensitively** and lowercase. `Home` is a `400`, not a synonym: accepting variants would let two spellings of one slot reach an upsert whose conflict target is the exact bytes, and the person would end up with two homes, one of which they could no longer see.

#### 7.20.1 `GET /api/users/me/places`

**Request.**

```
GET /api/users/me/places
Authorization: Bearer <app session JWT>
```

**Response `200`** (`application/json`):

```json
{
  "places": [
    {
      "kind": "home",
      "label": "1 Ferry Building · Embarcadero",
      "latitude": 37.7955,
      "longitude": -122.3937
    },
    {
      "kind": "work",
      "label": "3500 Deer Creek Rd · Palo Alto",
      "latitude": 37.3947,
      "longitude": -122.1503
    }
  ]
}
```

**`places` is always present and is an ARRAY, never `null`.** An account that has saved neither kind gets `{"places": []}` — a `200`, never a `404`. That is the state every account is in until somebody saves something, so it is the COMMON path, not an edge case, and clients MUST render it as two empty affordances ("Set home", "Set work").

**The array is SPARSE.** A kind that was never set, or was deleted, is simply absent, so the length is 0, 1 or 2. Clients MUST find a kind by SEARCHING for it, never by index — `places[0]` is not Home.

**The envelope is keyed `places`, deliberately not `items`.** This is not the cursor-paginated envelope of §7.0 / §7.2 / §7.8: the set is bounded BY THE KIND ENUM at two rows, so there is no cursor, no `hasMore`, and nothing an SDK pagination helper could page. The envelope itself is part of the contract and MUST NOT be stripped silently.

**Every field on every row is always present.** No `omitempty` anywhere: `omitempty` on a float drops `0`, and `0` is a real coordinate — the equator and the prime meridian both cross inhabited land — so a client reading an absent key as "unknown" would render a saved place with no pin.

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `places` | `SavedPlace[]` | P1 | Required, never `null`. 0–2 rows, `maxItems: 2`. Server order is `home` then `work`; do not depend on it. |
| `places[].kind` | `"home" \| "work"` | P0 | Required. Self-describing, so a row handed around alone keeps its identity. |
| `places[].label` | `string` | P1 | Required, 1–200 characters. Log-redacted, not encrypted. |
| `places[].latitude` | `number` | P1 | Required, `[-90, 90]`. **Encrypted at rest.** |
| `places[].longitude` | `number` | P1 | Required, `[-180, 180]`. **Encrypted at rest.** |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`GET` method on this handler. |
| 500 | `internal_error` | Store-layer failure, including a coordinate that will not decrypt. Retryable. |

#### 7.20.2 `PUT /api/users/me/places/{kind}`

**Request.** The WHOLE place. The `{kind}` path segment names the slot.

```
PUT /api/users/me/places/home
Authorization: Bearer <app session JWT>
Content-Type: application/json

{
  "label": "1 Ferry Building · Embarcadero",
  "latitude": 37.7955,
  "longitude": -122.3937
}
```

**This is an UPSERT, not a PATCH — the deliberate opposite of §7.19.2.** Push preferences are partial because five notification switches are genuinely independent, and two phones changing two categories must not clobber each other. A label and the coordinate it describes are ONE fact: a partial write there would let a client move the pin while keeping a label that no longer describes it, storing "1 Ferry Building" at an address three miles away. Every write therefore replaces the whole slot, and an omitted field is a `400` rather than "keep the stored value".

**One kind per call.** Setting both Home and Work is two requests.

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`.
2. Validate the `{kind}` path segment against the closed set, case-sensitively. Unknown → `400 invalid_request`. The rejected value is NOT echoed back.
3. Strict-decode the body — **unknown keys are a `400`**, matching §7.14 / §7.17 / §7.19.
4. If the body carries `kind`, it MUST equal the path segment; a mismatch is `400`. It is never honoured over the path — a body that could redirect the write would let a stale client overwrite Home while its URL said Work.
5. Validate `label` (required, trimmed, non-blank, ≤ 200 **runes**) and both coordinates (required, finite, in range).
6. Upsert on `(user_id, kind)`, encrypting both coordinates in one statement. `created_at` is preserved on replace; `updated_at` moves.
7. Return the stored row, scanned back OUT of the database through the decrypt path.

**`kind` in the body is OPTIONAL and REDUNDANT.** It exists only so a client holding a whole `SavedPlace` can post it back without stripping a field. Omitting it is the ordinary case and is exactly equivalent to sending the path value.

**Idempotent (§4.5).** Re-sending an identical body is a no-op producing the same response, so a retry after a dropped response is safe.

**Response `200`** — the stored place, echoed:

```json
{
  "kind": "home",
  "label": "1 Ferry Building · Embarcadero",
  "latitude": 37.7955,
  "longitude": -122.3937
}
```

**`200` on first write as well as on replace — never `201`.** The resource is the SLOT, which always exists in the URL space whether or not a row backs it, so a create and an update are indistinguishable to the caller and neither mints a new address. The echo is read back from the database rather than reflected from the request, so what the client adopts is provably what was persisted.

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `kind` | `"home" \| "work"` | P0 | Optional in the request (must match the path when present); always present in the response. |
| `label` | `string` | P1 | **Required.** 1–200 runes after trimming. Rejected, never truncated. |
| `latitude` | `number` | P1 | **Required.** Finite, `[-90, 90]`. Encrypted before it reaches the database. |
| `longitude` | `number` | P1 | **Required.** Finite, `[-180, 180]`. Encrypted before it reaches the database. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Unknown `{kind}`; body `kind` disagreeing with the path; malformed JSON; unknown key; missing/blank/over-long `label`; missing, non-finite or out-of-range coordinate. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`PUT` method on this handler. |
| 500 | `internal_error` | Store-layer failure, including an encrypt failure. The write did NOT happen — there is no plaintext fallback column, so a coordinate that cannot be sealed is not stored at all. Retryable. |

**Error bodies never echo the input.** A `400` names the offending FIELD and not its value: an out-of-range coordinate is still a location, a label is still an address, and error envelopes end up in crash reporters and log aggregators.

#### 7.20.3 `DELETE /api/users/me/places/{kind}`

**Request.** No body.

```
DELETE /api/users/me/places/work
Authorization: Bearer <app session JWT>
```

**Response `204 No Content`** — always, whether or not a row was there.

**Idempotent, and `404` is deliberately NOT in the table.** A client retrying a dropped `DELETE` must not be told its completed work failed, and "this slot is now empty" is true either way. A `404` would also be a small oracle: it would reveal whether the account had ever set that slot, which is a fact about a person's habits that this endpoint owes nobody.

**A real delete, not a tombstone** — the opposite of the `go_vehicle_shares` revocation model (§7.5). A revoked share is evidence in the CAR OWNER's audit trail; a saved place is a person's own note to themselves, so nobody is owed a record that this account once knew where its owner lived.

**Scoped to the named slot.** Deleting Home leaves Work untouched.

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Unknown `{kind}`. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 405 | `invalid_request` | Non-`DELETE` method on this handler. |
| 500 | `internal_error` | Store-layer failure. Retryable. |

#### 7.20.4 Account deletion

Saved places are deleted as **step 8** of the §7.6 `DELETE /api/users/me` sequence, between the push devices and the refresh tokens and BEFORE the identity transaction.

The position is load-bearing in one direction. `go_saved_places` has no foreign key (CG-DL-9), so nothing cascades: a row left behind after the identity rows go would be AES-256-GCM ciphertext of where a DELETED PERSON LIVES, keyed by a cuid that no longer resolves to anybody and reachable by nothing but a table scan. It would never be read, never be reported, and never go away.

Everything else about the slot is unconstrained — the rows are keyed only by `user_id` and nothing later in the sequence reads them — so it sits next to the push devices because both are personal effects with no counterparty. The step is idempotent (a re-run deletes zero rows), which is what keeps the whole sequence re-runnable. The `account_deleted` audit row gains `savedPlacesDeleted`: a COUNT, never the places themselves (CG-DL-5).

**Observability.** `saved place updated` and `saved place deleted` carry the P0 `user_id` and `kind` only — never a label, never a coordinate. `saved place deleted` also carries `row_removed`, which distinguishes a real delete from a no-op re-run in the log without changing the response. Handler:
[`internal/telemetry/saved_places_handler.go`](../../internal/telemetry/saved_places_handler.go);
validation `internal/telemetry/saved_places_validate.go`; store
[`internal/store/saved_places_repo.go`](../../internal/store/saved_places_repo.go);
coordinate crypto `internal/store/saved_places_scan.go` (reusing
`internal/store/vehicle_gps_encryption.go`); wiring
`cmd/telemetry-server/wiring_saved_places.go`.

---

### 7.21 Live Activity token registration (MYR-172, MYR-398)

> **Anchored:** FR-9.3 (ride-scoped resources), NFR-3.9 (data tiers), NFR-3.21 (party enforcement on every API call).

Two rider-scoped operations on one path, letting the rider's phone say "here is the Live Activity I just started for this ride" and "it is over now." Everything else about the feature — what the lock screen says, when it changes, when it goes away — is a **server-side APNs push**, not an endpoint, and is documented below because a client depends on it even though it cannot call it.

**Why this exists.** §7.17 gave us the first server→phone channel that survives a backgrounded app, but an alert is a MESSAGE: it fires once, it stacks up, and it tells a rider that something happened rather than what is true now. A ride is a **state a rider watches** — the car is 6 minutes away, then 4, then it is here — and the honest rendering of that is a Live Activity on the lock screen, not eleven notifications. The app starts the Activity locally when a ride is accepted (which needs **no permission prompt**, unlike an alert) and ActivityKit hands it a per-Activity push token. These endpoints are how that token reaches the server, which from then on owns what the Activity displays.

**RIDER-ONLY in v1, deliberately.** The vehicle owner is a party to the ride and may READ it (§7.8), but this surface answers a `403` to an owner. An endpoint that accepted an owner's token would quietly create rows the sender then pushes RIDER content to — including the P1 `destination` label, which is on a rider's own lock screen by design and on an owner's by accident. The owner-side Activity is an explicit [MYR-172](https://linear.app/myrobotaxi/issue/MYR-172) follow-up and will arrive with its own content-state; the `(ride, party)` key already admits it without a migration.

**Storage.** Go-owned `go_live_activities` (migration **0025**, plus the six leg-progress columns of migration **0027** and the island-expand high-water mark of migration **0029**), `UNIQUE (ride_request_id, user_id)`, swept 24 hours after the last write. `ride_request_id` is the **first genuine foreign key in the `go_` namespace** — CG-DL-9 bars references to the Prisma-owned schema, but `go_ride_requests` is Go-owned, and the ride's hard-delete paths (owner teardown, account deletion) make `ON DELETE CASCADE` the choice that cannot strand a token addressing a ride that no longer exists. `user_id` remains an unenforced pointer, as CG-DL-9 requires.

**Classification.** `activityToken` is **P1** — a capability: whoever holds it plus the team's APNs key can write to that phone's lock screen. Stored raw and protected by log redaction rather than app-level encryption, the same posture and the same rationale as `go_push_devices.device_token`; see [`data-classification.md`](data-classification.md) §1.18 and §3.2. Consequences that show up in this contract: **neither response echoes the token**, no error envelope repeats it, and only an 8-character prefix ever reaches a log line.

**No §5.2 mask entry**, for the same reason §7.19 and §7.20 have none: the resource is self-scoped — the JWT subject must BE the ride's rider, and the token is projected onto no wire field at all, since both responses return only booleans — so there is no role dimension to mask across (**Rule CG-DC-5** satisfied by this statement).

**Contract:** [`schemas/live-activity.schema.json`](schemas/live-activity.schema.json) (contracts **v0.28.0** — `LiveActivityContentState.progress` added additively by MYR-398 in v0.27.0 and `asOf` by the same issue's v3 wave here; every other field is byte-identical to **v0.24.0**) — `RegisterLiveActivityRequest`, `LiveActivityRegistrationResponse`, `EndLiveActivityResponse`, `LiveActivityContentState`, `LiveActivityEvent`, `LiveActivityRideStatus`.

**Three things the v3 card changed on this surface, all of them additive.** One new content-state field (`asOf`, §7.21.3); an **`aps.alert` dictionary on six pushes**, which is what makes the Dynamic Island expand (§7.21.4); and the Activity now starting at **REQUEST** rather than at accept, which widens what registers and what ticks but changes no shape (§7.21.5). The card's plate, colour and model are **not** on the wire and never will be — they are static for the life of a ride and belong to the Activity's own `ActivityAttributes`, by the same rule that kept the pickup label off it in r16.

#### 7.21.1 `POST /api/ride-requests/{id}/activity-token`

**Request.**

```
POST /api/ride-requests/rr_01HZX9K2M4/activity-token
Authorization: Bearer <app session JWT>        # MUST be the ride's rider
Content-Type: application/json

{ "activityToken": "8a1f4c2e9b7d0356…", "sandbox": false }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `activityToken` | `string` | **P1** | The token from `Activity.pushToken` on the device. **Required.** Trimmed; must be non-empty, at most **256 characters**, and **hexadecimal** (`[0-9a-fA-F]+`, either case). The two bounds are deliberately different bets. The LENGTH is a generous sanity check rather than a format assertion — the token is 64 characters today and rejecting a longer future one would silently break Live Activities for every rider on that iOS release. The CHARSET is not a guess: an APNs push token is the hex rendering of opaque binary and has been for the life of the API, both device tokens and ActivityKit tokens agree on it, and a token containing anything else could not address a device even if we stored it. It is also a P1 control — the token is interpolated into the APNs request path, and refusing a pathological value at the door means it never reaches the code where the "never logged in full" rule has to hold. |
| `sandbox` | `boolean` | P0 | `true` when the token was minted by a development or TestFlight build. **Optional, defaults to `false`.** Carried per-Activity rather than read from the device registry, because a rider who declined the notification permission still gets a Live Activity and may have **no `go_push_devices` row at all**. |

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`.
2. Resolve the ride and require the caller to be a **party**. A caller with no relation to the ride → `404 not_found`.
3. Require the caller to be the **rider**. A genuine party who is the OWNER → `403 permission_denied`.
4. Strict-decode the body — **unknown keys are a `400`**, matching §7.14 / §7.17 / §7.19 / §7.20.
5. Reject an empty, over-long or non-hexadecimal token → `400 invalid_request`, describing the RULE and **never echoing the value**.
6. Reject a ride already in a terminal state (`completed`, `declined`, `cancelled`) → `409 conflict`.
7. Reject a ride whose **reservation expired and which is still `accepted`** → `409 conflict` with `subCode: reservation_expired`. A ride that has since advanced to `arrived` or `enroute` is **not** refused. See "Two endings are final" below.
8. **Upsert** on `(ride_request_id, user_id)`, replacing `activity_push_token` and `sandbox`, stamping `updated_at`, and **clearing `ended_at`** — with steps 6 and 7 re-applied **as a predicate inside the write itself**, not merely re-checked.

**This is an UPSERT, and rotation is the reason — not deduplication.** ActivityKit **rotates the push token during the life of a single Activity**: the system hands the app a replacement and expects the server to start using it. So a rotation is an ordinary re-registration, and the conflict target is the `(ride, rider)` pair rather than the token — the §7.17 posture, where the token IS the identity, would accumulate a row per rotation and leave the sender guessing which one is live. A first registration and a rotation are indistinguishable on the wire and both answer `registered: true`.

**Re-registering after an end CLEARS the tombstone — on a ride that is still happening.** A client posting again is telling us it has a live Activity, which is the truth the server should adopt; the alternative is a registered Activity the ETA ticker skips forever because a stale `ended_at` says it is finished. That is the ordinary rotation path and it is unchanged.

**Two endings refuse re-registration — one of them finally, one of them only while it lasts.**

A **terminal status** is the obvious one, is visible in the ride, and is **final**. A **reservation expiry** is neither visible nor final (see the MYR-461 paragraph below), but it refuses for as long as it is all the ride is: the sweeper gives up on a late scheduled ride by recording `dispatch_status='failed'` with `dispatch_error='reservation_expired'` and **leaving the ride at `accepted`**, ending and tombstoning its Activities on the way past. It also latches `dispatched_at`, so a second expiry is impossible. Without this rule a single ActivityKit token rotation after the expiry would clear the tombstone, the ETA ticker would pick the row back up, and the rider would watch a lock screen count down to a pickup nobody was making — with nothing left in the system able to end it again.

That case gets a **`subCode`** precisely because the ride's own status does not explain it. A client seeing a bare `conflict` on an `accepted` ride cannot tell a lapsed reservation from a server bug, and would keep retrying; `reservation_expired` tells it to end the Activity and say why.

**Note what this does NOT refuse.** `dispatch_status='failed'` on its own means any nav push that did not land — the car was asleep, the proxy was down. That ride is still genuinely happening (the owner can drive it manually) and its Activity must keep working, rotations included. The refusal is scoped to the expiry outcome, never to dispatch failure in general and never to re-registration in general.

**Nor does it refuse a RESCUED reservation (MYR-461).** The expiry refusal holds only while the ride is still `accepted` — that is, while it is nothing but a reservation nobody came for. `reservation_expired` is a verdict on the **dispatch**, not on the ride, and this section says so twice already: the row stays `accepted` and its parties "may still cancel or **proceed manually**". When they do proceed manually the ride advances to `arrived` (the owner confirms the kerb) and then `enroute` (the rider starts), and at that point the expiry verdict has been **falsified by events** — the car is demonstrably there and a passenger is demonstrably aboard. Continuing to refuse registration then produced the defect this rule was scoped to fix: in external beta an expired reservation ran on for another **99 minutes** as a real, driven ride, and every registration attempt over that whole span answered `409`, so the rider's lock screen went dark at the expiry and could never come back. The ride's own status is the authority on whether a ride is over, and step 6 already enforces it; step 7 no longer second-guesses it.

**The client's part.** A `409 … reservation_expired` still means "end the Activity locally now", and that is unchanged. But because a rescued ride becomes registrable again, a client that ended its Activity at the expiry SHOULD start a fresh one when it next observes the ride at `arrived` or `enroute` — the dismissed Activity cannot be revived by any push, so only the app can bring the card back. A client that does not do this simply keeps the pre-MYR-461 behaviour; nothing breaks.

**The guard lives in the WRITE, not in a check before it.** Steps 6 and 7 are a read; the upsert is a write, and a `POST` arriving while the ride is transitioning passes both checks and then lands after the terminal tombstone. The `INSERT … SELECT` that performs the registration carries the same two predicates, so the refusal holds under the race rather than merely being unlikely. A registration that loses that race answers `409 conflict` with **no `subCode`** — by then the server no longer knows which ending won, and naming one would be a guess the client would act on.

**Idempotent (§4.5).** Re-sending an identical body is a no-op producing the same response, so a retry after a dropped response is safe.

**Response `200`** (`application/json`):

```json
{ "registered": true, "sandbox": false }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `registered` | `boolean` | P0 | Always `true` on a `200`. A failure is an error envelope, never `registered: false`. |
| `sandbox` | `boolean` | P0 | Echoes the declared APNs environment, so a client can confirm the server agrees about which gateway its token belongs to. **The response deliberately does NOT echo `activityToken`** — it is P1, the caller already knows the value it sent, and echoing it would put the token in every client log and proxy trace for no benefit. Same rule, same reason, as §7.17.1. |

**Errors:**

| HTTP | `error.code` | `subCode` | When |
|------|--------------|-----------|------|
| 400 | `invalid_request` | `null` | Malformed JSON; unknown key; missing/empty `activityToken`; token longer than 256 characters; token containing a non-hexadecimal character. |
| 401 | `auth_failed` | `null` | Missing/malformed/invalid bearer. |
| 403 | `permission_denied` | `null` | The caller is a party to this ride but is the **owner**, not the rider. Live Activities are rider-only in v1. |
| 404 | `not_found` | `null` | Unknown ride id, **or** a ride the caller is no party to — deliberately indistinguishable. |
| 409 | `conflict` | `null` | The ride is already `completed`, `declined` or `cancelled` — or the write's own guard refused a registration that raced the ride ending. |
| 409 | `conflict` | `reservation_expired` | The ride's reservation ran past its lateness ceiling; its Live Activity has already been ended for good. The ride's status is still `accepted`, which is exactly why the sub-code exists. |
| 500 | `internal_error` | `null` | Store-layer failure, or the registry not wired into this deployment. Nothing written; retryable. |

**The `404`-vs-`403` split is the house rule (§5.2), and it is load-bearing here.** A stranger gets `404`, never `403`, so this endpoint never confirms that a ride id exists to somebody with no relation to it — it is not an oracle for ride ids. Only a genuine party who happens to be the owner reaches the `403`, and telling THEM the ride exists reveals nothing they cannot already read from §7.8.

**The `409` is an instruction, not just a refusal.** A terminal ride will never be pushed to again — its final `event: "end"` has already fired and its rows are tombstoned — so accepting the registration would store a row nothing will ever update and only the 24-hour sweep would remove. The `409` tells the client to **end its Activity locally now**, which is exactly what its final-state fallback is for. The same instruction applies to the `reservation_expired` variant, with a reason the client can put on screen.

**What can end an Activity from the server side, exhaustively.** The lifecycle transitions carried on `ride.status.changed` (rider cancel, owner decline, completion), the reservation sweeper's expiry (§7.21.5), and owner teardown of the car (`DELETE /api/tesla/vehicles/{vehicleId}`, which end-pushes the affected rides **before** deleting them — the FK cascade removes the registration rows but cannot notify a phone). Beyond those, an Activity the server never manages to end is bounded by ActivityKit's own lifetime ceiling (~8 hours) and by the client's final-state fallback, not by anything this service does. That bound is why a missed end is a bad lock screen for a while rather than forever — but it is a ceiling, not a mechanism, and none of the paths above may rely on it.

#### 7.21.2 `DELETE /api/ride-requests/{id}/activity-token`

**Request.** No body.

```
DELETE /api/ride-requests/rr_01HZX9K2M4/activity-token
Authorization: Bearer <app session JWT>
```

Called when the Activity ends on the phone — the rider dismissed it, or the app ended it from its own final-state fallback.

**Response `200`** (`application/json`):

```json
{ "ended": true }
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `ended` | `boolean` | P0 | `true` when a live registration was actually closed by this call; `false` when there was nothing live to close — either it had already ended or it was never registered. **The two are deliberately indistinguishable**, matching the §7.17.2 unregister response. |

**Idempotent, and `false` is a `200`, not an error.** The client's end and the server's terminal-state push **race by design** and both are correct: the ride completing tombstones the row from the server side at the same moment the rider swipes the Activity away. A client told its completed work failed would retry forever. Unlike §7.17.2 there is no oracle concern to manage — the caller has already been proven to be this ride's rider, so there is nothing here they could probe.

**Errors:** identical to §7.21.1 except that there is no body to reject (**no `400`**) and **no `409`** — ending an Activity on a terminal ride is the ordinary case, not a conflict.

| HTTP | `error.code` | When |
|------|--------------|------|
| 401 | `auth_failed` | Missing/malformed/invalid bearer. |
| 403 | `permission_denied` | Caller is the ride's owner rather than its rider. |
| 404 | `not_found` | Unknown ride id, or caller is no party to it. |
| 500 | `internal_error` | Store-layer failure, or the registry not wired. Retryable. |

#### 7.21.3 The content-state (informative — no REST surface)

What the Activity DISPLAYS is delivered over APNs, addressed by the registered token. It never appears on any endpoint, and it reaches the device only over a token that identifies exactly one Activity, on one ride, for one rider. It is documented here because the Swift `ContentState` must decode it exactly.

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `v` | `integer` | P0 | Content-state schema version, currently **`1`**. Carried explicitly because the Swift `ContentState` is compiled into an app a rider may not update for months, so the wire shape freezes the moment a build ships. When a field's MEANING changes the server keeps sending `v1` to installed clients while `v2` goes to new ones — the alternative is a lock screen full of wrong numbers on every phone that has not updated. |
| `status` | `enum` | P0 | `requested` \| `accepted` \| `declined` \| `enroute` \| `arrived` \| `completed` \| `cancelled` \| `reservation_expired`. The first seven mirror `RideRequestStatus` exactly. **`reservation_expired` is the one member that is NOT a ride status**: the reservation sweeper gives up on a late scheduled ride by recording a dispatch failure and leaving the row at `accepted`, so without a distinct value the lock screen would sit on "your car is on its way" forever. Append-only; a client MUST tolerate an unrecognised member rather than fail to decode. The server sends the enum and **never prose** — all copy is the client's, so wording changes in an app update, not a deploy. |
| `eta` | `integer` (unix seconds) | P0 | The car's arrival time as an **ABSOLUTE instant**. **OMITTED ENTIRELY when unknown** — never `null`, never `0`, never a guess. Also omitted on `requested`, where no car has been assigned yet; see "The Dispatch phase" in §7.21.5. |
| `vehicleName` | `string` | P0 | The owner-chosen nickname, e.g. `"Blue Whale"`. `""` when the car has no name, in which case the client renders its own generic fallback rather than a blank. **At most 128 characters**, truncated as `destination` is. |
| `destination` | `string` | **P1** | The dropoff's short label — the name the RIDER chose when booking, e.g. `"Home"`. **At most 128 characters**, truncated server-side with an ellipsis (`…`) when longer. See the notes below. |
| `progress` | `number` (0..1) | P0 | How far along the **current leg** the car is — what the r16 progress track draws ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398)). **OMITTED ENTIRELY when the server cannot say** — never `null`, never `0`. Monotone per leg by construction. See "The progress track" below. |
| `asOf` | `integer` (unix seconds) | P0 | When this content-state's **data** was true. The v3 card renders it as the stale presentation's subline, `Last updated {h:mm A}`. **NOT `aps.timestamp`** — that one is when the push was SENT and must always be `now`; this one is when the server last LEARNED something, and it is meant to lag. Always sent. See "`asOf` and the freshness it finally admits to" below. |

**`eta` is absolute, and that is the central decision ([MYR-194](https://linear.app/myrobotaxi/issue/MYR-194)).** A duration decays silently on a screen the server cannot repaint — "4 min" stays "4 min" for an hour — whereas an instant stays true however late it is read, and the phone counts down on its own between pushes. It is what lets a ~60–90s cadence look continuous.

**The ETA is the CAR'S OWN carried navigation ETA.** Tesla's `minutesToArrival`, whole minutes, persisted verbatim and converted to an instant at send time. **There is no server-side route solver in this service**, so a car with no active nav route yields **no `eta` key at all** rather than an invented number.

**The 128-character caps are ENFORCED, not merely declared.** Both labels reach this payload from a request body — `destination` is the rider's own label for a saved place, `vehicleName` the owner's nickname for the car — and neither is bounded on the way in. Apple caps the whole Activity payload at **4KB** and answers an over-cap push with a flat `400` that takes out the ENTIRE Activity for that ride, every subsequent ETA tick included, not merely the long field. The cut is applied at content-state build time (so it also covers labels already stored from before the bound existed), counted in **runes rather than bytes** so it cannot split a multi-byte character into a `U+FFFD`, and marked with an ellipsis that is itself inside the budget — a truncated address that does not say it was truncated is one the rider silently misreads.

**`destination` is P1 and is the one deliberate exception to the alert-copy policy.** `internal/push/copy.go` refuses to put pickup/dropoff labels on a lock screen for §7.17 alerts, and that rule stands. It is carried here narrowly: a Live Activity is the rider's OWN ride on the rider's OWN device, addressed by a token scoped to that one Activity, and a ride card that cannot say where the car is taking you is not the feature. It is **never sent to an owner's Activity and never appears in an alert body**. See [`data-classification.md`](data-classification.md) §1.18.

##### The progress track ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398))

**`progress` is a fraction of the CURRENT leg, and the leg is not on the wire.** `accepted` is leg one — the car driving to the rider — and `enroute` is leg two, the car driving them onward; the client reads the leg off `status` and renders "Pick up in N min" or "Arriving in N min" accordingly. Sending the leg as well would put one fact on the wire twice, and two copies of a fact are two things that can disagree.

**The governing rule, from which everything below follows: an absent `progress` renders a TRACKLESS card, a wrong one renders a LIE.** A rider can read the headline and the meet-at line without a track. They cannot tell that an arrow four fifths of the way along is describing a car that has barely left. `0` is not the humble answer — it is the claim "the car has covered none of the distance" — so the honest degraded value is **no key at all**.

**It is the car's own navigation, twice removed from a guess.** The preferred input is Tesla's `tripDistanceRemaining` (road miles left on the car's active route) measured against the value captured at the start of the leg; when the car reports no distance the server falls back to `minutesToArrival` measured the same way. There is still **no server-side route solver in this service** — the same rule that governs `eta` — so a car with no active route produces no NEW fraction, not an invented one; a fraction already delivered is **held** rather than withdrawn, which is the distinction the degradation table's first two rows draw. A distance is preferred over minutes for a reason worth stating: a track depicts DISTANCE, and distance to a fixed point falls as the car drives, whereas minutes are a traffic re-estimate that moves in both directions for reasons the car did not move.

**A reading older than three minutes does not move the arrow.** The freshness horizon is deliberately the same as the `stale-date`: past it the reading is older than the window the card itself claims to be good for, and a server that kept advancing the track on data it has already declared past its own trust horizon would be contradicting itself.

**The horizon is measured on the READING, not on the car's row.** The only timestamp the car carries is `Vehicle."lastUpdated"`, which is stamped by every telemetry write of any column — so a writer that refreshes position without touching navigation (the active-ride REST polling in [MYR-394](https://linear.app/myrobotaxi/issue/MYR-394) is the concrete case) would hold a row-stamp gate open indefinitely over a `tripDistanceRemaining` of arbitrary age. The server therefore records the raw observation it acted on beside the anchor (`progress_reading`, `progress_reading_at`, migration 0027) and treats a reading that is **unchanged and older than the horizon** as no reading at all. The pre-filter is no longer a row stamp at all. [MYR-409](https://linear.app/myrobotaxi/issue/MYR-409) gave the car its own nav-reading timestamp (`go_vehicle_control_state.nav_reading_at`, migration 0034), written only by a frame that actually carried a navigation-group member and read by both content-state projections in place of `Vehicle."lastUpdated"`. This paragraph previously ended by noting that `Vehicle` is Prisma-owned and read-only to this service, so a per-field nav timestamp on the car was not available to it; that constraint is real and unchanged, and the column simply lives on the Go-owned side table instead, which is the same MYR-253 hydration pattern the rest of that table follows. The per-Activity anchor is kept and still does the other half: this stamp dates the last frame that MENTIONED navigation, while `progress_reading_at` catches a car dutifully re-sending the SAME value — a car stopped in traffic keeps the stamp current on its 30s nav resend, and only the reading's own staleness distinguishes that from movement. **NULL means no navigation frame has ever been seen, and every gate reads that as not fresh**; there is deliberately no fallback to the row stamp, which would reinstate the lie for exactly the population the column describes.

**`eta` is NOT gated, and this is a scope decision rather than a claim that a stale `eta` is harmless.** Be plain about what it leaves: `eta` is rebuilt from the push's own `now` on every tick, so a car whose telemetry froze at "7 minutes" keeps producing a fresh arrival instant seven minutes out, push after push — the instant does not slide into the past on its own. The `stale-date` is likewise re-armed on every push, because the ticker pushes unconditionally and the date is `timestamp + 3 min`; a data-stale update therefore arrives looking fresh. So a car that has gone quiet mid-leg renders a held track beside a perpetually renewed ETA, with no staleness signal on the card. Gating `eta` on the same horizon would omit a key that every installed build renders today, which is not an additive change and not this issue's — **it needs its own issue**, and the honest implementation is to omit `eta` when the nav reading is stale, NOT to back-date `timestamp` (ActivityKit uses that for out-of-order discard, so the card would need a separate explicit stale-at instead).

**MONOTONE PER LEG, ENFORCED SERVER-SIDE.** The value is clamped to the highest fraction already **delivered** to that Activity, so the track does not run backwards within a leg. Enforced rather than guaranteed by construction, and the bound is worth stating: the floor lives in a row that is written AFTER Apple accepts the push, so a push that is accepted by a process that then dies before the write can be followed by one lower value before the floor catches up. The write itself refuses to lower the floor (`progress_value <= $new` in its own `WHERE`), which closes the two-replica race; the process-death window is one step wide, self-healing, and not closable without writing before the send — which would advance the floor past pushes that never arrived and is strictly worse. The justification is that a regression is almost never the car: traffic re-estimates an ETA, a reroute lengthens a route, and neither means the car un-drove the road behind it — while an arrow sliding back down the track reads as a broken widget rather than as weather. **`eta` is deliberately NOT clamped and must not be**: a later arrival time is news the rider needs. **The client MUST render both as given.** "Most of the way there, and arriving later than it said a minute ago" is a coherent pair, not a contradiction, and a client that tries to reconcile them will end up hiding the true one.

**A route that grows RE-ANCHORS rather than restarting — on the DISTANCE source only.** When the remaining distance exceeds the leg's baseline — a reroute, or the car's nav destination changing under us — the baseline is re-scaled so the current reading means exactly the fraction already delivered. The ground moves under the arrow; the arrow does not move. The alternative (restart at zero) is the most alarming thing this card could do to somebody standing on a sidewalk. **Minutes are never re-anchored**, because on the ETA source a reading above the baseline is TRAFFIC, and the row below says traffic must be held: re-scaling it would silently redefine a 12-minute leg as a 40-minute one and render 0.85 for a car that is half way, with the monotone clamp waving the inflation through. The trade-off that buys: an ETA-source DESTINATION CHANGE freezes the track until the ETA falls back below the original baseline, since minutes cannot distinguish a longer route from slower traffic. That is the choice this contract already made.

**A full track means the leg is OVER, and only the ride record may say that.** An in-flight leg is capped just below 1 (`0.99`). Exactly `1` is sent on two statuses and both are assertions rather than estimates: **`arrived`** (the owner confirmed the car is at the kerb) and **`completed`** (the owner confirmed the dropoff). A car reporting zero miles remaining while the ride record still says `accepted` is all-but-arrived, not arrived, and reports the cap.

**The anchor is per ACTIVITY and it is in the database** (`go_live_activities`, migration 0027 — `progress_leg`, `progress_source`, `progress_baseline`, `progress_value`, `progress_reading`, `progress_reading_at`; [`data-classification.md`](data-classification.md) §1.18). Two reasons, either sufficient. The ETA ticker is **not sharded across replicas** — every process lists every live Activity and pushes to all of them — so two in-memory baselines for one ride would make the arrow oscillate between two answers every 75 seconds. And a deploy mid-ride would forget the baseline entirely. It is written **after** a delivery Apple accepted, never before, because the clamp's floor must record what the phone HAS SEEN; advancing it on a push that never arrived is exactly how the delivered sequence would come to contain a decrease. A **token rotation preserves it** (§7.21.1's upsert names its SET list), because a rotation is the same Activity on the same leg.

**A LEG THAT HAS NOT STARTED HAS NO TRACK, and `accepted` alone does not prove one has.** `accepted` covers two different situations: a car dispatched and driving to the rider, and a reservation accepted for tomorrow that is **dormant** until the earlier of its dispatch and its due instant ([MYR-376](https://linear.app/myrobotaxi/issue/MYR-376)). The ride's status is the same in both, the Activity ticks in both, and the car the server reads navigation from is the OWNER'S car — which between accepting a reservation and serving it is being driven on the owner's own errands. Without a gate the rider's card would anchor a pickup track on one of those errands and advance it to `0.99` when the owner parks; because the leg never flips, that fraction would then be the floor for the entire real pickup. So the pickup leg's track is gated on the SAME dormancy predicate the pickup transition uses — no key and **no anchor stored** while a reservation sleeps. The dropoff leg needs no equivalent: `enroute` is the rider having started leg two from inside the car. Note `eta` is not gated this way either and misreports on the same path; unlike the fraction it is self-correcting on the next reading, and it belongs with the `eta` issue above.

**Honest degradation, exhaustively.**

| Situation | Server sends | Client renders |
|---|---|---|
| Car streaming a nav route | the fraction | a track that advances |
| No active nav route, nothing delivered yet (nav cleared before the first reading) | **no key** | headline + meet-at, no track |
| Nav route cleared MID-leg (car still streaming, a fraction already delivered) | the **last delivered** fraction, held | a frozen track — and note the card is NOT stale, so nothing on it explains the freeze |
| Reservation accepted but not yet dispatched (dormant, whatever the car is doing) | **no key**, and no anchor stored | headline + meet-at, no track |
| Car reports minutes but no distance | the fraction, from the ETA ratio | a track that advances |
| Telemetry gone quiet mid-leg (> 3 min), or the nav reading frozen behind a fresh row stamp | the **last delivered** fraction, held | a frozen track — and note the stale-date is re-armed by the push itself, so it does NOT say why |
| Telemetry never seen this leg | **no key** | no track |
| Traffic pushes the ETA out | the fraction **held**, `eta` moved later | track still, arrival time later |
| Reroute / destination changed | the fraction held, then advancing again | continuous |
| Leg flip (`arrived` → `enroute`) | `1`, then a fresh leg from ~`0` | full, then a new track |
| `declined` / `cancelled` / `reservation_expired` | **no key** | no track on the ending card |
| Rider reinstalled mid-leg (new registration) | a fraction anchored at the FIRST reading we saw | a track that opens at 0 mid-journey |

That last row is the one honesty bound worth naming out loud: `progress` is the fraction of the leg completed **since the server first had a trustworthy reading of it**, which is normally the leg's start and occasionally is not. A non-streaming car whose position only arrives via [MYR-394](https://linear.app/myrobotaxi/issue/MYR-394)'s active-ride REST polling is the ordinary case that improves this input, and nothing here invents a position in its absence. **That polling does interact with the freshness horizon, and the interaction is handled here rather than there:** it refreshes `Vehicle."lastUpdated"` every ~25s while writing only position, speed and heading, which would make a row-stamp gate meaningless for exactly the population it serves. The horizon is keyed on the reading instead (see above), so the two are independent whichever lands first.

**`progress` is P0, and it is the one field on this surface that could ride an owner-side Activity unchanged.** Its neighbour `destination` is P1 because it is a PLACE. This is a unitless fraction of an unnamed journey: no coordinate, no place name, no distance, and no duration. "62% of the way" locates the car only for somebody who already knows both ends of the trip — which on this surface is the rider whose trip it is.

**`v` stays `1`.** A new OPTIONAL field is not a meaning change: Swift's `Codable` ignores unknown keys, so an installed pre-r16 build decodes a payload carrying `progress` exactly as it always did, and a post-r16 build reading a payload without one gets `nil` and draws no track. Both directions are safe, which is the whole test for whether the version discriminator has to move. The v3 wave's `asOf` passes the same test and `v` stays `1` again — and note that neither the island alert nor the Dispatch phase touches this question at all: the alert is an envelope key the `ContentState` never sees, and `requested` has been a member of the status enum since v0.24.0.

##### `asOf` and the freshness this surface finally admits to ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398))

**`asOf` is when the DATA was true. `aps.timestamp` is when the PUSH was sent. They are different instants and a client must not conflate them.** ActivityKit uses `aps.timestamp` to discard an update older than the one it is already showing, so that field must always be `now` — back-dating it to signal staleness would make the card's own ordering defence reject fresher updates, which is why the progress-track note above already ruled that approach out in as many words. `asOf` is the separate explicit stale-at that note said the honest implementation would need.

**It exists because of a blind spot this document already confessed to.** The ETA ticker pushes unconditionally; `stale-date` is `timestamp + 3 min` and is therefore *re-armed by the push itself*; and `eta` is rebuilt from each push's own `now`, so a car frozen at "7 minutes" keeps producing a fresh arrival instant seven minutes out, tick after tick. The result, stated plainly in the progress-track note above: **a car that has gone quiet mid-leg renders a held track beside a perpetually renewed ETA, with no staleness signal on the card.** ActivityKit will never mark that card stale, because from ActivityKit's point of view it is not. `asOf` is the one value on the payload that stops moving when the data does, and the client's `Last updated {h:mm A}` is the sentence that follows from it.

**It therefore MUST NOT re-stamp to `now` on a pass that computed nothing new.** This is the whole contract of the field, and it is the one way to implement it wrongly while passing every obvious test: a server that stamped `asOf` from the send clock would be correct on every healthy push and useless on exactly the pushes the rider needs it for.

**Derivation, exhaustively.** One rule: if the push rests on a **nav reading**, `asOf` is when that reading was first seen to hold; otherwise the push rests on the **ride record**, which is asserting something as of now.

| Situation | `asOf` |
|---|---|
| Fresh nav reading, newly changed | now (the reading was first seen this pass) |
| Nav reading unchanged since an earlier pass (car at a red light) | the **earlier** instant it was first seen |
| Progress HELD — car quiet, route cleared mid-leg, or reading stalled | the **old** instant, unmoved, while `timestamp` and `stale-date` move on |
| Lifecycle transition (`accepted`, `arrived`, `enroute`, `completed`, an ending) | now — the ride record just changed |
| `requested`, a dormant reservation, a leg with no telemetry at all | now — there is no reading, and the status is true as of now |

Note the second row is not a defect: an unchanged reading genuinely is older than this pass, the card is not stale so nothing renders `asOf`, and the instant it *would* render is the honest "when the car last told us something new". It also follows that `asOf ≤ aps.timestamp` always, and that the two are equal on most pushes.

**Always present, and P0.** Unlike `eta` and `progress` there is no case in which the server cannot say when it computed a state, so there is no honest-omission branch and no reason for one. It is nonetheless **optional in the schema**, so a payload minted by a pre-v0.28.0 server still validates and a build compiled before the field decodes unchanged. On the wire it is a **pointer in the sender**, deliberately: the zero value of a unix timestamp is not a neutral placeholder but the claim "this was true in January 1970", and a card reading `Last updated 12:00 AM` is worse than one reading nothing. P0 on the same test `progress` passes — a clock reading about the rider's own ride names no place and locates no car.

**What it does NOT do.** It does not gate `eta`. A client can now *detect* the stale case (`now − asOf` past the three-minute horizon) and render accordingly, which is a strictly better position than before, but the server still sends a renewed arrival instant on a frozen reading. Withholding `eta` on a stale reading remains its own issue, for the reason given above: it would omit a key every installed build renders today.

##### Why the "Meet at {pickup}" line is NOT on the wire

The r16 design adds a pickup line to the card, and the deliberate decision is that **the server does not push the pickup label**. It belongs in the Activity's **static `ActivityAttributes`**, set by the app when it starts the Activity locally.

**It cannot change.** A ride's `pickup` is fixed at creation — §7.8 has no endpoint that edits it, and rescheduling (MYR-192) negotiates the TIME, never the place. The content-state is for what CHANGES between pushes; a constant that the app already holds does not belong in a payload the server repeats every 60–90 seconds.

**The app already has it.** The Activity is started locally when the ride is accepted, from the `RideRequest` record the app is holding at that moment — `pickup.label` is a required member of `RidePlace` in [`schemas/ride-request.schema.json`](schemas/ride-request.schema.json), so there is no state in which a client can start an Activity and not know where the pickup is.

**And it is P1.** `destination` is on this surface as a **narrow, deliberate exception** to the alert-copy policy, and the argument for it was necessity: a ride card that cannot say where the car is taking you is not the feature, and the server is the only party that knows the label at push time. Necessity is precisely what fails for the pickup. Adding it would put a second P1 place label into ~40 pushes per ride, through APNs, into every proxy trace and every debug log that ever captures a payload — to deliver a string the receiving device typed in itself. **A field that can be answered on-device is not a field this payload should carry**, and this is the rule to apply to the next one as well.

**The v3 card is the next one, and the rule holds.** Its pickup-leg subline reads `7SRJ294 · Silver Model Y`, so the obvious reading is that the wire owes it a plate, a colour and a model. It does not. All three are **static for the life of a ride** — the ride names one vehicle at creation and no endpoint reallocates it — and all three are already in the `RideRequest` the app was holding when it started the Activity, exactly as `pickup.label` is. They belong in `ActivityAttributes` beside it. Repeating three constants in every one of ~40 pushes to save the client a field it already has would be the same mistake in three new places.

**The one thing the v3 card genuinely needed from the server was `asOf`**, and it passes the test the plate fails: it is not constant, it changes on every push, and it is a fact only the server holds.

#### 7.21.4 The APNs envelope (informative)

```
POST /3/device/{activityPushToken}
apns-topic:       app.myrobotaxi.ios.push-type.liveactivity
apns-push-type:   liveactivity
apns-priority:    10          # 5 for an ETA tick — but always 10 when aps.alert is present
apns-expiration:  1785535140  # == aps.stale-date on an ordinary `update`; now + 24h on an `end` or an ALERTING update
```

A routine ETA update. Note `asOf` sitting **thirty seconds behind** `timestamp`: the car's last new reading arrived half a minute before this push, which is the ordinary healthy shape rather than a problem.

```json
{
  "aps": {
    "timestamp": 1785534960,
    "event": "update",
    "content-state": {
      "v": 1,
      "status": "enroute",
      "eta": 1785535200,
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 0.62,
      "asOf": 1785534930
    },
    "stale-date": 1785535140
  }
}
```

A car with no active navigation route — the same ride, honestly degraded. Both `eta` and `progress` are **absent keys**, not zeros, and the card renders a headline with no time and no track rather than a car that has not moved. `asOf` is `now`, because nothing here rests on a nav reading:

```json
{
  "aps": {
    "timestamp": 1785534960,
    "event": "update",
    "content-state": {
      "v": 1,
      "status": "accepted",
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "asOf": 1785534960
    },
    "stale-date": 1785535140
  }
}
```

**A car that has gone quiet mid-leg — the case `asOf` exists for.** The ticker pushed anyway, so `timestamp` and `stale-date` are fresh and ActivityKit will not mark this card stale; `eta` was rebuilt from this push's own `now` and looks current; `progress` is the last delivered fraction, held. **Only `asOf` says what happened** — it is more than seven minutes behind the push, and the client renders `Last updated 3:31 PM` in the subline instead of the destination:

```json
{
  "aps": {
    "timestamp": 1785535380,
    "event": "update",
    "content-state": {
      "v": 1,
      "status": "enroute",
      "eta": 1785535620,
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 0.62,
      "asOf": 1785534930
    },
    "stale-date": 1785535560
  }
}
```

**An alerting update — the car reaching the pickup.** Identical in every other respect to the ones above; the `alert` dictionary is the only difference, and it is what makes iOS expand the Dynamic Island for about three seconds. Sent at priority 10 even though the Arriving variant of it originates on the ticker's conserving path:

```json
{
  "aps": {
    "timestamp": 1785535080,
    "event": "update",
    "content-state": {
      "v": 1,
      "status": "arrived",
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 1,
      "asOf": 1785535080
    },
    "stale-date": 1785535260,
    "alert": {
      "title": "Your ride",
      "body": "Your ride is here"
    }
  }
}
```

**A completed ride is TWO pushes, FIVE MINUTES APART** ([MYR-418](https://linear.app/myrobotaxi/issue/MYR-418) for the split, [MYR-421](https://linear.app/myrobotaxi/issue/MYR-421) for the gap). First an alerting `update` carrying the terminal content-state — note the omitted `eta` (the car has arrived, so there is no route), the `progress` of exactly `1` (asserted by the ride record, not estimated), and the alert that carries the sixth and last island expansion:

```json
{
  "aps": {
    "timestamp": 1785535380,
    "event": "update",
    "content-state": {
      "v": 1,
      "status": "completed",
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 1,
      "asOf": 1785535380
    },
    "stale-date": 1785535560,
    "alert": {
      "title": "Your ride",
      "body": "You've arrived"
    }
  }
}
```

Then the `end` **five minutes later** — sent by the ETA ticker's held-end pass, not by the transition — carrying the same final content-state, the `dismissal-date`, and **no alert**. Every instant on it is computed from its OWN send time, not from the completion five minutes earlier:

```json
{
  "aps": {
    "timestamp": 1785535680,
    "event": "end",
    "content-state": {
      "v": 1,
      "status": "completed",
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 1,
      "asOf": 1785535680
    },
    "stale-date": 1785535860,
    "dismissal-date": 1785535980
  }
}
```

An unhappy ending is a **single** push — outside the design's six phases there is no expansion to schedule — and it still carries its own terminal content-state, because a card that keeps saying "on its way" after a cancellation is the whole reason this surface pushes on every transition. `progress` is an absent key: a journey that did not happen has no fraction of itself completed.

```json
{
  "aps": {
    "timestamp": 1785535380,
    "event": "end",
    "content-state": {
      "v": 1,
      "status": "cancelled",
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "asOf": 1785535380
    },
    "stale-date": 1785535560,
    "dismissal-date": 1785535410
  }
}
```

##### `aps.alert` — the island auto-expand ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398))

**An update carrying an `alert` dictionary makes iOS expand the Dynamic Island for ~3 seconds and then collapse it itself.** That expansion is the mechanism the v3 design asks for and the dictionary is the only way to ask: the compact island is a figure or a glyph, and a phase changing underneath a figure that happens to look the same is a state change nobody sees.

**Six pushes carry one, and nothing else does — and all six are `update` events.** `requested` (Dispatch), `accepted` (Enroute), **Arriving**, `arrived`, `enroute` (On trip), `completed`. The design forbids the rest in as many words — not on ETA ticks, not on the stale flip — because an island that opens every 75 seconds for the length of a ride is a widget the rider turns off. The unhappy endings (`declined`, `cancelled`, `reservation_expired`) are deliberately outside the six: their content-state still lands, but an island opening to deliver bad news the card is already showing is not a kindness.

**Arriving is the one that is not a ride status,** and it is the whole reason this needs state. It is the **pickup leg's ETA at or under two minutes**, which the ticker re-evaluates every pass — so once it is true it stays true for every remaining tick of the leg. It fires **exactly once per Activity**, enforced by a persisted high-water mark rather than by hoping.

**Arriving is gated on the reading's AGE as well as its value, and unlike its neighbours it can be withheld.** The ETA behind it is the car's last streamed `minutesToArrival`, a column that keeps its value after the route that produced it ends — so a car that has just finished an errand reads `0` or `1` until it says otherwise. The dormancy gate does not help on an **instant** ride (`scheduled_for IS NULL` makes it unconditionally true), and the accept push fans out *before* the dispatched route has streamed back. Ungated, accepting an instant ride would expand the rider's island with "your ride is almost here" for a car twenty minutes away **and persist the mark at 3** — spending rungs 2 and 3 together, so the rider would never be told a car was on its way and never get the real Arriving. A wrong `eta` self-corrects on the next reading; a **burnt rung is durable for the life of the ride**, which is why this rung and not `eta` carries the gate. So Arriving requires the car's **navigation reading** to be inside the **3-minute** freshness horizon (the same horizon the progress track uses), and the accept push therefore resolves to **Enroute**, with Arriving computed on a later pass from a post-dispatch reading.

**[MYR-409](https://linear.app/myrobotaxi/issue/MYR-409) closed the arm this paragraph used to name as residual, and it took TWO gates rather than one, which is the part worth keeping.** The residual was that the horizon ran against a *row* stamp — `Vehicle."lastUpdated"`, moved by every telemetry write of any column — so a car awake and streaming motion was always inside it. The obvious fix, dating the reading itself, is now in place (`go_vehicle_control_state.nav_reading_at`, migration 0034) and it closes the **frozen-value** arm: a `minutesToArrival` left at `1` by a route that ended without a cancel can no longer ride a row stamp kept current by motion. **It does not close the arm the issue was named for.** A car navigating its owner's errand at the instant an instant ride is accepted is streaming perfectly fresh navigation — one second old, describing a route to the shops — so a freshness gate passes it, and if the owner happens to be two minutes out the rung burns anyway. **Recency and relevance are different questions, and only one of them is a timestamp on the car.** The second gate answers the other: the reading must not predate `go_ride_requests.dispatched_at`, the instant the rider's pickup was pushed to the car as a destination. A reading taken before the car was told where the rider is cannot be describing the rider, whatever its age. **A nil dispatch instant is not relevant either** — a dispatch that failed or was skipped means the car was never sent to this rider at all, and the dormancy gate covers only the sleeping-reservation case.

**The remaining bound is small and stated.** Dispatch is recorded when the destination is *pushed* to the car, not when the car acts on it, so a nav frame emitted in the seconds between those two events postdates the dispatch while still describing the old route. Closing that needs the car's active destination matched against the ride's pickup coordinates, and `pickup_lat_enc` / `pickup_lng_enc` are app-encrypted — the same reason the progress track's leg gate settles for dormancy. The window is now a few seconds wide rather than unbounded, and it errs safely at the moment that matters: on the accept push, dispatch is approximately *now*, so no reading yet postdates it and the rung is left unspent for the real Arriving.

**The mechanism: a monotone ladder plus a persisted mark.** The six phases are numbered `1..6` in the order a ride passes through them (`go_live_activities.alerted_phase`, migration 0029; `0` means none yet). A push may attach an alert only when its phase is **strictly above** the mark, and the mark is raised only after Apple accepts the push. Three properties follow, each of which had to be bought:

- **Once per phase, per Activity.** The `>` is the guarantee.
- **A duplicated or out-of-order `ride.status.changed` alerts nothing.** Replaying `accepted` after `arrived` compares 2 > 4 and stops. An equality test against "the last phase sent" would happily re-alert.
- **Two replicas cannot both alert *twice*, and the bound is stated rather than rounded up.** The ticker is not sharded, so every process lists every live Activity, and the mark is read from a snapshot taken at the top of a pass but raised only **after** Apple accepts the push. That window is real: two replicas holding the same pre-write snapshot will both attach the same alert, and one replica's snapshot ages across a pass of up to 200 sends, so it can deliver a *lower* rung's banner seconds after another process delivered a higher one ("almost here" arriving just after "your ride is here"). What the persisted mark guarantees is that this is **bounded and self-healing**: the guarded `UPDATE` refuses to lower the mark, exactly as the progress floor's does, so the cost is at most **one duplicate expansion per replica per phase** and the next pass — reading the raised row rather than the stale snapshot — alerts nothing. The alternative is a claim-then-send latch (`UPDATE … WHERE alerted_phase < $n RETURNING`, the shape `ClaimReservationDispatch` already uses), and it is deliberately **not** taken here: it burns a phase on every push APNs never accepts, and a process that dies mid-send makes that unrecoverable. Between "the island opened once more than it had to" and "a state change the rider never saw", this surface chooses the first.

**A fresh registration is SEEDED at the ride's current phase, not at zero.** The app starts the Activity locally and draws the current state itself, so the island has just been in front of the rider — expanding it to announce the phase they are already looking at would be an unearned interruption at the very start of every ride. Migration 0029 backfills existing rows the same way, so the deploy that ships this does not open every live island at once. Seeding is done from the **status alone** and therefore cannot express Arriving, which is the right way round: a rider who registers while the car is already ninety seconds out has genuinely not seen that expansion and gets it on the next tick. A **token rotation preserves the mark**, like the progress anchor, because it is the same Activity.

**Priority 10 whenever an alert is attached, overriding the conserving flag.** Priority 5 exists so a periodic ETA refresh does not compete with "your car is here" for Apple's per-Activity budget — and a push whose entire purpose is to open the island for three seconds is not a refresh that can afford to be coalesced. The combination is not hypothetical: Arriving is a ticker-computed threshold, so the one alert in the design that does not ride a lifecycle transition originates on exactly the path that sets priority 5.

**No `sound`.** The design asks for an expansion, not an interruption. Six beeps per ride from a surface whose whole premise is that it replaces eleven notifications would undo the feature; an alert dictionary with no sound expands the island silently.

**Title and body are FIXED CONSTANTS, and this is the one place this surface sends prose.** §7.21.3's rule is that the server sends the enum and never prose, so all copy can change in an app update rather than a deploy. APNs offers no way to honour that here: an alert dictionary is a title and a body. The localised form (`title-loc-key` / `loc-key`) would keep the strings in the app, and it was rejected because a key the installed build's string table lacks renders as **the raw key** on the lock screen — and a server that ships ahead of the app is this project's normal state. So the copy is deliberately bland and deliberately **not** the card's copy, which stays the client's:

| Phase | Trigger | `title` | `body` |
|---|---|---|---|
| 1 · Dispatch | `requested` | `Your ride` | `Finding your ride` |
| 2 · Enroute | `accepted` | `Your ride` | `Your ride is on the way` |
| 3 · Arriving | pickup ETA ≤ 2 min | `Your ride` | `Your ride is almost here` |
| 4 · Arrived | `arrived` | `Your ride` | `Your ride is here` |
| 5 · On trip | `enroute` | `Your ride` | `You're on your way` |
| 6 · Completed | `completed` (on the alerting `update` that precedes the `end` — see below) | `Your ride` | `You've arrived` |

**The P1 copy policy applies to these in full, and that is a stricter rule than the content-state's.** An alert dictionary renders as an ordinary banner on a device with no Dynamic Island and on a paired watch — a locked screen, to whoever is holding it — so it is governed by `internal/push/copy.go`'s payload policy rather than by the narrow exception that lets `destination` onto the content-state. **No place, no address, no vehicle nickname, no name.** Note also "almost here" rather than "arriving": *Arriving* is an engineering phase name, and the v3 copy rules retire the word from rider-facing text because it was the word the field report was about.

##### The sixth expansion rides an `update`, not the `end` ([MYR-418](https://linear.app/myrobotaxi/issue/MYR-418))

**No `end` push ever carries an `aps.alert`.** The rule is absolute and is enforced at the renderer, not merely at the callers: an alert set on an `end` is dropped before the bytes are built.

**What it cost to learn.** Completed is both the sixth rung of the ladder *and* the transition that ends the Activity, so the expansion was originally asked for by hanging the alert on the `end` push itself. On a real ride that shape produced: a registration that worked, alerts sent and **accepted by Apple** through phase 5, the `end` delivered with the correct final content-state and the correct alert dictionary — and **no expansion, no check glyph, nothing** at the moment the ride completed. Apple's ActivityKit push documentation introduces the alert dictionary under `start` and `update`; of an `end` it says only to "include the final content state", and prints no `end` example carrying one. An alert there is undocumented, not rejected, and not honoured. **This surface has no failure signal** — no `400`, no reason string, no metric — so a dictionary that does nothing is indistinguishable, from the server and from the logs, from one that worked.

**So the completion is a PAIR.** A terminal status that sits on the ladder — `completed` alone — sends an alerting `update` carrying the terminal content-state, then the `end` carrying the same content-state, its `dismissal-date`, and no alert. The update is an ordinary alerting update in every respect: priority 10, the 24-hour expiration floor, and the high-water mark raised on delivery.

**The mark finally reaches 6, and that matters beyond tidiness.** The old `end`-borne alert could never raise it: the guarded `UPDATE` is scoped to `ended_at IS NULL` and the tombstone was already racing it, so a completed ride's row tombstoned at **5** no matter what happened — which is exactly the ambiguous evidence the defect was found from. Now the mark is written by the pair's first push, while the row is still live, and a completed ride whose row still reads 5 is a real signal that the expansion did not land.

**The `end` always takes the later stamp.** `aps.timestamp` is rendered in whole seconds, so a pair built from one clock read would carry the identical integer; ActivityKit's documented rule is that it discards a push *older* than the one it is showing, and what it does with an equal stamp is written down nowhere. The push that must never be discarded is the `end` — the rows are tombstoned as it goes out, nothing retries, and a dropped `end` freezes the rider's card until ActivityKit's own multi-hour ceiling reaps it — so the ambiguity is resolved in its favour. Backdating the *update* instead would put the risk on the alert, and an alert whose stamp collides with an ETA tick sent a second earlier is the same silently-swallowed expansion, relocated. On a completed ride the hold below makes the gap five minutes and the question moot; the one-second rule still governs every ending that is not held.

**The unhappy endings are unchanged and remain a single push.** `declined`, `cancelled` and `reservation_expired` resolve to no phase at all, so no pre-end update is scheduled — but their `end` still carries their own final content-state, which Apple asks for and which is what the card renders for the seconds before it dismisses.

##### The completed `end` is HELD for the linger ([MYR-421](https://linear.app/myrobotaxi/issue/MYR-421))

**The measurement.** An `.ended` Activity **leaves the Dynamic Island about 1.4 seconds after the end lands** — the lock-screen card stays for its `dismissal-date`, but the island presence dies with the end. With the pair one second apart, the sixth expansion's check was on the island for roughly **two seconds in total** and then the Activity vanished from it. r19's report was that the completed check still could not be found, and the timing is the whole of why: nothing was wrong with the payload.

**So the two halves are pulled apart.** The `completed` transition sends **only the alerting `update`** — check, full track, sixth expansion — and returns. The `end` follows **five minutes later**, carrying the same final content-state and no alert, sent by the ETA ticker's held-end pass. The island keeps the check for the whole hold; the card keeps its own five minutes past the `end`.

**One number, not two.** The hold horizon and the `dismissal-date` offset are the same `DismissAfter` — the five minutes the client also counts down locally ([MYR-405](https://linear.app/myrobotaxi/issue/MYR-405)). It is deliberately **not** configurable: an operator-tunable hold is a chance to disagree with a number the client independently holds.

**The `dismissal-date` is computed from the `end`'s OWN send instant**, which is the arithmetic that makes the hold safe rather than merely late. Measured from the completion five minutes earlier it would already be in the **past** when the `end` landed — and a dismissal-date in the past tells iOS to remove the Activity **at once**, so the lock-screen card would disappear the same moment the island did and the linger would be gone from both surfaces. The consequence is stated rather than hidden: for a phone whose app is dead, the card's total life is now the hold **plus** the dismissal — about ten minutes rather than five. Whenever the app is alive the client's own five-minute reaper wins, as it always has.

**The rows SURVIVE the window, and everything that reads `ended_at IS NULL` was audited for it.** The tombstone moves to the delayed `end`; tombstoning at completion would exclude the row from its own final push, which is precisely the defect the send-then-tombstone ordering exists to prevent, relocated in time.

| Reader | Behaviour during the five minutes | Why it is right |
|---|---|---|
| Active-leg list (the ETA ticker) | **No ticks.** `completed` is not in `ActiveLegStatuses` — the filter is `requested`/`accepted`/`arrived`/`enroute`. | A tick would re-push a non-alerting update over the arrival state and reset the staleness clock on a ride that is over. |
| Lifecycle banner gate (§7.21.6) | **Suppresses nothing new.** The gate is keyed `(ride, user)`, and the only banner it could suppress on THIS ride is one for `completed` — which `statusAlert` does not generate, and `completed` is terminal so no further transition can fire. A banner for a *different* ride (the rider books again) looks up a different key and is unaffected. `ride.due` is never gated at all. | The window cannot silence anybody. |
| Registration (§7.21.1) | **Still 409s.** The guard is on the ride's STATUS, not on `ended_at`, so a client that re-registers during the window is still told the ride is closed. Unchanged. | A closed ride is closed whether or not its card has been ended yet. |
| Client end (§7.21.2) / APNs 410 | **Drops the row out of the due list.** A rider who swiped the card away, or whose app ran its own reaper first, tombstones or deletes the row and the held `end` is never sent. | The card is already gone; the end has nobody to reach. |
| **Owner vehicle teardown** (MYR-258) | **Ends the card as `completed`, not as `cancelled`.** This read is `ended_at IS NULL` too, and before the hold it could never match a completed ride — so it always meant "in flight" and `cancelled` was always true. Now it sees completed rides for the whole window, and the read carries each ride's **status** so the loop can tell them apart. | A rider who finished their trip and whose owner then unlinked the car would otherwise watch their completion check overwritten with a CANCELLATION and dismissed in 30 seconds. **Skipping completed rides is not the fix**: the teardown's cascade `DELETE` destroys the rows moments later, so the held-end pass would never fire and the card would sit on its arrival state until ActivityKit's ceiling. This is the last chance to end it, so it is ended honestly — final `completed` state, no alert, the same five-minute linger. |
| 24-hour stale sweep | **Cannot reach the row.** It keys on `updated_at`, and a ride is ticked every 60–90s until it completes, so a completing row is at most ~90 seconds old. | The 24-hour floors are untouched. |
| Phase-6 mark | **Raised by the announcement**, while the row is live, exactly as MYR-418 left it. | Unchanged. |

**THE TOMBSTONE IS THE RECEIPT FOR A DELIVERED `end`, NOT FOR HAVING TRIED.** A completed ride whose held `end` fails — a failed ride-context read, a failed registry read, an APNs 5xx or timeout — keeps its rows **live**, and the next pass re-lists it. Ending it on a failed attempt would drop it out of the due list permanently, because the ride is terminal and nothing else will ever look at it again; and since this path is a **batch**, one 30-second APNs blip overlapping a pass would do that to every card in the pass at once. Retrying is safe because the `end` is idempotent: a second one at an Activity the phone has already ended is answered `410`, which deletes the row. Two outcomes are deliberately **not** treated as failures — a rider who muted `ride_lifecycle` (a normal, permanent state, which would otherwise be retried every 75 seconds forever) and a token APNs permanently rejected (the row is already gone). The unhappy endings keep tombstoning unconditionally, because nothing ever comes back for them.

##### The retry TERMINATES, and three bounds are what make that true

Retrying is not free, and "retry until it works" is not a termination guarantee. **Only `410` and `400` take a row out of the due list.** A `403 TopicDisallowed` from a misconfigured `APNS_TOPIC`, a `404`, a timeout — all come back retryable, and `403 ExpiredProviderToken` is *deliberately* retryable too (the provider JWT is re-minted on a 40-minute cycle, and treating it as permanent would delete live rides' registrations during a credential blip). Left alone, one poisoned row would be re-attempted every 75 seconds until the 24-hour registration sweep deleted it: roughly **1,150 doomed pushes and no `end` ever sent**. Three bounds close that, and each closes something the others do not.

**1 · A give-up horizon — 30 minutes from completion.** Past it the card is **abandoned**: no further push, the rows tombstoned, and a loud `ERROR` naming the count. Thirty minutes is six times the hold and comfortably outlasts a rolling deploy, a database failover or any APNs incident this service has seen — and note that the *phone* being reachable is not what is being waited on, since an `end` Apple accepts is stored for 24 hours by its own `apns-expiration`; the window is for our ability to hand the push to Apple at all. It is short enough that the value being abandoned has genuinely decayed: the client's own five-minute reaper ([MYR-405](https://linear.app/myrobotaxi/issue/MYR-405)) removed the card on any phone whose app is alive, so what remains at thirty minutes is a dead-app phone showing a ride that finished half an hour ago. **The horizon is enforced by a bulk `UPDATE` with no `LIMIT`, run first on every pass** — not per ride — because the due list is `LIMIT`ed and ordered newest-first, so the rows *at* the horizon are exactly the ones a capped pass would reach last or never. A statement that cannot be starved is the only honest terminator.

**2 · Newest-completion-first ordering.** A reversal of what the query shipped with. Oldest-first made a capped pass shed the least overdue, which was correct while the list drained every pass and became a **starvation bug** the moment a failed `end` started leaving its row in place: an undeliverable ride then holds the head of every subsequent pass forever, so a few hundred of them (one bad `APNS_TOPIC` will do it) push every newly completed ride out of reach. Newest-first sheds the ends that matter least — the oldest, whose card is stalest and which the horizon is about to retire anyway — and a healthy backlog drains in the same number of passes either way, since the same number leaves per pass regardless of order.

**3 · A pass time budget and a per-ride deadline.** `RunPass` runs the ETA refresh and then the held ends **serially on one goroutine**, so a saturated batch does not delay the refresh it already ran — it delays every subsequent one, by keeping the loop away from its timer. At `MaxPerPass` = 200 with each send bounded only by the APNs client's own 10s × 2, an unbounded batch is tens of minutes with **no ETA refresh for any live ride**. So each ride runs under a 15-second deadline (one clean attempt; the client's *inline* retry is deliberately cut short, because the pass is already the retry and a pass retry costs the loop nothing while an inline one holds every ride behind it), and the pass yields after half a tick interval — checked **before** starting a ride, so the overshoot is at most one per-ride deadline. The bound that follows: **the held-end work costs at most `Interval/2 + 15s` ≈ 52s at the default cadence, under the 60-second floor of the jittered interval, so it can never delay the ETA refresh by more than a single tick.** The deadline cannot truncate a write — both mark writes and the tombstone run on contexts detached from it.

**A pass stops at the first sign of shutdown.** The tombstone deliberately runs on a context detached from cancellation, so a loop that ploughed on through `SIGTERM` would keep tombstoning rides whose pushes were failing *because* the context was cancelled. The check is at the top of each iteration; the survivors are found by the next process's startup pass.

**Two replicas can both send one ride's `end`, and that is accepted rather than prevented.** The service runs a single replica today; if it did not, both would list the same due ride, both would push, and the loser's tombstone would move zero rows — one duplicate `end` at a card that is already ending, invisible to the rider and answered `410` at worst. The obvious guard is a claim-then-send latch (`UPDATE … WHERE ended_at IS NULL RETURNING`, the shape `ClaimReservationDispatch` uses) and it is the **wrong** trade here for exactly the reason above: claiming before the send makes the tombstone a receipt for having tried, which is the stranding this section rules out. The same choice §7.21.4 makes about the island opening once more than it had to.

**It is a SWEEP, not a timer, and that is the crash-safety argument.** An in-process `time.AfterFunc` scheduled at completion is lost by every deploy and every restart — and the ride it was holding is **terminal**, so no transition will ever fire for it again and the ETA ticker does not list completed rides. A lost timer is a card stranded until ActivityKit's own multi-hour ceiling. The pass instead recomputes "completed at least five minutes ago and not yet ended" from `go_ride_requests.completed_at`, a column the status write itself stamps, on every pass of every process. A restart therefore costs **one pass of latency**, and the ticker's startup pass fires ~20 seconds in.

**The bound on island presence is the hold to the hold plus one tick interval** — five minutes to about six and a half — for a card whose `end` is delivered on its first attempt. A card whose end keeps failing is retried for up to the 30-minute horizon and then abandoned, so **the outer bound on a completed row's life is the horizon, not the 24-hour sweep**; nothing can stretch either further, because both predicates are durable columns rather than anything the loop remembers. `COALESCE(completed_at, updated_at)` is a floor for the same reason: a completed ride that somehow carried no stamp would otherwise be invisible to this query **forever**, and degrading to "held slightly longer" is the only acceptable failure.

**The unhappy endings are NOT held.** `declined`, `cancelled` and `reservation_expired` keep their immediate single `end` and their 30-second dismissal. Holding them would invert what they are for: the news *is* the ending, and a card that keeps promising a car for five minutes after the ride was cancelled is the defect this surface pushes on every transition to avoid.

**The topic is DERIVED, not configured.** It is `APNS_TOPIC` + `.push-type.liveactivity`, so there is no second environment variable and no way for the two topics to drift apart in an environment file. Apple requires the suffix on the topic **and** the matching `apns-push-type` header; either alone is rejected as `TopicDisallowed`, a `403` that reads like a credential problem and is not one.

**There is no `userInfo`.** Unlike the §7.17 alert payload, an Activity update carries no ride id outside the content-state — the token already addresses exactly one Activity on exactly one ride, so a ride id on the wire would be a P0 identifier buying nothing.

**`timestamp` is the ordering defence.** ActivityKit discards an update older than the one it is already showing, which the network makes a routine event rather than a theoretical one.

**Priority 10 for lifecycle transitions, 5 for ETA ticks (MYR-194).** Apple throttles high-frequency Activity updates by budget, so a periodic ETA refresh rides at conserving priority and never competes with "your car is here" for that budget.

**`stale-date` is timestamp + 3 minutes, and it is an HONESTY mechanism.** Once it passes, ActivityKit renders its own "as of X min ago" treatment on its own, so a phone that stopped receiving pushes SAYS SO instead of presenting a three-hour-old ETA as current. Three minutes is a little over two missed ticks — long enough that one dropped push does not flap the display, short enough that a rider is never confidently misinformed. This is what makes every degraded mode below safe rather than merely tolerable.

**`apns-expiration` is PER SHAPE, and the shapes want opposite things from it.**

**An ordinary `update` expires at its stale-date.** A queued ETA refresh that only reaches the phone after its content stopped being trustworthy would overwrite the Activity with a state ActivityKit is about to mark stale anyway — **worse than not arriving**, because it resets the staleness clock on information that has already expired. Late is worthless, so Apple is told to drop it.

**An ALERTING update takes the same 24-hour floor as an `end` ([MYR-413](https://linear.app/myrobotaxi/issue/MYR-413) review).** Since §7.21.6's gate, a rider watching a card gets **no lifecycle banner** on the phases the island alerts on — so the alerting update is the *sole* carrier of "your car is here" for that rider, and the banner it replaced had **no `apns-expiration` at all**, which is to say APNs stored and retried it for a phone that was off. Left at the three-minute stale-date, an alerting update to a phone in a tunnel is discarded by Apple and the rider reconnects to **nothing** — neither surface ever told them the car arrived. That is the ordinary case a lock-screen notification exists for, not an edge, and it is the asymmetry the gate would otherwise introduce: a durable push deleted in favour of a deliberately perishable one.

**A late alerting update is SAFE to deliver in a way a late ETA tick is not**, which is what makes the exception sound rather than merely necessary. The stale-date travels **in the payload** (`aps.stale-date`), independent of the header, so an update delivered an hour late self-declares as stale and gets ActivityKit's own "as of X min ago" treatment instead of presenting expired data as current; and `aps.timestamp` ordering means it can never overwrite a newer tick that arrived ahead of it. The worst case is an island that expands late on a card that already says it is out of date — strictly better than silence.

**An `end` expires 24 hours out** (or at its `dismissal-date`, on the rare occasion that is even later). It is the only push in this system with **no successor**: the registration rows are tombstoned the moment it is sent, the ETA ticker will never look at them again, and nothing retries. Given the stale-date it would be discarded by APNs after ~3 minutes, so a rider whose phone was in a tunnel when their ride was declined would keep "your car is on its way" on the lock screen until ActivityKit's own multi-hour ceiling removed it. A day comfortably outlasts a flat battery or an overnight flight.

It is deliberately **not** pinned to the `dismissal-date`, which is the tempting answer and the wrong one: that date is **30 seconds** for the unhappy endings, which would make the most important push in the feature the shortest-lived one. And an `end` delivered after its dismissal-date is not wasted — a dismissal-date in the past tells iOS to remove the Activity at once, which is exactly the outcome wanted for a card that has been lying since the tunnel.

**`dismissal-date` policy.** A `completed` ride lingers **5 minutes** ([MYR-406](https://linear.app/myrobotaxi/issue/MYR-406), shortened from 15): the rider should get to look at the arrival state rather than have it vanish the instant the owner taps "Dropped off". The client ends a completed Activity locally at the same five minutes ([MYR-405](https://linear.app/myrobotaxi/issue/MYR-405)) and that timer wins whenever the app is alive, so this date is the FALLBACK for a phone whose app is dead — two different numbers meant the same ride lingered for five minutes or fifteen depending on something the rider cannot see. Since MYR-421 the same five minutes is *also* the hold before the `end` goes out, and the date is measured from the `end`'s own instant; see the held-`end` section above for what that costs a dead-app phone. The unhappy endings — `declined`, `cancelled`, `reservation_expired` — dismiss after **30 seconds**. Not zero, deliberately: an Activity dismissed the same instant it is ended can disappear before the rider's eyes reach it, and "my ride vanished" is a worse experience than the bad news itself.

**Ending is a send, THEN a tombstone, in that order** — and on a completed ride the tombstone travels with the **held** `end` five minutes later, not with the announcement. A row ended first would be excluded from its own final push, leaving the lock screen on the last state it happened to receive — which for a declined ride is "your car is on its way", and for a held completion would be no `end` at all. The same ordering is what lets the completion pair's alerting update raise the phase mark, since that write is scoped to live rows.

#### 7.21.5 Update cadence and the preference gate

**Lifecycle transitions** are pushed as they happen: the notifier subscribes to the same `ride.status.changed` topic §7.17 does, so every transition the service already publishes is covered without a call site being edited. **Nothing is filtered out** — unlike an alert, where a rider's own cancellation would be noise, a Live Activity still showing "on its way" after the rider cancelled is simply a WRONG lock screen. Every transition is **one** push except `completed`, which is the pair described in §7.21.4 — and whose second half is sent **five minutes later by the ticker**, not by the transition.

**ETA ticks** re-push the current content-state every **60–90 seconds** (75s ± 20% jitter) while the ride is in `requested`, `accepted`, `arrived` or `enroute`. The ETA is the one thing on the Activity that goes wrong by SITTING STILL — the status changes only when something happens, but "arrives at 4:12" stops being true the moment traffic does. The jitter also de-synchronises replicas so they do not push Apple in a burst.

**`completed` is deliberately absent from that list, and it is now load-bearing rather than incidental.** A completed ride's registration row stays live for the five minutes its `end` is held (§7.21.4), so the status filter is the only thing standing between a finished ride and an ETA tick. A tick there would re-push a non-alerting update over the arrival state and reset the card's staleness clock on a ride that is over.

**The ticker also carries the held `end`.** Each pass does three independent things, in this order: refresh every active leg, send the `end` for every ride that completed at least five minutes ago and has not been ended, then run the hourly stale sweep if it is due. Only the first is an ETA refresh, and only the first answers to the kill-switch below.

**Two of those four statuses have no ETA to refresh, and tick anyway.** A stationary `arrived` car reports no nav route, and a `requested` ride has no car assigned at all. What each tick still moves is the `timestamp` and the `stale-date` — and that is the point, because an Activity nobody pushes to slides into ActivityKit's own "as of X min ago" rendering after three minutes. For `arrived` that would be a card apologising for being out of date while the rider stands next to the car it describes; for `requested` it would be "Finding your ride" accusing itself of a fault while the search is genuinely still running.

##### The Dispatch phase — the Activity now starts at REQUEST ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398))

The v3 card opens on **Dispatch** ("Finding your ride"), which means the app starts the Activity when the rider taps request rather than when the owner accepts. Three consequences on this surface, and **no new shape** among them:

**Registration already accepted a `requested` ride and still does.** §7.21.1 refuses exactly two things — a terminal status and a lapsed reservation — and `requested` is neither. Both the handler's read and the guard inside the upsert are unchanged.

**The ticker was widened**, because it was not: `requested` joined `accepted` / `arrived` / `enroute` in the active-leg query, so the Dispatch card stays fresh while the rider waits for an answer.

**`eta` is withheld on `requested`, and that is a new rule.** The projection reads navigation from the ride's vehicle, and before the owner accepts, that car has been assigned to nobody — it may be driving the owner somewhere else with navigation on. "Finding your ride" beside an arrival time computed from an errand is not a degraded ETA, it is a fabricated one. This is the same defect the dormancy gate closes for the progress track, and the fix is the same shape: **no key**. It is scoped narrowly to `requested`; the wider question of whether a *stale* `eta` should be withheld on statuses that do have a car assigned is still the open one named above, and still needs its own issue — this arm is about a leg with no driver, not about a reading with no age.

`progress` needs no new rule: a status with no leg has never produced a fraction (`legForStatus`), so a Dispatch card is trackless by the mechanism that was already there.

**Reservations are unaffected.** A scheduled ride is `accepted` from the moment it is booked and gets no Activity until it is due — that rule lives in the dormancy predicate, not in this list, and the Dispatch phase is about **instant** rides awaiting an answer.

**Both endings of a `requested` ride are already covered.** A rider cancel (`requested → cancelled`) and an owner decline (`requested → declined`) both go through the guarded status write that publishes `ride.status.changed`, so the lifecycle notifier delivers the terminal `event: "end"` with its dismissal-date and tombstones the rows, exactly as it does from `accepted`. Neither carries an island alert: both are outside the design's six.

**Alerts are attached by the phase ladder, not by the cadence.** A lifecycle push and a ticker pass compute the same phase from the same state, so which of the two happens to run first after a transition cannot change how many times the island opens — see §7.21.4.

**Kill-switch: `LIVE_ACTIVITY_TICKER_ENABLED`, default `true`.** Turning it off stops **only** the periodic ETA refresh; lifecycle transitions keep updating the Activity, and the Activity's own stale-date does the rest. That intermediate state is the one an operator actually wants when Apple starts throttling — degraded and honest rather than dark. **The loop itself keeps running** ([MYR-421](https://linear.app/myrobotaxi/issue/MYR-421)): the held completion `end` rides it and is the second half of a *lifecycle transition*, not a refresh, so leaving it inside the switch would have made the sentence above false and turned a throttling mitigation into every completed card stranded on every rider's lock screen until ActivityKit's multi-hour ceiling. The stale sweep keeps running for the same reason — it is a `DELETE`, not a push. `PUSH_ENABLED=false` (or a keyless deployment) silences the whole channel instead; in every such state the endpoints stay mounted and registrations still persist. See [`docs/deployment.md`](../deployment.md).

**The whole surface rides the existing `ride_lifecycle` push-preference category ([MYR-349](https://linear.app/myrobotaxi/issue/MYR-349), §7.19).** A rider who muted ride updates gets **no Activity updates at all** — neither lifecycle nor ETA — checked per recipient before every send and failing OPEN on a preference-lookup error, exactly as the alert notifier does. **The Activity still runs.** It was started locally by the app, which needs no permission to do so, and with nothing arriving it falls back to its own stale rendering once each stale-date passes: a card that says it is out of date rather than one that lies. This is deliberate and is the reason the gate can be a simple silence — a muted rider loses freshness, never correctness, and the same switch that stops the alerts stops the pushes that would contradict them.

**Registrations are unaffected by the gate.** A muted rider's token is still stored, so flipping `ride_lifecycle` back on resumes updates on the next transition or tick without the app re-registering.

**A muted rider cannot end up watching a stale countdown, either.** Because the ticker applies the SAME per-recipient gate as the lifecycle path, muting suppresses the ETA refresh and the lifecycle update together — there is no state in which the ETA keeps ticking while the ending is withheld. Silence plus the stale-date is a card that admits it is out of date; a half-gated surface would be one that quietly lies.

**A capped pass ROTATES; it does not shed the same rows every tick.** The per-pass cap (`MaxPerPass`, 200) exists as a guard rail rather than a routine truncation, but when it does bite, the Activities it sheds must not be the same ones forever. The pass reads least-recently-pushed first and **stamps `updated_at` on every Activity it successfully delivered to**, so the next pass reaches the ones it missed. An Activity Apple refused is deliberately NOT stamped — a permanently failing row must not hold the front of the queue. The same column is what the 24-hour sweep keys on, which is why a long ride is never swept out from under a live Activity: a row still being pushed to is by definition not stale.

**Observability.** Registration lines carry the P0 `ride_request_id`, `user_id` and `sandbox` plus the **8-character token prefix only** — never a whole token ([`data-classification.md`](data-classification.md) §3.2). The send line carries `ride_id`, `event`, `status`, `has_eta` and `has_progress` booleans, `alerted` and `alert_phase`, and delivery counts — and deliberately **not** the content-state, which embeds the P1 `destination`. The two booleans are the operational question this surface actually gets asked — "why is the rider's card blank?" — answered without logging a value: `has_progress: false` on a mid-leg ride says the car's navigation is absent or stale, which is the diagnosis. The fraction itself is P0 and could be logged; it is not, because a per-push number that nobody reads is noise in a line that is read on every incident. The two alert fields answer the other question this surface gets asked — "why did the island open again?" — and are the exception to that economy: `alerted` is nonzero on six pushes per ride and never on the other forty, so a line reporting it twice for one phase is the bug report. A ladder position names nothing and is P0. Handler:
[`internal/telemetry/ride_activity_token_handler.go`](../../internal/telemetry/ride_activity_token_handler.go);
sender `internal/push/{activity,activity_apns}.go`; consumers
`internal/push/{activity_notifier,activity_notifier_send}.go`; progress
derivation [`internal/push/activity_progress.go`](../../internal/push/activity_progress.go);
island-expand ladder
[`internal/push/activity_alert.go`](../../internal/push/activity_alert.go);
content-state projection and `asOf`
[`internal/push/activity_content_state.go`](../../internal/push/activity_content_state.go);
ETA ticker
`internal/push/activity_ticker.go`; the terminal pair and its hold
`internal/push/{activity_terminal,activity_held_end}.go`; store
[`internal/store/live_activity_repo.go`](../../internal/store/live_activity_repo.go)
and `internal/store/{live_activity_leg,live_activity_progress,live_activity_held_end}.go`; wiring
`cmd/telemetry-server/wiring_live_activity.go`.

**The held `end` logs its own two lines.** `live activity end held` on the announcement (`ride_id`, `status`, `hold`) and `live activity ticker: held ends sent` on the pass (`rides`, `held`, `capped`). Both are P0. They are the answer to the one operational question this shape introduces — "the ride finished six minutes ago and the card is still there" — because between them they say whether the hold was recorded and whether any pass has since come for it.

---

#### 7.21.6 The lifecycle banner defers to the island (MYR-413)

> **Anchored:** FR-9.3, NFR-3.21. No REST surface, no wire shape — this is a rule about which of two APNs channels speaks.

**The defect.** r17 build `202608011648`, at `arrived`: the rider's phone showed the compact Dynamic Island with a lock-screen banner sitting on top of it — *"Your car is here — your turn to start / Open MyRoboTaxi to start the ride."* Both pushes are ours, and they are sent by **two different subscribers to one `ride.status.changed` event**: the Live Activity notifier attaches the `aps.alert` of §7.21.4 (which asks iOS to expand the island), and the §7.17 ride-lifecycle notifier sends a separate ordinary alert carrying the same news in prose.

They are not merely redundant — **they compete for the same pixels.** A banner and the island's expanded presentation want the same strip of screen and the banner is the one that wins it, so the very push that duplicates the island's message is also the thing standing in front of it. Removing it is not tidying; it is how the expansion becomes visible.

**The rule.** A ride-lifecycle banner is **skipped** when both hold:

1. the transition is one the island alerts on — the status half of §7.21.4's ladder: `requested`, `accepted`, `arrived`, `enroute`, `completed`; **and**
2. the **recipient** has a live (`ended_at IS NULL`) `go_live_activities` row for that ride.

Everything else keeps its banner. Each exclusion is load-bearing:

| Case | Banner | Why |
|------|--------|-----|
| `declined` | **always sent** | Outside the design's six phases. Its Activity update carries no alert and expands nothing, so the banner is the only announcement there is — and this is among the worst news of a ride. |
| `cancelled`, `reservation_expired` | **no banner exists** | Not a suppression rule — a statement of what is already true. §7.17's `statusAlert` generates a lifecycle banner for `accepted`, `declined` and `arrived` **only**, so these two transitions have never produced one, before or after MYR-413. Their only announcements are the Activity's `end` push — which carries no `aps.alert` either, since `alertPhaseFor` maps them to no phase (§7.21.4) — and the card's final state before its 30-second dismissal. Whether they *should* get a banner is an open product question, not a gap this section closes. |
| Tombstoned row (rider swiped the card away, or the client ended it) | **sent** | A tombstone reads as *absent*. A card that is gone cannot deliver the news, and nobody may be left dark because a surface they can no longer see was assumed to be working. |
| Row deleted after an APNs rejection | **sent** | Same reason, same mechanism: no row, no suppression. |
| Never registered (most rides — booked from a browser) | **sent** | Unchanged pre-MYR-413 behaviour. |
| The **owner's** pushes | **unaffected** | Not a rule — a consequence of the key. The row is `(ride_request_id, user_id)` and v1 registers only the rider (§7.21.1), so an owner's lookup finds nothing. |
| `ride.request.created` (the owner's approval prompt) | **never gated** | `requested` is on the ladder, but this push goes to the OWNER and asks them to accept or decline — a different message to a different person, not a phase announcement. It is also load-bearing on this platform: **an owner who rides their own car is both parties**, so gating it would let the rider's own card swallow the prompt needed to accept the ride at all. |
| `ride.due` (the reservation's "heading your way") | **never gated** | Its own topic, and it moves no ride status, so the ladder never advances and no island expands for it. A gate there would delete the notification rather than defer to a better one. |

**Arriving is absent from the list because it is not a ride status.** It is a threshold the ETA ticker crosses (§7.21.4), so no lifecycle banner is ever sent for it and there is nothing to suppress.

**Three of the five statuses in the rule are unreachable today, and are listed for FORWARD-SAFETY rather than because a banner exists for them.** §7.17's `statusAlert` covers `accepted`, `declined` and `arrived` only, so `requested`, `enroute` and `completed` produce no lifecycle banner for the gate to suppress and **the gate is reachable today only on `accepted` and `arrived`.** They are carried in the set anyway so that a banner added later cannot land in front of an expanding island by omission — the set is a claim about which transitions the *island* alerts on, not about which ones the banner path currently reaches.

**The `completed` case is therefore moot rather than merely acceptable.** There is no `completed` banner to weigh against the card's five-minute linger (§7.21.4), because none is generated.

**And that is what makes MYR-421's held `end` safe here.** A completed ride's row now stays live (`ended_at IS NULL`) for the five minutes before its `end` goes out, so this gate reads it as a live card for that whole window — a window it never used to see. Nothing can be silenced by it. The gate is keyed `(ride_request_id, user_id)`, so the only banner it could suppress is one for **this ride**, and `completed` is terminal: no further transition on it can fire, and `statusAlert` generates nothing for `completed` in any case. A banner about a *different* ride — the same rider booking again a minute later — looks up a different key, finds no row, and is sent. `ride.due` is never gated at all. The window is invisible to the banner channel by construction rather than by luck.

**IT FAILS OPEN, in all three of the ways it can fail** — no registry wired, a lookup error, and a delivery no site marked suppressible — for the same reason and in the same direction as §7.19.3's preference gate. A duplicate notification is a minor annoyance to a human; a missed one is a rider standing on a sidewalk. A gate that failed CLOSED would turn a transient database hiccup into a rider who is told by **neither** surface that their car arrived.

**Ordering: after the preference check, before the device lookup.** A rider who muted `ride_lifecycle` costs one read rather than two, and a suppressed notification logs `push suppressed by live activity` rather than `push sent … delivered=0`, which in production reads like a delivery failure.

**THE BOUND, STATED HONESTLY: SUPPRESSION IS PER USER, NOT PER DEVICE.** The registry's natural key is `(ride, party)` because ActivityKit rotates an Activity's push token mid-ride (§7.21.1) — one row is one *person's* card, not one phone's. A rider carrying two phones, with the Live Activity running on one and the other merely signed in, presents a **single row**, and the read cannot tell the two apart: the banner is suppressed on **both**. That is the deliberate trade, and the alternative is worse — a banner on every phone including the one already showing the card is precisely the duplicate this section exists to remove. Making it per-device would need a device dimension on a table whose key is a *person's* Activity, which is a schema change and a separate issue, not an oversight here.

**Observability.** `push suppressed by live activity` carries `topic`, `ride_id` and `user_id` — all P0, two opaque cuids and a topic name. A failed registry lookup logs at ERROR and says it is sending the banner anyway. Gate: [`internal/push/notifier_activity_gate.go`](../../internal/push/notifier_activity_gate.go); enforcement `internal/push/notifier_send.go`; store read [`internal/store/live_activity_presence.go`](../../internal/store/live_activity_presence.go); wiring `cmd/telemetry-server/wiring_push.go`.

---

### 7.22 `GET /api/vehicles/{vehicleId}/booked-windows` (schedule-picker conflict read — MYR-385)

> **Anchored:** FR-9.1, FR-9.3, NFR-3.21.
> **Schema:** [`schemas/booked-windows.schema.json`](schemas/booked-windows.schema.json) — `VehicleBookedWindowsResponse`, `BookedWindow` (contracts **v0.26.0**).
> **Persisted:** nothing. A pure read over `go_ride_requests`; **no migration**, no new column, no new index — the reservation arm is served by `idx_go_ride_requests_vehicle_window` (migration 0026), which carries `vehicle_id = $1` plus **both `scheduled_for` range bounds** as Index Cond (see "How the index serves it" below).
> **Paired gate:** §7.8's MYR-383 window gate, which is what this surface reports on.

The **read side** of the §7.8 booking gate. The gate refuses a colliding reservation at create with `409 vehicle_unavailable` / `subCode: time_conflict`; in r15 that refusal was the **first** thing a rider heard — the schedule picker offered noon for a car the rider had themselves already booked at noon — and a refusal arriving only after somebody has committed to a choice reads as a bug rather than as a rule. This endpoint lets the picker dim the slot instead.

```
GET /api/vehicles/{vehicleId}/booked-windows?from=2026-08-01T00:00:00Z&to=2026-08-08T00:00:00Z
```

```json
{
  "items": [
    { "start": "2026-08-01T11:15:00Z", "end": "2026-08-01T12:45:00Z", "pending": true,  "own": false },
    { "start": "2026-08-01T18:15:00Z", "end": "2026-08-01T19:45:00Z", "pending": false, "own": true  }
  ]
}
```

#### The invariant, and how it is held

**A slot the picker dims from this response is exactly a slot the gate would refuse at that instant; a slot it leaves enabled is exactly one the gate would allow.** That is not maintained by care. Both surfaces are assembled from the *same three SQL fragments* in `internal/store/ride_request_conflict_queries.go`:

| Fragment | What it fixes | Used by |
|---|---|---|
| `rideWindowReservationOccupies` / `rideWindowInstantOccupies` | which rides occupy a window at all (the two arms + the count-pending flag) | the gate **and** this read |
| `rideWindowOverlap(anchor, lower, upper)` | the overlap test, `anchor > lower − W AND anchor < upper + W` | the gate **and** this read |
| `rideWindowHalfWidthExpr` / `RideConflictWindow` (Go const) | the half-width `W`, bound at `$4` | the gate **and** this read |

MYR-385 **factored** the gate's predicate; it did not copy it. Both statements are `occupies AND overlap` per arm, built from the fragments above; the only difference is what the bounds are. The gate passes the **same** placeholder for both (`rideWindowOverlap(anchor, $2, $2)` — one proposed instant); the read passes the caller's **range** (`rideWindowOverlap(anchor, $2, $3)`). Same rows, same strictness, and no second spelling of the rule to drift.

#### How the index serves it

`rideWindowOverlap` writes the half-width onto the **bounds**, not onto the anchor — `anchor > lower − W AND anchor < upper + W` rather than the algebraically identical `anchor − W < upper AND anchor + W > lower`. That looks redundant and is load-bearing: with `W` on the anchor's side the anchor is an **expression**, and Postgres cannot match an expression to the plain `scheduled_for` column of `idx_go_ride_requests_vehicle_window`. Stated exactly, the index serves:

| Conjunct | How |
|---|---|
| `r.vehicle_id = $1` | **Index Cond**, leading column |
| `r.scheduled_for > …` / `r.scheduled_for < …` | **Index Cond**, second column — *only* with the bare-column spelling |
| `scheduled_for IS NOT NULL`, `status IN (…)` | the index's own **partial predicate**, proved rather than probed |
| the ACTIVE-INSTANT arm's `NOW()` bounds | not index conditions and do not need to be — a separate bitmap branch over the `scheduled_for IS NULL` partial indexes (0004/0013), at most one row per vehicle |

EXPLAIN-verified against a real Postgres by `internal/store/ride_request_window_index_plan_test.go`, which seeds 1241 open reservations on one car (11 169 rows) and asserts the range reaches Index Cond for **both** statements. With the half-width on the anchor the planner abandons the index entirely: `Seq Scan`, `Rows Removed by Filter: 11168`, 251 buffers, versus a `Bitmap Index Scan` returning 1 row from 1 heap block and 9 buffers. That is the conflict probe, which runs on every create and every accept **inside the per-vehicle advisory booking lock**.

#### Six rules, all load-bearing

- **CONCRETE endpoints, not an anchor plus a radius.** The server could emit each occupying ride's instant and let clients add ±45 minutes. It deliberately does not. Per §7.8, the half-width is a **product guess** appearing in exactly one place on the server (`store.RideConflictWindow`), bound into SQL as a parameter, and encoded in **no schema, no enum and no client**. Emitting resolved endpoints is what preserves that: widening or narrowing it changes every picker on the **next response** — no client release, no contract bump, no migration. **Clients MUST NOT re-derive, re-centre, pad or round these instants, and MUST NOT infer the half-width from them.** A client that hard-codes 45 minutes is a client that will silently disagree with the gate the day the number moves.
- **The interval is OPEN at both ends.** Inherited from the gate's strict comparison — "exactly this far apart is allowed", a legal back-to-back booking. A booking at exactly `start` or exactly `end` is **accepted**. A picker dims `start < slot < end`; one that dimmed the endpoints would refuse slots the server would have taken. A store test books against an emitted edge in both directions and uses the gate itself as the oracle.
- **CREATE-path semantics: a `pending` claim blocks in full.** §7.8's two landing sites disagree about a merely `requested` reservation on purpose — create counts it, accept does not. A picker is a rider deciding what to **create**, so this surface answers with `countPending = true`. `pending` therefore changes only the **words** ("already requested" rather than the untrue "booked"); it never softens the block. Showing the accept-side answer would hand a rider a slot create is going to refuse, reintroducing the late 409 in the least obvious way available.
- **`own` tracks the RIDER, not the vehicle's owner.** True when the occupying ride is one the caller requested. The r15 report was a rider colliding with their **own** noon reservation, and "that car is busy" would have been a poor answer to it. Purely presentational — the gate does not care whose the other ride is. `own: false` says only "not yours", never who; and an owner browsing their own car sees `own: false` for rides other people booked in it, because the flag names the party the picker is speaking to.
- **Terminal rows free their window immediately**, exactly as in §7.8: a `declined`/`cancelled`/`completed` ride is outside both arms and stops being reported the instant it lands there. A picker that kept dimming a declined reservation's slot would be strictly worse than no picker at all.
- **A SNAPSHOT, not a subscription, and the `409` remains the authority.** Nothing pushes an update. A window can vanish before the rider submits (the holder declines) and a new one can appear (somebody else books). §7.8's `409 time_conflict` MUST still be handled; this surface reduces how often a rider meets it, it does not replace it.

#### The ACTIVE-INSTANT arm, and the one place the answer moves

§7.8's second arm has no stored instant: an instant ride the car is committed to (`accepted`/`arrived`/`enroute`) occupies a window around **now**, because a car mid-ride cannot also promise a pickup twenty minutes out. That arm is reported here, and it must be — omitting it would leave the next hour and a half enabled for a car that is currently driving somebody else.

It is reported **honestly, as a window anchored on the server's clock at the moment of the read**: `[now - 45min, now + 45min]`. Consequences, stated plainly rather than papered over:

- **It slides and the response does not.** The gate re-evaluates `NOW()` at submit time, so the interval this arm blocks moves forward with the wall clock while the response is frozen. The read can therefore **under-report** against a still-running ride — a slot an hour out is enabled now and would be refused if the rider submitted forty minutes from now with the ride still going — but it never over-reports against one.
- **It disappears when the ride ends**, which for an instant ride is usually soon, and is why under-reporting here is a small and self-correcting error rather than a growing one.
- **This is the only moving quantity on the surface.** Every reservation window is anchored on a stored `scheduledFor` and is exactly as true an hour later. The active-instant arm is the specific reason the create-time 409 stays the authority, and it is why a client SHOULD re-read the windows when a picker has been open for a long time rather than trusting a stale response.
- A merely **`requested`** instant ride occupies **nothing** — nobody has promised the car yet — mirroring the gate exactly. Dimming for it would refuse slots the gate would take.

#### Authorization — byte-for-byte the ride-CREATE gate

The caller must be the vehicle's **owner**, or a viewer holding a live accepted share whose grant carries the **ride capability** (`allowRides`, §7.5.7) — the identical `vehicleAccessFor(…, capRides)` call §7.8's create path makes, on the same handler, reached from the same `GetByID` row. Not "similar to" create's rule: **the same rule**, because this endpoint answers "what would create refuse?" and a caller create would turn away must be turned away here or the endpoint becomes an oracle answering a question `POST /api/ride-requests` would not.

| Caller | Result |
|---|---|
| Vehicle owner | `200` (no share lookup is issued) |
| Viewer with `allowRides` | `200` |
| Viewer without `allowRides` | `403 vehicle_not_owned` |
| Viewer whose grant is **suspended** | `403 vehicle_not_owned` — indistinguishable from having none |
| Stranger | `403 vehicle_not_owned` |
| Unknown vehicle | `404 not_found` |

A base-capability viewer can **see** the car and cannot **book** it, so a picker they can never submit from has nothing to dim; serving them would turn any share at all into a way to watch a stranger's car fill up. A refused caller never reaches the store — a read that happened and was then discarded is still a read.

**Deliberately NOT checked here:** the §7.18 ride-share pause and the §7.16 service window. Both refuse a *create*; neither describes a window on a calendar. Folding them in would make a paused car answer `items: []`, which a picker reads as **wide open** — the most misleading answer available. Those refusals are create's to deliver.

#### What this discloses, and why it is safe

Two instants and two booleans per occupying ride. The conflicting ride's **id, rider, requester name, passenger, pickup and dropoff never leave the statement** — P1, belonging to a party the caller is not ([`data-classification.md`](data-classification.md) §1.9). The instants and the `pending` flag are **P0 operational timing**, the same tier as `status` and the same disclosure §7.8's refusal message already makes when it names the conflicting instant.

**This grants no capability the caller lacked.** Somebody authorised to book this car can already discover the same instants one create attempt at a time, because each 409 names the nearest conflicting one; this endpoint only makes the answer cheap enough to render a picker with. That equivalence is exactly why the authorization gate has to be create's and not a looser one — it is the premise the disclosure argument rests on.

Note also what a caller **cannot** distinguish: `own: false` does not say whose the ride is, and two callers reading the same range receive byte-identical instants with only their own flags differing.

#### Query parameters and validation

| Param | Required | Default | Rules |
|---|---|---|---|
| `from` | no | **now** | RFC 3339 instant. May be in the past — an active-instant window straddles now, and clamping would make the response depend on a server clock the caller cannot predict. |
| `to` | no | **`from` + 7 days** | RFC 3339 instant. |

- `from` **must be strictly earlier** than `to`. Equal instants are `400 invalid_request` along with reversed ones: an empty range can only ever answer `items: []`, which a picker reads as "wide open" — a wrong answer dressed as a right one.
- The span **must not exceed 14 days** (`400 invalid_request`). It is **refused, never clamped**, for the same reason: a silently shortened answer looks complete and is not, and under-dimming is precisely the failure this endpoint exists to remove. A longer horizon is several calls. Like the half-width, the cap lives in exactly one place on the server — `store.MaxBookedWindowRange`, injected into the handler by `wiring.go` — because it is the store's bound (it is what makes the read's absent `LIMIT` safe), and the refusal message names the value it actually enforced.
- Only RFC 3339 is accepted. A bare date (`2026-08-01`) is a `400`, not midnight in an unnamed timezone.
- **UTC `Z` is recommended; a literal `+` in a query string MUST be percent-encoded** (`%2B`). A non-UTC offset such as `2026-08-01T00:00:00+02:00` is perfectly legal RFC 3339 and is honoured as the instant it names — but sent unencoded, the `+` decodes to a **space** before the handler ever sees it, and the value arrives as `2026-08-01T00:00:00 02:00`, which is a `400 invalid_request`.

#### Response

`200 OK` with `VehicleBookedWindowsResponse`. `items` is **always present** and is `[]` — never `null`, never an omitted key — for a free car, which is the common case and MUST render as an unrestricted picker rather than as an error or a loading state.

Ordered by `start` ascending, with a deterministic server-side tiebreak. **One item per occupying ride, not a merged union**: two items may overlap, and a consumer wanting a single dimmed mask should union them itself. They are not merged server-side because `pending` and `own` are per-ride facts a merge would have to drop or lie about. Overlap is rare in practice — the gate keeps open rides on one car at least a window-width apart — and the pairs that do overlap are the pre-gate legacy rows and the reschedule path, both documented **remaining boundaries of §7.8**, not of this surface.

**Unpaginated, and no cursor is needed.** The caller's range is the bound: 14 days divided by the gate's own minimum separation ceilings the result at a few hundred windows before ordinary booking density is considered, which is why the query carries **no `LIMIT`**. A limit would trade an unreachable runaway for a reachable silent truncation, and truncation here breaks the invariant the whole surface is built to hold.

A store read failure is `500 internal_error`, never an empty list — answering `items: []` on a query error would tell the picker the car is wide open, the single most dangerous wrong answer this endpoint can give.

Errors: `400 invalid_request` (missing `vehicleId`, unparseable/reversed/equal/over-wide range), `401 auth_failed`, `403 vehicle_not_owned`, `404 not_found` (unknown vehicle), `500 internal_error`. **No new error code and no new sub-code** — and in particular this surface never emits `vehicle_unavailable`/`time_conflict`: it reports windows, it never refuses a booking.

#### Implementation

Handler [`internal/telemetry/vehicle_booked_windows_handler.go`](../../internal/telemetry/vehicle_booked_windows_handler.go) (a method on `RideRequestHandler`, so its capability check *is* create's); store [`internal/store/ride_request_booked_windows_read.go`](../../internal/store/ride_request_booked_windows_read.go); shared fragments [`internal/store/ride_request_conflict_queries.go`](../../internal/store/ride_request_conflict_queries.go); adapter `cmd/telemetry-server/ride_request_adapters.go`; route **and the `store.MaxBookedWindowRange` injection** `cmd/telemetry-server/wiring.go`. Plan coverage: `internal/store/ride_request_window_index_plan_test.go` (Index Cond for both statements, against a real Postgres). The endpoint's tier row lives in the canonical per-vehicle access matrix, `TestEndpointGrantMatrix` in `internal/telemetry/vehicle_share_access_test.go`, immediately beside `POST /api/ride-requests` — the two rows must match cell for cell.

> **Filename footnote, because it cost a debugging cycle.** The store file is `..._booked_windows_read.go`, not `..._booked_windows.go`. Go strips a trailing `_test` and then reads the final `_windows` segment as the **GOOS build constraint `windows`**, so the original filename excluded the whole file from a darwin/linux build — silently, with no error, presenting only as a repo method that did not exist. The same trap applies to the test file. Do not rename either back.

---

### 7.23 `POST /api/tesla/vehicles/{vehicleId}/complete-setup` (zero-touch setup completion — MYR-505)

Owner-authenticated "finish setting this car up now" — the action half of the
`setupState` §7.0/§7.1 read surfaces.

**Why this endpoint has to exist.** After [MYR-489](https://linear.app/myrobotaxi/issue/MYR-489)
the server can heal a car that was linked but never streamed, and after
[MYR-491](https://linear.app/myrobotaxi/issue/MYR-491) it can say on the wire
what a car is waiting for. What it could not do was **start**. The heal fired on
a background reconciler pass — up to 45 minutes at the time this endpoint was
written, and since [MYR-529](https://linear.app/myrobotaxi/issue/MYR-529) a few
minutes for a car whose key is proven paired — or on a *signed vehicle command
that applied*, so in practice a new owner's onboarding finished when somebody
told them to tap **Lock**. A lock tap is not a setup step; it was a trigger
wearing a control's clothes, and it is exactly where a live beta onboarding
stalled. The client, meanwhile, knows the precise moment: it handed the owner
off to the Tesla app for the one approval Tesla requires a human for, and gets
`scenePhase` foreground back when they return. This endpoint is how that moment
becomes the trigger.

**Enablement.** The route is **ALWAYS mounted**. Both Tesla probes — `wake_up`
and `fleet_status` — are UNSIGNED authenticated reads against the direct Fleet
API and need no tesla-http-proxy; only the config push does. On a deployment
with no signing proxy the endpoint therefore still reports `awaiting_virtual_key`
/ `streaming` / `token_failed` correctly and returns `503 service_unavailable`
on the one path that needs the signer. A `404` would tell an SDK the server is
too old to complete setup, which would be false.

**Request.**

```
POST /api/tesla/vehicles/{vehicleId}/complete-setup
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
```

`{vehicleId}` is the Prisma cuid (the same key as §7.12 / §7.14 / §7.15 — NOT a
VIN). There is **no request body.**

**Behavior / sequence:**

1. Validate the bearer → `userId`. Missing/invalid → `401 auth_failed`. Missing
   `{vehicleId}` → `400 invalid_request`. Non-`POST` → `405 invalid_request`.
2. Resolve `vehicleId` → owner. Unknown vehicle → `404 not_found`
   (indistinguishable from ownership-filtered — never leaks existence); real
   mismatch → `403 vehicle_not_owned`. **Identical semantics to §7.12 / §7.14 /
   §7.15**, and neither touches Tesla.
3. **Per-vehicle cooldown**, keyed by VIN: one Tesla-hitting completion per
   **60 s**. Over the limit → `429 rate_limited` with `Retry-After: 60`.
   Consumed BEFORE the sequence and **not refunded on failure** (§7.15's
   argument), so a client retrying an unreachable car backs off instead of
   re-waking it every second.
4. **Streaming short-circuit.** If the car is already streaming — the same
   two-condition test §7.1's derivation uses, a non-default `status` AND a
   `lastUpdated` inside 30 minutes — respond `200` with
   `setupState.state: "streaming"` and **make no Tesla call at all**. This is
   what makes the common repeat (a client calling again after success) free.
5. **Wake** (`POST /api/1/vehicles/{vin}/wake_up`, unsigned, direct Fleet API).
   `wake_up` is exempt from Tesla's command signing, which is essential here:
   at this instant the virtual key may not exist. A successful call means Tesla
   **accepted** the wake, not that the car is online — for a sleeping car the
   response carries the pre-wake state — so the returned state is never used as
   a gate. A wake failure is carried, not fatal (see step 7).
6. **Pairing probe** (`POST /api/1/vehicles/fleet_status`, unsigned, direct
   Fleet API), bounded at **~30 s / at most 5 probes**, exiting the instant
   Tesla reports the key enrolled. `key_paired_vins` membership is read
   POSITIVELY: a VIN in neither partition reads as unpaired.
   * **Unpaired** → `200`, `setupState.state: "awaiting_virtual_key"`. Nothing
     is pushed. The client surfaces the retry proactively.
   * **Tesla never answered** (every probe failed) → `502 command_failed`.
     Pairing is UNKNOWN, and a fabricated state is worse than no answer.
7. **Paired but the wake was refused** → `503 vehicle_asleep`. No config is
   pushed at a car we could not reach.
8. **Paired.** Re-read the schedule row, then: if this pairing epoch's forced
   re-push is already spent, respond `200 configuring` and push nothing;
   otherwise reset the vehicle's attempt schedule (the SAME write an applied
   signed command performs, debounce included) and run
   [MYR-489](https://linear.app/myrobotaxi/issue/MYR-489)'s **forced re-push**
   — delete-then-create through the signing proxy — unforked. Applied →
   `200 configuring`. Failed → `502 command_failed`; the reconciler is already
   scheduled to repair it.

**`streaming` is a fourth `setupState` member, and it exists ONLY here.** §7.0
and §7.1 keep exactly the three members MYR-491 contracted, and their `null`
still means *no claim*. That is right for a passive read, where `null` covers a
healthy car, an unexamined car and an expired claim alike and the server cannot
tell which. It is exactly wrong as the answer to a **request**: a client that
just asked to finish setting up a car and receives "no claim" has learned
nothing, and the moment the narration can say *"Live!"* is the entire point of
this endpoint. Here the server does know, because it looked. So the action
surface returns a positive member and the read surfaces do not — one vocabulary,
one object shape, a superset on the surface that has earned the claim. This is
the additive widening §7.1's own contract anticipates ("new members may be added
in a minor version; consumers MUST treat an unrecognised value as *setup is
incomplete, reason unknown* and MUST NOT fail the decode"), so a client that
somehow met `streaming` on a read surface degrades safely. **The server never
emits `streaming` on §7.0 or §7.1.**

**"Asleep" and "not paired yet" are DIFFERENT ANSWERS and are never conflated.**
This is the load-bearing honesty property, and it is affordable only because
`fleet_status` is a **Tesla-side enrollment record, not a car query**: it does
not wake, dial or otherwise depend on the vehicle being reachable. So an expired
wake budget never degrades into `awaiting_virtual_key` — which would send an
owner back to the Tesla app to re-pair a key that is already fine — and a car
Tesla says is unpaired reports `awaiting_virtual_key` whether or not it woke.
Consequently **every failure mode here is an HTTP error, never a `setupState`**:
no error response carries one, and a client must render a retry affordance from
the code rather than a setup card.

**Idempotence means bounded effect on a REAL CAR, not identical bytes.** Three
independent mechanisms, each closing a different door. (1) The streaming
short-circuit makes the post-success repeat free. (2) The cooldown is **longer
than the probe budget**, which makes it a concurrency bound as well as a rate
limit — two requests for one car cannot overlap however they are timed, so
twenty simultaneous calls produce exactly one wake. (3) The
**pairing-epoch budget** — MYR-489's, shared verbatim rather than restated —
means the delete-then-create reaches the car at most once per epoch *even across
cooldowns*. The epoch is checked BEFORE the schedule reset, because the reset
stamps the epoch and would otherwise re-arm the very budget being tested, and it
is read from a row fetched AFTER the probe, so a reconciler pass that escalated
the same car during those 30 seconds is detected instead of double-spent. Worst
case for a client stuck in a retry loop: **one wake per minute and one forced
push per pairing epoch, per vehicle.**

**No new schedule write on the unpaired path, deliberately.** A probe that finds
the key still unenrolled records nothing. Recording it as an attempt would
increment `attempt_count`, so a client calling repeatedly would drive the
reconciler's exponential backoff to its ceiling and *bury* the very car it was
trying to help — the precise failure MYR-489 exists to undo. The reconciler owns
the schedule; this endpoint reads it and, on success only, resets it.

**Cooldown is in-memory and per-process**, resetting on restart and unshared
across replicas (effective limit up to N completions/min/vehicle under N
replicas). Same trade as §7.15: the backstops that actually protect the car are
the epoch budget and the bounded probe count, and a durable counter would cost a
DB round trip on every tap.

**Response `200`** (`application/json`), contract
[`schemas/vehicle-setup-completion.schema.json`](schemas/vehicle-setup-completion.schema.json):

```json
{
  "vehicleId": "clx9v0000000abcd",
  "setupState": {
    "state": "configuring",
    "since": "2026-08-09T04:00:00Z"
  }
}
```

| Field | Type | Classification | Notes |
|-------|------|----------------|-------|
| `vehicleId` | `string` | P0 | The Prisma cuid the request addressed, echoed so a client fanning several completions can attribute the answer. |
| `setupState` | `object` | P0 | **NEVER `null` on this surface** — unlike §7.0/§7.1, where `null` means no claim, a `200` here always carries a state, because the request asked a question the server just answered. Same two members and same semantics as `VehicleState.setupState`. |
| `setupState.state` | `string` enum | P0 | `streaming` — the car is sending live telemetry; setup is COMPLETE and nothing further is required. **This member is exclusive to §7.23.** `configuring` — the key is confirmed and the config has been pushed (by this call or earlier in the same pairing epoch); no owner action, the client narrates progress. `awaiting_virtual_key` — Tesla reports the virtual key is not enrolled; THE OWNER MUST ACT, and the client should offer the pairing retry proactively rather than waiting for them to find a dead button. `token_failed` — the owner's Tesla authorization can no longer be resolved or refreshed; THE OWNER MUST ACT (reconnect Tesla). |
| `setupState.since` | `string` (RFC 3339, UTC) | P0 | When the evidence behind `state` was recorded: the last live frame for `streaming`, the push (or the epoch it belongs to) for `configuring`, this read for `awaiting_virtual_key` / `token_failed`. Never zero and never in the future — the server floors it at the read instant. Copy data, not a re-derivation input. |

**Errors:**

| HTTP | `error.code` | When |
|------|--------------|------|
| 400 | `invalid_request` | Missing `{vehicleId}`. |
| 401 | `auth_failed` | Missing/malformed/invalid bearer. **Note the asymmetry with §7.15:** an owner whose *Tesla* token is unusable is NOT an auth failure here — it is `200` with `setupState.state: "token_failed"`, because that is a setup condition the card already has copy for. |
| 403 | `vehicle_not_owned` | Caller does not own the vehicle (matches §7.12 / §7.14 / §7.15). |
| 404 | `not_found` | Unknown vehicle, or ownership-filtered — intentionally indistinguishable. |
| 405 | `invalid_request` | Non-`POST` method. |
| 429 | `rate_limited` | A completion for this vehicle is already running, or ran within the last 60 s. Carries `Retry-After: 60`. Never returned for the streaming short-circuit's benefit — the limiter is consumed before it, so a car that is already live also spends a token. |
| 502 | `command_failed` | Tesla never answered the pairing probe within the budget (pairing UNKNOWN — **not** reported as unpaired), or the forced config re-push did not apply. Retryable; on the push arm the reconciler is already scheduled to repair the car. |
| 503 | `vehicle_asleep` | The virtual key IS paired but Tesla refused the wake, so no config was pushed at an unreachable car. Transient — the SDK backs off. |
| 503 | `service_unavailable` | This deployment has no signing proxy, so no config can be pushed at all (the `self-serve-onboarding.md` §5 safety guard). Pairing was still checked. |
| 500 | `internal_error` | Store-layer failure during the ownership lookup. Nothing was read from or written to Tesla. |

**No new error code, no new sub-code, and NO MIGRATION.** Every column read or
written already exists (`go_fleet_config_attempts`, migrations **0031** and
**0036**), and every code above is an existing member of the shared enum.

**Observability.** VINs are redacted to last-4 in every log line (§9, CG-DC-2).
The sequence emits one greppable event per outcome —
`setup_complete_already_streaming`, `setup_complete_token_failed`,
`setup_complete_wake_accepted`, `setup_complete_key_paired`,
`setup_complete_awaiting_virtual_key`, `setup_complete_wake_refused`,
`setup_complete_schedule_reset`, `setup_complete_push_already_spent`,
`setup_complete_configured` — alongside MYR-489's own
`fleet_config_forced_repush*` events, which the push step still emits because
the push step is still MYR-489's code.

#### Implementation

Sequence [`internal/telemetry/setup_completion.go`](../../internal/telemetry/setup_completion.go)
and [`setup_completion_configure.go`](../../internal/telemetry/setup_completion_configure.go);
tunables and the `streaming` member
[`setup_completion_config.go`](../../internal/telemetry/setup_completion_config.go);
handler [`setup_completion_handler.go`](../../internal/telemetry/setup_completion_handler.go);
Fleet API reads [`fleet_api_vehicle_wake.go`](../../internal/telemetry/fleet_api_vehicle_wake.go)
and [`fleet_api_fleet_status.go`](../../internal/telemetry/fleet_api_fleet_status.go);
the on-demand entry into MYR-489 and the shared epoch predicate
[`fleet_config_force_now.go`](../../internal/telemetry/fleet_config_force_now.go);
route [`cmd/telemetry-server/wiring_vehicle_complete_setup.go`](../../cmd/telemetry-server/wiring_vehicle_complete_setup.go).
The derivation helpers `isStreamingNow` and `setupStateAt` are MYR-491's, reused
so this surface and §7.1 cannot disagree about what "streaming" means.

**Not in `specs/rest.openapi.yaml`, deliberately.** That spec declares only the
core read and sharing subset and carries **no `/api/tesla/*` path at all** —
§7.12, §7.13, §7.14, §7.15, §7.16, §7.18 and §7.22 are likewise absent. This
endpoint's canonical machine-readable shape is
[`schemas/vehicle-setup-completion.schema.json`](schemas/vehicle-setup-completion.schema.json),
which the tagged conformance suite validates the `complete_setup` fixture
against.

---

### 7.24 `POST /api/ride-requests/join` (group-ride join — MYR-540)

> **Anchored:** FR-9.3 (ride-hailing), NFR-3.21 (ownership/authorization), NFR-3.23 (P1 location data). Contracts v0.35.0 `$defs.RideRequestJoinRequest`. Implemented by MYR-540.

The joiner-side redemption of a group ride's shared link — the other end of `RideRequest.shareUrl` (§7.8, "Group rides").

**It mirrors `POST /api/invites/redeem` (§7.5.5) rule for rule**, because it is the same act with a different subject: a code, held by whoever received the link, exchanged for access. Same body-not-path placement, same normalization, same per-user rate limit, same deliberate indistinguishability of every dead code. What differs is the RESPONSE, and the reason is stated in the contract itself: the sharing redeem needed its own response shape because `ShareInvite` is owner-facing and could never be handed to the redeemer, whereas **the ride a joiner joins IS the object they are now a party to**.

**Request**

```http
POST /api/ride-requests/join
Authorization: Bearer <token>
Content-Type: application/json

{ "code": "RBO246" }
```

`code` is the 6-character join code from the link's path segment, `^[A-Z0-9]{6}$`. It is a **separate, single-purpose code space** from `ShareInvite.code`: a join code is not an invite code, redeeming one at `POST /api/invites/redeem` answers `404`, and it grants membership of ONE ride rather than a standing grant on a vehicle. Server-minted at accept, never client-invented.

**THE CODE TRAVELS IN THE BODY, NOT THE PATH**, exactly as the invite redeem does, so it is never in a URL, a proxy log or a `Referer` header. The web landing shell at `/ride/{code}` verifies the link's signature statically against its compiled-in public key and then hands the bare code here; this endpoint is the only thing that decides whether it still means anything.

Clients normalize before sending exactly as the invite entry field does — upper-case, strip everything outside `[A-Z0-9]` — and **the server normalizes again**, so a code that survived a copy-paste with a stray space still redeems. A code that is still malformed after normalization is `400`.

**AUTHENTICATED, and that is a product decision before it is an implementation one** (client, 2026-08-13): app plus sign-in are REQUIRED, the joining account is the JWT `sub` and is never client-supplied, and live car positions never render on the open web.

**Response — `200 OK`: the full `RideRequest` the joiner now belongs to**, the same bare object `GET /api/ride-requests/{id}` returns, **with the caller already present in `members`**. That is the whole reason there is no separate response `$def`: the joiner goes straight to the rider tracking surface with no second round trip. The member list is read back inside the redemption's own transaction, so the body cannot race a concurrent join and omit the caller.

**ATOMIC AND IDEMPOTENT PER JOINER.** Re-submitting a code the same account already redeemed answers `200` with the same ride rather than a second membership row, so a retry after a dropped response is safe. The idempotence is the database's — `INSERT … ON CONFLICT (ride_id, user_id) DO NOTHING` — so two concurrent redemptions by one account resolve the same way as two sequential ones. Only the redemption that actually inserted a row publishes the internal `ride.member_joined` event.

**Errors**

| Status | `code` | When |
|--------|--------|------|
| `400` | `invalid_request` | Malformed body, an unknown key (`additionalProperties: false`), or a `code` that is not 6 × `[A-Z0-9]` after normalization. |
| `401` | `auth_failed` | Missing or invalid bearer token. |
| `404` | `not_found` | The code is **unknown**, **expired**, or belongs to a ride that has reached a **terminal status**. |
| `409` | `conflict` | The caller is already a party to the ride in another role — its requester, or the vehicle's owner. |
| `429` | `rate_limited` | More than 10 attempts in a rolling minute from this account. |
| `500` | `internal_error` | Lookup or write failure. |

**THE `404` IS NOT AN ORACLE, and this is the endpoint's central security property.** All three dead-code cases answer `404` with an **identical body**. An enumerating caller must not learn that a guessed code exists but belongs to a ride that has already finished, which would be a hit worth following up. The property holds *by construction* rather than by discipline: the resolving statement carries the expiry and the terminal-status refusal in its own `WHERE`, so it returns "no row" for all three and the handler could not tell them apart even if a later edit wanted it to.

**The `409` is deliberately NOT one of them.** A caller who is already the requester or the owner can see the ride in their own app, so there is nothing to conceal, and answering "not found" to them would read as a bug. It is also not a silent success: a requester in `members` would break the counting rule (`members.length + 1`).

**RATE-LIMITED, on §7.5.5's reasoning** — the code space is 36⁶ ≈ 2.2 billion and small enough to be worth enumerating. **10 attempts per rolling minute, per authenticated account**, counted on EVERY attempt whatever its outcome (counting only failures would let an attacker interleave one valid code with guesses). The budget is this endpoint's **own** — a separate counter from the invite redeem's, with the same numbers — because the two code spaces are separate and spending one endpoint's allowance must not close the other. Same single-instance caveat as §7.5.5: the counter is in-process, and fly.toml declares one machine.

**THE CODE IS P1 AND BEARER.** It is never logged, never echoed into an error message, and never rendered on any surface that is not already inside the ride.

**What joining grants** — see §7.8 "Group rides" for the full account: the rider view of this ride, viewer-tier access to its VEHICLE for the ride's lifetime, full collaboration on the trip, and the ride-lifecycle pushes. Not the cancel, not the start, and nothing at all once the ride ends.

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
| **DV-20** | **RESOLVED** | SDK-surface REST endpoints not yet mounted on the Go server | **RESOLVED — all four Go-server-owned §7 endpoints are mounted.** `GET /api/vehicles` (§7.0) landed in MYR-91 (2026-05-10); `GET /api/vehicles/{vehicleId}/snapshot` (§7.1) and `GET /api/vehicles/{vehicleId}/drives` (§7.2) landed in MYR-133 (2026-06-03); `GET /api/drives/{driveId}/route` (§7.4) landed in PR #260 (`DriveRouteHandler`, existing routePoints column + decryption); `GET /api/drives/{driveId}` (§7.3) landed in MYR-130 (2026-07-02) via `internal/telemetry/drive_detail_handler.go` backed by `DriveRepo.GetByID`. (§7.6 `DELETE /api/users/me` was outside DV-20's scope until **MYR-355 superseded the last half of DV-23** on 2026-07-30 and mounted it on the Go server; `cmd/telemetry-server/wiring_account_deletion.go`. The §7.5 endpoints WERE outside DV-20's scope for the same reason until **MYR-184 superseded that half of DV-23** and mounted all five on the Go server; the "terminal 404" statement that stood here is no longer true for §7.5.) | (Resolved.) All handlers enforce bearer auth + ownership + role-based `internal/mask` projection at the handler layer per §5.1; the shared-enum slice (REST-only `not_found` + `invalid_request` on `ErrorPayload.code`, plus `reauth_required` on `subCode`) was completed by MYR-98 on 2026-05-15. A route-surface regression test (`cmd/telemetry-server/wiring_routes_test.go`) guards against a contract route silently losing its mount. | FR-3.3, FR-3.4, NFR-3.5, §6, §7 | (Resolved — see MYR-91 / MYR-133 / PR #260 / MYR-130.) |
| **DV-21** | **New** | `service_unavailable` code reserved but not emitted | v1 does not emit `503 service_unavailable`. The code is reserved in this contract for forward-compat. | Server begins emitting `503 service_unavailable` during maintenance windows and graceful-shutdown states, with a `Retry-After` header. SDK error catalog already recognizes the code from day one. | NFR-3.10, §4.1.1 | `MYR-XX Emit 503 service_unavailable during graceful shutdown + maintenance` |
| **DV-22** | **New** | REST rate limit not enforced | No per-user REST rate limit is configured in [`internal/config/defaults.go`](../../internal/config/defaults.go) or wired through the server. The 120 req/min target in §4.1.2 is a PLANNED default, not an enforced value. | Add `WebSocketConfig.RestRateLimitPerMinutePerUser` (default 120) in `internal/config/defaults.go`. Implement a token-bucket rate limiter in the REST middleware keyed by `userId`. Breach returns `429 rate_limited` with a `Retry-After` header. Independent of `MaxConnectionsPerUser` (which governs concurrent WS sessions, not REST rps). | NFR-3.6, §4.1.2 | `MYR-XX Implement per-user REST rate limit (120 req/min default)` |
| **DV-23** | **SUPERSEDED (in full)** | Invite endpoints + `DELETE /api/users/me` handler location | Resolved 2026-05-08 (MYR-69) in favour of **Option 2**: the Next.js app owns `DELETE /api/users/me` and the §7.5 invite endpoints, against its Prisma-owned email-keyed `Invite` table, with the public API hostname proxying invite paths to it. | **SUPERSEDED 2026-07-29 (MYR-184) for the §7.5 half.** The premise expired: `react-frontend` is **deprecated**, so "serve it from the Next.js app" no longer names a running process, and the Prisma `Invite` table is **retired unused** — no invite row was ever written against it. Sharing is now **Go-owned end to end**: table `go_vehicle_shares` (migration 0020, no FKs to the sibling schema per CG-DL-9), five endpoints on the Go telemetry server (§7.5.1–§7.5.5), and the MYR-91 viewer merge that makes the `viewer` role real. The contract shape changed with it — **codes, not emails** (there is no email infrastructure and riders are Apple-native), so `email` / `senderId` / `sentDate` / `isOnline` are gone and `label` / `permission` / `code` / `expiresAt` take their place, per contracts v0.19.0 `vehicle-sharing.schema.json`. **The §7.6 half was SUPERSEDED in turn on 2026-07-30 by MYR-355**, for the same expired premise plus a second one: account deletion is an App Store review requirement for the native iOS client, and that client never reaches the Next.js app. Most of what a deletion must remove now lives in Go-owned tables (`go_ride_requests`, `go_vehicle_shares`, `go_push_devices`, `go_refresh_tokens`, `go_users`, `go_identity_apple`) that no Prisma cascade reaches, so the `$transaction` DV-23 assumed was the deletion is now only its last step. `DELETE /api/users/me` is served by the Go telemetry server, answers `204`, and is re-runnable rather than atomic (§7.6). **DV-23 is therefore superseded in full**; the only `/users/me` path still on the Next.js app is §7.7 export. Implementation follow-ups MYR-70 (Next.js invite handler) and MYR-73 (edge routing for invite paths) are **obsolete**; MYR-71 / MYR-72 are **obsolete for the deletion handler**. | FR-5.1, FR-5.2, FR-5.3, FR-10.1, FR-10.2, NFR-3.29, §7.5, §7.6 | (§7.5 superseded — see MYR-184. §7.6 superseded — see MYR-355.) |

### Divergence management rules

Same as [`websocket-protocol.md`](websocket-protocol.md) §10 divergence management rules (one-way door, closing rules, RESOLVED-with-implementation-pending, amendment divergences). DV-NN IDs are globally unique across both catalogues -- MYR-12 intentionally starts at DV-19 to avoid collision with the DV-01 through DV-18 IDs owned by the WebSocket contract.

---

## 11. Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-08-13 | **The §7.0 catalog gains its THIRD row source: vehicles of live group rides the caller has JOINED ([MYR-540](https://linear.app/myrobotaxi/issue/MYR-540) follow-up).** Without it a joiner's catalog omitted the ride's car — the app's watched-vehicle ladder resolves against the catalog, so the joiner's live map had no vehicle to watch even though the WS handshake would admit the subscription. Member rows are appended AFTER owner and shared rows as `role: "viewer"` projections with the ZERO grant (no ride capability), joined on `go_ride_members` under **character-for-character the WS access set's membership predicate** (non-terminal statuses only), and deduplicated: owner and share rows win over membership. Failure swallows to the strictly smaller set, like the shared merge. NO contracts change — the rows are ordinary `VehicleSummary` viewer rows. | Claude |
| 2026-08-13 | **Group rides: a ride other people can join, through a link the server mints at accept ([MYR-540](https://linear.app/myrobotaxi/issue/MYR-540), CLIENT-DIRECTED). Contracts v0.35.0.** Three additive optional fields on `RideRequest` — `groupRide` (absent = false, so every pre-540 row is correct unrewritten), `shareUrl` (the signed link) and `members` (`$defs.RideMember`, joiners only — the requester is `riderId` and is never duplicated in, which is what makes absence mean "no joiners") — plus one new endpoint, **`POST /api/ride-requests/join`** (§7.24), and one new create-body field. **MIGRATION 0040**: `group_ride` / `join_code` / `join_code_expires_at` on `go_ride_requests` (the invite link's storage idiom — code and expiry on the row the code grants access to) with a partial UNIQUE index over the code, plus `go_ride_members` (composite PK, `ON DELETE CASCADE`). **The link is minted AT ACCEPT, exactly once**, inside the accept's own guarded write — so a repeat mints nothing and the ride keeps the code it published — and only for a group ride. It reuses the MYR-368 signer verbatim (same key, same key id, same `k` triple) under the DOMAIN-SEPARATED payload `ride:{code}:{expUnix}`, so one compiled-in public key serves both link kinds and neither signature can be replayed as the other. `shareUrl` is emitted only while redeemable: accepted-or-later, inside the code's 7-day TTL, and terminal-plus-a-five-minute-linger — **the CODE dies at terminal with no grace at all** (the redeem predicate refuses it), the linger governs only whether the field is rendered, and it matches MYR-421's held-end window so there is one number. The join is **authenticated** (app + sign-in, client-decided), takes the code in the BODY, is **idempotent per joiner**, is rate-limited 10/min on its OWN budget, and answers an **indistinguishable `404`** for unknown / expired / terminal — by construction, since the resolving statement collapses all three. **ACL: the party set widens, two verbs do not.** A member gets the detail read, their own list, the trip PATCH (full collaboration, client-decided) and the §7.21 Live Activity registration (keyed `(ride, user)`, so the ticker fans out per registration unchanged); a member may **not** cancel (MYR-537 is unmoved — a member leaving is not a cancel, and there is no leave endpoint in v1) and may not start, both `403` rather than `404` because the caller is a party. Membership is a THIRD source of VEHICLE access after ownership and shares — ride-scoped, viewer-tier with the zero-value capability set, and ending at terminal status in the access-set query itself, so there is nothing to revoke; a live socket ages out on the 60s revalidation backstop rather than being severed, and a member who joins mid-session must reconnect. Ride-lifecycle pushes fan out to members in the RIDER's copy; joining itself notifies nobody. No WS frame for membership changes — clients refetch. Account deletion DELETES a member's rows. See §7.8 "Group rides" and §7.24. | Claude (Agent/go-engineer) |
| 2026-08-13 | **The car flashes its headlights three times when it arrives ([MYR-542](https://linear.app/myrobotaxi/issue/MYR-542), CLIENT-DECIDED).** **No contracts version change, no new field/endpoint/error/state, NO MIGRATION, nothing on the wire** — this is a gesture at the CAR, not a change to any client's contract. A new internal subscriber on MYR-538's `ride.waypoint_arrived` seam sends the signed `flash_lights` command ×3, ~1.5 s apart; **no horn**, client-decided, because a horn carries down a street and reads as aggression at any hour while headlights are addressed to the one person looking for the car. The seam's provenance is the whole safety argument: it is published ONLY by the auto-arrival detector, i.e. only for a car whose own telemetry showed it parked at the pickup, so the owner's manual `picked-up` tap — which does not publish it — can never flash a car from three miles away. Exactly once per (ride, waypoint) via an in-memory latch on top of the detector's own exactly-once publish; **at most three commands per waypoint** (a failed flash is not retried and abandons the remainder — a partial greeting reads as a fault where silence reads as a car that simply did not flash); and **every failure is logged only, never surfaced to any party**, because a missed courtesy is not actionable by rider or owner and the ride's status was settled before the event existed. Gated by `ARRIVAL_FLASH_ENABLED` (default true), deliberately separate from `AUTO_ARRIVAL_ENABLED`. See "Auto-arrival" under §7.8 `POST .../picked-up`. | Claude (Agent/go-engineer) |
| 2026-08-13 | **The server confirms the pickup itself when the car is observed at it ([MYR-538](https://linear.app/myrobotaxi/issue/MYR-538), CLIENT-DIRECTED).** Client: *"If car reaches destination you shouldn't require owner to mark as arrived we should be able to figure it out."* **No contracts version change, no migration, no wire change** — `accepted → arrived` simply gains a SECOND WRITER through the SAME dormancy-guarded `UpdateStatusFromDispatched` the owner's `picked-up` tap uses, so the two contend on one `UPDATE`, the database picks a winner, and the tap STAYS (unchanged endpoint, unchanged 409s, unchanged idempotent no-op). The rule, deliberately slow to fire: a ride `accepted` with leg-1 `dispatchStatus == "sent"` whose car is within ~80 m of the pickup, stopped, continuously for ~20 s — an interrupted dwell resets, an ambiguous vehicle is skipped, and an unrefreshable candidate set suspends detection rather than acting on it. The car's own `milesToArrival` is corroboration only, never a trigger (MYR-527: the dash can hold the wrong target). The winning write publishes the SAME `ride_status_changed` the tap does — rider push / Live Activity / WS fire identically — plus an internal-only `ride.waypoint_arrived` seam (ids + `"pickup"`, never broadcast) for MYR-542's light flash, because "the ride was picked up" and "the car is at the kerb" are different facts. Gated by `AUTO_ARRIVAL_ENABLED` (default true). See §7.8 "Auto-arrival" under `POST .../picked-up`. | Claude (Agent/go-engineer) |
| 2026-08-13 | **The pickup and drop-off are editable mid-ride, versioned, verified onto the dash, and everyone else on the ride hears about it ([MYR-541](https://linear.app/myrobotaxi/issue/MYR-541), CLIENT-DIRECTED).** Contracts **v0.33.0**: optional `tripVersion` on `RideRequest` (absence = 0), `RideRequestTripPatch`, optional `tripVersion` on the summary-only `ride_status_changed` frame (refetch signal, never places). Migration **0038** adds `go_ride_requests.trip_version` (IF NOT EXISTS). New `PATCH /api/ride-requests/{id}/trip` — see its section under §7.8 for the window ("pickup until boarded, drop-off until the ride ends"), the instant-application ACL, the versioned guarded write, the participants' pushes, and the dispatch half: an edited CURRENT-leg target re-shares through the MYR-526 sequencer and verifies per MYR-527, which is what makes a mid-ride destination change trustworthy. Internal seams: `ride.trip_changed` + the `TripEdit`/`TripVersion`/`PreviousStatus` fields on the status event (notifier suppresses the status copy for trip-edit publishes — no spurious "Your ride is confirmed"). | Claude (Agent/go-engineer) |
| 2026-08-13 | **The rider may cancel at any point of a live ride, and a committed owner hears about it ([MYR-537](https://linear.app/myrobotaxi/issue/MYR-537), CLIENT-DIRECTED: "allow rider to cancel ride at anytime even after it's started").** **No contracts version change, no new field/endpoint/error/state, NO MIGRATION.** The rider's `/cancel` allowed-from set widens `{requested, accepted}` → every live status, arbitrated by the same MYR-522 guarded stamped write. The transition matrix's `arrived` and `enroute` rows gain the rider's cancel; the OWNER's window is unchanged (dropped-off remains their `enroute` exit). NEW owner push — `«FirstName» cancelled the ride / No need to continue — the ride has ended.` — fired only when the pre-check status was `accepted`/`arrived`/`enroute` (a `requested` cancel stays silent; self-ride suppressed), routed off a new internal-only `PreviousStatus` on the status-changed event (best-effort under races, never on the WS frame). The push matters doubly because Tesla has no cancel-navigation API: the dash cannot be cleared remotely, so the owner being told IS the stand-down. **The account-deletion teardown keeps the pre-MYR-537 pair on purpose** (`requested`/`accepted`, now its own decoupled set): deletion is bookkeeping, and a car mid-trip finishes its ride. | Claude (Agent/go-engineer) |
| 2026-08-13 | **A trip may have STOPS between the pickup and the destination ([MYR-539](https://linear.app/myrobotaxi/issue/MYR-539), CLIENT-DIRECTED: exactly ONE pickup → N ordered stops → ONE destination).** Contracts **v0.34.0**: optional `RideRequest.stops` (`$defs.RideStop` — `place`, server-minted `id`, server-owned `status`), `$defs.RideStopPatch`, and `RideRequestTripPatch.stops` with REPLACE semantics. Migration **0039** adds the Go-owned `go_ride_stops` (encrypt-only coordinates like the endpoints, `(ride_id, position)` index, ON DELETE CASCADE so the MYR-447 sweep cannot strand P1 location data). The list is **optional and additive** — omitted when empty, so every pre-MYR-539 ride is byte-identical, and absence means "no stops", never "unknown". Editing rides the EXISTING `PATCH .../trip`: same body, same single `tripVersion` for the whole trip shape, same instant-apply ACL, the drop-off's window — and inside that write's transaction, so a stop refusal rolls the endpoints back with it. Two refusal classes: **409** for a replacement that removes or reorders a stop already `current`/`completed` (the road behind the car cannot be rewritten — refetch and re-apply), **400** for an id the ride does not have. **Leg advance:** the MYR-538 detector now watches `enroute` rides against their CURRENT stop and publishes `ride.waypoint_arrived` with `"stop:{id}"` / `"destination"`; a stop arrival marks it completed, promotes the next, and — only for the writer that won — re-shares the car's nav to the new target through the MYR-526 sequencer and MYR-527 verifier. A stop advance is lifecycle, NOT an edit: it does **not** bump `tripVersion`. **The destination advances nothing** — `dropped-off` stays the owner's tap; that event exists for MYR-542's flash. The rider's Start now targets the first stop. **No lifecycle-matrix change, no new status, no new error code.** See §7.8 "Multi-stop trips". | Claude (Agent/go-engineer) |
| 2026-08-12 | **A nav share is now verified against the car's own reporting, re-shared once, and escalated to the owner when it never takes ([MYR-527](https://linear.app/myrobotaxi/issue/MYR-527)).** **No contracts version change, no new field/endpoint/error/state, NO MIGRATION.** `sent` always meant "Tesla accepted the share", never "the car applied it", and the gap's first live casualty (2026-08-12, ride `ccc29b61…`) put a rider on a 3-hour trip whose dash still navigated to the pickup while the app — reading the car's nav by design (MYR-485) — showed "4 min away". The dispatcher now watches `DestinationLocation` converge on the shared target (~250 m, 90 s), re-shares once through the sequencer (`reacquire` admits the leg's own order; lower orders still refused), and publishes internal `ride.nav_unapplied` on a second miss → owner push "«Vehicle» may not have the route / Check the dash…". Diagnosis correction recorded on the issue: `destinationName` "NULL fleet-wide" was the RETIRED plaintext column — the encrypted destination pair streams and persists today, which is what makes the close-loop buildable with no fleet-config work. See "Nav-apply verification" under §7.8. | Claude (Agent/go-engineer) |
| 2026-08-12 | **A reserved car leaves early enough to ARRIVE on time, and both parties hear about it at the leave-time ([MYR-535](https://linear.app/myrobotaxi/issue/MYR-535), CLIENT-DIRECTED).** **No contracts version change, no new field, no new endpoint, no new error code, no new state, NO MIGRATION** — the due-sweep SQL is byte-identical and only its bind value widened. `scheduledFor` was always the pickup ARRIVAL instant and the sweeper treated it as the leave-time (measured: scheduled `21:00:00Z`, dispatched `21:00:08Z`; the client, riding his own reservation: *"the owner should leave before 4PM"*). The sweeper's SELECT bound is now the **dispatch horizon** (`now + 30 min`) and a per-row **leave-time gate** in Go dispatches once `now >= scheduledFor − lead`, where `lead = clamp(drive-time estimate from the car's current position + 3 min, 5 min, 30 min)` — computed in Go because both coordinate pairs are encrypted at rest. Degradations (no fix, `(0,0)` sentinel, unreadable position) fall to the FLOOR lead rather than holding, and an at-or-past-due candidate never reads the position at all, so a position outage can never hold a dispatch past its pickup instant. **Dormancy needed no change**: MYR-376's dispatch arm ("a dispatched reservation is live whatever the clock says") already makes an early-dispatched reservation pickup-able, which is now correct rather than latent. `ride.due` fires at the leave-time, so `dueAt` ordinarily PRECEDES `scheduledFor`; the rider's push body becomes "Your scheduled pickup is coming up." and the OWNER gains a push at the same seam — `«FirstName»'s pickup is coming up` / `Your car has the route — time to head out.` (first name resolved at delivery time so the dispatch event stays summary-only; suppressed on a self-ride, where the rider push alone survives, exactly as before). See "Reservation-time dispatch" §7.8 and the server-side sends table. | Claude (Agent/go-engineer) |
| 2026-08-10 | **A car whose virtual key is proven paired is reconciled in minutes, not quarter-hours ([MYR-529](https://linear.app/myrobotaxi/issue/MYR-529)).** **No contracts version change, no new field, no new endpoint, no new error code, no new state, NO MIGRATION.** §7.23's request and response are byte-identical; this entry records a BEHAVIOURAL change to what the server does after answering, because the timing it changes is the timing a client narrates. **The live case.** Joey Verrone linked at 01:22:08Z, his virtual key was proven paired at 01:23:10Z, [MYR-489](https://linear.app/myrobotaxi/issue/MYR-489)'s in-band signal reset the backoff and reconciled the car on the spot, Tesla answered `synced` — and the escalation was then declined, correctly, as a FIRST synced-but-quiet observation, parking the row at `next_attempt_at` +15 minutes with `forced_repush_at` NULL while its owner drove with no telemetry. Four other cars streamed to the second, so ingest was never in question. **Nothing was broken; a steady-state cadence was applied to the one minute of a car's life when a human is watching.** Three changes, all server-side. **1 · A fresh pairing epoch gets a HOT schedule** — examinations at ~1/3/8 minutes, then decaying into MYR-448's ordinary 15m/30m/1h curve — and the forced delete-and-recreate may satisfy its grace gate via that ladder as well as via the wall clock, so it lands on the second hot pass (~3 minutes past pairing) instead of never. The per-epoch force budget is **unchanged and still shared verbatim** with §7.23: the hot window changes WHEN the one spend happens, never how many there are. **2 · A live pairing epoch is exempt from the 30-minute staleness rule** in the reconciler's candidate query. That rule is right for the steady state and wrong for the only window in which pairing happens: Joey's `"Vehicle"` row kept being written by provisioning and web-sync traffic (01:41:33Z), so the silence it demands never accumulated and the schedule his row carried was unreadable by every pass. The exemption waives staleness ONLY, never the schedule, and expires with the epoch. **3 · The loop's TICK is separated from the backoff BASE** (1 minute vs 15), so a row that comes due is examined promptly rather than up to a full interval later. **The cost is bounded by a check that runs before any Tesla call:** "is this car streaming?" is answered from the listing row itself — `"Vehicle".status` plus `"lastUpdated"`, both already selected, both already P0 and already wire-exposed — so a car that came up on its own cancels its remaining hot passes for **zero** upstream traffic. **No new column is read, written, or projected, and `internal/mask` needs no entry.** §7.23 additionally hands Tesla's paired answer to the reconciler on the three paired branches it cannot finish itself (budget already spent, no signing proxy, wake refused), and a link-time config push that APPLIED is now recognised as the pairing evidence it always was; every such signal is deduped to one examination per vehicle per minute. | Claude (go-engineer) |
| 2026-08-09 | **A slow first consent no longer expires the link, and every link door now seeds the setup schedule ([MYR-517](https://linear.app/myrobotaxi/issue/MYR-517)).** **No contracts version change, no new field, no new endpoint, no new error code, NO MIGRATION.** Two server-side defects observed live in one onboarding funnel (Spencer White, 2026-08-09 18:07Z). **1 · The expiry is OURS, and it was ten minutes.** The owner authenticated, worked through Tesla's scope screens, and §7.11.2 answered `invalid_state`, forcing a full re-authentication. **Tesla's authorization `code` cannot be the constraint** — Tesla mints it at the END of consent and the callback exchanges it within one round-trip, so time on the consent screens is not charged against it; every second of it is charged against the `state` session created at `/start`. The TTL is now **30 minutes**, and the per-user in-flight cap goes **1 → 3** (oldest evicted) so a duplicate `/start` can no longer strand the browser flow the owner is standing in — a dead end that produced the identical "expired" message with no relation to the clock. §7.11.1 and §7.11.2 updated. **The client-facing surface is unchanged where it matters:** `reason=invalid_state` keeps its exact value and remains the field to branch on. An **additive** `detail` query param (`session_expired` \| `session_unknown`) is emitted alongside it so a client can, when it chooses to, treat a timed-out state as a silent re-entry into `/start` rather than an error the owner must read and act on. Ignoring `detail` is safe and is what every existing client does. Server-side, the expiry is its own greppable Info event (`tesla_link_session_expired`). **2 · The `go_fleet_config_attempts` seed fired on ONE branch and missed the car it was written for.** [MYR-491](https://linear.app/myrobotaxi/issue/MYR-491) wrote the schedule row only when Tesla answered the link-time push with `missing_key`; every other terminal outcome — the push APPLIED, the push failed, a non-key skip, no signing proxy — left the car with **no row at all**, which is the state prod shows for the reporting vehicle to this day. The seed is now **unconditional and idempotent on every door** (fresh link, re-link over an already-provisioned car, deliberate re-add), with the OUTCOME carrying what was actually observed: `awaiting_virtual_key`, `push_failed`, or `''` (no claim). **Defense in depth:** the applied-signed-command pairing write (`ResetFleetConfigScheduleOnPairing`) becomes an UPSERT, so the strongest evidence the system ever receives is recorded instead of dropped — and it does NOT turn a freshly created row into a Tesla round-trip, because a car we have never recorded a failed attempt against is not evidence of a broken car. That upsert also fixes a latch on §7.23: with no row the schedule reset no-opped, so the forced re-push stamped `forced_repush_at` against a NULL `signed_command_at`, which reads back as `epochForceSpent` **forever** — the endpoint could fire once per car and then answered `configuring` from a spend that may have accomplished nothing. **The wire is unmoved:** a seed row carries `last_outcome = ''`, which derives to `null` exactly as no row does, and is scheduling-identical to no row (`attempt_count 0`, `next_attempt_at now`). The honesty guarantee is restated accordingly in §7.1 `setupState`, [`vehicle-state-schema.md`](vehicle-state-schema.md) §7.1 rule 1 and [`data-classification.md`](data-classification.md) §1.19: **absence still yields no claim, and presence alone never licenses a card.** | Claude (go-engineer) |
| 2026-08-09 | **The rider picker can rank a shared car it is not watching — `VehicleSummary.location` on §7.0 ([MYR-515](https://linear.app/myrobotaxi/issue/MYR-515)).** Contracts **v0.31.0**, shipped in the same bump as MYR-507 below. **One new optional field**, **NO MIGRATION**. **The gap.** The picker renders a per-row pickup ETA and had no input for any row but one: a client holds exactly ONE coordinate — the watched car's, off the single-vehicle WebSocket subscription — and a viewer's per-row `/snapshot` is **403 by design** (MYR-432/449). §7.0 carried no position at all, so the client fill (merged and ready) had nothing to consume. **The shape:** a NULLABLE OBJECT `{ latitude, longitude }`, not two nullable scalars. The pair is an ATOMIC GROUP (§5.4) and splitting one is itself a contract violation, so nesting makes "both or neither" structural rather than conventional — the half-state two flat fields would permit has no representation. Field names follow `VehicleState` and `SavedPlace` (the account/vehicle family), not RidePlace's `lat`/`lng`. **`null` is the only "no fix" spelling here, and that is a deliberate DIVERGENCE from §7.1:** `VehicleState`'s columns are `NOT NULL` so that surface uses the `(0,0)` sentinel (`vehicle-state-schema.md` §2.3); this one can express absence, so the server collapses an exact `(0,0)` to `null` and **a consumer will never receive `0,0` here**. Do not port a Gulf-of-Guinea guard to this field — a picker handed the sentinel does not fail loudly, it confidently renders an ETA to a point in the Atlantic. Asserted in that direction. **Classification — P1, both roles, and the first P1 field on this resource besides `licensePlate`.** The argument is deliberately the narrow one, not "riders need location": a viewer **already receives this exact pair at this exact precision on the streaming path**, because `vehicleStateViewerFields` retains the whole Speed/GPS atomic group ("the entire point of a shared live map") and [MYR-435](https://linear.app/myrobotaxi/issue/MYR-435)'s narrowing deliberately left it. Same value, different transport, same entitled party; the row exists only because a live accepted grant produced it, so a suspended or revoked grant yields no row and no coordinate. **The honest caveat:** the catalog hands over N positions at once where the socket hands out one per subscription — an **aggregation** difference, not an entitlement one, since the grant is per-vehicle and a viewer could subscribe to each car in turn. **Coarsening was considered and rejected:** the platform has no precision convention, and rounding this row while the socket serves the exact value to the same viewer protects nothing while degrading the short-hop ETA the field exists for. **If a future policy coarsens shared positions it MUST coarsen the stream and this row together**, or the weaker surface silently sets the real privacy bound. **Cost:** the lean §7.0 projection now selects the `latitudeEnc`/`longitudeEnc` pair and decrypts it per row — the one deliberate exception to MYR-122's "no GPS columns", taken because the invariant's own wording is "MUST NOT SELECT columns the response body doesn't emit" and this one IS emitted. The destination / origin pairs and the `navRouteCoordinates` blob stay out; those were the expensive part. **Tolerance:** absent key = a server predating v0.31.0, read exactly like `null`. Not streamed under this name — a consumer actively watching a car should prefer the socket's coordinate, since this one is only as fresh as the last write to the vehicle row. | Claude |
| 2026-08-09 | **A shared car can say what it is — `VehicleSummary.trimLabel` on §7.0, and `model`/`year` that are actually populated ([MYR-507](https://linear.app/myrobotaxi/issue/MYR-507)).** Contracts **v0.31.0**. **ONE new optional field**, no new endpoint, no new error code, **NO MIGRATION** (every column already exists). **The report.** A rider was shown a shared car whose lock-screen Live Activity subline read a bare `UltraRed` and whose review-sheet row rendered the generic make `Tesla`. The client's descriptor ladder was working correctly — it had nothing to work with. **Two independent causes, and the first was not the wire.** (1) `Vehicle.model` and `Vehicle.year` are `required` on §7.0 and have been since v1, but for a car the **Go server** provisioned they held `''` and `0` **permanently**: `queryUpsertOwnedVehicle` seeded the placeholders under a comment promising "the web sync / streaming pipeline fills real values later", and no such writer has ever existed on either side. Cars linked through the Prisma web flow *did* carry them, which is why the fleet looked half-correct and the gap survived. Production at the time of the report: the reporting car held `model: ""`, `year: 0`, `color: "UltraRed"`. **The fix is provenance, not shape** — both are now derived from the VIN (position 4 = model line, position 10 = the ISO 3779 year code) at **link time**, and backfilled for existing rows by the **fifth** `data-lifecycle.md` §1.4 carve-out (`store.VehicleRepo.FillVehicleIdentity`) on the same non-waking read that carries the MYR-320 colour write. **Fill-if-empty, never overwrite:** Prisma records a richer model for some cars (`Model 3 Performance`) than a VIN position can encode (`Model 3`), so a blind overwrite would trade this bug for a subtler one. **The VIN rather than Tesla, deliberately:** Tesla's Fleet API carries **no model year at any path**, so the VIN is the only source of one in principle — and once the year comes from the VIN the model comes from the same 17 characters, so there is one derivation over one input. It also needs no token, no awake car and no network call, which is the whole point: the reporting car had been in the fleet for hours without ever producing the connectivity edge an API-sourced fix would have waited on. An unrecognised VIN code writes nothing — the server does not guess, because a wrong model on a ride card is worse than a blank one. (2) **The trim was genuinely absent from the wire.** `trimLabel` existed only on §7.1, withheld from the catalog by MYR-320 as "detail-sheet only" on a byte-cost argument that never weighed its one real consumer: **a viewer never fetches a `/snapshot`**, so for a rider the catalog row is the only surface on which a car can identify itself. It is now on §7.0 as an **optional, nullable** field, in **BOTH** role allow-lists — the same column, read not re-derived, so the catalog and the snapshot cannot name one car two different things, and it rides the `go_vehicle_control_state` join §7.0 already pays for. **Classification:** P0, both roles, and the argument is that it is an equipment fact of exactly the tier of the identity siblings `model`/`year`/`color` that have been in both allow-lists since v1 — all four legible through a windshield from the kerb, the badge on the boot lid *being* the trim. Nothing here for masking to protect. **A deliberate split from its MYR-320 sibling:** `fsdVersion` stays detail-sheet-only (a software designation with no catalog consumer, and no descriptor degrades without it) and the raw badge code `trim` (`p74d`) stays off both catalog roles as not display-safe — pinned by `TestMYR320DetailFieldsSplitOnTheVehiclesList` so a later change cannot carry either along by analogy. **Tolerance:** an absent key means a server predating v0.31.0 and reads exactly like `null`; `null` is **not** the empty string — it means "no trim known", and a descriptor MUST drop the fragment rather than emit a stray separator, exactly as it must for `model: ""` and `year: 0`. **What the client gets:** `year` + `model` + `trimLabel` + `color` on one row, which is the target descriptor verbatim — `2026 Model X Plaid · Ultra Red`. Colour-token formatting (`UltraRed` → `Ultra Red`) stays client-side: the value is Tesla's own string and the server does not rewrite it. | Claude |
| 2026-08-09 | **Returning from the Tesla app finishes setup by itself — new §7.23 `POST /api/tesla/vehicles/{vehicleId}/complete-setup` ([MYR-505](https://linear.app/myrobotaxi/issue/MYR-505)).** Contracts **v0.25.0**. One new owner-only endpoint, one new response schema, **NO MIGRATION** (`go_fleet_config_attempts` **0031** + **0036** already carry every column), **no new error code and no new sub-code**. **The gap it closes.** [MYR-489](https://linear.app/myrobotaxi/issue/MYR-489) taught the server to heal a linked-but-never-streaming car and [MYR-491](https://linear.app/myrobotaxi/issue/MYR-491) taught it to say what a car is waiting for, but neither could START: the heal fires on a reconciler pass (up to 45 minutes) or on a signed command that APPLIED, so a live beta onboarding finished only when somebody told the owner to tap **Lock**. A lock tap is not a setup step. The client knows the exact moment — it handed the owner off for Tesla's one required approval and gets `scenePhase` foreground back — and this makes that moment the trigger. **The sequence:** unsigned `wake_up` → bounded `fleet_status` probe (~30 s, ≤5 probes) → on paired, reset the attempt schedule and run MYR-489's forced re-push (delete-then-create through the signing proxy) **unforked, as an entry point rather than a second implementation** → answer with the resulting `setupState`. **`streaming` is a FOURTH `setupState` member and it exists ONLY on this surface** — §7.0/§7.1 keep exactly MYR-491's three, and their `null` still means *no claim*, which is right for a passive read and exactly wrong as the answer to a request: a client that asked to finish setting up a car and got "no claim" has learned nothing. One vocabulary, one object shape, a superset on the surface that has earned the claim; it is the additive widening §7.1's own contract anticipates, so a client meeting it elsewhere degrades safely. **The honesty property that shapes everything:** *asleep* and *not paired yet* are DIFFERENT ANSWERS and are never conflated, which is affordable only because `fleet_status` is a Tesla-side enrollment record rather than a car query — a vehicle that never wakes still yields a TRUE pairing answer. So an expired wake reports `503 vehicle_asleep` instead of sending an owner to re-pair a key that is already fine; a probe Tesla never answered is `502 command_failed` rather than a fabricated `awaiting_virtual_key`; and a push that failed is `502`, never `configuring`. **Every failure mode is an HTTP error and NO error response carries a `setupState`.** **Three independent bounds on what reaches a real car:** the streaming short-circuit (post-success repeats cost zero Tesla calls), a per-VIN 60 s cooldown that is deliberately LONGER than the probe budget so it doubles as a concurrency bound (twenty simultaneous calls produce exactly one wake), and MYR-489's **pairing-epoch budget shared verbatim** so the delete-then-create lands at most once per epoch even across cooldowns — checked BEFORE the schedule reset (which stamps the epoch and would re-arm it) and read from a row fetched AFTER the probe, so a reconciler pass racing us mid-request is detected rather than double-spent. Worst case for a stuck client: one wake per minute, one forced push per epoch, per vehicle. **The unpaired path writes NOTHING to the schedule, deliberately** — recording it would increment `attempt_count` and let a looping client drive the reconciler's backoff to its ceiling, burying the very car it was trying to help. **Reader/writer split preserved:** `wake_up` and `fleet_status` are unsigned reads on the DIRECT Fleet API; the config push is the only proxy call, and it happens inside MYR-489's code. The route is always mounted — with no signing proxy, pairing is still CHECKED and only the push answers `503 service_unavailable`. §1 TOC (entry 23), §6 catalog and §7.23 added; new [`schemas/vehicle-setup-completion.schema.json`](schemas/vehicle-setup-completion.schema.json), a `complete_setup` conformance fixture wired into the tagged suite, and a `data-classification.md` §1.19 wire-projection note recording the THIRD resource that projects the same four columns (no new column, no role dimension — the endpoint is owner-only and self-scoped, so `internal/mask` needs no entry). **`specs/rest.openapi.yaml` is deliberately NOT touched**: that spec covers only the core read/sharing subset and carries no `/api/tesla/*` path at all — not §7.12, §7.13, §7.14, §7.15, §7.16, §7.18 or §7.22 — so adding one here would be the inconsistency, not the omission. MYR-491 touched it only because it added a field to `VehicleState`/`VehicleSummary`, which the spec does declare. | Claude (go-engineer) |
| 2026-08-09 | **A car that has not come up yet says why — `setupState` on `VehicleState` (§7.1) and `VehicleSummary` (§7.0) ([MYR-491](https://linear.app/myrobotaxi/issue/MYR-491)).** Contracts **v0.24.0**. Adds ONE optional, nullable P0 object to both read surfaces. **No new endpoint, no new error code, and NO MIGRATION** — every column it reads already exists ([MYR-489](https://linear.app/myrobotaxi/issue/MYR-489)'s migration **0036** on `go_fleet_config_attempts`, plus **0031**); this is the wire half that PR #385 deliberately left undone. **The gap it closes** is [MYR-503](https://linear.app/myrobotaxi/issue/MYR-503): a tester onboarded, met a card with no location and no charge, tapped Lock and got a dead button, because nothing on the wire said his virtual key was never paired. The client's direction was that pairing guidance must NOT hang off a failed control — so this is a vehicle-level state, rendered before any command is attempted, naming the ONE thing left to do: `awaiting_virtual_key` (owner pairs the key), `configuring` (nothing to do; the server is waiting on the car), `token_failed` (owner reconnects Tesla), or `null` for no claim. **Emitted on BOTH surfaces and in BOTH role masks.** The snapshot feeds the owner's card; the catalog row feeds the rider-side picker, which reads list rows and today renders a never-streamed shared car as permanently "offline" because `Vehicle.status` is a write-once schema default until the first mTLS stream event ([MYR-437](https://linear.app/myrobotaxi/issue/MYR-437)). One server-side derivation serves both, so they cannot disagree, and no client re-derives it. **Four honesty rules are part of the contract:** absence of evidence yields `null`; `configuring` expires after 24 h to `null` (never to `awaiting_virtual_key`, which would send an owner to re-pair a key that is already paired); a currently-streaming car never carries `configuring`/`token_failed`, so a stale schedule row cannot card a live vehicle; and a car that streamed once and went quiet stays the FRESHNESS story, deliberately out of scope. `null` is NOT `ready` — there is no `ready` member, and consumers must not paint a positive badge from a no-claim value. **Deliberately NOT sourced from `key_not_paired` command rejections:** that code is overloaded — the server returns it when its own signing proxy is unconfigured — so a card built on it would tell the whole fleet to re-pair whenever our deployment broke. Tesla's `missing_key` on the config push is unambiguous, and the link-time push now SEEDS it (scheduling-neutral, `ON CONFLICT DO NOTHING`) so a new owner's card is populated within seconds instead of after the reconciler's first pass up to 45 minutes later. | Claude (go-engineer) |
| 2026-08-08 | **The Arriving alert rung stops firing on somebody else's route ([MYR-409](https://linear.app/myrobotaxi/issue/MYR-409)).** **No contracts version change, no wire-shape change, no new field, no new endpoint, no new error code** — migration **0034** and one new Go-owned column, nothing a client can see. This closes the residual §7.21.4 named and then could not fix: `navFresh` ran against `Vehicle."lastUpdated"`, a ROW stamp moved by every telemetry write of any column, so a car awake and streaming speed and position carried a permanently fresh timestamp over a `minutesToArrival` of arbitrary age. **The fix needed TWO gates and that is the interesting part.** (1) **Freshness now dates the READING** — `go_vehicle_control_state.nav_reading_at`, written only by a frame that actually carried a navigation-group member, read by both content-state projections in place of the row stamp. A nav CANCEL stamps it too, because "this car has no route" is current information and the same update NULLs the values it describes. The write is MONOTONE (`GREATEST(new, stored)`) via a dedicated statement rather than the shared COALESCE upsert, because the writer coalesces per VIN and two replicas share the row — a stamp that could regress would report a fresh reading as stale and flicker a gate off and on. NULL means no navigation has ever been seen and reads as NOT FRESH; there is deliberately **no backfill and no fallback to the row stamp**, which would reinstate the lie for exactly the population the column describes. This alone closes the FROZEN-VALUE arm. (2) **Relevance is a separate question and needed a separate gate.** A car navigating its owner's errand at the instant an instant ride is accepted is streaming perfectly fresh navigation — one second old, describing a route to the shops — so freshness passes it and, if the owner is two minutes out, the rung burns anyway. The reading must therefore also not predate `go_ride_requests.dispatched_at`, the instant the rider's pickup was pushed to the car; a nil dispatch (failed or skipped) is not relevant either, which the dormancy gate does not cover. **The remaining bound is stated, not papered over:** dispatch records when the destination was PUSHED, not when the car acted on it, so a frame in that few-second window postdates the dispatch while still describing the old route — closing it needs the car's active destination matched against the ride's pickup, and `pickup_lat_enc`/`pickup_lng_enc` are app-encrypted. It errs safely where it matters: on the accept push dispatch is approximately *now*, so nothing postdates it and the rung is left unspent. **`asOf` becomes more truthful with no shape change** — its derivation table already said a held reading keeps its old instant, and now the input can actually tell. **MYR-454's derived-status fold cannot bump the stamp:** it writes `status` from gear/speed, which carry no navigation, and the stamp lives on a different table behind a nav-presence gate — pinned by a test. The MYR-398 per-Activity `progress_reading_at` anchor is KEPT and still does the other half: this stamp dates the last frame that MENTIONED navigation, while that one catches a car dutifully re-sending the SAME value on its 30s resend. §7.21.3 (the freshness note, which previously recorded that a per-field nav timestamp on the car was unavailable) and §7.21.4 (the Arriving gate and its residual) updated; [`data-classification.md`](data-classification.md) §1.13 gains the column, **P0 156 → 157**. Implementation: `internal/store/{vehicle_nav_freshness,field_mapper,writer_flush,writer_coalesce,live_activity_leg,live_activity_context}.go`, `internal/push/{activity_alert,activity_ride_context}.go`. | Claude (go-engineer) |
| 2026-08-08 | **A rescued reservation may register a Live Activity again, and the owner is finally told the rider started ([MYR-461](https://linear.app/myrobotaxi/issue/MYR-461), [MYR-462](https://linear.app/myrobotaxi/issue/MYR-462)).** **No contracts version change** — no new endpoint, no new field, no new status, no new error code, no migration. Two behaviour changes from external-beta triage, both server-side. **§7.21.1 — the expiry refusal is now scoped to rides still at `accepted`.** `reservation_expired` is a verdict on the DISPATCH, and this document already promises twice (§7.8 transition notes, §7.8 `?activeForVehicle`) that such a ride's parties "may still cancel or **proceed manually**". The register guard refused on the dispatch columns alone, which conflated "the car never drove itself to the pickup" with "the ride is over" — and those come apart exactly when the humans proceed manually. In beta one expired reservation then ran as a real, driven ride (`arrived` → `enroute` → `completed`) for **99 further minutes** while every registration answered `409`, so the rider's card went dark at the expiry and could never return. Once the ride advances past `accepted` the verdict has been falsified by events and registration is allowed; an unrescued reservation stays refused, preserving the MYR-172 intent. **The predicate is duplicated by necessity** — the friendly sub-coded refusal in the handler and the guard inside the write that holds under a race — and the two must be scoped in lockstep, because the stricter one silently wins and the other becomes dead code. That is precisely how the first cut of this fix failed review: the statement was scoped, the handler was not, and the endpoint was unchanged while the store's test went green. **The client's part:** a dismissed Live Activity cannot be revived by any push, so a client that ended its card at the expiry SHOULD start a fresh one on next observing the ride at `arrived`/`enroute`. Not doing so keeps the old behaviour; nothing breaks. **§7.19.0 — `rideLifecycle` gains its first owner-facing status transition.** `arrived → enroute` (the rider pressing Start ride) is the owner's only signal that their car has left the kerb with somebody aboard, and no send site in the service had the owner as a recipient except the new-request push, so an owner learned the trip had started only by opening the app and waiting for a refresh — a **66-minute** perceived lag in beta. `ride.status.changed` now fans out to the RIDER for `accepted`/`declined`/`arrived` and to the OWNER for `enroute`; the gate reads the recipient's own row in each case and the two never both fire on one transition. **The owner-side send is suppressed when the owner IS the rider** — a self-ride is the common case on this platform and must not report the rider's own tap back to them. **`islandAlerts` stays false for the owner**: the MYR-413 gate defers a banner to the recipient's own Live Activity and only the rider registers one, so marking it suppressible could only delete the notification. Category stays `rideLifecycle` — per §7.19.0 the send sites must split before the column can — taking the fan-out site count from three to four. **Copy obeys the standing payload policy**: the requester's FIRST name and nothing else about the ride. Implementation: `internal/store/live_activity_repo.go`, `internal/telemetry/ride_activity_token_handler.go`, `internal/push/{copy,notifier}.go`. **The `Drive`/`AuditLog` reconstruction behind this triage was indirect** because `go_live_activities` is swept by a 24-hour `DELETE` on `updated_at`, so the row-level push record for the reported rides was gone before triage began — worth its own issue. | Claude (go-engineer) |
| 2026-08-02 | **An owner can finally ask what their car is serving RIGHT NOW — `?activeForVehicle` on the owner feed ([MYR-400](https://linear.app/myrobotaxi/issue/MYR-400)).** **No contracts version change** — no new endpoint, no new route, no schema field, no new status, no new error code, no migration. **ONE optional query param on the EXISTING feed**, following the grammar `?upcomingForVehicle` established (MYR-360), reusing the unchanged `RideRequestsListResponse` envelope and the unchanged opaque cursor; per `contracts/CONTRIBUTING.md` the versioning triggers are all *payload-shape* changes, and MYR-360 — the same kind of change on the same endpoint — touched that repo zero times and cut no tag. **The gap it closes.** Both existing owner surfaces describe rides that are NOT live: the default feed is `requested` only (decided rows leave it by construction) and `?upcomingForVehicle` is `accepted` AND STRICTLY FUTURE. A ride **in flight** fell between them, so the only way a client could hold one was to have performed the accept itself. iOS did exactly that — a DEVICE-PERSISTED ride-id pointer plus the party-only `GET /api/ride-requests/{id}` ([MYR-396](https://linear.app/myrobotaxi/issue/MYR-396)) — but a pointer only remembers what THAT device did: a ride accepted on a **second device**, or before a reinstall, was **unrecoverable**, and `integrateOwner` had to **DROP** a `ride_status_changed` frame about a live ride the pipeline did not hold, because nothing could fetch it. With external beta testers on more than one device that is routine, not an edge case. **The predicate:** `status IN ('arrived','enroute') OR (status = 'accepted' AND (scheduled_for IS NULL OR dispatch_status = 'sent' OR scheduled_for <= NOW()))`. The committed set is deliberately the three states the per-vehicle one-active-ride index (`uq_go_ride_requests_active_instant_vehicle`, 0013) is partial over — the states in which a car is *promised* to a ride; `requested` is excluded because the owner has not committed the car, and terminal states free it. **Dormancy is [MYR-376](https://linear.app/myrobotaxi/issue/MYR-376)'s predicate reused VERBATIM** (`store.rideNotDormantPredicate`, now on its third caller after the pickup transition and the Live Activity progress gate) rather than restated: two spellings of "is this ride live" would drift **silently**, each passing its own tests while disagreeing about whether a car is on its way. Consequently a reservation enters the slice at exactly the instant it becomes pickup-able — **including the dispatch arm**, so one the sweeper already dispatched is live whatever the clock says. **Dormancy gates `accepted` ONLY:** a picked-up ride is live by definition, and applying the reservation question past `arrived` would let a confirmed reschedule drop a ride **with a passenger aboard** out of the one read that exists to recover it. **`active` and `upcoming` are complementary** — a future dormant reservation is `upcoming`, the same reservation is `active` once live — but they are **NOT disjoint by construction and the doc does not claim they are**: the overlap set is exactly `accepted AND scheduled_for > now() AND dispatch_status = 'sent'`, a dispatched reservation whose instant is still future, satisfying both predicates at once. It is empty today by a property of the SWEEPER (which only claims `scheduled_for <= now()`), not of these predicates, and **becomes reachable when [MYR-192](https://linear.app/myrobotaxi/issue/MYR-192) reschedule ships** — resolve-reschedule moves `scheduled_for` forward without clearing the dispatch columns. **Recorded requirement on MYR-192:** it MUST clear `dispatch_status`/`dispatched_at` when it moves the instant, because §7.8 already promises a later reschedule re-imposes dormancy and the surviving `sent` arm *defeats* that promise. A cross-predicate test pins today's truth in both directions. **Both params at once is `400 invalid_request`**, and the refusal rests NOT on disjointness but on the two params naming different predicates, orderings and cursor anchor columns — no single page can honour both, and a precedence rule would silently answer a question the client did not ask. **Ordering does NOT depart from the default** (`createdAt DESC, id DESC`), unlike its sibling: soonest-first was load-bearing for a dialog naming "the NEXT reservation", whereas these rows are already live and the order need only be a stable total order. That means the slice reuses the **ordinary descending `(createdAt, id)` cursor and adds NO third cursor type** — §4.2.2 is unchanged. **It returns a LIST and the list can hold more than one row:** index 0013 is partial on `scheduled_for IS NULL`, so DUE RESERVATIONS are exempt from it and one can be live alongside an instant ride; the common case is 0 or 1, the pagination is not decoration. **A sweeper-EXPIRED reservation stays in the slice on purpose** — `reservation_expired` resolves the *dispatch*, not the *ride*; the row stays `accepted` and the matrix already promises its parties "may still cancel or proceed manually", so excluding it would invent lifecycle policy inside a read and hide exactly the orphaned, still-owed ride that has no other owner-scoped surface. **What a client can and cannot tell apart is NARROWER than it looks, and the doc says so plainly:** the wire carries `dispatchStatus`/`dispatchedAt` but NOT `dispatchError`, which is server-side only and deliberately absent from the projection. So `sent` (car was told) and absent (car not told yet) are readable, but `failed` is AMBIGUOUS — a sweeper-EXPIRED reservation and an ordinary nav-push FAILURE on a ride that is still genuinely happening are **indistinguishable on the wire today** (both read `accepted` / `failed` / `dispatchedAt` set). The server itself reads BOTH columns to tell them apart (`isReservationExpired`) precisely because `dispatchStatus` alone cannot, and no wire heuristic recovers it. Clients should render `failed` as the honest state it supports — *accepted, no car currently rolling* — and must not infer "expired" from `dispatchStatus` alone. **Surfacing a derived discriminator would be a contracts change and is deliberately NOT in this PR**; it is a follow-up decision. **Authorization is `incoming`'s exactly:** owner-scoped to the JWT subject, which the query string cannot widen, and an unknown/unowned `vehicleId` is an **EMPTY `200` page, never `403`/`404`** — the same existence-oracle argument as MYR-360, enforced by the **one shared validator** both params now call so they cannot drift on what a well-formed vehicle id is; only a MALFORMED value is `400`. **Absent both params the endpoint is still byte-identical**, pinned by the same golden-body test. **MIGRATION-INDEX AUDIT: no migration and no index change required.** The change is READ-ONLY — no new column, no new status value, no transition, so no row can enter or leave any partial index because of it. The query's leading conjunct is `owner_id`, served by the existing `idx_go_ride_requests_owner_status`; the vehicle and status/dormancy conjuncts filter a set already bounded by one owner. §5 error table, §6 catalog and §7.8 updated. Implementation: `internal/telemetry/ride_request_{active_handler,owner_handler,upcoming_handler}.go`, `internal/store/ride_request_active.go`. **iOS is a FOLLOW-UP story ([MYR-436](https://linear.app/myrobotaxi/issue/MYR-436)), not in this change** — cold-launch adoption asking this read when the pointer is empty, and `integrateOwner` fetching an unknown live ride instead of dropping the frame. Until a discriminator is surfaced, MYR-436 renders an expired reservation and a push-failed live one as the SAME honest state — *accepted, no car currently rolling*. | Claude (go-engineer) |
| 2026-08-02 | **Revoke and suspend now end an already-open WebSocket, not just the next handshake — §7.5.0 and §7.5.3 caveats CLOSED ([MYR-373](https://linear.app/myrobotaxi/issue/MYR-373)).** No REST wire change: same paths, same bodies, same statuses. What changes is what the mutation causes on the way out. Both §7.5.3 revoke and a §7.5.7 `PATCH` that leaves the grant `suspended` now publish an internal `share.access_revoked` event **after** committing and **after** the existing grantee cache bust, and the WebSocket hub closes that grantee's live sessions for that vehicle with close `4002` / `vehicle access revoked` (`websocket-protocol.md` §4.5.1). The ordering is deliberate: the close provokes the client's reconnect, and a reconnect served from a not-yet-busted cache would hand the grant straight back. **Scoped to the grantee** — the owner's own live map and every other viewer's keep streaming, because one grant moved and the car did not go away. Previously §7.5.0 said in as many words that there was "no socket teardown on suspend today" and §7.5.3 caveat (b) said a revoked viewer "keeps receiving broadcasts until they reconnect"; both are now obsolete and have been rewritten. Caveat (a) — the per-process cache — **survives and now covers the notification too**: on a multi-machine deployment only the instance that served the mutation clears its cache and closes its own sockets, and the others catch up within the 5-minute TTL plus one 60s revalidation sweep. Moot on today's single Fly machine. **`allowRides` closes nothing**, deliberately: it governs the §7.8 ride surface and has no WebSocket effect, so tearing down a viewer's live map over it would be a disconnection for a change that does not touch the live map. Lifting a suspension closes nothing either. `RevokeInvite` internally now reports the vehicle alongside the viewer, so the teardown targets the one car rather than every session the grantee holds. **One WebSocket-side behavior change rides along** and is recorded in `websocket-protocol.md` §6.1.1: a `subscribe` for a vehicle outside the caller's set now answers the typed `vehicle_not_owned` error **without closing the connection**. It used to close 4002, which turned into a server-driven reconnect loop once revocation began closing sockets — the viewer reconnects, re-handshakes into the reduced set, re-sends the subscribe its stale local state still lists, and is closed again. No REST surface is affected. **Sections updated in this document:** §7.5.0 (the suspension/DV-09 caveat), §7.5.3 (caveat (b) removed, (a) widened); `specs/rest.openapi.yaml` `revokeInvite` description. | Claude (go-engineer) |
| 2026-08-02 | **Three service-window enforcement gaps closed: the sweeper re-checks availability at dispatch, `scheduledFor` gets a 30-day horizon, and the accept-path vehicle read fails CLOSED — §7.8 ([MYR-372](https://linear.app/myrobotaxi/issue/MYR-372)).** **No contracts version change** — no endpoint, no schema field, no enum value, no header, no migration: every refusal here uses a shape §7.8 already documents, and the sweeper change is invisible on the wire by construction. **What forced it:** the [MYR-370](https://linear.app/myrobotaxi/issue/MYR-370) read-only server check, ahead of the first EXTERNAL TestFlight beta — riders who will hit these edges with no insider on hand to explain them. **GAP 1 — the sweeper never re-checked the vehicle.** §7.8 has said since MYR-313 that a scheduled accept is exempt from the `409 vehicle_unavailable` availability gate *because the scheduled-dispatch machinery must re-check the vehicle when the reservation comes due*. Nothing did. A car that entered service in the days between the accept and the due instant was nav-dialled anyway, and the rider walked out to meet a car on a lift. The sweeper now runs that check before the irreversible claim, from the same read that answers the MYR-342 pause.

⚠ **IT HOLDS ON `in_service` ONLY — NOT ON `offline` — AND THAT ASYMMETRY IS DELIBERATE.** The first cut of this change copied the accept gate and held on both, which adversarial review caught as a guaranteed stranding. `Vehicle.status` is `NOT NULL DEFAULT 'offline'`, the provisioning INSERT never sets it, and **nothing writes the value back for a car that has never streamed** — every status writer needs a live telemetry-stream event, the MYR-320 re-poll selects `in_service` rows only, and the §7.15 refresh writes every other field. ([MYR-454](https://linear.app/myrobotaxi/issue/MYR-454) narrowed this from "nothing in this server ever writes the value back" — the telemetry fold now heals `offline` on the first frame — which **sharpens** this argument: the permanently-`offline` population is precisely the never-streamed cars this paragraph is about.) So a key-paired car that has not yet streamed (every new owner, for their first days) sits at `offline` permanently, and a hold on it would repeat every 30 s until the ceiling and expire the reservation **every time** — while suppressing the very thing that clears it, since nav dispatch never depended on the stream and the command layer runs a **wake ladder** for offline/asleep cars as ordinary steady state. `in_service` is safe to wait on precisely because MYR-320 maintains it independently. **Accept's `409` on `offline` for INSTANT rides is unchanged** pre-existing MYR-277 behaviour. The two readers share one enumeration and act on different arms of it, with the argument recorded at the probe, at the predicate and at the adapter — this is the kind of asymmetry a later reader "fixes" back. **THE SEMANTICS ARE HOLD, NOT A NEW FAILURE:** the reservation stays `accepted`, nothing is claimed and nothing is recorded, and the next 30-second tick re-decides — so a car back from service *inside* the lateness window still takes the ride, a few minutes late, and one that never returns crosses the 30-minute ceiling and resolves as `failed` / `reservation_expired` exactly as a permanently-busy car already does. That is why **no new wire status was invented**: both states were already expressible and already documented, so a client renders this today with nothing new to learn. **THE VOCABULARY IS SPELLED ONCE** — the `in_service`/`offline` enumeration behind the accept gate's 409 is a single switch both surfaces read; a second copy would have drifted, and the copy that drifted would have been the unattended one. **ONE READ, not two:** the service arm and the pause arm come from a single joined statement, matching the discipline create and accept already keep. **GAP 2 — `scheduledFor` was unbounded above.** A pickup could be booked arbitrarily far out; a reservation is not a calendar entry but a standing instruction the sweeper re-evaluates every 30 seconds forever. Now `400 invalid_request` past **30 days** (`scheduledFor must be no more than 30 days in the future`), equal allowed, the existing shape and deliberately not a new code. The bound is a **fixed 720-hour duration compared in UTC**, not calendar arithmetic in the rider's zone — stated plainly because a client rendering "30 days" across a DST boundary can land an hour past it and be refused. **The PAST stays unbounded** — accept-after-due semantics depend on it, and the lateness ceiling is what resolves those. **GAP 3 — the accept-path vehicle read failed OPEN for scheduled rides**, leaving a reservation UNBOUNDED by the MYR-316 service window on a transient DB blip. The justification was that refusing would resurrect the MYR-313 stranding; the two failures are not alike — **MYR-313 was a PERMANENT `409` with no way through, a lookup failure is a TRANSIENT `500` the owner retries out of** — and silently granting a reservation that may sit inside a service visit to save one retry is the worse trade. Unknown now blocks on every path. **TWO CONSEQUENCES a consumer must know:** an unknown ride-share pause state and an unreadable rider grant now BLOCK a scheduled accept as well, because both ride that same read and both previously sailed past on its early return; clients MUST treat this `500` as retryable. **A vehicle that no longer EXISTS is separated out** as `409 vehicle_unavailable` / `Vehicle is no longer available` — permanent, so it must not wear a retryable code, and not a `404`, whose meaning on this endpoint is "unknown ride / non-party" and which would tell an owner their own request had vanished. **§7.22's advisory picker read is deliberately UNTOUCHED and stays fail-open** — it answers a calendar, not a gate, and folding a refusal into it would make a blocked car read as wide open. The **±45-minute** conflict half-width and all §7.22 semantics are unchanged. Also documented here for the first time: the MYR-369 rider-grant hold probe, which shipped in the sweeper but was described only in that issue's changelog row and never in §7.8's hold-policy list. **Item 3's other half — giving the service window a START instant, so a booking inside a FUTURE service visit is visible to the gate — is NOT in this change:** it needs a column and a contract decision, and stays open on MYR-372 for design. **Sections updated in this document:** §5 error-site catalog (create 400 inventory row now names the horizon; the accept service-window row now says fail-CLOSED; new row for the vanished-vehicle 409), §7.8 (create errors + new horizon-bound subsection; the availability-exemption, service-window and pause paragraphs on accept; the three-gate table; reservation-time dispatch hold policy, lateness ceiling and accept-after-due), and the MYR-316 transition-matrix bullet. The MYR-316 changelog row's fail-open clause is marked SUPERSEDED in place rather than rewritten — it is the record of what shipped that day. | Claude (go-engineer) |
| 2026-08-02 | **The completed `end` is HELD for the five-minute linger; the pair becomes update-then-delayed-end — §7.21.4 / §7.21.5 ([MYR-421](https://linear.app/myrobotaxi/issue/MYR-421)).** **No contracts version change** — no endpoint, no schema field, no header, no migration: the content-state is unchanged byte-for-byte and what moves is *when* the second half of the completion pair is sent. **The measurement that forced it:** an `.ended` Activity **leaves the Dynamic Island about 1.4 seconds after the end lands**. [MYR-418](https://linear.app/myrobotaxi/issue/MYR-418) had just moved the sixth expansion onto an alerting `update` and put the `end` one second behind it, so the completed check was on the island for roughly **two seconds in total** before the Activity vanished from it — the lock-screen card kept its five minutes via `dismissal-date`, but the ISLAND presence dies with the `end`, and r19 reported the check as unfindable. Nothing was wrong with the payload; the gap was the bug. **So the halves are pulled apart:** the `completed` transition sends **only** the alerting update, and the `end` — same final content-state, no alert — follows **five minutes later** from the ETA ticker's held-end pass. **One number:** the hold horizon and the `dismissal-date` offset are the same `DismissAfter`, the five minutes the client also counts down locally ([MYR-405](https://linear.app/myrobotaxi/issue/MYR-405)), and it is deliberately not configurable. **The dismissal-date is computed from the `end`'s OWN send instant** — measured from the completion it would already be in the past when the end landed, which tells iOS to remove the Activity at once and would delete the linger from both surfaces; the stated cost is that a dead-app phone now keeps the card about ten minutes rather than five, while a live app's own reaper wins as it always has. **The row SURVIVES the window and the tombstone travels with the delayed `end`** — tombstoning at completion would exclude the row from its own final push, the very defect the send-then-tombstone ordering exists to prevent. Every reader of `ended_at IS NULL` was audited and is tabulated in §7.21.4: the ETA ticker's active-leg filter excludes `completed` so no ticks reach a finished ride; the §7.21.6 banner gate is keyed `(ride, user)` and `statusAlert` generates no `completed` banner, so the window can silence nobody; registration still 409s on the ride's STATUS; a client-ended or APNs-rejected row drops out of the due list; and the 24-hour `updated_at` sweep cannot reach a row touched within the last ~90 seconds. **It is a sweep, not a timer** — a completed ride is terminal, so a lost in-process timer would strand the card until ActivityKit's multi-hour ceiling, whereas the pass recomputes the overdue set from `go_ride_requests.completed_at` on every tick of every process and a restart costs one pass. Island presence is therefore bounded at the hold to the hold plus one tick interval. **`LIVE_ACTIVITY_TICKER_ENABLED` no longer gates the loop, only the ETA refresh:** the held `end` is the second half of a *lifecycle transition*, which the switch's documented meaning already promises keeps working. **The unhappy endings are untouched** — `declined`, `cancelled` and `reservation_expired` keep their immediate single `end` and 30-second dismissal, and the 24-hour expiration floors and the phase-6 mark on the announcement are unchanged. **Adversarial-review corrections, same issue:** the tombstone is now the receipt for a DELIVERED end rather than for having tried — a failed context read, registry read or APNs push leaves the rows live and the next pass retries, since ending on a failed attempt would drop a terminal ride out of the due list permanently and, this being a batch, would do that to every card in the pass at once; a pass stops at the first sign of shutdown, because the tombstone runs on a cancellation-detached context and would otherwise retire rides whose pushes failed *because* of the shutdown; the sweep's per-ride work is registered on the notifier's inflight drain, which it bypassed entirely by running on the ticker goroutine, so `Wait()` no longer returns while held-end pushes are still writing; and owner vehicle teardown — whose `ended_at IS NULL` read could never see a completed ride before the hold — now carries each ride's status and ends a completed one AS completed rather than overwriting the rider's check with a cancellation. **Delta-review corrections, same issue:** the retry now TERMINATES — only 410/400 leave the due list, so a 403 TopicDisallowed or a timeout would have been re-attempted every 75 seconds until the 24-hour sweep deleted the registration (~1,150 doomed pushes, no end ever sent), and a **30-minute give-up horizon** now abandons the card honestly via a bulk `UPDATE` that no `LIMIT` can starve; the due list is ordered **newest-completion-first** rather than oldest, because a non-draining row held the head of a capped list and pushed newly completed rides out of reach; and the pass is bounded by a 15-second per-ride deadline plus a half-interval yield, so a saturated batch can never delay the ETA refresh by more than one tick. **403 was deliberately NOT reclassified as permanent** — `ExpiredProviderToken` is transient against the 40-minute JWT re-mint, and `send` is shared with the ETA refresh and the lifecycle fan-out, so doing so would delete live rides' registrations during a credential blip. | `go-engineer` |
| 2026-08-02 | **The sixth island expansion rides an alerting `update`, not the `end` — §7.21.4 ([MYR-418](https://linear.app/myrobotaxi/issue/MYR-418)).** **No contracts version change** — no endpoint, no schema field, no header, no migration: the content-state is unchanged byte-for-byte and what moves is *which APNs envelope carries the alert dictionary*, which is envelope policy rather than wire shape. **`completed` was the one rung of the phase ladder that is also the transition ending the Activity**, so [MYR-398](https://linear.app/myrobotaxi/issue/MYR-398) hung its expansion on the `end` push itself. On a real device that shape produced a registration that worked, alerts accepted by Apple through phase 5, an `end` delivered with the correct final content-state and the correct alert dictionary — and **no expansion, no check glyph, nothing** at the moment the ride completed. Apple's ActivityKit push documentation introduces the alert dictionary under `start` and `update`; of an `end` it says only to "include the final content state", and prints no `end` example carrying one. **An alert on an `end` is undocumented, not rejected, and not honoured** — and this surface has **no failure signal** (no `400`, no reason string, no metric), so a dictionary that does nothing is indistinguishable, from the server and from the logs, from one that worked. **So a completed ride is now a PAIR of pushes:** an alerting `update` carrying the terminal content-state (`status` `completed`, `progress` exactly `1`, no `eta`), then the **alert-free `end`** one second later carrying that same content-state plus the `dismissal-date`. The rule is enforced at the RENDERER rather than at the callers — an alert set on an `end` is dropped before the bytes are built — which is the direct answer to how a silent defect survived a review in the first place. **The `end` takes the LATER stamp, deliberately.** `aps.timestamp` is rendered in whole seconds, so a pair built from one clock read would carry the identical integer, and ActivityKit's documented rule covers only the *strictly older* push (that one it discards); what it does with an equal stamp is written down nowhere. The push that must never be discarded is the `end` — the rows tombstone as it goes out, nothing retries, and a dropped `end` freezes the rider's card until ActivityKit's own multi-hour ceiling reaps it — so the ambiguity is resolved in its favour. Backdating the *update* instead would merely relocate the risk onto the alert, where a stamp colliding with an ETA tick sent a second earlier is the same silently-swallowed expansion. **The `dismissal-date` is measured from the `end`'s own instant**, so [MYR-406](https://linear.app/myrobotaxi/issue/MYR-406)'s five minutes is five minutes regardless of the pair. **Phase 6 is finally reachable on the high-water mark.** The old `end`-borne alert could never raise it: the guarded `UPDATE` is scoped to `ended_at IS NULL` and the tombstone was already racing it, so a completed ride's row read **5** no matter what happened — which is exactly the ambiguous evidence the defect was found from. The mark is now written by the pair's FIRST push, while the row is still live (send-then-tombstone, §7.21.4), so a completed ride still reading 5 is now a real signal that the expansion did not land. **The unhappy endings are unchanged** — `declined`, `cancelled` and `reservation_expired` resolve to no phase at all, so no pre-`end` update is scheduled and each remains a SINGLE `end` push, still carrying its own final content-state, which is what the card renders for the seconds before it dismisses. **Both 24-hour `apns-expiration` floors survive intact:** the new alerting update is an ordinary alerting update in every respect (priority 10, the [MYR-413](https://linear.app/myrobotaxi/issue/MYR-413) alerting-update floor, the mark raised on delivery), and the `end` keeps the floor it already had. **Sections updated:** §7.21.4 (the completion pair's two exact payloads, the unhappy-ending example, the ladder table's phase-6 row, and a new "the sixth expansion rides an `update`" subsection), §7.21.5 (every transition is one push except `completed`). **No [`data-classification.md`](data-classification.md) change** — no new persisted field; `alerted_phase` merely now reaches a value migration 0029 already allowed. | Claude (go-engineer) |
| 2026-08-02 | **The ride-lifecycle banner now defers to the Dynamic Island — new §7.21.6 ([MYR-413](https://linear.app/myrobotaxi/issue/MYR-413)).** **No contracts version change** — no endpoint, no schema field, no header, no migration: this is a rule about which of two APNs channels speaks, not about what either one carries. r17 build `202608011648` at `arrived` showed the compact island with a banner sitting on top of it, and the client's reading was "instead of expanding the dynamic island you're pushing push notifications". Both pushes are ours, sent by **two subscribers to one `ride.status.changed`** — §7.21.4's `aps.alert`, which asks iOS to expand the island, and §7.17's ordinary alert carrying the same news in prose — and they **compete for the same pixels**: a banner and the expanded presentation want one strip of screen and the banner wins it, so the duplicate is also the thing standing in front of the expansion. **The rule:** skip the lifecycle banner when the transition is on the ladder's status half (`requested`, `accepted`, `arrived`, `enroute`, `completed`) **and** the RECIPIENT has a live (`ended_at IS NULL`) `go_live_activities` row for that ride. **`declined` keeps its banner unconditionally** — it is outside the design's six phases, its Activity update expands nothing, and it is among the worst news of a ride. **`cancelled` and `reservation_expired` have NO banner to keep**, and the section now says so rather than implying a surface that does not exist: §7.17's `statusAlert` covers `accepted`, `declined` and `arrived` only, so those two transitions have never produced a lifecycle banner, before or after this change — their only announcements are the Activity `end` push (which carries no `aps.alert` either) and the card's final state. Whether they *should* get one is an open product question, deliberately not answered here. For the same reason **three of the five statuses in the rule (`requested`, `enroute`, `completed`) are unreachable today** and are carried for forward-safety, so a banner added later cannot land in front of an expanding island by omission; the gate is reachable now only on `accepted` and `arrived`. **A tombstoned or APNs-deleted row reads as ABSENT**, so a rider who swiped the card away is never left dark. **The owner is unaffected as a consequence of the key rather than as a rule** — the row is `(ride, user)` and v1 registers only the rider — and the **`ride.request.created` push is deliberately never gated** even though `requested` is on the ladder: it is the owner's approval prompt, and an owner who rides their own car is *both parties*, so gating it would let the rider's card swallow the prompt needed to accept the ride. `ride.due` is not gated either (own topic, moves no status, expands no island). **Arriving needs no entry** — it is a threshold the ticker crosses, not a status, so no banner is ever sent for it. **Fails OPEN in all three modes** (no registry wired, lookup error, ungated delivery), same direction and same reason as §7.19.3, and ordered **after** the preference gate so a muted category costs one read rather than two. **The bound is stated rather than claimed away: suppression is per USER, not per device** — the registry keys on `(ride, party)` because ActivityKit rotates tokens mid-ride, so a rider with the card on one phone and a second phone signed in is one row, and both go quiet. **Also in this change:** the alerting payload was re-verified key-by-key against Apple's ActivityKit push documentation and pinned by an exact-layout test (`activity_alert_wire_test.go`) — push type, topic suffix, forced priority 10 and every `aps` key conform; the two divergences from Apple's printed example (literal `title`/`body` rather than `loc-key` dictionaries, and no `sound`) are permitted by the prose and by Apple DTS's answer on the alert field, and no defect was found in the shape. **Deleting a durable push means the one that inherits its job must become durable too.** The suppressed banner had **no `apns-expiration`** (APNs stores and retries it), while every non-`end` Activity update was pinned to its ~3-minute stale-date on the reasoning that a late ETA tick is worthless — so a rider offline in a tunnel when the car reached the kerb would have reconnected to *nothing*. **Alerting updates now take the same 24-hour floor as an `end`**; ordinary ticks keep the stale-date expiration, because a late refresh really is worthless. A late alerting update is safe in a way a late tick is not: the stale-date rides **in the payload**, so it self-declares as stale, and `aps.timestamp` ordering stops it overwriting a newer tick. **Sections updated:** §7.19.3 (a pointer to the second gate and why the two stay separate), §7.21.4 (the `apns-expiration` shapes gain the alerting-update floor), §7.21.6 (new). **No [`data-classification.md`](data-classification.md) change** — no new persisted field, and the one new log line carries two opaque cuids and a topic (all P0). | Claude (go-engineer) |
| 2026-08-01 | **The Live Activity v3 card — `asOf`, the island auto-expand ladder, and the Dispatch phase ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398)).** Contracts **v0.28.0**, migration **0029**, client [v0.28.0](https://linear.app/myrobotaxi/issue/MYR-398). Three additive changes on one surface. **(1) `LiveActivityContentState.asOf`** — the unix instant the state was computed from the car's last new reading, distinct from `aps.timestamp` (when the push was sent). The card can now say *"3 min ago"* against the reading rather than against the delivery, which is the difference between an honest staleness treatment and one that resets every 75 seconds whether or not the car said anything. Always present and P0; optional in the schema, so a pre-v0.28.0 payload still validates and an older build decodes unchanged. **(2) An `aps.alert` dictionary on six phase transitions** — `requested` (Dispatch), `accepted` (Enroute), **Arriving**, `arrived`, `enroute` (On trip), `completed` — which is the only way to make iOS expand the Dynamic Island, and **nothing else carries one**: not ETA ticks, not the stale flip, because an island that opens every 75 seconds is a widget the rider turns off. No `sound`, and `apns-priority` is forced to **10** whenever an alert is attached, overriding the ticker's conserving 5. The title and body are **fixed server-side constants** and the one deliberate exception to §7.21.3's "the server sends the enum, never prose" — `title-loc-key` was rejected because a key the installed build lacks renders as the raw key on a lock screen — so the copy is bland, generic, and bound by `copy.go`'s P1 payload policy in full (no place, no address, no nickname, no name), which is stricter than the content-state's `destination` exception. **Once per phase per Activity, enforced by a monotone `1..6` ladder** against `go_live_activities.alerted_phase` (**migration 0029**): a push may alert only strictly above the mark, and the mark is raised only after Apple accepts. Registration **seeds** the mark at the ride's current phase (the app drew that state locally, so announcing it would be an unearned interruption) and the migration backfills existing rows at Enroute so the deploy does not expand every live island at once; a token rotation preserves it, while re-registering after a client-side **end** re-seeds it, because that is a new Activity the app has just drawn. **Arriving is the one rung that is not a ride status** — pickup ETA ≤ 2 minutes — and therefore the one that needs the state: it is true on every remaining tick of the leg once true at all. It is gated on the reading's **age** as well as its value, because `minutesToArrival` outlives the route that produced it and the accept push on an *instant* ride fans out before the dispatched route streams back; ungated, accepting a ride would announce "almost here" for a car twenty minutes away and **durably burn rungs 2 and 3**. Replica duplication is **bounded, not eliminated**, and §7.21.4 now states the bound honestly rather than claiming it away. **(3) §7.21 token registration accepts `requested` rides** and the ticker's active-leg set widens to match, so the Dispatch card ("Finding your ride") stays fresh instead of apologising for being stale while the search is genuinely running; `eta` is **withheld** on `requested`, since the vehicle is assigned to nobody and an arrival time computed from the owner's errand is fabricated rather than degraded. Both terminal endings of a `requested` ride (rider cancel, owner decline) were already covered by the guarded status write. **`v` stays `1`** — additive optional fields are not a meaning change — and there is no new endpoint, no new error code and no new env var. **Sections updated:** §7.21 header (contract version + storage), §7.21.3 (`asOf`), §7.21.4 (the `aps.alert` dictionary, the ladder, the Arriving freshness gate, the replica bound), §7.21.5 (the Dispatch phase). **In [`data-classification.md`](data-classification.md):** §1.18 gains the `alerted_phase` row and the count audit trail moves **P0 154 → 155**. | Claude (sdk-architect + go-engineer) |
| 2026-07-31 | **A completed Live Activity now lingers 5 minutes, not 15 ([MYR-406](https://linear.app/myrobotaxi/issue/MYR-406)).** **No contracts version change** — nothing on the wire moves: the `dismissal-date` is a server-side policy on the APNs `end` envelope (§7.21.4), not a schema field, and no request or response shape is touched. The client ends a completed Activity locally at 5 minutes ([MYR-405](https://linear.app/myrobotaxi/issue/MYR-405)) and that timer wins whenever the app is alive, so the server's date is only the fallback for a phone whose app is dead — leaving it at 15 meant one ride could linger for either duration depending on something invisible to the rider. **The unhappy endings are unchanged at 30 seconds** and the constant behind them is deliberately still separate from the completed one; the tests now pin all three offsets as literals so folding them together cannot move an ending silently. **The `end` push's 24-hour `apns-expiration` floor is untouched** — a different mechanism ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398) review), and the dismissal-date has never been what an `end` expires at. **Sections updated:** §7.21.4 (`dismissal-date` policy). | Claude (go-engineer) |
| 2026-07-31 | **The Live Activity learns where the car IS — `LiveActivityContentState.progress`, a 0..1 fraction of the current leg ([MYR-398](https://linear.app/myrobotaxi/issue/MYR-398)).** Contracts **v0.27.0**, migration **0027**. r16, client-directed with an Uber lock-screen reference: the card showed a status and a nickname and *no relation between the car, the pickup and the dropoff*, which is the one thing a person waiting on a sidewalk is actually looking for. The redesign wants an ETA headline, a "Meet at {pickup}" line and a progress TRACK, and the notable fact about the server half is that it cost **exactly one new key**: `status` already carries the leg (`accepted` is the car coming to you, `enroute` is the car taking you onward), `eta` already carries the time, and **the pickup label is deliberately NOT pushed** — it cannot change for the life of a ride (§7.8 has no endpoint that edits it; MYR-192 negotiates the TIME), the app already holds it in the `RideRequest` it started the Activity from, and it is P1. `destination` is on this surface as a NARROW exception justified by NECESSITY — the server is the only party that knows it at push time — and necessity is exactly what fails for a string the receiving device typed in itself; adding it would put a second P1 place label through APNs ~40 times a ride to tell a phone what it already knew. It belongs in the Activity's static `ActivityAttributes`. **`progress` is P0 and its neighbour `destination` is P1, which is not an inconsistency:** a unitless fraction of an unnamed journey carries no coordinate, no place name and no distance, so it is the one field here that could ride a future owner-side Activity unchanged. **The derivation is the contract.** Preferred input is the car's own `tripDistanceRemaining` against the value captured at the leg's start; the fallback is `minutesToArrival` measured the same way; there is still no route solver in this service, so no route means **no key** rather than an invented number. A reading older than **3 minutes** — the same horizon as the `stale-date`, because past it ActivityKit is already telling the rider the card is out of date — does not move the arrow: the last delivered fraction is HELD, or nothing is sent if there has never been one. **An absent progress renders a trackless card; a wrong one renders a lie**, which is why `0` is never sent as a stand-in for "unknown" (`0` is the claim "the car has covered none of it") and why every degraded path resolves to an absent key. **MONOTONE PER LEG, clamped server-side** to the highest fraction already DELIVERED to that Activity — a regression is almost never the car, and an arrow sliding backwards reads as a broken widget rather than as traffic — while **`eta` is deliberately NOT clamped**: "most of the way there and arriving later than it said" is a coherent pair and the client must render both as given. A route that GROWS re-anchors (the ground moves, the arrow does not) instead of restarting at zero. **A full track means the leg is over and only the ride record may say so:** an in-flight leg is capped at `0.99`, and exactly `1` is sent on `arrived` and `completed`, which are assertions rather than estimates. The anchor lives in the database (migration **0027** adds `progress_leg`, `progress_source`, `progress_baseline`, `progress_value` to `go_live_activities`, all nullable with **no default** — a default of 0 would be a lie with a number on it) because the ETA ticker is **not sharded across replicas**, so two in-memory baselines for one ride would make the arrow oscillate between them every 75 seconds, and a deploy mid-ride would forget the baseline entirely. Written **after** a delivery Apple accepted, never before: the clamp's floor must record what the phone HAS SEEN. A token rotation preserves it. **`v` stays `1`** — a new optional field is not a meaning change, Codable ignores unknown keys, and both directions (old build/new payload, new build/old payload) are safe. Cadence is unchanged (60–90s ticker + lifecycle pushes) and no new env var. [MYR-394](https://linear.app/myrobotaxi/issue/MYR-394)'s active-ride REST position polling improves this input for non-streaming cars; nothing here depends on it and nothing here invents a position in its absence. **Sections updated:** §1 TOC entry 21, §7.21 header (storage + contract version), §7.21.3 (the `progress` row, "The progress track" and "Why the 'Meet at {pickup}' line is NOT on the wire"), §7.21.4 (two updated examples plus a new no-nav-route one), §7.21.5 (`has_progress` on the send line). **In [`data-classification.md`](data-classification.md):** §1.18 gains four P0 rows and the count audit trail moves **P0 148 → 152**. | Claude (sdk-architect + go-engineer) |
| 2026-07-31 | **The schedule picker can now see the conflict before the rider commits to it — new §7.22 `GET /api/vehicles/{vehicleId}/booked-windows` ([MYR-385](https://linear.app/myrobotaxi/issue/MYR-385)).** Contracts **v0.26.0** (new `schemas/booked-windows.schema.json`: `VehicleBookedWindowsResponse`, `BookedWindow`). r15 feedback: *"Still letting me schedule for noon even though I already have a ride scheduled for that time."* The MYR-383 gate (§7.8) shipped in r14 and **does** refuse that booking — but the picker had no conflict awareness, so the `409 time_conflict` was the FIRST signal the rider got, and a refusal that arrives after somebody has committed to a choice reads as a bug rather than as a rule. This is the read side of that gate. **THE INVARIANT:** a slot the picker dims is exactly a slot the gate would refuse; a slot it leaves enabled is exactly one the gate would allow — **held by construction, not by care**. MYR-385 **FACTORED** `rideWindowConflictPredicate` rather than copying it: the per-arm membership halves (`rideWindowReservationOccupies`/`rideWindowInstantOccupies`) and the overlap test (`rideWindowOverlap`) are named fragments the gate and the read are BOTH assembled from, and `RideConflictWindow` reaches both through the same bind parameter (`rideWindowHalfWidthExpr`, `$4`). Both statements are `occupies AND overlap` per arm; the gate passes one instant as both bounds, the read passes the caller's range. **The gate's rows and strictness are unchanged, and so is its SQL SPELLING — deliberately.** The first cut of this factoring collapsed the two arms' geometry into one COALESCE'd pair (`COALESCE(scheduled_for, NOW()) ∓ W` compared against the bounds); that returns identical rows and is a **performance regression**, because Postgres cannot match an expression to a plain btree column and the range therefore drops out of `Index Cond`. `rideWindowOverlap` keeps `W` on the CONSTANT side so `r.scheduled_for` stands bare and stays SARGable; the COALESCE'd `rideWindowStartExpr`/`rideWindowEndExpr` survive only in the read's SELECT projection, where an expression costs nothing. EXPLAIN-verified on 1241 open reservations for one car: the COALESCE'd WHERE clause plans as `Seq Scan`, `Rows Removed by Filter: 11168`, 251 buffers; the bare-column spelling as a `Bitmap Index Scan` whose Index Cond carries `vehicle_id` **and both `scheduled_for` bounds**, 1 row, 1 heap block, 9 buffers — ~16× on the probe that runs inside the per-vehicle advisory booking lock on every create and every accept. A plan test (`internal/store/ride_request_window_index_plan_test.go`) asserts the Index Cond for both statements against a real Postgres, so a later "simplification" back to the shared expression fails a test rather than a pager. **CONCRETE endpoints, not anchor-plus-radius, and that is the whole design.** The server could have emitted each ride's instant and let the client add ±45 minutes; it deliberately does not. Per §7.8 the half-width is a **product guess** living in one place on the server and encoded in no schema, enum or client — emitting resolved instants is what preserves that, so widening or narrowing it changes every picker on the **next response**, with no client release and no contract bump. Clients MUST NOT re-derive, re-centre or infer it; a contracts test asserts no `conflictWindowMinutes`-shaped field exists. **The interval is OPEN at both ends**, inherited from the gate's strict comparison — a booking at exactly `start` or `end` is ALLOWED, so a picker dims `start < slot < end`; a store test books against an emitted edge in both directions using the gate itself as the oracle. **CREATE-path semantics**, i.e. `countPending = true`: a still-undecided `requested` claim blocks in full and `pending` changes only the WORDS ("already requested", not the untrue "booked"). Answering with the accept-side rule would hand a rider a slot create is going to refuse. **`own` tracks the RIDER** — the r15 report was a rider colliding with their own noon reservation, and "that car is busy" would have been a poor answer; it is presentational only, and an owner sees `own: false` for rides other people booked in their car. **THE ACTIVE-INSTANT ARM IS REPORTED, HONESTLY.** §7.8's second arm anchors on `NOW()` rather than on a stored instant, so it is emitted as `[now − 45min, now + 45min]` **as of the read** and **slides** afterwards while the response does not: it can UNDER-report against a still-running ride (a slot an hour out is enabled now and would be refused forty minutes from now) and never over-reports, and it vanishes when the ride ends. That is the one moving quantity on the surface and the specific reason the create-time **`409 time_conflict` remains the authority** — this reduces how often a rider meets it, it does not replace it. A merely `requested` instant ride occupies nothing, mirroring the gate exactly. **AUTHORIZATION IS THE RIDE-CREATE GATE, byte-for-byte** — the same `vehicleAccessFor(…, capRides)` call on the same handler, which is why the endpoint is a method on `RideRequestHandler` despite serving a `/api/vehicles/` path: it answers "what would create refuse?", so a caller create would turn away must be turned away here or the endpoint becomes an oracle. Owner → 200 with no share lookup; `allowRides` viewer → 200; base-capability viewer, suspended grant and stranger → `403 vehicle_not_owned`; unknown vehicle → `404`. A refused caller never reaches the store. **Deliberately NOT checked:** the §7.18 pause and the §7.16 service window — both refuse a *create*, neither describes a window, and folding them in would make a paused car answer `items: []`, which a picker reads as WIDE OPEN. **P1 DISCIPLINE:** two instants and two booleans per ride, nothing else. The occupying ride's id, rider, requester name, passenger and places never leave the statement (data-classification.md §1.9); the instants and `pending` are P0 operational timing, the same disclosure §7.8's refusal already makes. **This grants no capability the caller lacked** — somebody authorised to book the car can already discover the same instants one create attempt at a time, since each 409 names the nearest — which is precisely why the gate had to be create's and not a looser one. Guarded by a loop over forbidden field names in both the contracts tests and the handler test, so adding one fails a test rather than needing a reviewer to notice. **Validation refuses rather than clamps:** `from` defaults to now (and may be in the past — an active-instant window straddles it), `to` to `from`+7d; `from == to` and `from > to` are `400`, and a span over **14 days** is `400` rather than a silent clamp, because a shortened answer looks complete and under-dims. The cap is `store.MaxBookedWindowRange`, **injected** into the handler by `wiring.go` rather than restated as a handler-local literal — the bound belongs to the store (it is what makes the read's absent `LIMIT` safe), and the refusal names the value it enforced. **Query-string caveat:** UTC `Z` is recommended; a literal `+` in an RFC 3339 offset decodes to a space and must be percent-encoded (`%2B`), or the value arrives unparseable and is a `400`. Unpaginated with **no `LIMIT`** — the range cap divided by the gate's own minimum separation is the bound, and a limit would trade an unreachable runaway for a reachable silent truncation that breaks the invariant. A store failure is `500`, never `items: []`. **READ-ONLY: no migration, no new column, no new index** — the reservation arm rides `idx_go_ride_requests_vehicle_window` (0026), which serves `vehicle_id = $1` on its leading column and **both `scheduled_for` range bounds** on its second as Index Cond, with the arm's `IS NOT NULL`/status conjuncts proved from the index's own partial predicate; the active-instant arm is a separate bitmap branch over the `scheduled_for IS NULL` partial indexes (0004/0013). **No new error code and no new sub-code.** **Sections updated:** §1 TOC (entry 22), §6 catalog (one row), §7.8 (two cross-references), §7.22 (new). **Footnote worth keeping:** the store file is `ride_request_booked_windows_read.go` — Go reads a trailing `_windows` as the GOOS build constraint `windows` and had silently excluded the whole file from the darwin/linux build, presenting only as a repo method that did not exist. | Claude (sdk-architect + go-engineer) |
| 2026-07-31 | **A vehicle cannot promise two rides in one window — the per-vehicle reservation conflict gate ([MYR-383](https://linear.app/myrobotaxi/issue/MYR-383)).** Client report (TestFlight r14): a rider booked a SECOND reservation into a time window an already-**accepted** reservation held on the same car. **Nothing refused it** — the collision surfaced only when the owner declined it by hand minutes later. Reservation ↔ reservation collision was a **documented v1 boundary** ([MYR-179](https://linear.app/myrobotaxi/issue/MYR-179), §7.8 "v1 boundaries"), explicitly deferred to "booking-time conflict detection, not dispatch". This is that gate. **THE MODEL:** every OPEN ride on a vehicle **occupies a window of ±45 minutes** around its ride instant — `scheduledFor` for a reservation, **now** for an active instant ride — and a reservation whose instant falls **strictly inside** one is refused. Exactly 45 minutes apart is a legal back-to-back booking. **Enforced at CREATE (rider-facing) and again at ACCEPT (backstop)**, the two-layer shape of the MYR-316 service-window bound, sharing **ONE** SQL predicate and **ONE** response composer so the layers cannot drift. The accept layer is not redundant: pairs booked *before* this gate can still be mutually conflicting, and the active-instant arm is time-relative (a car idle at booking may be mid-ride at accept). **ERROR SHAPE: `409 vehicle_unavailable` with the NEW `subCode: time_conflict`** — no new top-level code, because this is precisely the capability refusal that code already carries (MYR-277 in-service/offline, MYR-266 busy, MYR-342 paused) and NOT `conflict`, which means an illegal lifecycle transition. The sub-code is required because those three siblings are conditions of the car **right now** (client says "try later") while this is a property of the **time the rider picked** (client returns them to the picker), and §4.1 forbids branching on the message. **P1 DISCIPLINE — the body names the WINDOW, never the other ride:** the conflicting instant (P0 operational timing, the same disclosure the MYR-316 refusal already makes) and, when the claim is only pending, that fact — and **no** id, rider, requesterName, pickup or dropoff, and no `activeRideRequest`-style sibling object. The caller is not a party to that ride; a booking probe must not become a way to enumerate a stranger's calendar. Three messages, each true: `Vehicle is already booked for a ride at <RFC3339>`, `Vehicle already has a ride request for <RFC3339>` (pending), `Vehicle is already on a ride and can't also be booked for that time` (active instant, no instant to name). **`requested` COUNTS ON CREATE, NOT ON ACCEPT — deliberate, in ONE predicate behind one flag.** A pending reservation is a CLAIM on a slot but not a COMMITMENT of the car: create must not hand a rider a booking that will collide with somebody's pending request, while an owner's accept is precisely HOW a contested slot is decided — counting the peer there would refuse BOTH sides of a legacy double-booking and leave the owner unable to confirm either (the same stranding, from the other side), and would call a slot "booked" that nobody has booked. **MYR-313 NARROWED, PRECISELY:** its exemption covers the **status/availability gate only** (a reservation still accepts fine for an `in_service`/`offline` car — unchanged). The service-window bound (MYR-316), the pause (MYR-342) and now this window gate all **do** apply to reservations, because they ask about the reservation *instant* rather than the car *today*; §4.1.1's "scheduled rides are exempt" is therefore about (a)+(b), never about `vehicle_unavailable` as a whole, and the catalog row now says so. **RACE CONSTRUCTION: a per-vehicle ADVISORY TRANSACTION LOCK, not an index** — `BEGIN; pg_advisory_xact_lock(<vehicle>); <probe>; <INSERT|guarded UPDATE>; COMMIT`, so two conflicting bookings serialize and the loser's probe sees the winner's commit. A partial unique index or a btree_gist `EXCLUDE` was rejected for **three independent reasons**: half the predicate is relative to **now** (no immutable index expression can hold it, so an EXCLUDE covers one arm and the rule needs two mechanisms); a constraint cannot install over violating data, so the migration would have to **cancel live production reservations** to create itself; and `btree_gist` is an extension `CREATE EXTENSION` may not be permitted to install. **Every accept takes the lock**, including an instant accept that skips the probe, which is what makes the two serialize. **Migration 0026** `idx_go_ride_requests_vehicle_window` is a **plain partial index** — it makes the probe fast and **enforces nothing**, so NO production reservation is cancelled to install it (contrast the 0004 / 0013 dedups); the active-instant arm reuses `uq_go_ride_requests_active_instant_vehicle` (0013). **The 45 minutes is a PRODUCT GUESS and lives in exactly one place** (`store.RideConflictWindow`), passed to SQL as a bind parameter — no schema, no enum, no index, no client encodes it, and no test hard-codes it. **REMAINING BOUNDARIES, stated:** an **instant accept** is still not gated by a near-term reservation (the reverse direction IS now gated) — it is a change to the instant path with its own product question and stays on the picker-annotation follow-up; and a pair of reservations *already accepted* before this shipped still both dispatch. **v1a is server-side enforcement only** — no new read surface and no picker annotation. No new top-level error code, no new status, no wire-shape change. §4.1 `subCode` enum, §4.1.1 `vehicle_unavailable` row, §4.1.1.a, §4.1.1.b emission audit, §7.8 create section (new "Per-vehicle ride-window conflict") + create error list, §7.8 accept section (new backstop subsection + the three-gate table) + accept error list, and the MYR-179 "v1 boundaries" bullets annotated; `schemas/ws-messages.schema.json` `ErrorPayload.subCode` **`enum` member added** (`time_conflict`, appended LAST to preserve member order — the enum-order codegen trap) **and** its description extended. That enum is a machine constraint, not documentation: a strict validator would reject the value the server now emits without it, and the `wserrors.SubCode` discipline is the same as `ErrorCode`'s — the Go enum, both catalog tables and the JSON Schema move in ONE PR. **Contracts follow-up:** the same additive enum member + sentence is owed to `myrobotaxi/contracts` (minor tag — a new member on an append-only enum), alongside the pre-existing `vehicle_unavailable` enum drift noted in [MYR-375](https://linear.app/myrobotaxi/issue/MYR-375) — see the PR body. Implementation: `internal/store/ride_request_conflict{,_queries}.go`, `internal/store/ride_request_status_update.go` (`UpdateStatusFromUnconflicted`), `internal/store/ride_request_repo.go`, `internal/store/migrations/0026_ride_vehicle_window_index.{up,down}.sql`, `internal/telemetry/ride_window_conflict.go`, `internal/wserrors/wserrors.go`, `cmd/telemetry-server/ride_request_adapters.go`. | sdk-architect |
| 2026-07-31 | **A reservation cannot be picked up before its dispatch — reservation dormancy on `POST /api/ride-requests/{id}/picked-up` ([MYR-376](https://linear.app/myrobotaxi/issue/MYR-376)).** Production defect: an owner accepted a ride due the **next day** and immediately tapped "Picked up". `accepted → arrived` succeeded with `dispatchStatus` still absent — MYR-179 defers a reservation's leg-1 nav push to `scheduledFor`, so the ride sat in `accepted` looking exactly like an instant ride whose car was already rolling. The car was never dispatched and was in service; from `arrived` the rider's `start` would have pushed a real **dropoff** nav to it, and the ride had **no legal exit** (cancel is illegal from `arrived`). **The model:** a reservation is **DORMANT** from accept until the **earlier** of its dispatch resolving `sent` and its `scheduledFor` arriving; `accepted → arrived` is refused for that whole interval. Predicate, verbatim: `scheduled_for IS NULL OR dispatch_status = 'sent' OR scheduled_for <= NOW()`. **The time arm is load-bearing, not a softening** — the transition-matrix note for MYR-179 promises that a reservation the sweeper failed (`reservation_expired`), skipped, or never reached stays `accepted` and its parties "may still cancel or **proceed manually**"; a gate keyed on `'sent'` alone would leave every such ride with **cancel as its only exit**, the same stranding arrived at from the other side. Past due the owner is at the car and the ride is owed, so a manual pickup **is** the documented recovery. What is refused is strictly the **pre-due** pickup. **No new error code, no new status, no wire-shape change and no migration:** the refusal reuses `409 conflict` with the message `a scheduled ride cannot be picked up before its dispatch`, so a client switching on `code` needs no change. **INSTANT rides are entirely unaffected**, including ones whose push resolved `failed`/`skipped` — the car is at the kerb and the owner drives anyway. The comparison is **inclusive**; a **dispatched** reservation is live whatever the clock says; a confirmed **reschedule** moving the instant later correctly re-imposes dormancy; and the refusal is a **deferral** — the identical request succeeds once either arm opens. **Enforced INSIDE the guarded write**, never as a pre-check: the predicate rides the same single `UPDATE` as the `WHERE status = ANY(<legal-from>)` guard (new `store.RideRequestRepo.UpdateStatusFromDispatched`), so a pickup racing a dispatching sweeper — or the clock crossing `scheduledFor` — is arbitrated by the database, and `status = 'arrived'` ⇒ the reservation was live holds by construction. A zero-row miss is **classified by derivation** rather than by a follow-up read of `dispatch_status`/the clock: both dormancy escape hatches can only *arrive* in the gap, so re-evaluating them would misreport a genuine dormancy refusal as a status conflict. `start` gets **no** new gate and needs none — a dormant reservation can no longer reach `arrived`. The reservation sweeper's claim-time re-validation race with `picked-up` is **unchanged and still real**: the gate lifts exactly when the sweeper becomes interested, and the claim's `status = 'accepted'` re-check is what keeps a manual pickup safe mid-sweep. §7.8 pickup section (new "Reservation dormancy" subsection), owner-handshake paragraph, transition-matrix cell + notes, "Reservation-time dispatch" claim-time paragraph, and the §5 error-cases table updated. | sdk-architect |
| 2026-07-31 | **Live Activity push updates for the rider's active ride — new §7.21 `POST`/`DELETE /api/ride-requests/{id}/activity-token`, `go_live_activities` (migration 0025), an ActivityKit sender and an ETA ticker ([MYR-172](https://linear.app/myrobotaxi/issue/MYR-172); content-state, cadence and staleness decisions from [MYR-194](https://linear.app/myrobotaxi/issue/MYR-194)).** Contracts **v0.24.0**. §7.17 gave us alerts, which are MESSAGES: they fire once, they stack, and they say something happened rather than what is true now. A ride is a STATE a rider watches — 6 minutes away, then 4, then here — and the honest rendering of that is one lock-screen card the server keeps current, not eleven notifications. The app starts the Activity locally when a ride is accepted (which needs **no permission prompt**, unlike an alert) and ActivityKit hands it a per-Activity token; these two endpoints are how that token arrives and how the client says it is over. **RIDER-ONLY in v1** and the split matters: the owner is a party and may read the ride, but an owner registration would create rows the sender pushes RIDER content to — including the P1 `destination` — so an owner gets `403 permission_denied` while a **non-party gets `404 not_found`, never `403`**, preserving the §5.2 rule that the endpoint is not an oracle for ride ids. A terminal ride answers **`409 conflict`**, which is an instruction rather than a refusal: that Activity will never be pushed to again, so the client should end it locally now. **So does a ride whose reservation expired** — with `subCode: reservation_expired`, because the sweeper leaves that ride's status at `accepted` and a bare `conflict` on a live-looking ride is indistinguishable from a bug. Both guards are predicates **inside the upsert**, not checks before it, so a `POST` racing the ride's ending cannot re-register after the tombstone. The token must be **hexadecimal** as well as ≤256 characters — the charset is what an APNs token has always been, and validating it means a pathological value never reaches the sender that interpolates it into a request URL. **Registration is an UPSERT on `(ride, rider)` because ActivityKit ROTATES the token mid-Activity** and expects the server to switch to the new one — the §7.17 posture of keying on the token itself would accumulate a row per rotation and leave the sender guessing which is live — and re-registering after an end **clears the tombstone** on a ride that is still happening, because the client is telling us it has a live Activity again — but NOT after a terminal status or a reservation expiry, which are final. **Neither response echoes the token** (P1): `{"registered":true,"sandbox":…}` and `{"ended":…}` are booleans only, which is also the CG-DC-5 discharge — the token is projected onto no wire field, so `internal/mask` needs no entry, and both routes are self-scoped to the ride's rider so there is no role dimension to mask across. **What the lock screen shows is an APNs push, not an endpoint:** topic `{APNS_TOPIC}.push-type.liveactivity` (DERIVED, no new env var — Apple rejects the suffix and the `apns-push-type: liveactivity` header independently as `TopicDisallowed`), priority **10** for lifecycle transitions and **5** for ETA ticks so a periodic refresh never competes with "your car is here" for Apple's per-Activity budget, and `apns-expiration` **per shape**: an `update` expires at its **stale-date**, because an update arriving after its content stopped being trustworthy is worse than one that never arrives — it resets the staleness clock on expired information — while an `end` is held for **24 hours**, because it is the only push here with no successor (the rows are tombstoned as it is sent and nothing retries), so a stale-date expiry would leave a phone that was offline for three minutes showing "your car is on its way" until ActivityKit's own multi-hour ceiling reaped it. `aps.stale-date` is timestamp **+ 3 minutes** (MYR-194 honest-staleness): past it ActivityKit renders its own "as of X min ago" treatment, so a phone that stopped receiving pushes SAYS SO instead of presenting a three-hour-old ETA as current — which is what makes every degraded mode here safe. Dismissal: `completed` lingers **15 minutes** so the rider can look at the arrival state; `declined`/`cancelled`/`reservation_expired` go after **30 seconds** — not zero, because an Activity that vanishes before the rider's eyes reach it is worse than the bad news. Ending is a **send THEN a tombstone**, in that order: a row ended first would be excluded from its own final push and leave a declined ride reading "your car is on its way". Content-state is five fields (`v`, `status`, `eta`, `vehicleName`, `destination`) — `v` because the Swift `ContentState` is frozen the moment a build ships, `status` as an ENUM never prose so copy changes in an app update rather than a deploy (and `reservation_expired` is a member that is NOT a ride status, since the sweeper leaves a late reservation at `accepted`), and **`eta` as an ABSOLUTE unix instant, OMITTED when unknown** — a duration decays silently on a screen the server cannot repaint while an instant stays true however late it is read. **The ETA is the CAR'S OWN carried nav ETA** (Tesla `minutesToArrival`, whole minutes); there is **no server-side route solver in this service**, so no route means no key rather than an invented number. Ticks run every **60–90s** (75s ± 20% jitter) while the ride is `accepted`/`arrived`/`enroute`, under the new kill-switch **`LIVE_ACTIVITY_TICKER_ENABLED` (default true)**, which stops ONLY the periodic refresh and leaves lifecycle transitions updating — the degraded-but-honest state an operator wants when Apple throttles; stale rows are swept after 24h. `updated_at` is stamped by every successful push as well as by registration, which is what makes the per-pass cap rotate instead of shedding the same Activities on every tick. **The whole surface rides the existing `ride_lifecycle` preference category (§7.19, MYR-349):** a rider who muted ride updates gets NO Activity updates, the Activity still RUNS locally, and it falls back to its own stale rendering — a card that says it is out of date rather than one that lies. `destination` is the one deliberate exception to the alert-copy policy (`internal/push/copy.go`): the rider's own ride on the rider's own device, never sent to an owner's Activity and never in an alert body. **`ride_request_id` is the FIRST genuine foreign key in the `go_` namespace** — CG-DL-9 bars references to the Prisma-owned schema, but `go_ride_requests` is Go-owned, and the ride's hard-delete paths make `ON DELETE CASCADE` the choice that cannot leave behind a ROW addressing a ride that no longer exists; `user_id` stays an unenforced pointer as CG-DL-9 requires. The cascade is cleanup, **not notification** — it removes our only address for an Activity still running on a phone, so owner teardown end-pushes the affected rides BEFORE deleting them. No new error CODE: `invalid_request`, `auth_failed`, `permission_denied`, `not_found`, `conflict` and `internal_error` cover the surface. One new **`subCode`**, `reservation_expired`, is added to the shared `ErrorPayload.subCode` enum in [`schemas/ws-messages.schema.json`](schemas/ws-messages.schema.json) — an additive widening of an optional field on an unreleased version, so **contracts stays v0.24.0**; the generated TS/Swift unions gain one member and no wire shape changes. **Sections updated in this document:** §1 table of contents (entry 21), §6 endpoint catalog (two new rows), §7.21 (new — §7.21.1 register, §7.21.2 end, §7.21.3 content-state, §7.21.4 APNs envelope, §7.21.5 cadence + preference gate), and §4.1 (`error.subCode` enum gains `reservation_expired`). **In [`data-classification.md`](data-classification.md):** §1.18 (new table, 8 columns), §3.2 (`activity_push_token` row), §6 "By tier" and the count audit trail — **P0 141 → 148** (7 new P0 columns) and **P1 47 → 48** (one log-redaction-only token), with the log-redaction-only list growing 30 → 31 columns and the AES-256-GCM list unchanged at 17. **In [`docs/deployment.md`](../deployment.md):** the `LIVE_ACTIVITY_TICKER_ENABLED` row and the derived-topic note. | Claude (go-engineer) |
| 2026-07-30 | **Per-viewer share controls — the cumulative tier becomes per-grant editable flags, new §7.5.7 `PATCH /api/invites/{inviteId}` ([MYR-369](https://linear.app/myrobotaxi/issue/MYR-369)).** Contracts **v0.23.0**, migration **0024**. Until now a grant carried one `SharePermission` on a total order (`live` < `live_history` < `rides`), every gate compared with a `>=` over it, and §7.5 said in as many words that there was no edit-in-place endpoint — so an owner who wanted to stop ONE person requesting rides, while leaving them the live map, had exactly one move available: **revoke the grant and make them redeem a fresh code**. The tier could not express "paused" either; the only way to stop access at all was the permanent tombstone. An accepted grant now carries **independent, owner-editable flags** — `allow_rides` and `suspended_at` on `go_vehicle_shares` — surfaced as `ShareInvite.allowRides` / `ShareInvite.suspended` (accepted rows only, both **optional**, so every 0.22.0-era payload still validates) and edited through a **partial** `PATCH` whose body is `{allowRides?, suspended?}` with `minProperties: 1` and `additionalProperties: false`. **Absent ≠ false** — pointer-valued fields carry the distinction end to end, because a partial update that silently cleared the field it did not mention would be an access-control failure in both directions; an EMPTY body is `400`, never a `200` echo, since on this surface "I turned it off and it said OK" is the worst available outcome. **THE SUSPENSION INVARIANT, enforced server-side:** a suspended grant is excluded from the **viewer-merge access set** (`suspended_at IS NULL` — **six live occurrences across five files in two packages**, inventoried site by site in §7.5.0; the `auth`/`store` split is forced by the dependency rule, so the predicate is an invariant maintained by REPETITION and held up by convention plus tests rather than by construction), which is the single thing the §7.0 list, the §7.1 snapshot, the WS handshake and the §7.8 rides all resolve through — so one predicate kills all of them together and **no capability out-votes it** (`allowRides: true` on a suspended grant allows nothing). Consequences a consumer must know: a suspended grant produces **no `VehicleSummary` row at all** rather than a reduced one (there is deliberately no "suspended" marker to render); re-redeeming its code answers **404**, indistinguishably from an unknown or expired one, so the server never announces a suspension to the person it was applied to; and it **still serializes on the owner's own §7.5.2 listing**, which is the only place it appears and the only way to lift it. Suspension is the **reversible** alternative to §7.5.3 revoke — the row and its flags survive and one PATCH restores exactly what it held. **`live_history` IS RETIRED.** The "Live + history" capability is **removed from the product**: §7.2/§7.3/§7.4 are **owner-only again, unconditionally**, and no grant of any shape opens them — a legacy `live_history` grant loses the drives surfaces and **nothing else**. The enum member is **kept, not removed** (dropping it would break every installed decoder and make 0.23.0 a MAJOR bump), so it is **decodable and never emitted**: §7.5.1 still **accepts** it — an un-updated send-invite sheet must not start failing — and **persists it as `live`**, which is why a client MUST read the created row's `permission` rather than assume its input round-trips. **`permission` is now DERIVED on accepted rows** (`allowRides` → `rides`, else `live`) rather than stored, and the same derivation produces `VehicleSummary.sharePermission`; on a PENDING row it remains the invite-time **preset**, and **pending invites keep tier-at-redeem** — the mapping happens inside the accept `UPDATE` itself (`allow_rides = (permission = 'rides')`), atomically with the accept, so an invite minted before this change redeems to exactly what its preset always implied. The old `>=` instruction is **obsolete**; a `>=` over the two values still emitted happens to agree, which is what keeps un-updated clients correct. **THREE ENFORCEMENT LAYERS for the ride flag**, because the capability is now editable and an owner can withdraw it after a request exists: create (§7.8), the **owner-accept backstop** (the capability moving while a request sat unanswered), and the **reservation-dispatch probe** (`internal/dispatch/reservation_worker.go` — a reservation may sit accepted for DAYS, which is exactly the window a fixed tier did not have). Dispatch **holds** rather than expires and sits before the irreversible claim, so an owner who restores access inside the lateness window still gets the dispatch; accept **fails closed** on an unreadable grant, deliberately unlike the MYR-313 fail-open on an unreadable *vehicle*, because dispatching a car to somebody who may have just been suspended is not recoverable the way a retried accept is. **Cache bust targets the GRANTEE** on every successful patch (the MYR-184 bust-on-mutation pattern) — unconditional rather than conditional on which field moved, because for a suspension it is a security property: the cached access set is what the WS handshake consults, so a stale entry IS a live grant for up to the TTL. **Adversarial shape:** one conditional `UPDATE ... RETURNING` with `owner_user_id` and `status = 'accepted'` both ON THE WRITE (no read-then-write, no window), partial semantics expressed as `CASE WHEN $present THEN $value ELSE <column> END` rather than runtime-composed SQL, and **404-indistinguishability preserved** — missing, foreign and revoked are one body, so the endpoint cannot be an oracle for other people's invite ids; only "yours but still pending" is told plainly (`409`), because the caller demonstrably owns that row. The `permission` CHECK constraint is deliberately **unchanged** and still admits `live_history`: existing rows carry it, and tightening a CHECK under rows that violate it fails the migration outright. **Down-migration is a WIDENING and is documented as dangerous** — dropping the columns lifts every suspension and reverts every patched capability to its invite-time tier, unrecoverably; roll the application back and leave the columns in place instead. **DV-09 CAVEAT, recorded not closed:** suspension is enforced at the WS **handshake**, so a viewer on an already-open socket keeps receiving broadcasts until they reconnect — the same caveat §7.5.3(b) records for revoke, inherited rather than introduced, and tracked as [MYR-373](https://linear.app/myrobotaxi/issue/MYR-373). No socket teardown ships here. `data-classification.md` §1.15 updated (both columns P0), and its §6 "By tier" P0 count reconciled to the audit trail (139 → 141). **Sections updated in this document:** §2.3 (`PATCH` is no longer "NOT used in v1"), the §5 role table, §5.2.0 (viewer `sharePermission` now DERIVED — an earlier version of this entry said **§5.2.1**, which is the *snapshot* mask and was NOT touched by this change), §5.2.2–§5.2.4 (drive list / detail / route — owner-only), §5.2.5 (`inviteOwnerFields` enumeration + the PATCH role-access row), §6 endpoint catalog, §7.2–§7.4 RBAC, §7.5 (six endpoints, not five), §7.5.0 (including the six-site suspension-predicate inventory and the DV-09 caveat), §7.5.1, §7.5.2, §7.5.7 (new), §7.8 (create gate + the accept-path `403 vehicle_not_owned`). | Claude (go-engineer) |
| 2026-07-30 | **Invite codes become signed join LINKS — `ShareInvite.shareUrl`, new §7.5.6 ([MYR-368](https://linear.app/myrobotaxi/issue/MYR-368)).** Every `pending` row now carries `shareUrl` alongside `code`: `https://myrobotaxi.app/join/{code}?k={kid}.{expUnix}.{sigB64url}&from={from}&to={to}`. **Additive and optional** (contracts 0.21.0 → **0.22.0**) — a consumer that finds `code` with no `shareUrl` shares the bare code, and the field appears in exactly the same branch as `code`, so it is pending-only and absent on every accepted row. `k` is an **Ed25519** signature the **web join shell verifies statically** against a **compiled-in public key** — no database, no round trip — so a forged or edited link is bounced before the join page can act as an oracle against the 36^6 code space; the §7.5.5 rate limit only ever protected the API side. **Canonical signed payload: `join:{code}:{expUnix}:{from}:{to}`** — five colon-separated fields, always four colons, sanitized names, **empty string for an omitted name**. Every value the link carries is covered by the one signature, including BOTH display names, so somebody holding a genuine link cannot rewrite `from` to a name the recipient would trust. Names are reduced server-side to the **first whitespace token, ASCII letters only, capped at 20**, and the parameter is **omitted entirely** when nothing survives (accented and non-Latin names lose characters or drop out; consumers render generic copy, never a placeholder built from the code). The signed expiry is the row's **actual `expires_at`** in unix seconds — the same value the redeem predicate reads — not a re-derived TTL. **The signature does not mean the invite is live**: redemption (§7.5.5) stays the only authority, and a resend **re-signs** with the new code, the new expiry, and the current names, so the previous link dies with the code it embeds. Key management: private **seed** in the Fly secret `INVITE_LINK_SIGNING_KEY` (`openssl rand -base64 32`), public half via `ops invite-link public-key` (no DB, no network, runnable before the secret exists) plus a startup log line naming the key the **running** process loaded. **Startup fails fast** without the seed outside `--dev`, and on a malformed seed in any mode — the kill-switch precedent, because a keyless boot silently stops emitting `shareUrl` and nothing in the running system says so. The one-character **key id** in `k` is the rotation seam: the shell holds `1` and `2` at once while links signed under `1` age out over their 7 days. `shareUrl` is **P1 and bearer** exactly as `code` is (it contains it) and is listed in `inviteOwnerFields`; **no new persisted column** — the URL is derived per request, so `data-classification.md` §1.15 is unchanged. | Claude |
| 2026-07-30 | **Severing a Tesla link now REVOKES the grant at Tesla, not just our copy of the tokens ([MYR-366](https://linear.app/myrobotaxi/issue/MYR-366)).** Both severing paths gain an active, server-side `POST https://auth.tesla.com/oauth2/v3/revoke` (RFC 7009; `token`=stored **refresh** token, `token_type_hint=refresh_token`, `client_id`, **no client secret**) issued BEFORE the local delete — because the delete destroys the very credential the revoke presents, and after it only the owner can withdraw the grant by hand. **§7.6 account deletion:** a new step 2 in the [`data-lifecycle.md`](data-lifecycle.md) §3.1 ordering (the following steps renumber; both order tables were also brought back in line with the MYR-321 saved-places step, which had been added to the code but not to the tables), placed ahead of the vehicle teardown whose last-vehicle arm deletes the `Account` row and ahead of the identity transaction whose `User` cascade takes any that survives. **§7.12 per-vehicle teardown:** revokes on a **LAST-VEHICLE removal only** — the one removal that clears the tokens; a **mid-fleet** removal deliberately does NOT revoke, since the owner's remaining cars still need the link. The last-vehicle pre-check runs OUTSIDE the teardown's `FOR UPDATE` transaction (an HTTP call to Tesla must not be made while holding row locks over an owner's fleet); the store's locked count stays authoritative and a disagreement is logged, never fatal. **Best-effort throughout and NON-FATAL by construction** — no tokens on file, a DB read error, a network error, a Tesla 5xx and an already-invalid token are each a WARN and a continue; the revoker's signature returns a `bool`, not an `error`, so a failure cannot be propagated into a deletion. Tesla's availability MUST NOT be able to block a person's erasure of their own account. **Re-runnable:** a second run finds no stored token and makes no call. **No wire change on either endpoint** — §7.6 is still `204` with no body and §7.12's response is byte-identical, `revokeUrl` included and unchanged: revocation may have failed, so the honest client behaviour (offer the manual consent page) is the same either way and a `grantRevoked` field would only invite clients to hide a step still sometimes necessary. **Audit/logging:** no new `AuditLog` action; a P0 structured log line `event=tesla_tokens_revoked` carries the `user_id` and nothing else — never the token, its prefix, its length, or a VIN. Skipped entirely when no Tesla OAuth `client_id` is configured, so no unit test and no creds-less deployment can make an outbound revoke call. §7.6 gains a "Tesla grant revocation" subsection, its order table and "What is NOT deleted" rows are corrected, and §7.12's "OAuth revoke" note is rewritten (it previously stated there was no partner machine call). `data-lifecycle.md` §3.1 + §3.3 updated. Implementation: `internal/telemetry/{tesla_token_revoker,tesla_link_revocation,account_deletion_sequence,vehicle_teardown_handler}.go`, `cmd/telemetry-server/{wiring_vehicle_teardown,wiring_account_deletion}.go`. | go-engineer |
| 2026-07-30 | **Saved places become account state — §7.20 `GET /api/users/me/places`, `PUT`/`DELETE /api/users/me/places/{kind}`, and `go_saved_places` (migration 0023) ([MYR-321](https://linear.app/myrobotaxi/issue/MYR-321)).** Contracts **v0.21.0**. The same class of gap §7.19 closed, and the same kind of lie: the ride sheet's Home and Work shortcut chips have shipped since the sheet landed, backed by a private client-side `@State` struct — so "Home" meant one address on somebody's iPhone and a different one on their iPad, and a reinstall forgot both. **Account-scoped, not vehicle-scoped and not device-scoped:** a saved place belongs to a PERSON, so it follows them across every car they own, every car they are a viewer of, and every device they sign in on. Keying it by car would force a re-entry per vehicle AND put a home address on a surface a co-owner can read; **sharing a car grants access to the CAR, never to the other person's address book**, so these rows have no non-self reader and therefore no §5.2 mask entry (CG-DC-5 satisfied by statement, as §7.19 does). **TWO FIXED SLOTS, not a favourites list** — `home` and `work` are PEERS, not `SharePermission`-style cumulative tiers, and the enum IS the key space: it bounds the response at two rows, it is the `{kind}` path segment, and it is half the `(user_id, kind)` primary key. A user-named collection would need ordering, renaming, a create endpoint and a cap, none of which two chips need. Matched **case-sensitively** — `Home` is a `400`, not a synonym, because two spellings reaching an upsert whose conflict target is the exact bytes would give one person two homes, one of them invisible. **NO ROW MEANS NOT SET:** there is no tombstone and no null-coordinate placeholder, so a kind never set and a kind deleted are indistinguishable, and the list is a SPARSE array of 0–2 rows rather than a fixed pair with nullable members — a half-populated place is not a weaker place, it is not a place. The envelope is keyed `places` (not `items`), unpaginated for the same reason `ShareInviteListResponse` is and harder-bounded: `maxItems: 2`. An account that has saved nothing gets `{"places": []}`, a `200` and never a `404` — the state EVERY account is in on day one. **The `PUT` is a WHOLE-OBJECT UPSERT, the deliberate opposite of §7.19.2's partial body**, and the difference is the shape of the data rather than a style choice: five notification switches are genuinely independent, but a label and the coordinate it describes are ONE fact, and a partial write would let a client move the pin while keeping a label that no longer describes it. `200` on first write as well as replace, never `201` — the resource is the SLOT, which always exists in the URL space whether or not a row backs it — and the echo is scanned back OUT of the database through the decrypt path rather than reflected from the request. The body's `kind` is optional and redundant with the path, present only so a client holding a whole `SavedPlace` can post it back unstripped; a body disagreeing with the path is `400` and is **never** honoured over it, since a body that could redirect the write would let a stale client overwrite Home while its URL said Work. The `DELETE` is **always `204`, idempotent, and `404` is deliberately absent** — a retrying client must not be told its completed work failed, and a `404` would be an oracle for whether the account ever set that slot. It is a REAL delete, not a `go_vehicle_shares`-style tombstone: a revoked share is evidence in the CAR OWNER's audit trail, whereas a saved place is a person's own note to themselves. **ENCRYPTION FOLLOWS `go_ride_requests` EXACTLY (NFR-3.23):** `lat_enc`/`lng_enc` are TEXT holding base64 AES-256-GCM ciphertext, encrypt-only with no plaintext column, no dual-write window and no backfill; the repo panics on a nil `Encryptor` at construction and treats a decrypt failure as a hard read error, because there is no fallback column to fall back to. If anything on this platform justifies column encryption it is this table — a ride coordinate is where somebody went once, a saved home coordinate is where they SLEEP, and it is re-read on every launch. `label` stays P1 log-redacted but unencrypted, the same split 0002 makes between `pickup_lat_enc` and `pickup_label`. **Nothing here is ever logged** — no coordinate and no label on any path, and a `400` names the offending field without echoing its value, because error envelopes end up in crash reporters. Validation: `label` required/trimmed/non-blank/≤ 200 **runes** (not bytes, so a 200-character address in a non-Latin script is accepted), coordinates required, **finite** (`1e400` decodes to `+Inf` and would otherwise be sealed into ciphertext no reader can turn back into a place) and in range, decoded through POINTERS so an omitted key is distinguishable from an explicit `0` — `0` is a real coordinate and a plain `float64` would silently save a place off the coast of Ghana. **Account deletion (§7.6) gains a step 6**, before the identity transaction: `go_saved_places` has no FK so nothing cascades, and a row surviving the identity delete would be encrypted GPS of where a deleted person lives, keyed by a cuid nothing resolves and reachable only by a table scan. The audit row gains `savedPlacesDeleted` — a COUNT, never the places (CG-DL-5). No new error code: `invalid_request` and `auth_failed` cover the surface. P1 location throughout; `data-classification.md` §1.17. | Claude (go-engineer) |
| 2026-07-30 | **Notification preferences become real — §7.19 `GET`/`PUT /api/users/me/push-prefs`, `go_push_prefs` (migration 0022), and a per-category gate in the push notifier ([MYR-349](https://linear.app/myrobotaxi/issue/MYR-349)).** Fixes a LIE rather than a missing feature: both Settings screens have rendered a Notifications card since MYR-170 whose switches were backed by a private client-side struct — they moved, persisted nowhere and gated nothing — and MYR-186 then shipped the entire delivery pipeline with no preference layer, so an owner who had switched "Charging complete" off kept receiving it. Five categories (`rideLifecycle`, `driveStarted`, `driveCompleted`, `chargingComplete`, `viewerJoined`) on ONE row per person, because an account can be both owner and rider at once and the phone is the same phone; the NOTIFIER decides which column a given send answers to. `rideLifecycle` deliberately covers the whole ride status class — the rider's two switches both resolve to it, since no send site distinguishes `arrived` from `accepted`, and minting a column the server would never honour differently would be the same lie one layer down. The `GET` and the `PUT` echo ALL FIVE keys with no `omitempty` (dropping `false` would erase the interesting half of every value); the `PUT` body is PARTIAL, so an omitted key means LEAVE ALONE and two phones on two switches cannot clobber each other, and unknown keys are a `400` rather than a silent `200` that changes nothing. Enforcement is ONE gate on the single fan-out and it **fails OPEN** in all four failure modes — unwired store, lookup error, absent row, unknown category — because nothing about a ride waits on push, so failing closed would turn a database hiccup into platform-wide silence with no error anywhere. **Audit: only ONE of the five categories has a sender today.** `internal/push` has exactly three fan-out sites and all three are `rideLifecycle`; there is no drive-started, drive-completed, charging-complete or viewer-joined notifier in this service at all. The other four columns are created anyway because their switches are already on the owner's screen, so whichever notifier eventually sends them is born gated. P0 throughout. | go-engineer |
| 2026-07-30 | **Warn an owner before a ride-share pause breaks a confirmed reservation — `?upcomingForVehicle` on the owner feed, and `accepted → declined` for scheduled rides ([MYR-360](https://linear.app/myrobotaxi/issue/MYR-360)).** Today an owner who pauses ride sharing (§7.18) with an ACCEPTED FUTURE RESERVATION on that car breaks it silently: the sweeper **holds** at due time (its MYR-342 pause check runs before the claim), retries, and the ride resolves `reservation_expired` at the 30-minute lateness ceiling — so the rider learns nobody is coming **half an hour after their pickup time**. The hold-then-expire backstop **stays** (it covers crash and offline pauses); this makes the common path humane by letting the client warn the owner and offer an immediate decline. **Two additions, no new endpoint and no new route.** (1) **ONE optional query param on the EXISTING feed**, `GET /api/ride-requests/incoming?upcomingForVehicle={vehicleId}` — the same owner-scoped feed sliced differently: `owner_id` = JWT sub (**unchanged**, so no cross-owner read is expressible), plus `vehicle_id`, `status = 'accepted'`, and `scheduled_for IS NOT NULL AND scheduled_for > now()` (STRICT — a reservation already due belongs to the sweeper). The existing feed could not answer this: it is hardcoded to `requested` and has no vehicle filter, and `GET /api/ride-requests` is the *rider's* list. Ordered **`scheduledFor ASC, id ASC` — soonest first**, a deliberate departure from `createdAt DESC` that is **load-bearing**: the dialog names "the NEXT reservation", and under `createdAt DESC` a paginated cut could omit the soonest row. Cursor anchors on `(scheduledFor, id)` with the ascending row-value keyset `(scheduled_for, id) > (:cursorScheduledFor, :cursorId)`; **the opaque wire cursor format is unchanged** — still one base64 `{timestamp, id}` pair, two typed views of it (§4.2.2). Envelope, `limit` bounds and the in-statement `requesterName` resolution are all unchanged. **An unknown/unowned `vehicleId` is an EMPTY `200` page, never `403`/`404`** — the filter runs on the owner's own feed so it is already safe, and answering `404` would turn the param into an **existence oracle** for other people's cars; only a MALFORMED value is `400 invalid_request`. **Absent the param the endpoint is byte-identical**, pinned by a golden-body test. (2) **The §7.8 matrix's `accepted` → `declined` cell changes from `409` to the `decline` endpoint** — the first new legal edge since MYR-270 — narrowed to **SCHEDULED rides**; an accepted INSTANT ride still `409`s, because a car already dispatched to a rider on a sidewalk is a different situation. The narrowing rests on a row property immutable for a ride's lifetime (`scheduled_for IS NOT NULL` — reschedule moves the *instant*, never its existence), so the guarded `UpdateStatusFrom` write receives the ride's full **legal** from-set and the database still arbitrates every race (two concurrent declines → exactly one winner). Declining a reservation already PAST its `scheduledFor` is allowed on purpose — an explicit decline beats a silent `reservation_expired`, and the sweeper's claim re-checks `status = 'accepted'` so it simply matches no row. Accept, owner-only, and decline's freedom from every capability gate are unchanged. **MIGRATION-INDEX AUDIT: no migration and no index change required.** The change adds **no new status value** (`declined` has been in `go_ride_requests_status_check` since 0002) — only a legal EDGE into an existing **terminal** state, which appears in **neither** partial index (0004 covers `requested`/`accepted`/`enroute`/`arrived`; 0013 covers `accepted`/`enroute`/`arrived`), so the transition can only move rows OUT of a predicate, never in. Independently, the edge is scheduled-only and both indexes are partial on `scheduled_for IS NULL`, so every row it can touch is outside both entirely. Both arguments are individually sufficient. **Rider push copy fixed for the new edge:** `ride.status.changed` → rider fires as always, but a declined **reservation** now reads "*&lt;Vehicle&gt; can't make your scheduled ride*" instead of "*…can't take this ride*", which read as a reply to a request the rider had just made; per the standing rule it still **omits the time** (the server holds UTC and knows no client zone). `RideStatusChangedEvent` gains an internal `ScheduledFor` to carry that fact — **not** projected onto the summary `ride_status_changed` WS frame, which is unchanged. No schema change, no wire-shape change, no new error code. §4.2.2, §4.1.1.b, §6 and §7.8 updated. Implementation: `internal/telemetry/ride_request_{upcoming_handler,owner_decision,cursor}.go`, `internal/store/ride_request_upcoming.go`, `internal/push/copy.go`. | Claude (go-engineer) |
| 2026-07-29 | **The owner ride-share pause toggle — `rideShareEnabled` on `VehicleSummary` + `VehicleState`, a new §7.18 owner write, and a three-layer ride-request gate ([MYR-342](https://linear.app/myrobotaxi/issue/MYR-342)).** Contracts **v0.20.0**. Adds ONE optional P0 boolean to both read surfaces and one endpoint that writes it. Before this an owner had no way to say *"the car is fine, I am simply not lending it out right now"* — only to mark it `in_service` (a lie, and one MYR-313 lets scheduled rides through anyway) or to decline every request by hand, forever. Storage is `ride_share_enabled BOOLEAN NOT NULL DEFAULT true` on the Go-owned `go_vehicle_control_state` side table (migration **0021**) — deliberately non-nullable, deliberately outside `ControlStateUpdate` (a slot there would let a telemetry frame re-enable a paused car), with dedicated read/write statements modelled on the MYR-316 service-window writer. Emitted on every §7.0 row and on §7.1, in BOTH role masks: **the viewer is the party the value is about**, and a rider who cannot see it learns the car is paused only from a `409` after composing a whole request. **ABSENT means ENABLED** — never paused. Enforced in THREE layers (§7.8): create → `409 vehicle_unavailable`, accept → the same `409` as a backstop, reservation sweeper → **hold** before the irreversible claim, with the 30-minute lateness ceiling expiring anything never re-enabled. **Deliberate deviation:** all three apply to SCHEDULED rides, which MYR-313 exempts from the availability gate — a service visit ends, an owner's pause does not. The MYR-313 fail-open shape on scheduled accepts is preserved, so an unknown pause state never blocks. | Claude (go-engineer) |
| 2026-07-29 | **`command_failed` names the rejection reason ([MYR-329](https://linear.app/myrobotaxi/issue/MYR-329)).** A TestFlight owner's climate command was refused because his car was in service mode, but the app could only say "The car didn't accept that" — so he guessed at his battery. The reason was already in the building: `classifyResponse` has always parsed the proxy's `response.reason` (the car's own `ActionStatus.ResultReason.PlainText`, relayed by vehicle-command v0.4.1 `pkg/proxy/proxy.go:146`) into `TransportResult.Reason`, and the Executor put it on `CommandError.Detail` — a **server-log-only** field. The wire message stayed generic. §7.9 now documents that a `502 command_failed` whose reason matches a closed allow-list carries a canonical token in the free-text `error.message` (`vehicle command failed: vehicle_in_service`), with six tokens seeded: `vehicle_in_service`, `requires_user_acknowledgement`, `user_not_present`, `remote_access_disabled`, `low_battery`, `vehicle_busy`. **No schema, error-code, or field change** — `message` is free text per §4.1 and the code set is untouched; an unrecognized reason keeps the exact generic sentence, so every existing client is byte-identical. The car's firmware prose is never forwarded (it is unstable and untranslated): the message is assembled purely from server-side constants, so nothing upstream can reach a client, and the full sanitized reason is kept on the `vehicle command rejected` log line instead. Implementation: `internal/commands/reject_reason.go` (allow-list + message builder), `internal/commands/executor.go` (OutcomeFailed branch only — the other terminal outcomes keep their own precise copy), `internal/telemetry/vehicle_command_handler.go` (log the reason). | go-engineer |
| 2026-07-28 | **Push notification infrastructure — §7.17 device registry, an APNs sender, and ride-lifecycle notifications ([MYR-186](https://linear.app/myrobotaxi/issue/MYR-186)).** The first server→phone channel that does **not** require a live WebSocket. Every client signal before this arrived only while the app was foregrounded holding a socket — precisely the wrong assumption for the ride lifecycle, where the owner who must see an incoming request has their phone pocketed and the rider who must know their car has arrived is not staring at a map. **New Go-owned table `go_push_devices` (migration 0019**, `0019_push_devices.up.sql`): `id` (Go cuid), `user_id`, `device_token` (**UNIQUE**), `platform` (`CHECK IN ('ios')`), `sandbox`, `created_at`, `last_seen_at`; no Prisma FK (CG-DL-9). **`device_token` is P1** — a device identifier AND a capability, since anyone holding it plus the team's APNs key can push to that phone — stored raw with **log-redaction only** (never encrypted): the sender needs the exact bytes on every send, and the token is useless without `APNS_KEY_P8`, which a database-read attacker does not get from this table (`data-classification.md` §1.14, §3.2; only an 8-character prefix is ever logged). **New §7.17: `PUT` and `DELETE /api/push/devices`**, both user-scoped with **no vehicle in the path and therefore no ownership check** — the JWT subject IS the resource owner, the §7.6/§7.7 shape. **The upsert's conflict target is `device_token` ALONE, not `(userId, deviceToken)`, and that IS the design:** a token identifies a physical installation, so a phone handed over (or a second account signing in) must **RE-PARENT** the row; keying on the pair would leave two rows claiming one device and keep delivering the previous occupant's rides to the new occupant's lock screen. `DELETE` is caller-scoped **in the SQL** (`WHERE user_id = <caller>`) and idempotent — a miss is still a `200` with `unregistered:false`, a value that covers BOTH "already gone" and "belongs to someone else", deliberately indistinguishable so the endpoint cannot oracle whether an arbitrary token is registered and to whom. **The token is NEVER echoed** in a success body or an error envelope. **New `internal/push` sender:** ES256 provider JWT signed with the `.p8`, cached **40 minutes** (the middle of Apple's 20–60m window — younger is `TooManyProviderTokenUpdates`, older is `ExpiredProviderToken`), HTTP/2 over plain `net/http` (**verified against `api.sandbox.push.apple.com`, which answers `HTTP/2.0` — no `x/net/http2` dependency**), gateway chosen per the row's `sandbox` flag. Best-effort with a **one-retry** budget on 5xx/transport; **`410 Unregistered` and `400 BadDeviceToken` DELETE the row** — with no Prisma FK that self-healing feedback loop, not a cascade, is what keeps the table from accumulating dead installations — and `429` drops. **Three new bus consumers** on `ride.request.created` (→ owner), `ride.status.changed` (→ rider, on `accepted`/`declined`/`arrived` only) and `ride.due` (→ rider); every other transition is silent. Each handler hands the event to a bounded worker and **returns to the bus immediately**, so a slow APNs round-trip can never stall the ride's own WS broadcast behind it, and no send failure can touch the ride flow. **Payload policy — first names and vehicle nicknames ONLY:** a notification renders on a **LOCKED screen** to whoever holds the phone, so pickup/dropoff labels, addresses, coordinates and passenger phone numbers (§1.9 P1) **never** appear; `userInfo` is exactly `{"rideId":"…"}`. **Scheduled rides deliberately OMIT the time** — the server holds `scheduledFor` in UTC and knows no client time zone, so an absolute rendering would be either wrong ("5:30 PM" in the wrong zone) or unreadable ("Jul 31, 5:30 PM UTC"); the copy says only that the request is scheduled and correct local rendering belongs to the client. **At-most-once is NOT guaranteed and is accepted for v1:** `ride.status.changed` fires on every mutation including a reschedule sub-state change, which re-publishes the UNCHANGED main status, so an accepted-then-rescheduled ride can produce a second "Your ride is confirmed" — a duplicate is a minor annoyance, a miss is a rider on a sidewalk. (`ride.due` is exempt: its publisher holds a one-winner latch for the ride's lifetime.) **Runs KEYLESS and that is a supported state, not a failure:** without `APNS_KEY_P8`/`APNS_KEY_ID` the endpoints stay mounted, registrations persist, and each would-be send is logged `push skipped` — so the first deploy carrying the secrets reaches phones immediately instead of waiting for every app to relaunch. A key that is PRESENT but unusable fails fast, because then the operator believes push is on. New env: `PUSH_ENABLED` (bool, default `true`, `ParseBool` fail-fast), `APNS_KEY_P8` / `APNS_KEY_P8_B64`, `APNS_KEY_ID`, `APNS_TEAM_ID` (default `NFKX777598`), `APNS_TOPIC` (default `app.myrobotaxi.ios`). §7.17 added; §1 TOC + §6 catalog rows added; `data-classification.md` §1.14/§3.2/§6 and `docs/deployment.md` updated. Implementation: `internal/push/*`, `internal/store/{push_device_repo,vehicle_name}.go`, `internal/store/migrations/0019_push_devices.{up,down}.sql`, `internal/config/load_push.go`, `cmd/telemetry-server/wiring_push.go`. | go-engineer |
| 2026-07-28 | **Vehicle-details enrichment — `trimLabel` + `fsdVersion` on `/snapshot`, a Tesla-populated `color`, and a periodic in-service re-poll ([MYR-320](https://linear.app/myrobotaxi/issue/MYR-320)).** Contracts **v0.18.0**. Two OPTIONAL nullable **P0** string fields on `VehicleState` only — deliberately **NOT** on `VehicleSummary`, so unlike MYR-286/MYR-316 the list row is untouched: these are DETAILS-SHEET facts, and a catalog row that has to render six cars does not want them. **`trimLabel`** (`vehicle_data.vehicle_config.performance_package`, live-verified `"Performance"` on the owner's own car) is the DISPLAY-READY twin of `trim`, which stays the raw `trim_badging` badge code (e.g. `p74d`) for downstream classification — **both are kept, neither replaces the other, and only `trimLabel` may be rendered**: compose `<year> <model> <trimLabel>` and OMIT the label entirely when it is absent, never substituting `trim`, never re-casing it, never leaving a dangling separator. **`fsdVersion`** (live-verified `"FSD (Supervised) v14.3.5"`) comes from the **TITLE of the NEWEST `GET /api/1/vehicles/{vin}/release_notes` entry** — **no `vehicle_data` field and no proto carries it**, so that call is the only source Tesla exposes — and is DISTINCT from `softwareVersion`, the installed firmware build; the two move independently and neither derives from the other, so it is stored and emitted **VERBATIM** and consumers MUST NOT parse it, compare it ordinally, or gate a feature on a number extracted from it (its shape is Tesla's and may change). **Both are SNAPSHOT-ONLY:** no proto and no `fieldMap` entry — which is also exactly what carries them past the MYR-300 stream-recency gate — so **no fleet-config change** and a `vehicle_update` frame NEVER carries either, matching `trim` (MYR-279), `seatCoolingCapable` (MYR-308) and `serviceEstimatedEndAt` (MYR-316). Persisted by migration **0018** (`0018_vehicle_details_trim_label_fsd.up.sql`: `trim_label TEXT`, `fsd_version TEXT`, both nullable) on the Go-owned `go_vehicle_control_state` side table and LEFT-joined into `VehicleRepo.GetByID`; `null` means never read, never a fabricated value. **RBAC — BOTH roles:** added to `vehicleStateOwnerFields` in `internal/mask/tables.go`, with the viewer list inheriting them by the existing `removeField(…, "vin")` derivation — the identical treatment `trim` and `softwareVersion` already get, and both are P0 equipment/software facts about the car, so log-safe. **`color` gains a source without changing shape.** The field and its Prisma `Vehicle.color` column BOTH already existed and are unchanged — same type, same masks, same empty-string convention — but were never actually populated (the MYR-257 provisioning INSERT seeds `''`); they are now filled from `vehicle_data.vehicle_config.exterior_color` (live-verified `"Quicksilver"`) by the **FOURTH** sanctioned Go-side write carve-out on the Prisma-owned `Vehicle` table (`store.VehicleRepo.UpdateVehicleColor`, `internal/store/vehicle_color.go`), after MYR-257 provision, MYR-258 teardown and MYR-286 license-plate. That carve-out is the narrowest yet: **one column**, **owner-scoped in the WHERE clause**, UPDATE only, **no migration** (so CG-DL-9 does not fire — an application-runtime Prisma UPDATE is the sanctioned class), and an **EMPTY colour is NEVER written**, so a partial Tesla payload cannot blank a good value. **ZERO contract change to `color` itself** — only its provenance, recorded in `data-lifecycle.md` §1.3/§1.4. **Freshness:** all three reads are **non-waking** and ride the existing connectivity-edge path, now joined by a **periodic in-service re-poll** — a ~15m jittered ticker plus a startup pass, per-VIN debounced against the connectivity-edge reads and respecting the MYR-300 stream-recency gate — so a car that never flips a connectivity edge still acquires its details. Two new config knobs, validated at startup: `SERVICE_REPOLL_ENABLED` (bool, default `true`, `ParseBool` fail-fast) and `SERVICE_REPOLL_INTERVAL` (Go duration, default `15m`). §7.0 `color` row and §7.1 field table updated; `schemas/vehicle-state.schema.json` (before `lastUpdated`), `vehicle-state-schema.md` §1.1/§4, `data-classification.md` §1.3/§1.13, `data-lifecycle.md` §1.3/§1.4, and the `snapshot_completeness` fixture updated. Implementation: `internal/store/vehicle_color.go`, `internal/store/migrations/0018_vehicle_details_trim_label_fsd.{up,down}.sql`, `internal/mask/tables.go`. | go-engineer |
| 2026-07-28 | **The service window — `serviceEstimatedEndAt` on `VehicleState` + `VehicleSummary`, a new §7.16 owner write, and a scheduler bound ([MYR-316](https://linear.app/myrobotaxi/issue/MYR-316)).** Contracts **v0.17.0**. One OPTIONAL nullable **P0** RFC 3339 field answering the question an "In Service" badge cannot — *when do I get my car back?* — and, more consequentially, FLOORING the rider's scheduling picker. **Two columns, deliberately, added by migration 0017** (`0017_vehicle_control_state_service_window.up.sql`) to the Go-owned `go_vehicle_control_state` side table: `service_etc TIMESTAMPTZ` (Tesla's own estimate, from the Fleet API `GET /api/1/vehicles/{vin}/service_data` → `service_data.service_etc`) and `service_expected_end_at TIMESTAMPTZ` (owner-entered). Both nullable, both P0 — operational timing about the car, the same tier as the sibling `Vehicle.status`, so log-safe and unencrypted. **The wire value is `COALESCE(service_etc, service_expected_end_at)` — Tesla wins, the owner is the fallback — and the two-column split IS the design:** one merged column would let a late Tesla estimate ERASE what the owner typed, and would make a WITHDRAWN Tesla estimate fall back to `null` instead of back to the owner's answer. Both columns sit deliberately OUTSIDE the shared per-field COALESCE control-state upsert every other column in that table uses — that upsert **cannot express a NULL write** and clearing is first-class here — so they have dedicated writers in `internal/store/vehicle_service_window.go`. **Emission is gated on `status = 'in_service'`; every other status is `null`** — and the ServiceStatusMonitor ALSO physically CLEARS both columns when it observes the car leaving service, so the gate is belt-and-braces: a stale window can neither outlive the visit nor be resurrected by a status flip, and consumers never age the field out themselves. One shared resolver (`internal/telemetry/service_window.go`) serves the snapshot, the list, and the scheduler bound, so three readers cannot drift. **`null` is COMMON AND NORMAL, not a failure:** every field of Tesla's `service_data` response is nullable and an ALL-NULL body is the ordinary shape for a visit with no appointment record — it is not an error, not a fetch failure, and not a claim that the car is back. **Tesla read piggybacks, it does not poll:** it fires on the ServiceStatusMonitor's existing connectivity-edge path for an `in_service` vehicle, sharing the SAME per-VIN read debounce (`defaultServiceReadCooldown`, 45s) as the existing `GET /api/1/vehicles/{vin}` edge read and the MYR-260 `/vehicle_data` backfill, and reusing the token that read already resolved; non-fatal on error, leaving the last-known estimate. **New §7.16 `PUT /api/tesla/vehicles/{vehicleId}/service-window`** (body `{"expectedEndAt":"<RFC3339>"}`), owner-only: **four accepted spellings CLEAR** — absent key, explicit `null`, empty/whitespace string, empty body — and the value MUST be in the FUTURE, else `400 invalid_request` `expectedEndAt must be in the future` (a malformed timestamp gives `expectedEndAt must be an RFC 3339 date-time`). Ownership semantics are byte-identical to §7.12/§7.14 (unknown → `404 not_found`, indistinguishable from ownership-filtered; mismatch → `403 vehicle_not_owned`), non-`PUT` → `405 invalid_request`, store failure → `500 internal_error`. **That ownership check is load-bearing in a way §7.14's is not:** the plate `UPDATE` re-scopes `WHERE "userId" = <caller>` as a second enforcement, but the side table has **no `userId` column** (CG-DL-9, no Prisma FKs), so there is no owner-scoped SQL predicate to fall back on. The `200` body `{"vehicleId":"…","expectedEndAt":"…" or null}` echoes the **OWNER column, not the resolved `serviceEstimatedEndAt`** — echoing the resolved value would tell a client its write was overruled when it was merely outranked by Tesla on the next read. **Scheduler bound (create AND accept):** `POST /api/ride-requests` and `POST /api/ride-requests/{id}/accept` refuse a `scheduledFor` EARLIER than the vehicle's current `serviceEstimatedEndAt` with **`400 invalid_request`** — **no new error code**, and not a `409`: a reservation for a time the car provably cannot serve is a bad *request*, not an illegal transition or a capability conflict — message `scheduledFor must not be earlier than the vehicle's estimated service end (<RFC3339>)`. **Three rules:** a **null estimate ⇒ NO BOUND** (scheduling stays fully open; neither consumers nor the server may ever block on missing data), **EQUAL is ALLOWED** (strictly "earlier than"), and **INSTANT rides are unaffected** (no `scheduledFor` to compare; already gated by MYR-277's in-service `409 vehicle_unavailable`). **MYR-313 interaction, stated explicitly:** a scheduled ACCEPT now **does** read the vehicle, where [MYR-313](https://linear.app/myrobotaxi/issue/MYR-313) short-circuited before the read — but that read **FAILS OPEN for scheduled rides** (an unreadable vehicle leaves the reservation UNBOUNDED rather than refused), the exact inverse of the instant path's fail-closed `500`, so the MYR-313 stranding defect cannot return; MYR-313's exemption from the AVAILABILITY gate is **unchanged** (in_service/offline scheduled accepts still succeed). **[SUPERSEDED by MYR-372, 2026-08-02 — that read now fails CLOSED. See the newest row and §7.8; this clause is retained as the record of what shipped on this date.]** **RBAC — BOTH roles**, on `vehicleStateOwnerFields` AND `vehicleSummaryOwnerFields` in `internal/mask/tables.go` (viewer inherits by the existing `removeField` derivation): a rider needs the window for the same reason the owner does — it floors the picker, so withholding it would break the one consumer it was built for. **Delivery: NOT STREAMED** — Tesla has no proto for a service ETC, so there is no `fieldMap` entry, **no fleet-config change**, and a `vehicle_update` frame NEVER carries `serviceEstimatedEndAt`; it is snapshot/list-only and lands on the next §7.0 / §7.1 read. §7.16 added; §7 index + §6 catalog rows added; §7.0 + §7.1 field tables, examples and RBAC lines, the §7.1 blockquote notes, §5.2.0 / §5.2.1 mask matrices, §7.8 create + accept error lines, the accept bound + MYR-313 paragraphs, the transition-matrix bullet, and the §4.1.1.b emission audit all updated; `schemas/vehicle-state.schema.json` (before `lastUpdated`) + `schemas/vehicle-summary.schema.json`, the OpenAPI `VehicleSummary` component, `data-classification.md` §1.13 (two columns + note), `vehicle-state-schema.md` §1.1/§2.4/§4, and the `snapshot`/`vehicles_list*`/`snapshot_completeness` fixtures updated. Implementation: `internal/telemetry/{service_window,vehicle_service_window_handler}.go`, `internal/store/vehicle_service_window.go`, `internal/store/migrations/0017_vehicle_control_state_service_window.{up,down}.sql`, `internal/mask/tables.go`, `cmd/telemetry-server/wiring_vehicle_service_window.go`. | go-engineer |
| 2026-07-28 | **On-demand state refresh — new §7.15 owner endpoint ([MYR-315](https://linear.app/myrobotaxi/issue/MYR-315)).** New `POST /api/tesla/vehicles/{vehicleId}/refresh` (no body), the **pull half of an otherwise entirely push-based read surface**: the WebSocket carries only what the car volunteers, and an asleep / in-service / merely quiet car volunteers nothing, so §7.1 keeps returning the last frame it ever saw while the MYR-260 backfills — which fire on *connectivity edges* — never trigger for a car that simply sits parked. Without this endpoint a user looking at stale data has no action available at all. **Three-rung ladder, in order.** (1) **Freshness short-circuit:** a LIVE streamed frame inside the **120 s** MYR-300 stream-authoritative window answers `200 {"status":"fresh","lastUpdated":"<last frame>"}` with **zero Tesla calls and no cooldown consumed** — refreshing already-current data would only cost the owner battery, and a healthy car must never be rate-limited for being healthy. (2) **Cooldown:** otherwise one Tesla-hitting refresh per vehicle per **60 s**, else `429 rate_limited` + `Retry-After: 60`. (3) **Wake + ONE read:** wake under the **same bounded budget vehicle commands use** (§7.9 — 3 attempts, 2 s backoff) via the new `commands.Executor.EnsureAwake`, which shares `wakeStep` with `Execute`'s asleep branch so the two can never drift; **probe-first**, so an *online but quiet* car (the common case) costs one cheap `GET /api/1/vehicles/{vin}` and **no wake at all**; then exactly ONE `vehicle_data` read republished through the **existing MYR-260 mapping** so values land on the identical broadcast + persist path a streamed frame takes and live clients see an ordinary `vehicle_update`. Budget spent → `503 vehicle_asleep`; a car that slept between probe and read (Tesla `408`) reports the **same** `503` rather than a misleading `502`. **`seatCoolingCapable` and `trim` come along free** — the read includes `vehicle_config`, so an owner whose car has produced no connectivity edge since MYR-308 shipped can acquire the capability bit by tapping refresh, with no drive and no reconnect. **Ownership is byte-identical to §7.12/§7.14** (unknown → `404 not_found`, indistinguishable from ownership-filtered; real mismatch → `403 vehicle_not_owned`). **Cooldown is in-memory, per-process** — it resets on restart and is not shared across replicas (effective limit: N refreshes/min/vehicle under N replicas), a deliberate trade since the real backstop is the bounded wake budget plus the single-read shape; a failed refresh does **not** refund its token, so a client retrying a sleeping car backs off instead of re-waking it every second. **No new error code** (reuses `vehicle_asleep`, `rate_limited`, `invalid_request`), **no migration**, **no fleet-config change** (no new streamed fields), and **no schema change** — the endpoint returns a REST-only body, not a `VehicleState`. §7.15 added; §6 catalog + §7 index rows added. Implementation: `internal/commands/wake.go`, `internal/telemetry/{vehicle_refresh,vehicle_refresh_handler,service_status_stream_freshness,service_status_vehicle_data}.go`, `cmd/telemetry-server/wiring_vehicle_refresh.go`. | go-engineer |
| 2026-07-27 | **Reservation-time dispatch — a scheduled ride's pickup nav now fires at `scheduledFor`, not at accept ([MYR-179](https://linear.app/myrobotaxi/issue/MYR-179)).** v1 changes only **WHEN** the existing leg-1 (pickup) push runs. **No new lifecycle status, no new wire field and no wire-shape change** — the MYR-176 latch columns (`dispatch_status`/`dispatched_at`/`dispatch_error`) are reused unchanged; the only schema work is **migration 0016**, a partial index (no columns, no constraints). **Accept half:** the nav-dispatch pipeline no longer pushes when the accepted ride carries `scheduledFor`; it returns without touching the latch, leaving the row latch-unclaimed and outcome-absent (deliberately NOT `skipped` — that is the kill-switch outcome and would both misreport the ride and latch it out of the sweep). Observable on the wire as `dispatchStatus`/`dispatchedAt` staying **absent** after a scheduled accept until the reservation instant. **Instant accepts are unchanged.** **Sweeper half:** a new `internal/dispatch` `ReservationSweeper` ticks every 30 s over `scheduled_for IS NOT NULL AND status = 'accepted' AND dispatched_at IS NULL AND scheduled_for <= now` (oldest first, `LIMIT 25`; the sweeper's own clock is passed into the query rather than using `NOW()` so ONE clock governs both due-selection and the lateness deadline), then hands each candidate to its OWN small worker pool (2), where the **same** `dispatched_at` latch is claimed and the **same** push pipeline runs via a `runLeg` → claim + `runClaimedLeg` split. The pass itself claims **nothing**: the latch is stamped JUST IN TIME, inside the worker after the busy re-check and immediately before the push, so claim → outcome is bounded by the per-dispatch `OverallTimeout` and stays under the startup reconciler's 5-minute floor — a deploy mid-pass hands unreached candidates back rather than burning them. The sweeper's pool is separate from the dispatcher's, so a reservation backlog can never delay an instant accept's push (**instant dispatch behaviour is unchanged under any reservation load**). Reuse is the design: **exactly-once across replicas** (the latch admits one winner, so every server may sweep with no coordination) and **crash safety** come for free — a crash between claim and outcome leaves the identical `dispatched_at` set / `dispatch_status` NULL orphan the **existing leg-1 startup reconciler** already resolves; its query filters on the latch columns only and has never mentioned `scheduled_for`, so scheduled rows were already in scope and **it required no widening** (now pinned by a store test). **Lateness ceiling (30 min)** — evaluated FIRST, before the vehicle is even consulted: a reservation still undispatched past `scheduledFor + 30 min` is **claimed and failed honestly** `failed` / new internal code **`reservation_expired`**, without a push, whatever the car is doing. That covers both causes (car busy the whole window, and dispatch itself unavailable after downtime or a kill-switch window) with one code, and stops a stale reservation from dialling a car hours late — the MYR-176 "an honest, alertable outcome beats a late nav push" stance applied to the sweeper. The deadline is anchored on `scheduledFor`, so a restart mid-hold resumes rather than resets it and downtime buys no fresh window. **Vehicle-busy hold** — the reservation-time availability re-check MYR-313 deferred here: inside the ceiling and immediately before claiming, the worker tests the per-vehicle busy predicate (`scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')`, character-for-character `uq_go_ride_requests_active_instant_vehicle` and now SHARED in code with the `hasActiveRide` catalog flag so a third reader cannot drift). Busy → **hold** (no claim, no outcome, no push; retried next tick, so the ride dispatches the moment the car frees up); busy state unreadable → hold, never claim. Held rows are also filtered out of the selection window in SQL so they cannot head-of-line block younger dispatchable reservations out of the `LIMIT`, while past-ceiling rows are exempt from that filter and always surface for resolution. **Claim-time re-validation** — the reservation claim is its own guarded `UPDATE` re-checking `status = 'accepted' AND scheduled_for IS NOT NULL`, so a rider cancel or an owner picked-up landing between the sweep's SELECT and the claim LOSES: no push, no outcome, no event. The instant path's claim is deliberately left unguarded and byte-identical to MYR-176. **New internal `ride.due` seam** (`events.RideDueEvent`, ids + `scheduledFor` + `dueAt`): published once per due ride, and only when the push actually resolved **`sent`** — the topic means "your car is on the way", so kill-switched (`skipped`), token-failed, command-failed and expired reservations emit nothing (the latch admits one winner, so a false event could never be corrected). Internal-only (never broadcast, no REST surface), fire-and-forget and drop-safe, **no consumer yet** — a hook for the planned rider push notification. **New kill-switch `RESERVATION_DISPATCH_ENABLED`** (default true, `strconv.ParseBool` fail-fast like `DISPATCH_ENABLED`): false stops the sweeper entirely, leaving reservations unclaimed so re-enabling picks them up rather than having burned them; `DISPATCH_ENABLED=false` still records due reservations `skipped` exactly like instant accepts. **Indexing:** migration 0016 adds `idx_go_ride_requests_reservation_due`, a partial index over `scheduled_for` whose predicate is exactly the sweep query's three static conjuncts, so the every-30s pass on every replica stays an index probe as `go_ride_requests` accumulates terminal rows forever. **Accept-after-due:** `scheduledFor` is not floored to the future, so a reservation accepted after its instant dispatches on the next tick while inside the 30-minute ceiling and resolves `reservation_expired` beyond it — now documented. **v1 boundaries (both directions):** the busy predicate excludes scheduled rides (mirroring the index), so two reservations for one car at overlapping times both dispatch; and the model is one-directional — a car mid-RESERVATION is not busy to the instant path, so an instant accept can still re-point it. Both belong to booking-time conflict detection / an accept-side guard, not dispatch. §7.8 accept section, new "Reservation-time dispatch" subsection, `dispatch_error` code list, and transition-matrix note updated. Implementation: `internal/dispatch/{reservation_sweeper,reservation_dispatch,reservation_worker,dispatcher_leg}.go`, `internal/store/ride_request_reservation{,_queries}.go`, `internal/store/migrations/0016_ride_reservation_due_index.{up,down}.sql`, `internal/config/load_dispatch.go`, wired in `cmd/telemetry-server/reservation_wiring.go`. | go-engineer |
| 2026-07-27 | **Scheduled accepts are EXEMPT from the `vehicle_unavailable` availability gate ([MYR-313](https://linear.app/myrobotaxi/issue/MYR-313)).** `POST /api/ride-requests/{id}/accept` no longer consults the target vehicle's current status when the ride carries `scheduledFor` — the gate short-circuits before the vehicle read, so a status-lookup failure cannot strand a reservation either. **Why:** the MYR-277 gate asks "can this car be dispatched RIGHT NOW?", which is the question an INSTANT accept is asking and the wrong one for a reservation. A client could not confirm a Saturday 5:30 PM request because the car was in service that afternoon (`409 vehicle_unavailable`, "Vehicle is in service and can't be dispatched"). This restores consistency with the two guards the gate is the analogue of — the per-rider `uq_go_ride_requests_active_instant_rider` and the per-vehicle `uq_go_ride_requests_active_instant_vehicle` (MYR-266), **both partial on `scheduled_for IS NULL`** — and with what §4.1.1 already stated ("scheduled rides are exempt from both"): the docs were already correct and the MYR-277 status half was the outlier. **Instant behaviour is unchanged** — `in_service`/`offline` still `409 vehicle_unavailable`, still fails closed on an unreadable status, still never gates decline. **No schema, wire, or migration change** (the exemption is a predicate on an existing field). **Reservation-time availability is still required** and remains the scheduled-dispatch machinery's precondition ([MYR-179](https://linear.app/myrobotaxi/issue/MYR-179)) — it must re-check the vehicle when the reservation comes due; accepting a reservation is not dispatching it. §7.8 accept section + error list, §4.1.1 `vehicle_unavailable` row and §4.1.1.b note, and the §7.8 transition-matrix note updated. Implementation: `internal/telemetry/ride_request_owner_handler.go` (`rejectIfVehicleUnavailable`). | go-engineer |
| 2026-07-27 | **Owner-entered `licensePlate` — new §7.14 write endpoint + both-role read exposure ([MYR-286](https://linear.app/myrobotaxi/issue/MYR-286)).** `VehicleState` and `VehicleSummary` gain one OPTIONAL P1 string (contracts v0.15.0, `myrobotaxi/contracts#22`) so a rider can identify the correct car at pickup. **Not a Tesla value:** the Fleet API exposes no plate on any endpoint, telemetry field, or proto, so the column is populated ONLY by the owner via the new **§7.14 `PUT /api/tesla/vehicles/{vehicleId}/plate`** (body `{"plate":"…"}`). That endpoint **normalizes then validates, in that order** — trim + uppercase, then ≤ 10 chars and `^[A-Z0-9 -]*$` against the NORMALIZED value, so `"  abc 1234  "` is accepted as `"ABC 1234"` while a charset/length violation is `400 invalid_request` with the P1 value never echoed. An **empty string CLEARS** the plate (an ordinary write of `''`, not a separate verb). Ownership semantics are identical to §7.12: unknown vehicle `404 not_found` (indistinguishable from ownership-filtered), real mismatch `403 vehicle_not_owned`, and the `UPDATE` re-scopes `WHERE "userId" = <caller>` so a zero-row write returns 404 rather than a false 200. **RBAC — deliberately BOTH ROLES** (§5.2.0 / §5.2.1): unlike the sibling `vin`, which [MYR-279](https://linear.app/myrobotaxi/issue/MYR-279) gated to owners, the plate is in the owner AND viewer allow-lists, because a plate only a rider cannot see is useless for its one purpose. This REPLACES the pre-existing forward-looking owner-only mask rule that was written before the field was on the wire; the viewer VehicleState list is now owner-minus-`vin` and nothing else. **Empty-value convention:** the read paths emit `licensePlate` exactly like the sibling `color` — a plain non-`omitempty` string, so the key is ALWAYS present and "no plate set" is an EMPTY STRING; an ABSENT key means a pre-MYR-286 server. Neither means "we could not read the plate" — consumers keep the `VIN ····xxxx` fallback for both and never render an empty plate. **No migration** — the column already exists (`TEXT NOT NULL DEFAULT ''`); the write is a narrow Go-side UPDATE carve-out on the Prisma-owned `Vehicle` table, the third after MYR-257 provision / MYR-258 teardown, recorded in `data-lifecycle.md` §1.4 (CG-DL-9 governs migration SQL, not application-runtime UPDATEs). **No WebSocket delta in v1** — a `vehicle_update` never carries the field and an edit fires no push, so it lands on the next §7.0 / §7.1 read. §7.14 added; §7.0 + §7.1 field tables, examples and RBAC lines updated; §5.2.0 / §5.2.1 / §5.3 mask matrices corrected; `schemas/vehicle-state.schema.json` (before `lastUpdated`) + `schemas/vehicle-summary.schema.json` (appended), the OpenAPI `VehicleSummary` component + snapshot `x-role-masks`, `data-classification.md` §1.3 wire-exposure row, and the `snapshot`/`vehicles_list*` fixtures all updated. Implementation: `internal/telemetry/{vehicle_plate_handler,license_plate,vehicle_snapshot_types,vehicles_list_handler}.go`, `internal/store/{vehicle_plate,queries,types,vehicle_repo_scan,vehicle_repo_list}.go`, `internal/mask/tables.go`, `cmd/telemetry-server/wiring_vehicle_plate.go`. | go-engineer |
| 2026-07-27 | **`/snapshot` `seatCoolerLeft`/`seatCoolerRight` are now contracted as the ventilated-seat CAPABILITY signal ([MYR-299](https://linear.app/myrobotaxi/issue/MYR-299)).** **No response-shape change** — both fields have been persisted and returned here since [MYR-273](https://linear.app/myrobotaxi/issue/MYR-273) (migration 0010). What is new is the documented inference consumers may draw from them: a car without ventilated seats never emits Tesla protos 237/238, so **presence** (`!= null`, **including `0`**) means "this car has cooled seats" and absence means it does not. That makes the existing absent-vs-null discipline load-bearing rather than cosmetic: a persisted `0` MUST serialize as the JSON number `0` (never omitted, never `null`) or a vented car with both seats off reads as heat-only, and a never-read value MUST serialize as an explicit `null` (never a fabricated `0`) or every car advertises cooled seats it may not have. Both directions are now pinned by handler tests. Companion server change: the two fields gain `ResendIntervalSeconds: 120` in `DefaultFieldConfig` so presence is continuously re-asserted (Tesla emits them on change only) — **applies to a car only on a fleet-config re-push**. See `vehicle-state-schema.md` §1.1. | go-engineer |
| 2026-07-27 | **`/snapshot` now returns the media now-playing block + `seatCoolingCapable` ([MYR-303](https://linear.app/myrobotaxi/issue/MYR-303), [MYR-308](https://linear.app/myrobotaxi/issue/MYR-308)).** Contracts v0.16.0. MYR-303 adds eight NEW streamed fields — `mediaNowPlayingTitle` (proto 248), `mediaNowPlayingArtist` (247), `mediaNowPlayingAlbum` (249), `mediaNowPlayingStation` (250), `mediaPlaybackSource` (243), `mediaNowPlayingDurationMs` (245), `mediaNowPlayingElapsedMs` (246) and `mediaVolumeMax` (252) — subscribed in `DefaultFieldConfig` at a 10s interval WITH a 120s `ResendIntervalSeconds` (the MYR-300 lesson: Tesla emits the Media group on change only, so a server reconnecting mid-track never re-learns the block, and the near-constant `mediaVolumeMax` might never be re-emitted at all). Two names contract at the `fieldMap` boundary exactly as MYR-252 contracted `MediaAudioVolume`→`mediaVolume`: `MediaAudioVolumeMax`→`mediaVolumeMax`, plus an explicit `Ms` suffix on duration/elapsed because the contract fixes the unit. MYR-308 adds `seatCoolingCapable`, decoded from REST `vehicle_data.vehicle_config.has_seat_cooling` — a SPEC fact (is the car equipped with cooled seats?), not the runtime `seatVentEnabled`. Migration **0015** adds all nine columns to the Go-owned `go_vehicle_control_state` side table (five `TEXT`, two `BIGINT` ms counters, one `DOUBLE PRECISION`, one `BOOLEAN`), fed by the same persist path and read through the same `GetByID` LEFT JOIN. **Empty-string semantics diverge deliberately from MYR-298:** for the five free-text fields an empty value means the track ENDED and OVERWRITES a known title (persisted as `''`), whereas MYR-298's enum `mediaPlaybackStatus` drops `"Unknown"`/empty — an enum's Unknown means "could not read", a blank title is a real report. `""` and `null` are therefore distinct on the wire and MUST NOT be coalesced. **Classification:** the five text fields are the first **P1** rows in that table (free-text user content revealing listening habits) — redact in logs, both roles, and EXCLUDED from the FR-5.5 `limited_viewer` seam; the three numerics and `seatCoolingCapable` are P0. **MYR-300 interaction:** `seatCoolingCapable` has no proto and so no `fieldMap` entry, which is what carries it past the stream-recency gate — an in-service car acquires it on the connectivity-edge `/vehicle_data` read with no stream at all (same REST-only path as `trim`). No `endpoints=vehicle_config` parameter was needed; Tesla's default response already carries the sub-object. §5.2.1 mask rows (owner, viewer, `limited_viewer`) and §7.1 delivery notes updated; `data-classification.md` §1.13 gains nine columns (5 P1 + 4 P0) and the tier-summary audit trail. **Operational:** a fleet-config re-push per VIN is required after deploy before any car emits the eight new streamed fields. | go-engineer |
| 2026-07-27 | **`/snapshot` now returns `seatVentEnabled` + `mediaPlaybackStatus` ([MYR-298](https://linear.app/myrobotaxi/issue/MYR-298)).** Both are contracted `vehicle_update` fields (MYR-252) that were neither persisted nor emitted on the DB-backed `/snapshot`, so a client that missed the live WS frame could never learn them — a backgrounded phone, a sleeping car, or any socket drop lost the value permanently (NFR-3.5). Migration **0014** adds two nullable columns to the Go-owned `go_vehicle_control_state` side table — `seat_vent_enabled BOOLEAN`, `media_playback_status TEXT` — fed by the SAME live persist path as the MYR-269/273/274 siblings (`mapTelemetryToControlState` → `VehicleUpdate.ControlState` → writer flush → per-field `COALESCE` upsert) and read through the same `VehicleRepo.GetByID` LEFT JOIN. **No new wire fields and no schema shape change** — both names already exist in `vehicle-state.schema.json` and in `internal/mask/tables.go` `vehicleStateOwnerFields` (since MYR-252), so this is purely a delivery-channel addition: WS-live-only → also snapshot-backed. **Absent-vs-null:** the owner projection ALWAYS carries both keys; a never-read value is an explicit `null` (honest-unknown), never an omitted key and never a fabricated `false`/`"Stopped"` — identical to the MYR-274 siblings. A streamed `mediaPlaybackStatus` of `"Unknown"`/empty persists NULL and never overwrites a known status (same discipline as `"Unknown"` `hvacAutoMode`/`hvacPower`). **Deliberately NOT on the MYR-260 `/vehicle_data` backfill path** — Tesla's cached `vehicle_data` climate subset carries neither value, so there is nothing to backfill from, and their absence there keeps them clear of the backfill-overwrites-fresher-stream bug tracked in [MYR-300](https://linear.app/myrobotaxi/issue/MYR-300). §5.2.1 mask row and §7.1 delivery note updated; `data-classification.md` §1.13 gains both columns (P0). This completes the [MYR-253](https://linear.app/myrobotaxi/issue/MYR-253) hydration — 20 of 21 cabin read-backs are now snapshot-backed; only `hvacPower` stays WS-live-only (its derived `isClimateOn` is persisted). | go-engineer |
| 2026-07-26 | **`GET /api/vehicles` rows now carry `hasActiveRide` ([MYR-233](https://linear.app/myrobotaxi/issue/MYR-233)).** The catalog gains one OPTIONAL P0 boolean (contracts v0.14.0, `myrobotaxi/contracts#21`) answering "is this car serving a ride right now?" so a rider can render a Busy state and route new INSTANT requests to the scheduling flow instead of hitting a `409 ride_active` on accept. **Derivation mirrors the accept guard exactly:** `true` iff a `go_ride_requests` row exists for the vehicle with `scheduled_for IS NULL AND status IN ('accepted','arrived','enroute')` — character-for-character the predicate of the per-vehicle partial unique index `uq_go_ride_requests_active_instant_vehicle` (migration 0013, [MYR-266](https://linear.app/myrobotaxi/issue/MYR-266)). That reuse is the point: flag true ⇒ an accept would raise `23505` on that index; an accept conflicts ⇒ flag true. The index also bounds the match at one row per vehicle. Scheduled rides are EXEMPT and `requested` does NOT count (many riders may hold pending requests against one idle car; terminal states free it). **No migration and no new column** — the flag is derived read-time as a correlated `EXISTS` folded into the existing lean list query (`internal/store/queries.go` `queryVehiclesByUserList`), so the catalog still costs ONE statement (no N+1) and Postgres answers each probe from the partial index. §7.0 field table + example, §5.2.0 mask matrix (in BOTH role allow-lists — operational state, not owner-curated data), `schemas/vehicle-summary.schema.json`, the OpenAPI `VehicleSummary` component, and both `fixtures/rest/vehicles_list*.json` updated. **v1 caveat:** REST-read-time only — no WebSocket push of this flag to non-party viewers, so a Busy badge refreshes on the next list fetch. **Absence semantics:** this server always emits `true`/`false`; a missing key means a pre-MYR-233 server and MUST be read as "availability unknown → treat as available", never as Busy. No data-classification change (P0 derived state, same tier as the sibling `status`; no persisted column added). Implementation: `internal/store/{queries,vehicle_repo_list}.go`, `internal/mask/tables.go`, `internal/telemetry/vehicles_list_handler.go`, `cmd/telemetry-server/adapters.go`. | go-engineer |
| 2026-07-25 | **Re-add a previously removed car ([MYR-262](https://linear.app/myrobotaxi/issue/MYR-262)).** Wires the MYR-261 `store.RemovedVehicleRegistry.ClearTombstone` (previously with no runtime caller — a removed car was a permanent trap) to a deliberate re-add path. New owner-authenticated endpoint §7.13 `POST /api/tesla/vehicles/{teslaVehicleId}/re-add`: it clears the caller's OWN removed-vehicle tombstone **before** provisioning (the deliberate-vs-passive seam — the passive `ownerStreamHook.AfterLink` sync **never** clears a tombstone), then best-effort re-provisions the single owned car matching `teslaVehicleId` through the same shared per-vehicle path the passive sync uses (Fleet list → OWNER filter → `UpsertOwnedVehicle` → stream-config push). Path key is the **Tesla vehicle id** (the local `Vehicle` row is gone at re-add time). Fail-closed ownership at two layers: `ClearTombstone` is owner-scoped (`WHERE user_id = caller`, can't clear another user's tombstone), and the re-provision provisions only an OWNER-access match in the caller's own fleet. Idempotent (`wasTombstoned:false` no-op) and best-effort (`provisioned:false` when tokens were cleared on a last-vehicle removal — the next re-link's passive sync then provisions the car). Response `{readded, wasTombstoned, provisioned}` (all P0). Operator stopgap: `ops vehicles re-add --user-id <id> --tesla-vehicle-id <id>` clears a tombstone out-of-band over the same registry. No schema change (tombstone table + `ClearTombstone` + `vehicle_readd_allowed` audit shipped in MYR-261). No live Tesla call fires in tests/CI (the inline provisioner's pusher is nil unless the proxy is configured). See §7.13, §7.12, `data-lifecycle.md` §1.4.1. | go-engineer |
| 2026-07-25 | **MYR-270: owner-driven dispatch v2 — `picked-up`/`start`/`dropped-off`, retire `board` + drive-end auto-completion.** Supersedes the MYR-265 auto-leg model (§7.8). Removed `POST /api/ride-requests/{id}/board` and the `internal/ridecomplete` drive-end `enroute → completed` auto-completion. Added three guarded, idempotent endpoints over the existing `RideRequestStatus` enum (no new values): `POST .../picked-up` (**owner**, `accepted → arrived`, no nav), `POST .../start` (**rider**, `arrived → enroute`, fires the leg-2 **dropoff** nav push — the `ride.started` seam, renamed from `ride.boarded`), `POST .../dropped-off` (**owner**, `enroute → completed`, no nav). The rider cannot `start` before the owner confirms pickup (start legal only from `arrived`). New nullable `picked_up_at` audit column (migration 0009, P0, off-wire — data-classification.md §1.9); `enroute_at` is retained as a lifecycle timestamp only (no longer feeds drive-end correlation). Updated the §7.8 endpoint table, authorization model, transition matrix (strict linear chain `requested → accepted → arrived → enroute → completed`), and dispatch-outcome prose. No schema/wire-shape change. | go-engineer |
| 2026-07-25 | **Removed vehicles no longer resurrect on re-link ([MYR-261](https://linear.app/myrobotaxi/issue/MYR-261)).** The §7.12 teardown now writes a per-owner **removed-vehicle tombstone** (`go_removed_vehicles`, Go-owned migration `0006`, composite PK `(user_id, tesla_vehicle_id)`, no Prisma FK — CG-DL-9) in the **same transaction** as the `Vehicle` delete (recorded on `vehicle_deleted.metadata.tombstoned`). The post-link vehicle sync (`store.OwnerProvisioner.UpsertOwnedVehicle`, new `skipped_tombstoned` outcome) skips any tombstoned `(owner, teslaVehicleId)`, so a passive Tesla re-link can no longer re-insert a car the owner removed — the root-cause fix for the reappearance bug. Tombstone-wins is the safe default (the bulk `AfterLink` sync cannot distinguish deliberate re-add from incidental re-link); a **deliberate re-add** is an explicit action that first clears the tombstone via `store.RemovedVehicleRegistry.ClearTombstone`, which writes the new user-initiated `vehicle_readd_allowed` AuditLog row (§4.2). No REST route clears tombstones yet — the re-add UI is the app-side half; the backend exposes `ClearTombstone` as the sanctioned entry point. See §7.12 and `data-lifecycle.md` §1.4.1. | go-engineer |
| 2026-07-25 | **MYR-265: rider `POST /api/ride-requests/{id}/board` (accepted→enroute) + autonomous two-leg dispatch.** Added the board endpoint (§7.8): rider-only, idempotent (`enroute` re-board is a 200 no-op), guarded `accepted → enroute`, publishing the internal `ride.boarded` seam that fires the leg-2 **dropoff** nav push. Added `enroute → completed` via drive-end detection, **leg-correlated** on a new `enroute_at` board timestamp (only a drive started at/after board completes the ride — a delayed leg-1 pickup drive-end cannot false-complete). New columns `dropoff_dispatch_status`/`dropoff_dispatched_at`/`dropoff_dispatch_error`/`enroute_at` (migration 0007, P0, off-wire; data-classification.md §1.9). Updated the §7.8 endpoint table, transition matrix, and dispatch-outcome prose. No schema/wire-shape change (existing `RideRequestStatus` enum reused). | go-engineer |
| 2026-07-24 | **Owner car offboarding — full teardown ([MYR-258](https://linear.app/myrobotaxi/issue/MYR-258)).** New owner-authenticated endpoint §7.12 `DELETE /api/tesla/vehicles/{vehicleId}` ("Remove this car"), the exact inverse of the §7.11 onboarding link. Backed by a new `store.OwnerTeardown` writer — a single, transactional, idempotent, audited owner-scoped teardown that mirrors MYR-257's `store.OwnerProvisioner` in reverse: it DELETEs one `Vehicle` (`WHERE "id"=… AND "userId"=…`, cascading `Drive`/`TripStop`/vehicle-scoped `Invite` + encrypted route blobs and firing the existing `vehicle_deleted` NOTIFY that closes WS/mTLS streams + evicts caches) and, on the owner's **last** vehicle, DELETEs the Tesla `Account` tokens + resets `Settings` (`teslaLinked`/`virtualKeyPaired=false`), writing the user-initiated `vehicle_deleted` AuditLog row in the same transaction (CG-DL-3; P0 `{driveCount, wasLastVehicle, tombstoned}` metadata). Ownership is enforced at the SQL layer (owner-scoped predicate on every mutating statement) — a teardown can never touch another user's rows. Sequence: resolve token → best-effort `DELETE fleet_telemetry_config` at Tesla (new `FleetAPIClient.DeleteTelemetryConfig`, a near-copy of `GetTelemetryConfig` — same forwarded/unsigned proxy path, NOT the JWS-signed POST push; non-fatal on failure) → authoritative local teardown → return the honest post-state + owner-only follow-ups (the consent-revoke deep link and the manual virtual-key-removal steps; neither is a partner machine call — car-offboarding.md §1.2/§1.3). Idempotent (re-remove is a clean no-op, no duplicate audit). `data-lifecycle.md` §1.4 grants the Go server owner-scoped DELETE on `Vehicle`/`Account` + the `vehicle_deleted` audit INSERT; §4.2 `vehicle_deleted` action now emitted by Go. No live Tesla call fires in tests/CI (the config-deleter is nil/faked). | go-engineer |
| 2026-07-24 | **Fully self-serve owner onboarding — provision on Tesla link ([MYR-257](https://linear.app/myrobotaxi/issue/MYR-257)).** §7.11.2 `GET /api/tesla/link/callback` behavior change: on a successful code→token exchange the callback now PROVISIONS the caller's minimal Prisma owner rows instead of requiring a pre-seeded `Account`. New `store.OwnerProvisioner` (a single, audited, idempotent, transactional writer — NOT the identity module, keeping ADR-001 §4's identity/`User`-read-only boundary intact) **resolves the canonical Prisma user** — (a) existing Tesla-`Account` owner (never rewritten; caller's Apple identity converges via a `go_identity_apple` re-point), (b) existing `"User"` by email (adopted, preventing a `User.email @unique` collision on the Apple Hide-My-Email crossover), or (c) fresh `"User"` on the go_users id — then upserts `"Settings"` (`teslaLinked=true`) and `"Account"` (`ON CONFLICT (provider, providerAccountId) DO UPDATE` tokens only, never `userId`; dual-write-encrypted; `providerAccountId` = Tesla OIDC `sub` from a new `teslaauth.FetchUserInfo` call) for that canonical user, in one transaction — never duplicating a user or reassigning a Tesla account/vehicle across users. A brand-new go_users-native Apple user becomes a working owner with **zero ops steps** (`AUTH_APPLE_BOOTSTRAP` + pre-seeded-`Account` MVP steps deleted); a denied/failed link provisions nothing (no orphan `"User"`). A best-effort post-link hook lists the owner's vehicles (Fleet `GET /api/1/vehicles`, new `FleetAPIClient.ListVehicles`), seeds `"Vehicle"` identity rows **for owned vehicles only** (Fleet `access_type == "OWNER"`; shared-driver cars skipped + audited, and a `teslaVehicleId` owned by another user is never reassigned), and — only when the tesla-http-proxy is configured at runtime — pushes the fleet-telemetry config per VIN so the car streams without an ops `fleet-config push` (guarded so no live push ever runs in tests). `account_not_provisioned` is now legacy (unreachable on the happy path); `persist_failed` widens to cover userinfo/transaction failure. Data-lifecycle §1.4 updated: `User`/`Settings`/`Vehicle` gain narrow Go INSERT/upsert access, `Account` widens to upsert. Cross-repo Prisma-schema verification is a required `sdk-architect` gate — see [`../architecture/self-serve-onboarding.md`](../architecture/self-serve-onboarding.md). No new endpoint or request/response shape; no new persisted column. | go-engineer |
| 2026-07-24 | **Registered the owner-app charge-port, seat-climate, and media commands ([MYR-249](https://linear.app/myrobotaxi/issue/MYR-249)).** §7.9 command catalog gains eight signed commands the owner app needs: `charge_port_door_open`/`charge_port_door_close` (`vehicle_charging_cmds`, no params), `remote_seat_heater_request` (`seat_position` int 0–8, `level` int 0–3), `remote_seat_cooler_request` (`seat_position` **1 front-left / 2 front-right** — front seats only, a different numbering from the heater; `seat_cooler_level` **int 1–4**: 1 off/2 low/3 med/4 high, asymmetric with the heater's 0–3 `level` because the proxy's `Level(x-1)` and `SetSeatCooler`'s `+1` cancel to a 1:1 map onto the `HvacSeatCoolerLevel` enum), `media_toggle_playback`/`media_next_track`/`media_prev_track` (no params), and `adjust_volume` (`volume` float 0–11). All are `SignerRequired: true` and route the proxy path. **MYR-245 lesson applied:** every one was verified present in the **pinned** tesla-http-proxy (v0.4.1) `ExtractCommandAction` switch (and confirmed signed, not `ErrCommandUseRESTAPI`), so none 400s locally as `invalid_command` before the car is dialed — no proxy upgrade required. Body builders validate seat position/level and volume ranges (out-of-range → `400 invalid_request`, no Tesla call) following the existing `buildSetTemps`/`paramInt` patterns. **No stored-data / data-classification change** — command params are transit-only, never persisted or logged (VIN still redacted to last-4). Implementation: `internal/commands/registry.go` (entries + `buildSeatHeater`/`buildSeatCooler`/`buildAdjustVolume`) + `registry_test.go`. | go-engineer |
| 2026-07-23 | **User-facing in-app Tesla OAuth link — owner onboarding ([MYR-246](https://linear.app/myrobotaxi/issue/MYR-246)).** New §7.11: `POST /api/tesla/link/start` (owner-authenticated; mints a 10-min single-use PKCE+state session bound to the caller and returns the Tesla authorize URL — full scope set `openid offline_access user_data vehicle_device_data vehicle_location vehicle_cmds vehicle_charging_cmds` + `prompt_missing_scopes=true`, MYR-242) and `GET /api/tesla/link/callback` (unauthenticated Tesla redirect; validates `state`, exchanges the code, persists tokens via the existing encrypted `AccountRepo.UpdateTeslaToken` dual-write, then `302`s to the `myrobotaxi://tesla-linked?status=…` app deep link — no tokens/PII in the URL or logs). Lets the iOS app link a Tesla account in-app (`ASWebAuthenticationSession`) instead of the localhost `ops auth link` dev flow; the `client_secret` stays server-side. OAuth primitives factored out of `cmd/ops/auth_oauth.go` into shared `internal/teslaauth` (PKCE, authorize URL, code→token exchange); endpoints in new `internal/teslalink` (single-use TTL session store + handler). New config `TESLA_LINK_REDIRECT_BASE_URL` (enables the surface; its `/api/tesla/link/callback` must be registered on the Tesla app) + `TESLA_LINK_APP_REDIRECT` (default `myrobotaxi://tesla-linked`). No new persisted fields, WS messages, or error codes (start uses `auth_failed`/`internal_error`; callback redirects rather than emitting the error envelope). MVP requires the callee `Account` row to be pre-provisioned (ops step, bound via `AUTH_APPLE_BOOTSTRAP`) — self-serve `Account` upsert + server-side vehicle sync are documented follow-ups in [`../architecture/owner-onboarding.md`](../architecture/owner-onboarding.md). Wired in `cmd/telemetry-server/wiring_tesla_link.go`. | go-engineer |
| 2026-07-19 | **Dispatch pushes the pickup via `navigation_request`, not `navigation_gps_request` ([MYR-245](https://linear.app/myrobotaxi/issue/MYR-245)).** Fixes the Jul 18 2026 prod outage where every accept recorded `dispatch_error = invalid_request` and the car was never contacted. Root cause: the tesla-http-proxy's `ExtractCommandAction` has **no case** for `navigation_gps_request`, so the proxy returns HTTP 400 `invalid_command` **locally, before the vehicle is dialed**; our `classifyResponse` mapped that 400 → `invalid_request`. Only `navigation_request` is proxy-forwardable (proxy returns `ErrCommandUseRESTAPI` → Fleet REST). Dispatch now sends `navigation_request` with the pickup's full-precision `"<lat>,<lon>"` coordinate pair as the share `value` (text the car's nav geocoder resolves — the raw-coordinate form Teslemetry/Tessie-class clients use; on-car acceptance settled by MYR-245 live verification, fallbacks are the pickup address label or a maps URL). The `navigation_gps`-only `order` param is dropped; an out-of-range pickup coordinate now fails terminally with `invalid_request` **without dialing Tesla**. `navigation_gps_request` stays registered for API completeness but is documented NOT proxy-usable (§7.9 catalog + note updated; §7.8 "Dispatch outcome" updated). **Observability (no wire/schema change, no new DB column):** `classifyResponse` now falls back to the top-level `error` string (the proxy's `invalid_command` shape carries no envelope reason) in every non-OK branch and **sanitizes** it (strips URLs + signed-decimal coordinate pairs, collapses to a lowercase `[a-z0-9_ .:-]` charset, truncates to 120) before it reaches any log; a new optional `CommandError.Detail` carries that opaque reason, surfaced only on the server-side dispatch outcome log as `error_detail` (the `dispatch_error` code set is unchanged). Implementation: `internal/dispatch/dispatcher_retry.go` + `dispatcher.go`, `internal/commands/{registry,proxy_transport,errors,executor}.go`; docs/operations/vehicle-commands.md nav note corrected. | go-engineer |
| 2026-07-23 | **Unsigned nav commands route directly to the Fleet REST API ([MYR-245](https://linear.app/myrobotaxi/issue/MYR-245) layer 2).** The Jul 20 2026 ride still recorded `dispatch_error = invalid_request` after the `navigation_request` switch: the pinned tesla-http-proxy (v0.4.1) mis-forwards REST-API commands — verified live, it double-writes an HTTP 400 body (`"command requires using the REST API"` + `"upstream internal error"`) instead of relaying to Fleet. The same body POSTed directly to `https://fleet-api.prd.na.vn.cloud.tesla.com` returned `{"response":{"result":true,"queued":true}}` and the vehicle received the destination (live-verified 2026-07-23). New `RoutingTransport` sends `SignerRequired` commands to the proxy (unchanged) and unsigned commands (`navigation_request`) directly to Fleet REST (`FLEET_API_BASE_URL`, validated fail-fast at startup; 408/asleep → the existing wake+retry loop; 400/401/403/422 classified as before). `transport_unconfigured` now also covers a missing Fleet base for unsigned commands. §7.8 dispatch note, §7.9 transport/catalog notes, and the ops runbook corrected (the proxy does NOT forward unsigned commands in this deployment). | go-engineer |
| 2026-07-17 | **`POST /api/auth/refresh` enriches `user.name`/`user.email` on refresh, not just `user.id` ([MYR-243](https://linear.app/myrobotaxi/issue/MYR-243)).** §7.10.2 previously documented refresh's `user` as carrying "at least `id`" -- installs whose local profile cache was wiped got a blank name/email until the next full sign-in even though the fields were already `omitempty` on the wire. `Service.Refresh`'s successful-rotation path now best-effort enriches `UserInfo` the same way sign-in does, via a new `Store.GetUserProfile(ctx, userID)` that reads the `go_identity_apple` binding row (freshest by `last_login_at`), falling back to `go_users` when no binding row exists. **No wire-shape change** -- `name`/`email` were already optional fields; this only changes which requests populate them. A profile-lookup failure is fail-open for enrichment only, never for auth: the refresh still succeeds and `user` degrades to `id`-only rather than the request failing (logged via the identity module's existing audit trail, no PII). §7.10.2 response description updated accordingly; a stale `§7.9.1` cross-reference (section does not exist) corrected to `§7.10.1`. Implementation: `internal/identity/pgstore.go` (`GetUserProfile`), `internal/identity/service.go` (`Store` interface + `Service.projectUser`), `internal/identity/audit.go` (`profileLookupFailed`). | go-engineer |
| 2026-07-11 | **Populate `requesterName` on ride-request surfaces — P10 ride-hailing ([MYR-229](https://linear.app/myrobotaxi/issue/MYR-229)).** The optional `requesterName` field (contracts v0.11.0, `$defs.RideRequest` + the `ride_request_created` / `ride_status_changed` WS payloads) is now populated server-side from the requester's (`rider_id`) identity — resolved from the Prisma-owned `"User"` table (READ-ONLY, CG-DL-9) via the fallback chain **first name (first whitespace token of the display name) → email local-part → `"Rider"`**, OMITTED only when the rider has no `"User"` row (never an empty string). Surfaced on the party-only REST detail + every list item and carried on the two per-party-unicast WS summary frames. Every projection — plain SELECTs, list scans, and every `UPDATE ... RETURNING` / `INSERT ... RETURNING` (Create, status transitions, reschedules) — resolves the requester **inline via a correlated subselect in the same statement** as the ride row: no separate lookup, no N+1, no after-commit window, so the name is returned atomically with each read/mutation and an unresolvable identity **never fails** the ride operation. New §7.8 "Requester name (MYR-229)" subsection; §4.7/§4.8 field tables + examples updated; data-classification.md §1.9 gains a "Derived field — `requesterName`" note (**P1 PII, party-only, never logged**; no new stored column). Implementation: `internal/store/ride_request_queries.go` (inline `requesterIdentitySelect` subselect appended to `rideRequestColumns`) + `ride_request_requester.go` (pure fallback chain) + `ride_request_scan.go` (resolves per row), threaded through `ride_request_{repo,types}` → `cmd/telemetry-server/ride_request_adapters.go` → `internal/telemetry/ride_request_{types,wire,handler}.go` → `internal/events/ride_events.go` (`RequesterName *string`) → `internal/ws/{messages,ride_broadcast}.go`. | go-engineer |
| 2026-07-10 | **One active instant ride per rider — `POST /api/ride-requests` `409 ride_active` guard ([MYR-230](https://linear.app/myrobotaxi/issue/MYR-230)).** An instant create is now refused `409 ride_active` when the rider already holds an OPEN instant ride (status not terminal); **scheduled rides are exempt** (a rider may hold many scheduled rides plus one open instant ride, and an open scheduled ride never blocks a new instant one). The guard is authoritative via a **partial unique index** (`uq_go_ride_requests_active_instant_rider` on `rider_id WHERE scheduled_for IS NULL AND status IN ('requested','accepted','enroute','arrived')`, migration 0004) — two concurrent instant creates serialize in Postgres, exactly one wins, the loser's `23505` maps to the same 409 (create-side analogue of the `UpdateStatusFrom` race discipline). **Migration 0004 is self-cleaning:** before creating the index it cancels every OLDER open instant ride per rider (pre-guard production debris would otherwise abort the `CREATE UNIQUE INDEX`), keeping the most recent by `created_at DESC, id DESC`; the dedup fires no `ride_status_changed` WS frames (migrations run with no event bus — clients refetch via REST). The 409 body augments the standard error envelope with a sibling **`activeRideRequest`** (`$defs.RideRequest`, the `GET /api/ride-requests/{id}` shape) so the client adopts the existing ride instead of surfacing a decline (§7.8 new "409 `ride_active` body"). New **REST-only** shared-catalog code `ride_active` (409) added to §4.1.1, §4.1.1.a, the §4.1.1.b emission audit, `wserrors` + the reachability matrix, and the `schemas/ws-messages.schema.json` `ErrorPayload.code` enum (REST-only-on-the-wire). Implementation: `internal/store/ride_request_{repo,queries,errors}.go` (partial index + `GetActiveInstantByRider` + 23505→`ErrRideRequestActive`), `internal/telemetry/ride_request_{handler,types}.go` (pre-check + race backstop + `activeRideRequest` body), `cmd/telemetry-server/ride_request_adapters.go`. No data-classification change (adopted-ride coordinates go only to the ride's own rider, mirroring GET; never logged). **Contracts follow-up:** the shared error-envelope enum in `myrobotaxi/contracts` needs an additive `ride_active` entry (tolerant enum, patch tag) — see PR body. | go-engineer |
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
| 2026-07-29 | **Vehicle sharing moved to the Go server and the `viewer` role made real ([MYR-184](https://linear.app/myrobotaxi/issue/MYR-184)).** §7.5.4 resend re-mints across **every** row of a multi-vehicle invite in one transaction (a single-row re-mint left the previous code live and pending on the siblings for the rest of its 7-day TTL, and split the invite so the new code granted one car and the old code the rest). §7.5 rewritten from three email-keyed Prisma-backed invite endpoints to **five code-based endpoints on the Go telemetry server** — create / list / cancel-revoke / resend / redeem — backed by the Go-owned `go_vehicle_shares` table (migration 0020, no FKs to the sibling schema per CG-DL-9). Wire shapes per contracts **v0.19.0** `vehicle-sharing.schema.json`: `ShareInvite`, `SharePermission`, `CreateShareInviteRequest`, `RedeemShareInviteRequest`, `RedeemShareInviteResponse`, `ShareInviteListResponse` (envelope key `invites`, deliberately unpaginated). **§10 DV-23 flipped RESOLVED → SUPERSEDED for its §7.5 half** (the §7.6 half stands): `react-frontend` is deprecated and the Prisma `Invite` table is retired unused. **Codes, not emails** — `email`/`senderId`/`sentDate`/`isOnline` removed, `label`/`permission`/`code`/`expiresAt` added. New §7.5.0 documents the cumulative tier order (`live` < `live_history` < `rides`) and which server gate each tier opens; sharing grants no writes at any tier. **§7.8 CORRECTED**: the bullet claiming shared-viewer ride requests needed "no change to this handler" was wrong — the create-time check is a separate code path from the read-side access set, and MYR-184 changed it to admit a `rides`-tier viewer. The identical stale comment in `internal/telemetry/ride_request_handler.go` was corrected in lockstep. §5.2.0 viewer mask gains `sharePermission` and **loses its `name` subtraction** — the nickname is now viewer-visible (the rider UI renders "{Owner}'s {Vehicle}", and `name` is `required` in `vehicle-summary.schema.json`, so masking it made every viewer row invalid against its own shape); the viewer list now subtracts nothing; §5.2.5 `inviteOwnerFields` rebuilt from the never-shipped Prisma shape (`id`/`email`/`revokedAt`) to the real one (`inviteId`/`label`/`permission`/`code`/`expiresAt`). Cross-contract: `data-classification.md` §1.15 classifies `go_vehicle_shares`. Implementation: `internal/store/vehicle_share_*.go`, `internal/telemetry/share_*.go` + `vehicle_share_access.go`, `internal/auth/{share_permission,vehicle_access}.go`, wired in `cmd/telemetry-server/wiring_vehicle_sharing.go`. | go-engineer |
| 2026-05-08 | **DV-23 RESOLVED by [MYR-69](https://linear.app/myrobotaxi/issue/MYR-69).** Locked the FR-10 deletion + §7.5 invite-endpoint architecture to **Option 2 -- Next.js app owns `DELETE /api/users/me` and the three invite endpoints**, with the Go telemetry server holding **Insert-only** access to the Prisma-owned `AuditLog` table via raw pgx. §7.5 preamble rewritten from "two implementation paths" to a single locking sentence. §7.6 implementation notes' "may also run in the Next.js app layer" hedge replaced with a definitive Next.js-owns statement. §10 DV-23 row flipped from **New** to **RESOLVED** with resolution date, rationale, and pointers to MYR-70 / MYR-71 / MYR-72 / MYR-73 implementation follow-ups. **DV-20 row reduced in scope from six endpoints to four**: invite + user-deletion 404s on the Go server are now the terminal behavior (served by Next.js per DV-23), not transitional Go-server work; implementation order steps (5)/(6) and the FR-5.x / FR-10.1 anchors removed. Cross-contract update: [`data-lifecycle.md`](data-lifecycle.md) §1.4 adds an `AuditLog` row noting the telemetry server has Insert-only access; §4 preamble locks `AuditLog` ownership to the Next.js Prisma schema with the Go server as Insert-only writer (responsibility per §3.4). No wire / OpenAPI / SDK API changes -- the SDK still calls the single `https://api.myrobotaxi.com/api/...` base URL. | sdk-architect |
| 2026-04-14 | Initial full draft (MYR-12): §2 transport, §3 auth, §4 conventions (error envelope, pagination, versioning, headers, idempotency), §5 RBAC with forward-looking `limited_viewer` extension seam, §6 catalog summary, §7 per-endpoint reference (snapshot, drives list, drive detail, drive route, 3 invite ops, user self-deletion), §8 resource-schema index cross-referencing the inline OpenAPI components, §9 observability, §10 divergences DV-19 through DV-23 (REST auth middleware, unmounted SDK endpoints, reserved `503 service_unavailable`, REST rate limit, invite handler location decision). Adds REST-only error codes `not_found`, `invalid_request`, `service_unavailable` to the shared catalog with a note that the `ErrorPayload.code` enum in `schemas/ws-messages.schema.json` must be extended in the DV-20 follow-up. Canonical machine-readable twin is `specs/rest.openapi.yaml`. | sdk-architect |
