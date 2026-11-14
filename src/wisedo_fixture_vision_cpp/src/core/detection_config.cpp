#include <wisedo/vision/vision.hpp>

#include <opencv2/imgproc.hpp>

#include <algorithm>
#include <cmath>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <sstream>
#include <stdexcept>
#include <utility>

namespace wisedo::vision
{
    namespace
    {

        void requireBgrFrame(const cv::Mat &frame)
        {
            if (frame.empty() || frame.type() != CV_8UC3)
            {
                throw std::invalid_argument("expected a non-empty 8-bit BGR frame");
            }
        }

        double channelMedian(std::vector<uchar> &values)
        {
            if (values.empty())
                return 0.0;
            const std::size_t n = values.size();
            std::nth_element(values.begin(), values.begin() + n / 2, values.end());
            return values[n / 2];
        }

    } // namespace

    // ── Board localization & component detection helpers ───────────────────

    cv::Scalar getCenterMedianHsv(const cv::Mat &frame)
    {
        requireBgrFrame(frame);
        const int h = frame.rows;
        const int w = frame.cols;

        const int y1 = static_cast<int>(h * 0.20);
        const int y2 = static_cast<int>(h * 0.80);
        const int x1 = static_cast<int>(w * 0.20);
        const int x2 = static_cast<int>(w * 0.80);

        const cv::Rect center_roi(x1, y1, std::max(1, x2 - x1), std::max(1, y2 - y1));
        const cv::Mat center_bgr = frame(center_roi);

        cv::Mat center_hsv;
        cv::cvtColor(center_bgr, center_hsv, cv::COLOR_BGR2HSV);

        if (center_hsv.empty() || center_hsv.total() == 0)
        {
            return cv::Scalar(18, 108, 157);
        }

        std::vector<cv::Mat> channels;
        cv::split(center_hsv, channels);

        std::vector<uchar> h_vals, s_vals, v_vals;
        const std::size_t total = center_hsv.total();
        h_vals.assign(channels[0].datastart, channels[0].dataend);
        s_vals.assign(channels[1].datastart, channels[1].dataend);
        v_vals.assign(channels[2].datastart, channels[2].dataend);

        (void)total;
        const double med_h = channelMedian(h_vals);
        const double med_s = channelMedian(s_vals);
        const double med_v = channelMedian(v_vals);

        return cv::Scalar(med_h, med_s, med_v);
    }

    std::pair<cv::Scalar, cv::Scalar> boardThresholdsFromMedian(const cv::Scalar &median)
    {
        const double med_h = median[0];
        const double med_s = median[1];
        const double med_v = median[2];

        const cv::Scalar lower(
            std::max(0.0, med_h - 15.0),
            std::max(30.0, med_s - 50.0),
            std::max(40.0, med_v - 70.0));

        const cv::Scalar upper(
            std::min(180.0, med_h + 15.0),
            std::min(255.0, med_s + 60.0),
            std::min(255.0, med_v + 60.0));

        return {lower, upper};
    }

    cv::Rect findBoardBbox(
        const cv::Mat &hsv,
        const cv::Scalar &lower,
        const cv::Scalar &upper,
        const cv::Rect &fallback)
    {
        cv::Mat board_mask;
        cv::inRange(hsv, lower, upper, board_mask);

        std::vector<std::vector<cv::Point>> contours;
        cv::findContours(board_mask, contours, cv::RETR_EXTERNAL, cv::CHAIN_APPROX_SIMPLE);

        if (!contours.empty())
        {
            auto largest = std::max_element(contours.begin(), contours.end(),
                                            [](const std::vector<cv::Point> &a, const std::vector<cv::Point> &b)
                                            {
                                                return cv::contourArea(a) < cv::contourArea(b);
                                            });
            const double area = cv::contourArea(*largest);
            const double total_img_area = static_cast<double>(hsv.cols * hsv.rows);
            if (area > (total_img_area * 0.15))
            {
                return cv::boundingRect(*largest);
            }
        }
        return fallback;
    }

    cv::Rect getDynamicBoardBbox(const cv::Mat &frame, double margin_pct)
    {
        if (!frame.empty() && frame.type() == CV_8UC3)
        {
            const int w = frame.cols;
            const int h = frame.rows;
            const cv::Scalar med = getCenterMedianHsv(frame);
            const auto [lower, upper] = boardThresholdsFromMedian(med);

            cv::Mat hsv;
            cv::cvtColor(frame, hsv, cv::COLOR_BGR2HSV);

            const cv::Rect fallback(
                static_cast<int>(w * margin_pct),
                static_cast<int>(h * margin_pct),
                static_cast<int>(w * (1.0 - 2.0 * margin_pct)),
                static_cast<int>(h * (1.0 - 2.0 * margin_pct)));

            return findBoardBbox(hsv, lower, upper, fallback);
        }

        const int w = 1280;
        const int h = 720;
        return cv::Rect(
            static_cast<int>(w * margin_pct),
            static_cast<int>(h * margin_pct),
            static_cast<int>(w * (1.0 - 2.0 * margin_pct)),
            static_cast<int>(h * (1.0 - 2.0 * margin_pct)));
    }

