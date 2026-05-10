# MyRoboTaxi SDK v1 — Requirements

**Status:** Draft — awaiting review
**Owners:** tnando + sdk-architect agent
**Last updated:** 2026-04-04

This document captures the functional and non-functional requirements for the MyRoboTaxi SDK v1. Every subsequent contract doc, architectural decision, and PR is traceable back to this document.

---

## 1. North Star

Build a polished SDK that both web and iOS apps plug into, with comprehensive documentation and enforceable contracts. Stop the ping-pong between backend and frontend by making the SDK the single source of truth for data access. The UI becomes a dumb consumer.

**Design principles** (apply to every decision in this doc):

1. **Accuracy and performance first** — defaults must be right, don't expose perf knobs consumers have to tune.
2. **Atomicity** — logically-related fields are delivered together. UI never sees a half-changed group.
3. **Event-driven freshness** — staleness is signaled by explicit server events, not client-side timers.
4. **Extensibility as pattern, not architecture** — new telemetry fields are a data-model change + subscription wire-up, not an architectural change.
5. **Privacy-by-default** — raw telemetry is never persisted as a historical log. Application-level encryption for sensitive fields.
6. **Logic-only SDK** — no UI components, no map renderers, no styling. Consumers compose state with their own UI.
7. **Decoupled state** — connection state and per-field data freshness are independent concerns.
8. **Single source of truth** — every field has one source (DB or WebSocket), documented in the data lifecycle contract.

---

## 2. Functional Requirements (FRs)

### 2.1 Real-time vehicle state

**FR-1.1** SDK MUST stream live vehicle telemetry for: position, speed, heading, gear.
**FR-1.2** SDK MUST stream live charge state: battery level, charge state, estimated range.
**FR-1.3** SDK architecture MUST allow adding new telemetry fields (climate, doors, media, safety, etc.) via a mechanical data-model change, NOT an architectural change. Extensibility is tested by adding at least one new field post-v1 without modifying the SDK's core subscription mechanism.

**Out of scope for v1:** climate, doors/locks/sentry, media, TPMS, alerts.

### 2.2 Navigation state

**FR-2.1** SDK MUST expose: destination name, ETA (minutes to arrival), distance remaining, route polyline, origin location, trip start time.
**FR-2.2** Nav fields MUST be delivered as an atomic group — when any nav field changes, all correlated nav fields are updated together (see NFR-3).
**FR-2.3** When Tesla navigation is cancelled, the entire nav group MUST clear atomically — no stale destination, ETA, or polyline lingering.

**Out of scope for v1:** turn-by-turn maneuvers, lane guidance, rerouting hints.

### 2.3 Drive detection & history

**FR-3.1** SDK MUST emit live drive events: `drive_started`, `drive_updated` (per-point GPS updates), `drive_ended`.
**FR-3.2** SDK MUST expose paginated drive history (list of past drives with basic metadata).
**FR-3.3** SDK MUST expose per-drive route playback (full GPS trail for rendering on a map).
**FR-3.4** SDK MUST expose per-drive stats: distance, duration, avg/max speed, energy used (kWh), FSD miles driven, intervention count, start/end charge level, start/end location + address.
**FR-3.5** SDK MUST provide both a one-shot paginated fetch (for scrolling back) and a reactive subscription (latest drives auto-update when a drive completes).

### 2.4 Multi-vehicle support

**FR-4.1** v1 SDK supports a single active vehicle per user.
**FR-4.2** SDK public API signatures MUST be vehicle-scoped (e.g., `useVehicle(vehicleId)`) even in v1, so multi-vehicle support in v2 is purely additive — no API renames or breaking changes.

**Out of scope for v1:** switching active vehicles mid-session, multi-vehicle simultaneous streams.

### 2.5 Sharing & observability

**FR-5.1** SDK MUST support an invite flow: owner creates invite, recipient accepts, recipient becomes a read-only viewer.
**FR-5.2** SDK MUST expose a viewer list to owners (who has access, invite status).
**FR-5.3** SDK MUST support revoking viewer access.
**FR-5.4** Role model: `owner` (full read + write/commands) and `viewer` (full read, no write). Static field masks per role applied server-side.
**FR-5.5** Architecture MUST support adding a third role (e.g., `limited_viewer`) without schema changes.

