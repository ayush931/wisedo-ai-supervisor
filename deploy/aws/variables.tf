# ==============================================================================
# Variables — every AWS training knob maps 1:1 to a .env.example entry so that
# `scripts/launch_training.py` (job submission) and Terraform (persistent
# infra) can never drift. Defaults mirror .env.example; production overrides
# via terraform.tfvars (never committed secrets — there are no secrets here).
# ==============================================================================

# --- Environment & region (.env: APP_ENV, AWS_DEFAULT_REGION) ----------------

variable "environment" {
  description = "Deployment stage. Gates destructive defaults (e.g. stricter retention in production)."
  type        = string
  default     = "staging"

  validation {
    condition     = contains(["dev", "staging", "production"], var.environment)
    error_message = "environment must be one of: dev, staging, production."
  }
}

variable "aws_region" {
  description = "AWS region for all training resources. Must match AWS_DEFAULT_REGION used by the launcher."
  type        = string
  default     = "ap-south-1"

  validation {
    condition     = can(regex("^[a-z]{2}-[a-z]+-[0-9]$", var.aws_region))
    error_message = "aws_region must be a valid AWS region slug (e.g. ap-south-1)."
  }
}

variable "project" {
  description = "Resource name prefix (DNS-safe, lowercase)."
  type        = string
  default     = "wisedo-oee-ai-training"

  validation {
    condition     = can(regex("^[a-z0-9-]{3,48}$", var.project))
    error_message = "project must be 3-48 chars of lowercase letters, digits, hyphens."
  }
}

# --- Storage & security (.env: S3_BUCKET_NAME only; names below are Terraform-owned) ---

variable "s3_bucket_name" {
  description = "Custom dataset bucket name. Empty string auto-generates <project>-<account>-<region>, matching launcher default."
  type        = string
  default     = ""
}

variable "kms_alias" {
  description = "KMS CMK alias for S3 (SSE-KMS), EBS training volumes, and CloudTrail logs."
  type        = string
  default     = "alias/wisedo-oee-ai-training-cmk"

  validation {
    condition     = startswith(var.kms_alias, "alias/")
    error_message = "kms_alias must start with 'alias/'."
  }
}

variable "kms_deletion_window_days" {
  description = "KMS key scheduled-deletion window. Production must stay at 30."
  type        = number
  default     = 30

  validation {
    condition     = var.kms_deletion_window_days >= 7 && var.kms_deletion_window_days <= 30
    error_message = "kms_deletion_window_days must be between 7 and 30."
  }
}

variable "dataset_noncurrent_expire_days" {
  description = "Expire noncurrent dataset versions after N days (versioned immutability retained via current version)."
  type        = number
  default     = 180
}

variable "enable_cloudtrail_data_events" {
  description = "Enable CloudTrail S3 data-event audit logging on training buckets (ADR 0004 Sec. 9, required in production)."
  type        = bool
  default     = true
}

variable "cloudtrail_log_retention_days" {
  description = "CloudWatch retention for the CloudTrail log group."
  type        = number
  default     = 365
}

# --- SageMaker execution identity (name owned here; launcher takes the ARN) ---

variable "sagemaker_role_name" {
  description = "Name of the SageMaker training execution role."
  type        = string
  default     = "wisedo-oee-sagemaker-execution-role"
}

# --- SageMaker compute defaults (.env: SAGEMAKER_INSTANCE_*, USE_SPOT_*, ...) -
# These are *registry defaults* surfaced to operators; per-run values are set
# by the launcher env vars of the same name and validated there as well.

variable "allowed_instance_types" {
  description = "Selectable GPU pool: the operator allowlist. sagemaker_instance_type and the launcher must pick from this list."
  type        = list(string)
  default = [
    "ml.g5.xlarge", "ml.g5.2xlarge", "ml.g5.12xlarge", "ml.g5.48xlarge",
    "ml.g6.2xlarge", "ml.p4d.24xlarge", "ml.p4de.24xlarge",
  ]

  validation {
    condition     = length(var.allowed_instance_types) > 0
    error_message = "allowed_instance_types must contain at least one instance type."
  }
}

