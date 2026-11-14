#include <wisedo/vision/vision.hpp>
#include <opencv2/videoio.hpp>
#include <algorithm>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <stdexcept>
#include <string>
#include <vector>
namespace wisedo::vision { namespace {
void requireBgrFrame(const cv::Mat &frame) { if (frame.empty() || frame.type() != CV_8UC3) throw std::invalid_argument("expected a non-empty 8-bit BGR frame"); }
}
    // ── Exporters ───────────────────────────────────────────────────────────

    std::vector<std::string> computeFrameRanges(const std::vector<int> &indices)
    {
        if (indices.empty())
            return {};

        std::vector<std::string> ranges;
        int start = indices[0];
        int prev = start;

        for (std::size_t i = 1; i < indices.size(); ++i)
        {
            const int idx = indices[i];
            if (idx == prev + 1)
            {
                prev = idx;
            }
            else
            {
                ranges.push_back(start != prev ? (std::to_string(start) + "-" + std::to_string(prev)) : std::to_string(start));
                start = idx;
                prev = idx;
            }
        }
        ranges.push_back(start != prev ? (std::to_string(start) + "-" + std::to_string(prev)) : std::to_string(start));
        return ranges;
    }

    void exportDeviationCsv(const std::string &path, const std::vector<FrameDeviationRecord> &records)
    {
        const auto parent = std::filesystem::path(path).parent_path();
        if (!parent.empty())
        {
            std::filesystem::create_directories(parent);
        }

        std::ofstream file(path);
        if (!file.is_open())
        {
            throw std::runtime_error("could not open CSV output path: " + path);
        }

        file << "frame_index,frame_range,repeat_count,timestamp_sec,duration_sec,status,is_correct,"
             << "total_elements,correct_count,deviated_count,missing_slots_count,missing_slots,"
             << "extra_slots_count,image_path\n";

        for (const auto &r : records)
        {
            file << r.frame_index << ','
                 << (r.frame_range.empty() ? std::to_string(r.frame_index) : r.frame_range) << ','
                 << r.repeat_count << ','
                 << std::fixed << std::setprecision(3) << r.timestamp_sec << ','
                 << r.duration_sec << ','
                 << r.status << ','
                 << (r.is_correct ? "True" : "False") << ','
                 << r.total_elements << ','
                 << r.correct_count << ','
                 << r.deviated_count << ','
                 << r.missing_slots.size() << ',';

            if (r.missing_slots.empty())
            {
                file << "None";
            }
            else
            {
                file << "\"";
                for (std::size_t i = 0; i < r.missing_slots.size(); ++i)
                {
                    if (i > 0)
                        file << "; ";
                    file << r.missing_slots[i];
                }
                file << "\"";
            }

            file << ',' << r.extra_slots_count << ','
                 << r.image_path << '\n';
        }
    }

    void exportDeviationJson(
        const std::string &path,
        const std::string &video_source,
        const std::vector<FrameDeviationRecord> &records,
        int total_frames,
        bool deduplicated)
    {
        const auto parent = std::filesystem::path(path).parent_path();
        if (!parent.empty())
        {
            std::filesystem::create_directories(parent);
        }

        std::ofstream file(path);
        if (!file.is_open())
        {
            throw std::runtime_error("could not open JSON output path: " + path);
        }

        int correct_count = 0;
        for (const auto &r : records)
        {
            if (r.is_correct)
                ++correct_count;
        }
        const int unique_count = static_cast<int>(records.size());
        const int deviated_count = unique_count - correct_count;
        const double accuracy_pct = unique_count > 0 ? (100.0 * correct_count / unique_count) : 0.0;

        file << "{\n"
             << "  \"video_source\": \"" << video_source << "\",\n"
             << "  \"reference_source\": \"frame_0 (first frame)\",\n"
             << "  \"total_video_frames\": " << (total_frames > 0 ? total_frames : unique_count) << ",\n"
             << "  \"unique_reported_frames\": " << unique_count << ",\n"
             << "  \"deduplicated\": " << (deduplicated ? "true" : "false") << ",\n"
             << "  \"correct_frames\": " << correct_count << ",\n"
             << "  \"deviated_frames\": " << deviated_count << ",\n"
             << "  \"accuracy_pct\": " << std::fixed << std::setprecision(2) << accuracy_pct << ",\n"
             << "  \"frames\": [\n";

        for (std::size_t i = 0; i < records.size(); ++i)
        {
            const auto &r = records[i];
            file << "    {\n"
                 << "      \"frame_index\": " << r.frame_index << ",\n"
                 << "      \"frame_range\": \"" << (r.frame_range.empty() ? std::to_string(r.frame_index) : r.frame_range) << "\",\n"
                 << "      \"repeat_count\": " << r.repeat_count << ",\n"
                 << "      \"timestamp_sec\": " << std::fixed << std::setprecision(3) << r.timestamp_sec << ",\n"
                 << "      \"duration_sec\": " << r.duration_sec << ",\n"
                 << "      \"status\": \"" << r.status << "\",\n"
                 << "      \"is_correct\": " << (r.is_correct ? "true" : "false") << ",\n"
                 << "      \"total_elements\": " << r.total_elements << ",\n"
                 << "      \"correct_count\": " << r.correct_count << ",\n"
                 << "      \"deviated_count\": " << r.deviated_count << ",\n"
                 << "      \"missing_slots\": [";

            for (std::size_t s = 0; s < r.missing_slots.size(); ++s)
            {
                if (s > 0)
                    file << ", ";
                file << "\"" << r.missing_slots[s] << "\"";
            }
            file << "],\n"
                 << "      \"extra_slots_count\": " << r.extra_slots_count << ",\n"
                 << "      \"image_path\": \"" << r.image_path << "\"\n"
                 << "    }" << (i + 1 < records.size() ? "," : "") << "\n";
        }

        file << "  ]\n}\n";
    }

