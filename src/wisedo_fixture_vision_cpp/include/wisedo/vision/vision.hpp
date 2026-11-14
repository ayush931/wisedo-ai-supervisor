#pragma once

#include <opencv2/core.hpp>
#include <opencv2/videoio.hpp>

#include <cstddef>
#include <optional>
#include <string>
#include <vector>

namespace wisedo::vision
{

    struct SlotDefinition
    {
        std::string id;
        std::string type;
        cv::Rect bounds;
        std::string description;
    };

    struct DetectionParams
    {
        cv::Scalar board_hsv_lower{10, 50, 70};
        cv::Scalar board_hsv_upper{30, 180, 220};
        double missing_threshold{0.40};
        double margin_ratio{0.15};
    };

    struct InspectionConfig
    {
        std::string fixture_name{"fixture_auto"};
        std::string golden_frame_source{"frame_0"};
        DetectionParams detection;
        std::vector<SlotDefinition> slots;

        static InspectionConfig defaultFor(cv::Size frame_size, std::size_t slot_count = 25);
        static InspectionConfig load(const std::string &path);
    };

    struct SlotInspectionResult
    {
        std::string slot_id;
        std::string type{"component"};
        cv::Rect bounds;
        std::string status{"PRESENT"}; // "PRESENT" or "MISSING"
        bool is_present{false};
        double board_ratio{1.0};
        double confidence{0.0};
        bool matches_reference{true};
        std::string deviation_type; // "" (none), "MISSING", "EXTRA", "CHANGED"
    };

    struct FrameInspectionSummary
    {
        int frame_index{0};
        double timestamp_seconds{0.0};
        bool is_pass{false};
        std::size_t total_slots{0};
        std::size_t present_count{0};
        std::size_t missing_count{0};
        std::size_t deviated_slots_count{0};
        std::vector<std::string> missing_slot_ids;
        std::vector<std::string> deviated_slot_ids;
        double processing_time_ms{0.0};
        std::vector<SlotInspectionResult> slots;
    };

    struct FrameComparison
    {
        bool matches_reference{false};
        double mean_absolute_difference{0.0};
        double changed_pixel_percent{0.0};
    };

    /// Per-frame deviation record mirroring Python FrameDeviationRecord.
    struct FrameDeviationRecord
    {
        int frame_index{0};
        std::string frame_range;
        int repeat_count{1};
        double timestamp_sec{0.0};
        double duration_sec{0.0};
        std::string status{"CORRECT"}; // "CORRECT" or "DEVIATED"
        bool is_correct{false};
        std::size_t total_elements{0};
        std::size_t correct_count{0};
        std::size_t deviated_count{0};
        std::vector<std::string> missing_slots;
        std::vector<std::string> deviated_slots;
        std::size_t extra_slots_count{0};
        std::string image_path;
        double mean_diff{0.0};
        double changed_pct{0.0};
    };

    // ── Board localization & component detection helpers ───────────────────

    /// Computes median HSV across center crop (20%-80% of width and height).
    cv::Scalar getCenterMedianHsv(const cv::Mat &frame);

    /// Derives board HSV range from median color.
    std::pair<cv::Scalar, cv::Scalar> boardThresholdsFromMedian(const cv::Scalar &median);

    /// Largest board contour bbox, or default inset fallback.
    cv::Rect findBoardBbox(
        const cv::Mat &hsv,
        const cv::Scalar &lower,
        const cv::Scalar &upper,
        const cv::Rect &fallback);

    /// Calculates active fixture board bounding box dynamically.
    cv::Rect getDynamicBoardBbox(const cv::Mat &frame, double margin_pct = 0.10);

    /// Intersection-over-union for two bounding boxes.
    double boxIoU(const cv::Rect &a, const cv::Rect &b);

    /// Merge overlapping boxes via greedy IoU clustering.
    std::vector<cv::Rect> mergeBoxesNms(const std::vector<cv::Rect> &boxes, double iou_thresh = 0.30);

    /// Detect white/gray component boxes on fixture board.
    std::vector<cv::Rect> detectWhiteBoxes(const cv::Mat &frame, cv::Scalar *out_median = nullptr);

    /// Auto-discover component slot geometry on a reference frame and calibrate board HSV.
    std::vector<SlotDefinition> autoDiscoverSlots(const cv::Mat &frame, DetectionParams *out_params = nullptr);

    // ── Inspection & Change Detection ──────────────────────────────────────

    class FixtureInspector
    {
    public:
        explicit FixtureInspector(InspectionConfig config);

        FrameInspectionSummary inspect(const cv::Mat &frame, int frame_index, double fps) const;

