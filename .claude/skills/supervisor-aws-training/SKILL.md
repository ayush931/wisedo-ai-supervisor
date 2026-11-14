---
name: supervisor-aws-training
description: The optional offline AI training platform in deploy/aws - Terraform modules for S3/SageMaker/ECR/KMS/IAM/VPC, the guided picker, data preparation and the QLoRA training container. Use this skill for any work on training infrastructure, .tf files, SageMaker jobs, training cost or spot settings, or the launcher - and to confirm that none of it is a Supervisor runtime dependency.
---

# AWS offline training infrastructure

Covers `deploy/aws/` (`main.tf`, `iam.tf`, `network.tf`, `variables.tf`, `outputs.tf`,
`versions.tf`, `terraform.tfvars.example`, `scripts/`, `training/`).
Sources: `deploy/aws/README.md`, `deploy/README.md`, ADR 0004 (referenced throughout),
`docs/adr/agentic-ai-docs/0004-oee-intelligence-llm-training-pipeline.md`.
Governing rule: `.claude/rules/00-product-independence.md`.

**This platform is offline and optional.** The independent Go Supervisor runtime does not require
SageMaker or any AWS training resource to execute. Nothing here may become a release dependency.

## Terraform-first boundary

Infrastructure lifecycle is 100% declarative Terraform — the earlier imperative Python provisioning
is removed. Python remains only for containerized workload execution (`train.py` preprocessing and
QLoRA/LoRA training) and job parameter passing (`launch_training.py`).

Managed resources: S3 data lake and checkpoint buckets with SSE-KMS, SageMaker training jobs, ECR
registries, private VPC endpoints and least-privilege IAM roles.

## Make targets

`make infra-init`, `make infra-plan`, `make infra-apply`, `make train-pick` (guided picker),
`make train-dry-run`, `make data-prep`. Nothing is hardcoded — model, machine, IAM mode and
components are chosen through the picker or explicit per-run overrides, with `.env.example` mapping
into Terraform and the launcher.

## Security requirements

- SSE-KMS by default on all buckets; non-`aws:kms` uploads denied; TLS-only bucket policies; public
  access fully blocked; versioning on everywhere.
- CloudTrail S3 data events with log validation on data buckets (disable only in dev via
  `enable_cloudtrail_data_events=false`).
- `train.py` aborts on `tenant_id`, `actor_id` or serial markers as defense in depth behind the
  upstream anonymization pipeline. No secrets in SSM (non-secret config only), none in outputs, none
  in state-adjacent files.
- Production training runs inside the VPC (`vpc_id`, private subnets, `create_vpc_endpoints=true`)
  with no ingress and no NAT/IGW dependency.

## Cost controls

`USE_SPOT_TRAINING=true` with always-on S3 checkpointing; checkpoints tier to STANDARD_IA after
30 days; ECR keeps the last 20 images; log retention is parameterized. `budget_monthly_limit_usd`
plus `budget_alert_email` raise SNS alerts at 80% actual and 100% forecast SageMaker spend.

## Promotion

Monthly S3 aggregation, a policy revision, or manual dispatch submit via the launcher using the
GitHub OIDC role (`enable_github_oidc=true`, `github_repo` set). Promotion to the model package
group requires the evaluation-suite pass, enforced by process and recorded in the experiment.
Open-weights models only. Digest-pin the training image for releases.
