"""SageMaker training-job launcher for the OEE domain LLM (WIS-234 / ADR 0004).

Reads ONLY the AWS AI Training Infrastructure block of .env.example — the same
variables Terraform persists — and submits an ephemeral SageMaker training job
via workload IAM credentials (instance role / SSO / GitHub OIDC). Static AWS
keys are refused: placeholders such as ``your_access_key_here`` fail closed.

Usage:
    export $(grep -v '^#' .env | xargs)   # local iteration only, never commit .env
    python deploy/aws/scripts/launch_training.py --dry-run
    TRAINING_IMAGE_URI=<ecr-uri>:<tag> python deploy/aws/scripts/launch_training.py

Selection precedence (highest wins): CLI flags > environment (.env) > builtins.
    python deploy/aws/scripts/launch_training.py --interactive --dry-run
    python deploy/aws/scripts/launch_training.py --dry-run \
        --base-model Qwen/Qwen2.5-14B-Instruct --instance-type ml.g5.12xlarge --spot \
        --hyperparameter lora-r=32 --hyperparameter num-train-epochs=5
    ALLOWED_INSTANCE_TYPES=ml.g5.2xlarge,ml.g5.12xlarge python ... --dry-run

Environment contract (see .env.example):
    AWS_DEFAULT_REGION, S3_BUCKET_NAME, SAGEMAKER_ROLE_ARN, KMS_KEY_ID,
    TRAINING_IMAGE_URI, SAGEMAKER_INSTANCE_TYPE/COUNT, USE_SPOT_TRAINING,
    MAX_RUN_SECONDS, MAX_WAIT_SECONDS, VOLUME_SIZE_GB, BASE_MODEL_ID,
    DATASET_FORMAT/VERSION, LoRA hyperparameters, AWQ_W_BIT, AWQ_Q_GROUP_SIZE.

Extra operator inputs (NOT in .env — job-scoped, not platform config):
    TRAINING_IMAGE_URI, SAGEMAKER_ROLE_ARN, KMS_KEY_ID, JOB_NAME_PREFIX,
    S3_BUCKET_NAME + S3_ARTIFACTS_BUCKET (override when the dataset bucket has
    no -datasets suffix), VPC_SUBNETS, VPC_SECURITY_GROUPS (or --vpc with
    OEE_SSM_PREFIX to resolve both from Terraform SSM params),
    ALLOWED_INSTANCE_TYPES (comma list overriding the instance allowlist).
"""

# ruff: noqa: T201 — print is this CLI's output interface (dry-run JSON to
# stdout, submission ARN to stderr).

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import UTC, datetime

APPROVED_INSTANCE_TYPES = frozenset(
    {
        "ml.g5.xlarge",
        "ml.g5.2xlarge",
        "ml.g5.12xlarge",
        "ml.g5.48xlarge",
        "ml.g6.2xlarge",
        "ml.p4d.24xlarge",
        "ml.p4de.24xlarge",
    }
)

# Guidance for --interactive (GPU notes condensed from ADR 0004 Sec. 5).
INSTANCE_NOTES = {
    "ml.g5.xlarge": "1x A10G 24GB, cheapest, 8B QLoRA dev (~4-8h, ~$5-15)",
    "ml.g5.2xlarge": "1x A10G 24GB, default, 7-14B QLoRA",
    "ml.g5.12xlarge": "4x A10G 96GB, 8B LoRA bf16 (~6-12h, ~$45-70)",
    "ml.g5.48xlarge": "8x A10G 192GB, 70B QLoRA (~24-36h, ~$250-380)",
    "ml.g6.2xlarge": "1x L4 24GB, efficient 8B QLoRA alternative",
    "ml.p4d.24xlarge": "8x A100 40GB, 70B LoRA bf16 prod",
    "ml.p4de.24xlarge": "8x A100 80GB, 70B LoRA bf16 prod (~24-48h, ~$750-1500)",
}

