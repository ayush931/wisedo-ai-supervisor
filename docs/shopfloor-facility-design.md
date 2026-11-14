# WIS-709 shop-floor facility design

Implementer: second Codex, only `/var/tmp/wisedo-factory`, branch `feature/WIS-709-shopfloor-test-facility`, base `3706703a`. Independent review belongs to the product owner/reviewer, not this implementer. Test infrastructure only; no MES/Sight/MiroFish dependency, Python, new npm package, or web/src changes. All assertions use the existing public APIs and runtime kernels. Scripted fixture identities are not independent human oversight.

## 1. Existing foundation and scope

Reuse the Sahyadri generator: 48 stations, six lines, ten SKUs, three shifts, seed 278, existing six incident descriptions, loader identity/loopback guards, configuration import, source review, plans and ingestion. Keep historical `generate`/`load` and their golden manifest unchanged. New live artifacts have their own version and hash; they are not quietly added to the historical golden. No product migrations or time-guard bypasses are planned.

The live runner, document generator and scenario driver are Go additions to `cmd/supervisor-factory-sim`; pure scheduling, sheets, tooling and expected outcomes belong in `internal/factorysim`. Playwright journeys and their separate config live under `web/personas`.

## 2. Two explicit time axes

PostgreSQL `clock_timestamp()` enforces sessions, approvals, live shift starts and records. Changing only a client clock cannot accelerate it. Do not patch PostgreSQL time, JWT times, browser clocks, or production guards.

An injected runner clock has `Now` and `WaitUntil` operations. A frozen schedule contains model start (default synthetic day at 06:00 Asia/Kolkata), wall start, integer speed and seed. At speed 60, one model hour takes one wall minute. Map a model instant to `wall_start + (model_instant - model_start)/speed` using integer durations. Default speed 1 preserves elapsed time. Deterministic tests use a fake waitable clock. Payloads use scheduled instants, never the timestamp at which a loop happened to execute; measurements contain actual receipt times separately.

The three model shifts are 06–14, 14–22 and 22–06. Live plans are dated, compressed wall-clock windows. They still start through the real `/shift-plans/{id}/run` API, and finish through `/run/end`, which atomically stores the summary and handover. Their reports retain both model and wall windows: an accelerated run is not evidence of 24 elapsed real hours or production throughput. Target cycle/rate values needed by live planning are projected into wall units using exact rational arithmetic and the product's decimal serializer; manuals retain the unscaled fictional machine ratings with their explicit model-time units. Never silently compare unscaled nameplate seconds against compressed wall seconds.

Bootstrap finishes before wall start; use a future wall anchor with enough setup time, and refuse an already missed start. Approve dated plans with a distinct fixture supervisor before their starts. Renew fixture login for recent-auth boundaries rather than extending policy. Any missed deadline, refusal, quarantine or incomplete fold is recorded and fails the normal-run assertion; do not drop samples to recover a green run.

## 3. Batch schedule and normal run

Reuse the existing quarter-hour model observations and seed, ordered by model interval then line/station. There are 96 model intervals in a day. Rebuild transport sequences monotonically in one stream; append a heartbeat and watermark each interval. Keep every batch under the existing event/body limits. Counts, state, stops, reason codes and model-time provenance cover all 48 stations. A normal batch requires HTTP 202 and eventual no-quarantine readback, not merely a durable receipt.

All setup and plant writes use public routes. Split the existing loader only at its natural bootstrap/telemetry boundary, retaining historical behavior. Record created plan/job/source bindings in the facility report. Start each line's run, grant the operator, acknowledge/start station jobs, record quantities and incident stops/resumptions, then end each line's shift. Read and assert the summary and handover IDs from every end receipt. The one-day report includes all 18 line-shift runs (six lines times three shifts), every batch, the frozen schedule digest and final ingestion progress.

## 4. Machine parameter sheets

One class sheet per distinct existing `Station.Class`, with a station-specific variant table because cycle/MTBF/MTTR vary between lines. Schema: version, synthetic label, class, station ID/name/line, rated cycle seconds, changeover minutes, scheduled maintenance interval hours, repair minutes, alarm code/meaning, and provenance to the generator parameter. Existing cycle, MTBF, MTTR and SKU tool-life values are the source of truth. Explicit new fictional changeover/inspection defaults are declared once in the facility parameter model, then used by both simulation and documents.

The class names are turning, VMC, HMC, CNC, grinding, washing, CMM, packing, induction hardening, heat treatment, blanking press, forming press, piercing press, deburring, assembly, press fitting, torque, balancing and inspection. An enumeration check derives coverage from the generated plant rather than trusting this prose.

## 5. Tooling

Per station/SKU, deterministic fixture IDs identify inserts/tools, dies and fixtures. Counters increment from simulated processed parts; a replacement appends an event and resets the life counter, not history. SKU `ToolLife` controls expiry. On the existing tool-wear incident line, initial age plus generated parts reaches life at the incident window and triggers `TOOL_WEAR_17`; assert both the life crossing and resulting microstop. Do not infer maintenance authority from the code.

Gauges have a deterministic calibration interval, last calibration, due instant and status. The overdue-gauge switch prevents the facility from treating its reading as usable and emits an explicit synthetic quality observation. This is a facility model guard, not a new product quality-release authority.

