# Future Vision C++ Fixture Inspection

Fixture inspection is provided by the native `wisedo_vision_inspect` executable under [`src/wisedo_fixture_vision_cpp/`](../../../src/wisedo_fixture_vision_cpp/).

```bash
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp -DCMAKE_BUILD_TYPE=Release
cmake --build build/vision-cpp --parallel

build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results \
  --side-by-side \
  --codec mp4v
```

The executable uses frame 0 as the golden reference, compares all frames, and emits corrected/deviated images, reports, and ARM64-compatible OpenCV video output. A custom slot JSON is optional; omit `--config` to use first-frame auto-discovery.