# Guidance for --interactive (open-weights only per ADR 0004 Sec. 2).
MODEL_CHOICES = [
    ("Qwen/Qwen2.5-7B-Instruct", "default dev, strong math/logic, Apache-2.0"),
    ("Qwen/Qwen2.5-14B-Instruct", "mid-tier, very strong reasoning, Apache-2.0"),
    ("Qwen/Qwen2.5-32B-Instruct", "high-efficiency large, Apache-2.0"),
    ("meta-llama/Llama-3.1-8B-Instruct", "dev/edge quantized standby, Llama Community"),
    ("meta-llama/Llama-3.1-70B-Instruct", "production primary, Llama Community"),
    ("mistralai/Mistral-7B-Instruct-v0.3", "alternative 7B, Apache-2.0"),
]


def allowed_instance_types() -> frozenset[str]:
    """Operator allowlist: ALLOWED_INSTANCE_TYPES env overrides the default pool.

    Must stay in sync with terraform var.allowed_instance_types.
    """
    raw = _env("ALLOWED_INSTANCE_TYPES", "")
    if not raw:
        return APPROVED_INSTANCE_TYPES
    custom = frozenset(s.strip() for s in raw.split(",") if s.strip())
    if not custom:
        raise SystemExit("error: ALLOWED_INSTANCE_TYPES is empty")
    return custom


PLACEHOLDER_KEY_MARKERS = ("your_access_key_here", "your_secret_key_here", "example", "changeme")


def _env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _env_int(name: str, default: int) -> int:
    raw = _env(name, "")
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise SystemExit(f"error: {name}={raw!r} is not an integer") from exc


def _env_float(name: str, default: float) -> float:
    raw = _env(name, "")
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise SystemExit(f"error: {name}={raw!r} is not a float") from exc


def _env_bool(name: str, default: bool) -> bool:
    raw = _env(name, "").lower()
    if not raw:
        return default
    if raw in ("true", "1", "yes"):
        return True
    if raw in ("false", "0", "no"):
        return False
    raise SystemExit(f"error: {name}={raw!r} must be true/false")


def refuse_static_credentials() -> None:
    """Fail closed if placeholder/static AWS keys are exported.

    Production auth is workload IAM roles or SSO. Static keys must never be
    committed or pasted into .env; placeholders fail here instead of silently
    producing SignatureDoesNotMatch at submit time.
    """
    for var in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"):
        value = os.environ.get(var, "").strip().lower()
        if not value:
            continue
        if any(marker in value for marker in PLACEHOLDER_KEY_MARKERS) or value.startswith("akid"):
            raise SystemExit(
                f"error: {var} looks like a placeholder/example key. "
                "Unset it and use a workload IAM role or SSO instead."
            )


def build_hyperparameters() -> dict[str, str]:
    """LoRA/QLoRA + export hyperparameters (ADR 0004 Sec. 5, .env block)."""
    dataset_format = _env("DATASET_FORMAT", "qwen")
    if dataset_format not in ("qwen", "llama3"):
        raise SystemExit("error: DATASET_FORMAT must be 'qwen' or 'llama3'")
    return {
        "base-model-id": _env("BASE_MODEL_ID", "Qwen/Qwen2.5-7B-Instruct"),
        "dataset-format": dataset_format,
        "dataset-version": _env("DATASET_VERSION", "v1.0.0"),
        "learning-rate": str(_env_float("LEARNING_RATE", 0.0002)),
        "num-train-epochs": str(_env_int("NUM_TRAIN_EPOCHS", 3)),
        "batch-size": str(_env_int("BATCH_SIZE", 4)),
        "gradient-accumulation-steps": str(_env_int("GRADIENT_ACCUMULATION_STEPS", 4)),
        "lora-r": str(_env_int("LORA_R", 64)),
        "lora-alpha": str(_env_int("LORA_ALPHA", 128)),
        "lora-dropout": str(_env_float("LORA_DROPOUT", 0.05)),
        "max-seq-length": str(_env_int("MAX_SEQ_LENGTH", 4096)),
        "warmup-ratio": str(_env_float("WARMUP_RATIO", 0.03)),
        "weight-decay": str(_env_float("WEIGHT_DECAY", 0.01)),
        "max-grad-norm": str(_env_float("MAX_GRAD_NORM", 0.3)),
        "quantization": _env("QUANTIZATION", "4bit"),
        "bf16": _env("BF16", "true"),
        "awq-w-bit": str(_env_int("AWQ_W_BIT", 4)),
        "awq-q-group-size": str(_env_int("AWQ_Q_GROUP_SIZE", 128)),
    }