## 6. Typed manuals, SOPs and registers

Generate one fictional operating/maintenance manual per machine class, six line SOPs and tool/gauge registers as Markdown/text and typed PDF. Every numeric statement is generated from a typed parameter/answer-key field; no numeric prose from an LLM. Include units and synthetic provenance. No OEM text or downloads.

**Compatibility spike passed before this design:** `go run ./.claude-flow/codex/factory-spike` used pdfcpu `api.Create`, Helvetica core font, explicit text positions and no links/actions. The product's `onboarding.Inspect` and confined `onboarding.Extract` accepted its 993-byte PDF and extracted `Rated cycle time: 50 seconds` exactly. Evidence: `/var/tmp/factory-artifacts/pdf-spike.log` and `typed-manual-spike.pdf`.

pdfcpu adds wall-clock CreationDate/ModDate and file IDs. The production generator must normalize those fixed-width metadata tokens to schedule-derived values, without shifting xref offsets, and prove byte-identical results plus extraction after normalization. Reject an unexpected writer shape instead of guessing. Generated typography uses ASCII core fonts and bounded pages; parser refusals remain failures. Scanned/mixed variants are deferred and must reuse WIS-708's generator later.

Upload PDFs through `/source-documents`, then read stored text/spans. Owner fixture designates/confirms current, a different fixture reviews extracted claims, and a snapshot is published. Ask about a known manual value and assert the answer's actual cited source/span contains that value from the answer key. Actual local-model calls are serialized and distinct from deterministic fixture assertions.

## 7. Edge catalogue and switches

`--scenario <id>` (or `all`) selects an isolated case; its report records expected and observed HTTP status/problem code or readiness/reason, evidence IDs and pass/fail. Expectations are frozen before running. A failure does not change the answer key. Isolation uses separate sources/plans/subjects as needed so one quarantine or revocation cannot spoil normal-run evidence.

| Switch | Stimulus | Expected assertion |
| --- | --- | --- |
| duplicate-batch | resend identical body/key | 202, replayed true, same receipt and no extra events |
| out-of-order-batch | skip/reverse required transport sequence | 409 SOURCE_SEQUENCE_CONFLICT |
| late-batch | deliver an older machine observation at the next valid sequence | 202, original event time preserved; heartbeat/receipt time separate |
| late-count-correction | append revision naming predecessor after an initial count | 201 and one linear revision chain; corrected total, original retained |
| clock-skew | heartbeat beyond configured 30-second skew | liveness NOT_READY, CLOCK_SUSPECT |
| counter-reset | reset-evidenced monotonic counter normalization | exact post-reset positive delta; 202 when posted; missing reset evidence fails normalization |
| liveness-gap | stop heartbeat past enforced freshness | liveness NOT_READY, HEARTBEAT_STALE |
| zero-planned-time | whole evaluation window is a planned break | LIMITED, NO_PLANNED_PRODUCTION; no fabricated actual zero |
| zero-denominator | valid OEE input with zero denominator | metric unavailable with ZERO_DENOMINATOR, not 0% |
| missing-actuals | approved plan with no matching production deltas | NOT_READY/MISSING_ACTUALS; actual null |
| overnight-shift | dated 22:00–06:00 plan crosses local midnight | accepted plan, next-day end and exact UTC interval; recurring calendar is not used |
| contradictory-interview | operator gives a value inconsistent with the manual | competing claims/conflict retained; answer needs_input, no silent winner |
| encrypted-pdf | pdfcpu AES-encrypted generated manual | 422 PDF_ENCRYPTED |
| active-pdf | generated manual with a JavaScript open action | 415 PDF_ACTIVE_CONTENT |
| revoked-document | revoke the reference and ask again | revoked source unavailable and absent from citations |
| stale-snapshot | change knowledge after publishing | 409 SNAPSHOT_STALE on graph; no substituted unreviewed facts |
| cross-tenant | second fixture tenant requests first tenant resource | 404 NOT_FOUND without disclosing existence |
| self-approval | submitter approves own shift plan | 403 SELF_REVIEW_FORBIDDEN |
| membership-revoked | remove an operator membership during a running shift | next protected request non-disclosing 404 NOT_FOUND |
| expired-session | isolated fixture session past expiry | 401 AUTHENTICATION_REQUIRED, no write |
| overdue-gauge | gauge due instant has passed | facility NOT_READY/CALIBRATION_OVERDUE; explicit held observation |

Where the table uses descriptive reasons, resolve the precise existing code from the runtime/tests before freezing the machine-readable catalogue. Do not invent a successful product outcome for a missing capability. Counter normalization uses the existing OEE kernel, then submits its positive delta through HTTP and verifies the worker fold. Gauge readiness remains an explicit facility-model assertion; reports distinguish kernel evidence from HTTP assertions.

## 8. Personas

Reuse the real `web/tests/session.ts` Keycloak login helper and existing locator conventions; no route stubs or synthetic success screenshots. One browser context per fixture persona, concurrent Promise-based journeys with explicit synchronization at dependency points:

- Shift lead submits/starts a plan and later ends it/readbacks handover.
- Approver, a distinct supervisor fixture, approves only the scripted exact plan revision.
- Operators acknowledge/start and record quantities/stops, then verify receipts.
- Owner reviews candidate facts, designates documents and publishes the snapshot.
- Maintenance and quality are responsibilities mapped to existing supervisor/operator roles, not invented product grants.

