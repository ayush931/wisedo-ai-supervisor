---
name: supervisor-onboarding-agent
description: The onboarding agent (WIS-610 and its children WIS-611 to WIS-633) - capturing a factory's supplier-to-dispatch value stream from interviews and uploaded documents into candidate entities and typed claims, the process_understanding checklist, document passages, approved snapshots, chat retrieval over approved passages, and the link to shift planning and the read-only advisor. Use this skill for any onboarding, value-stream, document-upload, passage-selection, retrieval/RAG or "onboard a factory" work, and whenever a model's output could become a process fact.
---

# Onboarding agent

Source: Linear WIS-610 (parent epic) and its child issues; design and progress in
`docs/onboarding-agent/README.md` (§0 Progress is the per-step table). Status checked 2026-11-03;
Linear is the source of truth. Binding rule: `.claude/rules/11-onboarding-agent.md`, on top of
rules 03, 04, 05, 06, 08 and 10.

## The flow

1. A factory describes how a product is made, from suppliers to dispatch, through role interviews
   and uploaded documents.
2. The onboarding specialist proposes **candidate** entities and typed claims, and each one cites
   the exact span it came from.
3. A human reviewer approves, rejects or resolves conflicts. The approved claims become an
   immutable `ApprovedProcessSnapshotV1`.
4. Chat, shift planning and the advisor read only approved snapshot content. They advise and never
   act; any action goes through rule 05 approval.

## Built (merged on main)

| Step | Issue | Where |
|---|---|---|
| Value-stream vocabulary: supplier, customer, location, inspection point; `supplied_by`, `received_at`, `stored_at`, `inspected_at`, `ships_to`; lead, cycle and setup times, batch size, yield, sample size, inspection method. Also the nine-cell `process_understanding` checklist | WIS-611 | `internal/knowledge`, migration 070 |
| Interview turn proposes candidate entities and claims. Every name and number must appear in a cited span. At most 20 entities and 20 claims per turn. Entity types are `material`, `operation`, `supplier`, `customer`, `location`, `inspection_point`, `tool`, `role` | WIS-612 | `internal/agent/interview.go`, `internal/onboarding/turns.go` |
| Document upload: text, CSV, DOCX and XLSX are extracted with the Go standard library inside the intake gate. The kind is detected from the bytes, never from the file name. PDF (WIS-624) is parsed in a resource-capped child process, `RunPDFChildIfRequested` first in `main` and in the `TestMain` of every package that uploads PDFs; its refusals are `PDF_ENCRYPTED`, `PDF_NO_TEXT_LAYER`, `PDF_TEXT_UNREADABLE` (422), `PDF_ACTIVE_CONTENT`, `PDF_EXTERNAL_REFERENCE` (415), `PDF_TOO_MANY_PAGES`, `PDF_PARSE_BUDGET_EXCEEDED` (413), and `EXTRACTION_BUSY` (503, retry the same key). Other errors: `LIMIT_EXCEEDED` (413), `UNSUPPORTED_MEDIA` (415), `UNREADABLE_SOURCE` (422). The text is stored as a `document` source with automatic spans of at most `MaxDocumentSpanRunes` = 2000 | WIS-613 | `POST /v1/sites/{site}/source-documents`, `internal/onboarding/intake.go`, `extract.go`, `documents.go` |
| A turn may select passages, up to `agent.MaxRunPassages` = 8 and `MaxTurnPassageBytes` = 12 KiB; `evidence.get` then returns only those passages of that source. A captured source without selected passages is still read whole (compatibility path) | WIS-614 | `agent.AddPassagesTx`, migration 071, `evidence.get` |
| Value-stream checklist cells can be bound to approved claims and approved | WIS-615 | `internal/knowledge` coverage |
| Proposed facts are reviewed on the interview page | WIS-616 | `web/src/proposed-facts.tsx` |
| Any reviewer sees the site's candidate claims on Setup → Facts to review (`GET /claims?state=candidate`), with the same card and review controls | WIS-641 | `web/src/review-queue.tsx` |
| The owner marks a document authoritative and confirms it current on Documents; an owner or supervisor publishes a snapshot on Approved process | WIS-641 | `web/src/document-authority.tsx`, `web/src/process-graph.tsx` |
| An interview chooses which checklist it covers | WIS-617 | `internal/onboarding/interviews.go` |

Interview routes:
- `POST`/`GET /interviews` and `GET /interviews/{id}`;
- `GET /interviews/{id}/questions`;
- `POST /interviews/{id}/answers`;
- `POST /interviews/{id}/agent-turns` and `GET /interviews/{id}/agent-turns/{runId}`.

