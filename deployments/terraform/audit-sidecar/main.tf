terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

locals {
  default_tags = {
    Project     = "myrobotaxi"
    Component   = "audit-sidecar"
    ManagedBy   = "terraform"
    Issue       = "MYR-77"
    Compliance  = "NFR-3.29"
  }
  tags = merge(local.default_tags, var.tags)
}

# --------------------------------------------------------------------------
# S3 bucket
# Object Lock requires bucket versioning enabled at creation time.
# Once Object Lock is enabled on a bucket it CANNOT be disabled.
# --------------------------------------------------------------------------

resource "aws_s3_bucket" "audit_sidecar" {
  bucket = var.bucket_name

  # Prevent accidental destruction of a compliance-locked bucket.
  lifecycle {
    prevent_destroy = true
  }

  tags = local.tags
}

# Block all public access — this bucket must never be publicly readable.
resource "aws_s3_bucket_public_access_block" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Versioning is required for Object Lock (AWS constraint).
resource "aws_s3_bucket_versioning" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id
  versioning_configuration {
    status = "Enabled"
  }
}

# Object Lock configuration.
# - Default retention: COMPLIANCE mode, retention_days days.
#   Objects cannot be deleted or overwritten by ANY principal (including root)
#   until the retention period expires.
# - GOVERNANCE mode is NOT used for the default because GOVERNANCE can be
#   bypassed by principals with s3:BypassGovernanceRetention. Legal holds are
#   used instead for case-specific extensions.
#
# Important: Object Lock is configured at bucket creation time. This resource
# depends on the versioning resource because AWS requires versioning to be
# enabled before Object Lock can be applied.
resource "aws_s3_bucket_object_lock_configuration" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id

  rule {
    default_retention {
      mode = "COMPLIANCE"
      days = var.retention_days
    }
  }

  depends_on = [aws_s3_bucket_versioning.audit_sidecar]
}

# Server-side encryption: AES-256 (SSE-S3).
# Upgrade to SSE-KMS if your compliance framework requires a customer-managed
# key. The telemetry-server IAM role only needs s3:PutObject; encryption is
# transparent to the uploader.
resource "aws_s3_bucket_server_side_encryption_configuration" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

# Lifecycle rule: transition objects to Glacier Instant Retrieval once the
# Object Lock retention window closes. Long-term compliance cost reduction;
# objects remain accessible (with retrieval delay) for legal discovery.
resource "aws_s3_bucket_lifecycle_configuration" "audit_sidecar" {
  bucket = aws_s3_bucket.audit_sidecar.id

  rule {
    id     = "glacier-after-retention"
    status = "Enabled"

    filter {
      prefix = "audit/v1/"
    }

    transition {
      days          = var.glacier_transition_days
      storage_class = "GLACIER_IR"
    }

    # Never expire: audit logs are retained indefinitely (NFR-3.29).
    # The Glacier tier still stores them; we just move them off Standard.
  }

  depends_on = [aws_s3_bucket_object_lock_configuration.audit_sidecar]
}
