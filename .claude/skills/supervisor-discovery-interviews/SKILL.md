---
name: supervisor-discovery-interviews
description: Resumable role-guided process interviews, source upload and parsing, extraction jobs and claim review screens (WIS-271). Use this skill for any work on onboarding, interview sessions and questions, document upload limits, the parser or OCR worker, extraction job lifecycle, or the claim review and coverage UI - including when the request only mentions "onboarding" or "ingesting an SOP".
---

# Discovery interviews and claim review

Source: Linear WIS-271 (depends on WIS-270 and WIS-290). Status: sessions, questions, answers and
agent turns are built (`internal/onboarding`, `internal/agent/interview.go`).
- **Also built since:** document upload with Word, Excel, CSV and text extraction (WIS-613) and
  the proposed-facts review on the interview page (WIS-616).
- **PDF extraction (WIS-624):** born-digital PDFs are extracted in a resource-capped child process; scanned pages are refused with `PDF_NO_TEXT_LAYER`. **Not built:** OCR (WIS-625).
- **In review:** the upload screen (WIS-618).
Checked 2026-11-03.
Governing rules: `.claude/rules/08-untrusted-input-and-model-boundary.md`, `.claude/rules/07-api-contract-conventions.md`.

A user starts or resumes a session, sees role-specific questions, submits text or a document,
reviews extracted candidates with source spans, resolves conflicts, and requests an exact process
snapshot approval. Model output never clicks approve and never changes rights.

## Contracts

- `InterviewSessionV1` — `session_id`, `role` (`owner|supervisor|operator`), `participant_actor_id`,
  optional `process_id`, `status` (`active|paused|submitted|closed`), `revision`, timestamps.
- `QuestionV1` — `question_id`, `revision`, `requirement_id`, `role`, prompt ≤ 1000 chars, ≤ 10
  source spans, `reason` (`missing|conflict|stale`), `answer_state`, optional `snoozed_until`.
  Dedup key is `(session, requirement_id, evidence_digest)`; max 3 unanswered per participant; one
  reminder per 24 h. No invented answers on timeout.
- Extraction job — durable workflow ID of tenant/site/source/revision/extractor-version; status
  `queued|running|review_required|completed|failed|quarantined|cancelled`. The result commits only
  while source access and revision are still current. Retry budget is 3 transient attempts
  (5 s, 30 s, 120 s); an exhausted job goes to a visible failure queue; invalid input is not retried.

## Onboarding specialist (WIS-612)

An agent turn (`InterviewTurnOutputV1`, prompt `wis614-onboarding-2`) may propose, besides up to three
questions:

- up to 20 candidate **entities** of type `material|operation|supplier|customer|location|
  inspection_point|tool|role`, each with an output-local `ref` and its name exactly as a cited span
  writes it;
- up to 20 candidate **claims** in the WIS-611 value-stream vocabulary: no `subject` means the
  interviewed process; otherwise `subject` and a relationship's `object` are refs from the same output.

Deterministic checks reject the whole output, writing nothing:
- an undeclared ref or any ID (`CLAIM_INVALID`);
- a pair or property subject the knowledge contract refuses (`CLAIM_INVALID`);
- an entity name not written in its cited span (`ENTITY_NOT_IN_SOURCE`);
- a number not written in its cited span, with no unit conversion or computed values
  (`NUMBER_NOT_IN_SOURCE`);
- a span the run did not retrieve (`CITATION_NOT_IN_SCOPE`).

Accepted proposals are written through `CreateEntityTx` and `CreateClaimTx` as the run's agent
identity. They remain candidates: a reviewer approves, rejects or merges them.

## Document passages (WIS-614)

`POST /interviews/{id}/agent-turns` may carry `source_span_ids`: up to 8 distinct spans (for example
the automatic spans of a `POST /source-documents` upload), at most 12 KiB of text together, so a
document larger than one model request is read a few passages at a time. Each must be a span of the
current revision of a source the participant may read: unknown or unreadable is 404, an older
revision is 409 `SOURCE_CHANGED`, too much text is 413 `LIMIT_EXCEEDED`. The passages are captured
in `agent_run_spans` (migration 071), and `evidence.get` then returns only those passages of that
source, each as `{span_id, untrusted_source}`, so a passage the turn was not given cannot be cited.
A turn without `source_span_ids` is unchanged.