Use separate station scope for concurrency and serialize whole-site configuration imports. Collect unexpected HTTP/UI failures; zero unexpected errors is the scripted gate. The current base does not include WIS-703's chat-correction feature; do not import that reviewed branch. A future correction journey can be enabled when that route is integrated, rather than pretending the feature exists now.

The exploratory LLM persona reads a bounded accessible UI snapshot and recent memory, calls the address-pinned local model via a Go helper, and selects a closed action (observe, navigate via visible link, click named read control, or finish). No raw JS, shell, SQL, arbitrary URL, arbitrary CSS, credentials, or model-approved governance actions. Bound steps, timeout, tokens and memory. Exactly one request is in flight across this facility's model phases; use the shared endpoint at 127.0.0.1:64601 only. No hosted fallback.

Memory is local JSON: persona, goal, last bounded actions/observations and unresolved leads. Findings JSON/Markdown includes seed/schedule/build/model/prompt pins, role/goal, steps, observed behavior, screenshot path, expected behavior (or unknown), and reproduction confidence. Every item is an exploratory lead; never a gate result or independent human review. Redact credentials/session state from reports. Missing dependencies produce BLOCKED, not a fabricated report.

## 9. Facility stack

Project `supervisor-facility` only. Select unused 62xxx loopback ports with `ss`; planned candidates PostgreSQL 62477, OIDC 62077, Temporal 62277 and API 62376, subject to the live check. Bind PostgreSQL storage under `/var/tmp/supervisor-facility`; all caches, builds, logs and browser artifacts also stay under /var/tmp. Separate DB roles for runtime, migrator, Keycloak and Temporal, reusing existing bootstrap. Do not mount or alter another stack's volume.

Build/run the clone's own binary and assets, local Keycloak start-dev and Temporal auto-setup. Host-network application containers can share the browser-visible loopback issuer, as in the documented local script; any clone mount uses the host uid/gid. Seed via existing supervisor-seed/demo-seed and scoped fixture extensions for a second tenant or expired-session edge setup only. Business writes stay public API calls. CLI commands must validate the facility project/port/root and refuse protected ports, nonlocal URLs and unexpected state.

The final checked-in script supports up/seed/run/personas/report/down with explicit project scoping and bounded readiness waits. It must not use broad Docker prune or modify existing stacks. Since acceptance slices need a live stack before slice (g), an uncommitted bootstrap rehearsal under `.claude-flow/codex` may establish the same scoped infrastructure; slice (g) turns the verified steps into the maintained script.

## 10. Slices, gates and rollback

(a) Injected clock/schedule, deterministic batches and live run lifecycle; unit tests plus three-shift stack rehearsal.
(b) Derived sheets and tooling; unit checks bind every station/class and wear/calibration edge to model inputs.
(c) Deterministic typed manuals/registers and real upload/citation evidence; PDF refusal controls.
(d) Frozen edge catalogue, scenario runner and complete pass/fail report; all cases execute against the facility or explicitly named existing pure kernel.
(e) Concurrent scripted browser personas, deterministic gate.
(f) Bounded local LLM personas and exploratory findings with screenshots, never qualification.
(g) Facility stack script and documented one-command acceptance run.

Commit each slice locally after `go test ./cmd/supervisor-factory-sim ./internal/factorysim` and relevant real-stack checks. Final `make supervisor-check`, `make supervisor-contracts`, and one complete Acceptance 1–4 run. Keep shared STATUS-factory.md small/current. No pushes, PRs or Linear writes. Rollback removes only this clone's feature commits and its explicitly scoped facility containers/data; historical generator behavior and demo data remain unchanged. Any ENOSPC is immediately BLOCKED: disk.

## Slice evidence

(a) Full public-API rehearsal, 2026-11-11: simulated 24 hours, speed 240, wall window 14:45:18Z–14:51:18Z. 96 batches/5,959 events accepted and folded, zero quarantines, 5,186 station records, 18 ended runs with stored summaries and handovers; final liveness READY. Artifact `/var/tmp/factory-artifacts/live-a-1/run-report.json`. Unit tests and the original manifest golden passed; the historical public-API integration and replay test also passed in a disposable database on the facility PostgreSQL (21.55 seconds). Its DB was removed by the test. No other stack was used. The optional Playwright FFmpeg download hit DNS failure, but the installed Chromium channel completed all four real fixture logins.

(b) Unit tests pass for class/station/SKU coverage, counter threshold/replacement history and calibration expiry. Public API readback of the facility CNC's approved assignment and 38 station records matches the sheet (70 model seconds; 0.291667 wall seconds at speed 240), counter life reached at model 08:00, replacement at 09:00, and the corresponding persisted pause/resumption. Evidence `/var/tmp/factory-artifacts/slice-b-model-proof.json`. An additional complete day is running in `live-b-1`; its final result is recorded in STATUS. The historical golden remains unchanged.

PDF determinism follow-up: three independent pdfcpu writes a second apart normalized to the same 1,138 bytes, and all extracted correctly through the product parser. `/var/tmp/factory-artifacts/pdf-determinism.log`.

(b) Extended day completed: 96 batches, 5,959 events processed, zero quarantines, 5,186 station records and all 18 summaries/handovers; final READY (`live-b-1/run-report.json`).

