#include <wisedo/vision/vision.hpp>
#include <opencv2/imgproc.hpp>
#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cmath>
#include <iomanip>
#include <stdexcept>
#include <string>
#include <utility>
namespace wisedo::vision { namespace {
void requireBgrFrame(const cv::Mat &frame) { if (frame.empty() || frame.type() != CV_8UC3) throw std::invalid_argument("expected a non-empty 8-bit BGR frame"); }
}
    // ── Rendering ────────────────────────────────────────────────────────────

    cv::Mat renderDeviationAnnotations(
        const cv::Mat &frame,
        const FrameInspectionSummary &summary,
        const std::vector<cv::Rect> &extra_boxes,
        int frame_index,
        double fps,
        bool is_correct)
    {
        requireBgrFrame(frame);
        cv::Mat annotated = frame.clone();
        const int h = annotated.rows;
        const int w = annotated.cols;

        // 1. Draw expected reference slots
        for (const auto &slot : summary.slots)
        {
            const cv::Point tl(slot.bounds.x, slot.bounds.y);
            const cv::Point br(slot.bounds.x + slot.bounds.width, slot.bounds.y + slot.bounds.height);

            if (slot.matches_reference && slot.is_present)
            {
                const cv::Scalar green(36, 179, 36);
                cv::rectangle(annotated, tl, br, green, 2, cv::LINE_AA);

                const std::string label = "CORRECT: " + slot.slot_id;
                int baseline = 0;
                const auto text_size = cv::getTextSize(label, cv::FONT_HERSHEY_SIMPLEX, 0.35, 1, &baseline);
                int badge_y1 = std::max(0, tl.y - 14);
                int badge_y2 = tl.y;
                int text_y = (badge_y1 == 0) ? tl.y + 10 : tl.y - 3;
                if (badge_y1 == 0)
                {
                    badge_y1 = tl.y;
                    badge_y2 = tl.y + 14;
                }
                cv::rectangle(annotated, cv::Point(tl.x, badge_y1), cv::Point(tl.x + text_size.width + 4, badge_y2), cv::Scalar(20, 20, 20), cv::FILLED);
                cv::putText(annotated, label, cv::Point(tl.x + 2, text_y), cv::FONT_HERSHEY_SIMPLEX, 0.35, green, 1, cv::LINE_AA);
            }
            else
            {
                const cv::Scalar red(0, 0, 230);
                cv::rectangle(annotated, tl, br, red, 2, cv::LINE_AA);
                // Diagonal cross
                cv::line(annotated, tl, br, red, 2, cv::LINE_AA);
                cv::line(annotated, cv::Point(br.x, tl.y), cv::Point(tl.x, br.y), red, 2, cv::LINE_AA);

                const std::string tag = slot.deviation_type.empty() ? "MISSING" : slot.deviation_type;
                const std::string label = "DEVIATED: " + slot.slot_id + " (" + tag + ")";
                int baseline = 0;
                const auto text_size = cv::getTextSize(label, cv::FONT_HERSHEY_SIMPLEX, 0.35, 1, &baseline);
                int badge_y1 = std::max(0, tl.y - 14);
                int badge_y2 = tl.y;
                int text_y = (badge_y1 == 0) ? tl.y + 10 : tl.y - 3;
                if (badge_y1 == 0)
                {
                    badge_y1 = tl.y;
                    badge_y2 = tl.y + 14;
                }
                cv::rectangle(annotated, cv::Point(tl.x, badge_y1), cv::Point(tl.x + text_size.width + 4, badge_y2), cv::Scalar(0, 0, 180), cv::FILLED);
                cv::putText(annotated, label, cv::Point(tl.x + 2, text_y), cv::FONT_HERSHEY_SIMPLEX, 0.35, cv::Scalar(255, 255, 255), 1, cv::LINE_AA);
            }
        }

        // 2. Draw extra components
        for (std::size_t i = 0; i < extra_boxes.size(); ++i)
        {
            const auto &ex = extra_boxes[i];
            const cv::Point tl(ex.x, ex.y);
            const cv::Point br(ex.x + ex.width, ex.y + ex.height);
            const cv::Scalar amber(0, 140, 255);

            cv::rectangle(annotated, tl, br, amber, 2, cv::LINE_AA);
            const std::string label = "DEVIATED: EXTRA_" + std::to_string(i + 1);
            int baseline = 0;
            const auto text_size = cv::getTextSize(label, cv::FONT_HERSHEY_SIMPLEX, 0.35, 1, &baseline);
            int badge_y1 = std::max(0, tl.y - 14);
            int badge_y2 = tl.y;
            int text_y = (badge_y1 == 0) ? tl.y + 10 : tl.y - 3;
            if (badge_y1 == 0)
            {
                badge_y1 = tl.y;
                badge_y2 = tl.y + 14;
            }
            cv::rectangle(annotated, cv::Point(tl.x, badge_y1), cv::Point(tl.x + text_size.width + 4, badge_y2), cv::Scalar(0, 100, 200), cv::FILLED);
            cv::putText(annotated, label, cv::Point(tl.x + 2, text_y), cv::FONT_HERSHEY_SIMPLEX, 0.35, cv::Scalar(255, 255, 255), 1, cv::LINE_AA);
        }

        // 3. Top HUD banner
        const int hud_h = 44;
        const cv::Scalar hud_color = is_correct ? cv::Scalar(25, 140, 25) : cv::Scalar(30, 30, 200);
        cv::rectangle(annotated, cv::Point(0, 0), cv::Point(w, hud_h), hud_color, cv::FILLED);

        const double t_sec = fps > 0.0 ? frame_index / fps : 0.0;
        char hud_buf[256];
        if (is_correct)
        {
            std::snprintf(hud_buf, sizeof(hud_buf),
                          "[CORRECT] FRAME %03d (t=%.2fs) | All %zu/%zu Elements OK (0 Deviations)",
                          frame_index, t_sec, summary.total_slots, summary.total_slots);
        }
        else
        {
            const auto missing_count = summary.missing_count;
            const auto extra_count = extra_boxes.size();
            const auto total_dev = missing_count + extra_count;
            std::snprintf(hud_buf, sizeof(hud_buf),
                          "[DEVIATED] FRAME %03d (t=%.2fs) | %zu/%zu Correct | %zu Deviations (%zu Missing, %zu Extra)",
                          frame_index, t_sec, summary.present_count, summary.total_slots, total_dev, missing_count, extra_count);
        }
        cv::putText(annotated, hud_buf, cv::Point(16, 30), cv::FONT_HERSHEY_SIMPLEX, 0.65, cv::Scalar(255, 255, 255), 2, cv::LINE_AA);

        // 4. Bottom footer bar (deviated frames only)
        if (!is_correct)
        {
            const int foot_h = 28;
            cv::rectangle(annotated, cv::Point(0, h - foot_h), cv::Point(w, h), cv::Scalar(15, 15, 15), cv::FILLED);
            std::string foot_txt = "Deviated Elements: ";

            if (!summary.missing_slot_ids.empty())
            {
                std::string miss_str;
                const std::size_t preview_max = 6;
                for (std::size_t i = 0; i < std::min(preview_max, summary.missing_slot_ids.size()); ++i)
                {
                    if (!miss_str.empty())
                        miss_str += ", ";
                    miss_str += summary.missing_slot_ids[i];
                }
                if (summary.missing_slot_ids.size() > preview_max)
                {
                    miss_str += " (+" + std::to_string(summary.missing_slot_ids.size() - preview_max) + " more)";
                }
                foot_txt += "Missing: [" + miss_str + "]";
            }
            if (!extra_boxes.empty())
            {
                if (!summary.missing_slot_ids.empty())
                    foot_txt += " | ";
                foot_txt += "Extra Components: " + std::to_string(extra_boxes.size());
            }
            cv::putText(annotated, foot_txt, cv::Point(16, h - 8), cv::FONT_HERSHEY_SIMPLEX, 0.45, cv::Scalar(0, 200, 255), 1, cv::LINE_AA);
        }

        return annotated;
    }

