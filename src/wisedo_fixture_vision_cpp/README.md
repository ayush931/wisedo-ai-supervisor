# WiseDo Future Vision — Native C++20 OpenCV Core

This directory contains the native C++20/OpenCV implementation of the **WiseDo Future Vision (`wisedo_fixture_vision_cpp`)** subsystem. It runs independently of Python and provides first-frame reference inspection, annotations, reports, and ARM64-compatible video output.

## Source layout

```text
include/wisedo/vision/       Public C++ contracts and APIs
src/core/                    OpenCV detection, inspection, comparison, rendering, and I/O
src/app/                     CLI executable entry point
tests/unit/                  Native unit tests
cmake/                       OpenCV discovery helpers
```

Keep application argument parsing in `src/app/`, reusable vision behavior in
`src/core/`, public types in `include/`, and test fixtures in `tests/`. Run
`clang-format` with the checked-in `.clang-format` file before submitting C++ changes.

---

## 1. System Requirements & Package Installation

To build and run the C++ core, install the native C++ compiler (GCC 12+ / Clang 16+), CMake (3.22+), and OpenCV 4 development headers and libraries (`core`, `imgproc`, `videoio`, `imgcodecs`).

### Ubuntu / Debian / Raspberry Pi OS (x86_64 / ARM64)

```bash
sudo apt-get update
sudo apt-get install -y \
  build-essential \
  cmake \
  pkg-config \
  libopencv-dev
```

### Fedora / RHEL / CentOS Stream

```bash
sudo dnf install -y \
  gcc-c++ \
  cmake \
  pkgconf-pkg-config \
  opencv-devel
```

### Arch Linux / Manjaro

```bash
sudo pacman -S --needed \
  base-devel \
  cmake \
  opencv
```

### macOS (Apple Silicon / Intel)

```bash
brew install cmake pkg-config opencv
```

---

## 2. Build Instructions

### Quick Build (Release Mode)

```bash
# 1. Configure the build directory
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp -DCMAKE_BUILD_TYPE=Release

# 2. Compile in parallel across all CPU cores
cmake --build build/vision-cpp --parallel $(nproc 2>/dev/null || sysctl -n hw.ncpu)

# 3. Execute the automated unit test suite
ctest --test-dir build/vision-cpp --output-on-failure
```

### Build with Custom OpenCV Installation

If OpenCV is installed in a non-standard directory, pass `-DOpenCV_DIR`:

```bash
# Linux x86_64:
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp \
  -DCMAKE_BUILD_TYPE=Release \
  -DOpenCV_DIR=/usr/lib/x86_64-linux-gnu/cmake/opencv4

# Linux aarch64 (ARM64 / IPC):
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp \
  -DCMAKE_BUILD_TYPE=Release \
  -DOpenCV_DIR=/usr/lib/aarch64-linux-gnu/cmake/opencv4

# macOS Homebrew:
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp \
  -DCMAKE_BUILD_TYPE=Release \
  -DOpenCV_DIR=$(brew --prefix opencv)/lib/cmake/opencv4
```

---

## 3. Running Visual Inspection

### Containerized ARM64 Runtime

Build the native image from the repository root. Docker uses the host or target
architecture; on ARM64, use an ARM64 builder or run the build on the ARM64
device itself.

```bash
docker build \
  -f src/wisedo_fixture_vision_cpp/Dockerfile \
  -t wisedo-future-vision:local .

docker run --rm \
  -v "$PWD:/work:ro" \
  -v "$PWD/inspection_results:/work/inspection_results" \
  wisedo-future-vision:local \
  --video /work/video.mp4 \
  --output-dir /work/inspection_results \
  --side-by-side \
  --codec mp4v
```

The container has no Python dependency. The target OpenCV build must provide a
decoder for the input and an encoder for `mp4v` output.

The primary executable is `build/vision-cpp/wisedo_vision_inspect`.

### Standard Inspection on a Video

Establishes Frame 0 as golden standard, evaluates all frames, and saves results into `inspection_results/`:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results
```

### Standard Inspection with Side-by-Side Dual View Video

Generates both full annotated inspection video and dual-pane `[Reference Frame 0 | Inspected Frame N]` synchronized video and images:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results \
  --side-by-side \
  --codec mp4v
```

### Quick Verification Smoke Test (First N Frames)

Limit processing to the first 30 frames for fast developer feedback:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir /tmp/quick_inspection \
  --side-by-side \
  --max-frames 30
```

### Change-Gated Frame Deduplication

Collapse repeating identical visual states into consolidated records with duration tracking:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results \
  --deduplicate \
  --side-by-side
```

### Custom Fixture Slot Geometry

If using a pre-defined slot configuration file rather than dynamic first-frame auto-discovery:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --config configs/fixture_slots.json \
  --output-dir inspection_results
