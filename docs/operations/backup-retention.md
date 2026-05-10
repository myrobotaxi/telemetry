# Backup Retention Runbook

**Status:** Draft — v1
**Target artifact:** Operational runbook covering Supabase backup window, redelete-on-restore procedure, and the legal-basis-for-retention boundary
**Owner:** `infra` agent, with `security` review for legal-basis decisions
**Last updated:** 2026-05-09

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

The AuditLog table is the durable record of *what was deleted while the backup itself was the live database*, but a Supabase restore also rolls the AuditLog table back to `backup_timestamp` — any `account_deleted` rows written between `backup_timestamp` and the moment the restore is initiated would be erased by the restore itself. Reading from the restored AuditLog alone would yield zero post-backup deletions and the redelete procedure would silently no-op. To make the procedure operable, the post-backup `account_deleted` rows MUST come from a surface that survives the restore. §2.1 STEP 0 (the pre-restore sidecar export) is what produces that surface; STEP 1 reads from it, NOT from the restored in-DB AuditLog.

### 2.1 Procedure

```text
INPUT
  backup_timestamp  -- the wall-clock time the restored backup was taken
  restored_database -- the freshly-restored Supabase project

STEP 0 — pre-restore sidecar export (MANDATORY before initiating the restore)
  -- Run this against the live (pre-restore) database BEFORE the restore is
  -- triggered. The output is the durable record of post-backup deletions
  -- that the restore would otherwise erase.
  COPY (
    SELECT "id", "userId", "timestamp", "action",
           "targetType", "targetId", "initiator", "metadata", "createdAt"
    FROM "AuditLog"
    WHERE "action" = 'account_deleted'
      AND "timestamp" > <backup_timestamp>
    ORDER BY "timestamp" ASC
  ) TO '/sidecar/audit_log_post_backup.csv' WITH (FORMAT csv, HEADER true);

  -- Store the sidecar in an append-only external sink (S3 / object storage
  -- with object-lock, or any equivalent that survives the Supabase project's
  -- failure modes). The sidecar is the source-of-truth for STEP 1; the
  -- restored in-DB AuditLog is NOT.

STEP 1 — enumerate post-backup deletions from the sidecar
  -- Re-import the sidecar into a scratch table on the restored database, or
  -- read it directly via psql \copy. DO NOT use the restored in-DB AuditLog
  -- for this — the restore rolled it back to backup_timestamp.
  result := <rows from /sidecar/audit_log_post_backup.csv>

  -- Each row carries the userId (orphaned — intentional per
  -- data-lifecycle.md §4.5) and the {vehicleCount, driveCount, inviteCount}
  -- counts captured at the original deletion time.

STEP 2 — for each (userId) in the set, re-run the FR-10.1 cascade
  FOR EACH userId IN result:
    BEGIN TRANSACTION;
      -- Re-import the original audit row from the sidecar (preserves the
      -- original deletion's audit trail with its original timestamp).
      INSERT INTO "AuditLog" (
        "id", "userId", "timestamp", "action",
        "targetType", "targetId", "initiator", "metadata", "createdAt"
      ) VALUES (
        <sidecar.id>, <sidecar.userId>, <sidecar.timestamp>,
        <sidecar.action>, <sidecar.targetType>, <sidecar.targetId>,
        <sidecar.initiator>, <sidecar.metadata>, <sidecar.createdAt>
      );

      -- Write a SECOND audit row recording the redelete itself, with a
      -- distinct initiator so the redelete is grep-able.
      INSERT INTO "AuditLog" (
        "id", "userId", "timestamp", "action",
        "targetType", "targetId", "initiator", "metadata"
      ) VALUES (
        cuid(),
        userId,
        NOW(),
        'account_deleted',
        'user',
        userId,
        'system_pruner',                       -- no user UI path; ops-initiated
        jsonb_build_object(
          'reason',        'redelete_after_restore',
          'backupRestoredAt', <backup_timestamp>,
          'originalDeleteAt',  <sidecar.timestamp>
        )
      );

      -- Re-execute the §3.1 cascade. Cascading FKs (onDelete: Cascade)
      -- on Account, Vehicle (-> Drive, TripStop, Invite), Invite (sender),
      -- Settings handle child rows automatically.
      DELETE FROM "User" WHERE "id" = userId;
    COMMIT;

STEP 3 — verify telemetry-server cleanup
  -- The Go telemetry server polls Vehicle ownership on its next read cycle
  -- (data-lifecycle.md §3.5). Any active WebSocket connections for the
  -- redeleted users' vehicles will be torn down on the next cycle. Confirm
  -- via the Prometheus metric `tesla_inbound_rejected_total{reason="vehicle_not_authorized"}`
  -- and the WS connection count for the affected user IDs.

STEP 4 — audit
  -- §3.5.1 records the asymmetric DB-outage behavior of the two auth
  -- paths. During the restore window itself the Go server will fail-open
  -- on Tesla mTLS upgrades and fail-closed on browser WebSocket handshakes.
  -- After restore completes, both paths recover automatically.

OUTPUT
  count_redeleted        = |result|
  audit_rows_written     = 2 * |result|   -- one re-imported original + one new redelete row per user
                                          --   (the "original" comes from the sidecar in STEP 0;
                                          --    the redelete is written fresh in STEP 2)
```

