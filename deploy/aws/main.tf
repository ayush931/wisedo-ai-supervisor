# ==============================================================================
# Persistent training platform: KMS, S3 data lake, audit trail, ECR, registry.
# Ephemeral training jobs are submitted by scripts/launch_training.py.
# ==============================================================================

data "aws_caller_identity" "current" {}

locals {
  name_prefix = "${var.project}-${var.environment}"
  # Default bucket naming mirrors the launcher: wisedo-oee-ai-training-<account>-<region>
  default_bucket_base = "${var.project}-${data.aws_caller_identity.current.account_id}-${var.aws_region}"
  dataset_bucket      = var.s3_bucket_name != "" ? var.s3_bucket_name : "${local.default_bucket_base}-datasets"
  artifacts_bucket    = "${local.default_bucket_base}-artifacts"
  trail_log_bucket    = "${local.default_bucket_base}-trail-logs"
  in_vpc              = var.vpc_id != ""
  model_package_group = var.model_package_group_name != "" ? var.model_package_group_name : "${local.name_prefix}-llm"
  tags                = merge({ CostCenter = "ai-training" }, var.extra_tags)
}

# --- Customer-managed KMS key (SSE-KMS everywhere) ----------------------------

resource "aws_kms_key" "training" {
  description             = "CMK for OEE AI training: S3 data lake, EBS volumes, CloudTrail logs (${var.environment})"
  deletion_window_in_days = var.kms_deletion_window_days
  enable_key_rotation     = true

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AllowAccountAdministration"
        Effect    = "Allow"
        Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
        Action    = "kms:*"
        Resource  = "*"
      },
      {
        Sid       = "AllowSageMakerUse"
        Effect    = "Allow"
        Principal = { AWS = aws_iam_role.sagemaker_execution.arn }
        Action = [
          "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*",
          "kms:GenerateDataKey*", "kms:DescribeKey",
          "kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant",
        ]
        Resource = "*"
      },
      {
        Sid       = "AllowCloudTrailUse"
        Effect    = "Allow"
        Principal = { Service = "cloudtrail.amazonaws.com" }
        Action    = ["kms:GenerateDataKey*", "kms:Decrypt", "kms:DescribeKey"]
        Resource  = "*"
      },
      {
        # CloudWatch Logs must be able to write the KMS-encrypted log groups
        # (/aws/cloudtrail/*, /aws/sagemaker/TrainingJobs/*) — without this
        # statement both log groups fail to create. SourceAccount pins the
        # grant to this account's log groups only.
        Sid       = "AllowCloudWatchLogsUse"
        Effect    = "Allow"
        Principal = { Service = "logs.${var.aws_region}.amazonaws.com" }
        Action = [
          "kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*",
          "kms:GenerateDataKey*", "kms:DescribeKey",
        ]
        Resource = "*"
        Condition = {
          StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
        }
      },
    ]
  })

  tags = local.tags
}

resource "aws_kms_alias" "training" {
  name          = var.kms_alias
  target_key_id = aws_kms_key.training.key_id
}

# --- S3 data lake: datasets (immutable, versioned) ----------------------------

