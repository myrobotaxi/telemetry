# Owner onboarding — adding a second Tesla owner as a tester

**Status:** MVP design + runbook (MYR-246). Backend Tesla-link endpoints land in
this issue (§7.11 of [`../contracts/rest-api.md`](../contracts/rest-api.md));
the remaining steps are a mix of existing tooling and documented ops actions.

**Goal:** onboard a *second* real Tesla owner (tester #2) end-to-end —
Apple sign-in → user record → Tesla OAuth link (in-app) → vehicle sync →
virtual-key pairing → fleet telemetry config push — so their car streams live
telemetry and they can use the app exactly like the first owner (Lunar).

---

## 0. Why this is not "just do what the first owner did"

The **first** owner predates the Go identity system. Their onboarding was
hand-assembled by an operator:

- Their Prisma `User` + `Account` rows were created by the **Next.js web app**
  (NextAuth Google sign-in, then a web Tesla OAuth link that ran
  `syncVehiclesFromTesla`).
- Their Tesla tokens were (re)written directly by `ops auth link` — a localhost
  `:8765` PKCE flow that only an operator with `AUTH_TESLA_ID/SECRET`,
  `ENCRYPTION_KEY`, and `DATABASE_URL` can run. **Not usable by a real tester.**

A second owner who onboards **iOS-first** hits three structural facts that the
first owner never did:

1. **iOS auth is Sign in with Apple against the Go backend**, which mints an
   Apple-native user in the Go-owned `go_users` table — *not* a Prisma `User`
   row (see `internal/identity/linkage.go` precedence rule (4)).
2. **The Tesla `Account` row is Prisma-owned** and `Account.userId` is a foreign
   key to `"User"."id"` (`onDelete: Cascade`). An Apple-native `go_users` id is
   **not** in `"User"`, so tokens cannot be attached to it.
3. **`Vehicle.userId` is likewise an FK to `"User"`** — vehicle ownership,
   telemetry authorization (`internal/auth` `queryUserVehicleIDs`), and RBAC all
   key off a Prisma `User` cuid.

**Conclusion (the central identity decision):** tester #2 must resolve to a
**Prisma `User` cuid**, not a fresh `go_users` id, or the Tesla `Account` and
`Vehicle` rows have nothing valid to hang off. The existing
`AUTH_APPLE_BOOTSTRAP` mechanism is exactly the sanctioned bridge for this.

---

## 1. Identity path — how tester #2 gets a Prisma `User` row and binds to it

`internal/identity` resolves an Apple sign-in to a user cuid with this
precedence (`resolveUser` / `resolveFirstSignIn`):

1. An existing `go_identity_apple` binding for the Apple `sub` wins outright.
2. **First sign-in: config bootstrap override** — `AUTH_APPLE_BOOTSTRAP`
   (`email=cuid[,email=cuid]`) maps a verified email to an existing cuid.
3. First sign-in: verified-email match against the Prisma `"User"` table.
4. First sign-in: mint a fresh Apple-native user in `go_users` (**the wrong
   table for our FK needs**).

### Decision: provision a Prisma `User`, then bind Apple → that cuid via bootstrap

For the MVP we deliberately steer tester #2 to precedence **(2)** (or (3)):

- **Provision a Prisma `User` row** for tester #2. Least-effort, most-sanctioned
  path: have tester #2 sign in **once on the web app** (NextAuth Google) — this
  creates the `"User"` row automatically and gives us their cuid + email. (SQL-
  inserting a `"User"` row by hand also works but must generate a cuid and is a
  raw write to a Prisma-owned table; prefer the web sign-in.)
- **Bind their Apple identity to that same cuid** by adding
  `AUTH_APPLE_BOOTSTRAP=<tester_email>=<their_prisma_cuid>` (append to the
  existing map; comma-separated). This guarantees their iOS Apple sign-in binds
  to the Prisma `User` even when Apple hands us a private-relay address that
  email-match (3) would miss.

Once bound, the JWT `sub` the app carries **is the Prisma `User` cuid**, so the
Tesla `Account` write (§2) and every `Vehicle.userId` (§3) are FK-valid.

| Step | MVP — who does it | Future fully-self-serve |
|------|-------------------|-------------------------|
| Create the Prisma `User` row | **Ops** (tester does one web Google sign-in, or SQL insert). | Go identity mints the row on Apple sign-in **and** provisions a matching `"User"` row (or the schema drops the Account/Vehicle FK-to-`User` requirement so `go_users` ids are first-class owners). |
| Bind Apple `sub` → Prisma cuid | **Ops** — add `AUTH_APPLE_BOOTSTRAP` entry (one env change + restart). | Automatic email/one-time-code binding at first sign-in; no per-tester env edits. |
| Apple sign-in itself | **User** — in the app. | Same. |

---

## 2. Tesla OAuth link (in-app) — Part B of MYR-246

The car owner must grant the MyRoboTaxi Tesla app access to their vehicle
(tokens stored encrypted in the Prisma `Account` row). Historically this was
web-only (NextAuth `tesla` provider) or `ops auth link`. MYR-246 adds an
**in-app** path so tester #2 never needs the web for Tesla.

**New backend endpoints (this issue — §7.11 of the REST contract):**

- `POST /api/tesla/link/start` — owner-authenticated; returns the Tesla
  authorize URL (full scope set + `prompt_missing_scopes=true`) with a
  `redirect_uri` pointing at the backend callback. Mints a 10-min single-use
  PKCE+state session bound to the calling user.
- `GET /api/tesla/link/callback` — unauthenticated Tesla redirect target;
  validates `state`, exchanges the `code` (shared `internal/teslaauth` logic),
  persists tokens via `AccountRepo.UpdateTeslaToken` (existing encrypted dual-
  write), then `302`s to `myrobotaxi://tesla-linked?status=…`.

**iOS side (design — implemented in the iOS repo, MYR-115 seam):** the existing
`AddTeslaFlow` already has an injection point `var authenticate:
TeslaAuthenticator?` (today always `nil` → simulated sheet). Wire a real
`ASWebAuthenticationSession` authenticator:

1. Call `RestClient.post(["tesla","link","start"], …)` (authenticated path,
   attaches the session Bearer) → `{ authorizeUrl, state }`.
2. `ASWebAuthenticationSession(url: authorizeUrl, callbackURLScheme:
   "myrobotaxi")` → user consents on Tesla.
3. Backend callback runs the exchange and `302`s to
   `myrobotaxi://tesla-linked?status=success`; `ASWebAuthenticationSession`
   intercepts the custom-scheme redirect and resolves `.granted`.
4. On `.granted`, `AddTeslaFlow` advances to its "linked" phase; the live
   `Settings` "Tesla Account" list (`OwnerHomeState`/`linkedVehicles`) reflects
   the newly synced vehicles.

**iOS project changes required (design):**
- Register the `myrobotaxi` URL scheme. There is **no** `CFBundleURLTypes` today
  and the project uses XcodeGen with `GENERATE_INFOPLIST_FILE: YES`, so add it
  under the target's `info.properties` in `project.yml` and regenerate.
- Add a `TeslaAuthenticator` implementation backed by `ASWebAuthenticationSession`
  + `ASWebAuthenticationPresentationContextProviding` (none exists yet).
- Add `RestClient.teslaLinkStart()` following the `AuthPayloads.swift` "author
  the wire shape here until contracts codegen catches up" precedent; it uses the
  **authenticated** `post([...])` pipeline (not the pre-auth `sendAuth` path).

**Deep-link scheme (documented for iOS):** `myrobotaxi://tesla-linked` with a
single query param `status` (`success` | `error`) and, on error, `reason`
(`tesla_denied` | `invalid_state` | `missing_code` | `exchange_failed` |
`account_not_provisioned` | `persist_failed`). No tokens or PII are ever placed
in the deep link.

| Aspect | MVP | Future fully-self-serve |
|--------|-----|-------------------------|
| Token persistence | `UpdateTeslaToken` (**UPDATE** — the `Account` row must pre-exist, §2.1). | Server-side `Account` **upsert** (INSERT with the real Tesla `providerAccountId` from a `userinfo` call). |
| Who links | **User**, in-app (Part B). | Same. |
| `Account` row existence | **Ops** pre-seeds (§2.1). | Handled by the upsert above. |

### 2.1 The `Account`-row prerequisite (MVP limitation)

`UpdateTeslaToken` is an `UPDATE ... WHERE "userId"=$1 AND "provider"='tesla'`.
For a brand-new tester there is no such row yet, so the callback returns
`account_not_provisioned`. Two ways to satisfy the prerequisite for the MVP:

- **Preferred:** tester #2 does the **web** Tesla link **once** (NextAuth `tesla`
  provider on `myrobotaxi.app`). This creates the `Account` row *and* runs
  `syncVehiclesFromTesla` (covers §3 too). Thereafter the in-app link just
  refreshes/relinks tokens on the same Prisma user.
