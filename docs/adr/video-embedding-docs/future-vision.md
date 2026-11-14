# Future Vision C++ OpenCV Guide

Future Vision runs as a native C++20/OpenCV edge service. It has no Python runtime dependency.

## Build

```bash
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp -DCMAKE_BUILD_TYPE=Release
cmake --build build/vision-cpp --parallel
ctest --test-dir build/vision-cpp --output-on-failure
```

OpenCV 4 must provide `core`, `imgproc`, `imgcodecs`, and `videoio`. ARM64 deployments should use an OpenCV build with FFmpeg or GStreamer enabled.

## First-frame inspection

Frame 0 is captured as the golden reference. Every frame is compared against it and classified as `CORRECT` or `DEVIATED`. When no custom configuration is supplied, slot geometry is discovered from frame 0.

```bash
mkdir -p inspection_results
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results \
  --side-by-side \
  --codec mp4v
```

Optional smoke test:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir /tmp/future-vision-check \
  --max-frames 30
```

Do not pass `--config` unless the referenced JSON file exists. Auto-discovery is the default.

## Outputs

The command writes annotated images, corrected/deviated frame directories, CSV and JSON reports, an annotated video, and optionally a synchronized side-by-side video. The writer reports its OpenCV backend and exits if the requested codec cannot be opened.

`mp4v` is the default portable output codec for ARM64 deployments. The target system must still provide a matching decoder/player.

## Current native scope

Implemented in the native executable:

- First-frame reference comparison
- Change-gated processing and optional deduplication
- Dynamic slot discovery and HSV fixture inspection
- Annotated output frames and videos
- CSV/JSON deviation reports
- Side-by-side reference/current output

Image-to-video query matching is not currently exposed by the C++ CLI. No Python command is part of the supported Future Vision runtime.