resource "aws_s3_bucket" "datasets" {
  bucket = local.dataset_bucket
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "datasets" {
  bucket = aws_s3_bucket.datasets.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "datasets" {
  bucket = aws_s3_bucket.datasets.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.training.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "datasets" {
  bucket                  = aws_s3_bucket.datasets.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "datasets" {
  bucket = aws_s3_bucket.datasets.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# Layout contract with the launcher/training image:
#   s3://<bucket>/datasets/<DATASET_VERSION>/{train,val,test}/
#   s3://<bucket>/policies/<DATASET_VERSION>/   (CalculationPolicyV1 snapshots)
resource "aws_s3_object" "dataset_prefixes" {
  for_each = toset(["datasets/", "datasets/${var.dataset_version}/", "policies/", "policies/${var.dataset_version}/"])
  bucket   = aws_s3_bucket.datasets.id
  key      = each.value
  content  = ""
}

resource "aws_s3_bucket_lifecycle_configuration" "datasets" {
  bucket = aws_s3_bucket.datasets.id

  rule {
    id     = "abort-incomplete-mpu"
    status = "Enabled"
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"
    noncurrent_version_expiration {
      noncurrent_days = var.dataset_noncurrent_expire_days
    }
  }
}

# --- S3: training artifacts (checkpoints, merged + AWQ exports) ---------------

resource "aws_s3_bucket" "artifacts" {
  bucket = local.artifacts_bucket
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.training.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# Layout contract: checkpoints/<job-name>/, models/<model-version>/ (AWQ ready)
resource "aws_s3_object" "artifact_prefixes" {
  for_each = toset(["checkpoints/", "models/", "metrics/"])
  bucket   = aws_s3_bucket.artifacts.id
  key      = each.value
  content  = ""
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "abort-incomplete-mpu"
    status = "Enabled"
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  rule {
    id     = "tier-checkpoints-to-ia"
    status = "Enabled"
    filter {
      prefix = "checkpoints/"
    }
    transition {
      days          = 30
      storage_class = "STANDARD_IA"
    }
  }
}

# --- Deny unencrypted / insecure transport on both data buckets ---------------

data "aws_iam_policy_document" "secure_transport" {
  for_each = toset(["datasets", "artifacts"])

  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      each.value == "datasets" ? aws_s3_bucket.datasets.arn : aws_s3_bucket.artifacts.arn,
      "${each.value == "datasets" ? aws_s3_bucket.datasets.arn : aws_s3_bucket.artifacts.arn}/*",
    ]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid     = "DenyUnencryptedUploads"
    effect  = "Deny"
    actions = ["s3:PutObject"]
    resources = [
      "${each.value == "datasets" ? aws_s3_bucket.datasets.arn : aws_s3_bucket.artifacts.arn}/*",
    ]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      # IfExists: multipart UploadPart calls and uploads relying on the bucket
      # default encryption never carry s3:x-amz-server-side-encryption — a bare
      # StringNotEquals would deny every model artifact > 8 MB.
      test     = "StringNotEqualsIfExists"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms"]
    }
  }
}

resource "aws_s3_bucket_policy" "datasets" {
  bucket = aws_s3_bucket.datasets.id
  policy = data.aws_iam_policy_document.secure_transport["datasets"].json
}

resource "aws_s3_bucket_policy" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  policy = data.aws_iam_policy_document.secure_transport["artifacts"].json
}

# --- CloudTrail audit (S3 data events, log validation, KMS) -------------------

resource "aws_s3_bucket" "trail_logs" {
  count  = var.enable_cloudtrail_data_events ? 1 : 0
  bucket = local.trail_log_bucket
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "trail_logs" {
  count  = var.enable_cloudtrail_data_events ? 1 : 0
  bucket = aws_s3_bucket.trail_logs[0].id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "trail_logs" {
  count  = var.enable_cloudtrail_data_events ? 1 : 0
  bucket = aws_s3_bucket.trail_logs[0].id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.training.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "trail_logs" {
  count                   = var.enable_cloudtrail_data_events ? 1 : 0
  bucket                  = aws_s3_bucket.trail_logs[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "trail_logs" {
  count  = var.enable_cloudtrail_data_events ? 1 : 0
  bucket = aws_s3_bucket.trail_logs[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # CloudTrail calls GetBucketAcl WITHOUT the x-amz-acl header, so this
        # check must be its own unconditional statement — sharing it with the
        # PutObject ACL condition denies the trail's bucket-acl call outright.
        Sid       = "AllowCloudTrailBucketAcl"
        Effect    = "Allow"
        Principal = { Service = "cloudtrail.amazonaws.com" }
        Action    = "s3:GetBucketAcl"
        Resource  = aws_s3_bucket.trail_logs[0].arn
      },
      {
        Sid       = "AllowCloudTrailWrite"
        Effect    = "Allow"
        Principal = { Service = "cloudtrail.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = ["${aws_s3_bucket.trail_logs[0].arn}/oee-ai-trail/*"]
        Condition = { StringEquals = { "s3:x-amz-acl" = "bucket-owner-full-control" } }
      },
    ]
  })
}

resource "aws_cloudwatch_log_group" "trail" {
  count             = var.enable_cloudtrail_data_events ? 1 : 0
  name              = "/aws/cloudtrail/${local.name_prefix}"
  retention_in_days = var.cloudtrail_log_retention_days
  kms_key_id        = aws_kms_key.training.arn
  tags              = local.tags
}

resource "aws_iam_role" "cloudtrail_to_cw" {
  count = var.enable_cloudtrail_data_events ? 1 : 0
  name  = "${local.name_prefix}-cloudtrail-to-cw"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "cloudtrail.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "cloudtrail_to_cw" {
  count = var.enable_cloudtrail_data_events ? 1 : 0
  name  = "cloudtrail-to-cw"
  role  = aws_iam_role.cloudtrail_to_cw[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = "${aws_cloudwatch_log_group.trail[0].arn}:*"
    }]
  })
}

resource "aws_cloudtrail" "oee_ai" {
  count                         = var.enable_cloudtrail_data_events ? 1 : 0
  name                          = "${local.name_prefix}-trail"
  s3_bucket_name                = aws_s3_bucket.trail_logs[0].id
  s3_key_prefix                 = "oee-ai-trail"
  kms_key_id                    = aws_kms_key.training.arn
  enable_log_file_validation    = true
  include_global_service_events = true
  is_multi_region_trail         = true
  cloud_watch_logs_group_arn    = "${aws_cloudwatch_log_group.trail[0].arn}:*"
  cloud_watch_logs_role_arn     = aws_iam_role.cloudtrail_to_cw[0].arn

  event_selector {
    read_write_type           = "All"
    include_management_events = true

    data_resource {
      type   = "AWS::S3::Object"
      values = ["${aws_s3_bucket.datasets.arn}/", "${aws_s3_bucket.artifacts.arn}/"]
    }
  }

  tags = local.tags
}

# --- ECR: training + serving images (scanned, lifecycle-pruned) ---------------

resource "aws_ecr_repository" "training" {
  count                = var.enable_ecr_repo ? 1 : 0
  name                 = "${local.name_prefix}-training"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = var.environment != "production"

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = local.tags
}

resource "aws_ecr_lifecycle_policy" "training" {
  count      = var.enable_ecr_repo ? 1 : 0
  repository = aws_ecr_repository.training[0].name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep last ${var.ecr_image_keep_count} training images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = var.ecr_image_keep_count }
      action       = { type = "expire" }
    }]
  })
}

# --- CloudWatch: SageMaker training logs --------------------------------------

resource "aws_cloudwatch_log_group" "sagemaker_training" {
  name              = "/aws/sagemaker/TrainingJobs/${local.name_prefix}"
  retention_in_days = var.training_log_retention_days
  kms_key_id        = aws_kms_key.training.arn
  tags              = local.tags
}

# --- SageMaker Model Registry (promotion gates per ADR 0004 Sec. 7/12) --------

resource "aws_sagemaker_model_package_group" "oee_llm" {
  count                           = var.enable_model_registry ? 1 : 0
  model_package_group_name        = local.model_package_group
  model_package_group_description = "OEE domain LLM checkpoints. Promotion requires evaluation-suite pass (ADR 0004 Sec. 7)."
  tags                            = local.tags
}

# --- SSM parameters: NON-SECRET config consumed by launcher/pipelines ---------
# Secrets are NEVER stored here (ADR 0004 Sec. 9: no secrets on training data
# paths; auth is via workload IAM roles).

resource "aws_ssm_parameter" "config" {
  for_each = {
    "/${local.name_prefix}/base-model-id"       = var.base_model_id
    "/${local.name_prefix}/dataset-format"      = var.dataset_format
    "/${local.name_prefix}/dataset-version"     = var.dataset_version
    "/${local.name_prefix}/s3-dataset-bucket"   = aws_s3_bucket.datasets.id
    "/${local.name_prefix}/s3-artifacts-bucket" = aws_s3_bucket.artifacts.id
    "/${local.name_prefix}/kms-key-id"          = aws_kms_key.training.arn
    "/${local.name_prefix}/sagemaker-role-arn"  = aws_iam_role.sagemaker_execution.arn
  }
  name  = each.key
  type  = "String"
  value = each.value
  tags  = local.tags
}

# VPC attachment published for `launch_training.py --vpc` (zero-egress made easy).
resource "aws_ssm_parameter" "vpc" {
  for_each = local.in_vpc ? {
    "/${local.name_prefix}/vpc-subnets" = join(",", var.private_subnet_ids)
    "/${local.name_prefix}/vpc-sg"      = aws_security_group.sagemaker_training[0].id
  } : {}
  name  = each.key
  type  = "String"
  value = each.value
  tags  = local.tags
}

# --- Monthly SageMaker cost budget (0 = disabled) + hard cost stop -----------
# Alerts flow through the SNS topic (always) plus budget_alert_email when set.
# At 100% of actual spend an automatic budget action attaches a deny policy to
# the CI launcher role, so no further training job can be submitted.

resource "aws_sns_topic" "budget" {
  count = var.budget_monthly_limit_usd > 0 ? 1 : 0
  name  = "${local.name_prefix}-budget-alerts"
  tags  = local.tags
}

# AWS Budgets can only publish to the topic with this resource-policy grant
# (see "Creating an Amazon SNS topic for budget notifications" in the AWS docs).
resource "aws_sns_topic_policy" "budget" {
  count = var.budget_monthly_limit_usd > 0 ? 1 : 0
  arn   = aws_sns_topic.budget[0].arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowBudgetsPublish"
      Effect    = "Allow"
      Principal = { Service = "budgets.amazonaws.com" }
      Action    = "SNS:Publish"
      Resource  = aws_sns_topic.budget[0].arn
      Condition = {
        StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:aws:budgets::${data.aws_caller_identity.current.account_id}:*" }
      }
    }]
  })
}

# Optional email subscriber — skipped when budget_alert_email is empty
# (alerts still flow via the SNS topic; an empty endpoint would fail anyway).
resource "aws_sns_topic_subscription" "budget_email" {
  count     = var.budget_monthly_limit_usd > 0 && var.budget_alert_email != "" ? 1 : 0
  topic_arn = aws_sns_topic.budget[0].arn
  protocol  = "email"
  endpoint  = var.budget_alert_email
}

resource "aws_budgets_budget" "training" {
  count        = var.budget_monthly_limit_usd > 0 ? 1 : 0
  name         = local.name_prefix
  budget_type  = "COST"
  limit_amount = tostring(var.budget_monthly_limit_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "Service"
    values = ["Amazon SageMaker"]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = var.budget_alert_email != "" ? [var.budget_alert_email] : []
    subscriber_sns_topic_arns  = [aws_sns_topic.budget[0].arn]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = var.budget_alert_email != "" ? [var.budget_alert_email] : []
    subscriber_sns_topic_arns  = [aws_sns_topic.budget[0].arn]
  }
}

# Hard cost stop: a Deny policy that blocks all training-job creation. It is
# attached to the CI launcher role by the budget action below when actual
# spend reaches 100% of the monthly limit. Only meaningful when the launcher
# role exists (enable_github_oidc), hence the shared count condition.
resource "aws_iam_policy" "budget_stop" {
  count = var.budget_monthly_limit_usd > 0 && var.enable_github_oidc ? 1 : 0
  name  = "${local.name_prefix}-budget-stop"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "DenyTrainingJobAboveBudget"
      Effect   = "Deny"
      Action   = ["sagemaker:CreateTrainingJob"]
      Resource = "*"
    }]
  })
  tags = local.tags
}

# AWS Budgets assumes this role to attach/detach the stop policy on our behalf
# (trust + permissions pattern per "Setting up a role for AWS Budgets to run
# budget actions"; scoped to the single launcher role).
resource "aws_iam_role" "budget_action" {
  count = var.budget_monthly_limit_usd > 0 && var.enable_github_oidc ? 1 : 0
  name  = "${local.name_prefix}-budget-action"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "budgets.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "budget_action" {
  count = var.budget_monthly_limit_usd > 0 && var.enable_github_oidc ? 1 : 0
  name  = "budget-action"
  role  = aws_iam_role.budget_action[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "ApplyStopPolicyToLauncherRole"
      Effect   = "Allow"
      Action   = ["iam:AttachRolePolicy", "iam:DetachRolePolicy", "iam:ListAttachedRolePolicies"]
      Resource = [aws_iam_role.ci_launcher[0].arn]
    }]
  })
}

resource "aws_budgets_budget_action" "stop_training" {
  count              = var.budget_monthly_limit_usd > 0 && var.enable_github_oidc ? 1 : 0
  budget_name        = aws_budgets_budget.training[0].name
  action_type        = "APPLY_IAM_POLICY"
  approval_model     = "AUTOMATIC"
  notification_type  = "ACTUAL"
  execution_role_arn = aws_iam_role.budget_action[0].arn

  action_threshold {
    action_threshold_type  = "PERCENTAGE"
    action_threshold_value = 100
  }

  definition {
    iam_action_definition {
      policy_arn = aws_iam_policy.budget_stop[0].arn
      roles      = [aws_iam_role.ci_launcher[0].name]
    }
  }

  # Required subscriber block (>= 1); the alert rides the budget SNS topic.
  subscriber {
    subscription_type = "SNS"
    address           = aws_sns_topic.budget[0].arn
  }

  tags = local.tags
}
