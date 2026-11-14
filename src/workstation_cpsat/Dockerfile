FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS build
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates cmake curl nlohmann-json3-dev && rm -rf /var/lib/apt/lists/*
ARG ORTOOLS_URL=https://github.com/google/or-tools/releases/download/v9.14/or-tools_amd64_ubuntu-24.04_cpp_v9.14.6206.tar.gz
ARG ORTOOLS_SHA256=be3855a32a7390c3957d43ebd3faec1610acdc28f06ef33cb50f1f72a9aa6621
RUN curl -fsSL "$ORTOOLS_URL" -o /tmp/ortools.tar.gz && echo "$ORTOOLS_SHA256  /tmp/ortools.tar.gz" | sha256sum -c - && mkdir -p /opt/ortools && tar -xzf /tmp/ortools.tar.gz -C /opt/ortools --strip-components=1 && rm /tmp/ortools.tar.gz
WORKDIR /src
COPY . .
RUN cmake -S . -B build -DCMAKE_BUILD_TYPE=Release -DWORKSTATION_CPSAT_BUILD_TESTING=ON -DWORKSTATION_CPSAT_ORTOOLS_ROOT=/opt/ortools -DCMAKE_PREFIX_PATH=/opt/ortools && cmake --build build --parallel 2 && ctest --test-dir build --output-on-failure

FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends libgcc-s1 libstdc++6 && rm -rf /var/lib/apt/lists/*
COPY --from=build /opt/ortools/lib/ /opt/ortools/lib/
RUN mkdir -p /usr/share/licenses/ortools /usr/share/licenses/nlohmann-json
COPY --from=build /opt/ortools/share/doc/ortools/LICENSE /usr/share/licenses/ortools/LICENSE
COPY --from=build /usr/share/doc/nlohmann-json3-dev/copyright /usr/share/licenses/nlohmann-json/copyright
COPY --from=build /src/build/bin/supervisor_workstation_cpsat /usr/local/bin/supervisor_workstation_cpsat
ENV LD_LIBRARY_PATH=/opt/ortools/lib
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/supervisor_workstation_cpsat"]
