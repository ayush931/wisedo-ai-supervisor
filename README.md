# wiseDo AI Supervisor

> **Independent Supervisor implementation:** the Go foundation, React UI and local verification instructions are in [deploy/supervisor/README.md](deploy/supervisor/README.md). Build with `make supervisor-build`. The legacy OEE Python component has been decommissioned.

Independent, human-governed operational intelligence for manufacturing sites. Repository: [Wisedo-Technology/wisedo-ai-supervisor](https://github.com/Wisedo-Technology/wisedo-ai-supervisor).

## Local setup and run

The repository has two independent runtime components:

- **Go Supervisor:** API, PostgreSQL/RLS ledger, OIDC boundary, Temporal worker, and React web shell.
- **Future Vision C++:** native C++20/OpenCV edge inspection service. It is not required to run the Go Supervisor.

### Go Supervisor packages

Install Go 1.27, Node.js 22, Docker Engine, and Docker Compose v2. Copy the project environment template and replace every local password placeholder:

```bash
cp .env.example .env
set -a; source .env; set +a
```

Start the local PostgreSQL, Keycloak, and Temporal services, apply migrations, and build the application:

```bash
docker compose --env-file .env -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
docker compose --env-file .env -f deploy/supervisor/compose.yml --profile tools run --rm migrate
make supervisor-build
```

Run the API and worker in separate terminals:

```bash
./build/supervisor api
./build/supervisor worker
```

Run the web frontend in development mode (with Vite HMR and API proxying):

```bash
make web-dev
# or: cd web && npm run dev
```

Run the Go test suite with:

```bash
go test ./internal/... ./cmd/...
```

### Future Vision C++ packages

Future Vision requires a C++20 compiler, CMake 3.22+, and OpenCV 4 with `core`, `imgproc`, `imgcodecs`, and `videoio`. Installation commands for Ubuntu/Debian, Fedora, Arch, macOS, ARM64, and Docker are in the [native C++ guide](src/wisedo_fixture_vision_cpp/README.md).

Build and test it locally:

```bash
cmake -S src/wisedo_fixture_vision_cpp -B build/vision-cpp -DCMAKE_BUILD_TYPE=Release
cmake --build build/vision-cpp --parallel
ctest --test-dir build/vision-cpp --output-on-failure
```

Run first-frame inspection without a custom configuration file:

```bash
build/vision-cpp/wisedo_vision_inspect \
 --video video.mp4 \
 --output-dir inspection_results \
 --side-by-side \
 --codec mp4v
```

Frame 0 becomes the golden reference; later frames are classified as `CORRECT` or `DEVIATED`. No Python interpreter is required for either the native Future Vision runtime or the Go Supervisor runtime. AWS training scripts under `deploy/aws/` are optional and separate from local application execution.

## Product boundary and vision

The AI Supervisor is a standalone operational intelligence system that helps a plant team understand its approved process, detect measurable production shortfalls, investigate using trustworthy evidence, and coordinate a bounded response with a human in control.

The system answers six practical questions at every consequential step:

1. **What happened?** (Source-backed observation)
2. **What process is approved?** (Reviewed graph and plan)
3. **What is uncertain?** (Coverage, freshness, contradictions, and missing data)
4. **What could be done?** (Grounded recovery alternatives)
5. **Who authorized the exact action?** (Scoped, distinct-human approval of exact action hash)
6. **What actually happened afterward?** (Authoritative task readback and comparable measurements)

When any prerequisite cannot be established, the system reports the specific reason and remains read-only rather than filling gaps with AI speculation.

### Core capabilities

- **Independent commissioning:** Owner signs in, configures site/roles/sources; OIDC and tenant/site policies enforced with zero external WiseDo dependencies.
- **Process discovery:** Resumable interviews and source capture produce candidate claims; human review publishes an approved, versioned graph (`ApprovedProcessSnapshotV1`).
- **Operational evidence:** Plans, good-unit counts, events, and reports become traceable records with exact source mappings, sequence completeness, freshness, and quarantine.
- **Exception intelligence:** Repeated quantified plan risk (two consecutive evaluable 15-minute windows with deficit $\ge \max(5\text{ units}, 10\%)$) opens one durable case.
- **Investigation:** Supervisor sees hypotheses, questions, and alternative responses; evidence and uncertainty remain visible.
- **Authorized coordination:** Distinct human approves one bounded internal inspect/report task (`TaskV1`) with exact action hash, 30-minute expiry, and pre-dispatch rechecks.
- **Outcome and feedback:** Measurable post-action production windows are compared to baseline; append-only feedback with no automatic model weight training or unreviewed memory promotion.

## Edge Computer Vision: Future Vision (`wisedo_fixture_vision_cpp`) — C++ OpenCV

The repository contains the **Future Vision** native module (`src/wisedo_fixture_vision_cpp/`, [ADR 0007](docs/adr/video-embedding-docs/0007-wisedo-future-vision-architecture.md)), a deterministic C++20/OpenCV computer vision engine for industrial assembly fixtures and packaging lines.

- **Runtime:** Future Vision runs as a native C++20/OpenCV executable; it has no Python runtime dependency.
- **Edge rationale:** The C++ implementation provides deterministic slot inspection, frame-0 reference comparison, ARM64-compatible OpenCV video output, and direct camera/video backend support.
- **Architectural Integration:** Future Vision operates as an **external edge inspection sensor**. Frame deviation records and anomaly signals are published to the Go Supervisor's Operational Intake API (`POST /v1/sites/{site}/operational-sources/{source}/batches`) as normalized `SourceRecordV1` events. Future Vision adheres strictly to the PRD safety boundary: it performs optical inspection and evidence emission without autonomous PLC actuation or direct physical control.

## Current implementation status

This repository was renamed from `plantops-oee-intelligence`; its history, issues, PRs, and existing OEE code remain intact. Status below is what is merged on `main`; Linear is the source of truth for issue state. Issue states were re-checked on 2026-11-02 (WIS-632): every Supervisor v1 issue named here is Done in Linear. The "not yet built" lists were written before those issues closed and have not been re-verified line by line; check the issue before relying on one.

- **Independent Go Foundation (WIS-290, Linear: Done):** OIDC identity with PKCE and recent-auth proof, tenant/site isolation with forced PostgreSQL RLS (migrations `001`–`004`), bootstrap invitations redeemed from the token alone (`002`, `025`), membership changes, and Temporal worker integration. Not yet built: service principals, commissioning and promotion APIs, break-glass access, rate limiting, object storage with retention, telemetry, SBOM and release manifest.
- **Process Knowledge Graph (WIS-270, Linear: Done):** Provenance-bound candidate claims, entity revisions, source authority, claim reviews, conflict resolution, approved graph snapshots, and bounded graph retrieval (`005`–`019`). Not yet built: the `/knowledge` entity read, the contested-claim filter, and the performance gate.
- **Production Context (WIS-272, Linear: Done):**
  - Operational source registry, batch intake, and outbox progress fold (`020`–`022`).
  - Plan revisions with PlanApprovalV1 and intake admission for `production_plan` (`023`).
  - PlantContextSnapshotV1 with READY/LIMITED/NOT_READY readiness, exact 15-minute window arithmetic, and the two-window shortfall rule (`024`).
  - Not yet built: CSV/JSON imports, source revoke and health endpoints, backpressure and dead-letter, retention, and the load gate.
- **OEE calculation kernel (WIS-224) and timeline (WIS-226):** exact rational A/P/Q/OEE/TEEP and deterministic timeline normalization. Zero denominators publish no value (ADR 0005). The kernel is not yet served by any API route.
- **Interviews, cases, approvals, agent runtime, proposals, tasks, workspaces, feedback and qualification (WIS-271, WIS-273–WIS-281, Linear: Done):** their routes and migrations (`044`–`071`) are on `main`. "Done" is the Linear state, not customer acceptance (rule 09).
- **Onboarding agent (WIS-610 and children WIS-611–WIS-632):** progress and open work are in [`docs/onboarding-agent/README.md`](docs/onboarding-agent/README.md) §0.
- **Offline AI Training Infrastructure (`deploy/aws/`):** Converted from Python provisioning to declarative **Terraform (`.tf`)** modules (`main.tf`, `iam.tf`, `network.tf`, `variables.tf`, `outputs.tf`), managing S3 data lakes, SageMaker, KMS, ECR, and IAM least-privilege policies.

The legacy Python component `wisedo_plantops_oee` has been decommissioned. All product operational intelligence, knowledge graph, intake, and arithmetic reside in the independent Go Supervisor.

## Architecture and delivery

- [Independent Supervisor architecture and engineering gates](docs/independent-supervisor.md)
- [Future Vision Architecture & C++ Migration](docs/adr/video-embedding-docs/0007-wisedo-future-vision-architecture.md)
- [Future Vision User & Developer Guide](docs/adr/video-embedding-docs/future-vision.md)
- [Existing OEE component: setup, layout and limitations](docs/oee-foundation.md)
- [User manual: planning, running and handing over shifts](docs/user-manual/shifts.md)
- [Contributing and branch conventions](CONTRIBUTING.md)
- [Deployment status](deploy/README.md)
- [Linear engineering contract](https://linear.app/wisedo-technology/document/independent-ai-supervisor-engineering-contract-dependencies-and-73f40cb14475)
- [WIS-146: architecture/start gate](https://linear.app/wisedo-technology/issue/WIS-146)
- [WIS-290: independent application foundation](https://linear.app/wisedo-technology/issue/WIS-290)

WIS-265–WIS-269 group product outcomes; WIS-270–WIS-281 define the implementation and qualification work.

## Component verification

```bash
# Verify Go Supervisor foundation and domain tests
go test ./internal/... ./cmd/...

# Retained component verification
make install
make check
```
