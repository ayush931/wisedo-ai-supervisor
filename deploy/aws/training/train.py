"""QLoRA supervised fine-tuning entrypoint for the OEE domain LLM.

Runs inside the SageMaker training container (see Dockerfile.train) following
ADR 0004 Sec. 5 (Stage 2: SFT). Hyperparameters arrive as SageMaker
``HyperParameters`` — i.e. the exact LoRA/AWQ block from .env.example via
``scripts/launch_training.py`` — written to
``/opt/ml/input/config/hyperparameters.json`` (the SM_HP_* env vars only exist
with sagemaker-training-toolkit, which this image does not ship). Precedence:
CLI flag > SM_HP_* env > hyperparameters.json > built-in default.

Base weights (ADR 0004 Sec. 7, zero-egress): ``--base-model-id`` accepts a
staged ``s3://<bucket>/<prefix>`` URI (downloaded to /opt/ml/model-cache/base)
or an existing local directory. A Hugging Face Hub id requires the explicit
``TRAIN_ALLOW_HUB_DOWNLOAD=1`` escape hatch (local dev with internet only).

Data contract (S3 channel mounted by SageMaker):
    <channel>/train.jsonl, <channel>/val.jsonl
    Each line: {"instruction": ..., "input": ..., "output": ...} (Alpaca format,
    anonymized — tenant_id / actor / serials are stripped upstream per Sec. 9;
    this script refuses records containing tenant markers as defense in depth).

Spot interruptions: SageMaker syncs ``CheckpointConfig`` S3 URI to/from
``/opt/ml/checkpoints`` (the Trainer ``output_dir``), and this script resumes
from the newest ``checkpoint-*`` dir automatically.

Output: LoRA adapter weights to ``SM_MODEL_DIR`` (/opt/ml/model), plus the
resolved chat template id for the Stage-4 merge/quantize step.
"""

# ruff: noqa: T201 — print is this container entrypoint's logging interface
# (captured by SageMaker into CloudWatch).

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

FORBIDDEN_MARKERS = ("tenant_id", "actor_id", "serial_number")

# SageMaker writes the launcher's HyperParameters block here (string values).
HYPERPARAMETERS_FILE = Path("/opt/ml/input/config/hyperparameters.json")
# Spot resume target: SageMaker syncs this dir to/from CheckpointConfig S3.
CHECKPOINT_DIR = Path("/opt/ml/checkpoints")
# Staged base weights land here when --base-model-id is an s3:// URI.
MODEL_CACHE_DIR = Path("/opt/ml/model-cache/base")


