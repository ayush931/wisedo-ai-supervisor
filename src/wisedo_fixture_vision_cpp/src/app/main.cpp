#include <wisedo/vision/vision.hpp>

#include <opencv2/imgcodecs.hpp>
#include <opencv2/imgproc.hpp>
#include <opencv2/videoio.hpp>

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <stdexcept>
#include <string>
#include <tuple>
#include <utility>
#include <vector>

namespace
{

    struct Arguments
    {
        std::string video = "video.mp4";
        std::string output_dir = "inspection_results";
        bool side_by_side = false;
        std::string csv_path;
        std::string json_path;
        std::string correct_csv;
        std::string correct_json;
        std::string deviated_csv;
        std::string deviated_json;
        std::string render_video;
        std::string config_path;
        std::string codec = "mp4v";
        bool deduplicate = false;
        bool include_duplicates = false;
        double diff_thresh = 10.0;
        int max_frames = -1;
    };

    void cleanDirectoryImages(const std::filesystem::path &dir)
    {
        if (!std::filesystem::exists(dir))
            return;
        for (const auto &entry : std::filesystem::directory_iterator(dir))
        {
            if (entry.is_regular_file())
            {
                const auto ext = entry.path().extension().string();
                if (ext == ".jpg" || ext == ".jpeg" || ext == ".png")
                {
                    std::error_code ec;
                    std::filesystem::remove(entry.path(), ec);
                }
            }
        }
    }

    Arguments parse(int argc, char **argv)
    {
        Arguments args;
        for (int i = 1; i < argc; ++i)
        {
            const std::string flag = argv[i];
            auto value = [&]()
            {
                if (i + 1 >= argc)
                {
                    throw std::invalid_argument("missing value for " + flag);
                }
                return std::string(argv[++i]);
            };

            if (flag == "--video" || flag == "--input")
                args.video = value();
            else if (flag == "--output-dir")
                args.output_dir = value();
            else if (flag == "--side-by-side")
                args.side_by_side = true;
            else if (flag == "--csv")
                args.csv_path = value();
            else if (flag == "--json")
                args.json_path = value();
            else if (flag == "--correct-csv")
                args.correct_csv = value();
            else if (flag == "--correct-json")
                args.correct_json = value();
            else if (flag == "--deviated-csv")
                args.deviated_csv = value();
            else if (flag == "--deviated-json")
                args.deviated_json = value();
            else if (flag == "--render-video" || flag == "--output")
                args.render_video = value();
            else if (flag == "--config")
                args.config_path = value();
            else if (flag == "--codec")
                args.codec = value();
            else if (flag == "--deduplicate")
                args.deduplicate = true;
            else if (flag == "--include-duplicates")
                args.include_duplicates = true;
            else if (flag == "--diff-threshold" || flag == "--diff-thresh")
                args.diff_thresh = std::stod(value());
            else if (flag == "--max-frames")
                args.max_frames = std::stoi(value());
            else if (flag == "--help")
            {
                std::cout << "Usage: wisedo_vision_inspect [options]\n"
                          << "Options:\n"
                          << "  --video <path>         Path to inspection video (default: video.mp4)\n"
                          << "  --output-dir <path>    Base directory to save results (default: inspection_results)\n"
                          << "  --side-by-side         Also export side-by-side comparison images and video\n"
                          << "  --render-video <path>  Path to render annotated video\n"
                          << "  --csv <path>           Path to export all-frames CSV report\n"
                          << "  --json <path>          Path to export all-frames JSON report\n"
                          << "  --correct-csv <path>   Path to export separate CSV report for corrected frames\n"
                          << "  --correct-json <path>  Path to export separate JSON report for corrected frames\n"
                          << "  --deviated-csv <path>  Path to export separate CSV report for deviated frames\n"
                          << "  --deviated-json <path> Path to export separate JSON report for deviated frames\n"
                          << "  --config <path>        Optional path to slot configuration JSON\n"
                          << "  --codec <fourcc>       FourCC codec (default: mp4v)\n"
                          << "  --deduplicate          Deduplicate repeating consecutive frames\n"
                          << "  --include-duplicates   Keep all frames (default behavior)\n"
                          << "  --max-frames <N>       Limit processing to first N frames\n"
                          << "  --help                 Show this help message\n";
                std::exit(0);
            }
            else
            {
                throw std::invalid_argument("unknown argument: " + flag);
            }
        }

        // Set default output paths based on output_dir if not specified
        if (args.csv_path.empty())
            args.csv_path = args.output_dir + "/frame_deviations.csv";
        if (args.json_path.empty())
            args.json_path = args.output_dir + "/frame_deviations.json";
        if (args.correct_csv.empty())
            args.correct_csv = args.output_dir + "/corrected_frames.csv";
        if (args.correct_json.empty())
            args.correct_json = args.output_dir + "/corrected_frames.json";
        if (args.deviated_csv.empty())
            args.deviated_csv = args.output_dir + "/deviated_frames.csv";
        if (args.deviated_json.empty())
            args.deviated_json = args.output_dir + "/deviated_frames.json";
        if (args.render_video.empty())
            args.render_video = args.output_dir + "/annotated_results.mp4";

        if (args.include_duplicates)
            args.deduplicate = false;

        return args;
    }

} // namespace

