#include <wisedo/vision/vision.hpp>

#include <opencv2/imgproc.hpp>

#include <cassert>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <iostream>

int main()
{
    using namespace wisedo::vision;

    const cv::Size size{1280, 720};
    cv::Mat hsv_pixel(1, 1, CV_8UC3, cv::Scalar(20, 110, 160));
    cv::Mat bgr_pixel;
    cv::cvtColor(hsv_pixel, bgr_pixel, cv::COLOR_HSV2BGR);

    // 1. Test median HSV calculation
    cv::Mat empty(size, CV_8UC3, bgr_pixel.at<cv::Vec3b>(0, 0));
    const auto median = getCenterMedianHsv(empty);
    assert(std::abs(median[0] - 20.0) < 2.0);
    assert(std::abs(median[1] - 110.0) < 2.0);
    assert(std::abs(median[2] - 160.0) < 2.0);

    // 2. Test dynamic board bounding box
    const auto board_bbox = getDynamicBoardBbox(empty);
    assert(board_bbox.width > 0 && board_bbox.height > 0);

    // 3. Test box IoU and mergeBoxesNms
    const cv::Rect box1(10, 10, 50, 50);
    const cv::Rect box2(20, 20, 50, 50);
    const double iou = boxIoU(box1, box2);
    assert(iou > 0.30);
    const auto merged = mergeBoxesNms({box1, box2}, 0.30);
    assert(merged.size() == 1);
    assert(merged[0].x == 10 && merged[0].y == 10);
    assert(merged[0].width == 60 && merged[0].height == 60);

    // 4. Test empty fixture
    const auto config = InspectionConfig::defaultFor(size);
    assert(config.slots.size() == 25);
    FixtureInspector inspector(config);
    const auto empty_summary = inspector.inspect(empty, 0, 24.0);
    assert(!empty_summary.is_pass);
    assert(empty_summary.present_count == 0);
    assert(empty_summary.missing_count == 25);
    assert(empty_summary.missing_slot_ids.size() == 25);

    // 5. Test populated fixture (all 25 components present)
    cv::Mat populated = empty.clone();
    for (const auto &slot : config.slots)
    {
        populated(slot.bounds).setTo(cv::Scalar(200, 200, 200));
    }
    const auto populated_summary = inspector.inspect(populated, 1, 24.0);
    assert(populated_summary.is_pass);
    assert(populated_summary.present_count == 25);
    assert(populated_summary.missing_count == 0);
    assert(populated_summary.missing_slot_ids.empty());
    assert(std::abs(populated_summary.timestamp_seconds - (1.0 / 24.0)) < 1e-9);

    // 6. Test content change and reference comparison
    assert(!hasContentChanged(populated, populated));
    const auto same_reference = compareToReference(populated, populated);
    assert(same_reference.matches_reference);
    assert(same_reference.mean_absolute_difference < 1e-6);

    cv::Mat changed = populated.clone();
    changed(cv::Rect(0, 0, 200, 200)).setTo(cv::Scalar(0, 0, 0));
    assert(hasContentChanged(populated, changed));
    const auto different_reference = compareToReference(populated, changed);
    assert(!different_reference.matches_reference);
    assert(different_reference.changed_pixel_percent > 0.5);

    // 7. Test rendering functions
    const cv::Mat correct_annotated = renderDeviationAnnotations(populated, populated_summary, {}, 0, 24.0, true);
    assert(!correct_annotated.empty());
    assert(correct_annotated.size() == size);

    const cv::Mat deviated_annotated = renderDeviationAnnotations(empty, empty_summary, {cv::Rect(50, 50, 40, 40)}, 1, 24.0, false);
    assert(!deviated_annotated.empty());
    assert(deviated_annotated.size() == size);

    // 8. Test side-by-side view
    const cv::Mat sbs_view = createSideBySideView(correct_annotated, deviated_annotated, 1, false);
    assert(!sbs_view.empty());
    assert(sbs_view.cols == size.width * 2);
    assert(sbs_view.rows == size.height);

    // 9. Test FirstFrameComparator pipeline
    ComparatorConfig comp_config;
    comp_config.inspection_config = config;
    comp_config.diff_threshold = 1.5;
    comp_config.pixel_diff_threshold = 20.0;
    comp_config.min_changed_pixel_percent = 0.5;

    FirstFrameComparator comparator(comp_config);
    assert(!comparator.isBootstrapped());

    // Bootstrap with populated frame as frame 0 (Golden Reference)
    comparator.bootstrapGoldenReference(populated, 24.0);
    assert(comparator.isBootstrapped());
    assert(comparator.goldenSummary().is_pass);

    // Process frame 0 -> MUST be CORRECT with all slots matching reference
    auto res0 = comparator.processFrame(populated, 0, 24.0);
    assert(res0.is_correct);
    assert(res0.frame_index == 0);
    assert(res0.summary.total_slots == 25);
    assert(res0.record.status == "CORRECT");
    for (const auto &slot : res0.summary.slots)
    {
        assert(slot.matches_reference);
    }

    // Process identical frame 1 -> MUST be CORRECT
    auto res1 = comparator.processFrame(populated, 1, 24.0);
    assert(res1.is_correct);
    assert(res1.record.status == "CORRECT");

    // Process modified frame with 1 missing component -> MUST be DEVIATED
    cv::Mat missing_one = populated.clone();
    missing_one(config.slots[0].bounds).setTo(bgr_pixel.at<cv::Vec3b>(0, 0));
    auto res_missing = comparator.processFrame(missing_one, 2, 24.0);
    assert(!res_missing.is_correct);
    assert(res_missing.summary.deviated_slots_count >= 1);
    assert(res_missing.record.status == "DEVIATED");
    assert(!res_missing.summary.slots[0].matches_reference);
    assert(res_missing.summary.slots[0].deviation_type == "MISSING");

    // 10. Test CSV and JSON exporters
    const std::string tmp_csv = "/tmp/wisedo_test_deviations.csv";
    const std::string tmp_json = "/tmp/wisedo_test_deviations.json";
    const std::string tmp_subset = "/tmp/wisedo_test_subset.json";
    std::vector<FrameDeviationRecord> records = {res0.record, res1.record, res_missing.record};

    exportDeviationCsv(tmp_csv, records);
    assert(std::filesystem::exists(tmp_csv));
    std::ifstream csv_in(tmp_csv);
    std::string csv_header;
    std::getline(csv_in, csv_header);
    assert(csv_header.find("frame_index") != std::string::npos);
    assert(csv_header.find("is_correct") != std::string::npos);

    exportDeviationJson(tmp_json, "test_video.mp4", records, 3);
    assert(std::filesystem::exists(tmp_json));
    std::ifstream json_in(tmp_json);
    std::string json_content((std::istreambuf_iterator<char>(json_in)), std::istreambuf_iterator<char>());
    assert(json_content.find("video_source") != std::string::npos);
    assert(json_content.find("frame_0 (first frame)") != std::string::npos);
    assert(json_content.find("accuracy_pct") != std::string::npos);

    exportSubsetJson(tmp_subset, "test_video.mp4", {res0.record, res1.record}, "corrected", 3);
    assert(std::filesystem::exists(tmp_subset));
    std::ifstream subset_in(tmp_subset);
    std::string subset_content((std::istreambuf_iterator<char>(subset_in)), std::istreambuf_iterator<char>());
    assert(subset_content.find("status_filter") != std::string::npos);
    assert(subset_content.find("frame_ranges") != std::string::npos);

    // Cleanup temporary files
    std::filesystem::remove(tmp_csv);
    std::filesystem::remove(tmp_json);
    std::filesystem::remove(tmp_subset);

    std::cout << "All wisedo_vision_core tests passed successfully!\n";
    return 0;
}