- **Or:** an operator seeds a `tesla` `Account` row for the cuid (raw SQL) with a
  placeholder `providerAccountId`; the in-app callback then fills tokens.

The **fully self-serve** fix (future) is a server-side `Account` upsert: on
callback, GET Tesla `userinfo` with the fresh access token to obtain the Tesla
`sub`, then `INSERT ... ON CONFLICT (provider, providerAccountId) DO UPDATE`
with `type='oauth'`, `provider='tesla'`, `providerAccountId=<sub>`, dual-write-
encrypted tokens. This removes the pre-provisioning step entirely and is
collision-safe against a later web link. It is **out of scope for MYR-246**
(touches the Prisma-owned `Account` insert surface + a new `internal/store`
write path under `sdk-architect`/`contract-guard` review).

---

## 3. Vehicle sync — who writes the `Vehicle` rows

The `Vehicle` rows (keyed on `teslaVehicleId @unique`, `userId` FK to `User`)
are written **today only by the Next.js app** — `syncVehiclesFromTesla`
(`react-frontend/src/features/vehicles/api/sync.ts`): it lists vehicles
(`GET /api/1/vehicles`), fetches `vehicle_data`, checks pairing via
`fleet_status`, and `prisma.vehicle.upsert`s the full column set (name, model,
year, charge, GPS + encrypted shadows, status, setupStatus, virtualKeyPaired,
…). The Go telemetry server has **no** equivalent writer — `cmd/ops vehicles
list` and `VehicleRepo` only *read* `Vehicle`, and the live telemetry pipeline
*updates* existing rows but never *creates* them.

