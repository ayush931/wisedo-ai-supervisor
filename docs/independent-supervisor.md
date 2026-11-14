# wiseDo AI Supervisor — Independent Product Architecture

Repository: <https://github.com/Wisedo-Technology/wisedo-ai-supervisor>  
Version: 2.0 draft · September 30, 2026  
Status: Independent-product engineering contract.

The repository `Wisedo-Technology/wisedo-ai-supervisor` is an independent product. It preserves its Git history from the earlier OEE component foundation without inheriting any runtime, database, identity, SDK, or deployment dependency on existing WiseDo MES, PlantOps, Sight, or Flow.

---

## 1. Product Outcome and Scope

The **wiseDo AI Supervisor** is a standalone, human-governed operational intelligence system for manufacturing sites. It helps a plant team understand its approved process, detect measurable production shortfalls, investigate using trustworthy evidence, and coordinate a bounded response with a human in control.

The system is designed to answer six practical operational questions at every consequential step:

1. **What happened?** (Source-backed observation and immutable event provenance)
2. **What process is approved?** (Reviewed process graph and approved production plan)
3. **What is uncertain?** (Coverage gaps, freshness, contradictions, and missing inputs)
4. **What could be done?** (Grounded recovery alternatives with visible trade-offs)
5. **Who authorized the exact action?** (Scoped human approval of exact action hash and expiry)
6. **What actually happened afterward?** (Task readback and comparable post-action measurements)

When any prerequisite cannot be established, the system reports the precise reason and remains read-only rather than inventing facts or filling gaps with AI guesses.

### First End-to-End Release

The v1 release covers one customer site, one production line/product family, and one production exception workflow:

1. **Commission:** Clean install with all other WiseDo products absent. Bootstrap owner, authenticate via independent OIDC, configure site/line, working hours, and approved source grants.
2. **Discover and interview:** Conduct resumable interviews (owner, supervisor, operator) capturing objectives, routing, rates, and escalation.
3. **Review knowledge:** Present source excerpts and competing claims; authorized reviewers resolve contested facts to publish an immutable `ApprovedProcessSnapshotV1`.
4. **Ingest evidence:** Accept reviewed plans, good-unit deltas, machine events, and operator reports via mapped identities. Ingestion into inbox/outbox constitutes durable receipt, not semantic acceptance.
5. **Detect and investigate:** Evaluate epoch-aligned UTC 15-minute production windows. On a qualifying repeated shortfall (two adjacent windows each with deficit $\ge \max(5\text{ units}, 10\%)$), open one deduplicated `CaseV1`. Present pinned `PlantContextSnapshotV1`, hypotheses, and questions.
6. **Approve:** Present exact action, target asset, evidence revision, and 30-minute expiry to an authorized distinct human for cryptographic payload hash approval (`ApprovedActionV1`).
7. **Coordinate:** Atomically consume approval to dispatch one internal inspect/report task (`TaskV1`). Durably record acknowledgement, completion evidence, and authoritative readback.
8. **Observe and learn under review:** Measure subsequent comparable production windows, calculate outcome metrics, distinguish correlation from causation, and submit feedback candidates (`OutcomeFeedbackV1`) for reviewed human activation.

**Out of Scope (v1):** PLC/machine actuation, autonomous quality release, automated staff reassignment, live model weight retraining, unreviewed memory activation, and physical safety overrides.

---

## 2. Minimal Independent Architecture & Tech Stack

The architecture consists of a modular Go application, a Go background worker, PostgreSQL with forced row-level security (RLS), Temporal for durable workflow orchestration, a same-origin React/TypeScript UI, and independent OIDC authentication.

