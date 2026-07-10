# Vehicle Commands — Ops Runbook & Decision Record

**Status:** v1 — MYR-180
**Endpoint:** `POST /api/vehicles/{vehicleId}/command/{name}` (rest-api.md §7.9)
**Owner:** `go-engineer`

This document is the decision record and operations runbook for the Tesla
signed vehicle-command proxy: how commands are transported and signed, how
the command-signing key is managed, and the exact owner steps to pair a
virtual key so commands start working.

---

## 1. Decision: reuse the tesla-http-proxy sidecar (library NOT embedded)

Modern Tesla Fleet API vehicle commands require **end-to-end command
signing** with a virtual key the owner enrolls in the vehicle. Tesla ships
the signing protocol as an open-source Go library
(`github.com/teslamotors/vehicle-command`) plus a reference **HTTP proxy**
(`tesla-http-proxy`) that converts plain REST command calls into signed
protocol messages.

MYR-180 evaluated two integration paths:

| Option | What it means |
|--------|---------------|
| **A — embed the library** | Import `vehicle-command` into this Go process; load the P-256 key into server memory; manage signed sessions and domains in-process. |
| **B — reuse the sidecar** | Forward commands to the `tesla-http-proxy` sidecar this server **already runs** for `fleet_telemetry_config` pushes (`internal/telemetry.FleetAPIClient` → `TESLA_PROXY_URL`). |

**Decision: Option B.** Although the task's default preference was library
integration ("no extra process"), that rationale does not apply here — the
proxy process **already exists and is already deployed**. Reusing it is
strictly better:

1. **No process saved by the library.** The proxy already runs for
   telemetry-config signing; embedding the library would not remove a
   process, only add code.
2. **Key stays in exactly one place.** The P-256 command-signing key
   already lives ONLY in the proxy's config (`TESLA_KEY_FILE_B64` →
   `deployments/start.sh`). Embedding the library would put a **second copy
   of the private key into this Go process's memory** — a strictly worse
   security posture. This satisfies MYR-180's "key material is config-only,
   never generated/committed, never in the server process" constraint by
   construction.
3. **Signing, session caching, wake, and unsigned-command forwarding are
   free.** The proxy signs signer-required commands, caches per-vehicle
   sessions (`TESLA_CACHE_FILE`), re-handshakes on counter errors, and
   forwards unsigned commands (`navigation_request`) straight to the Fleet
   API (`pkg/proxy/proxy.go`: `navigation_request → ErrCommandUseRESTAPI →
   forwardRequest`). Embedding the library would re-implement all of this.
4. **Lower risk.** The proxy is Tesla's own reference implementation of the
   exact command REST surface. No large dependency (protobuf/crypto) is
   pulled into this module.

The command layer keeps a `Transport` seam (`internal/commands/transport.go`)
so the concrete transport could be swapped for a library-backed one later
without touching the registry, executor, scope-gating, or HTTP handler.

### Sources

- teslamotors/vehicle-command — https://github.com/teslamotors/vehicle-command (README: library vs `tesla-http-proxy`; `tesla-keygen`; `TESLA_CACHE_FILE` session cache)
- Fleet API Vehicle Commands — https://developer.tesla.com/docs/fleet-api/endpoints/vehicle-commands
- Fleet API scopes — https://developer.tesla.com/docs/fleet-api/authentication/overview (`vehicle_cmds`, `vehicle_charging_cmds`; there is **no** `vehicle_remote_start` Fleet scope)
- `pkg/proxy/command.go` `navigation_request → ErrCommandUseRESTAPI`; `pkg/proxy/proxy.go` forward-on-`ErrCommandUseRESTAPI`

---

## 2. Key management

The command-signing key is the **same domain EC key** already used for
Tesla partner registration and the `.well-known` public key. There is
nothing new to generate for MYR-180 if the telemetry integration is already
set up — the command signing reuses that key inside the proxy.

### 2.1 Generate the keypair (operator, offline — NEVER in prod, NEVER committed)

Tesla requires an EC key on the `prime256v1` (secp256r1 / NIST P-256)
curve. The repo already provides `scripts/generate-certs.sh`, which emits
`certs/server.key` (the private key) and `certs/public-key.pem` (the public
key hosted for pairing). To generate just the command key by hand:

