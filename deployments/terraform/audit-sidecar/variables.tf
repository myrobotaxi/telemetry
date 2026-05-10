variable "bucket_name" {
  description = "S3 bucket name for the AuditLog sidecar. Must be globally unique. Default matches backup-retention.md §2.1 placeholder."
  type        = string
  default     = "myrobotaxi-audit-sidecar-prod"
}

variable "region" {
  description = "AWS region for the bucket and IAM resources."
  type        = string
  default     = "us-east-1"
}

variable "retention_days" {
  description = "Object Lock default COMPLIANCE retention in days (NFR-3.29 + MYR-77). 90 days is the minimum; increase for stricter compliance requirements."
  type        = number
  default     = 90
}

variable "glacier_transition_days" {
  description = "Days after Object Lock expiry before objects transition to S3 Glacier Instant Retrieval. Set to retention_days + 1 to move objects immediately after the retention window closes."
  type        = number
  default     = 91
}

variable "service_principal" {
  description = "IAM principal (role ARN, user ARN, or federated identity) that the telemetry-server runs as. Granted s3:PutObject only."
  type        = string
  # No default — callers must supply their ECS task role ARN or EC2 instance
  # profile ARN. Example: arn:aws:iam::123456789012:role/telemetry-server-ecs-task
}

variable "admin_principal" {
  description = "IAM principal for the audit-sidecar-admin role. Granted lifecycle management and Object Lock override permissions. This is an operator role, NOT for service use."
  type        = string
  # No default — callers supply their admin/break-glass ARN.
}

variable "tags" {
  description = "Additional resource tags merged with the module's defaults."
  type        = map(string)
  default     = {}
}