def resolve_names(account_id: str, region: str) -> tuple[str, str]:
    """Resolve dataset + artifacts buckets from overrides or Terraform defaults."""
    custom_bucket = _env("S3_BUCKET_NAME", "")
    bucket = custom_bucket or f"wisedo-oee-ai-training-{account_id}-{region}-datasets"
    artifacts_override = _env("S3_ARTIFACTS_BUCKET", "")
    if artifacts_override:
        return bucket, artifacts_override
    if bucket.endswith("-datasets"):
        return bucket, bucket[: -len("-datasets")] + "-artifacts"
    raise SystemExit(
        f"error: S3_BUCKET_NAME={bucket!r} has no -datasets suffix, so the artifacts "
        "bucket cannot be derived — set S3_ARTIFACTS_BUCKET explicitly"
    )


def build_job_request(args: argparse.Namespace, account_id: str) -> dict:
    region = _env("AWS_DEFAULT_REGION", "ap-south-1")
    instance_type = _env("SAGEMAKER_INSTANCE_TYPE", "ml.g5.2xlarge")
    allowed = allowed_instance_types()
    if instance_type not in allowed:
        pool = ", ".join(sorted(allowed))
        raise SystemExit(f"error: SAGEMAKER_INSTANCE_TYPE={instance_type!r} not in: {pool}")
    instance_count = _env_int("SAGEMAKER_INSTANCE_COUNT", 1)
    if not 1 <= instance_count <= 8:
        raise SystemExit("error: SAGEMAKER_INSTANCE_COUNT must be 1-8")
    use_spot = _env_bool("USE_SPOT_TRAINING", False)
    max_run = _env_int("MAX_RUN_SECONDS", 86400)
    max_wait = _env_int("MAX_WAIT_SECONDS", 108000)
    if use_spot and max_wait < max_run:
        raise SystemExit(
            "error: MAX_WAIT_SECONDS must be >= MAX_RUN_SECONDS when USE_SPOT_TRAINING=true"
        )
    volume_gb = _env_int("VOLUME_SIZE_GB", 150)
    if not 30 <= volume_gb <= 1024:
        raise SystemExit("error: VOLUME_SIZE_GB must be 30-1024")

    image_uri = _env("TRAINING_IMAGE_URI", "")
    if not image_uri:
        raise SystemExit("error: TRAINING_IMAGE_URI is required (see training_ecr_uri output)")
    role_arn = _env("SAGEMAKER_ROLE_ARN", "")
    if not role_arn:
        raise SystemExit("error: SAGEMAKER_ROLE_ARN is required (see terraform output)")
    kms_key_id = _env("KMS_KEY_ID", "")
    if not kms_key_id:
        raise SystemExit("error: KMS_KEY_ID is required (`terraform output kms_key_id`)")

    dataset_version = _env("DATASET_VERSION", "v1.0.0")
    dataset_bucket, artifacts_bucket = resolve_names(account_id, region)
    dataset_prefix = _env("S3_DATASET_PREFIX", f"datasets/{dataset_version}/")
    stamp = datetime.now(UTC).strftime("%Y%m%d-%H%M%S")
    job_name = (
        f"{_env('JOB_NAME_PREFIX', 'oee-llm-sft')}-{dataset_version.replace('.', '-')}-{stamp}"[:63]
    )

    request: dict = {
        "TrainingJobName": job_name,
        "RoleArn": role_arn,
        "AlgorithmSpecification": {
            "TrainingImage": image_uri,
            "TrainingInputMode": "File",
            "MetricDefinitions": [
                {"Name": "train/loss", "Regex": r"'loss': ([0-9.]+)"},
                {"Name": "eval/loss", "Regex": r"'eval_loss': ([0-9.]+)"},
            ],
            "EnableSageMakerMetricsTimeSeries": True,
        },
        "InputDataConfig": [
            {
                "ChannelName": "training",
                "DataSource": {
                    "S3DataSource": {
                        "S3DataType": "S3Prefix",
                        "S3Uri": f"s3://{dataset_bucket}/{dataset_prefix}",
                        "S3DataDistributionType": "FullyReplicated",
                    }
                },
                "CompressionType": "None",
            }
        ],
        "OutputDataConfig": {
            "KmsKeyId": kms_key_id,
            "S3OutputPath": f"s3://{artifacts_bucket}/models/",
        },
        "ResourceConfig": {
            "InstanceType": instance_type,
            "InstanceCount": instance_count,
            "VolumeSizeInGB": volume_gb,
            "VolumeKmsKeyId": kms_key_id,
        },
        "StoppingCondition": {
            "MaxRuntimeInSeconds": max_run,
            **({"MaxWaitTimeInSeconds": max_wait} if use_spot else {}),
        },
        "HyperParameters": build_hyperparameters(),
        "CheckpointConfig": {
            "S3Uri": f"s3://{artifacts_bucket}/checkpoints/{job_name}/",
        },
        "EnableManagedSpotTraining": use_spot,
        "EnableInterContainerTrafficEncryption": True,
        "EnableNetworkIsolation": False,  # needs S3/ECR egress (VPC endpoints when in VPC)
        "Tags": [
            {"Key": "Project", "Value": "plantops-oee-intelligence"},
            {"Key": "Component", "Value": "ai-training"},
            {"Key": "DatasetVersion", "Value": dataset_version},
            {"Key": "BaseModel", "Value": _env("BASE_MODEL_ID", "Qwen/Qwen2.5-7B-Instruct")},
        ],
    }

    subnets = [s.strip() for s in _env("VPC_SUBNETS", "").split(",") if s.strip()]
    security_groups = [s.strip() for s in _env("VPC_SECURITY_GROUPS", "").split(",") if s.strip()]
    if subnets and security_groups:
        request["VpcConfig"] = {"Subnets": subnets, "SecurityGroupIds": security_groups}
    elif subnets or security_groups:
        raise SystemExit("error: VPC_SUBNETS and VPC_SECURITY_GROUPS must be set together")

    return request


