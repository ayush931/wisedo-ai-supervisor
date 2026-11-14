# WIS-708 offline model measurements — 2026-11-11

**Measured synthetic evidence; not customer acceptance or a WIS-280 qualification pass.**

Twenty actual local-model runs completed through the production scan path: **120/120 exact
field values (100%)**, no quarantines and no recorded boundary breaches. Each of Record,
Machine, Lot, Accepted, Rejected and Date scored 20/20. These small pages have engineering-authored
labels and zero independent reviews; this result does not establish general OCR accuracy.

CPU time per page: minimum 33.487 s, median 38.726 s, maximum 46.567 s. The full run took
785.555 s. Host: Intel Core Ultra 7 270K Plus, 24 logical CPUs, four llama.cpp CPU threads,
no GPU devices. The host was shared; the final sample overlapped the beginning of the unit
gate. These times are not an isolated benchmark. GPU timing was not measured.

## Evidence and pins

- [Per-case report](offline-models-20261111.json): expected/observed fields, image hashes,
  source text, case/run IDs, provider, tokens, latency and server outcomes.
- [Frozen manifest](offline-models-20261111-manifest.json), SHA-256:
  `9970e4a4b300596c0b504d0ed42b8c06c0d0204ef0c7024c70ffd79e96adb62b`.
- Generator and harness: `cmd/supervisor-eval/offline_pages_test.go` and
  `offline_evidence_test.go`. The renamed WIS-708 generator reproduced the manifest byte-for-byte.
- The report's `git_sha` is runtime base `3706703a`; the harness was uncommitted during
  measurement. Its compiled source hashes are recorded. The issue/test names were renamed
  from the placeholder during the run; no cases were rerun or discarded. Later harness changes
  only rename the issue/tests and clarify report metadata.
- Raw logs, original report, PNG/PDF files, requests, responses and host/container inspections
  are kept off-repo in an artifact directory, written `<artifact-dir>` below. The raw report's
  mistaken free-text CPU name was corrected against saved `hardware.txt` in the committed
  report. Outcomes are unchanged.
- Host-specific paths, the measuring user and internal tooling were replaced with placeholders
  in this report after review (WIS-708 review finding 4). No measured value changed.

| Pin | Value |
| --- | --- |
| Chat model | Qwen3.5-9B Q4_K_M; `sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8` |
| Vision projector | mmproj-BF16; `sha256:853698ce7aa6c7ba732478bad280240969ddf7b0fcbf93900046f63903a83383` |
| Embedding model | Qwen3-Embedding-0.6B Q8_0; `sha256:06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439` |
| Server image | `ghcr.io/ggml-org/llama.cpp@sha256:5bb779a449a18d8f4333e9814e887746342fe555c528f5eabc92b2093e32673a` |
| Server build | `b12169-3ed7284f2` |
| OCR prompt | `wis625-ocr-1`; SHA-256 `44305d575db515948076b4329cd199bc4fb177609a0d4de3318854fcd53975cd` |
| Tool version | `wis614-tools-6` |

## Execution boundary

Only project `codex-offline-models` and disposable database `codex-offline-pg` on free loopback
port 61213 were created. The existing model cache was mounted read-only in place. No model
files were copied or downloaded. Existing model, showcase, e2e and Sight services were untouched.

Two checks cover distinct boundaries:

1. Both model services started with `network_mode: none` (Docker `--network none` equivalent).
   Requests ran through `docker exec` and loopback. Chat answered `13` to 6+7 (26 input, three
   output tokens). A PNG returned all six fields (291 input, 76 output tokens; 12.039 s prompt
   plus 15.617 s generation). A separate smoke request used the image extracted from the actual
   scanned PDF, also with both services under `network_mode: none`: all six fields matched,
   `finish_reason: stop`, 291 input/76 output tokens, 10.394 s prompt plus 14.434 s generation.
2. The 20-case harness used an `internal: true` model network with no published model ports,
   and the host-side authenticated in-process Supervisor API with real disposable PostgreSQL.
   Its first upload was an image-only PDF through `/source-scan-batches`; the other 19 used
   `/source-scans`. Every job ran `Runtime.ExtractScan` with its normal two-minute limit, model
   pin checks, strict output validation, persistence and image deletion. The host invoked the
   worker method directly; browser login and Temporal transport were not exercised.

External egress from the internal model network failed with curl exit 7. The embedding endpoint
returned 1,024 dimensions for “synthetic inspection log” (five input tokens). Chat startup logs
reported one slot with 16,384 context tokens. The chat model was served under the alias
`Qwen3.5-9B`; the compose default alias is now `unsloth/Qwen3.5-9B-GGUF:Q4_K_M` (the WIS-702
profile name) for the same pinned files. The runtime base predates WIS-702's `/props` probe;
this work pins server context without importing or modifying that reviewed branch.

## Commands and outputs

The deployment README contains portable setup commands. These were the local evidence commands
(Go 1.27.0, `GOCACHE=<go-cache-dir>`):

```sh
docker compose --env-file <artifact-dir>/models.env \
  -p codex-offline-models -f deploy/supervisor/compose.yml \
  -f <artifact-dir>/network-none.yml \
  --profile models up -d --pull never llm-chat llm-embed
docker exec codex-offline-models-llm-chat-1 curl --fail --silent --max-time 180 \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.5-9B","messages":[{"role":"user","content":"What is 6 plus 7? Reply with the number only."}],"max_tokens":32,"temperature":0}' \
  http://127.0.0.1:8080/v1/chat/completions
# content: "13"; finish_reason: "stop"

pdfimages -png <artifact-dir>/scans/offline-clean-01.pdf \
  <artifact-dir>/pdf-page
# pdf-image-request.json embeds pdf-page-000.png as an image_url data URL.
docker exec -i codex-offline-models-llm-chat-1 curl --fail --silent --max-time 300 \
  -H 'Content-Type: application/json' --data-binary @- \
  http://127.0.0.1:8080/v1/chat/completions \
  < <artifact-dir>/pdf-image-request.json
```

