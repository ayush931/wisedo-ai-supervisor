# WIS-710 useful local-model investigations — 2026-11-11

**Synthetic measurements of the Go/model pipeline; not WIS-280 qualification or model-only accuracy.**

The review-follow-up replay passes all eight case checks: four cited inspection proposals and four
usable missing-input outputs. **Three missing-input questions are written by Go from server state.**
The fourth is model-written text reporting a conflict explicitly stated in its source fixture.
These are not four model question-writing successes. No run approved or executed a task.

[Per-case evidence](useful-investigations-20261111.json) retains the baseline, initial v13 and
review-follow-up rows, their model/code/prompt/tool pins, case/run IDs, outputs, timing, failures
and question authorship. [Frozen labels](useful-investigations.v1.json), SHA256:
`94e98f63975a3dc50979792e1e8c6c28d737c2a7b21ad79101d463ed2cf6521b`.

## What the missing-input checks measure

| Case | Who writes the persisted question? | What the model contributes |
| --- | --- | --- |
| dev-no-stop | Go, from the server-known `machine_fault` gap | Chooses needs_input/refused/failed; the grammar forces the missing key. |
| dev-no-assignee | Go, from the server-known `confirmed_assignee` gap | Same constrained status choice; Go supplies the exact sentence and the supervisor role. |
| heldout-material-stop | Go, from the server-known `machine_fault` gap | Same constrained status choice; no model-authored missing-input sentence. |
| heldout-conflicting-log | Model-reported conflict text | Reports the conflict already explicitly planted in the signed-log source; this is not evidence of independent conflict discovery. |

At v13, every Go-built question had a `Supervisor: ` prefix, which alone satisfied the human-role
half of the check. That prefix has been removed. The rerun also strips it before the role-word
check, so it cannot earn credit if it reappears. The remaining role-word regex is a limited text
check; it does not establish independent assignment of responsibility. The source's explicit
conflict sentence can satisfy the conflict-label and human-role checks.

At the first blind v9 run, the model chose two exact gap keys that Go expanded into questions;
`dev-no-stop` used model text without a human role and passed that half only because of the prefix.
The v9 conflict response also contained duplicated/off-target questions. The initial v13 and
intermediate counts remain historical pipeline assertions, not retrospective model successes.

## Before / after review fixes

Each partition contains two sufficient and two insufficient cases. “Usable missing-input output”
checks the expected gap/conflict in a persisted required question, a human-role word, and no task.
Proposal checks require a cited inspect/report task and exact observations; Go independently
validates identity, evidence, quantities, assignee, versions and scope. Labels were frozen;
harness assertions were not hashed at baseline and have been strengthened, as disclosed below.

| Pipeline measurement | Dev baseline | Dev after review | Heldout baseline | Heldout after review |
| --- | ---: | ---: | ---: | ---: |
| Correct proposals / sufficient cases | 0/2 | 2/2 | 0/2 | 2/2 |
| Incorrect proposals emitted | 0 | 0 | 0 | 0 |
| Usable missing-input outputs / insufficient cases | 0/2 | 2/2 (Go) | 0/2 | 2/2 (one Go, one model-reported source conflict) |
| Case checks passed | 0/4 | 4/4 | 0/4 | 4/4 |
| Terminal SCHEMA_INVALID / cases | 0/4 | 0/4 | 0/4 | 0/4 |
| Provider schema errors / calls | 0/12 | 0/12 | 0/12 | 0/12 |
| Mean latency, seconds | 14.586 | 15.197 | 14.739 | 16.025 |
| Median latency, seconds | 17.175 | 13.398 | 16.841 | 14.692 |

Overall mean: **14.662 s baseline → 15.611 s after review**; median 16.978 → 13.573 s.
Final range 5.947–28.719 s. Initial v13 mean was 15.881 s; its full rows remain under `after_v13`.
These shared-host observations are not a speed benchmark. Baseline: zero proposals, six needs_input
(including three sufficient cases), two refusals, and no usable missing-input artifact on these
labels. Final: four proposals and four usable missing-input outputs, with no refusals. Final prose
was inspected by the implementer; this rerun has no independent model-output adjudication.

