# Synthetic factory and live shop-floor facility (WIS-278, WIS-709)

**Fictional data only.** Sahyadri Precision Components is a synthetic Chakan/Pune plant:
48 stations, six lines, ten SKUs, three invented customers, INR, Asia/Kolkata. Prices and
machine parameters are modeling assumptions, not customer or market evidence. Planned annual
sales are ₹100.188 crore; actual generated counts include losses and corrections.

From the repository root, with Go 1.27 available:

```sh
go run ./cmd/supervisor-factory-sim generate \
  --out /tmp/sahyadri-seed278-14d --seed 278 --days 14 --start 2026-09-01
```

The output directory must not exist. Default date and seed are fixed; the same seed, date,
days and tool version produce byte-identical files. Days are bounded to 1–31. A built binary
runs from any working directory without a contracts directory. Generation runs the existing
runtime semantic validators, checks CSV/JSON projections and paired Markdown/source text,
and verifies OEE provenance. Loading regenerates the seed and compares every file byte-for-byte
before sending it to the public APIs, which validate each request.

Use a fresh local fixture provisioned through the existing [demo seed](../../deploy/supervisor/README.md)
and local runtime. The operational ingestion worker must run. Authenticate these four distinct
fixture users normally, then provide their `supervisor` cookie values in environment variables:

- `SUPERVISOR_SIM_OWNER_COOKIE`: foundation-reviewer (owner).
- `SUPERVISOR_SIM_PLANNER_COOKIE`: shift-lead (supervisor).
- `SUPERVISOR_SIM_APPROVER_COOKIE`: shift-approver (different supervisor).
- `SUPERVISOR_SIM_OPERATOR_COOKIE`: operator-1 (operator).

Use recent logins; do not save cookies in dataset files or commit them. The loader gets CSRF tokens
from `/v1/me`. Every participant must belong only to the standard synthetic tenant/site ending
`290`/`291`; real provisioning never uses those IDs. Visible names are realistic by product decision (2026-10-30); the data stays flagged `synthetic` in the manifest, observations and answer key.

```sh
export SUPERVISOR_ENVIRONMENT=local
# Set the four cookie variables above from your local fixture logins.
# Replace this example with your fixture's free loopback port. Never use CI port 48176.
go run ./cmd/supervisor-factory-sim load \
  --out /tmp/sahyadri-seed278-14d --url http://127.0.0.1:58180
```

Only literal loopback URLs are allowed; redirects and HTTP proxies are disabled. No database
credentials are accepted. All business writes go through the public APIs: dry-run and apply
configuration imports, sources/authority/currentness/spans, entities and reviewed machine-to-line
claims, roster, profiles, routing, orders, approved shifts/count plans, discovery
inventories, role interviews, operational batches, process snapshots, governed OEE calculations
and production contexts. Public responses bind server-owned IDs in the generated templates.
Approvals, claim reviews and source reviews are performed by **scripted synthetic fixture
identities**, not independent human reviewers. Distinct fixture subjects exercise the API's role
and self-review rules; they do not demonstrate independent human oversight.
A reload of the same dataset is a business no-op (audit/session touches remain normal).
A different seed/date/day count or generator version needs a fresh fixture; the loader is not
an update/import editor. Regenerate datasets after upgrading the generator.

The manifest lists hashes, economics, station parameters and all files. `answer-key.json` identifies
six first-day incidents: running throughput constraint, tool-wear microstops, late castings, a late
count correction, changeover quality loss and recovery following a synthetic maintenance observation.
The loader fails on rejected configuration rows, quarantined events, incomplete folding, non-READY
contexts or non-READY OEE calculations. OEE results remain CANDIDATE evidence.

Historical-mode boundaries:

- Recurring calendar API cannot represent overnight shifts. Its import is labeled daytime-only;
  all three full shifts, including 22:00–06:00, are loaded as dated shift plans.
- Historical workstation records cannot pass live shift/clock guards. Their observations are
  operational events; no approval is backdated and no record is inserted directly.
- Tool-wear codes are retained. Main currently treats stops with a stated reason as stoppage
  evidence, not sufficient maintenance authority. The answer key does not override that policy.