**Decision:** for the MVP, **reuse `syncVehiclesFromTesla`** — it already exists,
handles pairing detection + fleet-config push, and writes the correct columns.
It runs when the tester's browser session hits the web app (or an operator
triggers the sync route). Because the DB is shared, the Go server immediately
sees the new `Vehicle` rows for telemetry auth.

| Aspect | MVP | Future fully-self-serve |
|--------|-----|-------------------------|
| Who writes `Vehicle` rows | Next.js `syncVehiclesFromTesla`, triggered by the tester visiting the web app once (or an ops-triggered sync). | A **server-side Go sync** (new `POST /api/tesla/vehicles/sync` reading Fleet `GET /api/1/vehicles` and inserting `Vehicle` rows), so the app never needs the web. Reuses the same mapping (`tesla-mapper.ts` equivalent). |
| Trigger | **User** (web visit) or **Ops**. | **User**, in-app, right after the Tesla link callback. |

---

## 4. Virtual-key pairing — partner domain + deep link + verification

**Partner domain (determined from the repos): `myrobotaxi.app`.** It is
hardcoded in `react-frontend/scripts/register-tesla-partner.ts` (`DOMAIN =
'myrobotaxi.app'`) and `react-frontend/src/lib/constants.ts`
(`TESLA_KEY_PAIRING_URL = 'https://tesla.com/_ak/myrobotaxi.app'`). The partner
public key is hosted at
`https://myrobotaxi.app/.well-known/appspecific/com.tesla.3p.public-key.pem`
(a real P-256 key in `react-frontend/public/…`).

**No new partner registration is needed.** The partner account, telemetry keys,
mTLS, and the signing virtual key (`TESLA_KEY_FILE_B64` → tesla-http-proxy
sidecar) are already live for the first car (Lunar). Tester #2's car pairs to
the **same** partner virtual key.

**Pairing action (user):** open the deep link
`https://tesla.com/_ak/myrobotaxi.app` on the phone that has the Tesla app; the
Tesla app prompts the owner to **add the virtual key** to their vehicle. This is
a Tesla-app action; nothing in our backend performs it.

