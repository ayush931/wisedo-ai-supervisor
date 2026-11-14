cmake_minimum_required(VERSION 3.22)
project(wisedo_fixture_vision_cpp LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

# 1. Register project cmake module path
list(APPEND CMAKE_MODULE_PATH "${CMAKE_CURRENT_SOURCE_DIR}/cmake")

# 2. Search for OpenCV using FindOpenCV.cmake module
find_package(OpenCV REQUIRED)

if(NOT OpenCV_FOUND OR NOT OpenCV_LIBS)
    message(FATAL_ERROR
        "\n========================================================================\n"
        "OpenCV C++ development files not found!\n\n"
        "To fix this, install the native OpenCV development package:\n"
        "  • Ubuntu / Debian / Pi OS: sudo apt-get update && sudo apt-get install -y libopencv-dev\n"
        "  • Fedora / RHEL / CentOS:  sudo dnf install -y opencv-devel\n"
        "  • Arch Linux:             sudo pacman -S opencv\n"
        "  • macOS (Homebrew):       brew install opencv\n\n"
        "Or pass the OpenCV directory manually:\n"
        "  cmake -S src/wisedo_fixture_vision_cpp -B build -DOpenCV_DIR=/usr/lib/x86_64-linux-gnu/cmake/opencv4\n"
        "========================================================================\n"
    )
endif()

message(STATUS "OpenCV Version     : ${OpenCV_VERSION}")
message(STATUS "OpenCV Include Dirs: ${OpenCV_INCLUDE_DIRS}")
message(STATUS "OpenCV Libraries   : ${OpenCV_LIBS}")

# 3. Vision Core Library Target
add_library(wisedo_vision_core STATIC
    src/core/detection_config.cpp
    src/core/inspection_comparison.cpp
    src/core/rendering_comparator.cpp
    src/core/reporting_io.cpp
)
target_include_directories(wisedo_vision_core PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/include
    ${OpenCV_INCLUDE_DIRS}
)
target_link_libraries(wisedo_vision_core PUBLIC ${OpenCV_LIBS})
target_compile_options(wisedo_vision_core PRIVATE -Wall -Wextra -Wpedantic)

# 4. CLI Inspection Executable Target
add_executable(wisedo_vision_inspect src/app/main.cpp)
target_link_libraries(wisedo_vision_inspect PRIVATE wisedo_vision_core ${OpenCV_LIBS})
target_compile_options(wisedo_vision_inspect PRIVATE -Wall -Wextra -Wpedantic)

# 5. Unit Test Suite (CTest)
include(CTest)
if(BUILD_TESTING)
    add_executable(wisedo_vision_core_test tests/unit/test_core.cpp)
    target_link_libraries(wisedo_vision_core_test PRIVATE wisedo_vision_core ${OpenCV_LIBS})
    target_compile_options(wisedo_vision_core_test PRIVATE -Wall -Wextra -Wpedantic -UNDEBUG)
    add_test(NAME wisedo_vision_core_test COMMAND wisedo_vision_core_test)
endif()