- Case workers/LLM investigation, proposal approval, execution and outcome observation are not
  performed by this loader. The maintenance observation is synthetic, not a committed task.
  Recovery means threshold recovery; it is never evidence of causation.
- Historical loading does not upload PDFs; the facility `manuals` command below does. No customer integration, real machine signals, deployment or actual-provider qualification is implied.

Unit tests use two days and pin a small manifest golden hash. The database integration is
opt-in through `CASE=factory-sim`; it is not part of `CASE=all`. A missing database is a failure:

```sh
go test ./cmd/supervisor-factory-sim ./internal/factorysim
# Supply URLs for a disposable PostgreSQL with supervisor_runtime and supervisor_migrator roles.
make supervisor-integration CASE=factory-sim
# Extended rehearsal: 14-day generation/load, replay checks and the actual CLI load.
SUPERVISOR_FACTORY_SIM_14_DAYS=1 make supervisor-integration CASE=factory-sim
```

The integration test creates/drops its own database, uses a verified restricted runtime role,
starts the real HTTP handler on an ephemeral loopback port, and runs the ingestion fold worker.
Only tenant/membership/session fixture setup uses SQL. It does not claim a real Keycloak or Temporal
rehearsal. Do not run `make supervisor-check` in a shared checkout where `npm ci` is prohibited;
run that gate in a disposable source snapshot.

## Live facility (WIS-709)

`run` bootstraps the same plant on a **fresh** dedicated fixture, then delivers 96 quarter-hour
model intervals and operates all 48 stations through the real shift and station APIs. Use only
the facility's 62xxx loopback port. The historical `generate`/`load` golden is unchanged.

```sh
GOCACHE=/var/tmp/gocache-factory GOMODCACHE=/var/tmp/gomod-factory \
SUPERVISOR_ENVIRONMENT=local go run ./cmd/supervisor-factory-sim run \
  --url http://127.0.0.1:62376 --out /var/tmp/facility-day \
  --seed 278 --start 2026-09-01 --speed 60 \
  --wall-start 2026-11-12T10:00:00Z --sessions /var/tmp/facility-sessions.json
```

Choose a future wall start with time for setup (the default is now plus two minutes).
The session JSON maps `owner`, `planner`, `approver`, `operator` to their browser cookie values;
keep it mode 0600 outside the dataset. Renew it atomically through normal browser logins before
recent authentication expires. The runner checks renewed identities and never extends session
policy. The existing cookie environment variables also work for short runs.

The frozen schedule maps a simulated 24 hours to 24 wall minutes at speed 60 (1..300 allowed).
Real database time, approvals, session checks and shift guards remain enabled. Planning rates
and target cycle times use projected wall units; machine observation durations and nameplate
ratings retain model-time units. An accelerated run is synthetic test evidence, not elapsed
production time. `live-schedule.json` pins both axes; batches depend on the seed and schedule,
not loop wake-up jitter. Server receipts and generated identifiers are recorded separately.

`run-report.json` is updated during execution and on failure. A pass requires 96 newly accepted
batches, no quarantined events, a completed worker fold, and summaries/handovers for all 18
line-shift runs. Scripted station records carry the real fixture actor and are synthetic manual
entries, never hardware readings. A missed whole interval fails instead of dropping samples.
The historical late correction is reserved for its edge switch, separate from the normal day.

Live output also contains `machine-sheets.json` for all 19 classes and 48 station variants,
including each SKU's cycle time, changeover, service, MTBF, repair and alarm parameters.
`tool-register.json` contains 80 station/SKU tools, dies and fixtures, six calibrated gauges,
opening and closing counters, and append-only life/replacement events in model time.
The fictional CNC's opening tool age is calibrated from the first two hours' processed parts
so its next part reaches tool life at the existing incident. Counter state drives its wear
alarm and 720 seconds of microstops; the scheduled inspection/replacement resets the counter
and retains its history. Calibration is usable strictly before its due instant.

After the live plant is loaded, generate and upload the manuals:

```sh
SUPERVISOR_ENVIRONMENT=local go run ./cmd/supervisor-factory-sim manuals \
  --out /var/tmp/facility-manuals --url http://127.0.0.1:62376 \
  --sessions /var/tmp/facility-sessions.json --seed 278 --start 2026-09-01
```

This generates 19 class manuals, six line SOPs and two registers as typed PDF and Markdown,
with `manual-answer-key.json` containing the parameter values and expected text. PDF metadata
is normalized without moving xref offsets; numeric text is checked through the product parser.
Scanned/mixed editions are deferred to WIS-708's synthetic-scan generator.

The command uploads every PDF through Documents, reads back the stored text, designates and
confirms each source, submits 48 cycle-time claims, has a distinct fixture identity review them,
and publishes a snapshot. The product stores extracted document text and spans, not original
PDF bytes; the originals remain in the artifact directory. `manual-upload-report.json` records
the sources, exact station spans and the answer check. AC3 gates on what the server resolved:
the answer is `answered`, carries exactly one `cycle_time_seconds` quantity for the expected
claim with value 45, unit `s` and the expected span, and no other cycle-time value; the body
states `45 second(s)`; the expected span is cited; and the stored span text contains
`Rated cycle time: 45 seconds.`. It does not match the model's wording. AC3 gates on the pinned
local model (`facility.sh local-model`). It passes only when the model reads the evidence: the brief gives approved
facts without their station, so the model must call `evidence.get` (see the AC3 diagnosis in the
[facility design](../../docs/shopfloor-facility-design.md)). The pilot fixture provider reads the first captured
source, which is not the asked station among 48, so its answer fails AC3 and serves as the
negative control (`manual_upload_test.go`). These fixture approvals and answers are not human
review or actual-provider qualification.

### Maintained facility stack and acceptance

Run from any checkout of the repository, with Go 1.27 on `PATH`:

```sh
scripts/supervisor/facility.sh accept
```

The script checks the project, storage, the run-root ownership and fixed unused ports; uses only
`supervisor-facility` (PG 62477, Keycloak 62077, Temporal 62277, API 62376); builds its own
static binaries/assets (recording commit, dirty flag and binary digests in
`build-provenance.json`); migrates/seeds; and runs, in order:

1. the live day at speed 240, on the fixture provider;
2. `local-model`: recreates the API and worker on the pinned local model, checks the running
   containers' settings and the endpoint's reported model, and writes `local-model-provenance.json`;
3. `manuals` (AC3) and `scenarios` (19 of the 20 AC2 cases, `--exclude revoked-document`) on that
   model;
4. `audit`: reads the server's own run and call rows for that window into
   `local-model-audit.json` and fails unless every run and call is the pinned model, with at most
   one call in flight, and the AC3 answer has its own local run;
5. `fixture`: switches the API and worker back and checks the running containers;
   `scenarios-fixture` then runs `revoked-document` there (see below), into its own report;
6. scripted and exploratory personas, last, so the scripted owner's published claim cannot reach
   AC3; then the report.

Steps 2 to 4 hold `/var/tmp/supervisor-facility/model-request.lock` (non-blocking; a held lock is
BLOCKED). If the local model cannot be selected or verified, AC3 and the local AC2 cases are skipped and the run
fails; they never fall back to the fixture provider. Real fixture logins renew every five
minutes while the day runs. Every phase removes `artifacts/auth/sessions.json` when it exits,
passed or failed, and the next phase logs in again; the report fails if the file is left. Existing project containers must carry this script's run label;
it refuses other ownership. It never changes the shared model containers or another stack.

Go and npm caches and Playwright browsers default to `/var/tmp` (override with `GOCACHE`,
`GOMODCACHE`, `npm_config_cache` and `PLAYWRIGHT_BROWSERS_PATH`). Go is taken from `PATH`;
without it the script reports BLOCKED. The persona specs require `FACILITY_ARTIFACTS` (and the
exploratory spec `FACILITY_SIM_BINARY`) under `/var/tmp`; the script sets both. The existing
Playwright Chromium channel must be installed:

```sh
PLAYWRIGHT_BROWSERS_PATH=/var/tmp/playwright-factory web/node_modules/.bin/playwright install chromium --no-shell
```

No npm dependency is added. The two Compose files reuse the repository's pinned dependencies;
application containers mounting the clone use the current uid/gid. PostgreSQL data, logs,
binaries and artifacts stay under `/var/tmp/supervisor-facility`. The local passwords in the
script are public fictional fixture credentials. This is not a deployment profile.

The commands `up`, `seed`, `auth`, `run`, `local-model`, `manuals`, `scenarios`, `audit`,
`fixture`, `scenarios-fixture`, `personas`, `report`, `config` and `down` are individually available (`auth` alone
keeps its session file). `config` asserts the Compose scope without printing
credentials. `down` stops only this project and retains all data. A subsequent `accept` starts
fresh data in another timestamped run directory. It never deletes an old run to make a test pass.
A started run cannot be silently overwritten. Current run ID is in
`/var/tmp/supervisor-facility/current`; its artifacts are in `runs/<ID>/artifacts/`.

`acceptance-report.json` and `.md` summarize Acceptance 1–4, with AC2 as the 20 product edge
cases alone (the gauge self-check is its own line and still fails the run), the provider audit,
the session-file check and the product gaps. `facility-report.test.mjs` pins those checks. A failed
phase exits nonzero, but the remaining phases still run and report. Required repository checks are separate:

```sh
GOCACHE=/var/tmp/gocache-factory GOMODCACHE=/var/tmp/gomod-factory \
  npm_config_cache=/var/tmp/npm-factory make supervisor-check
GOCACHE=/var/tmp/gocache-factory GOMODCACHE=/var/tmp/gomod-factory \
  npm_config_cache=/var/tmp/npm-factory make supervisor-contracts
```

To run the local phase by hand, hold the lock across all four steps:

```sh
flock -n /var/tmp/supervisor-facility/model-request.lock sh -ec '
  scripts/supervisor/facility.sh local-model
  scripts/supervisor/facility.sh manuals
  scripts/supervisor/facility.sh scenarios
  scripts/supervisor/facility.sh audit'
scripts/supervisor/facility.sh fixture
```

The sequence stops at the first failure. `manuals` and `scenarios` also refuse to start unless the
running API and worker use the pinned local model, and `audit` checks that they ran on the same
containers `local-model` started.

`local-model` recreates only this project's API/worker with the fixed Qwen3.5-9B revision at
127.0.0.1:64601, one inference slot, and unchanged word retrieval. It never starts or modifies
model containers. No embeddings are needed for this comparison. In `accept`, `scenarios` still runs when
AC3 fails, so AC2 is reported; neither runs if local selection fails. Retain the original reports. `fixture` (or `seed`) selects the
fixture provider again; do not call either between measured phases. Read actual provider/model provenance from the
server's agent runs and calls; a client-side label alone is not proof. Results are measurements
on the synthetic plant, not qualification. The 2026-11-11 follow-up and exact wording limitation
are recorded in the [facility design](../../docs/shopfloor-facility-design.md).

### Edge switches and personas

After loading a normal day, run one isolated case or the whole catalogue:

```sh
SUPERVISOR_ENVIRONMENT=local go run ./cmd/supervisor-factory-sim scenarios \
  --scenario duplicate-batch --url http://127.0.0.1:62376 \
  --sessions /var/tmp/facility-sessions.json --out /var/tmp/facility-duplicate
```

`internal/factorysim/scenarios.go` is the answer key: 20 product edge cases (AC2) and, apart from
them, facility model self-checks (`overdue-gauge` tests the simulator's own gauge model; the
product has no gauge concept). Its exact JSON and digest are written before execution, and
`scenarios_test.go` pins that digest, so changing an expectation is a reviewed diff.
Every product case gates on an outcome the server decides:

- `contradictory-interview` gates on the open `value_conflict` between exactly the manual and
  interview claims, on the before snapshot holding the manual claim, and on the after-conflict
  snapshot holding neither. The chat reply is recorded as a labelled model observation and never
  gates. The issue's "contradiction becomes needs_input" has no server rule behind it; the
  scenario report lists it under `product_gaps`.