Turn refusal codes include:
- `CITATION_NOT_IN_SCOPE`, `ENTITY_NOT_IN_SOURCE`, `CLAIM_INVALID`;
- `TOOL_REF_NOT_IN_SCOPE`, `REQUIREMENT_NOT_IN_SCOPE`;
- `QUESTION_LIMIT`, `NO_NEW_INTERVIEW_OUTPUT`, `PROMPT_REJECTED`, `TURN_ACTIVITY_FAILED`.

## Merged since 2026-11-03, and what is still open

All onboarding PRs through #255 are on main (2026-11-06):
- WIS-618, WIS-620, WIS-621, WIS-626, WIS-628, WIS-629, WIS-630, WIS-634: screens, retrieval, snapshot capacity, shift comparison, the advisor and tasks.
- WIS-622 and WIS-623: the hosted Qwen route per site (rules 01 and 08 amended).
- WIS-624: PDF text extraction in a resource-capped child.
- WIS-627: pgvector (migration 075) with hybrid ranking.
- WIS-625: OCR of photos and scanned PDFs (migration 076) with lower authority.
- WIS-631: the two-tenant pilot, including the browser gate.
- WIS-640: the onboarding, chat and OCR eval case sets.
- WIS-641: the reviewer and owner screens.

Still open:
- **Waiting on a credential, a model or a reviewer:**
  - the actual-model runs: WIS-623, WIS-625, WIS-627, WIS-631, WIS-640;
  - corpus growth to the frozen release sizes: WIS-646.
- **Code follow-ups:**
  - WIS-642: route summary (`GET .../route-summary`, merged in #273);
  - WIS-643: OCR designation checkbox;
  - WIS-644: commissioning screens (Setup → Processes, merged in #272);
  - WIS-645: fax pages merged (#275); mixed typed/scanned PDFs in review (migration 081);
  - WIS-647: retrieval quality.

`docs/onboarding-agent/README.md` §0 is the per-step table.

## Working on it

- **Search first.** Start from `docs/onboarding-agent/README.md` §0 and §4. Then read the issue,
  including its comments. Then read the code above.
- **No new model tool.** A new tool such as `retrieval.search` needs a change to rule 08 and a
  review. WIS-620 deliberately selects passages on the server instead.
- **Gates:**
  - `make supervisor-check` and `make supervisor-contracts`;
  - `make supervisor-integration CASE=WIS-271` (interviews and uploads), `CASE=WIS-517` (chat),
    `CASE=WIS-270` (knowledge and snapshots), `CASE=WIS-275` (agent runtime and migrations),
    `CASE=WIS-520` (planning);
  - Playwright specs `web/tests/interviews.spec.ts` and `proposed-facts.spec.ts`.
- **Negative controls.** Record one for each boundary on the issue: a span that does not contain
  the name, a citation outside the captured passages, a claim split across the passage limit, a
  cross-tenant source, a revoked source.
- **Fixtures and actual models.** Fixture and scripted providers are contract checks. They are not
  evidence of actual model behaviour (rule 09).

**Python methods (rule 01, relaxed 2026-11-12).** An OCR, layout, parsing or retrieval method may be
a Python component (for example a local OCR service) instead of Go code. It is still bounded:
- Go captures the scope, calls it (pinned local HTTP or a short-lived child process), validates every
  response against a versioned contract, and refuses a malformed one.
- It has no authority and no privileged database role; text reaches evidence only through the
  job-bound `evidence.CompleteScanTx` path, and OCR text stays machine-extracted with the lower
  authority of rule 11 (never independent corroboration; designation needs `confirm_model_extracted`).
- Rule 11 parser limits apply: no network, bounded CPU, memory and time, explicit refusal codes.
- None exists yet: today's OCR reads pages through the Go vision-model client below.

**WIS-625 OCR (merged; actual-model run open):**
- `POST /source-scans` queues a PNG or JPEG page. The worker's extraction run reads it with the pinned local vision model (`SUPERVISOR_AGENT_VISION_MODEL`). The run has no tools, its budget is reserved from the page's pixels, and it is never pinned to the hosted route.
- The text is stored through the job-bound `evidence.CompleteScanTx`, which re-checks the uploader's membership. The source is marked `extraction: model_ocr`.
- Failures quarantine the job with a reason from migration 076's enumerated list.
- Scanned PDFs are not covered yet (slice B2).