| Case | Baseline | Review-follow-up output | Baseline / final ms |
| --- | --- | --- | ---: |
| dev-unexplained-stop | required root cause before inspecting | cited inspection | 19421 / 28043 |
| dev-earlier-stop | required root cause before inspecting | cited inspection | 17261 / 20823 |
| dev-no-stop | no retained question | Go fault-evidence question | 4572 / 5973 |
| dev-no-assignee | NO_ASSIGNEE_REQUIRES_CLARIFY refusal | Go assignee question | 17088 / 5947 |
| heldout-late-stop | required root cause before inspecting | cited inspection | 16813 / 23062 |
| heldout-mixed-stops | NUMERIC_MISMATCH refusal | cited inspection | 19795 / 28719 |
| heldout-material-stop | missing evidence called a conflict; no question | Go fault-evidence question | 5478 / 5995 |
| heldout-conflicting-log | no conflict question; task in artifact | model-reported source conflict; null task | 16868 / 6322 |

## Review fixes and remaining generation limits

The independent review blocked `903c902a` because rejected clarification text changed a contradiction
from needs_input to refused. The exact reviewer inputs reproduced this: an unsupported digit
returned UNSUPPORTED_NUMERIC_STATEMENT; a 495-rune question plus the prefix exceeded MaxQuestion
and returned INVALID_TEXT.

The fix preserves direct needs_input and its original reasons when Judge refuses the Go-built
clarification, and stores no rejected artifact. The digit case now stays
`needs_input / CONTRADICTORY_EVIDENCE` with no artifact. Removing the prefix lets the exact valid
495-rune input fit unchanged: it is validated and stored, still needs_input, with no task or
approval. This deliberately differs from the review's suggested no-artifact assertion for both
inputs because the length-overflow cause has been removed. The missing-input numeric-text test
also requires needs_input with no artifact. Original Go validation and authority checks remain.

Clarification rationales are now written by Go, so the observed model phrases “twenty units” and
“approximately five units” are not copied into that persisted rationale. The original-four stored
artifacts were read back to verify this. No general number-word detector was added to arbitrary
model prose; that broader semantic limitation remains. The raw historical responses are retained.

The runtime request test now checks the actual `p.requests[i].ResponseSchema`: exact missing-key
const, restricted status enum and null-only proposal. Forcing the maintenance schema on every
run fails this test. ToolVersion is now `wis614-tools-7`, covering the narrowed evidence.get enum
and omission when no captured document exists. Branch and test names use WIS-710.

Two narrowing choices limit what the local model can express:

- The maintenance grammar **forces unknowns.required=false**. Consequently Judge's required-unknown
  `REQUIRED_INPUT_MISSING` gate cannot fire on grammar-constrained maintenance output. Its Go
  validator remains for arbitrary provider output; constrained successes do not exercise it.
  A genuine prerequisite must keep the result in a non-maintenance path. Human approval remains.
- Zero server-known gaps means `missing_required_fields` has `maxItems:0`; the model cannot add
  other missing keys there. Other uncertainty must stay in non-maintenance choices or genuine
  contradiction reporting. Absence of a known gap does not establish a root cause.

**Hosted strict-schema acceptance/enforcement of the long regex patterns is untested.** The
fallback regression uses a scripted non-grammar provider to prove that invalid clarification
text cannot replace the original needs_input verdict. This is not hosted-provider validation.

## Original four cases and schema diagnosis

The retained synthetic demo dump was restored only into the owned disposable database.

| Case | Diagnosis | Review-follow-up result |
| --- | --- | --- |
| 0e07e22d-1460-4c38-9d66-3faed8eda3e3 | Unexplained stoppage and confirmed assignee existed; model wrongly required root cause/repair status first. | Cited inspection; causal hypothesis conditional. |
| 31795061-6099-49eb-8b74-2760637dd5eb | TOOL_WEAR_17 is machine_stoppage, not qualifying machine_fault; operational ID was used for evidence.get. No reviewed reason taxonomy permits reclassification. | Go question for qualifying machine-state evidence; no repeated assignee request. |
| 6fc46aef-3e8b-4023-b428-9935776a36f8 | No stoppage; large plant configuration cannot establish a fault. Missing evidence was called a contradiction and its explanation discarded. | Go fault-evidence question and Go rationale. |
| 97fa64a0-81a2-4071-aad4-b7dcac61afcc | No stoppage; correct abstention lost its explanation. | Go fault-evidence question and Go rationale. |