def redacted(request: dict) -> dict:
    """Render the job request for --dry-run with ARNs/keys intact but no secrets.

    (The request carries no secrets by construction — auth is the caller's IAM
    identity — so this is a straight JSON dump kept as a separate function to
    enforce that invariant at review time.)
    """
    return json.loads(json.dumps(request))


def ask() -> str:
    """stdin read that aborts cleanly on end-of-input instead of EOFError."""
    try:
        return input().strip()
    except EOFError:
        raise SystemExit("aborted (end of input)") from None


def pick(prompt: str, options: list[str], default: str) -> str:
    """Numbered stdin picker used by --interactive. Returns the chosen value.

    All prompts go to stderr so stdout stays pure JSON for --dry-run piping.
    """
    print(f"\n{prompt}", file=sys.stderr)
    for i, opt in enumerate(options, 1):
        marker = " [default]" if opt == default else ""
        print(f"  {i}. {opt}{marker}", file=sys.stderr)
    print(f"Choose [1-{len(options)}] (Enter = default): ", end="", file=sys.stderr)
    raw = ask()
    if not raw:
        return default
    try:
        return options[int(raw) - 1]
    except (ValueError, IndexError):
        raise SystemExit("error: invalid selection — aborted") from None


def pick_bool(prompt: str, default: bool) -> bool:
    hint = "Y/n" if default else "y/N"
    print(f"\n{prompt} [{hint}]: ", end="", file=sys.stderr)
    raw = ask().lower()
    if not raw:
        return default
    if raw in ("y", "yes"):
        return True
    if raw in ("n", "no"):
        return False
    raise SystemExit("error: answer y/n — aborted") from None


