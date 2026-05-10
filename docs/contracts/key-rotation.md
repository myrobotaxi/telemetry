# Encryption key rotation

> **Status (as of MYR-65, 2026-05-09): full P1 rollout + decrypt observability wired — rotation runbook is production-ready.** The `internal/cryptox` package and the startup wiring are landed; `ENCRYPTION_KEY` is required at boot. **`Account.access_token`, `Account.refresh_token`, and `Account.id_token` are encrypted on disk** via the MYR-62 dual-write rollout (TS Phase 1 + Go Phase 2). **The six Vehicle GPS columns (`latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`, `originLatitude`/`originLongitude`) are encrypted on disk** via the MYR-63 dual-write rollout (TS Phase 1 + Go Phase 2). **The route-blob polylines (`Vehicle.navRouteCoordinates`, `Drive.routePoints`) are encrypted on disk** via the MYR-64 dual-write rollout (TS Phase 1 + Go Phase 2). Every P1 column identified by `data-classification.md` §3.1 now has a Phase-2 encrypt-on-write path. **As of MYR-65, the `cryptox_decrypt_total{version="N"}` Prometheus counter is wired and exposed on the existing `/metrics` endpoint** — operators can now drive the rotation procedure end-to-end including step 6 (confirm v1 series decays to zero before retiring v1). The first staging drill is tracked in §"Drill log".

## Purpose

Operational contract for rotating the AES-256-GCM keys that protect P1 columns at rest (NFR-3.23). Defines the env-var schema both shapes accept, the procedure for retiring a key, and the observability operators rely on to confirm rotation completed before a key is dropped.

## Anchored requirements

- **NFR-3.22** — TLS in transit for all connections.
- **NFR-3.23** — AES-256-GCM column-level encryption for P1 fields.
- **NFR-3.24** — encryption key stored as Fly.io secret (`ENCRYPTION_KEY`).
- **NFR-3.25** — encryption transparent to SDK consumers (server store layer only).
- **NFR-3.26** — key rotation strategy (this document).

## Threat model

- An attacker with database read access (e.g., a leaked Supabase backup, an over-privileged ops account, or a misconfigured replica) is the primary adversary in scope.
- Application memory is out of scope for the rotation strategy itself: a compromised running process exposes plaintext regardless of at-rest encryption.
- Loss of the active key = loss of access to all rows encrypted under it. The KeySet design keeps retired keys readable until rotation is provably complete (see §"Procedure"), preventing accidental data loss during rotation.

## Ciphertext format

Every value emitted by `internal/cryptox.Encryptor` is base64-encoded:

```
[1 byte version][12 bytes nonce][N bytes ciphertext + 16 bytes GCM auth tag]
```

- `version` routes the decrypt path to the matching key in the active `KeySet`. Reserved: `0x00` is INVALID — guards against zero-init buffers being silently accepted.
- `nonce` is freshly random per call (NIST SP 800-38D §5.2.1.1). Catastrophic GCM failure mode is nonce reuse; never patch this constant or reuse a nonce.
- **Nonce-space rotation guidance.** With 96-bit random nonces the birthday-collision probability hits 2⁻³² at roughly 2³² (~4 billion) encryptions per key. NIST's stated bound is also 2³². Plan to retire any single key before it reaches ~1 billion encryptions across all P1 columns combined to stay well below the bound. At telemetry rates (e.g., ~10 GPS frames/sec/vehicle × 1000 vehicles × 86400 sec/day ≈ 0.86B/day for the GPS columns alone), this implies an annual or sooner rotation cadence once the GPS rollout lands. Re-evaluate the cadence as part of the column-rollout PRs once production write rates per column are measured.
- `ciphertext + tag` is the standard AES-GCM output. Tampering produces an authentication failure on `Decrypt` — never silently accepted.

The minimum ciphertext length is `1 + 12 + 16 = 29` bytes pre-base64. `Decrypt` rejects shorter inputs with `ErrCiphertextTooShort` before invoking AES.

## Env-var schema

The Encryptor accepts two deployment shapes; pick exactly one.

### Single-key shorthand (no rotation in progress)

```
ENCRYPTION_KEY=base64(32 random bytes)
```