- `revoked-document` runs on the fixture provider. Its revocation checks need a first answer that
  cites the target span, and only the fixture provider gives one deterministically: the local
  model cited it in runs at 16:23Z and 17:49Z on 2026-11-11 and answered `needs_input` without a
  citation at 21:52Z. A citation the model may or may not make is not gated. The case revokes the
  target before checking that precondition, so a failed run leaves no first-sorting source behind.
  Since WIS-703 the pilot fixture also pins the first approved fact (by claim ID), so the target
  claim ID sorts first as well.
- `revoked-document` requires the first answer to cite the target span. The target source's ID
  sorts first, so the fixture provider (which reads the first captured source) cites it; a model
  must still choose to. After revocation, the earlier answer must be redacted and a new answer
  must be `unavailable` / `SNAPSHOT_EVIDENCE_UNAVAILABLE` with no citations.
- `expired-session` gives one subject an active membership and two sessions that differ only in
  expiry: the unexpired one must write (200), the expired one must get 401 and leave no source.
- `overnight-shift` checks plan creation only (the real create status, 22:00 to 06:00, eight
  hours). The plan starts tomorrow, so its run is not started. Reports show expected/observed status and code, named kernel/model
layer where applicable, evidence and pass/fail. Business writes use public routes. Only the
cross-tenant, expired-session and revoked-membership identity fixtures use scoped SQL, requiring
`SUPERVISOR_TEST_ADMIN_URL` at **127.0.0.1:62477/supervisor**. Their tokens never enter reports.
The late station correction names its predecessor and verifies the ended summary's new total.
The counter-reset switch checks normalization with and without reset evidence, then submits the
normalized three-part delta and requires HTTP 202, a completed fold and zero quarantine.
Historical `answer-key.json` retains its incident catalogue; the normal live manifest omits the
three corrected parts from revenue because that correction belongs to the edge run.

The separate `web/personas/playwright.config.ts` reuses real Keycloak login and existing UI
controls, with one context per responsibility. Maintenance and quality map to existing roles;
scripted approvals remain fixture activity, not independent human review. Display-name fallback
404s and absent document decisions are classified explicitly; other errors fail the scripted
gate, as do browser console errors other than Chromium's log of those same allowlisted 404s.
No route responses are stubbed. With WIS-703 (#299) on the base, a corrector (operator fixture)
asks on Today (after the owner publishes a reference fact whose source and claim IDs sort first,
so the pilot fixture answers it deterministically on a fresh facility), marks the answer wrong and proposes a sourced 50-second fact for a separate
entity; the approver (a different person) approves it, which only marks it "awaiting snapshot";
the owner's snapshot then publishes it, and the candidate must report that snapshot. The journey
does not re-ask for the corrected answer: the pilot fixture provider answers from the first
captured source, so that check belongs to `web/tests/feedback.spec.ts` on an isolated site.

Exploration is bounded to two personas, six steps each, six short memory entries and one pinned
local request at a time. The Go `persona-step` helper accepts a closed JSON input on stdin and
uses the existing address-checked local adapter at 127.0.0.1:64601. Model, GGUF revision, prompt,
request and binary hashes are reported. It offers no model tools. Browser actions select only
currently visible read controls; HTTP writes and off-origin traffic are independently refused.
It never offers approval, record, publish, chat submission, shell, SQL or arbitrary URLs. Invalid
model output becomes a recorded refusal with no action. A missing local inference dependency
fails visibly. Each step records what the endpoint reports (`/v1/models` ID, size, type and
parameter count, and the `/props` model path) and refuses a different model. The endpoint
reports no weight digest, so `model_revision` is the configured pin, labelled as such; the GGUF
was verified at commissioning and is not a remote attestation of server memory.

`findings.json`, `findings.md`, per-persona memory and screenshots are exploratory leads, never
product gate results. Screenshots are captured before the chosen action; model observations
may be wrong. Reproduction confidence stays unknown until independently checked. Scanned and
mixed manuals remain deferred to WIS-708's generator.
