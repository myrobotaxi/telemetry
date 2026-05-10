# Backup Retention Runbook

**Status:** v1
**Target artifact:** Operational runbook covering Supabase backup window, redelete-on-restore procedure, and the legal-basis-for-retention boundary
**Owner:** `infra` agent, with `security` review for legal-basis decisions
**Last updated:** 2026-05-09 (MYR-77 sidecar implementation)

## Purpose

Closes the gap between hard-deletion at the primary database (FR-10.1 cascade per [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3) and the rolling backups Supabase keeps for disaster recovery. Hard-deleting a user from the live database does **not** purge them from the backup window; without a documented procedure, a routine restore weeks after the deletion would silently resurrect the user's data and violate GDPR Art. 17 (right to erasure). This runbook is what we follow on every restore to honor the erasure obligation across the backup boundary.

## Anchored requirements

- **FR-10.1** — user-initiated deletion of all user data
- **FR-10.2** — immutable audit log entry per deletion
- **NFR-3.29** — audit logs retained indefinitely
- **GDPR Art. 17** — right to erasure (extended across backup restores via the redelete procedure in §2)
- **GDPR Art. 15 / 20** — right of access / portability (the export companion documented in [`../contracts/rest-api.md`](../contracts/rest-api.md) §7.7)

---

## 1. Supabase backup window

The MyRoboTaxi Supabase project keeps two parallel recovery surfaces. Both are time-bounded; both are in scope for the redelete procedure in §2.

| Surface | Default window (Supabase Pro) | Notes |
|---------|-------------------------------|-------|
| Point-in-time recovery (PITR) | **7 days** | Per-second granularity. Used for "restore to T-2h" disaster recovery. |
| Daily full backups | **30 days** | One snapshot per day. Used for "restore to a specific date" recovery. |

**Tier verification.** The defaults above match Supabase's published Pro tier. The project's actual tier is not pinned in either repo's environment files (`my-robo-taxi-telemetry/.env*` and `my-robo-taxi/.env.example` do not record a `SUPABASE_TIER` or `SUPABASE_PROJECT_REF` constant), so this runbook documents the **30-day window assumption**. Ops MUST verify against the actual tier in the Supabase dashboard (`Settings → Backups`) and update this section if the tier is Free (no PITR, 7-day daily backups only) or Team / Enterprise (extended PITR).

**Operational implication.** Any data deleted from primary today is recoverable for **up to 30 days** via a backup restore. The redelete procedure in §2 is what we run after every restore to keep the erasure obligation intact.

---

## 2. Redelete-on-restore procedure

When a Supabase backup is restored — full restore or PITR — the live database is rolled back to the backup's timestamp. Any FR-10.1 deletions that completed AFTER that timestamp are reversed by the restore: User rows reappear, cascaded children (Vehicle, Drive, Invite, Settings, Account) reappear with them.

The AuditLog table is the durable record of *what was deleted while the backup itself was the live database*, but a Supabase restore also rolls the AuditLog table back to `backup_timestamp` — any `account_deleted` rows written between `backup_timestamp` and the moment the restore is initiated would be erased by the restore itself. Reading from the restored AuditLog alone would yield zero post-backup deletions and the redelete procedure would silently no-op. The audit-sidecar bucket (§2.1) is the durable surface that survives the restore — STEP 1 reads from it, NOT from the restored in-DB AuditLog.

### 2.1 Sidecar infrastructure (Supabase Storage, S3-compatible API)

> **No AWS account is required.** The AWS SDK Go is just the client library — it speaks the S3 wire protocol against any S3-compatible server. Supabase issues its own access keys from Storage → S3 connection, the endpoint URL points at Supabase, and there is no AWS billing relationship.

| Parameter | Value |
|-----------|-------|
| Bucket | `audit-sidecar` (Supabase Storage bucket; Private) |
| Region | `us-east-1` (any value works — Supabase ignores region) |
| Endpoint | `https://<project_ref>.supabase.co/storage/v1/s3` |
| Object key schema | `audit/v1/{yyyy}/{mm}/{dd}/{userId}/{timestamp_unix_nanos}-{auditLogId}.json` |
| Service credentials | Supabase Storage S3 access key — Storage → S3 connection |
| Admin credentials | Supabase service-role JWT (dashboard owner) |

**Bucket setup (one-time, via Supabase dashboard or SQL):**

1. Storage → Create bucket → name `audit-sidecar`, Public = OFF.
2. Storage → S3 connection → enable, generate an access key. The access key is what the telemetry-server uses (mapped onto `AUDIT_SIDECAR_ACCESS_KEY` / `AUDIT_SIDECAR_SECRET_KEY` env vars at runtime).
3. Storage → Policies → add an RLS policy on the `audit-sidecar` bucket so the S3-API key can `INSERT` (PutObject) but **not** `DELETE` or `UPDATE`. Example SQL (run in the SQL editor):

   ```sql
   -- Allow the bucket-scoped S3 key to insert objects.
   CREATE POLICY "audit-sidecar service can insert"
     ON storage.objects FOR INSERT TO public
     WITH CHECK (bucket_id = 'audit-sidecar');

   -- Explicitly deny DELETE and UPDATE on the bucket for the same role.
   -- Only the dashboard owner (service-role JWT) retains DELETE.
   CREATE POLICY "audit-sidecar service cannot mutate"
     ON storage.objects FOR UPDATE TO public USING (false);
   CREATE POLICY "audit-sidecar service cannot delete"
     ON storage.objects FOR DELETE TO public USING (false);
   ```

   Verify via SQL editor: `SELECT polname FROM pg_policy WHERE polrelid = 'storage.objects'::regclass;`
4. Set telemetry-server env vars on the deployment platform:
   - `AUDIT_SIDECAR_BUCKET=audit-sidecar`
   - `AUDIT_SIDECAR_ENDPOINT=https://<project_ref>.supabase.co/storage/v1/s3`
   - `AUDIT_SIDECAR_REGION=us-east-1` (any value)
   - `AUDIT_SIDECAR_ACCESS_KEY` = Supabase Storage S3 access key ID
   - `AUDIT_SIDECAR_SECRET_KEY` = Supabase Storage S3 access key secret

**Delivery semantics (at-most-once, forward-only):**

- Best-effort. DB INSERT happens first; the sidecar write is enqueued to an in-process bounded channel (10 000-entry cap) and delivered by a background worker with up to 3 PutObject attempts.
- Retries-exhausted entries are dropped (counter `audit_sidecar_write_failures_total{reason="put"}` increments). The DB row persists regardless — sidecar failure NEVER fails `AuditRepo.InsertAuditLog`.
- Forward-only: mirrors only rows written after the telemetry-server starts with `AUDIT_SIDECAR_BUCKET` set. Rows written before that deploy date are not in the bucket. For restores predating the sidecar deploy date, STEP 1 falls back to DB-only recovery (manual reading of the in-DB AuditLog before the restore is initiated).

### 2.2 Procedure

```text
INPUT
  backup_timestamp  -- the wall-clock time the restored backup was taken
  restored_database -- the freshly-restored Supabase project

STEP 0 — verify sidecar coverage (run BEFORE initiating the restore)
  -- Compare sidecar object count to DB row count for the date range
  -- between backup_timestamp and now. The sidecar is the durable
  -- record that survives the restore.

  export AWS_ACCESS_KEY_ID=<supabase-s3-key>           # from Supabase Storage → S3 connection
  export AWS_SECRET_ACCESS_KEY=<supabase-s3-secret>    # from Supabase Storage → S3 connection
  export AWS_DEFAULT_REGION=us-east-1
  SUPABASE_S3="https://<project_ref>.supabase.co/storage/v1/s3"

  # Count sidecar objects for the date range.
  aws s3 ls --endpoint-url "$SUPABASE_S3" \
    s3://audit-sidecar/audit/v1/<yyyy>/<mm>/<dd>/ --recursive | wc -l

  # Compare to in-DB AuditLog count for the same range
  # (against the standby replica to avoid load on primary).
  psql "$DATABASE_URL" -c "
    SELECT COUNT(*) FROM \"AuditLog\"
    WHERE timestamp >= '<backup_timestamp>'
  ;"

  -- If sidecar count ≈ DB count, proceed.
  -- If sidecar count < DB count, the gap is expected when:
  --   (a) the sidecar was deployed after some rows were written
  --       (forward-only — see §2.1).
  --   (b) worker retries were exhausted for some rows
  --       (check audit_sidecar_write_failures_total).
  -- Document the gap and proceed to STEP 1; the redelete is best-
  -- effort over the gap and a manual review of the in-DB AuditLog
  -- (which the restore will roll back) is the only remaining
  -- pre-restore reference for those rows.

STEP 1 — enumerate post-backup deletions from the sidecar
  -- Download account_deleted JSON objects from the sidecar for the
  -- date range between backup_timestamp and the restore-initiation
  -- moment. The sidecar is unaffected by the Supabase restore.
  aws s3 sync --endpoint-url "$SUPABASE_S3" \
    s3://audit-sidecar/audit/v1/<yyyy>/<mm>/<dd>/ \
    /tmp/audit-restore/<yyyy>/<mm>/<dd>/

  -- Filter to action='account_deleted' rows (each JSON object
  -- contains the full audit row).
  result := <objects in /tmp/audit-restore where .action == 'account_deleted'>

  -- Each row carries the userId (orphaned — intentional per
  -- data-lifecycle.md §4.5) and the {vehicleCount, driveCount,
  -- inviteCount} counts captured at the original deletion time.

STEP 2 — for each (userId) in the set, re-run the FR-10.1 cascade
  FOR EACH audit IN result:
    BEGIN TRANSACTION;
      -- Re-import the original audit row from the sidecar JSON
      -- (preserves the original deletion's audit trail with its
      -- original timestamp).
      INSERT INTO "AuditLog" (
        "id", "userId", "timestamp", "action",
        "targetType", "targetId", "initiator", "metadata", "createdAt"
      ) VALUES (
        <audit.id>, <audit.userId>, <audit.timestamp>,
        <audit.action>, <audit.targetType>, <audit.targetId>,
        <audit.initiator>, <audit.metadata>, <audit.createdAt>
      )
      ON CONFLICT (id) DO NOTHING;

      -- Write a SECOND audit row recording the redelete itself, with
      -- a distinct initiator so the redelete is grep-able.
      INSERT INTO "AuditLog" (
        "id", "userId", "timestamp", "action",
        "targetType", "targetId", "initiator", "metadata"
      ) VALUES (
        cuid(),
        audit.userId,
        NOW(),
        'account_deleted',
        'user',
        audit.userId,
        'system_pruner',                       -- no user UI path; ops-initiated
        jsonb_build_object(
          'reason',           'redelete_after_restore',
          'backupRestoredAt', <backup_timestamp>,
          'originalDeleteAt', <audit.timestamp>
        )
      );

      -- Re-execute the §3.1 cascade. Cascading FKs (onDelete: Cascade)
      -- on Account, Vehicle (-> Drive, TripStop, Invite), Invite (sender),
      -- Settings handle child rows automatically.
      DELETE FROM "User" WHERE "id" = audit.userId;
    COMMIT;

STEP 3 — verify telemetry-server cleanup
  -- The Go telemetry server's vehicle_deleted LISTEN/NOTIFY trigger
  -- (data-lifecycle.md §3.5, MYR-73) fires for each cascaded Vehicle
  -- delete; the receiver tears down active WebSocket connections and
  -- inbound Tesla mTLS streams for the affected VINs. Confirm via the
  -- Prometheus metrics:
  --   tesla_inbound_rejected_total{reason="vehicle_not_authorized"}
  --   ws_close_user_deletion_total

STEP 4 — verify and promote
  -- 1. Compare the redelete count to STEP 1's |result|.
  -- 2. Confirm append-only triggers are still active:
  --      SELECT tgname FROM pg_trigger WHERE tgrelid = '"AuditLog"'::regclass;
  --      Expect: prevent_audit_log_update, prevent_audit_log_delete
  -- 3. Promote the restored instance per the Supabase failover
  --    procedure.
  -- 4. Restart the telemetry-server with AUDIT_SIDECAR_BUCKET set so
  --    sidecar mirroring resumes immediately.

OUTPUT
  count_redeleted        = |result|
  audit_rows_written     = 2 * |result|   -- one re-imported original + one new redelete row per user
                                          --   (the "original" comes from the sidecar in STEP 1;
                                          --    the redelete is written fresh in STEP 2)
```

### 2.3 Cascade reference

The redelete cascade is identical to the user-initiated cascade documented in [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3 — same Prisma `onDelete: Cascade` propagation:

```
User (deleted)
 ├── Account[]           (onDelete: Cascade)
 ├── Vehicle[]           (onDelete: Cascade)
 │    ├── Drive[]        (onDelete: Cascade)
 │    ├── TripStop[]     (onDelete: Cascade)
 │    └── Invite[]       (onDelete: Cascade — vehicle-scoped invites)
 ├── Invite[]            (onDelete: Cascade — invites sent by user)
 └── Settings?           (onDelete: Cascade)
```

The audit row for the redelete uses `initiator: 'system_pruner'` (per [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.2) because there is no user UI path that triggered it; ops did. The metadata shape is restricted to P0 counts and timestamps per CG-DL-5; never include the user's email, GPS coordinates, or any P1 value.

### 2.4 Transactional guarantees

- The original-audit re-import, the redelete-audit insert, and the User delete MUST all be in the same transaction per [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3.4. If any of the three fails, none of them apply.
- If the transaction fails for any user in the set, log at `ERROR`, continue with the remaining users, and re-run the procedure for the failed ones after diagnosing.
- STEP 0 (sidecar coverage verification) is a **prerequisite**, not a transactional step — it MUST complete successfully before the restore is initiated. Skipping STEP 0 means STEP 1 may read incomplete data; the redelete is best-effort over any gaps.
- Idempotent: re-running STEPS 1–2 against the live database (re-reading from the sidecar) writes the redelete audit row a second time, but the original-audit re-import is `ON CONFLICT DO NOTHING` and the User delete is a no-op (the row is already gone). The redelete audit row's `metadata.reason: redelete_after_restore` discriminator makes duplicate redelete rows easy to detect during forensic queries; the AuditLog's append-only contract (NFR-3.29) preserves all of them.

---

## 3. Drive retention (NFR-3.27)

Drive records older than 365 days are pruned daily by the background pruning job (`internal/drives/` pruning goroutine). The pruning job writes a `drives_pruned` AuditLog entry for each batch — these are mirrored to the sidecar like any other audit row. The 1-year window is by design.

```promql
# Counter increments on each successful pruning batch.
drives_pruned_total

# Recent pruning entries:
# SELECT * FROM "AuditLog" WHERE action = 'drives_pruned' ORDER BY timestamp DESC LIMIT 10;
```

---

## 4. Tamper-resistance posture and legal holds

**v1 (Supabase Storage, current).** Append-only is enforced via two layers:

1. **RLS policies** on the `audit-sidecar` bucket deny `UPDATE` and `DELETE` for the service-role principal. The policies are listed in §2.1 above.
2. **Credential separation.** The dashboard owner holds the Supabase service-role JWT (which can drop the policies). The running telemetry-server holds only a bucket-scoped S3 access key with `INSERT` capability. A compromise of the service-role JWT — held only by humans, not the running service — is the only path to mutation.

There is no native Object Lock equivalent in Supabase Storage, so a malicious or accidental policy change by an admin would unblock deletes on a single click. v1 accepts this trade-off because the runbook is for disaster-recovery (backup-restore brings deleted-user data back), not for high-grade tamper-evident audit. The bigger threat — losing the deletion record entirely on a DB restore — is what STEP 0 / STEP 2 handle, and that does not require Object Lock.

**Legal holds (v1).** Use Supabase Storage's metadata to flag specific objects as legal-hold-protected, and add an RLS policy that denies `DELETE` for any object with that flag, even for the service-role JWT:

```sql
-- Flag a specific object.
UPDATE storage.objects
   SET metadata = jsonb_set(coalesce(metadata, '{}'::jsonb), '{legal_hold}', 'true'::jsonb)
 WHERE bucket_id = 'audit-sidecar' AND name = 'audit/v1/2026/05/09/user-xyz/1234567890-audit-abc.json';

-- Policy denying DELETE on legal-held objects (run once, applies bucket-wide).
CREATE POLICY "audit-sidecar legal hold blocks delete"
  ON storage.objects FOR DELETE
  USING (bucket_id = 'audit-sidecar' AND coalesce((metadata->>'legal_hold')::boolean, false));
```

**v2 hardening (deferred — separate Linear issue if/when needed).** Migrate the sidecar to a backend with native Object Lock (Cloudflare R2 or AWS S3 COMPLIANCE) so even an admin credential compromise cannot delete within the retention window. The Go-side abstraction (`auditsidecar.Sidecar` + `S3Putter` + `PutterConfig.Endpoint`) is backend-agnostic — only bucket setup and operator runbook would change.

---

## 5. Legal-basis-for-retention boundary

Three retention windows interact across the system. The boundary between them is the answer to "how long can a piece of data legally exist after the user requested erasure?" — the redelete procedure in §2 is what enforces the GDPR Art. 17 obligation across that boundary.

| Surface | Window | Why we keep it | GDPR Art. 17 alignment |
|---------|--------|----------------|------------------------|
| Primary database | Erased at FR-10.1 cascade time (synchronous) | Live serving | Erasure honored immediately at the live database. |
| Supabase backups | **30 days** (Pro tier default; verify per §1) | Disaster recovery — the legal basis for retention is "compliance with a legal obligation" (Art. 17(3)(b)) and "legitimate interest in operational continuity". Backups cannot be selectively edited to remove a single user, so the entire window must be preserved AND the redelete procedure (§2) MUST run after every restore. | Erasure honored on restore via redelete (§2). The 30-day window is the maximum lag between deletion and Art. 17 compliance for backup-resurrected data. |
| AuditLog table | **Indefinite** per NFR-3.29 (append-only) | Proves the erasure happened — the Art. 17 obligation is to delete the user's data, not to delete the metadata recording that we did so. The audit row contains only opaque IDs, action enums, timestamps, and P0 counts ([`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.4). It does not contain personal data after the user is deleted; the `userId` is an orphaned cuid. | Audit metadata about the erasure is itself not "personal data" once the User row is gone — the cuid is opaque. Indefinite retention is justified under Art. 6(1)(c) (legal obligation to demonstrate compliance). |
| Audit sidecar (Supabase Storage) | **Indefinite** by default (mirrors NFR-3.29) | Same legal basis as the AuditLog table — same content (an opaque-cuid record of erasure events). Lifecycle-deletion of sidecar objects is intentionally NOT configured at v1; if a future Linear issue caps the sidecar retention, it MUST stay ≥ the Supabase backup window so the runbook §2 can always source post-backup `account_deleted` rows. | Same Art. 6(1)(c) basis. |
| Cold logs (slog / Loki / equivalent) | **No longer than 90 days** | Operational debugging. Logs MUST NOT contain P1 values per [`../contracts/data-classification.md`](../contracts/data-classification.md) §2.2 and Rule CG-DC-2; with the redaction discipline already enforced on the emit path, log retention up to 90 days does not extend the user's data exposure. | Logs are P0 only; no Art. 17 erasure obligation applies to the log surface itself, but the 90-day cap caps the operational-data window. |

**The redelete procedure (§2) is how we honor GDPR Art. 17 across backup restores.** It is mandatory after every Supabase restore — full restore or PITR — within the 30-day backup window. Skipping it would resurrect deleted user data and breach Art. 17.

---

## 6. Monitoring

| Metric | Alert threshold | Action |
|--------|----------------|--------|
| `audit_sidecar_write_failures_total{reason="put"}` | > 0 for 5 min | Check Supabase Storage connectivity + bucket / RLS policies |
| `audit_sidecar_write_failures_total{reason="enqueue_full"}` | > 0 | Check queue depth; scale up telemetry-server if sustained |
| `audit_sidecar_queue_depth` | > 8000 (80% of 10K cap) | Investigate Supabase Storage throughput or worker stalls |
| `audit_sidecar_writes_total` | Drops to 0 when `AUDIT_SIDECAR_BUCKET` set | Sidecar worker may be stuck; restart pod |

---

## 7. Cross-references

| Topic | Document |
|-------|----------|
| FR-10.1 deletion cascade | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3 |
| AuditLog schema + enum values | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4 |
| `data_exported` action (GDPR Art. 15 / 20 companion) | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.2; [`../contracts/rest-api.md`](../contracts/rest-api.md) §7.7 |
| `DELETE /api/users/me` endpoint | [`../contracts/rest-api.md`](../contracts/rest-api.md) §7.6 |
| Field-level classification (P0 / P1 / P2) | [`../contracts/data-classification.md`](../contracts/data-classification.md) |
| Functional requirements (FR-10.x) | [`../architecture/requirements.md`](../architecture/requirements.md) §2.10 |
| Non-functional requirements (NFR-3.29 audit retention) | [`../architecture/requirements.md`](../architecture/requirements.md) §3.10 |
| Sidecar Go implementation | [`../../internal/store/auditsidecar/`](../../internal/store/auditsidecar/) (MYR-77) |
