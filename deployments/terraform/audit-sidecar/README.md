# Audit Sidecar — Terraform Module

Provisions the S3 bucket + IAM resources backing the MYR-77 audit sidecar
described in `docs/operations/backup-retention.md §2.1`.

## What this module creates

| Resource | Purpose |
|----------|---------|
| `aws_s3_bucket.audit_sidecar` | Append-only S3 bucket with Object Lock enabled |
| `aws_s3_bucket_versioning` | Required by Object Lock (AWS constraint) |
| `aws_s3_bucket_object_lock_configuration` | 90-day COMPLIANCE retention default |
| `aws_s3_bucket_server_side_encryption_configuration` | SSE-AES256 at rest |
| `aws_s3_bucket_lifecycle_configuration` | Glacier transition after retention window |
| `aws_s3_bucket_public_access_block` | All public access blocked |
| `aws_iam_policy.audit_sidecar_write` | `s3:PutObject` only — service role |
| `aws_iam_policy_attachment.audit_sidecar_write` | Attaches write policy to service principal |
| `aws_iam_role.audit_sidecar_admin` | Operator role for legal holds + runbook ops |
| `aws_iam_policy.audit_sidecar_admin` | Object Lock management + list/get |
| `aws_s3_bucket_policy` | Deny-delete + deny-unencrypted-put bucket policy |

## IAM split rationale

The telemetry-server service role receives **s3:PutObject only**. It cannot
delete, update lifecycle rules, or modify Object Lock settings. A compromised
service credential cannot erase audit evidence.

The `audit-sidecar-admin` role is for operators: legal holds, retention
configuration, runbook verification (listing and reading objects). It is NOT
assumed by the telemetry-server at any time.

## Apply procedure

```bash
cd deployments/terraform/audit-sidecar

terraform init

# Review the plan — no cost surprise on Object Lock, but verify retention days.
terraform plan \
  -var "bucket_name=myrobotaxi-audit-sidecar-prod" \
  -var "region=us-east-1" \
  -var "service_principal=telemetry-server-ecs-task" \
  -var "admin_principal=arn:aws:iam::<account>:role/break-glass-admin"

terraform apply \
  -var "bucket_name=myrobotaxi-audit-sidecar-prod" \
  -var "region=us-east-1" \
  -var "service_principal=telemetry-server-ecs-task" \
  -var "admin_principal=arn:aws:iam::<account>:role/break-glass-admin"
```

## Post-apply bring-up checklist

1. **Verify Object Lock** — confirm COMPLIANCE mode and 90-day default:
   ```bash
   aws s3api get-object-lock-configuration \
     --bucket myrobotaxi-audit-sidecar-prod
   ```

2. **Set env vars** on the telemetry-server deployment:
   ```
   AUDIT_SIDECAR_BUCKET=myrobotaxi-audit-sidecar-prod
   AUDIT_SIDECAR_REGION=us-east-1
   ```

3. **Restart** the telemetry-server pod/container to activate the sidecar.

4. **Verify first sidecar object** — trigger an AuditLog INSERT (e.g., a
   `mask_applied` event via a REST request in staging) and confirm an object
   lands under `audit/v1/`:
   ```bash
   aws s3 ls s3://myrobotaxi-audit-sidecar-prod/audit/v1/ --recursive | head
   ```

5. **Verify service IAM can PutObject but NOT DeleteObject**:
   ```bash
   # As the service role — should succeed:
   aws s3 cp /dev/null s3://myrobotaxi-audit-sidecar-prod/audit/v1/smoke-test.json \
     --content-type application/json

   # As the service role — must be rejected (AccessDenied):
   aws s3 rm s3://myrobotaxi-audit-sidecar-prod/audit/v1/smoke-test.json
   ```

6. **Check Prometheus metrics** — after the first AuditLog INSERT with the
   sidecar active, confirm `audit_sidecar_writes_total > 0` and
   `audit_sidecar_write_failures_total == 0` on `/metrics`.

## Forward-only caveat

The sidecar mirrors rows written **after** the telemetry-server restarts with
`AUDIT_SIDECAR_BUCKET` set. Rows written before the deploy date are not
backfilled. For restores predating the sidecar deploy date, use STEP 1 of the
backup-retention runbook directly against the DB AuditLog (see
`docs/operations/backup-retention.md §2`).