def run_interactive() -> None:
    """Guided selection: model, machine, spot, dataset — writes choices to env."""
    models = [f"{mid} — {note}" for mid, note in MODEL_CHOICES] + ["custom (type a model id)"]
    chosen_model = pick("Base model (open-weights only, ADR 0004 Sec. 2):", models, models[0])
    if chosen_model.startswith("custom"):
        print("Model id: ", end="", file=sys.stderr)
        custom = ask()
        os.environ["BASE_MODEL_ID"] = custom or MODEL_CHOICES[0][0]
    else:
        os.environ["BASE_MODEL_ID"] = chosen_model.split(" — ")[0]

    machines = [
        f"{t} — {INSTANCE_NOTES.get(t, 'custom pool entry')}"
        for t in sorted(allowed_instance_types())
    ]
    default_machine = machines[1] if len(machines) > 1 else machines[0]
    picked = pick("Training machine:", machines, default_machine).split(" — ")[0]
    os.environ["SAGEMAKER_INSTANCE_TYPE"] = picked

    print(f"\nNode count [{_env('SAGEMAKER_INSTANCE_COUNT', '1')}]: ", end="", file=sys.stderr)
    count = ask()
    if count:
        os.environ["SAGEMAKER_INSTANCE_COUNT"] = count
    use_spot = pick_bool("Use Spot (up to 70% cheaper, preemptible)?", False)
    os.environ["USE_SPOT_TRAINING"] = str(use_spot).lower()
    print(f"\nDataset version [{_env('DATASET_VERSION', 'v1.0.0')}]: ", end="", file=sys.stderr)
    version = ask()
    if version:
        os.environ["DATASET_VERSION"] = version
    print(
        f"\nSelected: {os.environ['BASE_MODEL_ID']} on {os.environ['SAGEMAKER_INSTANCE_TYPE']} "
        f"x{os.environ.get('SAGEMAKER_INSTANCE_COUNT', _env('SAGEMAKER_INSTANCE_COUNT', '1'))} "
        f"(spot={os.environ['USE_SPOT_TRAINING']})",
        file=sys.stderr,
    )
    if not pick_bool("Proceed?", True):
        raise SystemExit("aborted by operator") from None


def apply_cli_overrides(args: argparse.Namespace) -> None:
    """Explicit CLI flags win over .env (precedence: CLI > env > builtin)."""
    overrides = {
        "SAGEMAKER_INSTANCE_TYPE": args.instance_type,
        "SAGEMAKER_INSTANCE_COUNT": args.instance_count,
        "BASE_MODEL_ID": args.base_model,
        "DATASET_VERSION": args.dataset_version,
        "DATASET_FORMAT": args.dataset_format,
        "MAX_RUN_SECONDS": args.max_run_seconds,
        "MAX_WAIT_SECONDS": args.max_wait_seconds,
        "VOLUME_SIZE_GB": args.volume_gb,
    }
    for key, value in overrides.items():
        if value is not None:
            os.environ[key] = str(value)
    if args.spot is not None:
        os.environ["USE_SPOT_TRAINING"] = str(args.spot).lower()


def parse_hyperparameter_overrides(pairs: list[str]) -> dict[str, str]:
    extra = {}
    for pair in pairs:
        if "=" not in pair:
            raise SystemExit(f"error: --hyperparameter must be KEY=VALUE, got {pair!r}") from None
        key, value = pair.split("=", 1)
        if not key.strip():
            raise SystemExit(f"error: --hyperparameter has an empty key: {pair!r}") from None
        extra[key.strip()] = value.strip()
    return extra


