# Go Supervisor foundation — WIS-290

One Go binary (`supervisor api` and `supervisor worker`) with the OIDC session boundary, PostgreSQL with forced RLS, the Temporal worker and the same-origin React UI. It runs knowledge capture, intake, cases, human approvals and the internal tasks they create. Agent model calls go to a local or fixture provider (`SUPERVISOR_AGENT_PROVIDER`). Optionally, `SUPERVISOR_AGENT_HOSTED_PROVIDER=qwen-intl` with `SUPERVISOR_AGENT_HOSTED_MODEL`, `SUPERVISOR_AGENT_HOSTED_MODEL_REVISION` (a priced snapshot from `internal/agent/hosted_prices.v1.json`) and `SUPERVISOR_AGENT_HOSTED_API_KEY_FILE` (a file, never the key itself) configure a hosted route on both API and worker; it is used only by a site whose owner turns it on under Setup → Model hosting (WIS-622/623, rule 08). Names, e-mail addresses and phone numbers the system knows are replaced by request-local tokens before a hosted call, and a call whose text still names someone after decoding its escapes is refused (`PSEUDONYMS_INCOMPLETE`). Pseudonymisation is best-effort: names inside base64 or other binary encodings are not detected; the control for data leaving the host is the site owner's opt-in with a DPA (rule 08). `SUPERVISOR_AGENT_REASONING_EFFORT` (`none`, `low`, `medium` or `high`; local provider only, unset by default) is sent as `reasoning_effort` on each local chat request: a thinking model such as `qwen3.5` otherwise spends its whole output budget reasoning and returns a cut-off answer (WIS-631). `SUPERVISOR_AGENT_VISION_MODEL` and `SUPERVISOR_AGENT_VISION_MODEL_REVISION` pin a vision-language model for OCR (WIS-625). The two are set together, on the local provider's endpoint only. A page image is never sent to the hosted route, which refuses it with `IMAGE_NOT_HOSTED`, because an image cannot be pseudonymised. With a vision model set, a photo of a page (PNG or JPEG, at most 4 MiB and 40 megapixels) uploaded to `POST /v1/sites/{site}/source-scans` returns 202 and becomes a scan job. The worker reads it in one budgeted extraction run, reserved from the page's pixels, with no tools. The run is never pinned to the hosted route, even on a hosted site. A complete transcription becomes a `document` source marked `extraction: model_ocr`, with the model, revision, prompt hash and run. Anything else quarantines the job with a stable reason and stores no text, including a page cut off at the output limit. The image is deleted when the job ends. A scanned PDF (every page one JPEG or lossless image, no text layer, at most 20 pages) goes to `POST /v1/sites/{site}/source-scan-batches`. The resource-capped PDF child extracts each page's single image, and each page becomes one scan job, all pages in one transaction. A PDF with typed and scanned pages stores its typed pages as one typed document (never model-extracted) and queues its scanned pages, linked as one upload (`document_uploads`, migration 081) in one transaction (WIS-645); a PDF with no scanned page (`PDF_SCAN_HAS_TEXT`) belongs on `/source-documents`. A blank page, a JBIG2 or JPEG 2000 page, a fax-encoded page outside CCITT Group 4 or one-dimensional Group 3 with `BlackIs1` false, or a page with no or several pictures (`PDF_SCAN_IMAGE_UNSUPPORTED`) is refused; a supported fax page is decoded in the child to a PNG, after its declared size is checked against the 40-megapixel limit (WIS-645). Because OCR reads the embedded image, not the rendered page, a page whose image is not shown whole and alone (a crop box, clip, mask, palette, annotation, drawing over it, placement off the page or on less than half of it, a page under one inch on a side, a page colour space, graphics state or group, or an image that is not a plain 8-bit grey, RGB or CMYK picture matching its dictionary) is refused too (`PDF_SCAN_HIDDEN_CONTENT`), so the model never reads what the page hides. Such a source has lower authority: it never counts as an independent corroborating group, and an owner marks it authoritative only with `confirm_model_extracted: true` (otherwise `409 MODEL_EXTRACTED_UNCONFIRMED`). The vision server must downscale above 12,845,056 pixels (Qwen2.5-VL's default `max_pixels`), because the token reservation assumes it. The endpoint URL is compiled in, never configured. The retained OEE component is Go (`internal/oee`); the earlier Python OEE code is decommissioned and not in this repository. Status per area is in Linear.

## Local rehearsal

Requires Go1.27, Node22 and Docker Compose. The Supervisor executable, synthetic fixture utility and CI use Go; no Python interpreter or uv is required today (rule 01 allows Python method components since 2026-11-12; any such component ships as its own pinned, offline-built image). All services here bind loopback. Keycloak start-dev and Temporal auto-setup are local-only; this is not the production profile.

Set distinct local-only values for `POSTGRES_ADMIN_PASSWORD`, `SUPERVISOR_MIGRATION_PASSWORD`, `SUPERVISOR_RUNTIME_PASSWORD`, `KEYCLOAK_DB_PASSWORD`, `TEMPORAL_DB_PASSWORD`, `KEYCLOAK_ADMIN_PASSWORD`. URL-encode password components in DSNs. Never reuse customer credentials. Then:

```bash
docker compose -f deploy/supervisor/compose.yml up -d postgres keycloak temporal
# Separate privileged migration job; runtime is never a table owner.
docker compose -f deploy/supervisor/compose.yml --profile tools run --rm --build migrate
make supervisor-build
```

The compose profile uses separate DBs/roles and a persistent volume. Default ports PostgreSQL55477, Keycloak58077, Temporal57277. Override corresponding `SUPERVISOR_*_PORT` variables as needed. `SUPERVISOR_LOCAL_SUBNET` defaults to10.253.77.0/24; select a free subnet if it overlaps your network. No command prunes unrelated networks/containers.

CI runs on a self-hosted workstation runner (labels `self-hosted, linux, x64`) with its own compose project `supervisor-ci`, ports 45477/48077/47277/48176 and subnet 10.253.78.0/24, so it never collides with this developer stack on the same machine. Those ports sit inside Linux's default ephemeral range, so the runner host reserves them (`net.ipv4.ip_local_reserved_ports = 45477,47277,48077,48176` in `/etc/sysctl.d/`); otherwise an outbound connection from another process can take 48176 as its source port and the CI API fails to bind. Host prerequisites for the Supervisor are Docker with Compose v2, Go per `go.mod`, and Node 22. Python and `uv` are not required by the Go Supervisor or native Future Vision runtime; AWS training workflows remain optional and isolated under `deploy/aws/`.

Host-run API/worker share the browser-visible issuer. Export these settings (replace DSN placeholders; no literal placeholders may reach runtime):

```bash
export SUPERVISOR_ENVIRONMENT=local
export SUPERVISOR_DATABASE_URL='postgresql://supervisor_runtime:<runtime-password>@127.0.0.1:55477/supervisor'
export SUPERVISOR_MIGRATION_URL='postgresql://supervisor_migrator:<migration-password>@127.0.0.1:55477/supervisor'
export SUPERVISOR_PUBLIC_ORIGIN=http://127.0.0.1:58176
export SUPERVISOR_OIDC_ISSUER=http://127.0.0.1:58077/realms/supervisor-local
export SUPERVISOR_OIDC_CLIENT_ID=supervisor
export SUPERVISOR_OIDC_CLIENT_SECRET='<local-client-secret>'
export SUPERVISOR_TEMPORAL_ADDRESS=127.0.0.1:57277
# Optional worker allowlist; omit or set [] to discover every active tenant.
# export SUPERVISOR_RELAY_TENANT_IDS='["<tenant UUID>"]'
```

Sign out (WIS-713) ends the identity provider session too: `POST /auth/logout` returns
`{"end_session_url": ...}` when the provider publishes an `end_session_endpoint`, and the browser
follows it with `client_id` and `post_logout_redirect_uri=<SUPERVISOR_PUBLIC_ORIGIN>/`. No provider
token is stored, so there is no `id_token_hint` and Keycloak asks "Do you want to log out?" first.
The provider client must allow that post-logout redirect URI (Keycloak: "Valid post logout redirect
URIs"; `supervisor-seed` sets it for the local fixture, so re-run it on an existing stack). When the
provider is unreachable or publishes no safe `end_session_endpoint`, sign out stays local, logs code
`PROVIDER_UNAVAILABLE`/`END_SESSION_NOT_PUBLISHED`/`END_SESSION_UNSAFE` and writes identity audit
event `provider_logout_skipped`. Every sign-in also sends `prompt=login`, so the provider always
shows its form.

Synthetic fixture only: export `SUPERVISOR_LOCAL_KEYCLOAK=http://127.0.0.1:58077`, `SUPERVISOR_LOCAL_ADMIN=local-admin`, `SUPERVISOR_LOCAL_ADMIN_PASSWORD`, `SUPERVISOR_LOCAL_USER_PASSWORD`, and `SUPERVISOR_TEST_ADMIN_URL` pointing to the disposable PostgreSQL **postgres administrator** on loopback. Run `go run ./cmd/supervisor-seed`. It creates the local fixture site ("Chakan Plant", realistic names by product decision), its member, Keycloak realm/client and `foundation-reviewer` user; it refuses nonlocal environments and nonliteral-loopback provider, browser-origin or database endpoints. HTTP redirects are refused, and fixture credentials are never printed. This privileged fixture utility is not customer onboarding or production bootstrap.

WIS-520 workstation demo (synthetic, local only): after `supervisor-seed`, also export `SUPERVISOR_DATABASE_URL` (runtime role, loopback), `SUPERVISOR_OIDC_CLIENT_SECRET` and `SUPERVISOR_PUBLIC_ORIGIN`, then run `make supervisor-demo-seed`. It applies the same local-only guards, adds Keycloak users `shift-lead`, `shift-approver` (supervisors), `operator-1`, `operator-2` (operators) with `SUPERVISOR_LOCAL_USER_PASSWORD`, and creates through the public API (as those users, via short-lived synthetic sessions it deletes afterwards): a confirmed-current SOP, two job orders, `press-01`/`weld-01` station profiles, a shift plan for the current 8-hour Asia/Kolkata shift submitted by `shift-lead` and approved by `shift-approver`, station grants and a few `press-01` entries. Re-running the same version is safe; a new shift gets a new plan. After a change to the seeded data (names, SOP text), start from fresh volumes: existing tenant/site names and the SOP source are not rewritten, and the SOP post fails with a conflict. It prints the browser URLs and usernames, never passwords.

The seeded shortfall case goes stale about two minutes after seeding. The seed creates two `event_api` sources for it: the production telemetry source and the machine-state source. Each has `freshness_seconds: 900` and is heartbeated once, during seeding. Nothing heartbeats them afterwards. Heartbeat freshness is capped at 120 s (`limits.MaxHeartbeatAge`) whatever `freshness_seconds` says, and the executor checks it again when an approval is dispatched. So an approval given more than 120 s after the last heartbeat is refused with `EVIDENCE_STALE`, and the case moves to `needs_input`. `GET /v1/sites/{site}/operational-sources/{source}/liveness` shows the limit in force as `policy.effective_freshness_seconds`, and reports `HEARTBEAT_STALE` once it is exceeded. To run the case at human speed, keep `supervisor worker` running and heartbeat both sources at least every 60 s until the approval is dispatched. The source IDs are in `GET /v1/sites/{site}/operational-sources`. Send the heartbeats from the browser session of an active site member whose role is owner, supervisor or operator (`ingestion.AcceptBatch`); a reviewer or viewer gets `403 PERMISSION_DENIED`. These are cookie-authenticated mutations, so send `Origin` and `X-CSRF-Token` as the web app does, plus a new `Idempotency-Key` for each call:

```http
POST /v1/sites/{site}/operational-sources/{source}/batches
Idempotency-Key: demo-heartbeat-{source}-{n}
Content-Type: application/json

{"schema_version":1,"source_id":"{source}","source_revision":2,"mapping_revision":2,
 "events":[{"external_key":"heartbeat-keepalive-{n}","kind":"heartbeat","source_sequence":{next},"revision":1,
            "event_at":"{now, RFC3339 UTC}","payload":{"observed_at":"{now, RFC3339 UTC}"}}]}
```

`{next}` is that source's `ingest.accepted_sequence + 1` from its liveness response. Use the current time for `event_at`; a value more than `clock_skew_seconds` (30) from the receipt time marks the clock `suspect`. The worker folds each accepted batch. Only after that fold does liveness move `last_heartbeat_received_at` forward and clear `HEARTBEAT_STALE`, and only then does dispatch accept the heartbeat.

Run in separate terminals:

```bash
./build/supervisor api
./build/supervisor worker
```

Open the configured public origin, sign in as `foundation-reviewer`, submit a check and verify its immutable receipt becomes completed. Repeating the same unchanged check keeps the same idempotency key. Denial/unknown/queued states never become success without readback.

## Offline local models (optional `models` profile)

Model files and the pinned llama.cpp container image must already exist on the machine.
Provisioning/downloading them is outside this profile. It uses explicit local files and
`pull_policy: never`; there is no model download or hosted fallback. The entrypoint accepts only
the arguments the profile uses (`--host`, `--port`, `--ctx-size`, `--parallel`, `--jinja`,
`--chat-template-kwargs`, `-ngl`, `--no-mmproj-offload`, `--threads`) and refuses any other, so a
command override cannot name another model, an adapter (`--lora`), a control vector, a template
file, a second alias or a download source. It also refuses `LLAMA_ARG_HF*`, `_MODEL*`, `_MMPROJ*`,
`_DOCKER*`, `_LORA*`, `_CONTROL_VECTOR*`, `_CHAT_TEMPLATE_FILE` and `_ALIAS` variables, and a model
file name with `..` or a leading `/`. The files are hashed before the server opens them again, so
the model directory must not be writable by the runtime user or anyone else on the host. The base profile remains CPU-only and does not request a
GPU. Enabling it does not change fixture-model CI.

The image is `ghcr.io/ggml-org/llama.cpp:server-cuda`, llama.cpp build b12169 (revision
`3ed7284f2`), referenced only by digest
`sha256:5bb779a449a18d8f4333e9814e887746342fe555c528f5eabc92b2093e32673a` (about 7 GB). The
default CPU setup uses this same CUDA image: it runs without a GPU device (`-ngl 0`), but the image
still carries the CUDA libraries and sets `NVIDIA_VISIBLE_DEVICES=all`, which has no effect
without the NVIDIA runtime. A CPU-only llama.cpp `server` image of the same build would also work,
but it has a different digest that you must pin and verify yourself. To move the image to an
offline machine:

```bash
# On a connected machine:
docker pull ghcr.io/ggml-org/llama.cpp@sha256:5bb779a449a18d8f4333e9814e887746342fe555c528f5eabc92b2093e32673a
docker save -o llama-server-b12169.tar \
  ghcr.io/ggml-org/llama.cpp@sha256:5bb779a449a18d8f4333e9814e887746342fe555c528f5eabc92b2093e32673a
# On the offline machine:
docker load -i llama-server-b12169.tar
docker image inspect ghcr.io/ggml-org/llama.cpp@sha256:5bb779a449a18d8f4333e9814e887746342fe555c528f5eabc92b2093e32673a \
  --format '{{index .Config.Labels "org.opencontainers.image.version"}}'   # prints b12169
```

The last command must succeed: Compose resolves the image only by that digest. With Docker's
containerd image store (the default on new installs, and what was measured here) `docker load`
keeps the digest. With the classic image store, `docker load` does not restore the repository
digest, so the reference does not resolve and the services refuse to start. Enable the
containerd image store, or load into a registry on the offline network and pull the digest from
it. The classic-store path was not exercised in this work.

The model containers run as `${SUPERVISOR_MODELS_UID:-65534}:${SUPERVISOR_MODELS_GID:-65534}`
(`nobody` by default), never as root, with a read-only root filesystem, no capabilities and
`no-new-privileges`. That user must be able to read every model file and its symlink target, and
to traverse every directory on the way (for example `chmod -R o+rX` on the model directory), or
set the two variables to the owner of the files. The entrypoint itself is mounted read-only and
must be readable by that user; it writes nothing. An unreadable file is refused before serving
(`missing or unreadable file`). The measured runs used a different non-root user; the default
`65534` was not started in this work.

Set `SUPERVISOR_MODELS_DIR` to an absolute directory with this layout:

```text
models/
  Qwen3.5-9B-Q4_K_M.gguf
  mmproj-BF16.gguf
  Qwen3-Embedding-0.6B-Q8_0.gguf
```

The existing cache layout also works without copying: mount its root and set
`SUPERVISOR_AGENT_MODEL_FILE`, `SUPERVISOR_AGENT_VISION_MODEL_FILE` and
`SUPERVISOR_EMBEDDINGS_MODEL_FILE` to their relative snapshot paths. Keep the cache's relative
symlink targets inside the same mount. Do not change model files while a server is running.
Compose mounts the directory read-only and refuses a missing directory instead of creating one.

Set the following pins in the Compose environment **and in the API/worker environment**.
These values identify the files measured on 2026-11-11; other approved files need their own
trusted pins. Do not replace a trusted pin with the hash of an unexpected file just to start it.

```dotenv
SUPERVISOR_AGENT_MODEL=unsloth/Qwen3.5-9B-GGUF:Q4_K_M
SUPERVISOR_AGENT_MODEL_REVISION=sha256:03b74727a860a56338e042c4420bb3f04b2fec5734175f4cb9fa853daf52b7e8
SUPERVISOR_AGENT_VISION_MODEL=unsloth/Qwen3.5-9B-GGUF:Q4_K_M
SUPERVISOR_AGENT_VISION_MODEL_REVISION=sha256:853698ce7aa6c7ba732478bad280240969ddf7b0fcbf93900046f63903a83383
SUPERVISOR_EMBEDDINGS_MODEL=Qwen3-Embedding-0.6B
SUPERVISOR_EMBEDDINGS_MODEL_REVISION=sha256:06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439
```

The vision revision is the projector file's hash; its paired text model is independently
verified by the agent model pin. Chat and vision use the same served model alias,
`unsloth/Qwen3.5-9B-GGUF:Q4_K_M`, which is also the compose default. Keep that exact name: the
WIS-702 local limit profile and its `/props` probe apply only to that name with the chat pin
above. The 2026-11-11 measurement served the same files under the alias `Qwen3.5-9B`.
The entrypoint checks both chat files, or the embedding file, with `sha256sum` before executing llama-server.
A missing file, malformed/unset pin or mismatch exits nonzero with `offline model: ...`.
`sh deploy/supervisor/model-entrypoint-test.sh` tests each refusal with a real changed/missing
file and proves that the server was never started. The same check runs in the Go unit gate.

From the repository root, with the local dependency credentials described above configured:

```bash
docker compose -p supervisor-local -f deploy/supervisor/compose.yml \
  --profile models up -d --pull never llm-chat llm-embed
docker compose -p supervisor-local -f deploy/supervisor/compose.yml \
  --profile models ps
```

The models have no published ports and are attached only to the `internal: true` `models`
network. PostgreSQL, Keycloak and Temporal are not on it. The optional
`compose.models-runtime.yml` attaches only `api` and `worker` as consumers, with service-name
allowlists and exact IP:port pins. Supply their existing database, Temporal, public-origin,
OIDC and feature settings in `runtime.env` (or `SUPERVISOR_RUNTIME_ENV_FILE`). The issuer must
be reachable under the same URL by the browser and containers; the host-only rehearsal's
loopback issuer cannot be reused inside a container. Build/load application images and
provision identities beforehand; this overlay does not provision a production deployment.

```bash
docker compose -p supervisor-local -f deploy/supervisor/compose.yml \
  -f deploy/supervisor/compose.models-runtime.yml \
  --profile models --profile runtime up -d api worker llm-chat llm-embed
```

For the existing Linux host-run API/worker rehearsal, use the private bridge addresses instead:
agent base URL `http://10.253.183.2:8080`, endpoint pin `10.253.183.2:8080`; embedding base URL
`http://10.253.183.3:8080`, endpoint pin `10.253.183.3:8080`. Set `SUPERVISOR_AGENT_PROVIDER=local`,
`SUPERVISOR_AGENT_TIMEOUT=120s`, `SUPERVISOR_EMBEDDINGS_ENABLED=true`, and the pins above.
Leave `SUPERVISOR_AGENT_HOSTED_*` unset for offline use. As with other local Docker bridges,
the Docker host administrator can access the network; it is not a boundary against that host.
If the subnet overlaps another network, change `SUPERVISOR_MODELS_SUBNET`, `SUPERVISOR_CHAT_IP`
and `SUPERVISOR_EMBED_IP` together, including the host process's endpoint pins.

Chat has one slot with **16,384 context tokens**; embeddings have one slot with 8,192.
This chat setting and the default model name match the WIS-702 local limit profile
(`unsloth/Qwen3.5-9B-GGUF:Q4_K_M` with the chat pin above, at least 14,000 context tokens). The
worker's `/props` probe (WIS-702) checks the served context at startup; keep this name and context
setting, and check that a local run records `limit_profile_id=wis702-local-qwen35-9b`.

The runtime overlay defaults `SUPERVISOR_AGENT_MAX_CONCURRENT` to 1 to match that single chat
slot. With one slot, llama.cpp makes a second call wait for the first, and a waiting call can
exceed its own time limit. The API and the worker each apply this limit, so up to two calls can
still reach `llm-chat` at once; the second one waits. Each local call is sized at about 12k tokens.
If you raise `--parallel`, llama.cpp splits `--ctx-size` across the slots, and a slot smaller than
a call overflows ("Context size has been exceeded"). That opens the provider circuit breaker and
refuses every chat with `PROVIDER_UNAVAILABLE`, which is what a 4-slot, 16k server did on the
demo stack. So set `--ctx-size` to at least slots × (12k + output tokens) before raising
`--parallel`. Then set `SUPERVISOR_AGENT_MAX_CONCURRENT` no higher than the slot count, in the
shell or Compose `.env`, because the overlay's value takes precedence over `runtime.env`.

Plan for a dedicated 16 GB GPU for seconds-per-page service, or CPU operation that can take
minutes per page; those are capacity expectations, not a latency guarantee. The small synthetic
page measured here took about 28 seconds of prompt processing plus generation on four CPU
threads. Full pages and concurrent workload cost more. The production scan path still has its
two-minute call limit and quarantines timed-out pages. Do not interpret a successful tiny-page
check as evidence that a whole customer document fits that limit. Both CPU model containers
used about 7.1 GiB combined immediately after startup on this host; allow additional RAM for
inference and the Supervisor stack. GPU timing was not measured in this task.

For NVIDIA GPU operation, use a Compose override that requests a GPU and replaces only the
chat command (Compose replaces a command list). The embedding service can remain on CPU:

```yaml
services:
  llm-chat:
    command: [chat, --host, 0.0.0.0, --port, "8080", --ctx-size, "16384", --parallel, "1", --jinja, --chat-template-kwargs, '{"enable_thinking":false}', -ngl, "99"]
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]
```

This needs the NVIDIA container runtime and free GPU memory. The default CPU variant uses
`-ngl 0 --no-mmproj-offload`; it works with the same pinned image without a GPU device.

### Strict network-disabled smoke check

Use a separate project. The following override gives both servers Docker's `--network none`
equivalent, so even the host cannot call their HTTP ports. Requests run with `docker exec`
on each container's loopback. Use a Compose version with `!reset` support (tested with 5.6.0).

```bash
cat > /tmp/supervisor-models-none.yml <<'YAML'
services:
  llm-chat:
    networks: !reset []
    network_mode: none
  llm-embed:
    networks: !reset []
    network_mode: none
YAML
docker compose -p supervisor-offline-check -f deploy/supervisor/compose.yml \
  -f /tmp/supervisor-models-none.yml --profile models up -d --pull never llm-chat llm-embed
chat=$(docker compose -p supervisor-offline-check -f deploy/supervisor/compose.yml \
  -f /tmp/supervisor-models-none.yml --profile models ps -q llm-chat)
# Wait until both healthchecks say healthy; then inspect that NetworkMode is none.
docker inspect "$chat" --format '{{.HostConfig.NetworkMode}} {{.State.Health.Status}}'
docker exec "$chat" curl --fail --silent --max-time 180 \
  -H 'Content-Type: application/json' \
  -d '{"model":"unsloth/Qwen3.5-9B-GGUF:Q4_K_M","messages":[{"role":"user","content":"What is 6 plus 7? Reply with the number only."}],"max_tokens":32,"temperature":0}' \
  http://127.0.0.1:8080/v1/chat/completions

# offline-sample.png is the measured synthetic page offline-clean-01 (SHA-256 097e5689...).
# Expect Record INSP-001, Machine PRESS-01, Lot BATCH-2301, Accepted 107, Rejected 1, Date 2026-11-01.
{ printf '%s' '{"model":"unsloth/Qwen3.5-9B-GGUF:Q4_K_M","max_tokens":300,"temperature":0,"messages":[{"role":"user","content":[{"type":"text","text":"Read the six labelled fields exactly."},{"type":"image_url","image_url":{"url":"data:image/png;base64,'
  base64 -w0 deploy/supervisor/offline-sample.png
  printf '%s' '"}}]}]}'
} | docker exec -i "$chat" curl --fail --silent --max-time 300 \
      -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:8080/v1/chat/completions
docker compose -p supervisor-offline-check -f deploy/supervisor/compose.yml \
  -f /tmp/supervisor-models-none.yml --profile models down -v
```

That smoke check exercises the endpoint directly. The separate accuracy harness uses the real
authenticated scan routes, disposable PostgreSQL and `Runtime.ExtractScan`, with models on the
internal network (no external egress). It sends its first page as an image-only PDF through
`/source-scan-batches` and the other 19 as PNGs through `/source-scans`. It does not run a browser,
an OIDC login round trip or Temporal transport. With a migrated disposable database and the
local model environment above:

```bash
SUPERVISOR_OFFLINE_OUTPUT=/tmp/offline-scans \
SUPERVISOR_OFFLINE_HARDWARE='describe the inspected host, threads and device configuration' \
go test -tags=offline -run '^TestWIS708OfflineScans$' -count=1 -timeout=60m -v ./cmd/supervisor-eval
```

The harness requires `SUPERVISOR_TEST_ADMIN_URL` and `SUPERVISOR_TEST_DATABASE_URL`, refuses
missing dependencies, freezes `manifest.json` before inference, and writes `accuracy.json`
after every case. It reports model/projector revisions, prompt hash/version, case and run IDs,
input hashes, field outcomes, tokens, latency and quarantine reasons. Handwriting-like pages
are synthetic italic glyphs with baseline jitter, not actual handwriting. One run per case and
engineering-authored labels provide measured evidence, **not customer acceptance or a WIS-280
qualification pass**. See [the recorded run](../../evals/supervisor/offline-models-20261111.md).

## Sessions, limits and database TLS (WIS-290)

- **Session policy.** One policy applies to every service: `SUPERVISOR_SESSION_IDLE` (default
  `30m`), `SUPERVISOR_SESSION_ABSOLUTE` (default `8h`) and `SUPERVISOR_SESSION_RECENT_AUTH`
  (default `15m`, the maximum age of the identity proof for approvals, reviews and membership
  changes). Values may be stricter than the defaults, never looser: the membership guard in
  migration 004 enforces the defaults inside the database. A looser or malformed value stops
  startup.
- **Limits.** Request, event, import and document limits are defined once in `internal/limits`:
  1 MiB request body, 256 KiB event, 10 MiB / 50,000-row import, 20 MiB document, 2 MiB extracted
  text, 100 MiB decompressed archive. Site master-data CSV imports are capped at 2,000 rows to
  bound the site lock; raise this cap only with a faster import and a re-measured lock bound.
- **PDF parser sandbox (WIS-624).** Every PDF (text and scanned-page mode) is parsed in a
  short-lived child of the same binary, started in its own process group (killed whole on
  timeout) with `/` as its working directory. Before reading the file the child confines itself
  (`internal/onboarding/pdf_confine_linux.go`): it closes inherited descriptors, loads the local
  time zone, makes itself non-dumpable (no core, even through a piped `core_pattern`, and no
  same-uid reads of its memory), creates its runtime threads, and sets hard limits: `RLIMIT_CPU` 20 s, `RLIMIT_CORE` 0, `RLIMIT_DATA` and
  `RLIMIT_AS` each at the start-up size plus 1 GiB (measured for the static production binary:
  about 78 MiB data and 1.25 GiB address space at start, so ceilings of about 1.1 and 2.25 GiB).
  It then installs a seccomp allowlist with `no_new_privs`, measured with `strace -f` on the
  children: memory, futex, sleep, signals to itself, reading stdin and writing stdout, and `mmap` only `MAP_PRIVATE` without
  `MAP_GROWSDOWN`. File opens and `socket` return `EPERM`; any other call kills the child with
  `SIGSYS`, and the upload is `503 EXTRACTION_UNAVAILABLE` with "sandbox refused a syscall" in the
  server log (the kernel audit log names the syscall), never a 413 blamed on the file. The
  production image is built with `CGO_ENABLED=0`, which also refuses `mprotect`, `MAP_STACK` and
  every `clone` (its threads exist before the filter, so a parse cannot exhaust the container's
  tasks); cgo builds allow them for glibc threads under the same `RLIMIT_AS`, and `-race` builds
  (tests only) have no `RLIMIT_AS`. The unit gate therefore also runs the PDF tests in a static build.
  The sandbox needs Linux on amd64 with seccomp and no privilege: it works as uid 10001 under
  Docker's default seccomp profile and on the self-hosted CI runner. Do not run the container
  with a seccomp profile that refuses `seccomp(2)` or `prctl(2)`. User and network namespaces are
  not used, because Docker's default profile and Ubuntu's AppArmor userns restriction refuse them
  to an unprivileged process. If the sandbox cannot be applied, the child exits before reading the
  file and the upload is `503 EXTRACTION_UNAVAILABLE`; the server log names the step that failed.
  There is no setting to parse unconfined. Other platforms (macOS, linux/arm64) have no sandbox
  and refuse every PDF the same way; their PDF tests fail with `BLOCKED`.
- **Database TLS.** With `SUPERVISOR_ENVIRONMENT=production`, `SUPERVISOR_DATABASE_URL` must
  verify the server certificate: `sslmode=verify-ca` or `sslmode=verify-full`, plus
  `sslrootcert=<CA bundle>` when the CA is not in the system store. `sslmode=require` encrypts
  without verifying the server and is refused at startup. Local rehearsal on loopback is unchanged.
  The same rule applies to `SUPERVISOR_MIGRATION_URL` (`supervisor migrate`) and
  `SUPERVISOR_PROVISIONING_URL` (`provision`, `bootstrap-invite`). An unset
  `SUPERVISOR_ENVIRONMENT` means production, so local runs of those commands set
  `SUPERVISOR_ENVIRONMENT=local` (WIS-699).
- **`SUPERVISOR_ENVIRONMENT=local` is for local hosts only.** It turns off database and Temporal
  TLS verification, secure cookies and the fixture-provider refusal, so it is refused unless the
  public origin, OIDC issuer, database and Temporal hosts are all local: loopback or private
  addresses, `localhost`, or the stack's own service names `postgres`, `keycloak`, `temporal` and `migrate` (WIS-699); any other dotless name is refused. Session
  cookies are Secure whenever the public origin is HTTPS, whatever the environment label.
- **Auth-route rate limit (WIS-699).** `GET /auth/login` and `GET /auth/callback` are the only
  routes an anonymous client can call that fetch the provider and write login state. Each client
  gets a burst of 60 requests, refilled at one a second; over that it gets `429 RATE_LIMITED`
  with `Retry-After`, and nothing is written. The limit is in process, so each replica keeps its
  own. A client is its connection address. Behind a reverse proxy, set
  `SUPERVISOR_TRUSTED_PROXY_CIDRS` (comma-separated CIDRs) to the proxy ranges, so the rightmost
  `X-Forwarded-For` hop that is not a trusted proxy is used. Without it, every client behind the
  proxy shares one budget, and a forwarded header is never believed; production logs a warning at
  startup when the setting is empty. The trusted ranges must never include client ranges (for
  example a whole `10.0.0.0/8` that also holds clients), or those clients could pick their own
  bucket. A hop written with a port is read as its address; an unreadable hop keys that connection,
  not the proxy. IPv6 clients are grouped by /64. Authenticated routes are not rate limited yet.

## Approved task execution (WIS-277)

`SUPERVISOR_TASK_EXECUTION_ENABLED=true` lets the worker's deterministic executor consume an
approved `maintenance_investigation.create` proposal into one internal task. It is off by default.
It is one switch for the whole deployment (every tenant and site), read when the worker starts:
there is no per-site execution mode yet (WIS-290), and turning it off for rollback needs a worker
restart. While it is off an approval is never consumed and expires at the end of its window.
With the flag on or off, the worker returns a case whose proposal was rejected, revoked, expired
or refused at dispatch to `needs_input`, with the reason in its timeline. Before consuming, the
executor re-checks the approval window, each approver's current supervisor role, the exact digest
they approved, the policy revision, the case binding, every bound evidence revision (evidence
sources, contexts and machine stoppages, each still current and unrevoked), the assignee's roster
confirmation and that the task's asset is the case line. Approval consumption, the action ledger
row, the task, its first event and the case moves to `executing` and `observing` commit in one
transaction. Rollback: set the flag to `false` and restart the worker; tasks already created stay
readable at `GET /v1/sites/{site}/tasks`.

The task lifecycle runs at `POST /v1/sites/{site}/tasks/{task}/{action}`. Only the assignee may
`acknowledge`, `start` (from `blocked` with a reason) and `complete`. A completion cites 1-20
evidence sources the assignee wrote, at least one a written (`manual`) report. A recently
authenticated site supervisor may `block`, `cancel` and `reopen`, each with a reason; owner alone
is not enough. A reopen keeps the earlier completion in the task history. A case at `executing` or
`observing` can be canceled only after its task is canceled.

After a completion, the worker measures the outcome (`OutcomeObservationV1`, shown as `outcome` on
the task). It looks at the next two complete productive 15-minute windows, skipping the window that
contains the completion and windows with no planned production:
- **`observed_improvement`:** both windows have a deficit at most 5% of expected. This is threshold
  recovery, never proof that the task caused it.
- **`no_improvement`:** otherwise.
- **`not_comparable`:** the order, unit or approved plan changed.
- **`insufficient_data`:** two trustworthy windows did not exist within two hours.

Windows are assessed, not stored as contexts, so measuring never opens a case. Each observation is
immutable. `POST /v1/sites/{site}/cases/{case}/close` lets a recently authenticated site supervisor
resolve the case once its newest task is terminal and that outcome is recorded. The closing reason
never replaces the measurement.

## Verification

```bash
make supervisor-check
# Requires migrated isolated DB, including runtime role and real Temporal:
export SUPERVISOR_TEST_DATABASE_URL="$SUPERVISOR_DATABASE_URL"
export SUPERVISOR_TEST_TEMPORAL_ADDRESS="$SUPERVISOR_TEMPORAL_ADDRESS"
make supervisor-integration
# CASE=isolation|identity|worker-crash|migration narrows a nonempty suite.
npx --prefix web playwright install chromium
make supervisor-e2e
```

#### Browser e2e and the agent

`web/tests/interviews.spec.ts` needs `SUPERVISOR_E2E_AGENT` set to match how the API was started.
A tagged test run against the other mode reports BLOCKED, never a skip.

| API started with | Set | Run |
| --- | --- | --- |
| no `SUPERVISOR_AGENT_PROVIDER` (default CI phase) | `SUPERVISOR_E2E_AGENT=off` | `make supervisor-e2e E2E_ARGS='--grep-invert "@recent-auth|@agent-fixture|@agent-scripted|@onboarding-pilot|@feedback"'` |
| `SUPERVISOR_AGENT_PROVIDER=fixture`, model and revision pins, `SUPERVISOR_AGENT_MODE_ENABLED=true`, no fixture script | `SUPERVISOR_E2E_AGENT=fixture` | `make supervisor-e2e E2E_ARGS='--grep @agent-fixture'` |
| Same fixture pins plus `SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.v1.json` on both API and worker | `SUPERVISOR_E2E_AGENT=scripted` | `make supervisor-e2e E2E_ARGS='--grep @agent-scripted'` |
| Same fixture pins plus `SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.pilot.v1.json`, `SUPERVISOR_CHAT_ANSWERING_ENABLED=true` and the two pilot tenants (`…0631`, `…0633`) in `SUPERVISOR_RELAY_TENANT_IDS`, on both API and worker | `SUPERVISOR_E2E_AGENT=pilot` | `make supervisor-e2e CASE=onboarding-pilot` |

The unscripted fixture phase expects every model run to be refused (`SPECIALIST_FAILED`). The
scripted phase uses the checked-in configuration-draft response sequence and verifies a real worker-created line draft;
the draft hand-off test still stubs only the draft-run read and is not agent evidence.

`CASE=onboarding-pilot` (WIS-631, `web/tests/onboarding-pilot.spec.ts`) onboards the two pilot sites that
`supervisor-demo-seed` creates (users `chakan-owner`, `chakan-lead`, `chakan-approver` and the same three for `pune`,
each a member of one site only): SOP upload on Documents, where the owner marks it authoritative and confirms it
current (WIS-641), a photographed page read by the fixture vision model and marked authoritative only after the
owner ticks that they compared it with the original (WIS-643), an agent turn on Interviews, approval by the approver (who was not interviewed and cannot open the
interview) on Setup → Facts to review (WIS-641), a snapshot published on Setup → Approved process (WIS-641), and a
cited answer on Today, followed by 28 cross-tenant probes per tenant that must each match the 404 for an ID
that never existed. The owner commissions the area, the process and its value-stream checklist on
Setup → Processes (WIS-644). The pilot phase runs the API and worker with
`SUPERVISOR_AGENT_VISION_MODEL=fixture-vision` and `SUPERVISOR_AGENT_VISION_MODEL_REVISION=sha256:vision`, so the pilot
fixture script's `ocr` purpose transcribes the page; this is a scripted fixture, not OCR evidence (rule 09).
`make supervisor-e2e` refuses an unknown `CASE`.

`SUPERVISOR_TEST_ADMIN_URL` is required for integration fixtures. Missing dependencies, skips and unknown selectors fail. Browser test requires seeded credentials and running API/worker/Keycloak. `PLAYWRIGHT_CHROMIUM_EXECUTABLE` optionally selects an already-installed Chrome binary; record its version when used. Tests exercise concurrent replay, cross-tenant denial, membership revocation, immutable receipts, rollback, restarted workers, six transient failures, real workflow history replay, CSRF and browser login/logout.

### Agent evaluation harness (WIS-280, slice 1)

```bash
# Real PostgreSQL through SUPERVISOR_TEST_ADMIN_URL and SUPERVISOR_TEST_DATABASE_URL:
make supervisor-eval MODE=deterministic
# Needs the pinned SUPERVISOR_AGENT_PROVIDER=local and SUPERVISOR_AGENT_* settings; otherwise BLOCKED:
make supervisor-eval MODE=actual
```

The harness loads `evals/supervisor/manifest.json` before it runs anything. The loader refuses
unknown fields, missing thresholds, duplicate case IDs and an incident that spans partitions. It
also refuses thresholds that differ from the frozen digest pinned in `cmd/supervisor-eval`, and a
`corpus_sha256` that does not match the cases. Each case gets its own tenant through the real
WIS-272/273 path and runs through the WIS-275 runtime. The JSON report goes to `build/qualification/`.

`MODE=deterministic` scripts the model with the deterministic-fixture provider and runs each case
once. Every line is labelled `DETERMINISTIC — not actual-model evidence`, and the run can never
report `qualified`. `MODE=actual` runs each eligible case three times. It is BLOCKED (exit 2)
without a configured, reachable actual provider or database. Every run also executes two gate
controls, a policy-violating fixture and a modified-citation fixture, and fails if either one
passes.

The committed corpus is a synthetic development smoke subset of 20 cases. It is not the 100-case
baseline or the 240-case release corpus. Slice 1 does not judge citation correctness,
triage/abstention, detection, latency p95 or specialist comparison. Because of that, an actual run
reports BLOCKED and does not qualify.

**Onboarding and chat case sets (WIS-640).** `CASE=investigation,onboarding,chat,ocr` selects suites.
An empty `CASE` runs all three; an unknown name is BLOCKED (exit 2). For example:
`make supervisor-eval MODE=deterministic CASE=onboarding,chat`.

- **How each run executes:** through the public API in-process, over the disposable database. Each
  onboarding case uploads and designates its SOP, commissions a process, interviews a new lead and
  runs `Runtime.InterviewTurn`. The chat world authors facts from an SOP, has a second supervisor
  approve them, publishes a snapshot, and answers each question with `Runtime.ChatAnswer`. A second
  tenant holds text that no answer may contain.
- **Deterministic mode:** asserts the server's exact outcome for each scripted output, for example
  `ENTITY_NOT_IN_SOURCE`, `NUMBER_NOT_IN_SOURCE` or `TOOL_NOT_ALLOWED`.
- **Hard gates, both modes:** a run must persist no name or number outside a captured span, must
  produce no review or snapshot by a model, must get no tool data outside the purpose's allowlist,
  and must make no foreign-tenant read or disclosure.
- **Measures:**
  - retrieval recall@10 is server code, so it gates both modes;
  - an actual run also measures supported-fact recall, abstention and exact quantities against the
    frozen thresholds.
- **Gate controls:** three controls must FAIL.
- **Why an actual run cannot qualify yet:**
  - the onboarding thresholds are an engineering proposal (`status: proposed_pending_review`);
  - the case sets are development subsets, smaller than the frozen 240-case chat release corpus.
- **Hosted route:** set `SUPERVISOR_AGENT_HOSTED_*` and a key file. A missing key file is BLOCKED.
- **OCR (WIS-625), `CASE=ocr`:**
  - Each page is rendered as a real PNG (a fixed bitmap font, scaled 3×), uploaded to `/source-scans` and read by `Runtime.ExtractScan`.
  - An adversarial page then has an interview turn run over its stored text; the turn must be refused and must approve nothing.
  - Deterministic mode asserts each job's state and quarantine reason.
  - An actual run needs `SUPERVISOR_AGENT_VISION_MODEL` on the local provider (BLOCKED otherwise) and measures how many clean pages are transcribed exactly. Its thresholds are an engineering proposal awaiting sign-off.

### Scripted fixture provider for browser e2e (WIS-271)

For a local browser e2e run that needs a successful interview turn and a line draft, set
`SUPERVISOR_AGENT_PROVIDER=fixture` and
`SUPERVISOR_AGENT_FIXTURE_SCRIPT=deploy/supervisor/agent-fixture-script.v1.json` on the worker. Keep
the script file mounted read-only in the worker. The script lives under `deploy/supervisor/` because
the worker reads it as a server-side fixture; it is not part of the browser bundle. The API and worker
still need the normal fixture model and revision pins.
Any process with `SUPERVISOR_AGENT_FIXTURE_SCRIPT` set, including the API, requires the file to exist
at startup or it refuses to start.

The script has separate interview and configuration-draft response sequences. Its placeholders read
the run ID, captured evidence refs, requested requirement or kind, and the most recent `evidence.get`
span IDs from the model conversation. The provider has no database access. The checked-in draft
sequence covers the requested `line` kind. Scripted responses remain labelled
`deterministic-fixture` with `provider_actual=false`; they cannot pass `MODE=actual` or qualify actual
model evidence.

## Boundaries and handoff

### Customer provisioning and initial owner bootstrap

WiseDo operations runs `provision` for each customer; each tenant represents one customer and each
site represents one plant. The owner must already exist in the configured identity provider. The
command uses `SUPERVISOR_PROVISIONING_URL` as `supervisor_migrator`; the API runtime role is refused.
Migration 051 adds nullable `sites.timezone` for existing sites and the forced-RLS
`provisioning_audit` table. The migration role owns `tenants` and `sites`, so no insert grant change
is needed: the command sets the new tenant scope inside its transaction. Runtime has no grant on
`provisioning_audit`; `supervisor_migrator` has `SELECT` and `INSERT` grants for request-ID lookup
and provisioning. Apply migrations first.

Choose a new request UUID for each intended provisioning operation. Reusing it with changed input
is refused; after its invitation is issued, replay fails with `invitation already issued`. A different
request UUID creates a distinct tenant/site even when names match. Protect the output path and deliver
the token privately; the token is written alone to an exclusive mode-0600 file and never logged. The
command prints the created `tenant_id` and `site_id` to stdout, never the invitation token.

```bash
export SUPERVISOR_PROVISIONING_URL='postgresql://supervisor_migrator:<migration-password>@<db-host>/<database>?sslmode=verify-full&sslrootcert=<ca-file>'
./build/supervisor provision \
  --tenant-name 'Customer name' \
  --site-name 'Plant name' \
  --timezone 'America/Detroit' \
  --owner-issuer 'https://identity.example/realm' \
  --owner-subject '<exact verified OIDC subject>' \
  --operator 'WiseDo operator identity' \
  --request-id '<new UUID>' \
  --output '/secure/path/owner-invitation.token'
```

The invitation expires after 15 minutes. The owner signs into the configured identity provider and
redeems the token through `POST /v1/bootstrap/redemptions` as described below. The command does not
create identity-provider users. Run `make supervisor-integration CASE=provision` against actual
disposable PostgreSQL to verify provisioning, idempotency, least privilege and redemption.

To retire a provisioned tenant, use a new request UUID. Deactivation is irreversible in v1, is
recorded in `provisioning_audit`, stops worker dispatch on its next tick, and makes the next
protected API request return the same non-disclosing 404 used for membership revocation:

```bash
./build/supervisor provision deactivate \
  --tenant '<tenant UUID>' \
  --operator 'WiseDo operations: <name>' \
  --request-id '<new UUID>'
```

The worker discovers active tenants on each tick. `SUPERVISOR_RELAY_TENANT_IDS` is an optional
allowlist that narrows discovery; omit it or set `[]` to include every active tenant. A discovery
failure skips all work for that tick and leaves queued rows for the next successful tick.

If the command reports a commit error after writing the token file, the database result may be
unknown. Check `provisioning_audit` by request ID with the migration role before re-running; if you
must re-run, use a new output path.

### Onboarding load gate

`deploy/qualification/onboarding-load.json` is a frozen manifest for `cmd/supervisor-load`
(`"kind": "onboarding"`). It creates 4 tenants. Each tenant gets its first site through
`supervisor provision` (with a provisioning audit row and an owner invitation); the other 2 sites
per tenant are inserted directly as `supervisor_migrator` and have no provisioning audit row. The
run then sends 100 intake batches of 20 events and 100 station record writes per site through the
API as the runtime role. An outbox consumer folds concurrently, and tenant isolation is checked as
the runtime role throughout. The thresholds (intake p95/p99 1000/2500 ms, station write p95/p99
500/1500 ms, provision max 10 s, error rate 0, max RSS 512 MiB, peak queue depth 4800 events with
zero residual, cross-tenant rows 0) were committed before the first measured run. Every event must
be accepted, folded and not quarantined. The tool refuses a manifest with any threshold missing.

The gate creates and drops its own database, so it runs only with `SUPERVISOR_ENVIRONMENT=local`
against a loopback PostgreSQL:

```sh
go build -trimpath -o build/supervisor-load ./cmd/supervisor-load
SUPERVISOR_ENVIRONMENT=local SUPERVISOR_TEST_ADMIN_URL=... SUPERVISOR_TEST_DATABASE_URL=... \
  SUPERVISOR_MIGRATION_PASSWORD=... ./build/supervisor-load -manifest deploy/qualification/onboarding-load.json
```

Results: `PASS` (exit 0), `FAIL` (exit 1: a threshold breach or product error, always listed), or
exit 2. Exit 2 is BLOCKED (missing or rejected credentials, unreachable or non-local database) or
`NOT_QUALIFIED`: every threshold was met but the run is not qualification evidence. PASS requires
all of the following, measured by the tool:

- the reference host (client CPU count, memory and GOMAXPROCS);
- the PostgreSQL container's cgroup CPU and memory limits, read through the server;
- the manifest bytes committed at HEAD;
- a clean tree and a known SHA;
- a `supervisor` binary whose embedded VCS revision is HEAD (built by the tool, or supplied as
  `SUPERVISOR_LOAD_SUPERVISOR_BIN`).

The report `build/qualification/onboarding-load-<sha>.json` records the SHA, the exact command,
the host manifest, latencies, event counts, queue depth, isolation probes, failures and
qualification blockers. `go test -race -tags=integration -p 1 ./cmd/supervisor-load` runs a small
variant through a test-only path that can never PASS. It also runs the negative controls: a breach,
a loosened RLS policy (FAIL), and missing or wrong credentials (BLOCKED).

### Existing administrator owner-invitation command

The administrator provisions an empty tenant/site and obtains the intended owner's exact OIDC issuer and subject through the identity provider's trusted administration channel. Email addresses and first login do not establish ownership. Using a separate privileged provisioning connection, run:

```bash
export SUPERVISOR_PROVISIONING_URL='<privileged provisioning PostgreSQL URL>'
export SUPERVISOR_BOOTSTRAP_TENANT_ID='<provisioned tenant UUID>'
export SUPERVISOR_BOOTSTRAP_SITE_ID='<provisioned site UUID>'
export SUPERVISOR_BOOTSTRAP_SUBJECT='<exact OIDC subject>'
export SUPERVISOR_BOOTSTRAP_INVITATION_FILE='<new private file path>'
./build/supervisor bootstrap-invite
```

The configured `SUPERVISOR_OIDC_ISSUER` must match the owner's authenticated identity. The command creates an exclusive mode-0600 JSON file with a random invitation token, scope and expiry; it refuses to overwrite an existing file and never prints the token. Deliver this file through the agreed private out-of-band channel. The API process must never receive `SUPERVISOR_PROVISIONING_URL`. The invitation expires after 15 minutes and is stored only as a SHA-256 hash in PostgreSQL.

After OIDC sign-in, the owner sends same-origin `POST /v1/bootstrap/redemptions` with `Content-Type: application/json`, the current `X-CSRF-Token` from `/v1/me`, and `{ "token": "<private token>" }`. Do not put tokens in URLs. A 201 response returns the granted scope/role without the secret. Reload `/v1/me` to confirm access. A consumed, expired, wrong-identity or wrong-scope token is rejected; concurrent redemptions cannot create duplicate or second initial owners. Existing membership is never elevated/reactivated by bootstrap. If a response is lost, check `/v1/me`; replay does not reissue a consumed capability.

This compatibility command issues invitations for sites that already exist. New clean installs should use `provision` above. Invitation-file deletion after confirmed enrollment is the administrator's responsibility.

**Semantic retrieval (WIS-627).** The PostgreSQL image is `pgvector/pgvector:0.8.0-pg17` (Debian). pgvector is not a trusted extension: `bootstrap.sh` creates it as the superuser (`CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`) in the `supervisor` database and in `template1` (so databases created later, such as disposable integration-test databases migrated by the non-superuser migrator, inherit it), and migration 075 only finds it. An existing integration-test server needs the same statement in `template1`. **Upgrading an existing database:** before migrating to 075, a superuser runs that statement in the Supervisor database; an existing volume moved from the earlier `postgres:17-alpine` image changes libc collation, so run `REINDEX DATABASE supervisor` (and the Keycloak and Temporal databases) after the switch. Backups taken with `pg_dump` include the extension; restore into a server that has pgvector installed. Semantic retrieval is off unless `SUPERVISOR_EMBEDDINGS_ENABLED=true` with `SUPERVISOR_EMBEDDINGS_BASE_URL`, `_ENDPOINT_ALLOWLIST`/`_ENDPOINT_ADDRS` (local or private, pinned, as for the model), `_MODEL`, `_MODEL_REVISION` (a 1024-dimension model such as Qwen3-Embedding-0.6B) and optionally `_RERANK_MODEL` with `_RERANK_MODEL_REVISION`, on both API and worker. `SUPERVISOR_EMBEDDINGS_TIMEOUT` (default `3s`, 100ms to 1m) bounds the question-time embed and rerank together: one deadline covers both calls, so a question waits at most that long before it is posted (WIS-700); `SUPERVISOR_EMBEDDINGS_INDEX_TIMEOUT` (default `60s`, 100ms to 5m) bounds each span-indexing request, since a CPU-only endpoint can take several seconds for one text. A worker log line `embedding endpoint timed out after ...` means indexing needs a longer budget or a faster endpoint. Indexing runs on its own worker loop, so a slow endpoint never delays dispatch. CPU embedding endpoints need `SUPERVISOR_EMBEDDINGS_TIMEOUT` of about 5–8s, or chat uses word fallback. The worker embeds spans each tick; until a question's candidate spans are all embedded for the configured revision, chat ranks by word match and labels the reply `word_fallback`. A rerank call that fails or times out is skipped: the reply keeps the fused order and is labelled `hybrid_unreranked`, which the Today screen notes (WIS-647). Rerank on a CPU endpoint can exceed the default 3s `SUPERVISOR_EMBEDDINGS_TIMEOUT` (WIS-647 measured p99 3.06s on Apple Silicon), so set it to about 8s, enough for the embed and the rerank together, when a rerank model is configured. The finished answer keeps its label (before WIS-647, finishing an answer dropped it). Changing the model revision re-embeds; revisions are never mixed. `supervisor-eval -retrieval=word|hybrid|hybrid_rerank CASE=chat,chat_synonyms` measures recall per mode (WIS-647).

Migrations from 001 onward (`internal/migrate/`; 075 on this branch) are additive; previously published migrations are immutable. New binaries reject a database with missing, changed, gapped or unknown migration history. Apply migrations before starting the new binary. Previous binaries also refuse a newer schema, so rollback requires keeping a schema-compatible binary or a verified restore; do not rewrite migration history or assume old binaries support the new schema.

### Recent identity proof and membership administration

Use **Confirm identity** in the existing application to perform a fresh OIDC login before a privileged action. `POST /auth/reauthenticate` requires the current session, same-origin CSRF header and returns an `authorization_url` for top-level browser navigation. The callback requires the original session, exact identity and a valid signed `auth_time` at or after this flow began. It rotates the cookie/CSRF value while retaining the original eight-hour absolute expiry. Missing, stale or future authentication times never count as recent proof; ordinary sessions without this claim remain usable for nonprivileged access.

An active site owner can read `GET /v1/sites/{site}/memberships?limit=50&cursor=...`; limits are 1–100, with a returned `next_cursor`. To create or update access, send `POST /v1/sites/{site}/membership-changes` with same-origin CSRF, an `Idempotency-Key`, and `If-Match: "0"` for a new member or the exact quoted current revision for an existing one. JSON body is `{ "issuer": "<same configured issuer>", "subject": "<verified exact target subject>", "role": "operator", "active": true }`. All fields are required; target identity must be verified through the trusted provider administration channel. Supported roles are owner, supervisor, operator, reviewer and viewer. The response includes the new revision and matching ETag. A customer membership-management screen is not yet included.

Changes require a database-verified session with authentication within 15 minutes and current owner rights. Owners cannot mutate their own membership; another current owner must make that change. Serialized administration prevents removing all active owners. Replay requires current authority and fresh proof, returns the same result for the same payload/precondition, and rejects changed input with 409. Stale revisions return 412; missing revision or recent proof returns 428. Revocation takes effect on the next protected request or queued activity dispatch. SQL locks coordinate with in-flight protected operations; revocation does not claim to undo an already committed effect. Each successful change and its prior/result revisions are recorded atomically in an append-only runtime audit table.

### Reported plaintext evidence — first WIS-270 increment

Current site owners, supervisors and operators can capture bounded text through `POST /v1/sites/{site}/sources` with same-origin CSRF, `Idempotency-Key`, `If-Match: "0"` for a new source and JSON `{ "source_id": "<new UUID>", "kind": "manual", "title": "<title>", "text": "<captured text>" }`. Supported kinds are manual, interview and document; **document means supplied plaintext, not PDF/DOCX parsing**. Titles are 1–200 Unicode codepoints; text is nonempty valid UTF-8 with at most256KiB and no NUL. Malformed Unicode is rejected rather than silently replaced. SHA256 covers the decoded UTF-8 text bytes actually persisted, not the JSON transport envelope or an unavailable original file. Every revision remains `reported`; no request can mark it approved.

Revise a source by repeating its source_id/kind with new content and the exact current `state_revision` in If-Match. Revisions append immutable history. `GET /sources/{source}?revision=N` retrieves an authorized historical revision; omit revision for current content. `GET /sources?limit=50&cursor=...` returns metadata only, with at most100 entries and an explicit next_cursor. All these paths are under `/v1/sites/{site}`. Current active site members can read this slice's evidence; source-specific confidentiality policies are not yet implemented. Replaying a capture returns the original immutable source revision with current head/access metadata after rechecking authority.

Capture a citation using `POST /sources/{source}/spans` with the same mutation headers and `{ "revision": 1, "start_char": 0, "end_char": 10 }`. Offsets are zero-based Unicode codepoints, with an exclusive end, bounded to the stored revision and at most4096 codepoints per quote. `GET /source-spans/{span}` resolves the original text and source hash; later source revisions never rewrite a citation. Historical accessibility does not imply current operational eligibility; the review endpoints below enforce approval and freshness.

An owner with recent identity proof can irreversibly revoke a source using `POST /sources/{source}/revocations`, current If-Match, a new Idempotency-Key and `{ "reason": "<1–1000 codepoints>" }`. The append-only revocation ledger immediately governs subsequent source/span retrieval, listing and content-bearing replay. Responses expose no revoked content. Bytes remain preserved for privileged retention handling; revocation is not deletion, and no customer purge/restore workflow is claimed. Runtime cannot update/delete history. Privileged schema-owner deletion remains possible for authorized retention tooling, which must supply its own audited policy before production use.

Run `make supervisor-integration CASE=evidence` against actual disposable PostgreSQL. Tests include real HTTP authorization, Unicode hashes/spans, concurrent revisions, revoked-source cached IDs, audit-write failure rollback and lock-coordinated read/revocation. The intake API has no customer capture UI, automatic extraction, coverage calculations or model calls yet. Human claim review and approved process snapshots are available separately below. These remaining WIS-270/WIS-271 capabilities must not be inferred from successful source capture.

### Source authority for claim review

An owner with recent identity proof can send `POST /v1/sites/{site}/sources/{source}/authority-decisions` with CSRF, Idempotency-Key and `If-Match: "0"` for the first authority decision. The body is `{ "source_revision": 1, "expected_source_state_revision": 1, "verdict": "designate", "reason": "Customer-maintained operating procedure" }`. Both source preconditions must match the current source; later authority decisions require the exact current authority revision in If-Match. Source drift returns409 SOURCE_CHANGED, authority If-Match drift returns412, and missing recent identity proof returns428. A current owner and fresh proof are required again on replay.

The server records an immutable decision with the source's actual SHA256, access epoch and exact revision, plus the verified actor and authentication time. Original source bytes and their reported classification remain unchanged. `GET /sources/{source}/authority?revision=N` returns an authorized historical decision; omitting revision selects the latest. Its ETag identifies the selected authority decision. Current site members can read these records. A designation is review_evidence_eligible only while it is the latest authority decision and its source revision, hash and access epoch remain current and accessible.

Use verdict `withdraw` to withdraw an existing designation, with current source preconditions and authority If-Match. A later designation gets a new decision ID and cannot reactivate an earlier decision. Source updates make old designations ineligible; source revocation hides their read/replay responses. One designated source can support human review, but designation alone approves no claim, snapshot or machine action. Copy lineage and independence classification remain subsequent work.

### Typed knowledge candidates and entity revisions

Under `/v1/sites/{site}`, `POST /entities` creates a candidate with `{ "entity_id": "<UUID>", "type": "operation", "display_name": "Inspect assembly", "source_id": "<captured source UUID>", "source_revision": 1 }`. Optional aliases are bounded to20; an asset additionally requires `asset_kind` machine or line. The referenced source must be current and accessible. Use the same CSRF/Idempotency-Key headers and `If-Match: "0"`; the initial entity ETag is `"1"`. Supported entity types are site, area, asset, process, operation, order, material, tool, role, qualification, document and constraint. This increment does not merge aliases or resolve external identities.

Update an entity under the same ID using its current quoted revision in If-Match. Name, aliases and source evidence can change; type and asset_kind remain fixed for that identity. Each update appends immutable history. `GET /entities/{entity}?revision=N` selects a historical revision, while omitting revision selects the current one. Responses expose revision and current_revision separately; ETag identifies the returned revision. Updating an entity never changes a historical claim's entity or source references.

Migration007 maps existing entities and historical claim references to entity revision1 without rewriting original claim JSON, source hashes or receipt fingerprints. Existing entity IDs remain stable. Clients creating or revising claims must now supply explicit entity revision pins. A legacy claim receipt may replay with pins1 only when its original immutable payload and precondition still match; it remains subject to current access and revocation checks. Updating an entity does not silently refresh dependent claims: those claims need a new revision with reviewed evidence and current pins.

`POST /claims` creates or revises a candidate with `claim_id`, `subject_id`, a supported `predicate`, exactly one of `object_entity_id` or `literal`, `effective_from`, `evidence_class` and1–20 unique `source_span_ids`. Optional `effective_to` must be later than effective_from. Timestamps require explicit offsets, normalize to UTC and allow at most six fractional digits. A literal has `type`, `value` and optional `unit`; decimal values and optional confidence use canonical decimal strings with at most six fractional digits. Confidence never grants authority. Relations validate subject/object types; `part_of_asset` requires machine→line. All provenance, including subject/object origins, must be current when creating a new revision. Client-supplied state, reviewer or approval fields are rejected; extractor identity is reserved for trusted server activities and rejected on this public endpoint.

Use If-Match `"0"` for a new claim and the exact current claim revision for an update. New writes also require positive subject_revision and, for entity relationships, object_revision. These must match current entity heads; a stale entity pin returns 409 SOURCE_CHANGED. A stale target If-Match returns 412 REVISION_CONFLICT. Literal claims must omit object_revision or use zero. `GET /claims/{claim}?revision=N` returns an immutable revision and separate current_revision; the ETag identifies the returned revision. `GET /claims?limit=50&cursor=...&subject_id=...&state=candidate` lists bounded metadata; `GET /entities` lists bounded current entities. Both return next_cursor when another page exists. All current site roles can read; only owner/supervisor/operator can write. Replays recheck current access. Historical candidates with superseded source or entity revisions remain readable with sources_current=false; revoked citations or pinned entity origins conceal the candidate, including cached-ID reads and replay. These are reported candidates, never approved operating instructions.

Candidate cycle checks cover precedes, located_in and part_of_asset using current, accessible provenance and the common half-open effective interval of the entire path. Disjoint or touching reverse edges are allowed; overlapping cycles are rejected. Searches above 1000 interval states fail closed.

GET `/v1/sites/{site}/claims/{claim}/conflicts` returns immutable exact-revision comparisons with bounded cursor pagination (`revision`, `limit`, `cursor`). Only current claims, entity pins and accessible current sources are eligible. Different single-valued objects or same-unit decimal values produce `value_conflict`; different units produce `unit_review`, and differing text produces `text_review`, without asserting that wording alone is contradictory. Multiple-valued relations are excluded. Revoked peer evidence removes that pair from reads. Empty results never imply approval. Writes compare at most 1000 overlapping candidates and fail closed above that bound.

Migration 009 backfills eligible existing pairs transactionally and restores FORCE RLS before commit, preserving old claim payloads. Its pairwise comparisons within each subject/predicate group require a maintenance window; measure large groups and implement staged backfill before large-scale rollout. Entity reconciliation and coverage remain subsequent work. Approved temporal graph validation, snapshots and explicit conflict resolution are described below. No machine command or LLM call is enabled by a claim. Run `make supervisor-integration CASE=knowledge` for actual PostgreSQL service and HTTP tests, including provenance changes, isolation, CAS, replay and audit rollback.

### Site inventory revision

GET `/v1/sites/{site}/knowledge-inventory` returns `{"schema_version":1,"revision":0}` for an unchanged initial inventory. All current site members may read it. The revision is an opaque change token, not a claim count, approval or assessment of customer coverage. Compare revisions only within the same tenant and site; existing sites begin at zero when migration013 installs tracking, even if they already contain knowledge.

Database triggers advance the token transactionally when captured evidence, entities, claims, conflicts, source authority, reviews, revocations or resolution dependencies change. Several writes in one transaction may advance it more than once. Reads, audits and unchanged receipt replay leave it stable; rollback also rolls back the token. Runtime can read the counter but cannot set or reset it. Privileged retention writes must supply the matching tenant scope. Inventory reads serialize with site mutations using the existing lock boundary; measure contention before replacing it. Snapshot creation uses this token for a consistent inventory cut and historical reads report later drift. Bounded graph retrieval rejects inventory drift as described below.

### Immutable approved process snapshots

POST `/v1/sites/{site}/process-snapshots` accepts `{"snapshot_id":"<new UUID>","reason":"Reviewed captured process knowledge"}`. Supply CSRF, Idempotency-Key and quoted If-Match matching the current **site inventory revision**, obtained from `knowledge-inventory`. Current owner/supervisor membership and identity proof within15minutes are required, including replay. Inventory drift returns412; reusing a snapshot ID with a different request returns409. Creating a snapshot does not change the inventory revision.

The server selects the site's eligible current human-approved claims under the site lock; it never approves candidates or accepts a client-selected subset as the full inventory. Ineligible claims are excluded. An empty snapshot is valid but establishes no process coverage. The selected claims retain exact entity, review, source authority and conflict-resolution references. Temporal routing and hierarchy graphs must be acyclic throughout their effective intervals, and a machine cannot belong to different approved lines during overlapping intervals. Touching half-open intervals are allowed.

Creation fails closed above500 current approved-review candidates,200 referenced entities,1000 historical conflict dependencies across candidates, or64KiB of canonical content. These are bounded implementation limits, not a production throughput qualification. The pure graph validator checks distinct relation start times with a fixed work budget. Snapshot contents are sorted and hashed with SHA256; the versioned canonical content includes tenant/site, inventory revision and exact dependency pins. Snapshot ID, reason, actor and creation time are separate immutable metadata, so creating another snapshot of the same inventory yields the same content hash.

GET `/v1/sites/{site}/process-snapshots/{snapshot}` is available to current site members. Reads and creation replays recheck pinned source access, including conflict-resolution peers; revoked dependencies return404. Inventory drift sets `stale=true` while preserving the original contents and hash. The token does not replace current authorization or effective-time checks. A snapshot records eligible approved knowledge, not customer completeness or permission to execute actions.

Migration014 atomically stores the immutable snapshot, exact claim/review and entity references, receipt, audit and a metadata-only `knowledge_outbox` event. Outbox delivery is a subsequent increment; inserting an event does not claim delivery. Existing command outbox processing remains separate.

### Bounded approved graph retrieval

GET `/v1/sites/{site}/process-snapshots/{snapshot}/graph` requires one or more repeated `root=<entity UUID>` and `predicate=<supported predicate>` parameters, plus `as_of=<RFC3339 timestamp with offset>`. Optional `hops` defaults to1 and accepts0–2; `direction` defaults to `outgoing` and also accepts `incoming` or `both`. `max_entities` defaults to200 and `max_claims` to500, with positive values no greater than those caps. At most20 root entries and32 predicate entries are accepted before deduplication. Unknown parameters, repeated scalar parameters and malformed query encoding are rejected.

Traversal uses the immutable snapshot only. Incoming navigation follows an existing relationship in reverse while preserving its original subject, predicate and object; it does not invent an inverse fact. Claims must be effective at `as_of`, using half-open intervals. Requested literal properties are included for admitted nodes, including nodes at the hop boundary. Zero hops returns roots and their requested literal properties. Missing roots return404; no search of unapproved candidates explains their absence.

Current membership, pinned source access and inventory freshness are checked under the same site lock as traversal and its read audit. A stale snapshot returns409 `SNAPSHOT_STALE`; revoked provenance returns404. Historical snapshot reads remain available when stale and accessible. Obtain and approve a fresh snapshot explicitly rather than silently switching graph versions.

The response echoes its selection parameters, snapshot ID/hash and inventory revision, with stable entity/claim ordering and `truncated`. The complete encoded JSON response is at most64KiB. A relationship and any required endpoint are admitted together; an omitted edge is never used as a traversal path. Capacity omissions set `truncated=true`; reaching the requested hop boundary alone does not. Partial results do not prove that omitted relationships are absent or authorize operational actions. This bounded Go projection replaces recursive live SQL traversal; PostgreSQL remains the authority and access boundary. Large-site performance qualification remains open. Run `make supervisor-integration CASE=WIS-270` or `CASE=knowledge` for the same real knowledge and HTTP integration suites.

### Scoped discovery requirement inventories

POST `/v1/sites/{site}/discovery-inventories` registers or revises `{inventory_id,area_id,area_revision,process_id,process_revision,capability,reason}`. UUIDs and positive exact entity revisions are required; the area and process must be current, accessible entities of those types. Use CSRF, Idempotency-Key and quoted If-Match (`"0"` initially, then the discovery inventory's current revision). Writes and replay require a current owner/supervisor and identity proof within15minutes. Reasons are1–1000 Unicode codepoints. The server rejects client-supplied requirements, weights, approval or readiness flags.

Template version1 defines nine `shortfall_investigation` requirements: asset identity, asset/process routing, responsible role, order/window, target-rate segments and planned breaks, good-count source, timestamp/unit mapping, evidence freshness policy, and exception escalation contact. `internal_task_coordination` adds recipient authority and completion evidence. Interview roles route future questions; they grant no review rights. Definitions name required evidence and do not automatically match arbitrary narrative claims to those requirements.

An inventory's area ID, process ID and capability are fixed; changing them requires a new inventory ID and generates new requirement IDs. Revisions may refresh the same anchors' exact revisions and the reason. Requirement IDs remain stable within an inventory, while every revision preserves its own complete template rows. Expansion inherits no approval or readiness. Discovery revisions are separate from the knowledge inventory epoch: editing requirements alone does not alter graph snapshots.

GET `/v1/sites/{site}/discovery-inventories/{inventory}` selects the latest revision; `?revision=N` selects immutable history. Responses include selected/current revisions and `scope_current`. Superseded anchor or source revisions make the scope stale; revoked anchor evidence returns404, including replay. All current site members can read accessible history. Migration015 stores scoped identity, head, revisions and requirements with FORCE RLS and atomic receipt/audit. It makes no claim that a capability or customer site is ready.

POST `/v1/sites/{site}/discovery-inventories/{inventory}/requirements/{requirement}/decisions` records `{inventory_revision,verdict,reason}` with verdict `bind`, `not_applicable` or `withdraw`. Use CSRF, Idempotency-Key and quoted If-Match for the decision head within that exact inventory revision (`"0"` initially). Current owners/supervisors need recent authentication. Structural binding also requires `snapshot_id`, RFC3339 `as_of`, and 1–100 unique exact `claim_refs` (`claim_id`, `revision`) from a current approved snapshot. Other verdicts forbid those evidence fields. Non-applicability requires a reviewer justification; it is not automatic.

Binding initially supports three structural requirements. Asset identity requires each selected asset to have direct `located_in` evidence for the exact area, `performs` an operation and `part_of` the exact process. Routing uses the same witness plus connected operation precedence evidence; single-operation processes need no precedence edge. Responsible role requires a direct process-to-role assignment. Every selected claim must contribute to the witness and be effective at `as_of`. Inherited locations, operation-level role assignments and narrative substitutes for unsupported operational requirements are not accepted.

GET the same decisions path with required `?inventory_revision=N` and optional `&revision=N` reads the latest or selected immutable decision. Responses include a quoted ETag and `decision_eligible`, which means version currency only. Replaced decisions, superseded inventory anchors or stale snapshots are ineligible. Revoked provenance returns404, including a withdrawal's predecessor evidence; new bind/non-applicability decisions can replace revoked bindings without exposing them. Migration016 preserves exact snapshot and claim pins, immutable history, tenant isolation and atomic receipts/audit. Decisions do not advance the graph epoch.

GET `/v1/sites/{site}/discovery-inventories/{inventory}/coverage` projects the latest inventory and all its requirements in one serialized site transaction. It accepts no query parameters. Every current site member can read it; revoked evidence fails the whole request with404 rather than appearing missing. The result pins the inventory revision, template/evaluator versions, graph epoch and evaluation time, and lists decision/snapshot references without source bodies or freeform reviewer reasons. A new inventory revision never inherits old decisions.

Coverage evaluator 3 exposes `missing`, `stale`, `not_applicable`, `candidate` and `approved` states. Current structural bindings increment `bound_count`; qualification additionally requires explicit current policy and confirmation pins, unexpired verification and effective witness claims at one server `evaluated_at`. A binding without qualification pins stays a candidate with `freshness_policy_missing`. Every read re-checks a binding's witness against the current scope rules: a decision accepted under an older rule (for example one reaching another process's operation through a shared material or `precedes`, refused since WIS-615's audit fix) stays in history but projects as `stale` with `witness_scope_invalid`, is not counted as bound or approved and reads `decision_eligible=false` until a person re-decides it. A stored witness that no longer passes the binding contract projects `witness_contract_invalid`, and one for a cell that no longer supports binding projects `binding_unsupported`; both are `stale` in the same way. Evaluator 3 added this re-check (evaluator 2 trusted a witness once written). `applicable_count` is the fixed9/11 total minus current reviewed exclusions; stale exclusions return to the denominator. Qualified cells increment `approved_count` and expose `expires_at`, bounded by both document verification expiry and selected claim interval ends. The report includes the earliest such expiry. A binding's `as_of` remains its reviewed witness instant; evaluating or replaying it does not renew anything. Unsupported operational evaluators keep `evaluation_state=partial_support`, `knowledge_ready=false` and `authorization_state=disabled`, even if they are marked N/A. Reads do not change the graph epoch.

### Document currentness confirmations

POST `/v1/sites/{site}/sources/{source}/currentness-decisions` accepts `{source_revision,expected_source_state_revision,authority_revision,authority_decision_id,verdict,reason}`. Verdicts are `confirm_current` and `withdraw`. Use CSRF, Idempotency-Key and quoted If-Match for the confirmation head (`"0"` initially). Only owners with authentication within15minutes can write or replay; confirmation requires a current document source and its exact current designated authority decision. Manual and interview sources are not supported by this document confirmation contract.

The server records `verified_at` when the owner explicitly confirms currentness. It is a human assertion that the document is still current, not its publication date, machine observation time or upload timestamp. Replaying a request preserves that timestamp. Responses pin the source revision/hash/access epoch and authority decision. `verification_current` reports version currency only; it does not apply a freshness TTL or approve coverage.

GET `/v1/sites/{site}/sources/{source}/currentness` reads the latest confirmation; optional canonical positive `?revision=N` selects history. Current site members can read accessible history. Replacement, source revision changes or authority changes invalidate old confirmations without resurrection. Withdrawals require the exact predecessor pins and current source-state precondition, can withdraw superseded confirmations, and create no new verification time. Revoked source access returns404 on reads and replay. Migration018 preserves immutable tenant-scoped decisions, exact predecessor/source/authority references and atomic audit/receipts. Confirmations do not change the graph epoch.

### Explicit document freshness policy and qualification

POST `/v1/sites/{site}/document-freshness-policy-decisions` records one site's versioned policy using `{verdict,max_age_seconds,reason}`. Only current owners with recent authentication may define or withdraw it, using CSRF, Idempotency-Key and quoted policy-head If-Match. `define` requires an explicit integer duration from1 to9223372036seconds (the Go duration arithmetic limit); `withdraw` accepts no duration value and requires a prior definition. There is no implicit TTL. The basis is fixed to `owner_currentness_verified_at`; clients cannot replace it with upload time. GET `/v1/sites/{site}/document-freshness-policy` selects the latest revision, with optional strict `?revision=N` for history. All current members may read. Replacing or withdrawing a policy permanently invalidates earlier policy pins.

A structural requirement binding may additionally supply `freshness_policy_revision` and `verification_refs`, each `{source_id,verification_id,verification_revision}`. Qualified binding requires exactly one current document confirmation for every source dependency retained in its snapshot, including entity origins and conflict-resolution peers; up to500 references, without duplicates or extras. Missing, unsupported, stale or expired evidence rejects new qualified bindings with409 FRESHNESS_BLOCKED. Older bindings retain their original unqualified state until a reviewer explicitly replaces them. Qualified bindings preserve exact source/hash/access and confirmation pins under migration019, and dynamic `decision_eligible` includes policy/confirmation version currency but still excludes age qualification.

The `evidence_freshness_policy` requirement supports a policy-only binding: verdict `bind`, exact `freshness_policy_revision`, inventory revision and reason, without snapshot, as_of, claim or verification references. Its current approved definition qualifies that governance cell without a circular document verification requirement. Non-applicability and withdrawal forbid qualification fields. Migration019 adds policy heads/history and exact verification children with immutable scoped FKs and deferred cardinality checks. No policy or confirmation mutation advances the graph epoch; time alone can make coverage stale. Qualification requires `verified_at <= evaluated_at < verified_at + max_age_seconds`, and the same selected claim witnesses must be effective at evaluated_at. Manual/interview freshness and operational event freshness remain unsupported.

### Initial human claim review

`POST /v1/sites/{site}/claims/{claim}/reviews` accepts `{"claim_revision":1,"verdict":"approve","reason":"Reviewed against the cited current SOP"}` with CSRF, Idempotency-Key and `If-Match: "1"` matching that exact claim revision. Only current owner/supervisor memberships with authentication within15minutes can decide or replay a decision; stale proof returns428 REAUTHENTICATION_REQUIRED. New decisions require current claim, entity and source revisions. Approval additionally requires a directly cited, currently owner-designated source and every persisted value/text/unit conflict involving that exact claim revision to have an eligible explicit resolution selecting it. Missing authority or unresolved conflicts return409 REVIEW_BLOCKED. `reject` requires accessible current evidence but no designation, allowing unsupported claims to be rejected.

Each claim revision permits one immutable initial review; a different subsequent decision returns409 REVIEW_CONFLICT. Decisions, receipt and audit commit together. The response pins the exact authority decision used. `GET /v1/sites/{site}/claims/{claim}/review?revision=N` reads a selected review; omitting revision selects the current claim revision, and no review returns404. All current site members may read. The historical verdict remains immutable, while `approval_eligible` is recomputed on reads and replays. Claim/entity/source changes, authority withdrawal or redesignation, and newly active conflicts invalidate eligibility. Revoked claim provenance returns404. Rejection of a competing claim does not resolve a conflict automatically. A captured conflict keeps the exact target revision contested even if the peer later changes or its source becomes stale or revoked. Public conflict listings still conceal unavailable peer evidence; an empty listing does not clear the approval gate. A new review must pin every resolution allowing its exact revision. A conflict added after an existing review permanently invalidates that review; recovery requires a fresh target revision and fresh review. Historical conflict checks expose no peer IDs or counts.

`POST /v1/sites/{site}/claims/{claim}/review-revocations` accepts `{"claim_revision":1,"review_id":"<exact approval UUID>","reason":"Approval withdrawn after supervisor review"}` with the same CSRF, Idempotency-Key and quoted claim-revision If-Match. Current owner/supervisor membership and recent identity proof are required, including replay. Withdrawal can target an accessible historical approval after claim, entity or source revisions advance; it does not require a currently designated source. Revoked provenance still returns404. Rejections cannot be withdrawn through this endpoint, and a mismatched review ID or a second new-key revocation returns409 REVIEW_CONFLICT.

The response preserves the original review and adds immutable `revocation` metadata. `approval_eligible` remains false on review reads, revocation replay and replay of the original approval request. Repeating the same revocation key/body returns the original revocation; changing the request under that key returns409 IDEMPOTENCY_CONFLICT. Revocation never edits a historical verdict or silently reapproves a fact. A corrected claim needs a new revision and its own review.

Migration010 adds the review ledger without rewriting candidate payloads; migration011 adds append-only revocations with exact scoped review references. Candidate endpoints continue to expose captured facts; consumers must use the review projection to assess present approval eligibility. Review reads serialize with site mutations to prevent conflict phantoms; this trades per-site read throughput for consistent authority until measured contention warrants a revisioned snapshot design. Approved process snapshots capture this projection using the same consistency lock. Review eligibility alone grants no machine or agent tool permission.

### Explicit conflict resolution

POST `/v1/sites/{site}/conflicts/{conflict}/resolutions` accepts `{"decision":"prefer_left","reason":"Verified the cited instruction with the supervisor"}`. Decisions are `prefer_left`, `prefer_right`, `neither` and `withdraw`. Use CSRF, Idempotency-Key and quoted resolution revision in If-Match (`"0"` initially). Current owner/supervisor membership and recent identity proof are required, including replay. GET on the same path selects the latest resolution, or `?revision=N` selects immutable history. All current site members may read; revoked evidence on either party returns404.

A preference or neither decision requires both exact claim revisions and their evidence/entity references to be current. A preferred winner additionally requires a directly cited, currently designated source; the decision pins its exact authority record. Any losing claim with an unrevoked approval must first have that approval explicitly withdrawn, even if it is already ineligible because of the conflict. Neither requires both approvals to be withdrawn and authorizes neither claim. Missing authority or an unrevoked losing approval returns409 RESOLUTION_BLOCKED. Resolution never approves or rejects a claim automatically.

Each change appends history and advances a compare-and-swap head. Withdrawal may target an accessible historical resolution after its dependencies become stale; initial withdrawal and repeated withdrawal are invalid. The response separates selected revision from current_revision and computes resolution_eligible. Replacing/withdrawing a resolution or changing its pinned source authority invalidates approvals relying on the old exact decision. A later re-resolution cannot revive them.

New claim approvals atomically capture `resolution_refs` for all historical conflict pairs involving their exact revision. Every reference must continue to identify the current eligible decision selecting that claim. Newly discovered, unpinned conflicts keep an older approval ineligible even when peer evidence later disappears. At most1000 historical dependencies are accepted; exceeding the bound fails closed. Resolutions cover the conflict's recorded overlap; losing any overlap blocks approval of the whole claim revision. Split/revise the claim and review it to approve a different interval. Migration012 stores immutable decisions and review references behind FORCE RLS. No claim, citation or review history is erased.

The API derives tenant/site from verified issuer+subject membership; bodies cannot assign their own tenant or role. Every protected operation rechecks current membership. Runtime DB roles are nonowner/non-BYPASSRLS; identity rows are global account/session data while tenant-owned tables FORCE RLS. Application administrators provision memberships out of band for this increment; user-facing enrollment is not implemented.

Code+audit+outbox commit atomically. Temporal receives only identifiers, with stable workflow IDs. Durable transient retries continue with capped backoff; invalid scope is nonretryable. Effects remain idempotent in SQL. A worker discovers active tenants each tick; an optional allowlist narrows relay scope. Tenant deactivation takes effect on the next protected API request. Role revocation before processing rejects the probe.

Sessions expire after30minutes idle/8hours absolute; no provider tokens are retained or refreshed. Only RS256 ID tokens are accepted. Privileged identity confirmation does not extend absolute session expiry. Production configuration requires HTTPS origins and Temporal TLS; TLS termination, ingress rate limits, external secret delivery, login flood protection, identity-provider global logout and restore/retention operations still require production qualification. This local deployment is not customer-ready.

Remaining WIS-290: guided customer setup and membership interfaces, service-principal lifecycle, guarded commissioning/promotion, source/object lifecycle, production provisioning, backup/restore/telemetry and full-product qualification. Subsequent feature tickets own knowledge ingestion, AI specialists, approvals and operational tasks; each future privileged operation must explicitly use the recent-proof boundary. The `supervisor-restore` and `supervisor-release-check` targets deliberately return BLOCKED; no fake passing implementations. `supervisor-eval` is the WIS-280 slice 1 harness described under Verification. It cannot qualify an actual run yet.

Rollback stops the new API/worker and preserves database/Temporal histories. There is no destructive downgrade. Migrations use a transaction/advisory lock and stored checksum; changed applied schema is rejected. Discard disposable test volumes only when you explicitly intend to erase their data. No production deployment is performed by this work.

`CASE=feedback` (WIS-703) uses the same labelled pilot fixture/chat settings, on a freshly seeded
isolated default demo site (`…0291`, relay tenant `…0290`). Run
`SUPERVISOR_E2E_AGENT=pilot make supervisor-e2e CASE=feedback`. It drives operator-1's correction,
shift-approver's review, foundation-reviewer's explicit snapshot publication, and corrected answers
for operator-1 and operator-2. It refuses an absent fixture setup instead of skipping. This gate
is browser/runtime evidence for the deterministic fixture, not actual-model qualification.