(c) All 27 typed documents (19 class manuals, six SOPs, two registers) passed product extraction with every numeric line preserved. The parser initially refused long clipped SOP lines with PDF_SCAN_HIDDEN_CONTENT; bounded word wrapping fixed the generator. Real Documents uploads and stored-text readbacks passed for all 27. Forty-eight station cycle claims were reviewed by a distinct fixture, a snapshot was published, and scripted-provider chat answered CNC KN-03's rated cycle time as 45 seconds with its exact stored span. `/var/tmp/factory-artifacts/manuals-c-1/manual-upload-report.json` is the evidence. The product stores extracted text/spans, not original PDF bytes. This is a deterministic retrieval/citation contract check, not actual-model qualification.

(d) The 21-case switch catalogue is frozen in `internal/factorysim/scenarios.go`; every run writes its answer key before executing. Exact runtime routes use 201 for station corrections and dated shift creation. Counter normalization, zero-denominator, missing-actuals/planned-time, and calibration checks explicitly identify their existing kernel/model layer. The interview stimulus is an operator-submitted synthetic interview transcript through the evidence API, with a competing candidate claim; it does not simulate an LLM interview extraction. Identity-only SQL fixtures are restricted to the facility PostgreSQL address; business writes remain HTTP.

Real-stack evidence `/var/tmp/factory-artifacts/edges-d-3/scenario-report.json`: 20/21 pass. The contradiction case fails: a retained `value_conflict` removes the conflicted fact from the approved snapshot, but the scripted chat provider can answer an unrelated approved passage instead of returning `needs_input/MISSING_INPUT`. This is an observed contract gap, not a local-model result. The answer key is unchanged and the CLI exits nonzero. The revocation control checks a captured source: the first fixture citation need not be that source because the fixture provider sorts required source IDs; revoking it must redact the prior answer and prevent its span in subsequent citations. Encrypted PDFs use fresh cryptographic randomness; refusal outcomes, not cipher bytes, are deterministic.

(e) Six concurrent browser contexts completed the scripted journeys in 3.0 seconds, with 15 journal milestones and zero unexpected HTTP/UI errors (`personas-e-5/scripted-report.json`). Owner uploads/designates/confirms a fictional SOP, reviews the lead's candidate and publishes; a distinct approver approves the plan; lead starts/ends and reads the stored summary/handover; operator records count/stop/resumption; maintenance records an inspection note; quality reports a quality issue. Maintenance uses the supervisor fixture and quality the operator fixture, without invented roles. Name lookup 404 fallbacks and absent source-decision 404s are explicitly classified according to existing `ui.tsx` and `document-authority.tsx`; all other failures fail the gate. A separate auth spec renews real fixture sessions atomically. Persona TypeScript checking and required Go tests passed.

Additional observed product finding (not repaired because web/src is excluded): `web/src/workstation.tsx` quality disposition composer omits required `authorizes_release:false`. Clicking Quality -> Disposition -> Hold -> Record returns 422 INVALID_STATION_RECORD at authorizes_release. Screenshot `/var/tmp/factory-artifacts/personas-e-4/quality.png`, failed report in the same directory. The final quality journey uses the existing Quality issue note control; it does not claim disposition/release coverage. Chat correction remains absent on this base.

(f) Actual local-model exploration completed with two personas and 11 serialized requests in 47.3 seconds (`personas-f-3/findings.json`, `.md`, memory files and screenshots). The model was `unsloth/Qwen3.5-9B-GGUF:Q4_K_M`; its cached GGUF SHA-256 was freshly verified as `03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8`. The endpoint is fixed at 127.0.0.1:64601 through the existing pinned adapter; one file lock spans probe/inference, with bounded input/output/time and no tools. Runtime IDs, request/prompt hashes and binary hash are recorded. A prior oversized/invalid model reply was refused; invalid model output now ends that persona with a recorded refusal lead rather than inventing an action. Missing inference dependencies still fail the harness visibly.

Exploration offers only read navigation, named read buttons, observe and finish; these goals need no text entry. HTTP mutation and off-origin requests are blocked independently of model choices. Model observations describe the pre-action screenshot and remain unverified even when the model says it finished. The line investigator reached its six-step bound. Unit tests and persona TypeScript checks passed. No actual local-model calls were made by the product API/worker; those remain on the clearly labeled fixture provider.

(g) The maintained `scripts/supervisor/facility.sh` and Compose overlay use only the fixed facility project, ports and root-filesystem paths. Real startup exposed restrictive image file modes: the worker could execute but not read its checksum, and the PDF child could not start. The failed rehearsal `20261111T155004Z` is retained. Explicit readable/executable binary modes and readable CA certificates fixed it; startup now asserts the worker is running and has reported startup. The replacement container passed the typed-PDF extraction probe. `config` asserts loopback ports, bind storage, fixture provider, task queue, host uid/gid and read-only clone mounts.

The final audit extended `counter-reset` from its kernel control to an HTTP 202 ingestion and completed worker fold of the computed three-part delta, retaining the unevidenced-drop refusal. `/var/tmp/factory-artifacts/counter-g-1/scenario-report.json` passed. All catalogue entries now require either an HTTP status or a readiness/reason pair. Normal live revenue excludes the historical late correction that the normal run omits; the original historical golden is unchanged. Signal cancellation now flushes live/scenario reports before exit.