`writeVersion` is fixed at `0x01`. The KeySet is `{0x01: <key>}`. Use this shape when no rotation is in progress and you do NOT need to keep an old key readable.

### Versioned shape (rotation in progress, or after rotation completes)

```
ENCRYPTION_KEY_V1=base64(32 random bytes)   # old key, retained for read
ENCRYPTION_KEY_V2=base64(32 random bytes)   # new key
ENCRYPTION_WRITE_VERSION=2                   # Encrypt uses V2; Decrypt fans out by version byte
```

- `ENCRYPTION_KEY_V{N}` may be present for any byte value `1..255`.
- `ENCRYPTION_WRITE_VERSION` MUST point to a present `ENCRYPTION_KEY_V{N}` — startup fails otherwise.
- All present versioned keys are added to the readable set, so ciphertexts written under any version remain decryptable.

### Mutual exclusivity

Setting both `ENCRYPTION_KEY` and any `ENCRYPTION_KEY_V{N}` is a configuration error. Startup fails with a clear message. Pick exactly one shape per deployment.

### Empty values

Any empty env-var value is treated as not-set. This is so a deployment cannot silently launch with an empty key, and so test harnesses that clear vars with `t.Setenv("ENCRYPTION_KEY", "")` behave identically to "not set."

## Procedure: rotate from V1 to V2

This is the canonical happy-path rotation. Each step is independent and observable.

1. **Generate the new key.** `head -c 32 /dev/urandom | base64`. Store it in your secret manager (Fly.io secrets, AWS Secrets Manager, etc.) as `ENCRYPTION_KEY_V2`.

