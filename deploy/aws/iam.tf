# ==============================================================================
# IAM: SageMaker execution role (least privilege) + optional GitHub OIDC
# launcher role. No static AWS keys anywhere — auth is workload roles / SSO
# (.env placeholders such as your_access_key_here are rejected by launcher).
# ==============================================================================

resource "aws_iam_role" "sagemaker_execution" {
  name                 = var.sagemaker_role_name
  permissions_boundary = var.sagemaker_permissions_boundary_arn != "" ? var.sagemaker_permissions_boundary_arn : null
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "sagemaker.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

# Scoped data-plane access: only this project's buckets + CMK.
resource "aws_iam_role_policy" "sagemaker_data" {
  name = "${local.name_prefix}-sagemaker-data"
  role = aws_iam_role.sagemaker_execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # The datasets bucket also holds raw/ exports (readable by the
        # data-prep role, NOT by training). The execution role reaches only
        # the curated datasets/ and policies/ prefixes — S3 channel reads for
        # the training job live under datasets/<version>/.
        Sid      = "S3DataLakeObjects"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = ["${aws_s3_bucket.datasets.arn}/datasets/*", "${aws_s3_bucket.datasets.arn}/policies/*"]
      },
      {
        # ListBucket is prefix-scoped for the same reason (raw/ stays hidden).
        Sid      = "S3DataLakeList"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = [aws_s3_bucket.datasets.arn]
        Condition = {
          StringLike = { "s3:prefix" = ["datasets", "datasets/*", "policies", "policies/*"] }
        }
      },
      {
        # GetBucketLocation carries no s3:prefix key, so it must stay in an
        # unconditioned statement or the channel download cannot resolve the
        # bucket region.
        Sid      = "S3DataLakeBucketLocation"
        Effect   = "Allow"
        Action   = ["s3:GetBucketLocation"]
        Resource = [aws_s3_bucket.datasets.arn]
      },
      {
        # Artifacts bucket unchanged: checkpoints (CheckpointConfig) and model
        # outputs (OutputDataConfig) are written here by the training job.
        Sid      = "S3ArtifactsScoped"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:GetBucketLocation"]
        Resource = [aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
      },
      {
        Sid      = "KMSS3Scoped"
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource = [aws_kms_key.training.arn]
        Condition = {
          StringEquals = { "kms:ViaService" = ["s3.${var.aws_region}.amazonaws.com"] }
        }
      },
      {
        # SageMaker EBS volume encryption calls KMS directly (not via S3),
        # so it needs the same key without a ViaService condition. Scope
        # remains tight: this single CMK, this execution role only.
        Sid      = "KMSSageMakerVolumes"
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey", "kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant"]
        Resource = [aws_kms_key.training.arn]
        Condition = {
          StringEquals = { "kms:CallerAccount" = data.aws_caller_identity.current.account_id }
        }
      },
      {
        Sid      = "ECRAuth"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid      = "LogsScoped"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:CreateLogGroup", "logs:DescribeLogStreams"]
        Resource = ["${aws_cloudwatch_log_group.sagemaker_training.arn}:*"]
      },
      {
        Sid      = "MetricsScoped"
        Effect   = "Allow"
        Action   = ["cloudwatch:PutMetricData"]
        Resource = "*"
        Condition = {
          StringEquals = { "cloudwatch:namespace" = "AWS/SageMaker" }
        }
      },
    ]
  })
}

# ECR image pull — only when this stack manages the repo (otherwise the
# operator's custom policy covers their own registry).
resource "aws_iam_role_policy" "sagemaker_ecr_pull" {
  count = var.enable_ecr_repo ? 1 : 0
  name  = "${local.name_prefix}-sagemaker-ecr-pull"
  role  = aws_iam_role.sagemaker_execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid    = "ECRScopedPull"
      Effect = "Allow"
      Action = [
        "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
        "ecr:BatchCheckLayerAvailability",
      ]
      Resource = [aws_ecr_repository.training[0].arn]
    }]
  })
}

# AWS managed policies — selectable via sagemaker_managed_policy_arns. The
# default list is EMPTY: the scoped inline policies above (sagemaker_data,
# ecr_pull, vpc) already cover the training data plane, so managed ARNs are an
# opt-in for genuinely extra capability — never a bootstrap convenience.
resource "aws_iam_role_policy_attachment" "sagemaker_managed" {
  for_each   = toset(var.sagemaker_managed_policy_arns)
  role       = aws_iam_role.sagemaker_execution.name
  policy_arn = each.value
}

# Operator-supplied least-privilege policy (optional, replaces the default).
resource "aws_iam_role_policy" "sagemaker_custom" {
  count  = var.sagemaker_custom_policy_json != "" ? 1 : 0
  name   = "${local.name_prefix}-sagemaker-custom"
  role   = aws_iam_role.sagemaker_execution.id
  policy = var.sagemaker_custom_policy_json
}

# VPC ENI management — attached ONLY when training runs inside a VPC.
resource "aws_iam_role_policy" "sagemaker_vpc" {
  count = local.in_vpc ? 1 : 0
  name  = "${local.name_prefix}-sagemaker-vpc"
  role  = aws_iam_role.sagemaker_execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "ec2:CreateNetworkInterface", "ec2:CreateNetworkInterfacePermission",
        "ec2:DeleteNetworkInterface", "ec2:DeleteNetworkInterfacePermission",
        "ec2:DescribeNetworkInterfaces", "ec2:DescribeVpcs",
        "ec2:DescribeDhcpOptions", "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups",
      ]
      Resource = "*"
    }]
  })
}

