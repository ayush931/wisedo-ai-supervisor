#include <wisedo/vision/vision.hpp>
#include <opencv2/imgproc.hpp>
#include <algorithm>
#include <chrono>
#include <cmath>
#include <limits>
#include <stdexcept>
#include <utility>
namespace wisedo::vision { namespace {
void requireBgrFrame(const cv::Mat &frame) { if (frame.empty() || frame.type() != CV_8UC3) throw std::invalid_argument("expected a non-empty 8-bit BGR frame"); }
cv::Rect boundedRect(const cv::Rect &candidate, cv::Size frame_size) { const int x1 = std::clamp(candidate.x, 0, std::max(0, frame_size.width - 1)); const int y1 = std::clamp(candidate.y, 0, std::max(0, frame_size.height - 1)); const int x2 = std::clamp(candidate.x + candidate.width, 0, std::max(0, frame_size.width)); const int y2 = std::clamp(candidate.y + candidate.height, 0, std::max(0, frame_size.height)); return cv::Rect(x1, y1, std::max(1, x2 - x1), std::max(1, y2 - y1)); }
}
    // ── Fixture Inspector ───────────────────────────────────────────────────

    FixtureInspector::FixtureInspector(InspectionConfig config) : config_(std::move(config))
    {
        if (config_.slots.empty() ||
            config_.detection.missing_threshold <= 0.0 ||
            config_.detection.missing_threshold >= 1.0 ||
            config_.detection.margin_ratio < 0.0 ||
            config_.detection.margin_ratio >= 0.5)
        {
            throw std::invalid_argument("invalid inspection configuration");
        }
    }

    FrameInspectionSummary FixtureInspector::inspect(const cv::Mat &frame, int frame_index, double fps) const
    {
        requireBgrFrame(frame);
        const auto t_start = std::chrono::steady_clock::now();

        cv::Mat hsv;
        cv::cvtColor(frame, hsv, cv::COLOR_BGR2HSV);

        FrameInspectionSummary summary;
        summary.frame_index = frame_index;
        summary.timestamp_seconds = fps > 0.0 ? frame_index / fps : 0.0;
        summary.total_slots = config_.slots.size();
        summary.slots.reserve(config_.slots.size());

        for (const auto &slot : config_.slots)
        {
            const cv::Rect bounds = boundedRect(slot.bounds, frame.size());
            const int margin_x = std::max(2, static_cast<int>(bounds.width * config_.detection.margin_ratio));
            const int margin_y = std::max(2, static_cast<int>(bounds.height * config_.detection.margin_ratio));
            cv::Rect roi(bounds.x + margin_x, bounds.y + margin_y, bounds.width - 2 * margin_x, bounds.height - 2 * margin_y);
            if (roi.width <= 0 || roi.height <= 0)
            {
                roi = bounds;
            }

            const cv::Mat hsv_roi = hsv(roi);
            cv::Mat board_mask;
            cv::inRange(hsv_roi, config_.detection.board_hsv_lower, config_.detection.board_hsv_upper, board_mask);
            const double board_ratio = static_cast<double>(cv::countNonZero(board_mask)) / static_cast<double>(hsv_roi.total());
            const bool present = board_ratio < config_.detection.missing_threshold;

            if (present)
            {
                summary.present_count += 1;
            }
            else
            {
                summary.missing_count += 1;
                summary.missing_slot_ids.push_back(slot.id);
            }

            const double conf = std::min(1.0, 0.5 + std::abs(board_ratio - config_.detection.missing_threshold));
            summary.slots.push_back({
                slot.id,
                slot.type,
                bounds,
                present ? "PRESENT" : "MISSING",
                present,
                board_ratio,
                conf,
                present, // matches_reference initialized to presence
                present ? "" : "MISSING",
            });
        }

        summary.is_pass = (summary.missing_count == 0);
        summary.deviated_slots_count = summary.missing_count;
        summary.deviated_slot_ids = summary.missing_slot_ids;

        const auto t_end = std::chrono::steady_clock::now();
        summary.processing_time_ms = std::chrono::duration<double, std::milli>(t_end - t_start).count();

        return summary;
    }

    // ── Content Change & Reference Comparison ──────────────────────────────

    FrameComparison compareToReference(
        const cv::Mat &reference,
        const cv::Mat &current,
        double mean_diff_threshold,
        double pixel_diff_threshold,
        double min_changed_pixel_percent)
    {
        requireBgrFrame(reference);
        requireBgrFrame(current);
        if (reference.size() != current.size())
        {
            return {false, std::numeric_limits<double>::infinity(), 100.0};
        }
        cv::Mat diff;
        cv::absdiff(reference, current, diff);
        const cv::Scalar channel_means = cv::mean(diff);
        const double mean_difference = (channel_means[0] + channel_means[1] + channel_means[2]) / 3.0;

        cv::Mat changed;
        cv::compare(diff, cv::Scalar(pixel_diff_threshold, pixel_diff_threshold, pixel_diff_threshold), changed, cv::CMP_GT);
        const cv::Mat changed_flat = changed.reshape(1);
        const double changed_percent = 100.0 * cv::countNonZero(changed_flat) / static_cast<double>(diff.total() * diff.channels());

        return {
            mean_difference < mean_diff_threshold && changed_percent < min_changed_pixel_percent,
            mean_difference,
            changed_percent,
        };
    }

    bool hasContentChanged(
        const cv::Mat &previous,
        const cv::Mat &current,
        const std::optional<cv::Rect> &board,
        double mean_diff_threshold,
        double pixel_diff_threshold,
        double min_changed_pixel_percent)
    {
        requireBgrFrame(previous);
        requireBgrFrame(current);
        if (previous.size() != current.size())
        {
            return true;
        }

        const cv::Rect frame_rect(0, 0, current.cols, current.rows);
        const cv::Rect area = board.value_or(frame_rect) & frame_rect;
        if (area.empty())
        {
            return true;
        }

        const cv::Mat p_crop = previous(area);
        const cv::Mat c_crop = current(area);

        cv::Mat diff;
        cv::absdiff(p_crop, c_crop, diff);
        const cv::Scalar m = cv::mean(diff);
        const double mean_val = (m[0] + m[1] + m[2]) / 3.0;
        if (mean_val >= mean_diff_threshold)
        {
            return true;
        }

        cv::Mat changed;
        cv::compare(diff, cv::Scalar(pixel_diff_threshold, pixel_diff_threshold, pixel_diff_threshold), changed, cv::CMP_GT);
        const cv::Mat changed_flat = changed.reshape(1);
        const double pct = (100.0 * cv::countNonZero(changed_flat)) / static_cast<double>(diff.total() * diff.channels());
        return (pct >= min_changed_pixel_percent);
    }

} // namespace wisedo::vision