```bash
# Private key (KEEP SECRET — never commit, never log):
openssl ecparam -name prime256v1 -genkey -noout -out tesla-command-key.pem

# Public key in the exact PEM form Tesla needs for the .well-known endpoint
# and virtual-key pairing:
openssl ec -in tesla-command-key.pem -pubout -out tesla-public-key.pem

# Verify the curve is correct (Tesla rejects anything but prime256v1):
openssl ec -in tesla-command-key.pem -noout -text 2>&1 | grep 'ASN1 OID'
# -> ASN1 OID: prime256v1
```

`generate-if-absent is NOT used` anywhere in the server — the key is
supplied by config only.

### 2.2 Where each half goes

| Artifact | Where it lives | Config var |
|----------|----------------|------------|
| Private key (`tesla-command-key.pem`) | **tesla-http-proxy sidecar only** — base64-encoded into an env var, decoded to a file at boot (`deployments/start.sh`). Never in the Go telemetry process. | `TESLA_KEY_FILE_B64` (or `TESLA_KEY_FILE` path) |
| Public key (`tesla-public-key.pem`) | Served by the telemetry server at the well-known route (already implemented, `internal/server/server.go`). | `TESLA_PUBLIC_KEY` |

The public key is hosted at the fixed Tesla URL:

```
https://<domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

The telemetry server serves it verbatim from `TESLA_PUBLIC_KEY`; if that env
var is empty the route is disabled (logged at startup). See
[`../tesla-setup.md`](../tesla-setup.md) §2 for the hosting details.

### 2.3 Env vars (all pre-existing; MYR-180 adds none)

| Var | Consumed by | Purpose |
|-----|-------------|---------|
| `TESLA_PROXY_URL` | telemetry server (command transport + fleet-config) | tesla-http-proxy base URL. **Empty → command signing disabled** (every signer-required command returns `key_not_paired`, logged at startup). |
| `AUTH_TESLA_ID` / `AUTH_TESLA_SECRET` | telemetry server | Tesla OAuth client creds; enable owner-token auto-refresh for commands (as for fleet-config). Empty → no refresh. |
| `TESLA_KEY_FILE_B64` / `TESLA_KEY_FILE` | tesla-http-proxy sidecar | The P-256 command private key. |
| `TESLA_PUBLIC_KEY` | telemetry server | Public key served at the `.well-known` route. |

The per-vehicle command cooldown and wake/counter retry budgets are code
constants with sane defaults (`internal/commands`, `internal/telemetry`);
no env vars gate them in v1.

---

## 3. Owner virtual-key pairing runbook (MYR-115)

Until the vehicle owner pairs the application's virtual key, **every
signer-required command returns `403 key_not_paired`**. This is expected
and safe — the endpoint is live and returns typed errors, nothing crashes.

Exact owner steps, once the app is registered and the public key is hosted:

1. Ensure the vehicle is **awake and nearby** (pairing needs the car and the
   owner's key card).
2. Open the pairing link on the owner's phone (Tesla app installed):

   ```
   https://tesla.com/_ak/<domain>          # e.g. https://tesla.com/_ak/myrobotaxi.app
   ```

3. In the Tesla app: select the vehicle(s) to pair, approve the key request.
4. **Tap the key card** on the center console when prompted (required for
   security).
5. Confirm pairing succeeded — after a short delay, re-issue any
   signer-required command (e.g. `door_lock`); it should return
   `200 {"status":"applied"}` instead of `key_not_paired`.

Notes:
- Max 5 third-party apps per vehicle.
- The owner can revoke access anytime in the Tesla app (subsequent commands
  revert to `key_not_paired`).
- Unsigned commands (`navigation_request` / `navigation_gps_request`) do NOT
  require the virtual key, but still need `TESLA_PROXY_URL` configured and a
  valid owner OAuth token with `vehicle_cmds` scope.

---

## 4. Pre-pairing / degraded behavior summary

| Condition | Behavior |
|-----------|----------|
| `TESLA_PROXY_URL` unset | Startup logs "signing disabled"; endpoint mounted; signer-required commands → `403 key_not_paired`; navigation → `502 command_failed`. Server does not crash. |
| Proxy set, key not paired (MYR-115 pending) | Signer-required commands → `403 key_not_paired` (proxy/Tesla reject; mapped by the executor). |
| Owner token lacks the command scope | `403 permission_denied` — returned **without** calling Tesla (scope parsed from the token's `scp` claim). |
| Vehicle asleep | Executor wakes + retries (bounded); on exhaustion → `503 vehicle_asleep` (SDK retries with backoff). |
| Vehicle rejects (`result:false`) or counter error survives re-handshake | `502 command_failed`. |