**Out of scope for v1:** per-viewer custom field visibility, granular command permissions for viewers.

### 2.6 Authentication & token lifecycle

**FR-6.1** SDK MUST accept a `getToken: () => Promise<string>` callback from the consumer. SDK never stores credentials.
**FR-6.2** SDK MUST call `getToken()` on initial connection and on every auth error (401/expired).
**FR-6.3** SDK MUST NOT implement OAuth flows, token refresh logic, or credential storage. Those belong in the consumer's auth layer.

### 2.7 Error handling & recovery

**FR-7.1** SDK MUST expose typed error codes (e.g., `auth_expired`, `network_timeout`, `vehicle_offline`, `permission_denied`) so consumers branch without string-matching.
**FR-7.2** SDK MUST automatically retry transient errors (network blips, 5xx, timeouts) with exponential backoff.
**FR-7.3** Only terminal errors (auth expired, permission denied, vehicle not owned, persistent server failures) surface to the UI.

### 2.8 State machine exposed to consumers

**FR-8.1** SDK MUST expose two independent state dimensions:
- `connectionState`: `initializing | connecting | connected | disconnected | error`
- `dataState` per atomic field group: `loading | ready | stale | cleared | error`

**FR-8.2** The UI composes these two dimensions to render its own loading/error/stale presentation. SDK does not combine them into a single enum.

### 2.9 Historical data fetching

**FR-9.1** Drive history follows the real-time strategy: one-shot paginated fetch + reactive subscription for recency.
**FR-9.2** A completed drive MUST appear in the live drives list without the UI re-fetching.

### 2.10 User data controls

**FR-10.1** SDK/server MUST support user-initiated deletion of all user data: drive history, vehicle state snapshot, invites, active sessions.
**FR-10.2** Every deletion MUST write an immutable audit log entry: user ID, timestamp, what was deleted, initiator.

