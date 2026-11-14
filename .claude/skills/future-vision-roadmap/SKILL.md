---
name: future-vision-roadmap
description: ADR 0007 - the Future Vision architecture, measured performance targets, the four-phase C++ migration roadmap, the planned Supervisor ingestion client, and what the CLI deliberately does not yet expose. Use this skill when planning Future Vision work, discussing latency or memory targets, the camera pipeline, the image-to-video matcher, or packaging the edge daemon - as distinct from building and running today's executable.
---

# Future Vision architecture and roadmap

Source: `docs/adr/video-embedding-docs/0007-wisedo-future-vision-architecture.md` (**Status:
Approved / Native C++20 OpenCV runtime**), plus `match-video.md` and `fixture-inspection.md`.
Today's build, CLI and pipeline are in the `future-vision-cpp` skill. This skill is the forward plan.
Governing rule: `.claude/rules/00-product-independence.md`.

> The Python module names in the ADR's older design sections are architecture history only. They are
> **not supported runtime entry points**. The supported implementation is the native C++20/OpenCV
> executable under `src/wisedo_fixture_vision_cpp/`.

## Why classical CV

Deep-learning inspection imposes 25–50 ms latency and heavy hardware (PyTorch, CUDA, 2+ GB VRAM).
Future Vision uses a deterministic classical core designed for rigid industrial fixturing.

| Metric | Future Vision | YOLOv8s | Advantage |
| --- | --- | --- | --- |
| Inference / frame | 1.8 ms (550 FPS) | 38.5 ms (26 FPS) | 21× faster |
| RAM footprint | 42 MB | 2,450 MB | 58× less |
| GPU | none | NVIDIA CUDA required | zero hardware CAPEX |
| Model weights | 0 MB (pure code) | 45 MB + PyTorch | instant container launch |
| Cold start | < 10 ms | 1,800 ms | sub-second startup |
| Determinism | 100% | probabilistic | audit-proof |

Benchmarks are on Intel Core i7-1185G7 and ARM Cortex-A76 (Raspberry Pi 5).

## Native C++ targets

These are **targets**, with the Python prototype as the measured baseline:

| Dimension | Python prototype | Native C++20 target |
| --- | --- | --- |
| Per-frame latency | 1.8 ms (550 FPS) | **< 0.8 ms (> 1,200 FPS)** |
| Memory (RSS) | ~42 MB | **< 15 MB** |
| Dependencies | Python runtime, virtualenv | shared `libopencv_core`/`imgproc`/`videoio` |
| Concurrency | restricted by the GIL | native threads, lock-free ring buffers |
| Video pipeline | OpenCV bindings | direct V4L2 zero-copy mmap, GenICam / GigE Vision C++ SDKs |
| Memory layout | per-frame heap allocation | pre-allocated `cv::Mat` frame pools, RAII |

Also targeted: zero GPU requirement, zero GIL overhead and no garbage-collection pauses on
high-speed lines, with dynamic golden-state bootstrapping so the first frame of any feed becomes the
reference without CAD or annotation files.

## Planned engine classes

`DualCriteriaDetector` (HSV saturation deficit `ΔS = 1 − S̄_ROI/255` combined with Sobel gradient
energy `E`), `FirstFrameComparator` (`BootstrapGoldenReference`, `ProcessFrame`,
`HasSignificantChange`), the immutable `SlotInspectionResult` and `FrameInspectionSummary` value
objects, and `SupervisorIngestionClient` (`PostBatch`).

The unified presence decision is `(ΔS ≥ τ_S ∧ E ≥ τ_E) ∨ P(part) ≥ 0.60`.

## Four-phase migration roadmap

1. **Algorithmic parity** — implement `DualCriteriaDetector` and `FirstFrameComparator` in C++20
   against the golden dataset, validating 100% mathematical parity with the prototype's unit tests.
2. **Camera pipeline and frame pool** — zero-copy V4L2/GigE frame buffer management and
   SIMD-accelerated colour conversion using AVX2/NEON intrinsics.
3. **Supervisor intake client** — a lightweight C++ HTTP client packaging inspection deviations into
   strict `POST /v1/sites/{site}/operational-sources/{source}/batches` payloads conforming to `SourceRecordV1`.
4. **Standalone daemon packaging** — a self-contained Alpine/Debian edge container under 35 MB for
   bare-metal edge IPC deployment.

## Planned integration with the Supervisor and OEE

1. **Supervisor ingestion** — runs as an external edge daemon or sidecar on the plant-floor IPC,
   emitting deviation records and frame summaries as normalized `machine_state` or `operator_report`
   events under `SourceRecordV1`. Findings become authoritative evidence for deterministic case
   generation and human-governed investigation. **Future Vision never executes an autonomous PLC
   line stop or direct machine actuation.**
2. **Quality ratio in retained OEE** — correct versus deviated counts inform `Q = good / total`
   without manual operator logging.
3. **Audit trail sealing** — generated CSV/JSON reports, deviation metrics and side-by-side frames
   upload as evidence artifacts linked to investigation cases.

## Deliberate current gaps

The C++ CLI **does not** expose arbitrary still-image-to-video query matching. The ADR describes an
image-to-video matcher (SSIM structural similarity, HSV Bhattacharyya histogram distance, a composite
weighted score) as part of the Python prototype; those Python entry points have been removed from the
supported runtime and must not be used. Reviving that capability means implementing it natively, not
re-enabling the prototype.

The supported native workflow remains first-frame reference comparison through
`build/vision-cpp/wisedo_vision_inspect`. A custom slot JSON is optional — omit `--config` for
first-frame auto-discovery.

## Release gate

`docs/independent-supervisor.md` §6: the Future Vision C++ engine must produce verified inspection
records with no memory leaks and no dropped frames under sustained 60 FPS operation.
