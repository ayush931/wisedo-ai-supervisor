# FindOpenCV.cmake
# Multi-strategy discovery module for OpenCV on Linux (x86_64, aarch64), macOS, and custom installations.

if(OpenCV_FOUND AND OpenCV_LIBS)
    return()
endif()

# Strategy 1: Search standard CMake config locations
set(_OPENCV_CONFIG_HINTS
    ${OpenCV_DIR}
    $ENV{OpenCV_DIR}
    /usr/lib/x86_64-linux-gnu/cmake/opencv4
    /usr/lib/aarch64-linux-gnu/cmake/opencv4
    /usr/lib64/cmake/opencv4
    /usr/lib/cmake/opencv4
    /usr/local/lib/cmake/opencv4
    /usr/local/share/opencv4
    /usr/share/opencv4
    /opt/opencv/lib/cmake/opencv4
    /opt/homebrew/lib/cmake/opencv4
    /usr/local/Cellar/opencv
)

find_package(OpenCV CONFIG QUIET HINTS ${_OPENCV_CONFIG_HINTS})
if(OpenCV_FOUND AND OpenCV_LIBS)
    set(OpenCV_FOUND TRUE)
    return()
endif()

# Strategy 2: Use pkg-config
find_package(PkgConfig QUIET)
if(PkgConfig_FOUND)
    pkg_check_modules(PC_OpenCV QUIET opencv4)
    if(NOT PC_OpenCV_FOUND)
        pkg_check_modules(PC_OpenCV QUIET opencv)
    endif()

    if(PC_OpenCV_FOUND AND PC_OpenCV_LIBRARIES)
        set(OpenCV_FOUND TRUE)
        set(OpenCV_VERSION ${PC_OpenCV_VERSION})
        set(OpenCV_INCLUDE_DIRS ${PC_OpenCV_INCLUDE_DIRS})
        set(OpenCV_LIBS ${PC_OpenCV_LIBRARIES})
        return()
    endif()
endif()

# Strategy 3: Direct filesystem search for headers and libraries
find_path(OpenCV_INCLUDE_DIRS
    NAMES opencv2/core.hpp
    HINTS
        /usr/include/opencv4
        /usr/local/include/opencv4
        /usr/include
        /usr/local/include
        /opt/homebrew/include/opencv4
        /opt/opencv/include
)

set(_OPENCV_REQUIRED_COMPONENTS core imgproc videoio imgcodecs)
set(_OPENCV_FOUND_LIBS "")
foreach(_comp ${_OPENCV_REQUIRED_COMPONENTS})
    find_library(OpenCV_${_comp}_LIB
        NAMES opencv_${_comp}
        HINTS
            /usr/lib/x86_64-linux-gnu
            /usr/lib/aarch64-linux-gnu
            /usr/lib64
            /usr/lib
            /usr/local/lib
            /opt/homebrew/lib
            /opt/opencv/lib
    )
    if(OpenCV_${_comp}_LIB)
        list(APPEND _OPENCV_FOUND_LIBS ${OpenCV_${_comp}_LIB})
    endif()
endforeach()

if(OpenCV_INCLUDE_DIRS AND _OPENCV_FOUND_LIBS)
    set(OpenCV_FOUND TRUE)
    set(OpenCV_LIBS ${_OPENCV_FOUND_LIBS})
    set(OpenCV_VERSION "4.x (manual)")
    return()
endif()

# Strategy 4: If all fail, provide a helpful and actionable message
include(FindPackageHandleStandardArgs)
find_package_handle_standard_args(OpenCV
    REQUIRED_VARS OpenCV_INCLUDE_DIRS OpenCV_LIBS
    FAIL_MESSAGE
    "\n========================================================================\n"
    "OpenCV C++ development files not found!\n\n"
    "Please install OpenCV C++ development headers and libraries:\n"
    "  • Ubuntu / Debian / Pi OS: sudo apt-get update && sudo apt-get install -y libopencv-dev\n"
    "  • Fedora / RHEL / CentOS:  sudo dnf install -y opencv-devel\n"
    "  • Arch Linux:             sudo pacman -S opencv\n"
    "  • macOS (Homebrew):       brew install opencv\n\n"
    "If OpenCV is installed in a custom location, pass -DOpenCV_DIR=<path>:\n"
    "  cmake -S src/wisedo_fixture_vision_cpp -B build -DOpenCV_DIR=/usr/lib/x86_64-linux-gnu/cmake/opencv4\n"
    "========================================================================\n"
)
