ARG GO_IMAGE=golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:5ec23b7c622b00547753bc6ffd78085dd868a68aeb41c8e0a1418f033e13d96d
FROM ${NODE_IMAGE} AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/index.html web/tsconfig.json ./
COPY web/src ./src
COPY contracts/typescript /contracts/typescript
RUN npm run build
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -o /supervisor ./cmd/supervisor
FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852 AS cpsat-build
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends build-essential ca-certificates cmake curl nlohmann-json3-dev && rm -rf /var/lib/apt/lists/*
COPY src/workstation_cpsat /src
RUN test "$TARGETARCH" = "amd64" || { echo "CP-SAT runtime currently supports amd64 only" >&2; exit 1; }
RUN curl --fail --location --silent --show-error https://github.com/google/or-tools/releases/download/v9.14/or-tools_amd64_ubuntu-24.04_cpp_v9.14.6206.tar.gz --output /tmp/ortools.tar.gz \
 && echo "be3855a32a7390c3957d43ebd3faec1610acdc28f06ef33cb50f1f72a9aa6621  /tmp/ortools.tar.gz" | sha256sum --check - \
 && mkdir -p /opt/ortools \
 && tar -xzf /tmp/ortools.tar.gz --strip-components=1 -C /opt/ortools \
 && cmake -S /src -B /build -DCMAKE_BUILD_TYPE=Release -DWORKSTATION_CPSAT_BUILD_TESTING=ON -DCMAKE_PREFIX_PATH=/opt/ortools -DCMAKE_BUILD_RPATH=/opt/ortools/lib \
 && cmake --build /build --parallel 2 \
 && ctest --test-dir /build --output-on-failure
FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libabsl20220623t64 libprotobuf32t64 tzdata zlib1g && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /supervisor /supervisor
COPY --from=cpsat-build /build/bin/supervisor_workstation_cpsat /usr/local/bin/supervisor_workstation_cpsat
COPY --from=cpsat-build /opt/ortools/lib /opt/ortools/lib
COPY --from=cpsat-build /opt/ortools/share/doc/ortools/LICENSE /usr/share/licenses/ortools/LICENSE
COPY --from=cpsat-build /usr/share/doc/nlohmann-json3-dev/copyright /usr/share/licenses/nlohmann-json/copyright
COPY --from=web /web/dist /app/web/dist
USER 10001:10001
ENV SUPERVISOR_LISTEN_ADDRESS=0.0.0.0:8080
# The solver finds OR-Tools through its RPATH (/opt/ortools/lib): it runs with a cleared
# environment, so no LD_LIBRARY_PATH reaches it (WIS-699).
ENV SUPERVISOR_WORKSTATION_CPSAT_PATH=/usr/local/bin/supervisor_workstation_cpsat
EXPOSE 8080
ENTRYPOINT ["/supervisor"]
CMD ["api"]