> **Sidecar surface — operational note.** The append-only external sink referenced in STEP 0 is the durable substrate of this procedure. Recommended implementation: an S3 / object-storage bucket with object-lock (Compliance mode) and a 90-day retention floor that exceeds the backup-window cap in §1, so the sidecar never expires before its corresponding backup does. The bucket is a Fly.io secret-managed reference, not committed to either repo. Filing this is a follow-up that ops should track (the sink does not exist today; without it, STEP 0 cannot run, which means the entire redelete procedure is non-operational until the sink is provisioned).

### 2.2 Cascade reference

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

### 2.3 Transactional guarantees

- The original-audit re-import, the redelete-audit insert, and the User delete MUST all be in the same transaction per [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3.4. If any of the three fails, none of them apply.
- If the transaction fails for any user in the set, log at `ERROR`, continue with the remaining users, and re-run the procedure for the failed ones after diagnosing.
- STEP 0 (pre-restore sidecar export) is a **prerequisite**, not a transactional step — it MUST complete successfully against the live database before the restore is initiated. Skipping STEP 0 makes STEP 1 unable to source the post-backup audit set, and the entire procedure becomes non-operational.
- Idempotent: re-running STEP 1 against the live database (re-reading from the same sidecar) after a successful pass writes the redelete audit row a second time but the User delete is a no-op (the row is already gone). The redelete audit row's `metadata.reason: redelete_after_restore` discriminator makes duplicate redelete rows easy to detect during forensic queries; the AuditLog's append-only contract (NFR-3.29) preserves all of them.

---

## 3. Legal-basis-for-retention boundary

Three retention windows interact across the system. The boundary between them is the answer to "how long can a piece of data legally exist after the user requested erasure?" — the redelete procedure in §2 is what enforces the GDPR Art. 17 obligation across that boundary.

| Surface | Window | Why we keep it | GDPR Art. 17 alignment |
|---------|--------|----------------|------------------------|
| Primary database | Erased at FR-10.1 cascade time (synchronous) | Live serving | Erasure honored immediately at the live database. |
| Supabase backups | **30 days** (Pro tier default; verify per §1) | Disaster recovery — the legal basis for retention is "compliance with a legal obligation" (Art. 17(3)(b)) and "legitimate interest in operational continuity". Backups cannot be selectively edited to remove a single user, so the entire window must be preserved AND the redelete procedure (§2) MUST run after every restore. | Erasure honored on restore via redelete (§2). The 30-day window is the maximum lag between deletion and Art. 17 compliance for backup-resurrected data. |
| AuditLog table | **Indefinite** per NFR-3.29 (append-only) | Proves the erasure happened — the Art. 17 obligation is to delete the user's data, not to delete the metadata recording that we did so. The audit row contains only opaque IDs, action enums, timestamps, and P0 counts ([`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.4). It does not contain personal data after the user is deleted; the `userId` is an orphaned cuid. | Audit metadata about the erasure is itself not "personal data" once the User row is gone — the cuid is opaque. Indefinite retention is justified under Art. 6(1)(c) (legal obligation to demonstrate compliance). |
| Cold logs (slog / Loki / equivalent) | **No longer than 90 days** | Operational debugging. Logs MUST NOT contain P1 values per [`../contracts/data-classification.md`](../contracts/data-classification.md) §2.2 and Rule CG-DC-2; with the redaction discipline already enforced on the emit path, log retention up to 90 days does not extend the user's data exposure. | Logs are P0 only; no Art. 17 erasure obligation applies to the log surface itself, but the 90-day cap caps the operational-data window. |

**The redelete procedure (§2) is how we honor GDPR Art. 17 across backup restores.** It is mandatory after every Supabase restore — full restore or PITR — within the 30-day backup window. Skipping it would resurrect deleted user data and breach Art. 17.

---

## 4. Cross-references

| Topic | Document |
|-------|----------|
| FR-10.1 deletion cascade | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §3 |
| AuditLog schema + enum values | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4 |
| `data_exported` action (GDPR Art. 15 / 20 companion) | [`../contracts/data-lifecycle.md`](../contracts/data-lifecycle.md) §4.2; [`../contracts/rest-api.md`](../contracts/rest-api.md) §7.7 |
| `DELETE /api/users/me` endpoint | [`../contracts/rest-api.md`](../contracts/rest-api.md) §7.6 |
| Field-level classification (P0 / P1 / P2) | [`../contracts/data-classification.md`](../contracts/data-classification.md) |
| Functional requirements (FR-10.x) | [`../architecture/requirements.md`](../architecture/requirements.md) §2.10 |
| Non-functional requirements (NFR-3.29 audit retention) | [`../architecture/requirements.md`](../architecture/requirements.md) §3.10 |