variable "sagemaker_instance_type" {
  description = "Default training instance type (must be inside allowed_instance_types)."
  type        = string
  default     = "ml.g5.2xlarge"

  validation {
    condition     = contains(var.allowed_instance_types, var.sagemaker_instance_type)
    error_message = "sagemaker_instance_type must be one of var.allowed_instance_types."
  }
}

variable "sagemaker_instance_count" {
  description = "Default training node count. Fixed at 1: train.py has no distributed training, so every extra node would re-run the whole job (pay N times)."
  type        = number
  default     = 1

  validation {
    condition     = var.sagemaker_instance_count == 1
    error_message = "sagemaker_instance_count must be 1 — multi-node requires distributed training support, not implemented."
  }
}

variable "volume_size_gb" {
  description = "Encrypted EBS volume size per training node in GB. Mirrors VOLUME_SIZE_GB."
  type        = number
  default     = 150

  validation {
    condition     = var.volume_size_gb >= 30 && var.volume_size_gb <= 1024
    error_message = "volume_size_gb must be between 30 and 1024."
  }
}

variable "training_log_retention_days" {
  description = "CloudWatch retention for /aws/sagemaker/TrainingJobs."
  type        = number
  default     = 90
}

# --- Job-behavior defaults (selectable; per-run override via launcher) --------

variable "default_use_spot_training" {
  description = "Default for USE_SPOT_TRAINING (up to 70% cheaper, preemptible; checkpointing stays on)."
  type        = bool
  default     = false
}

variable "default_max_run_seconds" {
  description = "Default for MAX_RUN_SECONDS (hard per-job compute budget)."
  type        = number
  default     = 86400

  validation {
    condition     = var.default_max_run_seconds >= 60 && var.default_max_run_seconds <= 2419200
    error_message = "default_max_run_seconds must be between 60 and 2419200 (28 days, SageMaker max)."
  }
}

variable "default_max_wait_seconds" {
  description = "Default for MAX_WAIT_SECONDS (queue + spot wait; must cover max run on spot)."
  type        = number
  default     = 108000

  validation {
    condition     = var.default_max_wait_seconds >= var.default_max_run_seconds
    error_message = "default_max_wait_seconds must be >= default_max_run_seconds."
  }
}

# --- IAM selectability (no hardcoded trust beyond the SageMaker service) ------

variable "sagemaker_managed_policy_arns" {
  description = "AWS managed policies attached to the execution role. Default is empty: the role's scoped inline policies (sagemaker_data, ecr_pull, vpc) already cover least-privilege needs; add ARNs only for extra capability."
  type        = list(string)
  default     = []
}

variable "sagemaker_permissions_boundary_arn" {
  description = "Optional permissions boundary for the execution role (empty = none)."
  type        = string
  default     = ""
}

variable "sagemaker_custom_policy_json" {
  description = "Optional custom inline policy JSON for the execution role (empty = none). Use to replace the managed default with least-privilege statements."
  type        = string
  default     = ""

  validation {
    condition     = var.sagemaker_custom_policy_json == "" || can(jsondecode(var.sagemaker_custom_policy_json))
    error_message = "sagemaker_custom_policy_json must be valid JSON or empty."
  }
}

# --- Registry / image / endpoint selectability --------------------------------

variable "enable_model_registry" {
  description = "Create the SageMaker Model Package Group for gated promotion (ADR 0004 Sec. 7)."
  type        = bool
  default     = true
}

variable "model_package_group_name" {
  description = "Model package group name. Empty string uses <project>-<environment>-llm."
  type        = string
  default     = ""
}

variable "enable_ecr_repo" {
  description = "Create the ECR repository for the training image."
  type        = bool
  default     = true
}

variable "ecr_image_keep_count" {
  description = "Keep the last N training images; older ones expire."
  type        = number
  default     = 20

  validation {
    condition     = var.ecr_image_keep_count >= 1 && var.ecr_image_keep_count <= 1000
    error_message = "ecr_image_keep_count must be between 1 and 1000."
  }
}

