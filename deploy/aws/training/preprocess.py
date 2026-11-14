"""Dataset preparation for OEE domain fine-tuning (ADR 0004 Sec. 4).

Turns raw evidence exports into anonymized Alpaca-format splits the training
job consumes as ``datasets/<DATASET_VERSION>/{train,val,test}.jsonl``:

    raw evidence JSONL --> anonymize --> template Q&A --> quality filter -->
    dedup --> 90/5/5 splits --> manifest.json --> (optional) S3 upload (SSE-KMS)

Input record shape (one JSON object per line, subset of OeeEvidencePackV1):
    {"scope": ..., "window": ..., "readiness": "READY" | "NOT_READY" | ...,
     "reason_codes": [...],
     "metrics": {"oee": 0.542, "availability": 0.621, "performance": ..., "quality": ...}}

Anonymization (defense in depth behind the upstream scrub pipeline, Sec. 9):
denied keys are stripped; any record still containing a forbidden marker in
its values is rejected and counted, never written.

Runs on stdlib only for local files; ``s3://`` input/output needs boto3
(pip install -r ../scripts/requirements-launcher.txt).

Usage:
    python deploy/aws/training/preprocess.py \\
        --input ./exports/evidence-oct.jsonl --output-dir /tmp/oee-data \\
        --dataset-version v1.1.0
    python deploy/aws/training/preprocess.py \\
        --input s3://exports-bucket/raw/2026-09/ --dataset-version v1.1.0 \\
        --s3-upload s3://wisedo-oee-ai-training-<acct>-ap-south-1-datasets/datasets/v1.1.0/
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import tempfile
from datetime import UTC, datetime
from pathlib import Path

# ruff: noqa: T201 — print is this CLI's progress interface.

DENIED_KEYS = frozenset(
    {
        "tenant_id",
        "actor_id",
        "user_id",
        "equipment_serial",
        "serial_number",
        "shift_name",
        "operator_name",
    }
)

# Markers are matched against a lowercased blob, so they must be lowercase
# ("eyj" catches JWT-looking eyJ... headers in free-text fields).
FORBIDDEN_MARKERS = ("tenant_id", "actor_id", "serial_number", "eyj")

REQUIRED_METRICS = ("oee", "availability", "performance", "quality")

MAX_OUTPUT_CHARS = 2000
VAL_FRACTION = 0.05
TEST_FRACTION = 0.05


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Build anonymized OEE SFT splits (ADR 0004 Sec. 4).")
    p.add_argument(
        "--input",
        required=True,
        help="Local .jsonl file/dir or s3:// prefix of raw evidence JSONL.",
    )
    p.add_argument(
        "--output-dir", default=".", help="Where to write train/val/test.jsonl + manifest.json."
    )
    p.add_argument(
        "--dataset-version", required=True, help="Release tag, e.g. v1.1.0 (becomes the S3 prefix)."
    )
    p.add_argument("--seed", default="oee", help="Deterministic split seed.")
    p.add_argument(
        "--s3-upload",
        default="",
        help="Optional s3://bucket/prefix/ destination for the split files (SSE-KMS).",
    )
    p.add_argument(
        "--kms-key-id", default=os.environ.get("KMS_KEY_ID", ""), help="CMK for the S3 upload."
    )
    return p.parse_args(argv)


def s3_client():
    try:
        import boto3
    except ImportError:
        raise SystemExit("error: s3:// paths need boto3 (see requirements-launcher.txt)") from None
    return boto3.client("s3")


def split_s3_uri(uri: str) -> tuple[str, str]:
    if not uri.startswith("s3://"):
        raise SystemExit(f"error: not an s3:// URI: {uri!r}")
    rest = uri[5:].rstrip("/")
    bucket, _, prefix = rest.partition("/")
    return bucket, (prefix + "/") if prefix else ""


def collect_input_files(source: str, staging: Path) -> list[Path]:
    if source.startswith("s3://"):
        client, staging_dir = s3_client(), staging / "raw"
        staging_dir.mkdir(parents=True, exist_ok=True)
        bucket, prefix = split_s3_uri(source)
        files = []
        token = None
        while True:
            kwargs = {"Bucket": bucket, "Prefix": prefix}
            if token:
                kwargs["ContinuationToken"] = token
            resp = client.list_objects_v2(**kwargs)
            for obj in resp.get("Contents", ()):
                if obj["Key"].endswith(".jsonl"):
                    dest = staging_dir / Path(obj["Key"]).name
                    client.download_file(bucket, obj["Key"], str(dest))
                    files.append(dest)
            token = resp.get("NextContinuationToken")
            if not token:
                break
        if not files:
            raise SystemExit(f"error: no .jsonl objects under {source}")
        return files
    path = Path(source)
    if path.is_file():
        return [path]
    files = sorted(path.glob("*.jsonl"))
    if not files:
        raise SystemExit(f"error: no .jsonl files in {path}")
    return files


def anonymize(record: dict) -> dict | None:
    """Strip denied keys; return None if forbidden markers survive in values."""
    clean = {k: v for k, v in record.items() if k not in DENIED_KEYS}
    blob = json.dumps(clean).lower()
    if any(marker in blob for marker in FORBIDDEN_MARKERS):
        return None
    return clean


def valid_evidence(record: dict) -> bool:
    metrics = record.get("metrics")
    if not isinstance(metrics, dict) or not all(k in metrics for k in REQUIRED_METRICS):
        return False
    try:
        return all(0.0 <= float(metrics[k]) <= 1.0 for k in REQUIRED_METRICS)
    except (TypeError, ValueError):
        return False


def template_pairs(record: dict) -> list[dict]:
    """Deterministic instruction pairs (mirrors the ADR 0004 Sec. 4 snippet)."""
    m = record["metrics"]
    oee, avail = m["oee"] * 100, m["availability"] * 100
    perf, qual = m["performance"] * 100, m["quality"] * 100
    raw_scope = str(record.get("scope", "unknown-scope"))
    scope_hash = hashlib.sha256(raw_scope.encode("utf-8")).hexdigest()[:8]
    scope = f"line-{scope_hash}"
    readiness = record.get("readiness", "READY")
    codes = ", ".join(record.get("reason_codes", [])) or "none recorded"
    context = f"Scope: {scope}. OEE={oee:.2f}%, A={avail:.2f}%, P={perf:.2f}%, Q={qual:.2f}%."
    pairs = [
        {
            "instruction": "Explain the final OEE calculation based on the provided evidence.",
            "input": context,
            "output": (
                f"The overall OEE is {oee:.2f}%. Availability {avail:.2f}%, "
                f"performance {perf:.2f}%, quality {qual:.2f}%."
            ),
            "source": "template_oee_breakdown",
        },
        {
            "instruction": "What drove availability for this scope and window?",
            "input": context,
            "output": f"Availability is {avail:.2f}%. Recorded reason codes: {codes}.",
            "source": "template_availability_driver",
        },
    ]
    if readiness != "READY":
        pairs.append(
            {
                "instruction": f"Why is this evidence {readiness}?",
                "input": context,
                "output": (
                    f"Readiness is {readiness}. Reason codes: {codes}. "
                    "Remediate before trusting the metric."
                ),
                "source": "template_readiness_reasoning",
            }
        )
    return pairs


def quality_ok(pair: dict) -> bool:
    return (
        all(pair.get(k) for k in ("instruction", "input", "output"))
        and len(pair["output"]) <= MAX_OUTPUT_CHARS
        and len(pair["instruction"]) >= 10
    )


def fingerprint(pair: dict) -> str:
    blob = f"{pair['instruction']}\n{pair['input']}\n{pair['output']}"
    return hashlib.sha256(blob.encode("utf-8")).hexdigest()


def split_index(digest: str, seed: str) -> str:
    """Deterministic 90/5/5 split from the pair hash (stable across reruns)."""
    bucket = int(hashlib.sha256(f"{seed}:{digest}".encode()).hexdigest(), 16) % 100
    if bucket < int(VAL_FRACTION * 100):
        return "val"
    if bucket < int((VAL_FRACTION + TEST_FRACTION) * 100):
        return "test"
    return "train"


def write_jsonl(path: Path, pairs: list[dict]) -> None:
    with path.open("w", encoding="utf-8") as fh:
        for pair in pairs:
            fh.write(json.dumps(pair) + "\n")


def upload_to_s3(out_dir: Path, dest: str, kms_key_id: str) -> None:
    client = s3_client()
    if not kms_key_id:
        raise SystemExit("error: --s3-upload needs --kms-key-id (or KMS_KEY_ID env) for SSE-KMS")
    bucket, prefix = split_s3_uri(dest)
    for name in ("train.jsonl", "val.jsonl", "test.jsonl", "manifest.json"):
        extra = {"ServerSideEncryption": "aws:kms", "SSEKMSKeyId": kms_key_id}
        client.upload_file(str(out_dir / name), bucket, f"{prefix}{name}", ExtraArgs=extra)
        print(f"uploaded s3://{bucket}/{prefix}{name}")


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="oee-prep-") as staging:
        files = collect_input_files(args.input, Path(staging))
        seen: set[str] = set()
        splits: dict[str, list[dict]] = {"train": [], "val": [], "test": []}
        stats = {"raw_records": 0, "rejected_pii": 0, "rejected_shape": 0, "duplicates": 0}

        for path in files:
            for line in path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                stats["raw_records"] += 1
                try:
                    record = json.loads(line)
                except json.JSONDecodeError:
                    stats["rejected_shape"] += 1
                    continue
                clean = anonymize(record)
                if clean is None:
                    stats["rejected_pii"] += 1
                    continue
                if not valid_evidence(clean):
                    stats["rejected_shape"] += 1
                    continue
                for pair in template_pairs(clean):
                    if not quality_ok(pair):
                        stats["rejected_shape"] += 1
                        continue
                    digest = fingerprint(pair)
                    if digest in seen:
                        stats["duplicates"] += 1
                        continue
                    seen.add(digest)
                    splits[split_index(digest, args.seed)].append(pair)

    if not splits["train"]:
        raise SystemExit("error: zero training pairs produced — check input shape and filters")

    for name, pairs in splits.items():
        write_jsonl(out_dir / f"{name}.jsonl", pairs)

    manifest = {
        "dataset_version": args.dataset_version,
        "generated_at": datetime.now(UTC).isoformat(),
        "counts": {name: len(pairs) for name, pairs in splits.items()},
        **stats,
    }
    (out_dir / "manifest.json").write_text(json.dumps(manifest, indent=2), encoding="utf-8")
    print(json.dumps(manifest, indent=2, sort_keys=True))

    if args.s3_upload:
        upload_to_s3(out_dir, args.s3_upload, args.kms_key_id)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