| Component               | Technology                                                   | Owned Responsibility                                                                                                                                   |
| :---------------------- | :----------------------------------------------------------- | :----------------------------------------------------------------------------------------------------------------------------------------------------- |
| **API & Backend**       | Go (1.24+)                                                   | Same-origin REST API, strict JSON decoding, pure rational arithmetic (`math/big`), domain validations, and security boundaries.                        |
| **Persistence & Audit** | PostgreSQL 16+ (Forced RLS)                                  | Canonical business ledger, multi-tenant composite keys, transactional outbox, immutable revisions, and auditable history.                              |
| **Orchestration**       | Temporal                                                     | Sole durable scheduler for background workflows (case lifecycles, timer escalations, retry policies). Does not replace the PostgreSQL business ledger. |
| **Identity & Policy**   | Independent OIDC                                             | Authenticated principal resolution, CSRF protection, tenant/site membership policy, and step-up reauthentication.                                      |
| **Role UI**             | React / TypeScript / Vite                                    | Accessible workspaces for Owner, Supervisor, and Operator; renders capability state without serving as the authorization authority.                    |
| **Edge Vision Core**    | Native C++20 / OpenCV (Future Vision, `src/wisedo_fixture_vision_cpp/`) | External edge optical inspection for assembly fixtures and jigs, publishing normalized inspection records to the Supervisor API.                       |
| **Model inference**     | OpenAI-compatible HTTP from Go                               | Local or private, address-pinned endpoints by default. Hosted Qwen (Singapore) only per site, opt-in and off by default, with a DPA and pseudonymisation (WIS-622, rule 08). Every output is validated in Go. |

---

## 3. Source and Integration Contracts

- **`SourceRecordV1`**: Identifies source/connector, tenant/site, mapped internal entity ID, external key/revision, event/recorded timestamps, units, quality, and provenance.
- **`ProcessClaimV1` / `ApprovedProcessSnapshotV1`**: Proposed statement or typed relationship, evidence class, source spans, valid time, reviewer decision, and immutable snapshot digest.
- **`PlantContextSnapshotV1`**: Pins exact source/graph/plan revisions for an order/line/window, input readiness, and calculation digests. Missing actuals produce `NOT_READY` rather than zero counts.
- **`CaseV1`**: Durable lifecycle (`detected` $\rightarrow$ `investigating` / `needs_input` $\rightarrow$ `proposed` $\rightarrow$ `awaiting_approval` $\rightarrow$ `executing` $\rightarrow$ `observing` $\rightarrow$ `resolved`), deduplicated per tenant/site/line/order/exception kind.
- **`RecoveryProposalV1`**: Separates observations from hypotheses, supporting and contrary evidence, unknowns, alternatives, exact canonical action hash, and expiry.
- **`ApprovedActionV1` / `TaskV1`**: Durable authorization granted by a distinct human, 30-minute expiry, pre-dispatch rechecks (source freshness, policy, action hash), and atomic consumption into one internal inspect/report task.
- **`OutcomeFeedbackV1`**: Links baseline, task readback, subsequent observation windows, and reviewer interpretation. Feedback is append-only; acceptance does not prove causation.

---

## 4. Edge Computer Vision: Future Vision (`wisedo_fixture_vision_cpp`) — C++ OpenCV

The repository houses the **Future Vision** native C++20/OpenCV implementation (`src/wisedo_fixture_vision_cpp/`, [ADR 0007](adr/video-embedding-docs/0007-wisedo-future-vision-architecture.md)), dedicated to visual inspection of manufacturing jigs, packaging lines, and assembly fixtures. Future Vision runs without a Python runtime.

### Native C++ OpenCV Runtime

To ensure deterministic execution on edge Industrial PCs (IPCs) and embedded ARM64 hardware:

1. **Frame-0 reference:** The first video frame is used as the golden reference for all later frames.
2. **Native execution:** Inspection, rendering, report generation, and video writing run in C++20/OpenCV.
3. **ARM64 output:** The executable uses an OpenCV backend and `mp4v` output suitable for ARM64 deployments.
4. **Direct Hardware Acquisition:** Integrates directly with GenICam, GigE Vision, and Linux V4L2 video capture pipelines.

### Integration Boundary with the AI Supervisor

- **External Observer Model:** Future Vision operates as an **external edge inspection sensor**, not within the Go Supervisor core runtime.
- **Normalized Event Ingestion:** Inspection summaries, component deviations, and missing part alerts are formatted and transmitted to the Supervisor Operational Intake API (`POST /v1/sites/{site}/operational-sources/{source}/batches`) as normalized `machine_state` or `operator_report` events under `SourceRecordV1`.
- **Zero Autonomous Actuation:** Future Vision detects and reports visual deviations; it does not issue autonomous PLC rejections or override human authorization.

---

## 5. Implementable Backlog & Delivery Checkpoints