int main(int argc, char **argv)
{
    try
    {
        const Arguments args = parse(argc, argv);
        if (!std::filesystem::exists(args.video))
        {
            throw std::runtime_error("Video file not found: " + args.video);
        }

        // 1. Create and clean output directories
        const std::filesystem::path out_dir(args.output_dir);
        const std::filesystem::path corrected_dir = out_dir / "corrected";
        const std::filesystem::path deviated_dir = out_dir / "deviated";
        const std::filesystem::path sbs_dir = out_dir / "side_by_side";
        const std::filesystem::path sbs_corrected_dir = sbs_dir / "corrected";
        const std::filesystem::path sbs_deviated_dir = sbs_dir / "deviated";

        std::filesystem::create_directories(corrected_dir);
        std::filesystem::create_directories(deviated_dir);
        cleanDirectoryImages(out_dir);
        cleanDirectoryImages(corrected_dir);
        cleanDirectoryImages(deviated_dir);

        if (args.side_by_side)
        {
            std::filesystem::create_directories(sbs_corrected_dir);
            std::filesystem::create_directories(sbs_deviated_dir);
            cleanDirectoryImages(sbs_corrected_dir);
            cleanDirectoryImages(sbs_deviated_dir);
        }

        // 2. Open input video capture
        cv::VideoCapture cap(args.video);
        if (!cap.isOpened())
        {
            throw std::runtime_error("Could not open input video: " + args.video);
        }

        const double fps = cap.get(cv::CAP_PROP_FPS) > 0.0 ? cap.get(cv::CAP_PROP_FPS) : 24.0;
        const int total_frames = static_cast<int>(cap.get(cv::CAP_PROP_FRAME_COUNT));
        const int width = static_cast<int>(cap.get(cv::CAP_PROP_FRAME_WIDTH));
        const int height = static_cast<int>(cap.get(cv::CAP_PROP_FRAME_HEIGHT));
        if (width < 2 || height < 2)
        {
            throw std::runtime_error("Video has invalid dimensions");
        }

        // 3. Read first frame (frame 0) as golden reference standard
        cap.set(cv::CAP_PROP_POS_FRAMES, 0);
        cv::Mat first_frame;
        if (!cap.read(first_frame) || first_frame.empty())
        {
            throw std::runtime_error("Could not read first frame from: " + args.video);
        }

        // 4. Configure slot geometry and dynamic HSV thresholds
        wisedo::vision::InspectionConfig config;
        if (!args.config_path.empty())
        {
            config = wisedo::vision::InspectionConfig::load(args.config_path);
        }
        else
        {
            auto discovered = wisedo::vision::autoDiscoverSlots(first_frame, &config.detection);
            if (!discovered.empty())
            {
                config.slots = std::move(discovered);
            }
            else
            {
                config = wisedo::vision::InspectionConfig::defaultFor({width, height});
            }
        }

        const auto total_elements = config.slots.size();

        wisedo::vision::ComparatorConfig comp_config;
        comp_config.inspection_config = std::move(config);
        comp_config.diff_threshold = args.diff_thresh;
        comp_config.deduplicate = args.deduplicate;

        wisedo::vision::FirstFrameComparator comparator(std::move(comp_config));
        comparator.bootstrapGoldenReference(first_frame, fps);

        // 5. Open video writers
        std::optional<wisedo::vision::PortableVideoWriter> video_writer;
        if (!args.render_video.empty())
        {
            video_writer = wisedo::vision::PortableVideoWriter::open(args.render_video, fps, {width, height}, args.codec);
        }

        std::optional<wisedo::vision::PortableVideoWriter> sbs_video_writer;
        std::string sbs_video_path;
        if (args.side_by_side && !args.render_video.empty())
        {
            sbs_video_path = (out_dir / "annotated_side_by_side.mp4").string();
            sbs_video_writer = wisedo::vision::PortableVideoWriter::open(sbs_video_path, fps, {width * 2, height}, args.codec);
        }

        // 6. Print console banner
        std::cout << "\n===========================================================================\n"
                  << "FIRST FRAME REFERENCE INSPECTOR\n"
                  << "Video Source      : " << args.video << " (" << width << "x" << height << ", " << fps << " FPS, " << total_frames << " frames)\n"
                  << "Reference Baseline: Frame 0 (" << total_elements << " components established)\n"
                  << "Output Directory  : " << args.output_dir << "\n"
                  << "===========================================================================\n\n";

        // Reset video to frame 0 for synchronous sequential processing
        cap.set(cv::CAP_PROP_POS_FRAMES, 0);

        std::vector<wisedo::vision::FrameDeviationRecord> records;
        int frame_idx = 0;
        int computed_count = 0;
        const auto t0 = std::chrono::steady_clock::now();

        cv::Mat prev_frame;
        cv::Mat last_annotated;
        cv::Mat last_sbs_annotated;
        using Signature = std::tuple<bool, std::vector<std::string>, std::size_t>;
        std::optional<Signature> last_signature;
        int current_start_idx = 0;
        int current_repeat_count = 0;

        cv::Mat frame;
        while (cap.read(frame) && !frame.empty())
        {
            if (args.max_frames >= 0 && frame_idx >= args.max_frames)
            {
                break;
            }

            const double t_sec = fps > 0.0 ? frame_idx / fps : 0.0;

            // 1. Content change gating: skip computation if content has not changed
            const bool content_changed = prev_frame.empty() || (!args.deduplicate) ||
                                         wisedo::vision::hasContentChanged(
                                             prev_frame,
                                             frame,
                                             comparator.boardBbox(),
                                             args.diff_thresh < 5.0 ? args.diff_thresh : 1.5);

            if (!content_changed)
            {
                current_repeat_count += 1;
                if (video_writer && !last_annotated.empty())
                {
                    video_writer->write(last_annotated);
                }
                if (sbs_video_writer && !last_sbs_annotated.empty())
                {
                    sbs_video_writer->write(last_sbs_annotated);
                }
                prev_frame = frame.clone();
                ++frame_idx;
                continue;
            }

            // 2. Content changed -> execute inspection & deviation computation
            computed_count += 1;
            auto result = comparator.processFrame(frame, frame_idx, fps);

            const auto missing_count = result.summary.missing_count;
            const auto extra_count = result.extra_boxes.size();
            const auto deviated_count = missing_count + extra_count;
            const bool is_correct = (deviated_count == 0);
            const std::string status_str = is_correct ? "CORRECT" : "DEVIATED";

            auto missing_sorted = result.summary.missing_slot_ids;
            std::sort(missing_sorted.begin(), missing_sorted.end());
            const Signature sig{is_correct, missing_sorted, extra_count};

            // 3. Deduplication check
            if (args.deduplicate && last_signature && *last_signature == sig)
            {
                current_repeat_count += 1;
                if (video_writer && !last_annotated.empty())
                {
                    video_writer->write(last_annotated);
                }
                if (sbs_video_writer && !last_sbs_annotated.empty())
                {
                    sbs_video_writer->write(last_sbs_annotated);
                }
                prev_frame = frame.clone();
                ++frame_idx;
                continue;
            }

            // 4. Result changed -> finalize previous record range if deduplicating
            if (args.deduplicate && !records.empty())
            {
                const int end_idx = frame_idx - 1;
                records.back().frame_range = (current_repeat_count > 1) ? (std::to_string(current_start_idx) + "-" + std::to_string(end_idx)) : std::to_string(current_start_idx);
                records.back().repeat_count = current_repeat_count;
                records.back().duration_sec = fps > 0.0 ? current_repeat_count / fps : 0.0;
            }

            current_start_idx = frame_idx;
            current_repeat_count = 1;
            last_signature = sig;
            last_annotated = result.annotated_frame;
            last_sbs_annotated = result.side_by_side_frame;

            // 5. Save annotated frame image
            char filename_buf[64];
            std::snprintf(filename_buf, sizeof(filename_buf), "frame_%04d_%s.jpg", frame_idx, status_str.c_str());
            const auto sub_dir = is_correct ? corrected_dir : deviated_dir;
            const auto frame_filepath = (sub_dir / filename_buf).string();
            cv::imwrite(frame_filepath, result.annotated_frame);

            // Optional side-by-side image
            if (args.side_by_side)
            {
                char sbs_filename_buf[64];
                std::snprintf(sbs_filename_buf, sizeof(sbs_filename_buf), "sbs_frame_%04d_%s.jpg", frame_idx, status_str.c_str());
                const auto sbs_sub_dir = is_correct ? sbs_corrected_dir : sbs_deviated_dir;
                cv::imwrite((sbs_sub_dir / sbs_filename_buf).string(), result.side_by_side_frame);
            }

            // 6. Write frames to video writers
            if (video_writer)
            {
                video_writer->write(result.annotated_frame);
            }
            if (sbs_video_writer)
            {
                sbs_video_writer->write(result.side_by_side_frame);
            }

            // 7. Store deviation record
            result.record.image_path = frame_filepath;
            result.record.frame_range = std::to_string(frame_idx);
            result.record.repeat_count = 1;
            result.record.duration_sec = fps > 0.0 ? 1.0 / fps : 0.0;
            records.push_back(result.record);

            // 8. Console frame log
            std::string dev_detail = is_correct ? "All match reference" : (std::to_string(missing_count) + " missing, " + std::to_string(extra_count) + " extra");
            std::cout << "Frame " << std::setw(3) << std::setfill('0') << frame_idx
                      << " (t=" << std::fixed << std::setprecision(2) << std::setw(5) << t_sec << "s) "
                      << (is_correct ? "[CORRECT]  | " : "[DEVIATED] | ")
                      << std::setw(2) << result.summary.present_count << "/" << total_elements
                      << " Correct | " << dev_detail << '\n';

            prev_frame = frame.clone();
            ++frame_idx;
        }

        // Finalize last record range
        if (args.deduplicate && !records.empty())
        {
            const int end_idx = frame_idx - 1;
            records.back().frame_range = (current_repeat_count > 1) ? (std::to_string(current_start_idx) + "-" + std::to_string(end_idx)) : std::to_string(current_start_idx);
            records.back().repeat_count = current_repeat_count;
            records.back().duration_sec = fps > 0.0 ? current_repeat_count / fps : 0.0;
        }

        // Release video writers
        if (video_writer)
        {
            video_writer->release();
            std::cout << "\n[Video Output] Annotated inspection video saved to: " << args.render_video << '\n';
        }
        if (sbs_video_writer)
        {
            sbs_video_writer->release();
            std::cout << "[Video Output] Annotated side-by-side video saved to: " << sbs_video_path << '\n';
        }

        const auto t1 = std::chrono::steady_clock::now();
        const double elapsed = std::chrono::duration<double>(t1 - t0).count();
        const double proc_speed = (elapsed > 0.0) ? (frame_idx / elapsed) : 0.0;

        // 9. Export Reports
        if (!args.csv_path.empty())
        {
            wisedo::vision::exportDeviationCsv(args.csv_path, records);
            std::cout << "[CSV Report] Saved full report to: " << args.csv_path << '\n';
        }

        if (!args.json_path.empty())
        {
            wisedo::vision::exportDeviationJson(args.json_path, args.video, records, frame_idx, args.deduplicate);
            std::cout << "[JSON Report] Saved full report to: " << args.json_path << '\n';
        }

        std::vector<wisedo::vision::FrameDeviationRecord> correct_records;
        std::vector<wisedo::vision::FrameDeviationRecord> deviated_records;
        for (const auto &r : records)
        {
            if (r.is_correct)
                correct_records.push_back(r);
            else
                deviated_records.push_back(r);
        }

        if (!args.correct_csv.empty())
        {
            wisedo::vision::exportDeviationCsv(args.correct_csv, correct_records);
            std::cout << "[Corrected Frames CSV] Saved to: " << args.correct_csv << '\n';
        }
        if (!args.correct_json.empty())
        {
            wisedo::vision::exportSubsetJson(args.correct_json, args.video, correct_records, "corrected", frame_idx);
            std::cout << "[Corrected Frames JSON] Saved to: " << args.correct_json << '\n';
        }

        if (!args.deviated_csv.empty())
        {
            wisedo::vision::exportDeviationCsv(args.deviated_csv, deviated_records);
            std::cout << "[Deviated Frames CSV] Saved to: " << args.deviated_csv << '\n';
        }
        if (!args.deviated_json.empty())
        {
            wisedo::vision::exportSubsetJson(args.deviated_json, args.video, deviated_records, "deviated", frame_idx);
            std::cout << "[Deviated Frames JSON] Saved to: " << args.deviated_json << '\n';
        }

        const auto correct_count = correct_records.size();
        const auto deviated_count = deviated_records.size();
        const double corr_pct = !records.empty() ? (100.0 * correct_count / records.size()) : 0.0;
        const double dev_pct = !records.empty() ? (100.0 * deviated_count / records.size()) : 0.0;
        const int skipped_count = frame_idx - computed_count;

        std::vector<int> correct_indices;
        for (const auto &r : correct_records)
        {
            correct_indices.push_back(r.frame_index);
        }
        const auto ranges = wisedo::vision::computeFrameRanges(correct_indices);
        std::string range_desc;
        for (std::size_t i = 0; i < ranges.size(); ++i)
        {
            if (i > 0)
                range_desc += ", ";
            range_desc += ranges[i];
        }
        if (range_desc.empty())
            range_desc = "None";

        // 10. Print summary of inspection results
        std::cout << "\n===========================================================================\n"
                  << "SUMMARY OF INSPECTION RESULTS (FIRST FRAME REFERENCE):\n"
                  << "  Total Video Frames Evaluated: " << frame_idx << '\n'
                  << "  Computation Executed On     : " << computed_count << " frames (skipped " << skipped_count << " unchanged frames)\n"
                  << "  Total Unique State Frames   : " << records.size() << '\n'
                  << "  CORRECTED States            : " << correct_count << " (" << std::fixed << std::setprecision(1) << corr_pct << "%)\n"
                  << "  DEVIATED States             : " << deviated_count << " (" << std::fixed << std::setprecision(1) << dev_pct << "%)\n"
                  << "  Execution Time              : " << std::fixed << std::setprecision(2) << elapsed << "s (" << std::fixed << std::setprecision(1) << proc_speed << " FPS)\n"
                  << "---------------------------------------------------------------------------\n"
                  << "SEPARATE OUTPUT DIRECTORIES & REPORTS:\n"
                  << "  [CORRECTED] Image Folder    : " << corrected_dir.string() << "/ (" << correct_count << " images)\n"
                  << "  [CORRECTED] Frame Ranges    : Frames " << range_desc << '\n'
                  << "  [CORRECTED] CSV Report      : " << args.correct_csv << '\n'
                  << "  [CORRECTED] JSON Report     : " << args.correct_json << '\n'
                  << "  [DEVIATED]  Image Folder    : " << deviated_dir.string() << "/ (" << deviated_count << " images)\n"
                  << "  [DEVIATED]  CSV Report      : " << args.deviated_csv << '\n'
                  << "  [DEVIATED]  JSON Report     : " << args.deviated_json << '\n';

        if (args.side_by_side)
        {
            std::cout << "  [SIDE-BY-SIDE] Corrected    : " << sbs_corrected_dir.string() << "/\n"
                      << "  [SIDE-BY-SIDE] Deviated     : " << sbs_deviated_dir.string() << "/\n";
        }
        if (video_writer)
        {
            std::cout << "  [ANNOTATED VIDEO] Output    : " << args.render_video << '\n';
        }
        if (sbs_video_writer)
        {
            std::cout << "  [SIDE-BY-SIDE VIDEO] Output : " << sbs_video_path << '\n';
        }
        std::cout << "===========================================================================\n\n";

        return 0;
    }
    catch (const std::exception &error)
    {
        std::cerr << "wisedo_vision_inspect: " << error.what() << '\n';
        return 2;
    }
}
