# Supervisor workstation CP-SAT CLI

This is an optional, single-request C++17 solver executable for the Go Supervisor to invoke as a child process. It reads one JSON request from stdin and writes one JSON response to stdout. It is not a daemon, gRPC service, approved-plan publisher, or boot dependency: if it is absent, the Go Supervisor remains independently usable. It has no runtime, database, identity, SDK, or deployment dependency on WiseDo MES.

The scheduling model adapts the MES `factoryos.cpsat.v1` scheduling concepts (orders, operation chains, fixed workcenters with capacity, downtime demand, and shift horizon) into the Supervisor `PlanningRequestV1` shape. MES implementation source was not present in the local checkouts, so this is a contract-level independent implementation, not copied or source-verified code. Wire semantics follow `internal/workstation/planning.go`; the Go boundary must validate tenant/site authority and the request before invoking this process.

## Wire format

Request fields are `schema_version`, `line_id`, RFC3339 `starts_at` and `ends_at`, IANA `timezone`, `orders`, `workstations`, and `downtimes`. Orders contain job order ID/revision, optional ready/due timestamps, priority, and ordered operations. Operations use `operation_id`, `sequence`, `workstation_id`, `name`, positive integer `quantity`, decimal-string `seconds_per_unit`, and `duration_basis`. Workstations contain `id` and positive `capacity`; downtime contains workstation, RFC3339 interval and positive `demand`.

Response has `schema_version`, `method`, `status` (`OPTIMAL`, `FEASIBLE`, `INFEASIBLE`, or `UNKNOWN`), `is_optimized`, `assignments`, and `makespan_seconds` for feasible solutions. `is_optimized` is true only for a proven optimum. No assignments are returned for infeasible/unknown. Times are emitted in UTC RFC3339. Assignment timing uses integer seconds; operation duration is the ceiling of quantity times standard cycle time.

The trusted caller configures bounded search controls with process arguments (`--time-limit-seconds`, `--seed`, `--workers`); they are never accepted in the request body. Input is capped at 1 MiB, horizon at 24 hours, 50 orders, 200 operations, 100 stations and 100 downtime intervals. The standalone parser rejects unknown and duplicate fields. It accepts RFC3339 timestamps only at whole-second precision because the model uses integer-second ticks. The Go API remains responsible for canonical UUIDs and verifying that each timestamp offset agrees with the request's IANA timezone; the CLI only checks the timezone field is present as a string.

## Build and test

For a quick developer build, use the official OR-Tools 9.14 Ubuntu 24.04 C++ binary package. Extract it to a directory such as `/opt/ortools`; the package root contains `lib/cmake/ortools/ortoolsConfig.cmake` and exports `ortools::ortools`. The archive is `or-tools_amd64_ubuntu-24.04_cpp_v9.14.6206.tar.gz`, with SHA-256 `be3855a32a7390c3957d43ebd3faec1610acdc28f06ef33cb50f1f72a9aa6621`.

```sh
cmake -S src/workstation_cpsat -B build/workstation-cpsat -DCMAKE_BUILD_TYPE=Release -DWORKSTATION_CPSAT_BUILD_TESTING=ON -DWORKSTATION_CPSAT_ORTOOLS_ROOT=/opt/ortools
cmake --build build/workstation-cpsat --parallel
ctest --test-dir build/workstation-cpsat --output-on-failure
```

If no package root is supplied, CMake fetches OR-Tools v9.14 source and builds it; this fallback is much slower. nlohmann/json v3.11.3 is fetched if it is not installed. The Docker build pins and verifies the binary package SHA before compiling and running tests:

```sh
docker build -t supervisor-workstation-cpsat -f src/workstation_cpsat/Dockerfile src/workstation_cpsat
```

OR-Tools is distributed under Apache-2.0; nlohmann/json is MIT-licensed. The OR-Tools archive includes its license under `share/doc/ortools/LICENSE`; nlohmann/json license is available from its upstream project and must be included in third-party notices for distributed builds.

This solver produces a scheduling suggestion only. The Supervisor must retain human review and its existing approval boundary before publishing/delivering a plan.
