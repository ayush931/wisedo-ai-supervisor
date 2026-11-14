# ==============================================================================
# plantops-oee-intelligence — AWS AI training infrastructure (WIS-234 / ADR 0004)
# ------------------------------------------------------------------------------
# SCOPE: offline LLM fine-tuning platform ONLY (S3 data lake, KMS, SageMaker
# execution role, model registry, ECR, audit trail). This is OUT OF SCOPE for
# the OEE core FastAPI runtime (ADR 0002 Sec. 2.2): the core service never
# reads these resources and ignores the AWS_* training vars (extra="ignore").
#
# Ephemeral SageMaker *training jobs* are NOT Terraform resources — they are
# submitted by scripts/launch_training.py so each run is independently
# parameterized, retried, and cost-attributed. Terraform owns persistent infra.
# ==============================================================================

terraform {
  required_version = ">= 1.9.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }

  # Remote state is REQUIRED for any shared environment. Never commit local
  # *.tfstate files (see .gitignore). Configure via -backend-config, e.g.:
  #   terraform init \
  #     -backend-config="bucket=wisedo-terraform-state-<account>-<region>" \
  #     -backend-config="key=oee-ai-training/<env>.tfstate" \
  #     -backend-config="region=ap-south-1" \
  #     -backend-config="use_lockfile=true"
  backend "s3" {}
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "plantops-oee-intelligence"
      Component   = "ai-training"
      ManagedBy   = "terraform"
      Environment = var.environment
    }
  }
}