def resolve_vpc_from_ssm(region: str) -> None:
    """Populate VPC_SUBNETS/VPC_SECURITY_GROUPS from Terraform SSM params.

    Requires OEE_SSM_PREFIX (e.g. wisedo-oee-ai-training-staging) and AWS
    credentials even for --dry-run, since SSM must be read.
    """
    prefix = _env("OEE_SSM_PREFIX", "")
    if not prefix:
        raise SystemExit("error: --vpc needs OEE_SSM_PREFIX (e.g. wisedo-oee-ai-training-staging)")
    try:
        import boto3
    except ImportError:
        raise SystemExit("error: --vpc needs boto3 (requirements-launcher.txt)") from None
    ssm = boto3.client("ssm", region_name=region)
    try:
        resp = ssm.get_parameters(Names=[f"/{prefix}/vpc-subnets", f"/{prefix}/vpc-sg"])
    except Exception as exc:
        raise SystemExit(f"error: cannot read VPC SSM params for {prefix!r}: {exc}") from None
    values = {p["Name"]: p["Value"] for p in resp.get("Parameters", [])}
    subnets = values.get(f"/{prefix}/vpc-subnets", "")
    sg = values.get(f"/{prefix}/vpc-sg", "")
    if not subnets or not sg:
        raise SystemExit(f"error: no VPC under /{prefix}/ — set vpc_id + subnets first")
    os.environ["VPC_SUBNETS"] = subnets
    os.environ["VPC_SECURITY_GROUPS"] = sg


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Submit an OEE LLM SageMaker training job (ADR 0004)."
    )
    parser.add_argument(
        "--dry-run", action="store_true", help="Print the resolved job request without submitting."
    )
    parser.add_argument(
        "--interactive", action="store_true", help="Guided picker: model, machine, spot, dataset."
    )
    parser.add_argument("--instance-type", default=None, help="Override SAGEMAKER_INSTANCE_TYPE.")
    parser.add_argument("--instance-count", default=None, help="Override SAGEMAKER_INSTANCE_COUNT.")
    parser.add_argument(
        "--spot",
        dest="spot",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="Override USE_SPOT_TRAINING (--spot / --no-spot).",
    )
    parser.add_argument("--base-model", default=None, help="Override BASE_MODEL_ID.")
    parser.add_argument("--dataset-version", default=None, help="Override DATASET_VERSION.")
    parser.add_argument(
        "--dataset-format",
        default=None,
        choices=["qwen", "llama3"],
        help="Override DATASET_FORMAT.",
    )
    parser.add_argument("--max-run-seconds", default=None, help="Override MAX_RUN_SECONDS.")
    parser.add_argument("--max-wait-seconds", default=None, help="Override MAX_WAIT_SECONDS.")
    parser.add_argument("--volume-gb", default=None, help="Override VOLUME_SIZE_GB.")
    parser.add_argument(
        "--hyperparameter",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="Extra/override training hyperparameter (repeatable).",
    )
    parser.add_argument(
        "--vpc",
        action="store_true",
        help="Resolve subnets/SG from Terraform SSM params (needs OEE_SSM_PREFIX + AWS creds).",
    )
    args = parser.parse_args(argv)

    if args.interactive:
        run_interactive()
    apply_cli_overrides(args)

    refuse_static_credentials()
    region = _env("AWS_DEFAULT_REGION", "ap-south-1")
    if args.vpc:
        resolve_vpc_from_ssm(region)

    account_id = "000000000000"
    client = None
    if not args.dry_run:
        try:
            import boto3  # lazy import: launcher has its own requirements file
        except ImportError:
            raise SystemExit(
                "error: boto3 missing (pip install -r deploy/aws/scripts/requirements-launcher.txt)"
            ) from None
        session = boto3.Session(region_name=region)
        account_id = session.client("sts").get_caller_identity()["Account"]
        client = session.client("sagemaker")
    else:
        account_id = _env("AWS_ACCOUNT_ID", "000000000000")

    request = build_job_request(args, account_id)
    request["HyperParameters"].update(parse_hyperparameter_overrides(args.hyperparameter))
    print(json.dumps(redacted(request), indent=2, sort_keys=True))

    if args.dry_run:
        return 0
    assert client is not None
    response = client.create_training_job(**request)
    print(f"submitted: {response['TrainingJobArn']}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