def load_json_hyperparameters() -> dict[str, str]:
    """Read hyperparameters.json if present (local runs without SageMaker: {}).

    Keys are the kebab-case hyperparameter names the launcher sends
    (base-model-id, learning-rate, ...); values are JSON-encoded strings.
    """
    if not HYPERPARAMETERS_FILE.is_file():
        return {}
    try:
        raw = json.loads(HYPERPARAMETERS_FILE.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(
            f"error: unreadable hyperparameters file {HYPERPARAMETERS_FILE}: {exc}"
        ) from exc
    if not isinstance(raw, dict):
        raise SystemExit(f"error: {HYPERPARAMETERS_FILE} must be a JSON object")
    return {str(k): str(v) for k, v in raw.items()}


def hp_default(flag: str, env: str, builtin: str, json_hp: dict[str, str]) -> str:
    """Hyperparameter layer under the CLI flag: SM_HP_* env > json > default."""
    if env in os.environ:
        return os.environ[env]
    return json_hp.get(flag, builtin)


def parse_args() -> argparse.Namespace:
    hp = load_json_hyperparameters()

    def d(flag: str, env: str, builtin: str) -> str:
        return hp_default(flag, env, builtin, hp)

    p = argparse.ArgumentParser(description="OEE domain QLoRA SFT (ADR 0004 Stage 2).")
    p.add_argument(
        "--base-model-id",
        default=d("base-model-id", "SM_HP_BASE_MODEL_ID", "Qwen/Qwen2.5-7B-Instruct"),
    )
    p.add_argument(
        "--dataset-format",
        default=d("dataset-format", "SM_HP_DATASET_FORMAT", "qwen"),
        choices=["qwen", "llama3"],
    )
    p.add_argument(
        "--train-dir",
        default=d("train-dir", "SM_CHANNEL_TRAINING", "/opt/ml/input/data/training"),
    )
    p.add_argument("--model-dir", default=d("model-dir", "SM_MODEL_DIR", "/opt/ml/model"))
    p.add_argument(
        "--learning-rate",
        type=float,
        default=float(d("learning-rate", "SM_HP_LEARNING_RATE", "0.0002")),
    )
    p.add_argument(
        "--num-train-epochs",
        type=int,
        default=int(d("num-train-epochs", "SM_HP_NUM_TRAIN_EPOCHS", "3")),
    )
    p.add_argument("--batch-size", type=int, default=int(d("batch-size", "SM_HP_BATCH_SIZE", "4")))
    p.add_argument(
        "--gradient-accumulation-steps",
        type=int,
        default=int(d("gradient-accumulation-steps", "SM_HP_GRADIENT_ACCUMULATION_STEPS", "4")),
    )
    p.add_argument("--lora-r", type=int, default=int(d("lora-r", "SM_HP_LORA_R", "64")))
    p.add_argument(
        "--lora-alpha", type=int, default=int(d("lora-alpha", "SM_HP_LORA_ALPHA", "128"))
    )
    p.add_argument(
        "--lora-dropout",
        type=float,
        default=float(d("lora-dropout", "SM_HP_LORA_DROPOUT", "0.05")),
    )
    p.add_argument(
        "--max-seq-length",
        type=int,
        default=int(d("max-seq-length", "SM_HP_MAX_SEQ_LENGTH", "4096")),
    )
    p.add_argument(
        "--warmup-ratio",
        type=float,
        default=float(d("warmup-ratio", "SM_HP_WARMUP_RATIO", "0.03")),
    )
    p.add_argument(
        "--weight-decay",
        type=float,
        default=float(d("weight-decay", "SM_HP_WEIGHT_DECAY", "0.01")),
    )
    p.add_argument(
        "--max-grad-norm",
        type=float,
        default=float(d("max-grad-norm", "SM_HP_MAX_GRAD_NORM", "0.3")),
    )
    p.add_argument(
        "--resume-from-checkpoint",
        default=d("resume-from-checkpoint", "SM_HP_RESUME_FROM_CHECKPOINT", "auto"),
        choices=["auto", "true", "false"],
        help=(
            "auto (default): resume from the newest checkpoint-* under "
            "/opt/ml/checkpoints when one exists; true forces (warns and starts "
            "fresh when none exists); false always starts at step 0."
        ),
    )
    # SageMaker's container contract appends a stray "train" argv entry after
    # the entrypoint (ENTRYPOINT train.py + `train`), which no flag matches —
    # parse_known_args drops it instead of failing.
    args, _unknown = p.parse_known_args()
    return args


def download_base_model(s3_uri: str, dest: Path) -> Path:
    """Fetch staged base weights (all objects under the prefix) to ``dest``."""
    try:
        import boto3
    except ImportError:
        raise SystemExit("error: boto3 is required to download s3:// base weights") from None
    bucket, _, prefix = s3_uri[5:].partition("/")
    prefix = f"{prefix.rstrip('/')}/" if prefix else ""
    dest.mkdir(parents=True, exist_ok=True)
    s3 = boto3.client("s3")
    count, token = 0, None
    while True:
        kwargs = {"Bucket": bucket, "Prefix": prefix}
        if token:
            kwargs["ContinuationToken"] = token
        resp = s3.list_objects_v2(**kwargs)
        for obj in resp.get("Contents", ()):
            key = obj["Key"]
            rel = key[len(prefix) :]
            if not rel or key.endswith("/"):  # prefix placeholder, not a weight
                continue
            target = dest / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            s3.download_file(bucket, key, str(target))
            count += 1
        token = resp.get("NextContinuationToken")
        if not token:
            break
    if count == 0:
        raise SystemExit(f"error: no staged base weights under {s3_uri}")
    print(f"staged {count} base-weight files from {s3_uri} -> {dest}")
    return dest


def resolve_base_model(base_model_id: str) -> tuple[str, bool]:
    """Resolve base weights to a loadable source (ADR 0004: zero-egress).

    Returns ``(source, trust_remote_code)``. Remote code execution is only ever
    permitted on the explicit Hub escape hatch: caller-supplied model ids with
    ``trust_remote_code=True`` would run arbitrary code with the execution
    role's S3+KMS permissions, and never inside the VPC anyway.
    """
    if base_model_id.startswith("s3://"):
        return str(download_base_model(base_model_id, MODEL_CACHE_DIR)), False
    if Path(base_model_id).is_dir():
        return base_model_id, False
    if os.environ.get("TRAIN_ALLOW_HUB_DOWNLOAD", "") == "1":
        print(f"warning: fetching {base_model_id} from the Hugging Face Hub (local dev only)")
        return base_model_id, True
    raise SystemExit(
        f"error: base model {base_model_id!r} is neither an s3:// URI nor a local "
        "directory. Stage the base weights in S3 and pass "
        "--base-model-id s3://<bucket>/<prefix> per ADR 0004 (zero-egress), or "
        "set TRAIN_ALLOW_HUB_DOWNLOAD=1 to allow a Hub download in local dev."
    )


def find_latest_checkpoint(checkpoint_dir: Path) -> Path | None:
    """Newest checkpoint-<global_step> dir under output_dir, or None."""
    if not checkpoint_dir.is_dir():
        return None
    numbered = [
        d
        for d in checkpoint_dir.glob("checkpoint-*")
        if d.is_dir() and d.name.removeprefix("checkpoint-").isdigit()
    ]
    if not numbered:
        return None
    return max(numbered, key=lambda d: int(d.name.removeprefix("checkpoint-")))


def resolve_resume(mode: str, checkpoint_dir: Path) -> Path | None:
    """Map --resume-from-checkpoint (auto/true/false) to a checkpoint dir.

    Spot interruptions restart the container; SageMaker has already pulled the
    CheckpointConfig S3 URI back into ``checkpoint_dir``, so resuming here
    picks up the interrupted step instead of paying for step 0 again.
    """
    normalized = mode.strip().lower()
    latest = find_latest_checkpoint(checkpoint_dir)
    if normalized == "false":
        return None
    if latest is None:
        if normalized == "true":
            print(f"warning: --resume-from-checkpoint=true but no checkpoint-* in {checkpoint_dir}")
        return None
    return latest


def load_records(train_dir: Path) -> tuple[list[dict], list[dict]]:
    """Load train/val splits and enforce the anonymization invariant."""
    train_path, val_path = train_dir / "train.jsonl", train_dir / "val.jsonl"
    if not train_path.exists():
        raise SystemExit(f"error: training channel missing train.jsonl: {train_path}")

    def _read(path: Path) -> list[dict]:
        records = [
            json.loads(line)
            for line in path.read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]
        for rec in records:
            blob = json.dumps(rec).lower()
            if any(marker in blob for marker in FORBIDDEN_MARKERS):
                raise SystemExit(f"error: {path.name} leaks PII markers — anonymization breach")
            if not all(k in rec for k in ("instruction", "input", "output")):
                raise SystemExit(
                    f"error: {path.name} record is not Alpaca format (instruction/input/output)"
                )
        return records

    val = _read(val_path) if val_path.exists() else []
    return _read(train_path), val


def format_chat(record: dict, dataset_format: str) -> str:
    system = (
        "You are the wiseDo PlantOps OEE Domain Intelligence Assistant. "
        "You explain metrics using only sealed OeeEvidencePackV1 facts. "
        "Metric fabrication is strictly prohibited."
    )
    instruction, context, response = record["instruction"], record["input"], record["output"]
    if dataset_format == "qwen":
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{instruction}"
            + (f"\n\nContext:\n{context}" if context else "")
            + f"<|im_end|>\n<|im_start|>assistant\n{response}<|im_end|>"
        )
    # llama3 format
    return (
        f"<|begin_of_text|><|start_header_id|>system<|end_header_id|>\n{system}<|eot_id|>"
        f"<|start_header_id|>user<|end_header_id|>\n{instruction}"
        + (f"\n\nContext:\n{context}" if context else "")
        + f"<|eot_id|><|start_header_id|>assistant<|end_header_id|>\n{response}<|eot_id|>"
    )


