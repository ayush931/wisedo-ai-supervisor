# AWS AI Training Infrastructure (Terraform .tf / WIS-234 / ADR 0004)

Declarative **Terraform (`.tf`)** platform for offline LLM fine-tuning and dataset management.

> [!NOTE]
> The AI cloud infrastructure has been fully converted from imperative Python provisioning into declarative **Terraform (`.tf`)** configuration files (`main.tf`, `iam.tf`, `network.tf`, `variables.tf`, `outputs.tf`, `versions.tf`). Python is strictly retained for the machine learning workload itself (data preprocessing in `preprocess.py` and model training in `train.py`) and job parameter serialization (`launch_training.py`).
>
> **Out of scope for the independent Go AI Supervisor runtime**: The Supervisor core service never touches these cloud training resources and operates completely independently.

## Architecture (ADR 0004 Sec. 6)

```text
PostgreSQL OEE DB --nightly export--> S3 data lake (versioned, SSE-KMS, CloudTrail)
                                            |
                                     preprocessing (Batch/cron, anonymized)
                                            |
                                   SageMaker Training Jobs (GPU, spot-optional, VPC)
                                            |── checkpoints ──> S3 artifacts
                                            |── metrics ──────> SageMaker Experiment
                                            └── pass gates ───> Model Registry (package group)
                                                                     |
                                                              merge + AWQ quantize
                                                                     |
                                                              ECR training image
                                                                     v
                                                          EKS vLLM serving (Phase 7, separate)
```

## Layout

| Path | Purpose |
| ---- | ------- |
| `versions.tf` | Pinned Terraform/AWS provider + S3 backend stub |
| `variables.tf` | All knobs, 1:1 with `.env.example` (defaults match) |
| `main.tf` | KMS CMK, S3 data lake + artifacts, bucket policies, CloudTrail, ECR, log groups, model registry, SSM config |
| `iam.tf` | SageMaker execution role (least privilege) + optional GitHub OIDC launcher role |
| `network.tf` | VPC endpoints for zero-egress training (only when `vpc_id` is set) |
| `outputs.tf` | ARNs/URIs consumed by the launcher (no secrets, ever) |
| `scripts/launch_training.py` | Submits ephemeral `CreateTrainingJob` from `.env` vars; `--dry-run` to preview |
| `training/` | QLoRA SFT entrypoint (`train.py`) + `Dockerfile.train` for the BYOC image |
| `training/preprocess.py` | Anonymize → template → filter → dedup → 90/5/5 splits → manifest (+ SSE-KMS S3 upload) |
| `.github/workflows/ai-training-data.yml` | Monthly + manual data-prep trigger (secrets: role ARN, region, bucket, KMS key) |

## Choosing your options (nothing is hardcoded)

Three selection layers, highest precedence wins: **launcher CLI flags > `.env`
environment > Terraform defaults**. Terraform owns persistent-platform choices;
the launcher owns per-run choices.

### Guided picker

```bash
python deploy/aws/scripts/launch_training.py --interactive --dry-run
```

Walks you through base model → machine → node count → spot → dataset version
with cost/capability notes, then prints the resolved job for review.

### Explicit per-run overrides

```bash
python deploy/aws/scripts/launch_training.py --dry-run \
  --base-model Qwen/Qwen2.5-14B-Instruct \
  --instance-type ml.g5.12xlarge --instance-count 1 --spot \
  --dataset-version v1.1.0 \
  --hyperparameter lora-r=32 --hyperparameter num-train-epochs=5
```

Every `.env` training var has a matching flag (`--max-run-seconds`,
`--max-wait-seconds`, `--volume-gb`, `--dataset-format`, `--no-spot`, …);
`--hyperparameter KEY=VALUE` (repeatable) overrides any LoRA/AWQ value;
`--vpc` (with `OEE_SSM_PREFIX`) resolves subnets/SG from Terraform SSM params.
Custom dataset bucket without a `-datasets` suffix needs `S3_ARTIFACTS_BUCKET`.

### Which model? (ADR 0004 Sec. 2, open-weights only)