    cv::Mat createSideBySideView(
        const cv::Mat &reference_annotated,
        const cv::Mat &frame_annotated,
        int frame_index,
        bool is_correct)
    {
        requireBgrFrame(reference_annotated);
        requireBgrFrame(frame_annotated);

        cv::Mat left = reference_annotated.clone();
        cv::Mat right = frame_annotated.clone();

        if (left.size() != right.size())
        {
            cv::resize(right, right, left.size());
        }

        // Column banner on reference (left)
        const int col_h = 32;
        cv::rectangle(left, cv::Point(0, 44), cv::Point(left.cols, 44 + col_h), cv::Scalar(40, 40, 40), cv::FILLED);
        cv::putText(left, "REFERENCE: FIRST FRAME (FRAME 0) - GOLDEN STATE",
                    cv::Point(16, 66), cv::FONT_HERSHEY_SIMPLEX, 0.55, cv::Scalar(255, 255, 255), 2, cv::LINE_AA);

        // Column banner on inspected frame (right)
        const cv::Scalar col_color = is_correct ? cv::Scalar(20, 120, 20) : cv::Scalar(20, 20, 160);
        const std::string status_label = is_correct ? "CORRECT (MATCHES REFERENCE)" : "DEVIATED FROM REFERENCE";
        cv::rectangle(right, cv::Point(0, 44), cv::Point(right.cols, 44 + col_h), col_color, cv::FILLED);

        char label_buf[128];
        std::snprintf(label_buf, sizeof(label_buf), "INSPECTED: FRAME %03d - %s", frame_index, status_label.c_str());
        cv::putText(right, label_buf, cv::Point(16, 66), cv::FONT_HERSHEY_SIMPLEX, 0.55, cv::Scalar(255, 255, 255), 2, cv::LINE_AA);

        cv::Mat combined;
        cv::hconcat(left, right, combined);
        return combined;
    }