`network-none.yml` resets each model service's networks with `!reset []`, sets
`network_mode: none` and `user: "<uid>:<gid>"`, a non-root user. The later
`internal-network.yml` override sets only that user and retains the base Compose network. The
measured configuration therefore differed from the committed one at the time, which had no
`user:` key; the compose file now defaults to the non-root `65534:65534`
(`SUPERVISOR_MODELS_UID`/`SUPERVISOR_MODELS_GID`), which was not itself started in this run. The saved `models.env` contains the pins
above, cache snapshot paths and subnet/IPs `10.253.181.0/24`, `.2`, `.3`. DB credentials live in
a private environment file, never in this report.

```sh
# Create a disposable, migrated PostgreSQL database (codex-offline-pg) and write its
# connection settings to <artifact-dir>/pg.env.
# Source the private DB environment and local model settings; no hosted provider.
# SUPERVISOR_AGENT_BASE_URL=http://10.253.181.2:8080
# SUPERVISOR_AGENT_ENDPOINT_ADDRS=10.253.181.2:8080
# SUPERVISOR_AGENT_PROVIDER=local; SUPERVISOR_AGENT_VISION_MODEL=Qwen3.5-9B
# SUPERVISOR_AGENT_TIMEOUT=120s
# SUPERVISOR_OFFLINE_OUTPUT=<artifact-dir>/scans
go test -count=1 -tags=offline -run '^TestWISNEWOfflineScans$' -timeout=60m -v ./cmd/supervisor-eval
# PASS: TestWISNEWOfflineScans (785.55s)
# Current spelling for reproduction: TestWIS708OfflineScans
```

## Gates

- Startup regression: valid chat/projector and embedding files start; changed or missing files
  for all three are refused before serving; malformed/missing pins and unknown roles are refused.
- Removed-check mutation: the negative test fails with `FAIL: chat changed byte was accepted`,
  proving it detects a bypassed verifier.
- Default Compose config: only existing PostgreSQL, Keycloak, Temporal and default network;
  no model pins needed. Optional runtime config: only API, worker and both models on the private
  network, with no model ports published and read-only model mounts.
- `make supervisor-contracts`: PASS, no contract drift.
- `make supervisor-check`: PASS (format, vet, race/unit suite, static PDF sandbox tests,
  npm clean install/build and audit; zero npm vulnerabilities). Vite retained its chunk-size warning.
- Full integration/browser/GPU gates were not run. No Go store or production Go runtime code
  changed. The measured scan harness did use real PostgreSQL and the production HTTP/worker paths.

## Per-case outcomes

One run per case. Missing fields, changed values or duplicate labels count as wrong; scoring
normalizes label case and surrounding whitespace only. Skew is three degrees, low-contrast ink
is RGB 180 on white, and stamped pages carry a red CHECKED stamp. Handwriting-like pages use
italic glyphs with baseline jitter, not human handwriting.
The reused report's `exact_text` boolean is not evaluated and retains its zero value;
accuracy here refers only to the six labelled fields, not whole-page text equality.
Each case's `run.observed` block in the JSON is the reused WIS-280 agent-run struct, which this
OCR harness does not fill; its zero values are not measurements. The OCR run's values are in
`run.knowledge` and `run.ocr`.

| Case ID | Route | Fields | State | Seconds |
| --- | --- | ---: | --- | ---: |
| offline-clean-01 | /source-scan-batches | 6/6 | completed | 38.320 |
| offline-clean-02 | /source-scans | 6/6 | completed | 39.081 |
| offline-clean-03 | /source-scans | 6/6 | completed | 33.487 |
| offline-clean-04 | /source-scans | 6/6 | completed | 39.948 |
| offline-skewed-01 | /source-scans | 6/6 | completed | 36.653 |
| offline-skewed-02 | /source-scans | 6/6 | completed | 38.413 |
| offline-skewed-03 | /source-scans | 6/6 | completed | 40.049 |
| offline-skewed-04 | /source-scans | 6/6 | completed | 41.527 |
| offline-low_contrast-01 | /source-scans | 6/6 | completed | 45.679 |
| offline-low_contrast-02 | /source-scans | 6/6 | completed | 40.992 |
| offline-low_contrast-03 | /source-scans | 6/6 | completed | 46.567 |
| offline-low_contrast-04 | /source-scans | 6/6 | completed | 43.923 |
| offline-stamped-01 | /source-scans | 6/6 | completed | 35.300 |
| offline-stamped-02 | /source-scans | 6/6 | completed | 37.668 |
| offline-stamped-03 | /source-scans | 6/6 | completed | 45.066 |
| offline-stamped-04 | /source-scans | 6/6 | completed | 36.554 |
| offline-handwritten_style-01 | /source-scans | 6/6 | completed | 39.038 |
| offline-handwritten_style-02 | /source-scans | 6/6 | completed | 34.641 |
| offline-handwritten_style-03 | /source-scans | 6/6 | completed | 33.641 |
| offline-handwritten_style-04 | /source-scans | 6/6 | completed | 38.353 |