    double boxIoU(const cv::Rect &a, const cv::Rect &b)
    {
        const int x1 = std::max(a.x, b.x);
        const int y1 = std::max(a.y, b.y);
        const int x2 = std::min(a.x + a.width, b.x + b.width);
        const int y2 = std::min(a.y + a.height, b.y + b.height);

        const int iw = std::max(0, x2 - x1);
        const int ih = std::max(0, y2 - y1);
        const double inter = static_cast<double>(iw * ih);
        if (inter <= 0.0)
            return 0.0;

        const double area_a = std::max(1, a.area());
        const double area_b = std::max(1, b.area());
        return inter / (area_a + area_b - inter);
    }

    std::vector<cv::Rect> mergeBoxesNms(const std::vector<cv::Rect> &boxes, double iou_thresh)
    {
        if (boxes.empty())
            return {};

        std::vector<cv::Rect> remaining = boxes;
        std::sort(remaining.begin(), remaining.end(), [](const cv::Rect &a, const cv::Rect &b)
                  { return a.area() > b.area(); });

        std::vector<cv::Rect> merged;
        for (const auto &box : remaining)
        {
            bool matched = false;
            for (auto &kept : merged)
            {
                if (boxIoU(box, kept) >= iou_thresh)
                {
                    const int x1 = std::min(kept.x, box.x);
                    const int y1 = std::min(kept.y, box.y);
                    const int x2 = std::max(kept.x + kept.width, box.x + box.width);
                    const int y2 = std::max(kept.y + kept.height, box.y + box.height);
                    kept = cv::Rect(x1, y1, x2 - x1, y2 - y1);
                    matched = true;
                    break;
                }
            }
            if (!matched)
            {
                merged.push_back(box);
            }
        }

        // Spatial sort: row cluster (y // 50), then x
        std::sort(merged.begin(), merged.end(), [](const cv::Rect &a, const cv::Rect &b)
                  {
            const int row_a = a.y / 50;
            const int row_b = b.y / 50;
            if (row_a != row_b)
                return row_a < row_b;
            return a.x < b.x; });

        return merged;
    }

    std::vector<cv::Rect> detectWhiteBoxes(const cv::Mat &frame, cv::Scalar *out_median)
    {
        requireBgrFrame(frame);
        const int h = frame.rows;
        const int w = frame.cols;

        cv::Mat hsv;
        cv::cvtColor(frame, hsv, cv::COLOR_BGR2HSV);

        const cv::Scalar med = getCenterMedianHsv(frame);
        if (out_median)
        {
            *out_median = med;
        }

        const auto [lower_board, upper_board] = boardThresholdsFromMedian(med);
        const cv::Rect fallback(
            static_cast<int>(w * 0.15),
            static_cast<int>(h * 0.10),
            static_cast<int>(w * 0.70),
            static_cast<int>(h * 0.80));

        const cv::Rect board = findBoardBbox(hsv, lower_board, upper_board, fallback);

        // White/gray mask: S in [0, 60], V in [90, 255]
        cv::Mat white_mask;
        cv::inRange(hsv, cv::Scalar(0, 0, 90), cv::Scalar(180, 60, 255), white_mask);

        // Active board ROI mask with 3% inset margin
        cv::Mat roi_mask = cv::Mat::zeros(white_mask.size(), CV_8UC1);
        const int mx = std::max(4, static_cast<int>(board.width * 0.03));
        const int my = std::max(4, static_cast<int>(board.height * 0.03));
        const cv::Rect inner_board(
            board.x + mx,
            board.y + my,
            std::max(1, board.width - 2 * mx),
            std::max(1, board.height - 2 * my));

        const cv::Rect safe_inner = inner_board & cv::Rect(0, 0, w, h);
        if (!safe_inner.empty())
        {
            roi_mask(safe_inner).setTo(255);
        }

        cv::Mat active;
        cv::bitwise_and(white_mask, roi_mask, active);

        cv::Mat kernel = cv::getStructuringElement(cv::MORPH_RECT, cv::Size(3, 3));
        cv::morphologyEx(active, active, cv::MORPH_OPEN, kernel);

        std::vector<std::vector<cv::Point>> contours;
        cv::findContours(active, contours, cv::RETR_EXTERNAL, cv::CHAIN_APPROX_SIMPLE);

        const double min_area = 400.0;
        const double max_area = static_cast<double>(board.width * board.height) * 0.08;
        const double max_w = board.width * 0.45;
        const double max_h = board.height * 0.45;

        std::vector<cv::Rect> boxes;
        for (const auto &cnt : contours)
        {
            const cv::Rect bbox = cv::boundingRect(cnt);
            const double area = static_cast<double>(bbox.area());
            if (area < min_area || area > max_area)
                continue;
            const double aspect = static_cast<double>(bbox.width) / std::max(1, bbox.height);
            if (aspect < 0.20 || aspect > 5.0)
                continue;
            if (bbox.width > max_w || bbox.height > max_h)
                continue;
            boxes.push_back(bbox);
        }

        return boxes;
    }