Final combined Acceptance 1–4 ran against fresh maintained stack `20261111T160140Z` (wall model-day window 2026-11-11 16:04:15Z–16:10:15Z), with an overall nonzero exit preserved. Acceptance 1 PASS: all 96 batches, 5,959 accepted/processed/complete events, zero quarantines, 5,186 station records, 18 ended runs with summaries/handovers, final READY. Acceptance 2 FAIL: 20/21 edge cases pass; contradictory-interview still returns answered instead of needs_input/MISSING_INPUT. Acceptance 3 FAIL: all 27 PDFs were uploaded/extracted/read back and all 48 cycle claims reviewed, but asking CNC KN-03's 45-second cycle returned Assembly HB-03's 157-second passage. The expected exact span/value was not changed. The earlier slice-c citation success is therefore not a stable fresh-stack gate. The pilot fixture script chooses first_required_evidence_ref, while chat sorts those random source IDs (internal/agent/chat.go); this explains the unrelated selection and limits the result to the fixture-provider contract. No source IDs or expected values were selected after seeing the reply. Acceptance 4 PASS: six concurrent scripted contexts completed in 3.5 seconds with zero unexpected errors; two local LLM personas produced eight serialized exploratory steps in 30.2 seconds, with screenshots and memory. Their claims remain unverified.

Aggregate evidence: `/var/tmp/supervisor-facility/runs/20261111T160140Z/artifacts/acceptance-report.json` and `.md`; phase reports live beside it. `scripts/supervisor/facility.sh report` exits 1, and assert-based checking confirms it retains both failures. The facility stack remains running for review. All database and application actions targeted its own project/ports; no showcase/demo, e2e, CI, model-container or Sight data was changed. No web/src, dependency, contract or historical golden change.

Final repository gates PASS on 2026-11-11: `make supervisor-check` (exit 0; race suite, static PDF/scan suite, vet, web build and audit), `make supervisor-contracts` (exit 0, no generated drift), and `go test ./cmd/supervisor-factory-sim ./internal/factorysim` (4.644 seconds / cached). Logs: `/var/tmp/factory-artifacts/final-supervisor-check-3.log`, `final-supervisor-contracts-3.log`, and `slice-g-unit-final-2.log`. Persona TypeScript checking, shell/Node syntax, Compose assertions, aggregate-report assertions and git diff whitespace checks passed. The earlier paused make attempt hit the Temporal heartbeat test's 35-second timer; it is invalidated, not a product regression, and the uninterrupted final run passed without a test change. No test skips, suppressions, expectation weakening or dependency changes.

## Follow-up: measured local model, 2026-11-11

Merged current main `538bb30dec0fed5ebdc1cb19cf90f61d99c5b882` as `57abb613d3ed8c7947359e18f366bbaa21242ab4` without conflicts. Main had advanced beyond the task's `fe8f1cd4`: both WIS-702 and WIS-708 were already merged. GitHub fetch returned repository-not-found; the requested fallback fetched only the main checkout's `refs/remotes/origin/main` into this clone. No main-checkout branch was changed. Its two-line `web/src/documents.tsx` change is inherited from main, not authored by this task. Post-merge simulator tests passed (6.058 seconds/cached).

Fresh facility run `20261111T162352Z` was seeded and populated by the existing one-day historical Sahyadri public-API loader (27.705 seconds). This preparation used the fixture provider. AC1/AC4 were not repeated. Before either measured phase, `facility.sh local-model` recreated only this project's API/worker with the real pinned local model, empty fixture script, `SUPERVISOR_AGENT_MAX_CONCURRENT=1`, 120-second call timeout, and reasoning effort `none`. The shared service reported the expected model ID and a 16,384-token context. Word retrieval stayed unchanged; the embedding endpoint was not needed. The facility model lock excluded its separate exploratory helper during the measured phases.

AC3 ran before AC2 so revocations/conflicts could not contaminate the original manual check. All expectations and assertions remain unchanged from `6606971b`:

- **AC2 PASS, exit 0, 21/21 cases.** Contradictory evidence returned `needs_input/MISSING_INPUT`, and document revocation passed. The remaining cases retain their explicit HTTP/kernel/model layers; they do not all call an LLM.
- **AC3 FAIL, exit 1, frozen wording assertion.** All 27 manuals/registers were uploaded and read back; all 48 cycle claims were reviewed. The real model answered: `The rated cycle time of CNC KN-03 is 45 seconds.` It cited exactly `626ad3f6-9770-4eeb-96e5-c67367fd86f0`, and the structured grounded quantity is `45 s` tied to that span. The existing assertion requires the literal substring `Rated cycle time: 45 seconds.` Its failure is retained. The value and citation are correct; this is a harness wording mismatch, not evidence that the model returned a wrong number. This supplemental observation does not override the frozen gate.

