# syntax=docker/dockerfile:1

FROM ubuntu:24.04 AS build

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    cmake \
    libopencv-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY src/wisedo_fixture_vision_cpp ./wisedo_fixture_vision_cpp
RUN cmake -S wisedo_fixture_vision_cpp -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_TESTING=OFF \
    && cmake --build build --parallel

FROM ubuntu:24.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
    ca-certificates \
    ffmpeg \
    libopencv-dev \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/build/wisedo_vision_inspect /usr/local/bin/wisedo_vision_inspect
USER 10001:10001
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/wisedo_vision_inspect"]