def main() -> None:
    """QLoRA SFT training entrypoint."""
    # Multi-node cost guard: secondary hosts exit cleanly since train.py
    # has no distributed training support (variables.tf forces count=1,
    # but defense-in-depth in case someone submits a raw API call).
    host_rank = int(os.environ.get("SM_CURRENT_HOST_RANK", os.environ.get("RANK", "0")))
    if host_rank != 0:
        print(f"Secondary host (rank={host_rank}): exiting to avoid redundant training cost.")
        return

    args = parse_args()

    # Resolve base model (zero-egress: s3:// or local dir preferred)
    model_path, trust_remote = resolve_base_model(args.base_model_id)

    print(f"Base model: {model_path}")
    print(f"Dataset format: {args.dataset_format}")
    print(f"Trust remote code: {trust_remote}")
    print(f"Output dir: {args.model_dir}")

    # Load and validate training data
    train_dir = Path(args.train_dir)
    train_path = train_dir / "train.jsonl"

    if not train_path.exists():
        raise SystemExit(f"error: training data not found at {train_path}")

    train_records, val_records = load_records(train_dir)
    print(f"Loaded {len(train_records)} training records, {len(val_records)} validation records")

    # Anonymisation defense-in-depth: refuse records with tenant markers
    for i, rec in enumerate(train_records + val_records):
        for field_val in (rec.get("instruction", ""), rec.get("input", ""), rec.get("output", "")):
            for marker in FORBIDDEN_MARKERS:
                if marker in field_val.lower():
                    raise SystemExit(
                        f"error: record {i} contains forbidden marker '{marker}' — "
                        "data must be anonymised before training (ADR 0004 Sec. 9)"
                    )

    # Format chat records
    train_texts = [format_chat(r, args.dataset_format) for r in train_records]
    val_texts = [format_chat(r, args.dataset_format) for r in val_records] if val_records else None

    # Spot resume: find newest checkpoint
    resume_from_path = resolve_resume(args.resume_from_checkpoint, CHECKPOINT_DIR)
    resume_from = str(resume_from_path) if resume_from_path else None

    # ---- Training with PEFT QLoRA ----
    try:
        import torch
        from peft import LoraConfig, get_peft_model
        from transformers import AutoModelForCausalLM, AutoTokenizer, TrainingArguments
        from trl import SFTTrainer
    except ImportError as exc:
        raise SystemExit(
            "error: training dependencies not installed. "
            "Ensure torch, transformers, peft, trl are in the container image."
        ) from exc

    print("Loading tokenizer...")
    tokenizer = AutoTokenizer.from_pretrained(model_path, trust_remote_code=trust_remote)
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token

    print("Loading base model with 4-bit quantization...")
    from transformers import BitsAndBytesConfig

    bnb_config = BitsAndBytesConfig(
        load_in_4bit=True,
        bnb_4bit_quant_type="nf4",
        bnb_4bit_compute_dtype=torch.bfloat16,
        bnb_4bit_use_double_quant=True,
    )
    model = AutoModelForCausalLM.from_pretrained(
        model_path,
        quantization_config=bnb_config,
        device_map="auto",
        trust_remote_code=trust_remote,
    )

    lora_config = LoraConfig(
        r=args.lora_r,
        lora_alpha=args.lora_alpha,
        target_modules=[
            "q_proj",
            "k_proj",
            "v_proj",
            "o_proj",
            "gate_proj",
            "up_proj",
            "down_proj",
        ],
        lora_dropout=args.lora_dropout,
        bias="none",
        task_type="CAUSAL_LM",
    )
    model = get_peft_model(model, lora_config)
    model.print_trainable_parameters()

    training_args = TrainingArguments(
        output_dir=str(CHECKPOINT_DIR),
        num_train_epochs=args.num_train_epochs,
        per_device_train_batch_size=args.batch_size,
        gradient_accumulation_steps=args.gradient_accumulation_steps,
        learning_rate=args.learning_rate,
        bf16=True,
        logging_steps=10,
        save_strategy="steps",
        save_steps=100,
        eval_strategy="steps" if val_texts else "no",
        eval_steps=100 if val_texts else None,
        save_total_limit=3,
        report_to="none",
        gradient_checkpointing=True,
        gradient_checkpointing_kwargs={"use_reentrant": False},
    )

    from datasets import Dataset as HFDataset

    train_dataset = HFDataset.from_dict({"text": train_texts})
    eval_dataset = HFDataset.from_dict({"text": val_texts}) if val_texts else None

    trainer = SFTTrainer(
        model=model,
        args=training_args,
        train_dataset=train_dataset,
        eval_dataset=eval_dataset,
        tokenizer=tokenizer,
        max_seq_length=args.max_seq_length,
        dataset_text_field="text",
    )

    print("Starting QLoRA SFT training...")
    trainer.train(resume_from_checkpoint=resume_from)

    # Save LoRA adapter to SM_MODEL_DIR
    model_dir = Path(args.model_dir)
    model_dir.mkdir(parents=True, exist_ok=True)
    trainer.save_model(str(model_dir))
    tokenizer.save_pretrained(str(model_dir))

    # Write metadata for Stage 4 merge/quantize
    metadata = {
        "base_model_id": args.base_model_id,
        "dataset_format": args.dataset_format,
        "lora_r": args.lora_r,
        "lora_alpha": args.lora_alpha,
        "num_train_epochs": args.num_train_epochs,
        "learning_rate": args.learning_rate,
    }
    (model_dir / "training_metadata.json").write_text(
        json.dumps(metadata, indent=2), encoding="utf-8"
    )
    print(f"Training complete. Adapter saved to {model_dir}")


if __name__ == "__main__":
    main()
