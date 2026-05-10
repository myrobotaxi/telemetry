output "bucket_name" {
  description = "S3 bucket name. Set AUDIT_SIDECAR_BUCKET to this value on the telemetry-server deployment."
  value       = aws_s3_bucket.audit_sidecar.bucket
}

output "bucket_arn" {
  description = "S3 bucket ARN for use in cross-account or cross-region policies."
  value       = aws_s3_bucket.audit_sidecar.arn
}

output "service_policy_arn" {
  description = "ARN of the telemetry-server write policy. Attach to the ECS task role or EC2 instance profile."
  value       = aws_iam_policy.audit_sidecar_write.arn
}

output "admin_role_arn" {
  description = "ARN of the audit-sidecar-admin role. Use for legal holds and runbook operations."
  value       = aws_iam_role.audit_sidecar_admin.arn
}

output "region" {
  description = "AWS region. Set AUDIT_SIDECAR_REGION to this value on the telemetry-server deployment."
  value       = var.region
}
