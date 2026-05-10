# Backup and Retention Runbook

**Owner:** infra agent  
**Last updated:** 2026-05-09 (MYR-77)  
**Anchored NFRs:** NFR-3.29 (AuditLog indefinite retention), NFR-3.27 (Drive 1-year rolling window)

---

## 1. Overview

This runbook covers backup verification and data-retention procedures for the
MyRoboTaxi telemetry service. There are two persistence layers relevant to
this runbook:

| Layer | Source of truth | Retention policy |
|-------|----------------|-----------------|
| PostgreSQL AuditLog table | **Canonical** — Prisma-owned; append-only enforced by DB triggers (NFR-3.29) | Indefinite — never deleted |
| S3 audit sidecar | Best-effort forward-only mirror | 90-day Object Lock COMPLIANCE + Glacier transition |

The S3 sidecar is a **defence-in-depth supplement**, not a replacement for the
database. The DB is always the authoritative source of truth. See §2.1 for
delivery semantics.

---

## 2. AuditLog restore procedure

This section describes how to verify and, if necessary, restore AuditLog data
following a PostgreSQL incident (e.g., accidental table truncation, row-level
corruption, or a bad migration).

### 2.1 STEP 0 — Verify sidecar coverage

**Before** restoring from S3, verify that the sidecar covers the target date
range.

**Sidecar infrastructure:**

| Parameter | Value |
|-----------|-------|
| Bucket | `myrobotaxi-audit-sidecar-prod` |
| Region | `us-east-1` |
| Object key schema | `audit/v1/{yyyy}/{mm}/{dd}/{userId}/{timestamp_unix_nanos}-{auditLogId}.json` |
| Service IAM role | `arn:aws:iam::<account>:role/telemetry-server-audit-sidecar` |
| Admin IAM role | `arn:aws:iam::<account>:role/audit-sidecar-admin` |
| IaC | `deployments/terraform/audit-sidecar/` |

**Delivery semantics (at-most-once, forward-only):**

- The sidecar is best-effort. DB INSERT always happens first; the sidecar write
  is enqueued to an in-process bounded channel (10 000-entry cap) and
  delivered by a background worker with up to 3 PutObject attempts.
- If the worker exhausts retries, the entry is dropped (counter
  `audit_sidecar_write_failures_total{reason="aws"}` increments). The DB row
  persists regardless.
- The sidecar is forward-only: it mirrors rows written after the telemetry-server
  process started with `AUDIT_SIDECAR_BUCKET` set. **Rows written before that
  deploy date are not in S3.** For restores predating the sidecar deploy date,
  skip STEP 0 and go directly to STEP 1 (DB-only recovery).

**Verification steps:**

```bash
# 1. List objects for the target date range.
aws s3 ls s3://myrobotaxi-audit-sidecar-prod/audit/v1/2026/05/09/ --recursive | wc -l

# 2. Compare object count to DB row count for the same range.
#    Run against the standby replica to avoid load on the primary.
psql "$DATABASE_URL" -c "
  SELECT COUNT(*) FROM \"AuditLog\"
  WHERE timestamp >= '2026-05-09T00:00:00Z'
    AND timestamp <  '2026-05-10T00:00:00Z'
;"

# 3. Assess coverage. If sidecar count ≈ DB count, proceed to STEP 0b.
#    If sidecar count < DB count, the gap is expected if:
#      a) The sidecar was deployed after some rows were written (normal).
#      b) Worker retries were exhausted for some rows (check the failure metric).
#    Document the gap before proceeding to STEP 1.
```

**Prometheus metric check:**

```promql
# Inspect cumulative failures — a non-zero value means some rows were dropped.
audit_sidecar_write_failures_total{reason="aws"}
audit_sidecar_write_failures_total{reason="enqueue_full"}
```

### 2.2 STEP 1 — Restore from DB backup

If the incident involves DB corruption or loss, restore from the most recent
Supabase Point-In-Time Recovery (PITR) snapshot to a staging cluster. Do not
restore directly to production without a verification step.

```bash
# Follow Supabase restore procedure:
# https://supabase.com/docs/guides/platform/backups#point-in-time-recovery

# After restore, verify row count on the restored instance:
psql "$RESTORED_DATABASE_URL" -c "SELECT COUNT(*) FROM \"AuditLog\";"
```

### 2.3 STEP 2 — Reconcile gaps using S3 sidecar

