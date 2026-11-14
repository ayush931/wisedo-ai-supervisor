# Outputs consumed by scripts/launch_training.py and CI (role ARN, bucket,
# KMS key, ECR URI). No secrets are ever output.

output "dataset_bucket" {
  description = "S3 data-lake bucket (datasets/<DATASET_VERSION>/ prefix layout)."
  value       = aws_s3_bucket.datasets.id
}

output "artifacts_bucket" {
  description = "S3 bucket for checkpoints, merged and AWQ-quantized model exports."
  value       = aws_s3_bucket.artifacts.id
}

output "kms_key_id" {
  description = "CMK ARN for S3 SSE-KMS, EBS volumes (VolumeKmsKeyId), and CloudTrail logs."
  value       = aws_kms_key.training.arn
}

output "kms_alias" {
  description = "CMK alias."
  value       = aws_kms_alias.training.name
}

output "sagemaker_execution_role_arn" {
  description = "Execution role ARN for CreateTrainingJob RoleArn."
  value       = aws_iam_role.sagemaker_execution.arn
}

output "sagemaker_execution_role_name" {
  description = "Execution role name."
  value       = aws_iam_role.sagemaker_execution.name
}

output "training_ecr_uri" {
  description = "ECR repository URI for the training image (TRAINING_IMAGE_URI prefix). Empty when enable_ecr_repo is false."
  value       = var.enable_ecr_repo ? aws_ecr_repository.training[0].repository_url : ""
}

output "model_package_group_arn" {
  description = "SageMaker Model Registry group for gated promotion (ADR 0004 Sec. 7). Empty when enable_model_registry is false."
  value       = var.enable_model_registry ? aws_sagemaker_model_package_group.oee_llm[0].arn : ""
}

output "training_log_group" {
  description = "CloudWatch log group for training jobs."
  value       = aws_cloudwatch_log_group.sagemaker_training.name
}

output "ci_launcher_role_arn" {
  description = "GitHub OIDC launcher role ARN (empty when enable_github_oidc is false)."
  value       = var.enable_github_oidc ? aws_iam_role.ci_launcher[0].arn : ""
}

output "budget_topic_arn" {
  description = "SNS topic for budget alerts (empty when the budget is disabled)."
  value       = var.budget_monthly_limit_usd > 0 ? aws_sns_topic.budget[0].arn : ""
}

output "sagemaker_security_group_id" {
  description = "Training ENI security group (empty when no VPC)."
  value       = local.in_vpc ? aws_security_group.sagemaker_training[0].id : ""
}