## Question coverage by role

Owner captures objectives, authority and exceptions. Supervisor captures routing, target-rate
segments, capacity and escalation. Operator captures real steps, stoppages and workarounds.

## API

`POST /interviews`, `GET /interviews/{id}`, `PATCH /interviews/{id}`,
`POST /interviews/{id}/answers`, `GET /interviews/{id}/questions`, `POST /sources/uploads`
(multipart), `GET /sources/{id}/ingestion`. Upload returns 202 with job, source and status.
Answer and source edits append revisions, invalidate derived draft proposals and queue new
extraction; they never mutate approved history.

## Parser and upload limits

UTF-8 TXT/Markdown, PDF and DOCX through a pinned parser worker; plain-text input keeps working
with the parser disabled. 20 MiB per file, 200 PDF pages, 2 MiB extracted text, 1 active parser job
per site and 20 queued. The streaming byte limit is independent of `Content-Length`; overflow is 413.
Reject encrypted PDFs, ZIP input, executables, unsupported MIME, external relationships, network
fetch and DOCX decompression beyond 100 MiB. The parser runs with no network and 120 s / 2 GiB
limits. A parser or OCR method may be a Python component under rule 01 (2026-11-12) with the same
limits and a Go-validated contract. Image-only scans need explicitly enabled local OCR; uncertain spans stay `review_required`.
Do not fabricate a confidence value the engine did not produce. Max 100 claims per source from the
extractor; larger sources chunk at page or section boundaries with preserved spans and deterministic
dedup keys.

Errors: `415 UNSUPPORTED_MEDIA`, `422 UNREADABLE_SOURCE`, `429 INGESTION_QUEUE_FULL`,
`503 EXTRACTION_UNAVAILABLE`, `503 EXTRACTION_BUSY`, `503 SCAN_QUEUE_FULL` (a tenant past its cap on running
scan pages, `SUPERVISOR_AGENT_SCAN_TENANT_CAP`, default 40; a page still running after one hour stops counting; WIS-715), and the PDF refusals listed in the
onboarding-agent skill.

## UI acceptance

Progress survives refresh, network failure and worker restart. Autosave shows saving/saved/failed
and offers compare/reload on revision conflict; idempotent answer retries create no duplicates. The
claim drawer shows the exact quoted span, source identity and revision, effective times, the
draft/approved/contested distinction and the reviewer reason. A labelled table is the primary
accessible graph view; a diagram is supplemental. Keyboard navigation, labelled controls, focus
management, announced async status and non-colour-only cues are acceptance requirements.

## Verification

Go tests under `internal/onboarding`, then `make supervisor-integration CASE=WIS-271` and
`make supervisor-e2e E2E_ARGS=tests/interviews.spec.ts` (the e2e target takes Playwright arguments, not `CASE=`); browser specs `web/tests/interviews.spec.ts` and `web/tests/proposed-facts.spec.ts`. Negative
fixtures: scan, corrupt PDF, oversized stream without `Content-Length`, DOCX zip bomb, external
relationship, prompt injection, source revoked during extraction, duplicate answers, cross-tenant
session IDs. Parser fixtures must be synthetic or authorized and redistribution-safe. Mocked
extractor output is not evidence of extraction quality — WIS-280 evaluates real output.

## Implemented so far

`internal/onboarding/intake.go` is only the deterministic intake gate: `ReadBounded` counts received
bytes (never Content-Length), `Inspect` classifies bytes as text, PDF or DOCX and rejects executables,
encrypted PDFs, non-DOCX ZIPs, path traversal, external relationships and decompression beyond the cap,
and `Extract` returns the text of plain text, CSV, DOCX, XLSX and (in a child process) born-digital
PDF. The upload route is `POST /source-documents` (WIS-613); this paragraph's earlier "no route or
parser yet" is history.

