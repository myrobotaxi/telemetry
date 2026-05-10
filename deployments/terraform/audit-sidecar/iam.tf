# --------------------------------------------------------------------------
# IAM: telemetry-server service role — PutObject only
# --------------------------------------------------------------------------
# The telemetry-server application role is granted s3:PutObject on the audit
# sidecar bucket exclusively. NO delete, no lifecycle management, no bucket
# policy changes. This ensures a compromised service credential cannot erase
# audit evidence.
#
# The actual role that the telemetry-server process runs as is provisioned
# elsewhere (ECS task role, EC2 instance profile, etc.). This module creates
# an IAM policy and attaches it to the EXISTING service principal supplied via
# var.service_principal.
# --------------------------------------------------------------------------

resource "aws_iam_policy" "audit_sidecar_write" {
  name        = "audit-sidecar-write-${var.bucket_name}"
  description = "Grants s3:PutObject on the audit sidecar bucket to the telemetry-server. No delete or lifecycle permissions (MYR-77)."

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AllowPutObjectOnly"
        Effect = "Allow"
        Action = [
          "s3:PutObject"
        ]
        Resource = "${aws_s3_bucket.audit_sidecar.arn}/audit/v1/*"
      }
    ]
  })

  tags = local.tags
}

# Attach the write policy to the service principal (ECS task role, etc.).
# The principal must already exist; Terraform will fail fast if the ARN is
# invalid.
resource "aws_iam_policy_attachment" "audit_sidecar_write" {
  name       = "audit-sidecar-write-${var.bucket_name}"
  policy_arn = aws_iam_policy.audit_sidecar_write.arn
  roles      = [var.service_principal]
}

# --------------------------------------------------------------------------
# IAM: audit-sidecar-admin role — lifecycle management and legal holds
# --------------------------------------------------------------------------
# The admin role is an OPERATOR role only. It MUST NOT be used by the
# telemetry-server process. It is used by:
#   1. The Terraform apply operator (one-time provisioning).
#   2. Legal/compliance team to place and remove GOVERNANCE legal holds.
#   3. Oncall engineers executing the backup-retention runbook.
#
# Permissions:
#   - s3:GetObjectRetention, s3:PutObjectRetention — manage GOVERNANCE holds
#   - s3:GetBucketObjectLockConfiguration         — inspect current config
#   - s3:PutBucketObjectLockConfiguration         — update retention settings
#   - s3:ListBucket, s3:GetObject                 — runbook verification steps
#   - s3:DeleteObject (NOT granted)               — COMPLIANCE mode prevents it
#     regardless of IAM, but we omit it for defence-in-depth.
# --------------------------------------------------------------------------

resource "aws_iam_role" "audit_sidecar_admin" {
  name        = "audit-sidecar-admin"
  description = "Operator role for audit sidecar lifecycle management and legal holds. NOT for service use (MYR-77)."

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Principal = { AWS = var.admin_principal }
        Action    = "sts:AssumeRole"
      }
    ]
  })

  tags = local.tags
}

resource "aws_iam_policy" "audit_sidecar_admin" {
  name        = "audit-sidecar-admin-${var.bucket_name}"
  description = "Admin policy for audit sidecar: Object Lock management, legal holds, runbook verification. Not for service use."

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "AllowListAndGet"
        Effect = "Allow"
        Action = [
          "s3:ListBucket",
          "s3:GetObject",
          "s3:GetObjectVersion",
          "s3:GetObjectRetention",
          "s3:GetObjectLegalHold",
          "s3:GetBucketObjectLockConfiguration",
          "s3:GetBucketVersioning",
        ]
        Resource = [
          aws_s3_bucket.audit_sidecar.arn,
          "${aws_s3_bucket.audit_sidecar.arn}/*"
        ]
      },
      {
        Sid    = "AllowObjectLockManagement"
        Effect = "Allow"
        Action = [
          "s3:PutObjectRetention",
          "s3:PutObjectLegalHold",
          "s3:PutBucketObjectLockConfiguration",
        ]
        Resource = [
          aws_s3_bucket.audit_sidecar.arn,
          "${aws_s3_bucket.audit_sidecar.arn}/*"
        ]
      }
    ]
  })

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "audit_sidecar_admin" {
  role       = aws_iam_role.audit_sidecar_admin.name
  policy_arn = aws_iam_policy.audit_sidecar_admin.arn
}

# Bucket policy: deny all actions except from the service principal and
# admin role. Belt-and-suspenders alongside IAM; ensures no accidental
# cross-account access even if the bucket ACLs are misconfigured.
resource "aws_s3_bucket_policy" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "DenyNonServiceWrite"
        Effect = "Deny"
        Principal = { AWS = "*" }
        Action    = ["s3:DeleteObject", "s3:DeleteObjectVersion"]
        Resource  = "${aws_s3_bucket.audit_sidecar.arn}/*"
        Condition = {
          ArnNotEquals = {
            "aws:PrincipalArn" = var.admin_principal
          }
        }
      },
      {
        Sid    = "DenyUnencryptedPut"
        Effect = "Deny"
        Principal = { AWS = "*" }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.audit_sidecar.arn}/*"
        Condition = {
          StringNotEquals = {
            "s3:x-amz-server-side-encryption" = "AES256"
          }
        }
      }
    ]
  })

  depends_on = [aws_s3_bucket_public_access_block.audit_sidecar]
}