variable "vpc_endpoint_services" {
  description = "Interface endpoints to create when create_vpc_endpoints is true. Pick only what training needs."
  type        = list(string)
  default     = ["ecr.api", "ecr.dkr", "logs", "sagemaker.api", "sagemaker.runtime"]

  validation {
    condition = alltrue([
      for s in var.vpc_endpoint_services :
      contains(["ecr.api", "ecr.dkr", "logs", "sagemaker.api", "sagemaker.runtime"], s)
    ])
    error_message = "vpc_endpoint_services may only contain: ecr.api, ecr.dkr, logs, sagemaker.api, sagemaker.runtime."
  }
}

# --- Model & dataset config (.env: BASE_MODEL_ID, DATASET_FORMAT/VERSION) ----

variable "base_model_id" {
  description = "Approved open-weights base model (100% open-source per ADR 0004 Sec. 2; closed APIs excluded)."
  type        = string
  default     = "Qwen/Qwen2.5-7B-Instruct"
}

variable "dataset_format" {
  description = "Chat template format of the curated dataset."
  type        = string
  default     = "qwen"

  validation {
    condition     = contains(["qwen", "llama3"], var.dataset_format)
    error_message = "dataset_format must be 'qwen' or 'llama3'."
  }
}

variable "dataset_version" {
  description = "S3 dataset release tag consumed as datasets/<version>/ prefix."
  type        = string
  default     = "v1.0.0"

  validation {
    condition     = can(regex("^v[0-9]+\\.[0-9]+\\.[0-9]+$", var.dataset_version))
    error_message = "dataset_version must be a semantic tag like v1.0.0."
  }
}

# --- Private networking (zero-egress training per ADR 0004 Sec. 7/9) ---------

variable "vpc_id" {
  description = "VPC for SageMaker training ENIs and interface endpoints. Empty = account default networking (dev only)."
  type        = string
  default     = ""

  validation {
    condition     = var.vpc_id == "" || length(var.private_subnet_ids) > 0
    error_message = "vpc_id requires private_subnet_ids (training ENIs need explicit private subnets)."
  }
}

variable "private_subnet_ids" {
  description = "Private subnets for training ENIs. Required in production when vpc_id is set."
  type        = list(string)
  default     = []
}

variable "create_vpc_endpoints" {
  description = "Create S3 gateway + ECR/Logs/SageMaker interface endpoints so training needs no NAT/IGW."
  type        = bool
  default     = false

  validation {
    condition     = !var.create_vpc_endpoints || var.vpc_id != ""
    error_message = "create_vpc_endpoints requires vpc_id (endpoints attach to a VPC)."
  }
}

# --- GitHub Actions OIDC (CI/CD trigger per ADR 0004 Sec. 6) -----------------

variable "enable_github_oidc" {
  description = "Create an IAM role trust for GitHub Actions OIDC to submit training jobs via the launcher."
  type        = bool
  default     = false
}

variable "github_repo" {
  description = "GitHub repo (org/repo) allowed to assume the CI launcher role. Required when enable_github_oidc is true."
  type        = string
  default     = ""
}

# --- Cost guard (ADR 0004 Sec. 10; 0 = disabled) -------------------------------

variable "budget_monthly_limit_usd" {
  description = "Monthly SageMaker cost budget in USD. Drives the SNS/email alerts AND the 100%-actual stop-training action; 0 disables everything."
  type        = number
  default     = 200

  validation {
    condition     = var.budget_monthly_limit_usd >= 0
    error_message = "budget_monthly_limit_usd must be >= 0."
  }
}

variable "budget_alert_email" {
  description = "Optional email for budget alerts (actual >= 80%, forecast >= 100%). Empty = alerts flow via the SNS topic only; confirm the SNS subscription email when set."
  type        = string
  default     = ""
  sensitive   = true

  validation {
    condition     = var.budget_alert_email == "" || can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.budget_alert_email))
    error_message = "budget_alert_email must be empty or a valid email address."
  }
}

# --- Tagging -----------------------------------------------------------------

variable "extra_tags" {
  description = "Additional tags merged into every resource (e.g. CostCenter, Owner)."
  type        = map(string)
  default     = {}
}
