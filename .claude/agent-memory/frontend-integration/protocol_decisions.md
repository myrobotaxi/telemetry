---
name: Protocol decisions
description: Confirmed WebSocket protocol decisions between the Go telemetry server and the MyRoboTaxi Next.js frontend
type: project
---

## WS message envelope

Server sends: `{ "type": "vehicle_update", "payload": { "vehicleId": "...", "fields": {...}, "timestamp": "..." } }`

Frontend parses: `msg.type` then casts `msg.payload as VehicleUpdate` — VehicleUpdate is `{ vehicleId, fields, timestamp }`.

The server's `vehicleUpdatePayload` struct uses `json:"vehicleId"` (camelCase) — this matches.
The server's `wsMessage` uses `json:"payload,omitempty"` as `json.RawMessage` — this matches.

**Why:** Verified by reading ws/messages.go (server) and websocket.ts (client). The envelope is compatible.

## Auth flow

Client sends: `{ type: "auth", payload: { token: "..." } }` immediately on open.
Server reads this as `wsMessage` then unmarshals `authPayload` from `msg.Payload`.

Server's `authPayload` struct: `Token string json:"token"` — matches the client's `{ token: "..." }`.

**Why:** Verified by reading ws/handler.go authenticateClient and websocket.ts handleOpen.

## Heartbeat

Server: `RunHeartbeat` sends `{ "type": "heartbeat" }` every 15s (default, configurable).
Client: expects a message every 15s (`HEARTBEAT_TIMEOUT_MS = 15_000`), reconnects on timeout.

**Compatible.** The server default and client timeout are identical.

## Error messages

Server sends: `{ "type": "error", "payload": { "code": "auth_failed", "message": "..." } }`
Client logs: `console.error('[VehicleWebSocket] Server error:', msg.payload)` — no structured handling beyond logging.

Error codes defined: `auth_failed`, `auth_timeout`.

## VIN resolver

Broadcaster calls `b.resolver.GetByVIN(ctx, payload.VIN)` to get the database vehicleID (Prisma cuid).
If VIN resolution fails, the event is silently skipped (warns but does not error to client).
This means the simulator's VIN must be registered in the database and have a VINResolver implementation wired up.