Backlog items are tracked under Linear issues [WIS-146](https://linear.app/wisedo-technology/issue/WIS-146) through [WIS-281](https://linear.app/wisedo-technology/issue/WIS-281).
**Linear is the source of truth for status.** The column below was checked against Linear on
2026-11-02 (WIS-632) and will drift; check the issue before relying on it. "Done" is the Linear
state of the issue. It is not a claim of customer acceptance (rule 09).

| Issue | Deliverable | Linear status (2026-11-02) |
| :---- | :---------- | :------------------------- |
| [**WIS-146**](https://linear.app/wisedo-technology/issue/WIS-146) | Independent architecture, boundaries, and repository alignment | Done |
| [**WIS-290**](https://linear.app/wisedo-technology/issue/WIS-290) | Go application foundation, OIDC identity, PostgreSQL RLS migrations (`001`–`004`), bootstrap invite | Done |
| [**WIS-270**](https://linear.app/wisedo-technology/issue/WIS-270) | Durable process claims, conflict detection, reviewer resolutions, approved graph snapshot (`005`–`019`) | Done |
| [**WIS-271**](https://linear.app/wisedo-technology/issue/WIS-271) | Resumable role interviews, 3-question queues, interview capture & claim review UI | Done |
| [**WIS-272**](https://linear.app/wisedo-technology/issue/WIS-272) | Operational source registry (`020`), event batch intake (`021`), outbox progress fold (`022`), production arithmetic (`internal/production`), context snapshots, 2-window shortfall rule | Done |
| [**WIS-273**](https://linear.app/wisedo-technology/issue/WIS-273) | Durable `CaseV1` schema, deduplication triggers, Temporal case workflow orchestration | Done |
| [**WIS-274**](https://linear.app/wisedo-technology/issue/WIS-274) | Independent approval module (`ApprovedActionV1`), distinct human check, 30-min expiry, action hash | Done |
| [**WIS-275**](https://linear.app/wisedo-technology/issue/WIS-275) | Scoped tools, model gateway client, untrusted input boundaries, prompt injection testing | Done |
| [**WIS-276**](https://linear.app/wisedo-technology/issue/WIS-276) | Grounded `RecoveryProposalV1` generation, hypothesis separation, alternative responses | Done |
| [**WIS-277**](https://linear.app/wisedo-technology/issue/WIS-277) | Internal `TaskV1` creation, dispatch, assignment, acknowledgement, durable readback, reconciliation | Done |
| [**WIS-278**](https://linear.app/wisedo-technology/issue/WIS-278) | React role workspaces (Owner commissioning, Supervisor cases/approvals, Operator tasks) | Done |
| [**WIS-279**](https://linear.app/wisedo-technology/issue/WIS-279) | Append-only `OutcomeFeedbackV1`, post-action comparison, reviewed memory candidate activation | Done |
| [**WIS-280**](https://linear.app/wisedo-technology/issue/WIS-280) | Actual-agent evaluation harness (30/100 case suite, 6 adversarial, provider runs, refusal assertions) | Done |
| [**WIS-281**](https://linear.app/wisedo-technology/issue/WIS-281) | Independent clean install rehearsal, 10 shadow shifts, one-line customer pilot signoff | Done |

Open follow-up work for the onboarding agent, retrieval, scheduling link and advisor is tracked
under [WIS-610](https://linear.app/wisedo-technology/issue/WIS-610) (child issues WIS-611 to
WIS-632); see `docs/onboarding-agent/README.md` §0.

---

## 6. Verification and Quality Gates

A release-ready AI Supervisor requires:

- **Clean Installation Gate:** Boots and runs in an isolated environment with all other WiseDo systems absent.
- **Security & Multi-Tenancy Gate:** Zero unauthorized effects, zero cross-tenant disclosures, forced PostgreSQL RLS verified by automated tests.
- **Deterministic Calculation Gate:** Window calculations use exact rational arithmetic and half-even rounding; missing inputs fail closed to `NOT_READY`.
- **Human Authority Gate:** No agent-generated text or model output can approve an action or dispatch a task; consequential edits require new approval.
- **Edge Vision Gate:** Future Vision C++ engine produces verified inspection records without memory leaks or dropped frames under sustained 60 FPS operation.