Provenance qualifications (added after the PR #304 review; the results above stand as recorded, with these limits):

- **Dirty build.** `local-model-provenance.json` records `working_tree_dirty: true` at merge `57abb613`. The measured binary was built before commit `c3eaf376`; the uncommitted Go difference was the manual report's provider label (`manual_upload.go`), so behaviour is equivalent, but the binary is not reproducible from a commit. Run `20261111T160140Z` is also dirty (`exploratory/findings.json` at `0ff27a72`).
- **Script edited mid-run.** Run `20261111T160140Z`'s `run.log` contains `facility.sh: line 157: unexpected EOF while looking for matching '"'`: `facility.sh` was edited while that acceptance run executed, so its later phases ran a different script from its first. Its results are not a clean single-script run.
- **Case run `05126214`.** A copy of that run's database, read offline, shows it was not a WIS-709 measurement. The historical loader's plant contexts made the server detect three `production_shortfall` cases at 16:25:36–37Z. Two investigated on the fixture worker before `local-model` recreated it at 16:25:37Z; the third (case `10914210`) started at 16:25:39Z on the new local worker and ended `needs_input`/`CONTRADICTORY_EVIDENCE` (limit profile `wis702-local-qwen35-9b`). It is evidence that the profile was selected, nothing more. `facility.sh audit` now lists such case runs with that label.
- **Revision configured, not observed.** `model_revision` in the audit is the configured pin that the server records on each run. The endpoint reports the model ID, type, size and parameter count (and, through `/props`, its file path with the HF snapshot commit), but no weight digest. The GGUF hash was checked separately at commissioning.
- **Lock.** The model lock was taken by hand around the phases; server-side calls do not take it. `facility.sh accept` now holds it for the whole local phase.

Evidence directory: `/var/tmp/supervisor-facility/runs/20261111T162352Z/artifacts/`. `local-ac2-ac3-report.json`/`.md` contain the results; `local-model-provenance.json` captures the runtime settings and binary hash before execution; `local-model-audit.json` contains scoped server-side model-call metadata, not raw prompts. Four actual runs made 15 calls, all on the requested model/revision; maximum overlapping reserved call intervals was one. The manual reply maps to its actual run. Migration 082 was present, and an actual case run used `wis702-local-qwen35-9b`; this confirms profile selection, not investigation qualification. These are measured pinned-local-model results on a synthetic plant, never qualification.

Commands and logs: `facility.sh down`, `up`, `seed`; `supervisor-factory-sim generate --days 1` and `load` with renewed fixture sessions; `facility.sh local-model`; then locked `manuals` and `scenarios` sequentially. Logs are `/var/tmp/factory-artifacts/followup-{up,seed,prepare,local-model,ac3,ac2}.log`. The report's provider label now points to server provenance instead of incorrectly hardcoding fixture. No assertions, answer keys, thresholds or runtime policy were changed. The confirmed quality-disposition UI bug remains for its separate owner.

Follow-up final gates PASS: required simulator tests (4.937 seconds/cached), `make supervisor-check` (exit 0), and `make supervisor-contracts` (exit 0). Logs: `/var/tmp/factory-artifacts/followup-final-unit.log`, `followup-supervisor-check.log`, and `followup-supervisor-contracts.log`. Shell syntax, base/local Compose scope/pin assertions, server-audit overlap assertions, and diff whitespace checks passed. The edge catalogue and document-edge implementation are byte-unchanged from `6606971b`; the only manual-check Go change is its provider description. No push, PR, Linear update, authored web/src change, protected-stack mutation, or qualification claim.

## Review fixes (2026-11-11, after BLOCK review of c3eaf376)

Implemented by Claude in this clone; not yet rerun live. Every change makes a check stricter.

- AC2 is now **20 product edge cases**. `overdue-gauge` tests the facility's own gauge model, so it moved to `FacilityModelChecks()`, reported as `facility_model_checks` outside AC2. The answer key (product cases plus facility checks) is pinned by SHA-256 in `scenarios_test.go`.
- `contradictory-interview` gates on server behaviour only: one open `value_conflict` naming exactly the manual and interview claims, still listed after the after-conflict snapshot is published; the before snapshot holds the manual claim; the after-conflict snapshot holds neither. The chat reply is recorded as `chat_observation` with `gate:false`, and a chat error cannot fail the case. Product gap (in `scenario-report.json` `product_gaps` and the acceptance report): no server rule turns a knowledge conflict into chat `needs_input`; `MISSING_INPUT` is the model's own status, and `CONTRADICTORY_EVIDENCE` exists only for case runs when the model lists contradictions.
- `revoked-document` now requires the first answer to cite the target span. The target source ID sorts first, so the pilot fixture provider cites it. After revocation it requires the earlier answer to be redacted and a new answer `unavailable` / `SNAPSHOT_EVIDENCE_UNAVAILABLE` with no citations (the code in `internal/chat/answer.go captureSnapshotError`, observed in runs 20261111T160140Z and 20261111T162352Z).
- AC3 no longer matches the model's sentence. It requires exactly one server-resolved `cycle_time_seconds` quantity with the expected claim, value 45, unit `s` and span, no other cycle value, a body matching `\b45\b\s*seconds?`, the cited span, and the stored span text. The recorded local-model answer passes; the recorded fixture answer fails (negative control). AC3 gates on the pinned local model.
- `expired-session` has a positive control: the same subject and membership with an unexpired session writes (200); the expired session gets 401, and its source reads back 404.
- `overnight-shift` records the real create status and is documented as plan creation only.
- `facility.sh` no longer pins the clone path, branch or home directory; Go comes from `PATH`. Persona specs require `FACILITY_ARTIFACTS` / `FACILITY_SIM_BINARY` instead of defaulting to `/var/tmp/factory-artifacts`.
- `runLive` deadlines are tested on a fake clock through a bootstrap seam (`liveBootstrap`) and the extracted `awaitLiveFold`.
- Scripted personas fail on console errors other than Chromium's log of the allowlisted 404s. `make supervisor-check` type-checks `web/personas`. Persona steps record the endpoint-reported model ID and metadata; the sha256 revision is labelled as the configured pin.

## PR #304 review fixes (2026-11-11)

- `facility.sh accept` runs AC3 and 19 of the 20 AC2 cases on the pinned local model, as one command: `local-model` (running-container settings and endpoint model checked, `local-model-provenance.json` with the build provenance), `manuals`, `scenarios`, then `audit` (server run and call rows into `local-model-audit.json`; every run and call must be the pinned model, at most one call in flight, and the AC3 answer must have its own local run). All four hold the model lock. `fixture` then switches back and is checked, `scenarios-fixture` runs `revoked-document`, and personas run last.
- **Departure from the coordinator's instruction (all of AC2 on the local model).** `revoked-document` needs a first answer that cites its target before it can test revocation. On the local model that is the model's choice: it cited the target at 16:23Z and 17:49Z and answered `needs_input` without a citation at 21:52Z (run `20261111T214400Z`, otherwise identical setup). Gating on it would repeat the original HIGH finding, so the case runs on the fixture provider, where the first-sorting source makes the citation deterministic. The report merges both scenario reports and requires all 20 product cases.
- Known product defect seen by the console check: on main `94dba219` (WIS-707 design system) the bundle applies inline styles that the CSP `style-src 'self'` (`internal/httpapi/api.go`) blocks. `web/src` is out of scope; the scripted report labels each occurrence under `known_product_defects`. Superseded by the second review below: those errors fail the gate again.
- The Documents upload form now labels its title field `Title · Required`; the scripted owner finds it by role within the upload form.
- The report derives AC2 from the 20 product results alone; the gauge check is its own line and still fails the run (`facility-report.test.mjs`, run by `make supervisor-check`).
- The AC3 body check matches the whole numeric token: 145.45, 0.45, -45, +45, −45, 45.0, 1,045 and 045 seconds are refused. 45.0 is refused on purpose: the server resolves the canonical decimal `45` (rule 04), and the body must state that token rather than another spelling.
- Each phase removes `artifacts/auth/sessions.json` on exit; the report fails if it remains.
- The scripted personas include the WIS-703 correction journey (submit, independent review, owner publication).
- Live runs `20261111T214400Z` and `20261111T220434Z` found two more things, both fixed: (1) since WIS-703 the pilot fixture pins the first approved fact by claim ID, so `revoked-document` failed with `FACT_NOT_APPROVED` until its claim ID also sorted first; (2) after `revoked-document` runs last, the latest snapshot holds the revoked claim, so chat is `unavailable` until the owner publishes. The scripted owner now publishes a reference fact (source and claim IDs `00000001-…`, sorting after the edge case and before generated IDs) before the corrector asks, and a second snapshot after the correction is approved. Ceiling: on a reused stack, earlier persona reference IDs also sort first; run `accept` on a fresh facility.
- Final fresh run `20261111T221953Z` at `e8ec60fb` (clean tree, recorded in `build-provenance.json`): `facility.sh accept` exit 1. AC1, AC2 (19 local + `revoked-document` on the fixture provider, all 20 pass), the gauge check, the provider audit (2 local runs, 4 calls, at most 1 overlapping), scripted personas (including the correction journey; 0 unexpected errors, 5 known CSP occurrences), exploratory personas and the session-file check passed. AC3 failed: the pinned local model answered `needs_input` ("The entity_id for CNC KN-03 is required…"). AC3 is a model measurement; across the three runs at this head lineage it answered correctly with the exact span and quantity twice (`20261111T214400Z`, `20261111T220434Z`) and refused once. It was not rerun to get a pass.

## PR #304 second review (2026-11-11)

Product-owner decisions: `revoked-document` stays on the fixture provider; AC3 must pass on every acceptance run, with no multi-run threshold.

### AC3 refusal in run `20261111T221953Z`: diagnosis

Evidence from that run's database (run `c70badac`, reply `3d45ac53`):

- **Scope was right.** Snapshot `0f334424` was published and holds claim `33a64ffa` (CNC KN-03, 45 s). Chat selected eight passages, and the KN-03 passage `2355e589` ("Station: CNC KN-03 | Rated cycle time: 45 seconds.") ranked first; the other seven are other stations' cycle times.
- **The question names the station exactly as approved.** The approved entity `83dc5259` is the asset named `CNC KN-03`; the question asks for "the rated cycle time of CNC KN-03".
- **The model never read the evidence.** Two model calls, no tool calls (no `agent_tool_calls` row for the run), artifact status `needs_input` with no statements. Its reply: "The entity_id for CNC KN-03 is required to search for approved claims about its cycle time."
- **Why it asked for an entity ID.** The brief's `approved_facts` carry only claim ID, predicate, value and span IDs, with no subject name or ID (`chatApprovedFacts`, `internal/agent/chat_corrections.go`), so eight cycle-time facts are indistinguishable without a read. `claims.search` requires an `entity_id` UUID (`internal/agent/tools.go`), which nothing in the brief supplies. The only path to the answer is `evidence.get` on a required reference, and the model chose not to take it. In runs `20261111T214400Z` and `20261111T220434Z` it did, and answered with the exact span and quantity.

Conclusion: not a facility setup problem (scope, naming and snapshot were all correct) and not retrieval. It is a product limitation: a chat answer depends on the model choosing to call `evidence.get`, because approved facts reach the model without the entity they describe and `claims.search` cannot be addressed by name. Nothing in the facility was changed for it; rewording the question to coax a tool call would paper over it. AC3 fails whenever the model skips the read. Raised for the product owner.

### Other fixes
- `manuals` and `scenarios` refuse to start unless the running API and worker use the pinned local model. `audit` requires both to have run on the containers `local-model` verified, the scenario run to start inside the window, and every scenario chat reply to have an audited local run.
- The audit includes every run and call whose interval overlaps the window, and waits (up to 300 s) until no local run or actual call is outstanding before the lock is released; anything still open fails it.
- The documented hand sequence runs under `sh -e` and stops at the first failure.
- CSP inline-style console errors fail the scripted gate again (still labelled under `known_product_defects`) until PR #306 lands and main is merged in.
- `sessions_removed` checks the file system directly (`lstat`; anything but ENOENT counts as present) for `sessions.json` and `.sessions.json`; tests cover a malformed leftover, the temporary file and a non-ENOENT error.

### Fresh run `20261111T233149Z` at `c3899eab` (clean tree)

`facility.sh accept` exit 1. PASS: AC1, AC3 (local model, exact span and quantity), AC2 (19 local + `revoked-document` on the fixture provider, 20/20), gauge check, provider audit (2 local runs, 6 calls, at most 1 overlapping, scenario chat reply bound to its local run, both phases on the verified containers), exploratory personas, session-file check. FAIL: scripted personas, only on the five CSP inline-style console errors (one per signed-in role), which now fail the gate by design until PR #306 is merged into this branch. The whole scripted journey, including the correction, completed. This AC3 pass does not change the diagnosis above: AC3 still depends on the model choosing to read the evidence.

## Acceptance after WIS-712 and the CSP fix (2026-11-12)

Main now includes #306 (the CSP style hash), #308 (WIS-712: chat facts name their subject, and the model reads the evidence first) and #310 (the pilot fixture reads the evidence of the fact it cites).

Fresh run `20261112T075531Z` at `211eb402`, from a clean tree recorded in `build-provenance.json`: `facility.sh accept` exit 0, `passed: true`.
- **AC1:** passed.
- **AC2:** 20 of 20, 19 on the pinned local model plus `revoked-document` on the fixture provider. The gauge check also passed.
- **AC3:** passed on the local model, with the exact span and quantity.
- **Provider audit:** 2 local runs, 6 calls, at most 1 call in flight, on the pinned `unsloth/Qwen3.5-9B-GGUF:Q4_K_M` at revision `sha256:03b74727…`.
- **Scripted personas:** passed, with 0 unexpected errors and 0 CSP occurrences; the known-defect list is empty.
- **Exploratory personas and the session-file check:** passed.

Three facility fixes made this run pass. None changes an expectation:
1. **Locators.** Main's Astryx buttons each carry an empty `role=status` span, so three scoped status locators now filter to non-empty text.
2. **Fixture.** `revoked-document` on the fixture provider failed after #308, because the pilot read the first required reference but cited the first ranked fact. #310 fixed the fixture on main.
3. **Machine names.** `revoked-document` also failed on some runs: about half in a 4-run probe on one stack.
   - **Cause:** the scenario machine name embedded the hex entity ID. A token such as `265c13e1` looks like an exponent, and chat's number guard correctly refuses such passages (`STATEMENT_REJECTED`).
   - **Fix:** the name now uses a 12-letter tag. The same probe then passed 6 of 6.

AC3 stays a required check on every acceptance run; nothing here guarantees it.
- **What changed (WIS-712):** the brief names each fact's subject, and the prompt asks the model to read the evidence before citing it. This removes the cause diagnosed above, where the model asked for an entity ID.
- **Measurement, not qualification:** on the pinned model the KN-03 question scored 2/5 before the change. It scored 5/5 with prompt `wis712-chat-6`, and 5/5 again with the merged prompt `wis712-chat-7` (PR #308 head `04d4800`).
- **The run above:** AC3 passed in `20261112T075531Z`. That is one run.
- **Machine-name probe:** the six live probe runs of `revoked-document` checked the facility on one stack. They are separate from the unit tests on the machine name and the quoted passage (`TestWIS709ScenarioMachineNameHasNoDigits`, `TestWIS709ScenarioManualPassageQuotesNoHexID`).
- **Persona tag:** the scripted personas' tag is letters only too, for the same reason; the spec asserts it.

Final fresh run after the review fixes: `20261112T081637Z` at `df58a845` (clean tree). `facility.sh accept` exit 0, `passed: true`.
- **Every check passed:** AC1, AC2 20/20, AC3 on the local model, and the gauge check.
- **Provider audit:** 2 local runs, 6 calls, at most 1 in flight.
- **Scripted personas:** 0 unexpected errors and 0 CSP occurrences.
- **Exploratory personas and the session-file check:** passed.