| Choice | When |
| ------ | ---- |
| `Qwen/Qwen2.5-7B-Instruct` (default) | Dev iterations, cheapest, strong math/logic |
| `Qwen/Qwen2.5-14B/32B-Instruct` | Mid-tier reasoning without 70B cost |
| `meta-llama/Llama-3.1-8B-Instruct` | Edge / quantized standby (AWQ) |
| `meta-llama/Llama-3.1-70B-Instruct` | Production primary (needs p4-class machines) |
| `mistralai/Mistral-7B-Instruct-v0.3` | Alternative 7B base |

### Which machine? (ADR 0004 Sec. 5)

| Choice | When |
| ------ | ---- |
| `ml.g5.xlarge` | Cheapest 8B QLoRA dev (~4–8h, ~$5–15) |
| `ml.g5.2xlarge` (default) | 7–14B QLoRA general use |
| `ml.g5.12xlarge` | 8B LoRA bf16 quality pass (~6–12h, ~$45–70) |
| `ml.g5.48xlarge` | 70B QLoRA (~24–36h, ~$250–380) |
| `ml.g6.2xlarge` | Efficient L4 alternative for 8B |
| `ml.p4d.24xlarge` / `ml.p4de.24xlarge` | 70B LoRA bf16 production (~24–48h, ~$750–1,500) |

Restrict what anyone can pick via `allowed_instance_types` (Terraform) or the
`ALLOWED_INSTANCE_TYPES` env var on the launcher (kept in sync).

### Which IAM mode?

| Mode | How |
| ---- | --- |
| Bootstrap (default) | `sagemaker_managed_policy_arns = [".../AmazonSageMakerFullAccess"]` |
| Production least-privilege | Set that list to `[]`, paste scoped statements into `sagemaker_custom_policy_json`, optionally add `sagemaker_permissions_boundary_arn` |

### Which components?

`enable_model_registry`, `enable_experiment`, `enable_ecr_repo` (with
`ecr_image_keep_count`), `vpc_endpoint_services`, `default_use_spot_training`,
`default_max_run/wait_seconds` — all variables, all shown in
`terraform.tfvars.example`.

## Data preparation (monthly, ADR 0004 Sec. 4)

Raw evidence exports become the `datasets/<version>/{train,val,test}.jsonl` +
`manifest.json` the training job consumes. Two ways to run it:

```bash
# Local / ad-hoc (stdlib only; boto3 needed only for s3:// paths)
python deploy/aws/training/preprocess.py \
  --input ./exports/evidence.jsonl --output-dir /tmp/oee-data \
  --dataset-version v1.1.0

# ...with SSE-KMS upload straight to the data lake layout
python deploy/aws/training/preprocess.py \
  --input s3://exports-bucket/raw/2026-09/ --dataset-version v1.1.0 \
  --s3-upload s3://<dataset-bucket>/datasets/v1.1.0/ --kms-key-id <kms-key-id>
```

Or automatically: `.github/workflows/ai-training-data.yml` runs on the 1st of
each month (and on manual dispatch) via the OIDC launcher role. Set repo
secrets `AWS_CI_LAUNCHER_ROLE_ARN`, `AWS_REGION`, `DATASET_BUCKET`, `KMS_KEY_ID`
from `terraform output`. Records with surviving PII markers are rejected and
counted in the manifest — never written.

## `.env.example` → Terraform → Launcher mapping