2. **Switch the deployment to versioned shape.** Set `ENCRYPTION_KEY_V1` to the value that was previously `ENCRYPTION_KEY`, and unset `ENCRYPTION_KEY`. Set `ENCRYPTION_KEY_V2` to the new key. Set `ENCRYPTION_WRITE_VERSION=1` (still writing v1 — this step only adds the v2 key to the readable set so we can't get cut off mid-rotation). Deploy.

3. **Verify v1 + v2 are both readable.** Hit a few `/snapshot` endpoints; confirm decrypts succeed. Optional: synthesize a v2 ciphertext via a one-shot CLI tool and confirm decrypt fans out to v2 correctly.

4. **Flip the active write version.** Set `ENCRYPTION_WRITE_VERSION=2`. Deploy. New writes are now under v2; existing v1 ciphertexts remain readable.

5. **Re-encrypt the corpus.** A background job reads each P1 column, decrypts under the version-routed key, re-encrypts under v2 (the active write version), and writes back. The job MUST be idempotent — re-running on already-v2 rows is a no-op (decrypt v2 → encrypt v2). Track progress with the `cryptox_decrypt_total{version="..."}` counter (see §"Observability").

6. **Confirm v1 is no longer reached.** When `cryptox_decrypt_total{version="1"}` has been zero for at least 24 hours under representative production load, retirement is safe. If a long-tail row remains v1, the metric stays nonzero — do NOT retire v1 in that case.

7. **Retire v1.** Unset `ENCRYPTION_KEY_V1` from the secret manager. Deploy. The KeySet now only has v2; any v1 ciphertext that surfaces post-retirement returns `ErrUnknownKeyVersion` — surfacing the bug rather than silently failing.

8. **(Optional) Collapse to single-key shorthand.** If no further rotation is planned in the near term, set `ENCRYPTION_KEY` to the v2 key value, unset `ENCRYPTION_KEY_V2` and `ENCRYPTION_WRITE_VERSION`. The semantic is unchanged; the env footprint shrinks.

## Procedure: emergency retire (suspected key compromise)

1. Treat the suspected-compromised key as untrusted; assume an attacker can decrypt any data encrypted under it.
2. Generate a new key as `ENCRYPTION_KEY_V{N+1}`. Set `ENCRYPTION_WRITE_VERSION=N+1`. Deploy.
3. Run the re-encrypt-on-read job at maximum throughput. Monitor `cryptox_decrypt_total{version="N"}` decay.
4. Do NOT wait for the standard 24-hour zero window — retire the compromised version as soon as the decrypt metric reaches zero. Any rows that fail to re-encrypt in time will surface `ErrUnknownKeyVersion` post-retirement and can be re-keyed individually after the immediate threat is neutralized.
5. Rotate the secret-store credentials too — if the key leaked, the secret-store access path may also be compromised.

## Observability

The `internal/cryptox` package emits one Prometheus counter per successful decrypt, exposed on the existing `/metrics` endpoint:

- `cryptox_decrypt_total{version="N"}` — increments on every successful decrypt of a ciphertext stamped with version `N`. During rotation, the v1 series should decay to zero before v1 is retired (procedure step 6).

Implementation details:

- Wired into `keySetEncryptor.DecryptString` AFTER the AES-GCM `Open` succeeds. Failed decrypts (auth-tag mismatch, truncated input, unknown version, base64 errors) MUST NOT count — otherwise tampered traffic at the decrypt path inflates the counter and hides the v1-decay signal.
- All readable-version labels are pre-registered with value `0` at startup, so a `/metrics` scrape immediately after deploy reports the full label set rather than missing labels (the latter is indistinguishable from a never-seen version on operator dashboards).
- Composition root: `cmd/telemetry-server/wiring.go` `setupEncryption` constructs `cryptox.PrometheusMetrics` against the same registry that already serves `/metrics`. Library consumers without a Prometheus registry get the zero-cost `cryptox.NoopMetrics` default — no allocation, no overhead.

Operators also have the application logs (`encryptor initialized` at startup logs `write_version` and `readable_versions`) for an at-startup sanity check that the deployed key set matches expectations.

## Drill log

Each entry records a real or staged execution of the rotation procedure with the dates, observed counter behavior, and any runbook adjustments. The drill log is the audit trail operators consult before approving a production rotation.

### Drill 1 — Staging dual-key dry run (planned, not yet executed)

- **Status:** Planned — to be executed by the on-call operator on staging once the metric wiring lands on `main`.
- **Tracking:** Code-level deliverables (metric wiring + runbook updates) ship in [MYR-65](https://linear.app/myrobotaxi/issue/MYR-65). Drill execution itself is an ops-only follow-up tracked separately so contract-level changes don't block on staging-environment availability.
- **Prerequisites:**
  - `cryptox_decrypt_total{version="N"}` visible on the staging `/metrics` endpoint with at least the v1 label pre-seeded at zero.
  - A v2 key generated via `head -c 32 /dev/urandom | base64` and stored in the staging Fly.io secret manager.
  - Representative load: at least one synthetic vehicle session (cmd/simulator) and one human session active during step 4 onward, so the v1 series ticks under realistic decrypt traffic and the decay curve is observable.
  - Re-encrypt-on-read background job available (currently per-column backfill CLIs: `cmd/backfill-account-tokens`, `cmd/backfill-vehicle-gps`, `cmd/backfill-route-blobs`). Drill must exercise all three or document which columns the drill skipped.
- **Plan:** Execute steps 1–7 of the §"Procedure" section above, in order. Each step is observable independently: step 2 is a deploy with a config change, step 3 is a manual check, step 4 is a deploy, step 5 is a backfill run, step 6 is a Grafana query against the staging Prometheus, step 7 is a deploy.
- **Success criteria:**
  - Step 3: a manual `/snapshot` request decrypts both v1 and v2 ciphertexts (both labels increment at least once on `cryptox_decrypt_total`).
  - Step 4: new writes carry version byte `0x02` (verify by tailing a freshly-written row through the backfill CLI's verifier or by base64-decoding the column).
  - Step 5: the v1 backfill counters in `account_token_plaintext_remaining_total`, `vehicle_gps_plaintext_remaining_total`, and `route_blob_plaintext_remaining_total` go to zero AND `cryptox_decrypt_total{version="1"}` decay rate trends to zero.
  - Step 6: `cryptox_decrypt_total{version="1"}` rate is `0` for ≥24h continuous under representative staging load. If the counter is stale (no traffic), extend the window — `0` from a quiet system does not satisfy the criterion.
  - Step 7: post-retirement, any v1 ciphertext that surfaces returns `ErrUnknownKeyVersion`. Synthesize a v1 blob via `cmd/ops cryptox encrypt --version 1 …` and confirm decrypt fails as expected.
- **Runbook adjustments to record:** When the drill runs, append a new sub-entry under this section recording (a) date, (b) observed v1 decay curve (sketch / Grafana screenshot link), (c) any unexpected behavior, and (d) runbook patches required.

## Failure modes

| Symptom | Likely cause | Action |
|---------|--------------|--------|
| `cryptox: ciphertext shorter than minimum` | DB row truncated/corrupt; not produced by `cryptox` | Inspect the row; if real, restore from backup. |
| `cryptox: invalid ciphertext version 0x00` | Zero-init buffer leaked into a row; or a bug in producer | Inspect the row; check that the writer goes through `cryptox.Encryptor`. |
| `cryptox: unknown key version: version=N` | Ciphertext under retired key, or future version not yet deployed | Restore the retired key (read-only) and run the re-encrypt job, OR roll forward to a deployment that has version N's key. |
| `gcm.Open: cipher: message authentication failed` | Tampered ciphertext, wrong key, or wrong-version key | Investigate: integrity attack, KeySet misconfiguration, or row corruption. Never accept the value. |
| Startup fails: `loading encryption key set: ...` | `ENCRYPTION_KEY` (or the versioned shape) missing/invalid | Set the secret per §"Env-var schema". The binary deliberately fails-fast (NFR-3.24). |

## References

- Implementation: [`internal/cryptox/`](../../internal/cryptox/)
- Classification table: [`data-classification.md`](data-classification.md) §3
- Anchored requirements: [`docs/architecture/requirements.md`](../architecture/requirements.md) NFR-3.22 — NFR-3.26
- NIST SP 800-38D — Recommendation for Block Cipher Modes of Operation: Galois/Counter Mode (GCM) and GMAC

## Change log

| Date | Change | Author |
|------|--------|--------|
| 2026-05-01 | Initial draft authored by [MYR-16](https://linear.app/myrobotaxi/issue/MYR-16). Defines the ciphertext format, env-var schema (single-key shorthand + versioned shape), rotation procedure, emergency retire procedure, observability plan, and failure-mode table. Foundation only — no P1 column is yet encrypted on disk; per-column rollouts are tracked as separate Linear issues. | sdk-architect + go-engineer |
| 2026-05-09 | [MYR-62](https://linear.app/myrobotaxi/issue/MYR-62) flips Account OAuth-token columns (`access_token`, `refresh_token`, `id_token`) from the unfinished list to the encrypted set. Foundation-status callout updated; OAuth tokens removed from the "no P1 column is yet encrypted" wording. Vehicle/Drive GPS columns and route blobs remain on the unfinished list. | go-engineer |
| 2026-05-09 | [MYR-63](https://linear.app/myrobotaxi/issue/MYR-63) flips the six Vehicle GPS columns (`latitude`, `longitude`, `destinationLatitude`, `destinationLongitude`, `originLatitude`, `originLongitude`) from the unfinished list to the encrypted set. Foundation-status callout updated; Vehicle GPS removed from the unfinished list. Route blob (`navRouteCoordinates`) remains as MYR-64. | go-engineer |
| 2026-05-09 | [MYR-64](https://linear.app/myrobotaxi/issue/MYR-64) flips the route-blob polylines (`Vehicle.navRouteCoordinates`, `Drive.routePoints`) from the unfinished list to the encrypted set. Foundation-status callout updated; the unfinished list is now empty — every P1 column in `data-classification.md` §3.1 has an encrypt-on-write path. | go-engineer |
| 2026-05-09 | [MYR-65](https://linear.app/myrobotaxi/issue/MYR-65) wires `cryptox_decrypt_total{version="N"}` Prometheus counter on the existing `/metrics` endpoint, removes the "do NOT execute the rotation procedure in production yet" callout, adds the §"Drill log" section, and rewrites §"Observability" to drop the TODO. Counter increments only after successful AES-GCM Open; tampered/truncated/unknown-version inputs are explicitly excluded so they cannot mask the v1-decay signal. All readable-version labels pre-seeded at zero so the first scrape reports the full label set. The procedure is now production-ready end-to-end. | go-engineer |