    // ── First-Frame Golden Reference Comparator ─────────────────────────────

    FirstFrameComparator::FirstFrameComparator(ComparatorConfig config)
        : config_(std::move(config)), inspector_(config_.inspection_config)
    {
    }

    bool FirstFrameComparator::bootstrapGoldenReference(const cv::Mat &first_frame, double fps)
    {
        requireBgrFrame(first_frame);
        golden_reference_ = first_frame.clone();
        board_bbox_ = getDynamicBoardBbox(golden_reference_);

        // If no slots configured, auto-discover components from first frame and calibrate board HSV
        if (config_.inspection_config.slots.empty())
        {
            DetectionParams params = config_.inspection_config.detection;
            auto discovered = autoDiscoverSlots(golden_reference_, &params);
            if (discovered.empty())
            {
                config_.inspection_config = InspectionConfig::defaultFor(golden_reference_.size());
            }
            else
            {
                config_.inspection_config.slots = std::move(discovered);
                config_.inspection_config.detection = params;
            }
            inspector_.setConfig(config_.inspection_config);
        }

        ref_boxes_.clear();
        for (const auto &slot : config_.inspection_config.slots)
        {
            ref_boxes_.push_back(slot.bounds);
        }

        golden_summary_ = inspector_.inspect(golden_reference_, 0, fps);
        for (auto &slot : golden_summary_.slots)
        {
            slot.matches_reference = true;
            slot.deviation_type = "";
        }
        golden_summary_.is_pass = true;
        golden_summary_.missing_count = 0;
        golden_summary_.deviated_slots_count = 0;
        golden_summary_.missing_slot_ids.clear();
        golden_summary_.deviated_slot_ids.clear();

        golden_annotated_ = renderDeviationAnnotations(golden_reference_, golden_summary_, {}, 0, fps, true);
        bootstrapped_ = true;
        return true;
    }

    ProcessedFrameResult FirstFrameComparator::processFrame(const cv::Mat &current_frame, int frame_index, double fps)
    {
        requireBgrFrame(current_frame);
        if (!bootstrapped_)
        {
            bootstrapGoldenReference(current_frame, fps);
        }

        ProcessedFrameResult result;
        result.frame_index = frame_index;
        result.timestamp_sec = fps > 0.0 ? frame_index / fps : 0.0;

        if (frame_index == 0)
        {
            result.comparison = FrameComparison{true, 0.0, 0.0};
            result.summary = golden_summary_;
            result.extra_boxes = {};
            result.is_correct = true;
            result.annotated_frame = golden_annotated_.clone();
        }
        else
        {
            result.comparison = compareToReference(
                golden_reference_,
                current_frame,
                config_.diff_threshold,
                config_.pixel_diff_threshold,
                config_.min_changed_pixel_percent);

            result.summary = inspector_.inspect(current_frame, frame_index, fps);

            // Detect any unexpected extra components
            const auto raw_boxes = detectWhiteBoxes(current_frame);
            const auto frame_boxes = mergeBoxesNms(raw_boxes, 0.30);
            for (const auto &fb : frame_boxes)
            {
                double max_iou = 0.0;
                for (const auto &rb : ref_boxes_)
                {
                    max_iou = std::max(max_iou, boxIoU(fb, rb));
                }
                if (max_iou < 0.30)
                {
                    result.extra_boxes.push_back(fb);
                }
            }

            const auto missing_count = result.summary.missing_count;
            const auto extra_count = result.extra_boxes.size();
            const auto deviated_count = missing_count + extra_count;

            result.is_correct = (deviated_count == 0);
            result.summary.is_pass = result.is_correct;
            result.summary.deviated_slots_count = deviated_count;

            result.annotated_frame = renderDeviationAnnotations(
                current_frame,
                result.summary,
                result.extra_boxes,
                frame_index,
                fps,
                result.is_correct);
        }

        result.side_by_side_frame = createSideBySideView(
            golden_annotated_,
            result.annotated_frame,
            frame_index,
            result.is_correct);

        result.record.frame_index = frame_index;
        result.record.frame_range = std::to_string(frame_index);
        result.record.repeat_count = 1;
        result.record.timestamp_sec = result.timestamp_sec;
        result.record.duration_sec = fps > 0.0 ? 1.0 / fps : 0.0;
        result.record.status = result.is_correct ? "CORRECT" : "DEVIATED";
        result.record.is_correct = result.is_correct;
        result.record.total_elements = result.summary.total_slots;
        result.record.correct_count = result.summary.present_count;
        result.record.deviated_count = result.summary.missing_count + result.extra_boxes.size();
        result.record.missing_slots = result.summary.missing_slot_ids;
        result.record.extra_slots_count = result.extra_boxes.size();
        result.record.mean_diff = result.comparison.mean_absolute_difference;
        result.record.changed_pct = result.comparison.changed_pixel_percent;

        return result;
    }

} // namespace wisedo::vision