```

---

## 4. Complete CLI Command Reference

| Flag                         | Default                           | Description                                            |
| :--------------------------- | :-------------------------------- | :----------------------------------------------------- |
| `--video`, `--input`         | `video.mp4`                       | Path to production video or camera stream              |
| `--output-dir`               | `inspection_results`              | Base directory to export images, videos, and reports   |
| `--side-by-side`             | `false`                           | Export dual-pane reference vs current comparison views |
| `--render-video`, `--output` | `<out_dir>/annotated_results.mp4` | Annotated inspection video output file                 |
| `--csv`                      | `<out_dir>/frame_deviations.csv`  | Master audit CSV containing every evaluated frame      |
| `--json`                     | `<out_dir>/frame_deviations.json` | Master audit JSON containing complete metadata         |
| `--correct-csv`              | `<out_dir>/corrected_frames.csv`  | Separate CSV containing only matching frames           |
| `--correct-json`             | `<out_dir>/corrected_frames.json` | Filtered JSON of correct frames with frame ranges      |
| `--deviated-csv`             | `<out_dir>/deviated_frames.csv`   | Separate CSV containing only defective frames          |
| `--deviated-json`            | `<out_dir>/deviated_frames.json`  | Filtered JSON of deviated frames with frame ranges     |
| `--codec`                    | `mp4v`                            | FourCC video codec (`mp4v`, `avc1`, `H264`, `MJPG`)    |
| `--deduplicate`              | `false`                           | Collapse consecutive identical states                  |
| `--include-duplicates`       | `true`                            | Preserve all evaluated frames (default behavior)       |
| `--diff-threshold`           | `10.0`                            | Sensitivity threshold for change detection gating      |
| `--max-frames`               | `-1`                              | Maximum frames to process (`-1` for complete video)    |
| `--config`                   | `""`                              | Optional JSON configuration path                       |
| `--help`                     | —                                 | Display CLI usage summary                              |

---

## 5. Output Directory Structure & Artifacts

When inspection executes, it generates the following directory structure:

```
inspection_results/
├── corrected/                          # Images of frames matching reference
│   ├── frame_0000_CORRECT.jpg
│   ├── frame_0001_CORRECT.jpg
│   └── ...
├── deviated/                           # Images of defective / deviated frames
│   ├── frame_0025_DEVIATED.jpg
│   └── ...
├── side_by_side/                       # Dual-view [Ref Frame 0 | Inspected Frame N]
│   ├── corrected/
│   │   ├── sbs_frame_0000_CORRECT.jpg
│   │   └── ...
│   └── deviated/
│       ├── sbs_frame_0025_DEVIATED.jpg
│       └── ...
├── annotated_results.mp4               # Full inspection video with HUD overlays
├── annotated_side_by_side.mp4          # Synchronized side-by-side video (when --side-by-side)
├── frame_deviations.csv                # Master Excel-ready CSV audit report
├── frame_deviations.json               # Master JSON dataset with accuracy metrics
├── corrected_frames.csv                # Filtered CSV of correct frames
├── corrected_frames.json               # Structured JSON of correct frames with index ranges
├── deviated_frames.csv                 # Filtered CSV of deviated frames
└── deviated_frames.json                # Structured JSON of deviated frames with index ranges
```

---

## 6. Algorithmic Implementation Details

The C++ core implements the following classical computer vision pipeline:

1. **Dynamic Board Localization & Color Calibration**:
    - Computes median Hue, Saturation, and Value across the center crop (20% to 80% on each axis) of Frame 0.
    - Derives adaptive HSV tolerance bounds $[H \pm 15, S - 50 \dots S + 60, V - 70 \dots V + 60]$.
    - Masks the tan fixture board using contour area filtering ($>15\%$ of image area).
2. **Dynamic Slot Auto-Discovery**:
    - Detects low-saturation, high-value components ($S \in [0, 60], V \in [90, 255]$) inside a 3% inset margin of the active board.
    - Performs morphological opening (3x3 rectangular structuring element) to eliminate sensor noise.
    - Clusters overlapping boxes via greedy Non-Maximum Suppression (IoU $\ge 0.30$) into bounding unions.
    - Sorts components spatially in row-major order (`y // 50`, `x`) and labels them `SLOT_01`, `SLOT_02`, etc.
3. **Change-Gated Temporal Processing**:
    - Compares consecutive frames using absolute matrix difference (`cv::absdiff`) over the board bounding box.
    - Skips heavy re-inspection when mean difference $< 1.5$ and fewer than $0.5\%$ of pixels shift beyond noise threshold ($20.0$), saving up to 80% of CPU cycles on static packaging lines.
4. **Dual-Criteria Presence Verification**:
    - Evaluates each slot ROI with a 15% inward margin to eliminate boundary alignment noise.
    - Calculates base plate ratio: $\text{board\_ratio} = \frac{|\text{board pixels in ROI}|}{|\text{total pixels in ROI}|}$.
    - Verified present when $\text{board\_ratio} < 0.40$.
5. **Extra Component Detection**:
    - Identifies candidate components on the current frame.
    - Flags any component having $\text{max\_IoU} < 0.30$ against all reference slot standard positions as an unexpected extra part.
6. **Industrial HUD Annotation**:
    - **Green Bounding Box + Badge**: Matching element present (`CORRECT: SLOT_XX`).
    - **Bright Red Bounding Box + Diagonal Cross + Badge**: Absent element (`DEVIATED: SLOT_XX (MISSING)`).
    - **Amber Bounding Box + Badge**: Unexpected element (`DEVIATED: EXTRA_X`).
    - **Top HUD Banner (44 px)**: High-visibility pass (`[CORRECT]`) / fail (`[DEVIATED]`) banner with real-time slot statistics.
    - **Bottom Footer (28 px)**: Summary of missing and extra components on deviated frames.

---

## 7. Continuous Integration (CI)

A dedicated GitHub Actions workflow is configured under [`.github/workflows/future-vision-cpp.yml`](file:///.github/workflows/future-vision-cpp.yml).

The pipeline automatically:

- Installs necessary OpenCV development packages.
- Compiles the static library `libwisedo_vision_core.a` and CLI `wisedo_vision_inspect`.
- Executes CTest unit tests (`wisedo_vision_core_test`).
- Performs an end-to-end smoke verification test against `video.mp4` validating video and report generation.