        const InspectionConfig &config() const noexcept { return config_; }
        void setConfig(InspectionConfig config) { config_ = std::move(config); }

    private:
        InspectionConfig config_;
    };

    bool hasContentChanged(
        const cv::Mat &previous,
        const cv::Mat &current,
        const std::optional<cv::Rect> &board = std::nullopt,
        double mean_diff_threshold = 1.5,
        double pixel_diff_threshold = 20.0,
        double min_changed_pixel_percent = 0.5);

    FrameComparison compareToReference(
        const cv::Mat &reference,
        const cv::Mat &current,
        double mean_diff_threshold = 1.5,
        double pixel_diff_threshold = 20.0,
        double min_changed_pixel_percent = 0.5);

    // ── Rendering helpers ───────────────────────────────────────────────────

    /// Draw full deviation/correctness annotations onto a frame.
    /// Marks slots matching reference with GREEN and deviated slots with RED/CROSS.
    /// Marks extra components with AMBER/ORANGE.
    /// Top HUD banner displays status, frame number, timestamp, and slot statistics.
    cv::Mat renderDeviationAnnotations(
        const cv::Mat &frame,
        const FrameInspectionSummary &summary,
        const std::vector<cv::Rect> &extra_boxes,
        int frame_index,
        double fps,
        bool is_correct);

    /// Create a side-by-side [Reference Frame 0 | Inspected Frame N] view.
    cv::Mat createSideBySideView(
        const cv::Mat &reference_annotated,
        const cv::Mat &frame_annotated,
        int frame_index,
        bool is_correct);

    // ── First-Frame Golden Reference Comparator ─────────────────────────────

    struct ComparatorConfig
    {
        InspectionConfig inspection_config;
        double diff_threshold{10.0};
        double pixel_diff_threshold{20.0};
        double min_changed_pixel_percent{0.5};
        bool deduplicate{false};
    };

    struct ProcessedFrameResult
    {
        int frame_index{0};
        double timestamp_sec{0.0};
        bool is_correct{false};
        FrameInspectionSummary summary;
        std::vector<cv::Rect> extra_boxes;
        FrameComparison comparison;
        FrameDeviationRecord record;
        cv::Mat annotated_frame;
        cv::Mat side_by_side_frame;
    };

    class FirstFrameComparator
    {
    public:
        explicit FirstFrameComparator(ComparatorConfig config);

        bool bootstrapGoldenReference(const cv::Mat &first_frame, double fps);
        ProcessedFrameResult processFrame(const cv::Mat &current_frame, int frame_index, double fps);

        bool isBootstrapped() const noexcept { return bootstrapped_; }
        const cv::Mat &goldenReference() const noexcept { return golden_reference_; }
        const cv::Mat &goldenReferenceAnnotated() const noexcept { return golden_annotated_; }
        const FrameInspectionSummary &goldenSummary() const noexcept { return golden_summary_; }
        const std::vector<cv::Rect> &referenceBoxes() const noexcept { return ref_boxes_; }
        const cv::Rect &boardBbox() const noexcept { return board_bbox_; }
        const ComparatorConfig &config() const noexcept { return config_; }

    private:
        ComparatorConfig config_;
        FixtureInspector inspector_;
        cv::Mat golden_reference_;
        cv::Mat golden_annotated_;
        FrameInspectionSummary golden_summary_;
        std::vector<cv::Rect> ref_boxes_;
        cv::Rect board_bbox_;
        bool bootstrapped_{false};
    };

    // ── Report Exporters ────────────────────────────────────────────────────

    void exportDeviationCsv(
        const std::string &path,
        const std::vector<FrameDeviationRecord> &records);

    void exportDeviationJson(
        const std::string &path,
        const std::string &video_source,
        const std::vector<FrameDeviationRecord> &records,
        int total_frames,
        bool deduplicated = false);

    void exportSubsetJson(
        const std::string &path,
        const std::string &video_source,
        const std::vector<FrameDeviationRecord> &records,
        const std::string &status_label,
        int total_frames);

    std::vector<std::string> computeFrameRanges(const std::vector<int> &indices);

    // ── Video Writer ────────────────────────────────────────────────────────

    class PortableVideoWriter
    {
    public:
        static PortableVideoWriter open(
            const std::string &output_path,
            double fps,
            cv::Size frame_size,
            std::string codec = "mp4v");

        void write(const cv::Mat &frame);
        void release();
        bool isOpened() const noexcept;
        const std::string &backend() const noexcept;

    private:
        cv::VideoWriter writer_;
        std::string backend_;
    };

} // namespace wisedo::vision