**Verifying pairing status:** call Tesla Fleet `POST /api/1/vehicles/fleet_status`
with `{ "vins": [vin] }` and check the VIN is in `key_paired_vins`
(`react-frontend/src/lib/tesla-client.ts` `getFleetStatus`; `syncVehiclesFromTesla`
already does this and flips `Vehicle.virtualKeyPaired` / `setupStatus`
accordingly). A paired car also starts returning `drive_state` in
`vehicle_data`, which the sync treats as a secondary paired signal.

| Aspect | MVP | Future fully-self-serve |
|--------|-----|-------------------------|
| Partner registration | Already done (shared). | Same. |
| Trigger the `_ak` deep link | **User** taps it (the app already imports `TESLA_KEY_PAIRING_URL`; the iOS app should surface the same deep link / a "Pair virtual key" CTA). | Same (Tesla requires the owner to approve in the Tesla app; cannot be automated). |
| Confirm pairing | **Ops/automatic** via `fleet_status` (sync flips `virtualKeyPaired`). | In-app: the app polls a backend "pairing status" read and advances the setup banner. |

---

## 5. Fleet telemetry config push — start the stream for the new VIN

Once paired, the vehicle must be told to stream telemetry to
`telemetry.myrobotaxi.app`. The **fleet telemetry config** (`fleet_telemetry_config`
with our field set + CA + hostname) is pushed via the Go server's
`POST /api/fleet-config/{vin}` handler (which calls the tesla-http-proxy), and
the ops CLI wraps this as **`ops fleet-config push --vin <newVIN> --user-id
<prismaCuid>`** (`cmd/ops/fleet.go`). The Next.js `pushFleetConfig` also targets
this same Go endpoint on the pairing transition.

| Aspect | MVP | Future fully-self-serve |
|--------|-----|-------------------------|
| Push the config | **Ops** runs `ops fleet-config push --vin … --user-id …`, **or** `syncVehiclesFromTesla`'s pairing-transition auto-push does it. | In-app trigger after pairing confirmation (the app calls `POST /api/fleet-config/vehicle/{vehicleId}` — already exists, vehicleId-keyed). |
| Verify streaming | **Ops** — `ops fields watch --vin <newVIN>` / testbench, or wait for the `Vehicle` row's `lastUpdated` to advance. | In-app "connected" state driven by live telemetry. |

---

## 6. Runbook — onboard tester #2 THIS WEEK (today's code + Part B)

Prereqs (one-time, already true for prod): partner registered, telemetry keys +
mTLS live, signing virtual key in the proxy, `AUTH_TESLA_ID/SECRET`,
`ENCRYPTION_KEY`, `AUTH_ES256_PRIVATE_KEY`, `APPLE_NATIVE_CLIENT_ID` all set.

1. **Provision the Prisma user (ops).** Have tester #2 sign in once on
   `https://myrobotaxi.app` with Google. Grab their `"User"."id"` (cuid) and
   email:
   `ops` has no user-create command — read the cuid via the web session or
   `SELECT id,email FROM "User" WHERE email='<tester_email>';`.
2. **Bind Apple → cuid (ops).** Append to the telemetry server's env:
   `AUTH_APPLE_BOOTSTRAP=<existing entries>,<tester_email>=<their_cuid>` and
   redeploy/restart. (Verified-email match can substitute if their Apple email
   equals their web email and is not a private relay — bootstrap is the reliable
   path.)
3. **Seed the Tesla `Account` row (ops) — or use the web link once.**
   Simplest: while tester #2 is on the web app, have them complete the
   **web Tesla link** (NextAuth `tesla`). This creates the `Account` row and runs
   `syncVehiclesFromTesla` (does step 5's vehicle rows + step 6 pairing check for
   free). If you skip the web link, seed a `tesla` `Account` row for the cuid so
   the in-app callback's `UpdateTeslaToken` finds a row.
4. **Register the backend redirect URI (ops — Tesla portal).** In the Tesla
   developer portal, add `https://telemetry.myrobotaxi.app:4443/api/tesla/link/callback`
   (the value of `TESLA_LINK_REDIRECT_BASE_URL` + `/api/tesla/link/callback`) to
   the app's Allowed Redirect URIs. Set `TESLA_LINK_REDIRECT_BASE_URL` on the
   server to that same origin and restart. *(If Tesla rejects a non-standard
   port, use whichever public origin maps to the client mux and register that
   exact value — the callback path is fixed.)*
