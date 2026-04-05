# REST API Contract

**Status:** TODO — placeholder
**Target artifact:** OpenAPI 3.1 specification
**Owner:** `sdk-architect` agent

## Purpose

Defines the HTTP REST surface served by the telemetry server: cold-page snapshot fetches, paginated drive history, per-drive detail and route playback, sharing/invite flows, viewer management, and user-initiated data deletion. This is the non-streaming half of the SDK contract — the WebSocket protocol handles live state, REST handles snapshots and lifecycle actions.

## Anchored requirements

- **FR-3.2, FR-3.3, FR-3.4** — paginated drive history, per-drive route playback, per-drive stats
- **FR-5.1, FR-5.2, FR-5.3, FR-5.4, FR-5.5** — invite creation, viewer list, revocation, role model
- **FR-9.1, FR-9.2** — one-shot paginated fetch + reactive subscription pairing
- **FR-10.1, FR-10.2** — user-initiated deletion + audit log write

## Sections to author (TODO)

- [ ] Authentication scheme (bearer token from `getToken()` per FR-6.1)
- [ ] Endpoint catalog: `GET /vehicles/{id}/snapshot`, `GET /vehicles/{id}/drives` (paginated), `GET /drives/{id}`, `GET /drives/{id}/route`, invite endpoints, deletion endpoint
- [ ] Pagination scheme (cursor-based, page size limits)
- [ ] Error response envelope (typed error codes per FR-7.1)
- [ ] RBAC enforcement notes (role field masks per NFR-3.19, NFR-3.20)
- [ ] OpenAPI document link + generation instructions
