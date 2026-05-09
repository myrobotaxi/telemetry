# Encryption key rotation

> **Status (as of MYR-62, 2026-05-09): partial rollout — OAuth tokens encrypted.** The `internal/cryptox` package and the startup wiring are landed; `ENCRYPTION_KEY` is required at boot. **`Account.access_token`, `Account.refresh_token`, and `Account.id_token` are encrypted on disk** via the MYR-62 dual-write rollout (TS Phase 1 + Go Phase 2). The remaining P1 columns (Vehicle GPS coords, destination GPS, route blobs) require coordinated Prisma migrations in `../my-robo-taxi/` and are tracked as separate Linear issues. **Do NOT execute the rotation procedure in production until the `cryptox_decrypt_total{version="N"}` counter is wired** (see §"Observability") — without the counter, step 6 of the procedure cannot be completed safely.

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

The `internal/cryptox` package emits one counter per decrypt call (TODO: wired in a follow-on PR alongside the first column rollout):

- `cryptox_decrypt_total{version="N"}` — increments on every successful decrypt of a ciphertext stamped with version `N`. During rotation, the v1 series should decay to zero before v1 is retired.

Until the counter is wired (see "Foundation status" in `data-classification.md` §3.3), rotation operators rely on the application logs (`encryptor initialized` at startup logs `write_version`) and integration tests against representative production data shapes.

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