5. **Tester links Tesla in-app (user).** In the iOS app: Settings → "Add another
   Tesla" → the real `ASWebAuthenticationSession` flow (Part B). Consent on
   Tesla. The callback persists tokens and deep-links back
   `myrobotaxi://tesla-linked?status=success`.
6. **Pair the virtual key (user).** Tester opens
   `https://tesla.com/_ak/myrobotaxi.app` in the Tesla app and approves adding
   the key to their car.
7. **Confirm pairing + push telemetry config (ops).** Run
   `ops fleet-config push --vin <newVIN> --user-id <cuid>` (or let the web sync's
   auto-push handle it). Confirm with `ops fields watch --vin <newVIN>` or by
   watching the `Vehicle` row's `lastUpdated`.
8. **Verify (ops/user).** Tester #2's car shows live in the app (Settings "Tesla
   Account" list + Home). Confirm the granted Tesla JWT `scp` includes
   `vehicle_cmds` / `vehicle_charging_cmds` (`ops auth token --user-id <cuid>` →
   decode) so commands + dispatch work.

If step 3 used the web link, steps 5 is optional (in-app link becomes a
re-link/refresh) and step 5's real value is proving the in-app path for the next
tester.

---

## 7. Exact manual steps required from Thomas

1. **Tesla developer portal — add the redirect URI**
   `https://telemetry.myrobotaxi.app:4443/api/tesla/link/callback` (Part B's
   backend callback) to the Fleet app's Allowed Redirect URIs, alongside the
   existing web (`https://myrobotaxi.app/api/auth/callback/tesla`) and CLI
   (`http://localhost:8765/callback`) entries.
2. **Server env (telemetry, Fly):**
   - `TESLA_LINK_REDIRECT_BASE_URL=https://telemetry.myrobotaxi.app:4443`
     (exact origin registered in step 1; enables the §7.11 endpoints).
   - `TESLA_LINK_APP_REDIRECT=myrobotaxi://tesla-linked` (optional; this is the
     default).
   - `AUTH_APPLE_BOOTSTRAP` — append `<tester_email>=<their_prisma_cuid>`.
3. **Provision tester #2's Prisma `User`** — have them do one web Google sign-in
   (creates the row), and note the cuid.
4. **Provision the Tesla `Account` row** — have tester #2 do the web Tesla link
   once (also does vehicle sync + pairing check), *or* seed the row via SQL.
5. **iOS app scheme** — register the `myrobotaxi` URL scheme in `project.yml`
   (`info.properties` → `CFBundleURLTypes`) and ship a build with the real
   `ASWebAuthenticationSession` authenticator wired into `AddTeslaFlow` (the
   MYR-115 seam).
6. **After the tester pairs the virtual key**, run
   `ops fleet-config push --vin <newVIN> --user-id <cuid>` (unless the web sync
   already pushed it).

---

## 8. Future fully-self-serve version (summary of the gaps)

To let tester #3+ onboard with **zero ops actions**:

1. **Identity:** Go identity provisions a Prisma-compatible owner on Apple
   first-sign-in (or the `Account`/`Vehicle` FKs move off `"User"` so `go_users`
   ids are first-class owners), removing the per-tester `AUTH_APPLE_BOOTSTRAP`
   edit and the web-signup step.
2. **Tesla link:** server-side `Account` **upsert** in the callback (userinfo →
   `providerAccountId` → `INSERT ... ON CONFLICT`), removing the pre-provisioned-
   row requirement.
3. **Vehicle sync:** a Go `POST /api/tesla/vehicles/sync` endpoint (server-side
   `syncVehiclesFromTesla` equivalent) so the app never needs the web.
4. **Pairing + config:** in-app "pair virtual key" CTA (the `_ak` deep link) plus
   a backend pairing-status read and an in-app `fleet-config` push, so the setup
   banner advances to "connected" without ops.

The virtual-key **approval** itself always stays a user action inside the Tesla
app — Tesla requires the owner to physically approve adding the key.