    std::vector<SlotDefinition> autoDiscoverSlots(const cv::Mat &frame, DetectionParams *out_params)
    {
        cv::Scalar med;
        const auto raw_boxes = detectWhiteBoxes(frame, &med);
        const auto merged = mergeBoxesNms(raw_boxes, 0.30);

        if (out_params)
        {
            const auto [lower, upper] = boardThresholdsFromMedian(med);
            out_params->board_hsv_lower = lower;
            out_params->board_hsv_upper = upper;
        }

        std::vector<SlotDefinition> slots;
        slots.reserve(merged.size());
        for (std::size_t i = 0; i < merged.size(); ++i)
        {
            char buf[32];
            std::snprintf(buf, sizeof(buf), "SLOT_%02zu", i + 1);
            slots.push_back({
                buf,
                "component",
                merged[i],
                "Auto-detected component " + std::to_string(i + 1),
            });
        }
        return slots;
    }

    // ── Inspection Configuration ────────────────────────────────────────────

    InspectionConfig InspectionConfig::defaultFor(cv::Size frame_size, std::size_t slot_count)
    {
        if (frame_size.width < 2 || frame_size.height < 2 || slot_count == 0)
        {
            throw std::invalid_argument("frame size and slot count must be positive");
        }

        InspectionConfig config;
        const int board_x = static_cast<int>(frame_size.width * 0.15);
        const int board_y = static_cast<int>(frame_size.height * 0.10);
        const int board_width = static_cast<int>(frame_size.width * 0.70);
        const int board_height = static_cast<int>(frame_size.height * 0.80);
        const int rows = 5;
        const int columns = 5;
        const int x_min = board_x + 6;
        const int x_max = board_x + board_width - 6;
        const int y_min = board_y + 6;
        const int y_max = board_y + board_height - 6;
        const int step_x = std::max(10, (x_max - x_min) / columns);
        const int step_y = std::max(10, (y_max - y_min) / rows);

        for (int row = 0; row < rows && config.slots.size() < slot_count; ++row)
        {
            for (int column = 0; column < columns && config.slots.size() < slot_count; ++column)
            {
                const int x1 = x_min + column * step_x + 4;
                const int y1 = y_min + row * step_y + 4;
                const int x2 = std::min(frame_size.width - 2, x1 + step_x - 8);
                const int y2 = std::min(frame_size.height - 2, y1 + step_y - 8);
                const std::string slot_id =
                    std::string("SLOT_") + (config.slots.size() < 9 ? "0" : "") +
                    std::to_string(config.slots.size() + 1);
                config.slots.push_back({
                    slot_id,
                    "grid_roi",
                    cv::Rect(x1, y1, std::max(2, x2 - x1), std::max(2, y2 - y1)),
                    "Grid slot " + slot_id,
                });
            }
        }
        return config;
    }

    InspectionConfig InspectionConfig::load(const std::string &path)
    {
        cv::FileStorage storage(path, cv::FileStorage::READ | cv::FileStorage::FORMAT_JSON);
        if (!storage.isOpened())
        {
            throw std::runtime_error("could not open inspection config: " + path);
        }

        InspectionConfig config;
        const auto params = storage["detection_params"];
        if (!params.empty())
        {
            std::vector<int> lower, upper;
            params["board_hsv_lower"] >> lower;
            params["board_hsv_upper"] >> upper;
            if (lower.size() == 3)
            {
                config.detection.board_hsv_lower = cv::Scalar(lower[0], lower[1], lower[2]);
            }
            if (upper.size() == 3)
            {
                config.detection.board_hsv_upper = cv::Scalar(upper[0], upper[1], upper[2]);
            }
            if (!params["missing_threshold"].empty())
            {
                params["missing_threshold"] >> config.detection.missing_threshold;
            }
            if (!params["margin_ratio"].empty())
            {
                params["margin_ratio"] >> config.detection.margin_ratio;
            }
        }

        const auto slots = storage["slots"];
        for (const auto &node : slots)
        {
            std::string id = "", type = "component";
            std::vector<int> bbox;
            node["slot_id"] >> id;
            node["type"] >> type;
            node["bbox"] >> bbox;
            if (id.empty() || bbox.size() != 4)
            {
                continue;
            }
            config.slots.push_back({id, type, cv::Rect(bbox[0], bbox[1], bbox[2] - bbox[0], bbox[3] - bbox[1]), id});
        }
        if (config.slots.empty())
        {
            throw std::runtime_error("inspection config contains no valid slots: " + path);
        }
        return config;
    }

} // namespace wisedo::vision