If the DB restore is missing rows that the sidecar captured (e.g., rows
written between the PITR snapshot time and the incident), re-insert from S3:

```bash
# Download objects for the gap date range.
aws s3 sync \
  s3://myrobotaxi-audit-sidecar-prod/audit/v1/2026/05/09/ \
  /tmp/audit-restore/2026/05/09/ \
  --no-sign-request  # remove if using role credentials

# Inspect a sample object.
cat /tmp/audit-restore/2026/05/09/user-xyz/1234567890-audit-abc.json

# Re-insert rows into the restored DB.
# Use the fields from the JSON payload; the INSERT below mirrors
# internal/store/audit_repo.go queryAuditInsert.
# Run this as a script against the restored instance — NOT production.
for f in $(find /tmp/audit-restore -name '*.json'); do
  id=$(jq -r .id "$f")
  userId=$(jq -r .userId "$f")
  timestamp=$(jq -r .timestamp "$f")
  action=$(jq -r .action "$f")
  targetType=$(jq -r .targetType "$f")
  targetId=$(jq -r .targetId "$f")
  initiator=$(jq -r .initiator "$f")
  metadata=$(jq -c .metadata "$f")
  createdAt=$(jq -r .createdAt "$f")

  psql "$RESTORED_DATABASE_URL" -c "
    INSERT INTO \"AuditLog\" (\"id\",\"userId\",\"timestamp\",\"action\",\"targetType\",\"targetId\",\"initiator\",\"metadata\",\"createdAt\")
    VALUES ('$id','$userId','$timestamp','$action','$targetType','$targetId','$initiator','$metadata','$createdAt')
    ON CONFLICT (\"id\") DO NOTHING;
  "
done
```

### 2.4 STEP 3 — Verify and promote

1. Count rows on the restored instance and compare to the pre-incident count
   (from monitoring or the S3 object count).
2. Confirm append-only triggers are still active:
   ```sql
   SELECT tgname FROM pg_trigger WHERE tgrelid = '"AuditLog"'::regclass;
   -- Expect: prevent_audit_log_update, prevent_audit_log_delete
   ```
3. Promote the restored instance following the Supabase failover procedure.
4. Restart the telemetry-server with `AUDIT_SIDECAR_BUCKET` set so sidecar
   mirroring resumes immediately.

---

## 3. Drive retention (NFR-3.27)

Drive records older than 365 days are pruned daily by the background pruning
job (`internal/drives/` pruning goroutine). The pruning job writes a
`drives_pruned` AuditLog entry for each batch. No S3 backup is maintained for
Drive rows — the 1-year window is by design.

To verify the pruning job is running:

```promql
# Counter increments on each successful pruning batch.
drives_pruned_total

# Check the AuditLog for recent pruning entries:
# SELECT * FROM "AuditLog" WHERE action = 'drives_pruned' ORDER BY timestamp DESC LIMIT 10;
```

---

## 4. Object Lock and legal holds

To place a GOVERNANCE legal hold on a specific audit object (e.g., to preserve
evidence for litigation):

```bash
# Assume the admin role before running.
aws s3api put-object-legal-hold \
  --bucket myrobotaxi-audit-sidecar-prod \
  --key "audit/v1/2026/05/09/user-xyz/1234567890-audit-abc.json" \
  --legal-hold '{"Status": "ON"}'
```

The admin role (`audit-sidecar-admin`) holds `s3:PutObjectLegalHold`. The
service role does NOT have this permission. See
`deployments/terraform/audit-sidecar/iam.tf` for the IAM split.

COMPLIANCE mode means even the admin role cannot delete objects within the
90-day retention window. Legal holds extend protection beyond that window
indefinitely (until explicitly released).

---

## 5. Monitoring

| Metric | Alert threshold | Action |
|--------|----------------|--------|
| `audit_sidecar_write_failures_total{reason="aws"}` | > 0 for 5 min | Check S3 connectivity + IAM permissions |
| `audit_sidecar_write_failures_total{reason="enqueue_full"}` | > 0 | Check queue depth; scale up telemetry-server if sustained |
| `audit_sidecar_queue_depth` | > 8000 (80% of 10K cap) | Investigate S3 throughput or worker stalls |
| `audit_sidecar_writes_total` | Drops to 0 when `AUDIT_SIDECAR_BUCKET` set | Sidecar worker may be stuck; restart pod |
