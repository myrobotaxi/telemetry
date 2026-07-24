# Owner car offboarding — full teardown (design)

**Status:** Design + Fleet API verification (MYR-258). **Design only — no implementation in this PR.**
Build after [MYR-257](https://linear.app/myrobotaxi/issue/MYR-257) (`store.OwnerProvisioner`) merges to avoid
colliding on the Account/Vehicle store code. This doc is the exact inverse of
[`owner-onboarding.md`](owner-onboarding.md) / MYR-257's [`self-serve-onboarding.md`].

**Goal.** An owner completely removes their car across three layers:

1. **Our app/backend** — vehicle row + drive history + encrypted route blobs gone; Tesla tokens cleared; the car stops streaming to us.
2. **Our Tesla access** — the MyRoboTaxi OAuth grant is revoked so the app loses account access and disappears from the owner's tesla.com third-party apps.
3. **The virtual key in the car** — the enrolled "MyRoboTaxi" key is removed from the vehicle keychain.

The central honest truth of this feature: **layers 1 is fully automatable; layer 2 is owner-confirmed (app-initiated, not a machine call); layer 3 is strictly owner-manual (Tesla security model).** The design makes the app-side removal immediate and authoritative, does the automatable Tesla-side cleanup best-effort, and gives the owner clear, correct instructions for the two steps only they can perform.

---

## 1. Fleet API capability verification (with citations)

### 1.1 Telemetry stream teardown — `DELETE …/fleet_telemetry_config` EXISTS ✅

- **Endpoint:** `DELETE /api/1/vehicles/{vehicle_tag}/fleet_telemetry_config` (Tesla's `delete_fleet_telemetry_config`). It is the inverse of the `create_fleet_telemetry_config` we push today. Confirmed present in Tesla's vehicle-endpoints set and in third-party mirrors of the Fleet API (Tessie "Get/Set/Delete Telemetry Config").
- **Effect:** removes the vehicle's stored telemetry configuration. The car stops streaming to `telemetry.myrobotaxi.app` once it re-reads config / reconnects — **not guaranteed instantaneous** for an already-open stream, but our receiver stops accepting it immediately regardless (see §5). Removing access "immediately remove[s] the application's ability to … stream realtime vehicle data" is the documented behavior for access removal.
- **Caveat (multi-config):** Tesla has known cases where a config cannot be deleted when multiple applications share a vehicle ([fleet-telemetry#294](https://github.com/teslamotors/fleet-telemetry/issues/294)). Not our situation today (single partner), but the teardown must treat a non-2xx delete as **best-effort, non-fatal**.
- **How it mirrors our PUSH (verified in-repo):**
  - We PUSH via `FleetAPIClient.PushTelemetryConfig` → `POST {proxyURL}/api/1/vehicles/fleet_telemetry_config` (no VIN in path; VINs in the body). This path is special because the **tesla-http-proxy JWS-signs** the config: `pkg/proxy/proxy.go:handleFleetTelemetryConfig` intercepts `POST …/fleet_telemetry_config`, signs the config with the partner command key, and forwards to `…/fleet_telemetry_config_jws`.
  - We already GET status via `FleetAPIClient.GetTelemetryConfig` → `GET {proxyURL}/api/1/vehicles/{vin}/fleet_telemetry_config` (per-VIN path, `internal/telemetry/fleet_api_vehicle_get.go`). The proxy does **not** special-case GET — it forwards it untouched to Tesla with the bearer token.
  - **DELETE follows the GET path, not the POST path.** A DELETE carries no config body to sign, so the proxy's `handleFleetTelemetryConfig` (which reads+signs a `{vins, config}` body) does not apply; the request falls through to `p.forwardRequest` and reaches Tesla unmodified. **Therefore a new `FleetAPIClient.DeleteTelemetryConfig(ctx, token, vin)` is a near-verbatim copy of `GetTelemetryConfig`** — same client, same `BaseURL = cfg.Proxy().URL`, same per-VIN URL, `http.MethodDelete`, same bearer, no JWS. Auth path is identical to the status read we already ship.

### 1.2 OAuth revoke — NOT a partner machine call; owner-confirmed consent page ✅ (verified)

There is **no Fleet-API partner endpoint that lets MyRoboTaxi revoke the owner's grant server-to-server.** What exists:

- **Consent-revoke page (the real mechanism):** `https://auth.tesla.com/user/revoke/consent?revoke_client_id={CLIENT_ID}&back_url={RETURN_URL}`. This revokes the **entire third-party grant** for our `client_id`: it invalidates issued access **and** refresh tokens and **removes MyRoboTaxi from the owner's tesla.com third-party-apps list**. It requires the owner to be logged into their Tesla account and to confirm — so it is **app-initiated, owner-confirmed**, not a silent backend call.
- **Owner's Account Security page:** the owner can independently remove the app under tesla.com → Account → Security → Third-Party Apps. Same effect.
- **Legacy `owner-api.teslamotors.com/oauth/revoke` (POST `token=…`)** exists but is the **deprecated Owner API**, revokes only the passed access token, and is **not** the Fleet API. Do **not** use it — it does not touch the refresh token or the grant.
- **What "delete our tokens locally" does vs. does not do:** deleting the `Account` row destroys *our* copy of the tokens (so we can no longer call Tesla), but it does **not** revoke the grant — the app stays listed in the owner's connected apps and the refresh token remains technically valid at Tesla until it expires or the owner revokes. **Local token deletion ≠ grant revocation.** Full cut = local delete **and** owner completes the consent-revoke page.

**Design consequence:** the teardown clears our tokens automatically and then hands the owner the consent-revoke deep link (opened in `ASWebAuthenticationSession`) as the honest "fully remove our access at Tesla" step.

### 1.3 Virtual-key removal — OWNER-MANUAL only ✅ (confirmed definitively)

**No Fleet API path removes the partner's own virtual key. It is strictly owner-manual in the Tesla app / car.** Evidence:

- **No REST endpoint.** Tesla Fleet API exposes no "remove key" / "unpair" REST call. Key management lives in the vehicle (VCSEC) or the Tesla app. `vehicle-command@v0.4.1` contains **no** revoke/remove-key REST helper (grep: only `pkg/vehicle/security.go`).
- **The only programmatic removal is a VCSEC whitelist op** — `(*Vehicle).RemoveKey` (`pkg/vehicle/security.go:268`) sends `WhitelistOperation_RemovePublicKeyFromWhitelist` over an **authenticated command session**. Two independent blockers make this unusable for self-teardown:
  1. **It needs a live command session** authenticated with a key that can manage the whitelist. After we revoke/clear tokens we cannot establish one.
  2. **Firmware policy forbids it for cloud keys.** `pkg/protocol/protocol.md` (Roles): on **2023.38+**, a **Fleet Manager** (a cloud-based Owner key — exactly what MyRoboTaxi's virtual key is) **"cannot add or remove other users' keys from the vehicle."** Cloud-key self-management of the whitelist is not available.
- **No deep link to the removal screen.** The `tesla.com/_ak/{domain}` deep link only **adds** a key (it opens the "Set Up Third-Party Virtual Key" prompt, e.g. `tesla.com/_ak/tessie.com`). There is **no** documented `_ak`-style or app deep link that targets the key-**removal** screen. The owner must navigate there manually.
- **Owner-manual steps (car touchscreen — the reliable path):** **Controls → Locks →** find the **"MyRoboTaxi"** key in the list → **tap it → Remove/Delete Key →** authenticate by tapping an enrolled **key card** on the center console. Removing the key "immediately … remove[s] the application's ability to send commands and stream realtime vehicle data." (Some app builds surface the same list under the vehicle's **Security & Drivers / Keys** section, but Controls → Locks on the car is the canonical, always-present path.)

---

## 2. Automatable-vs-owner-manual matrix

| Teardown action | Layer | Automatable? | Mechanism | Notes |
|---|---|---|---|---|
| Stop the car streaming to us | Tesla | **Yes (best-effort)** | `DELETE /api/1/vehicles/{vin}/fleet_telemetry_config` via proxy (forwarded, unsigned) | Needs a valid token → must run **before** token deletion/revoke. Non-fatal on failure (config self-expires; receiver rejects anyway). |
| Reject any live inbound stream now | Backend | **Yes** | `Vehicle` row DELETE → `vehicle_deleted` NOTIFY → dispatcher evicts VIN cache + closes mTLS stream | Automatic side effect of the local delete. |
| Delete vehicle + drives + route blobs | Backend | **Yes** | Prisma cascade from `Vehicle` delete | `Drive`, `TripStop`, vehicle-scoped `Invite`, all `*Enc` blobs cascade. |
| Clear our Tesla tokens | Backend | **Yes** | Delete/clear the `Account` (tesla) row | Removes *our* access; does **not** revoke the grant. |
| Reset link/pairing flags | Backend | **Yes** | `Settings.teslaLinked=false`, `virtualKeyPaired=false`, clear pairing reminders | Mirrors `unlinkTesla`. |
| Audit the teardown | Backend | **Yes** | INSERT `vehicle_deleted` (and/or `account_deleted`) AuditLog row in the same txn | Append-only; user-initiated. |
| Revoke the OAuth grant at Tesla | Tesla | **App-initiated, owner-confirmed** | Open `auth.tesla.com/user/revoke/consent?revoke_client_id=…&back_url=myrobotaxi://tesla-unlinked` | No silent partner API. Owner must confirm; then app disappears from tesla.com third-party apps. |
| Remove the "MyRoboTaxi" virtual key | Car | **No — owner-manual** | Tesla app/car: Controls → Locks → the key → Remove (card auth) | No Fleet API, no deep link, blocked for cloud keys on 2023.38+. |

---

## 3. Data-deletion scope

### 3.1 Two modes — "Remove this car" vs. "Delete my account"

| | **Remove this car (unlink)** — this issue's default | **Delete my account** — existing FR-10.1 |
|---|---|---|
| Scope | One `Vehicle` (by `vehicleId`/VIN) | The whole `User` |
| Keeps user account? | **Yes** (Apple identity, `go_users`, `User` row all survive) | No |
| Tesla `Account` tokens | Cleared **if this was the owner's last linked vehicle** (see §3.3) | Deleted (cascade) |
| Existing impl | `unlinkTesla()` (deletes **all** the user's vehicles, no per-VIN, no audit, no Tesla-side calls) | `DELETE /api/users/me` (txn + `account_deleted` audit + `user.delete` cascade) |
| Audit action | `vehicle_deleted` | `account_deleted` |

MYR-258 delivers the **per-vehicle unlink** and reuses the existing account-delete path unchanged. "Delete my account" already meets GDPR erasure via `DELETE /api/users/me`.

### 3.2 Per-table teardown table (authoritative Prisma cascade from `prisma/schema.prisma`)

Deleting one `Vehicle` row cascades exactly (all FKs `onDelete: Cascade`):

| Table | On single-vehicle unlink | How | P-class data destroyed |
|---|---|---|---|
| `Vehicle` | **Deleted** (the target row) | Explicit delete by `id` (owner-scoped) | GPS `latitude/longitude(+Enc)`, destination/origin `(+Enc)`, `navRouteCoordinates(+Enc)` (P1) |
| `Drive` | **Deleted** (all for this vehicle) | Cascade (FK→Vehicle) | `routePoints` + `routePointsEnc` recorded route blobs (P1) |
| `TripStop` | **Deleted** (all for this vehicle) | Cascade (FK→Vehicle) | Stop names/addresses |
| `Invite` (vehicle-scoped) | **Deleted** | Cascade (FK→Vehicle) | Sharee emails/labels |
| `Account` (tesla) | **Deleted/cleared iff last vehicle** (§3.3) | Explicit, owner+provider scoped | `access/refresh/id_token(_enc)` OAuth tokens (P0) |
| `Settings` | **Updated** (`teslaLinked`/`virtualKeyPaired`=false) iff last vehicle | `upsert` | flags only |
| `User` | **Retained** | — | — |
| `go_users` / `go_identity_apple` | **Retained** | — | Apple identity binding survives; the owner stays signed in |
| `AuditLog` | **+1 row** (`vehicle_deleted`), never deleted | INSERT-only | NFR-3.29: indefinite; metadata is P0 counts only |

Retained-by-design (matches data-lifecycle §3.3): `AuditLog` (indefinite, orphaned `userId` intentional), the user's identity, and any **other** vehicles the owner keeps.

### 3.3 FK / NOT-NULL / ownership considerations

- **`Account.userId`, `Vehicle.userId` → `User.id` (`onDelete: Cascade`).** Single-vehicle unlink deletes the `Vehicle` by its own PK; the `User` is untouched, so no cascade fires upward. Safe.
- **Do NOT delete the `Account` while the owner still has other linked vehicles** — the remaining vehicles' telemetry auth and token refresh depend on it. Only clear tokens when the deleted vehicle was the owner's **last** one (count `Vehicle WHERE userId=…` inside the txn = 0 after delete). This is why per-vehicle unlink is not just "reuse `unlinkTesla`" (which nukes all vehicles).
- **`Settings.userId` is `@unique`, NOT NULL** — use `upsert`, never insert-blind (an owner may have no `Settings` row yet).
- **`Vehicle.vin`/`teslaVehicleId` are `@unique` nullable** — a pre-pairing vehicle may have a null VIN; the teardown and the `vehicle_deleted` trigger both tolerate empty VIN (the dispatcher skips the mTLS-close branch — verified in `events/vehicle_deleted_event.go`).
- **`AuditLog`:** append-only (DB triggers block UPDATE/DELETE; `contract-guard` CG-DL-2). The teardown writer may only **INSERT**.

### 3.4 Ownership rule — the teardown writer (mirror MYR-257 in reverse)

The established boundary (data-lifecycle §3.4 / rest-api §10 DV-23, MYR-69) is that the **Next.js app owns Prisma deletes + the user-initiated audit row**. MYR-257 deliberately widened this for onboarding with a **sanctioned, single, transactional, audited carve-out** — `store.OwnerProvisioner` (`internal/store/owner_provision.go`) — gated by `sdk-architect` + `contract-guard` and a data-lifecycle §1.4 update.

**MYR-258 mirrors that symmetrically:** a new **`store.OwnerTeardown`** in `internal/store/` — the reverse of `OwnerProvisioner`:

| | `OwnerProvisioner` (MYR-257) | `OwnerTeardown` (MYR-258) |
|---|---|---|
| Trigger | successful `/tesla/link/callback` | owner "Remove this car" |
| `User` | `INSERT … ON CONFLICT DO NOTHING` | **untouched** (per-vehicle) |
| `Vehicle` | upsert identity rows | **DELETE by id** (cascades) |
| `Account` | upsert encrypted tokens | **DELETE iff last vehicle** |
| `Settings` | `teslaLinked=true` | `teslaLinked/virtualKeyPaired=false` iff last vehicle |
| `AuditLog` | (n/a) | INSERT `vehicle_deleted` |
| Properties | one txn, idempotent, audited | one txn, idempotent, audited |

It is **not** the identity module (ADR-001 §4 "identity keeps `User` read-only" stays intact) and adds **no migration** (runtime upserts/deletes, same class as `AccountRepo`) so CG-DL-9 is respected. **Requires the same gates as #257: a `docs/contracts/data-lifecycle.md` §1.4 update granting Go narrow DELETE access to `Vehicle`/`Account` + INSERT of the user-initiated `vehicle_deleted` audit row, plus `sdk-architect` + `contract-guard` review and cross-repo schema verification against the Next.js Prisma source.**

> **Alternative considered (tradeoff):** keep the deletes in Next.js by hardening the existing `unlinkTesla()` server action (add per-VIN scope, the `vehicle_deleted` audit row, the Tesla `DELETE fleet_telemetry_config` call, and the revoke handoff). This respects DV-23 without a new carve-out, **but** splits the teardown across two repos/round-trips for an iOS client that already talks only to the Go backend for `/api/tesla/*`, and it duplicates the token-resolution + proxy plumbing that already lives in Go. Given MYR-258 explicitly asks to mirror #257's writer in reverse and is filed in the telemetry repo, the Go `OwnerTeardown` is the recommended primary; `unlinkTesla` should be updated to delegate or be deprecated to avoid two divergent teardown paths.

---

## 4. Endpoint / API shape to build

New authenticated backend endpoint (telemetry server; iOS already targets this backend for `/api/tesla/link/*`):

```
DELETE /api/tesla/vehicles/{vehicleId}
Authorization: Bearer <app session JWT>        # owner identity = JWT sub
```

Handler outline (`internal/telemetry` handler + `store.OwnerTeardown`, wired like the fleet-config handlers):

1. **AuthZ:** validate bearer → `userId`; verify `vehicleId` belongs to `userId` (reuse the `verifyVINOwnership`/vehicle-authorizer pattern). 403 on mismatch. Resolve the VIN.
2. **Resolve Tesla token** (`AccountRepo.GetTeslaToken` + refresh, exactly like `fleet.go` push). If absent → skip step 3.
3. **`FleetAPIClient.DeleteTelemetryConfig(ctx, token, vin)`** (new; mirrors `GetTelemetryConfig`). Treat any error / non-2xx / 404 as **best-effort success** — log, continue. Skip for empty VIN.
4. **`store.OwnerTeardown.RemoveVehicle(ctx, userId, vehicleId)`** — one `pgx.Tx`:
   - `DELETE FROM "Vehicle" WHERE id=$1 AND "userId"=$2` (cascades Drive/TripStop/Invite; fires `vehicle_deleted` NOTIFY).
   - `SELECT count(*) FROM "Vehicle" WHERE "userId"=$2` → if 0: `DELETE FROM "Account" WHERE "userId"=$2 AND provider='tesla'` and `upsert Settings … teslaLinked=false, virtualKeyPaired=false, keyPairingReminderCount=0`.
   - `INSERT` `vehicle_deleted` AuditLog (`targetType='vehicle'`, `targetId=vehicleId`, `initiator='user'`, `metadata={driveCount, wasLastVehicle}` — P0 counts only, CG-DL-5).
5. **Respond** with the honest post-state + owner instructions:

```json
{
  "removed": true,
  "wasLastVehicle": true,
  "teslaTokensCleared": true,
  "streamConfigDeleted": true,          // false if the Tesla DELETE failed
  "revokeUrl": "https://auth.tesla.com/user/revoke/consent?revoke_client_id=<CLIENT_ID>&back_url=myrobotaxi://tesla-unlinked",
  "virtualKeyRemoval": {
    "required": true,
    "automatable": false,
    "steps": ["Open the Tesla app or your car's touchscreen",
              "Go to Controls → Locks",
              "Tap the “MyRoboTaxi” key",
              "Tap Remove/Delete Key",
              "Authenticate by tapping a key card on the center console"]
  }
}
```

The in-process WS/stream/cache cleanup is **not** in the handler — it happens automatically via the `vehicle_deleted` NOTIFY → `vehicleDeletedDispatcher` (verified: `cmd/telemetry-server/vehicle_deleted_dispatcher.go`).

---

## 5. Ordering, idempotency, and failure

### 5.1 Safe order of operations

```
1. Resolve Tesla token (needed for step 2; skip 2 if none)
2. Tesla: DELETE fleet_telemetry_config     ← MUST precede token deletion/revoke
3. Local txn: DELETE Vehicle (+cascade) → [if last] DELETE Account, reset Settings → INSERT audit
   └─ (async, automatic) vehicle_deleted NOTIFY → dispatcher: evict VIN cache, close WS, close mTLS stream
4. Return instructions → iOS opens the consent-revoke URL (owner confirms)  ← LAST (kills the token)
5. Owner removes the virtual key in the Tesla app (manual, anytime)
```

Rationale: **the local delete (step 3) is the source of truth for "gone from the app immediately."** Everything Tesla-side is best-effort cleanup layered around it. Step 2 comes before step 3/4 because it needs a live token; revoke (step 4) is last because it destroys the token and can't be undone silently.

### 5.2 What if the Tesla calls fail

- **`DELETE fleet_telemetry_config` → 500 / offline / 404:** non-fatal. The local delete still runs, so the owner's data is gone from us — the privacy guarantee holds. Residue: the stale config lingers at Tesla until it self-expires (we push `exp` ≈350 days) and the car may keep trying to connect, but **our receiver rejects it immediately** — the `vehicle_deleted` dispatcher evicts the VIN cache so the next inbound mTLS upgrade for that VIN fails authorization. Retryable.
- **Local txn fails:** atomic — nothing deleted, no audit row (data-lifecycle §3.4 atomicity). Retryable. If step 2 already ran, that's fine (idempotent).
- **Revoke skipped / owner cancels the consent page:** our tokens are already deleted, so the app is in a clean state and cannot call Tesla. Only residue is the Tesla-side grant (still listed in connected apps) + the key — both owner-manual anyway. The owner ends in a clean **app** state regardless.

### 5.3 Idempotency — re-running teardown is safe

- `DELETE Vehicle … WHERE id AND userId` on an already-deleted row → 0 rows affected (not an error).
- `DELETE Account` / `Settings upsert` → idempotent.
- `DELETE fleet_telemetry_config` on an already-deleted config → 404/2xx, both treated as success.
- Audit is INSERT-only; a re-run appends a second `vehicle_deleted` row (acceptable — audit is a log, and re-runs are rare). Optionally short-circuit to a 200 no-op (and skip the audit) when the vehicle is already gone, to avoid duplicate rows.
- The consent-revoke page and key-removal are naturally idempotent (already-revoked/already-removed → no-op for the owner).

---

## 6. iOS flow

**Where it lives:** Settings → the vehicle detail (the same "Tesla Account" list / `OwnerHomeState.linkedVehicles` surface that MYR-246 wired for "Add another Tesla"). Add a destructive **"Remove this car"** row at the bottom of each vehicle's detail.

**Destructive-confirmation pattern (reuse what the app already has):** the app's existing `ConfirmDialog` destructive pattern (as used for account deletion) — a sheet titled **"Remove this car?"**, body listing exactly what happens ("Deletes this car, its drive history, and clears MyRoboTaxi's access to it. This can't be undone."), a red **Remove** button + **Cancel**. For a last-vehicle removal that also clears the Tesla link, the body should say so.

**Flow:**
1. Tap **Remove this car** → `ConfirmDialog`.
2. On confirm → `RestClient.delete(["tesla","vehicles", vehicleId])` (authenticated). Spinner.
3. On success → the vehicle disappears from the list **immediately** (optimistic + server-confirmed). Show a **post-teardown sheet** ("Car removed") with two clearly-optional follow-ups, framed honestly:
   - **"Remove MyRoboTaxi from your Tesla account"** → opens `response.revokeUrl` in `ASWebAuthenticationSession` (`callbackURLScheme: "myrobotaxi"`, resolves on `myrobotaxi://tesla-unlinked`). Copy: "We've deleted our copy of your Tesla tokens. To fully remove MyRoboTaxi from your Tesla account, revoke access here." (Skippable; only relevant when `teslaTokensCleared && wasLastVehicle`.)
   - **"Remove the key from your car"** → renders `response.virtualKeyRemoval.steps` verbatim (Controls → Locks → MyRoboTaxi → Remove → card auth). Copy: "Only you can do this, in the Tesla app or on your car's screen." **No deep link exists for this screen** — present it as instructions, not a button.
4. Post-state honesty: the car is gone from the app the instant step 3 returns; the sheet makes clear the two Tesla-side steps are the owner's to complete and that skipping them leaves an (inert, since tokens are gone) grant + key on the Tesla side.

**Deep-link addition:** register `myrobotaxi://tesla-unlinked` (sibling to the existing `myrobotaxi://tesla-linked` from MYR-246) as the `back_url` for the consent-revoke session.

---

## 7. Summary of key verified facts

- **`DELETE /api/1/vehicles/{vin}/fleet_telemetry_config` exists** and stops the car streaming (best-effort; multi-config caveat). It rides the **same proxy/auth path as our existing GET status** — forwarded unsigned, unlike the JWS-signed POST push. A `DeleteTelemetryConfig` client method is a copy of `GetTelemetryConfig`.
- **OAuth revoke is not a partner machine call.** Deleting our `Account` tokens removes *our* access but does **not** revoke the grant. Full cut = the owner-confirmed consent page `auth.tesla.com/user/revoke/consent?revoke_client_id=…`, which kills access+refresh tokens and removes the app from tesla.com third-party apps.
- **Virtual-key removal is owner-only.** No Fleet API endpoint, no deep link; the sole programmatic path (VCSEC `RemoveKey`) needs a live session and is blocked for cloud/Fleet-Manager keys on 2023.38+. Owner does it in Tesla app: Controls → Locks → the key → Remove (card auth).
- **Recommended teardown sequence:** delete stream config (while token valid) → local transactional delete (Vehicle cascade + last-vehicle token/Settings clear + `vehicle_deleted` audit; NOTIFY handles WS/stream/cache cleanup) → hand the owner the revoke deep link → owner removes the key manually. Local delete is authoritative and succeeds even if every Tesla call fails; every step is idempotent and re-runnable.

---

## References

- Repo: `internal/telemetry/fleet_api.go`, `fleet_api_vehicle_get.go`, `fleet_config_status_handler.go`; `cmd/ops/fleet.go`; `cmd/telemetry-server/vehicle_deleted_dispatcher.go`; `internal/store/notify_listener.go`, `account_repo.go`; `internal/teslaauth/oauth.go`; `internal/teslalink/handler.go`; `prisma/schema.prisma`, `src/features/settings/api/actions.ts` (`unlinkTesla`), `src/app/api/users/me/route.ts` (react-frontend).
- Contracts: `docs/contracts/data-lifecycle.md` §3–§4; `docs/architecture/owner-onboarding.md`; MYR-257 `self-serve-onboarding.md` + PR #312 (`store.OwnerProvisioner`).
- `github.com/teslamotors/vehicle-command@v0.4.1`: `pkg/proxy/proxy.go` (`handleFleetTelemetryConfig`), `pkg/vehicle/security.go` (`RemoveKey`), `pkg/protocol/protocol.md` (Roles / Fleet Manager 2023.38+).
- Tesla: developer.tesla.com Fleet API vehicle-endpoints (`create/delete_fleet_telemetry_config`), virtual-keys developer-guide/overview; `auth.tesla.com/user/revoke/consent`; [fleet-telemetry#294](https://github.com/teslamotors/fleet-telemetry/issues/294); Tessie Get/Set/Delete Telemetry Config reference.