| `.env` var | Terraform var | Launcher behavior |
| ---------- | ------------- | ----------------- |
| `AWS_DEFAULT_REGION` | `aws_region` (default `ap-south-1`) | boto3 session region |
| `S3_BUCKET_NAME` | `s3_bucket_name` (empty → `<project>-<account>-<region>-datasets`) | `InputDataConfig` S3 prefix |
| `SAGEMAKER_ROLE_ARN` | `sagemaker_role_name` (name) → `sagemaker_execution_role_arn` output | `RoleArn` (required, no default) |
| `KMS_KEY_ID` | `kms_alias` (alias) → `kms_key_id` output | `VolumeKmsKeyId` / `OutputDataConfig` (required, no default) |
| `TRAINING_IMAGE_URI` | `training_ecr_uri` output | training container image (required, no default) |
| `SAGEMAKER_INSTANCE_TYPE/COUNT` | same names (validated allowlist) | `ResourceConfig` |
| `USE_SPOT_TRAINING`, `MAX_RUN/WAIT_SECONDS` | — (job-scoped) | `EnableManagedSpotTraining` + `StoppingCondition` (fails if wait < run on spot) |
| `VOLUME_SIZE_GB` | `volume_size_gb` | EBS size + `VolumeKmsKeyId` |
| `BASE_MODEL_ID`, `DATASET_FORMAT/VERSION` | same names | hyperparameters + `datasets/<version>/` prefix + SSM params |
| `LEARNING_RATE … MAX_GRAD_NORM`, `QUANTIZATION`, `BF16` | — (job-scoped) | SageMaker hyperparameters → `train.py` (`SM_HP_*`) |
| `AWQ_W_BIT`, `AWQ_Q_GROUP_SIZE` | — (job-scoped) | passed through for the Stage-4 merge/quantize step |

Static keys (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`)
were removed from `.env.example`: nothing consumes them and the launcher
**refuses** them — auth is workload IAM roles / SSO / OIDC.

## Usage

```bash
cd deploy/aws
cp terraform.tfvars.example terraform.tfvars   # uncommitted, adjust for env

terraform init \
  -backend-config="bucket=wisedo-terraform-state-<account>-ap-south-1" \
  -backend-config="key=oee-ai-training/staging.tfstate" \
  -backend-config="region=ap-south-1" \
  -backend-config="use_lockfile=true"
terraform plan -out=tfplan
terraform apply tfplan

# Build + push the training image (digest-pin for releases; run from deploy/aws)
cd deploy/aws
aws ecr get-login-password --region ap-south-1 | \
  docker login --username AWS --password-stdin <account>.dkr.ecr.ap-south-1.amazonaws.com
docker build -f training/Dockerfile.train -t $(terraform output -raw training_ecr_uri):v1.0.0 .
docker push $(terraform output -raw training_ecr_uri):v1.0.0
cd -

# Submit a training run (from repo root; .env holds placeholders + job config)
export SAGEMAKER_ROLE_ARN=$(terraform -chdir=deploy/aws output -raw sagemaker_execution_role_arn)
export KMS_KEY_ID=$(terraform -chdir=deploy/aws output -raw kms_key_id)
export TRAINING_IMAGE_URI=<account>.dkr.ecr.ap-south-1.amazonaws.com/<repo>:v1.0.0
python deploy/aws/scripts/launch_training.py --dry-run   # review first
python deploy/aws/scripts/launch_training.py             # submit
```

Or via Make: `make infra-plan`, `make infra-apply`, `make train-dry-run`.

## Security (ADR 0004 Sec. 9)

- SSE-KMS by default on all buckets; uploads without `aws:kms` denied; TLS-only
  bucket policies; public access fully blocked; versioning on everywhere.
- CloudTrail S3 **data events** on data buckets with log validation (disable only
  in dev via `enable_cloudtrail_data_events=false`).
- `train.py` aborts on `tenant_id`/`actor_id`/serial markers (defense in depth
  behind the upstream anonymization pipeline); no secrets in SSM (non-secret
  config only), none in outputs, none in state-adjacent files.
- Production training runs inside the VPC (`vpc_id` + private subnets +
  `create_vpc_endpoints=true`) with no ingress and no NAT/IGW dependency.

## Cost (ADR 0004 Sec. 10)

- `USE_SPOT_TRAINING=true` saves up to 70% on preemptible runs (checkpointing to
  S3 is always on); checkpoints tier to STANDARD_IA after 30 days; ECR keeps the
  last 20 images; log retention is parameterized.
- Hard cap: `budget_monthly_limit_usd` + `budget_alert_email` raise SNS alerts
  at 80% actual / 100% forecast SageMaker spend (confirm the subscription email).

## CI/CD triggers (ADR 0004 Sec. 6)

Monthly S3 aggregation, policy-revision, or manual dispatch submit via the
launcher using the OIDC role (`enable_github_oidc=true`, `github_repo` set).
Promotion to the model package group requires the evaluation-suite pass
(Sec. 7/12 gates) — enforced by process, recorded in the experiment.
