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

**Before** restoring from the sidecar, verify that it covers the target date
range.

**Sidecar infrastructure (Supabase Storage, S3-compatible API).**

> **No AWS account is required.** The AWS SDK Go is just the client
> library — it speaks the S3 wire protocol against any S3-compatible
> server. Supabase issues its own access keys from Storage → S3
> connection, the endpoint URL points at Supabase, and there is no
> AWS billing relationship.

| Parameter | Value |
|-----------|-------|
| Bucket | `audit-sidecar` (Supabase Storage bucket; Private) |
| Region | `us-east-1` (any value works — Supabase ignores region) |
| Endpoint | `https://<project_ref>.supabase.co/storage/v1/s3` |
| Object key schema | `audit/v1/{yyyy}/{mm}/{dd}/{userId}/{timestamp_unix_nanos}-{auditLogId}.json` |
| Service credentials | Supabase Storage S3 access key — Storage settings → S3 connection |
| Admin credentials | Supabase service-role JWT (dashboard owner) |

**Bucket setup (one-time, via Supabase dashboard or SQL):**

1. Storage → Create bucket → name `audit-sidecar`, Public = OFF.
2. Storage → S3 connection → enable, generate an access key. The access key
   is what the telemetry-server uses (mapped onto `AWS_ACCESS_KEY_ID` /
   `AWS_SECRET_ACCESS_KEY` env vars at runtime).
3. Storage → Policies → add an RLS policy on the `audit-sidecar` bucket so
   the S3-API key can `INSERT` (PutObject) but **not** `DELETE` or `UPDATE`.
   Example SQL (run in the SQL editor):

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

   Verify via the Supabase REST API or SQL editor:
   `SELECT polname FROM pg_policy WHERE polrelid = 'storage.objects'::regclass;`
4. Set telemetry-server env vars on the deployment platform:
   - `AUDIT_SIDECAR_BUCKET=audit-sidecar`
   - `AUDIT_SIDECAR_ENDPOINT=https://<project_ref>.supabase.co/storage/v1/s3`
   - `AUDIT_SIDECAR_REGION=us-east-1` (any value)
   - `AUDIT_SIDECAR_ACCESS_KEY` = Supabase Storage S3 access key ID
   - `AUDIT_SIDECAR_SECRET_KEY` = Supabase Storage S3 access key secret

**v2 hardening note (deferred):** Supabase Storage does not expose Object
Lock. v1 immutability is enforced at the policy layer (RLS deny on UPDATE /
DELETE for the service role) and at the credential layer (the service-role
JWT — which can drop policies — is held only by the dashboard owner, not
present in the running service). A v2 hardening would migrate the sidecar
to a backend with native Object Lock (Cloudflare R2 or AWS S3) for
defense-in-depth against a service-role JWT compromise.

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
# Configure the aws CLI to talk to Supabase Storage. The CLI reads
# AWS_* env vars by convention, but the credentials are Supabase's —
# no AWS account is involved.
export AWS_ACCESS_KEY_ID=<supabase-s3-key>           # from Supabase Storage → S3 connection
export AWS_SECRET_ACCESS_KEY=<supabase-s3-secret>    # from Supabase Storage → S3 connection
export AWS_DEFAULT_REGION=us-east-1
SUPABASE_S3="https://<project_ref>.supabase.co/storage/v1/s3"

# 1. List objects for the target date range.
aws s3 ls --endpoint-url "$SUPABASE_S3" \
  s3://audit-sidecar/audit/v1/2026/05/09/ --recursive | wc -l

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
audit_sidecar_write_failures_total{reason="put"}
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

### 2.3 STEP 2 — Reconcile gaps using sidecar

If the DB restore is missing rows that the sidecar captured (e.g., rows
written between the PITR snapshot time and the incident), re-insert from
the sidecar:

```bash
# Download objects for the gap date range from Supabase Storage.
aws s3 sync --endpoint-url "$SUPABASE_S3" \
  s3://audit-sidecar/audit/v1/2026/05/09/ \
  /tmp/audit-restore/2026/05/09/

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

## 4. Legal holds and tamper-resistance posture

**v1 (Supabase Storage, current).** Append-only is enforced via two layers:

1. **RLS policies** on the `audit-sidecar` bucket deny `UPDATE` and
   `DELETE` for the service-role principal. The policies are listed in
   §2.1 above.
2. **Credential separation.** The dashboard owner holds the Supabase
   service-role JWT (which can drop the policies). The running
   telemetry-server holds only a bucket-scoped S3 access key with
   `INSERT` capability. A compromise of the service-role JWT — held only
   by humans, not the running service — is the only path to mutation.

There is no native Object Lock equivalent in Supabase Storage, so a
malicious or accidental policy change by an admin would unblock deletes
on a single click. v1 accepts this trade-off because the runbook is for
disaster-recovery (backup-restore brings deleted-user data back), not
for high-grade tamper-evident audit. The bigger threat — losing the
deletion record entirely on a DB restore — is what STEP 0 / STEP 2
handle, and that does not require Object Lock.

**Legal holds (v1).** Use Supabase Storage's metadata to flag specific
objects as legal-hold-protected, and set up a separate RLS policy that
denies `DELETE` for any object with that flag, owned even by the
service-role JWT:

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

**v2 hardening (deferred — separate Linear issue).** Migrate the
sidecar to a backend with native Object Lock (Cloudflare R2, AWS S3
COMPLIANCE mode) so even an admin credential compromise cannot delete
within the retention window. The Go-side abstraction
(`auditsidecar.Sidecar` + `S3Putter` + `PutterConfig.Endpoint`) is
backend-agnostic — only the bucket setup and operator runbook change.

---

## 5. Monitoring

| Metric | Alert threshold | Action |
|--------|----------------|--------|
| `audit_sidecar_write_failures_total{reason="put"}` | > 0 for 5 min | Check Supabase Storage connectivity + bucket / RLS policies |
| `audit_sidecar_write_failures_total{reason="enqueue_full"}` | > 0 | Check queue depth; scale up telemetry-server if sustained |
| `audit_sidecar_queue_depth` | > 8000 (80% of 10K cap) | Investigate S3 throughput or worker stalls |
| `audit_sidecar_writes_total` | Drops to 0 when `AUDIT_SIDECAR_BUCKET` set | Sidecar worker may be stuck; restart pod |