Original replay: one proposal, three needs_input, no schema refusals. Stored artifacts are in the
JSON report. Baseline requests used 2,745–10,085 actual input tokens, below the existing 12,000
local input limit and 16,384 endpoint context. System text was 3,713 bytes; final schema 7,726 bytes
with maintenance / 4,194 without. No evidence justified raising limits.

Historical two-of-four SCHEMA_INVALID responses were not retained; their exact fields remain
unknown. Fresh zero-error rates cannot replace that historical result. A later reproduced
regression came from an unbounded text regex overriding sibling maxLength in the local decoder:
a 40-character probe hit 100 output tokens and truncated JSON; embedding the bound inside a
JSON-safe pattern returned valid JSON in 14 tokens. That explains the new regression, not the
unavailable historical responses. V10 failures, v11 manual task-text failure and v12 missing-human
failure remain in the intermediate evidence; no failed run was silently discarded.

## Pins, provenance and reproduction

- Model: `unsloth/Qwen3.5-9B-GGUF:Q4_K_M`; weight SHA256 rechecked:
  `03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8`.
- Baseline code `94dba219`, prompt `wis702-investigation-6`, tools `wis614-tools-6`.
  Its harness was uncommitted and lacked a patch digest.
- Initial v13 measured code `06c8ba2c`; review-follow-up measured code `7c5f474d`.
  Prompt stays `wisnew-investigation-13` because its instructions/schema did not change;
  tool provenance is `wis614-tools-7`. Final rows pin the clean tracked code and patch digest.
- First blind heldout read followed the v9 implementation freeze (`eb00d346`). Later reruns,
  including this one, reuse that already evaluated holdout. Cases are structural near-copies of
  development cases. Only the label file was frozen; later checks tightened task description and
  removed prefix credit. No labeling or threshold was weakened to force a pass.
- Shared endpoint: temperature zero, reasoning_effort none, context 16,384, four server slots;
  client serializes inference under the shared flock. Max output 2,000, cumulative budget 24,000,
  approved local input limit 12,000. RTX 5070 Ti, 16,303 MiB; image digest in JSON. Timing includes
  runtime/tokenization/tools/persistence, excluding setup and lock wait. Shared load/cache are
  uncontrolled; the endpoint was externally restarted between earlier historical runs.

Use the repository's newdb.sh process for the owned disposable PostgreSQL on loopback 55810,
with a non-owner runtime role and private environment file. Set GOCACHE, GOMODCACHE and
npm_config_cache outside /home. The actual harness takes the model lock itself; do not nest flock.
Its report path must be new.

```sh
INVEST_REPORT=/var/tmp/new-investigation-report.jsonl go test -tags=integration,investigationactual \
  ./internal/agent -run '^TestWIS710InvestigationLocalCases$' -count=1 -v
make supervisor-check
make supervisor-contracts
make supervisor-integration CASE=WIS-276
```

The actual harness uses real intake, approved plans/contexts, roster, runtime, persistence and the
recovery bridge. Fixture regressions also read retained questions through the authenticated advisor
HTTP API. Browser login, Temporal transport, a live plant, repeated statistical trials, broad
multilingual/semantic coverage and full qualification are not covered. Raw diagnostics and RED/green
logs remain untracked in the clone's `.claude-flow/codex/evidence/`.

Fresh review-follow-up gates all passed (exit 0): `supervisor-check` recorded 2,263 passing test/subtest
events and zero skips; static PDF/scan checks, frontend build and dependency audit also passed
(zero vulnerabilities). `supervisor-contracts` passed four package checks and found no drift.
`supervisor-integration CASE=WIS-276` recorded 1,093 passing test/subtest events and zero skips.
Log digests are in the JSON report. These gates used the same production code as the final replays.