The backup-retention boundary that closes the GDPR Art. 17 obligation across Supabase backup restores is documented in the operational runbook [`docs/operations/backup-retention.md`](../operations/backup-retention.md) — Supabase backup window, redelete-on-restore procedure, and the legal-basis-for-retention boundary spanning primary DB / backups / AuditLog / cold logs. The `data_exported` audit action that records GDPR Art. 15 / 20 portability exports is documented in [`docs/contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.2 and the export endpoint itself in [`docs/contracts/rest-api.md`](../contracts/rest-api.md) §7.7.

> **Resolved 2026-05-10 ([MYR-76](https://linear.app/myrobotaxi/issue/MYR-76) + [MYR-79](https://linear.app/myrobotaxi/issue/MYR-79)):** The recent-login re-auth gate is implemented and documented. The Next.js handlers for `DELETE /api/users/me` and `GET /api/users/me/export` require the caller's most recent fresh OAuth sign-in to be within `REAUTH_MAX_AGE_SEC` (default 300 s) — mirroring the OIDC `auth_time` claim — and reject with `401 auth_failed` / `subCode: reauth_required` otherwise. The gate applies symmetrically to deletion and export per the GDPR Art. 17 recent-auth corollary, since both endpoints surface the full ownership graph. Mechanism in [`docs/contracts/rest-api.md`](../contracts/rest-api.md) §7.6 / §7.7 (Re-auth precondition) and §4.1.1 (`auth_failed` row, `subCode: reauth_required`).

### 2.11 Observability (SDK-side)

**FR-11.1** SDK MUST expose structured logging with levels (debug/info/warn/error) via a pluggable logger interface. Default logger writes to console in debug mode, silent in production.
**FR-11.2** SDK MUST NEVER log sensitive data (tokens, GPS coordinates, destination addresses).
**FR-11.3** SDK MUST expose in-memory performance metrics: latency histograms per-message-type, reconnect counts, message counts, error rates, staleness events.
**FR-11.4** SDK MUST provide an optional OpenTelemetry hook. Consumers wire in their own OTel SDK; ours emits spans/metrics following OTel semantic conventions.
**FR-11.5** SDK MUST provide a debug mode that elevates logging, enables verbose metrics, and exposes internal state (current snapshot, pending subscriptions, connection state machine) via a debug API.

---

## 3. Non-Functional Requirements (NFRs)

### 3.1 Latency targets (end-to-end Tesla → UI render)

| Event | Target | Notes |
|-------|--------|-------|
| **Live telemetry updates** (GPS, speed, nav changes, drive events, battery, charge state, range) | **< 2s** | p95 measured at the consuming client |
| **Cold page load snapshot** (DB → UI first render) | **< 500ms** | No loading skeletons per-field; full state on first paint |
| **Live stream start** after page load | **< 1s** after auth completes | Time from `getToken()` resolution to first live message |
| **Nav change atomicity window** | **<= 200ms** | Server debounce window for grouping sibling field updates |

### 3.2 Atomicity contract

**NFR-3.1** Fields that change together MUST be delivered together. The server MUST group related telemetry updates into atomic messages.

**Atomic groups:**
- **Navigation**: destinationName, destinationLatitude, destinationLongitude, originLatitude, originLongitude, navRouteCoordinates, etaMinutes, tripDistanceRemaining, tripStartTime
- **Charge**: chargeLevel, chargeState, estimatedRange, timeToFull
- **GPS**: latitude, longitude, heading, speed
- **Gear**: gearPosition, derivedStatus (driving/parked)

**NFR-3.2** Server-side debounce: the broadcaster buffers sibling field updates for up to 200ms before emitting a single grouped message.
**NFR-3.3** DB snapshots MUST be self-consistent: if `destinationName` is present, `navRouteCoordinates` and `etaMinutes` for that destination MUST also be present, or all three are NULL.
**NFR-3.4** UI MUST NEVER see a partial group — e.g., showing a destination with no route.

### 3.3 DB snapshot completeness

**NFR-3.5** Every field the UI renders MUST be persisted to the DB. On cold page load, the snapshot MUST contain enough data to render the full UI without per-field loading spinners.
**NFR-3.6** If any UI-visible field is missing from the DB at page-load time, that is a bug in the persist path, not a display concern.

### 3.4 Freshness & staleness

**NFR-3.7** Freshness is event-driven, NOT time-based. The SDK MUST NOT use client-side TTL timers to mark data stale.
**NFR-3.8** Data is stale only when: (a) the server explicitly signaled a clear (nil value via WebSocket + NULL write to DB), or (b) the WebSocket is disconnected.
**NFR-3.9** When the server marks a field invalid, the SDK MUST apply the clear atomically within the affected group.

### 3.5 Reliability & connection lifecycle

**NFR-3.10** SDK MUST support automatic reconnect with exponential backoff (initial delay 1s, max delay 30s, jitter applied).
**NFR-3.11** On reconnect, SDK MUST re-fetch the DB snapshot and resume live stream without user intervention.
**NFR-3.12** SDK MUST gracefully tolerate offline mode: cached state from DB remains visible, connection state signals to the UI, no forced reloads.
**NFR-3.13** Offline tolerance: no maximum — cached data remains visible indefinitely until the user acts on it or reconnection succeeds.

### 3.6 Scale targets

| Metric | Target |
|--------|--------|
| Users | 1,000 |
| Vehicles | 2,000 |
| Concurrent WebSocket clients | 5,000 |
| Telemetry throughput per vehicle | 1 Hz sustained, 10 Hz burst tolerance |

**NFR-3.14** Server architecture MUST be shardable — no in-memory singletons that can't scale horizontally.
**NFR-3.15** Broadcast fanout MUST NOT be O(n) over all clients per message. Per-vehicle subscription routing required.
**NFR-3.16** Event bus MUST be bounded with backpressure handling (drop-oldest or pause-producer policy documented).
**NFR-3.17** DB writes MUST be batched/coalesced to avoid hot-path writes per telemetry event.

### 3.7 Security & access control

**NFR-3.18** Role-based access control with two roles in v1: `owner`, `viewer`.
**NFR-3.19** Every WebSocket broadcast MUST be projected through the recipient's role mask before sending. No raw fan-out.
**NFR-3.20** Persisted DB reads MUST respect the viewer's role-based field visibility.
**NFR-3.21** Vehicle ownership MUST be enforced on every API call and every WebSocket subscription. Unauthorized access returns `permission_denied`.

### 3.8 Encryption

**NFR-3.22** **In transit**: all connections use TLS.
- Tesla → server: mTLS on port 443
- Server ↔ browsers/apps: WSS + HTTPS
- Server ↔ DB: TLS with `sslmode=require`
- Server ↔ Tesla Fleet API: HTTPS

**NFR-3.23** **At rest (application-level)**: AES-256-GCM column-level encryption for:
- Tesla OAuth `access_token` and `refresh_token` (`Account` table)
- GPS coordinates: `Vehicle.latitude`, `Vehicle.longitude`, `Drive.routePoints`, `Vehicle.navRouteCoordinates`
- Destination coordinates: `Vehicle.destinationLatitude`, `Vehicle.destinationLongitude`, `Vehicle.originLatitude`, `Vehicle.originLongitude`

**NFR-3.24** Encryption key stored as Fly.io secret (`ENCRYPTION_KEY`).
**NFR-3.25** Encryption is transparent to the SDK — the server store layer handles encrypt/decrypt. The SDK never sees ciphertext.
**NFR-3.26** Key rotation strategy documented in a separate contract doc.

### 3.9 Data classification

Every persisted field MUST be labeled with a classification tier in the contract docs:
- **P0** (Public): VIN's last-4, vehicle name, vehicle model. May appear in logs.
- **P1** (Sensitive, encrypted at rest): GPS coordinates, destination data, OAuth tokens. Never in logs.
- **P2** (Sensitive + access-logged): reserved for future use (e.g., payment info, health data if added).

### 3.10 Data retention

**NFR-3.27** **Drive records**: 1 year rolling window. Background job prunes drives older than 365 days.
**NFR-3.28** **Raw telemetry history**: NOT persisted. Only two persistence points:
- `Vehicle` row: current snapshot only, overwritten on update
- `Drive.routePoints`: GPS trail bounded by drive lifetime

**NFR-3.29** **Audit logs** (deletion events): retained indefinitely.

### 3.11 Developer experience & bundle size

**NFR-3.30** TypeScript SDK gzipped bundle: **< 75 KB** (hard budget, enforced in CI).
**NFR-3.31** Public API surface: **10-20 hooks/methods**. No kitchen-sink utilities.
**NFR-3.32** SDK is **logic-only**: no React components, no map renderers, no theming. Consumers compose state with their own UI.

### 3.12 Platform support

The SDK split is two implementations, each owning its native platform set. There is **no React Native** in v1 — Apple platforms consume the Swift SDK directly.

**TypeScript SDK (`@myrobotaxi/sdk`)** — web/Next.js consumers only. Supports:
- React (hooks layer, separate entry point — primary consumer is the MyRoboTaxi Next.js app)
- Vanilla TypeScript (core client, no React dependency)
- Node.js (SSR, scheduled tasks, server-side usage — no browser globals in core)

**NFR-3.33** The core TypeScript SDK uses a WebSocket abstraction that runs on browser `WebSocket` and Node `ws` only. Apple platforms (iOS / iPadOS / macOS / watchOS / visionOS) MUST consume the Swift SDK directly (NFR-3.34) and MUST NOT consume the TypeScript SDK via React Native bridging.

**Swift SDK (`MyRoboTaxiSDK`)** — Apple platforms only. Supports:
- iOS 26+
- iPadOS 26+
- macOS 26+
- watchOS 26+
- visionOS 26+

**NFR-3.34** Baseline: Swift 6 with strict concurrency (`-strict-concurrency=complete`), async/await, structured concurrency with parent-child Task hierarchies, actor isolation for shared mutable state, `@Observable` (Swift 5.9+ Observation framework) for state exposure, `URLSessionWebSocketTask` for WebSocket transport. Distribution: Swift Package Manager only — no CocoaPods, no Carthage. The Swift SDK MUST NOT depend on:

- third-party WebSocket libraries (no SocketRocket, no Starscream),
- Combine for net-new code (Combine is in maintenance — use `AsyncSequence` / `AsyncStream`),
- `@Published` / `ObservableObject` / `Combine.Publisher` on the public API (legacy state-exposure pattern),
- `DispatchQueue.sync` or `NSLock` for shared state (use actor isolation).

**NFR-3.35** No UIKit / AppKit / SwiftUI dependencies. The Swift SDK is UI-layer-agnostic — SwiftUI, UIKit, and AppKit consumers all compose state themselves.

**NFR-3.36** Apple platform lifecycle handling: the Swift SDK MUST tolerate aggressive OS-driven app suspension (especially watchOS) and resume cleanly. Lifecycle bindings are specified in [`docs/contracts/swift-lifecycle.md`](../contracts/swift-lifecycle.md). Summary requirements:

- **NFR-3.36a** Consumer-driven foreground reconnect: on receipt of an `handleForegroundTransition()` call from the consumer (forwarded from `ScenePhase.active`, `UIScene.willEnterForegroundNotification`, `WKApplicationDidBecomeActiveNotification`, or `NSApplication.didBecomeActiveNotification` — whichever matches the consumer's UI framework), the Swift SDK MUST bypass the reconnect backoff timer for the first attempt and reset the retry counter to 0. Backoff resumes only if the foreground reconnect attempt fails. The SDK is UI-framework-agnostic (NFR-3.35) and MUST NOT observe scene transitions itself.
- **NFR-3.36b** The Swift SDK MUST expose `performBackgroundSnapshotRefresh()` and `performBackgroundDriveRoutePrefetch(maxDrives:)` async methods so consumers can invoke them from `BGAppRefreshTask` / `BGProcessingTask` handlers (iOS / iPadOS / macOS / visionOS) or `WKApplicationRefreshBackgroundTask` handlers (watchOS). The SDK MUST observe `Task.checkCancellation()` so consumer-supplied expiration handlers can cancel in-flight work. The SDK MUST NOT register or schedule background tasks itself — that is consumer responsibility.
- **NFR-3.36c** watchOS sessions MUST tolerate `WKExtendedRuntimeSession` termination and consumer-signalled background transitions (`handleBackgroundTransition()`). On watchOS only, the SDK proactively closes the WebSocket on background and treats the close as `disconnected` (not `error`); on the next `handleForegroundTransition()` call the SDK rehydrates from the REST snapshot and applies the §7.2 reconnect sequence from `websocket-protocol.md`.

**NFR-3.36d** Test framework: Swift Testing (`import Testing`, `@Test` macros) for all net-new tests. XCTest is acceptable only for legacy harnesses or APIs Swift Testing does not yet cover. All test fixtures `Sendable`; concurrency-safe by construction.

### 3.13 Versioning policy

**NFR-3.37** Strict semver. Breaking changes require a major version bump.
**NFR-3.38** Deprecated APIs remain functional for one full major version, emit runtime deprecation warnings.
**NFR-3.39** Migration guides published with every major release.
**NFR-3.40** Protocol-level multi-versioning (server supporting v1 + v2 simultaneously) is a v3+ concern, not v1.

### 3.14 Release cadence

**NFR-3.41** **Weekly scheduled releases** for stable SDK versions.
**NFR-3.42** **Hotfix lane** for critical bugs (security, data correctness) bypasses the weekly cycle.
**NFR-3.43** **Canary pre-releases** (`v1.x.x-canary.N`) tagged on every merge to main for internal dogfooding.
**NFR-3.44** Release notes auto-generated from PR labels (`breaking`, `feature`, `fix`, `chore`).

### 3.15 Test bench (ship-gate)

**NFR-3.45** Test bench is a **v1 ship-gate**, not a nice-to-have. All four layers below MUST pass before v1 ships.

1. **Contract conformance**: every WebSocket message type and REST endpoint validated against AsyncAPI/OpenAPI spec. SDK parsing verified against canonical fixtures.
2. **FR validation**: every FR has a deterministic test scenario (e.g., "set nav → UI reflects < 2s", "cancel nav → atomic clear", "reconnect → full state restored", "page load mid-drive → complete snapshot").
3. **NFR measurement**: automated latency measurements (p50/p95/p99), load tests at scale target (5K concurrent clients), bundle size check, memory/CPU budgets.
4. **Chaos scenarios**: server crash mid-drive, Tesla API 5xx, Tesla API stale data, token expiry mid-stream, network partition, WebSocket drops with varied backoff, auth server unavailable.

**NFR-3.46** Two test benches:
- **TUI test bench** (`cmd/testbench` in telemetry repo): backend/protocol validation, developer-facing.
- **Web test bench** (`my-robo-taxi-testbench` standalone repo): UI/SDK validation, user-facing, dogfoods the SDK.

### 3.16 Observability (server)

**NFR-3.47** Structured logs: slog JSON to stdout, ingested by a log aggregator (Loki/Datadog/Axiom).
**NFR-3.48** Prometheus metrics exposed on `:9090`, covering every pipeline stage.
**NFR-3.49** Distributed tracing via OpenTelemetry: spans across receiver → event bus → broadcaster/store writer → WebSocket broadcast.
**NFR-3.50** SLO dashboards derived from latency contract (§3.1).
**NFR-3.51** SLO burn-rate alerts, error rate alerts, connection health alerts, auth failure spike alerts.

### 3.17 Documentation

**NFR-3.52** Markdown source in repo, published as a static site (Mintlify/Docusaurus/Nextra).
**NFR-3.53** API reference auto-generated via TypeDoc (TS) and DocC (Swift) on every release.
**NFR-3.54** Interactive examples (runnable sandboxes) for common flows.
**NFR-3.55** Hosted on Vercel or Cloudflare Pages.

### 3.18 Work tracking

**NFR-3.56** Linear is the source of truth for work tracking (epics, tickets, cycles).
**NFR-3.57** Branch names and PR titles include Linear ticket IDs for auto-linking.
**NFR-3.58** GitHub issues redirect to Linear after migration.

---

## 4. Data Migration Policy

**MP-1** Pre-launch, single-user state: the SDK v1 launch is a full reset. All existing drive records, vehicle state snapshots, and historical nav data are cleared.
**MP-2** v1 launches with a clean DB. Every row thereafter conforms to the new contract.
**MP-3** Post-v1, targeted backfills are permitted only for specific UX breaks, not prophylactically.

---

## 5. Out of Scope for v1 (Explicit)

To prevent scope creep, the following are explicitly deferred:

- Multi-vehicle simultaneous streams (v2)
- Per-viewer custom field visibility (v2)
- Climate, doors/locks, sentry mode, media telemetry (v2+)
- TPMS, alerts, errors stream (v2+)
- Turn-by-turn navigation maneuvers (v2+)
- Command-and-control (unlock, honk, climate-start, etc.) — read-only for v1
- Multi-region deployment
- Protocol-level multi-versioning
- Third-role access tier (`limited_viewer`)
- GPS data pseudonymization for analytics
- Real-time collaborative features (multiple viewers on same vehicle interacting)

---

## 6. Success Criteria (v1 Launch)

v1 is ready to launch when:

1. ✅ All FRs above have passing test bench scenarios
2. ✅ All NFR latency targets met at p95 in load tests
3. ✅ All four test bench layers (contract, FR, NFR, chaos) pass
4. ✅ TypeScript SDK bundle < 75KB gzipped
5. ✅ Swift SDK builds and passes on all target platforms
6. ✅ Contract docs complete and published
7. ✅ Web app fully migrated to SDK (zero direct WebSocket/DB calls from UI code)
8. ✅ SLO dashboards live, alerts wired
9. ✅ Column-level encryption deployed for all P1 fields
10. ✅ Weekly release pipeline operational, first stable `v1.0.0` published

---

## 7. Change Log

| Date | Change | Author |
|------|--------|--------|
| 2026-04-04 | Initial draft from 27-question alignment session | Claude + tnando |