    void exportSubsetJson(
        const std::string &path,
        const std::string &video_source,
        const std::vector<FrameDeviationRecord> &records,
        const std::string &status_label,
        int total_frames)
    {
        const auto parent = std::filesystem::path(path).parent_path();
        if (!parent.empty())
        {
            std::filesystem::create_directories(parent);
        }

        std::ofstream file(path);
        if (!file.is_open())
        {
            throw std::runtime_error("could not open JSON output path: " + path);
        }

        std::vector<int> indices;
        indices.reserve(records.size());
        for (const auto &r : records)
        {
            indices.push_back(r.frame_index);
        }

        const auto ranges = computeFrameRanges(indices);

        file << "{\n"
             << "  \"video_source\": \"" << video_source << "\",\n"
             << "  \"reference_source\": \"frame_0 (first frame)\",\n"
             << "  \"status_filter\": \"" << status_label << "\",\n"
             << "  \"total_video_frames\": " << (total_frames > 0 ? total_frames : static_cast<int>(records.size())) << ",\n"
             << "  \"total_" << status_label << "_frames\": " << records.size() << ",\n"
             << "  \"frame_ranges\": [\n";

        for (std::size_t i = 0; i < ranges.size(); ++i)
        {
            file << "    \"" << ranges[i] << "\"" << (i + 1 < ranges.size() ? "," : "") << "\n";
        }
        file << "  ],\n"
             << "  \"frame_indices\": [\n";

        for (std::size_t i = 0; i < indices.size(); ++i)
        {
            file << "    " << indices[i] << (i + 1 < indices.size() ? "," : "") << "\n";
        }
        file << "  ],\n"
             << "  \"frames\": [\n";

        for (std::size_t i = 0; i < records.size(); ++i)
        {
            const auto &r = records[i];
            file << "    {\n"
                 << "      \"frame_index\": " << r.frame_index << ",\n"
                 << "      \"frame_range\": \"" << (r.frame_range.empty() ? std::to_string(r.frame_index) : r.frame_range) << "\",\n"
                 << "      \"repeat_count\": " << r.repeat_count << ",\n"
                 << "      \"timestamp_sec\": " << std::fixed << std::setprecision(3) << r.timestamp_sec << ",\n"
                 << "      \"duration_sec\": " << r.duration_sec << ",\n"
                 << "      \"status\": \"" << r.status << "\",\n"
                 << "      \"is_correct\": " << (r.is_correct ? "true" : "false") << ",\n"
                 << "      \"total_elements\": " << r.total_elements << ",\n"
                 << "      \"correct_count\": " << r.correct_count << ",\n"
                 << "      \"deviated_count\": " << r.deviated_count << ",\n"
                 << "      \"missing_slots\": [";

            for (std::size_t s = 0; s < r.missing_slots.size(); ++s)
            {
                if (s > 0)
                    file << ", ";
                file << "\"" << r.missing_slots[s] << "\"";
            }
            file << "],\n"
                 << "      \"extra_slots_count\": " << r.extra_slots_count << ",\n"
                 << "      \"image_path\": \"" << r.image_path << "\"\n"
                 << "    }" << (i + 1 < records.size() ? "," : "") << "\n";
        }

        file << "  ]\n}\n";
    }

    // ── Video Writer ─────────────────────────────────────────────────────────

    PortableVideoWriter PortableVideoWriter::open(
        const std::string &output_path,
        double fps,
        cv::Size frame_size,
        std::string codec)
    {
        if (fps <= 0.0 || frame_size.width < 2 || frame_size.height < 2 || codec.size() != 4)
        {
            throw std::invalid_argument("invalid video output settings");
        }
        const auto parent = std::filesystem::path(output_path).parent_path();
        if (!parent.empty())
        {
            std::filesystem::create_directories(parent);
        }
        PortableVideoWriter result;
        result.writer_.open(output_path, cv::VideoWriter::fourcc(codec[0], codec[1], codec[2], codec[3]), fps, frame_size, true);
        if (!result.writer_.isOpened())
        {
            throw std::runtime_error("OpenCV could not open video writer with codec " + codec + "; install an ARM64/x86 FFmpeg/GStreamer backend or use another supported codec");
        }
        result.backend_ = result.writer_.getBackendName();
        return result;
    }

    void PortableVideoWriter::write(const cv::Mat &frame)
    {
        requireBgrFrame(frame);
        writer_.write(frame);
    }

    void PortableVideoWriter::release()
    {
        if (writer_.isOpened())
        {
            writer_.release();
        }
    }

    bool PortableVideoWriter::isOpened() const noexcept { return writer_.isOpened(); }
    const std::string &PortableVideoWriter::backend() const noexcept { return backend_; }

} // namespace wisedo::vision
