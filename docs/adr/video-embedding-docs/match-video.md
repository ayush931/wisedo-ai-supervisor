# Future Vision Video Comparison Status

Future Vision's supported native workflow is first-frame reference comparison through the C++20/OpenCV executable:

```bash
build/vision-cpp/wisedo_vision_inspect \
  --video video.mp4 \
  --output-dir inspection_results \
  --side-by-side \
  --codec mp4v
```

The current C++ CLI does not expose arbitrary still-image-to-video query matching. Python entry points for that historical proposal have been removed from the supported runtime and must not be used.