# --- GitHub Actions OIDC launcher role (manual / monthly CI triggers) --------

resource "aws_iam_openid_connect_provider" "github" {
  count = var.enable_github_oidc ? 1 : 0
  url   = "https://token.actions.githubusercontent.com"
  client_id_list = [
    "sts.amazonaws.com",
  ]
  thumbprint_list = [
    # GitHub-published Actions OIDC CA thumbprints (GitHub changelog 2023-06-27;
    # either intermediary cert may be served, so trust both). The previous value
    # here was missing its trailing "1" and failed the 40-char SHA-1 format.
    "6938fd4d98bab03faadb97b34396831e3780aea1",
    "1c58a3a8518e8759bf075b76b750d4f2df264fcd",
  ]
  tags = local.tags
}

resource "aws_iam_role" "ci_launcher" {
  count = var.enable_github_oidc ? 1 : 0
  name  = "${local.name_prefix}-ci-launcher"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.github[0].arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud" = "sts.amazonaws.com"
          # Protected branch ONLY: any other branch/tag/PR ref cannot assume
          # the launcher role (repo:* would allow arbitrary refs).
          "token.actions.githubusercontent.com:sub" = "repo:${var.github_repo}:ref:refs/heads/main"
        }
      }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "ci_launcher" {
  count = var.enable_github_oidc ? 1 : 0
  name  = "${local.name_prefix}-ci-launcher-submit"
  role  = aws_iam_role.ci_launcher[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Condition keys used here are documented for sagemaker:CreateTrainingJob
        # in the AWS Service Authorization Reference: sagemaker:InstanceTypes,
        # sagemaker:VpcSubnets, sagemaker:VpcSecurityGroupIds, aws:RequestTag.
        # Request-tag keys live in this Create-only statement because
        # Describe/Stop/List calls carry no tags (a StringEquals on an absent
        # key never matches and would lock us out of our own jobs).
        Sid    = "SubmitTrainingJob"
        Effect = "Allow"
        Action = ["sagemaker:CreateTrainingJob"]
        Resource = [
          "arn:aws:sagemaker:${var.aws_region}:${data.aws_caller_identity.current.account_id}:training-job/*",
        ]
        Condition = merge(
          {
            StringEquals = {
              "aws:RequestedRegion" = var.aws_region
              # build_job_request() in scripts/launch_training.py always sends
              # these two tags — keep launcher and policy in sync.
              "aws:RequestTag/Project"   = "plantops-oee-intelligence"
              "aws:RequestTag/Component" = "ai-training"
            }
            # Single instance type per job; it must sit in the operator pool.
            "ForAllValues:StringLike" = {
              "sagemaker:InstanceTypes" = var.allowed_instance_types
            }
          },
          # When training VPCs exist, submissions may only use those subnets
          # and the platform-managed SG (vacuous pass when no VpcConfig).
          local.in_vpc ? {
            "ForAllValues:StringEquals" = {
              "sagemaker:VpcSubnets"          = var.private_subnet_ids
              "sagemaker:VpcSecurityGroupIds" = [aws_security_group.sagemaker_training[0].id]
            }
          } : {}
        )
      },
      {
        # Describe/Stop take no tags and no instance/VPC parameters — they are
        # scoped to the training-job ARN with the region check only.
        Sid    = "ManageTrainingJobs"
        Effect = "Allow"
        Action = ["sagemaker:DescribeTrainingJob", "sagemaker:StopTrainingJob"]
        Resource = [
          "arn:aws:sagemaker:${var.aws_region}:${data.aws_caller_identity.current.account_id}:training-job/*",
        ]
        Condition = {
          StringEquals = { "aws:RequestedRegion" = var.aws_region }
        }
      },
      {
        # ListTrainingJobs is a List-* API with no resource-level permissions,
        # so it must stay on "*" (an ARN here would never match).
        Sid      = "ListTrainingJobs"
        Effect   = "Allow"
        Action   = ["sagemaker:ListTrainingJobs"]
        Resource = "*"
        Condition = {
          StringEquals = { "aws:RequestedRegion" = var.aws_region }
        }
      },
      {
        Sid      = "PassExecutionRole"
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = [aws_iam_role.sagemaker_execution.arn]
        Condition = {
          StringEquals = { "iam:PassedToService" = "sagemaker.amazonaws.com" }
        }
      },
      {
        Sid      = "ReadConfig"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters"]
        Resource = ["arn:aws:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:parameter/${local.name_prefix}/*"]
      },
      {
        # Monthly data-prep workflow: read raw exports, write SSE-KMS splits.
        Sid      = "DataPrepScoped"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:ListBucket", "s3:GetBucketLocation"]
        Resource = [aws_s3_bucket.datasets.arn, "${aws_s3_bucket.datasets.arn}/*", aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
      },
      {
        Sid      = "DataPrepKMS"
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource = [aws_kms_key.training.arn]
      },
    ]
  })
}
