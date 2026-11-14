---
name: future-vision-cpp
description: The native C++20/OpenCV edge inspection engine in src/wisedo_fixture_vision_cpp - build, CLI, the frame-0 reference pipeline, slot discovery, HUD annotation and the Supervisor integration boundary. Use this skill for any work on fixture or slot inspection, OpenCV code, CMake/CTest for the vision core, ARM64 or IPC deployment, or whenever vision output needs to reach the Supervisor.
---

# Future Vision — native C++20 OpenCV core

Covers `src/wisedo_fixture_vision_cpp/`.
Sources: `src/wisedo_fixture_vision_cpp/README.md`, `README.md`,
`docs/independent-supervisor.md` §4, `docs/adr/video-embedding-docs/0007-wisedo-future-vision-architecture.md`.
Governing rule: `.claude/rules/00-product-independence.md`.

Future Vision runs as a native C++20/OpenCV executable with no Python runtime dependency, and is not
required to run the Go Supervisor.

## Layout

```
include/wisedo/vision/   public C++ contracts and APIs
src/core/                OpenCV detection, inspection, comparison, rendering, I/O
src/app/                 CLI executable entry point
tests/unit/              native unit tests
cmake/                   OpenCV discovery helpers
```

Keep argument parsing in `src/app/`, reusable vision behavior in `src/core/`, public types in
`include/`, and fixtures in `tests/`. Run `clang-format` with the checked-in `.clang-format` before
submitting C++ changes.

## Build and test

Requires a C++20 compiler, CMake 3.22+ and OpenCV 4 with `core`, `imgproc`, `imgcodecs`, `videoio`.
Per-distro install commands are in the module README.

```bash
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp -DCMAKE_BUILD_TYPE=Release
cmake --build build/vision-cpp --parallel
ctest --test-dir build/vision-cpp --output-on-failure
```

Run first-frame inspection:

```bash
build/vision-cpp/wisedo_vision_inspect --video video.mp4 --output-dir inspection_results \
  --side-by-side --codec mp4v
```

## Pipeline

1. **Dynamic board localization and colour calibration** — median HSV across the 20–80% centre crop
   of frame 0; adaptive bounds `[H ± 15, S − 50 … S + 60, V − 70 … V + 60]`; contour area filter
   `> 15%` of image area.
2. **Dynamic slot auto-discovery** — low-saturation, high-value components (`S ∈ [0,60]`,
   `V ∈ [90,255]`) inside a 3% inset; 3×3 morphological opening; greedy NMS at IoU ≥ 0.30; row-major
   sort (`y // 50`, `x`) labelled `SLOT_01`, `SLOT_02`, …
3. **Change-gated temporal processing** — `cv::absdiff` over the board bounding box; skip heavy
   re-inspection when mean difference `< 1.5` and fewer than 0.5% of pixels shift beyond noise
   threshold 20.0.
4. **Dual-criteria presence verification** — 15% inward ROI margin; present when
   `board_ratio < 0.40`.
5. **Extra component detection** — a component with `max_IoU < 0.30` against all reference slot
   positions is an unexpected extra part.
6. **Industrial HUD annotation** — green `CORRECT: SLOT_XX`; bright red with diagonal cross
   `DEVIATED: SLOT_XX (MISSING)`; amber `DEVIATED: EXTRA_X`; 44 px top banner; 28 px bottom footer.

Frame 0 is the golden reference; later frames are `CORRECT` or `DEVIATED`.

## Supervisor integration boundary

Future Vision is an **external edge inspection sensor**, not part of the Go Supervisor core runtime.
Inspection summaries, component deviations and missing-part alerts are normalized into
`SourceRecordV1` `machine_state` or `operator_report` events and published to
`POST /v1/sites/{site}/operational-sources/{source}/batches`.

**Zero autonomous actuation.** It detects and reports visual deviations. It never issues a PLC
rejection and never overrides human authorization.

## CI and release gate

`.github/workflows/future-vision-cpp.yml` installs OpenCV, builds
`libwisedo_vision_core.a` and `wisedo_vision_inspect`, runs CTest, and performs an end-to-end smoke
verification validating video and report generation. The edge-vision release gate
(`docs/independent-supervisor.md` §6) requires verified inspection records with no memory leaks and
no dropped frames under sustained 60 FPS operation.
