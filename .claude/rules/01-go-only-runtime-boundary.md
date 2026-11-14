# Rule 01 — Runtime boundary: Go owns authority; Python is allowed for methods

Source: WIS-290 "Language decision / Go production-core boundary" (2026-09-27); relaxed on
2026-11-12 by the product owner to allow Python components, so the Supervisor can adopt
state-of-the-art retrieval, parsing and model methods that only exist in Python. The authority
boundary below is unchanged. The Linear record of the WIS-290 decision still needs updating.

Go owns the HTTP API, OIDC sessions, canonical persistence and authorization, source validation,
approvals, deterministic execution, idempotency and Temporal workflows/workers. That ownership
is about authority, not language preference, and it does not move.

## Allowed

- Python components for methods: retrieval and indexing, document parsing and layout, OCR,
  reranking, evaluation, model serving and training. Use an existing open-source library rather
  than re-implementing it in Go when it is the better method.
- A Python component runs as a separate process or service that Go calls: an HTTP service on a
  pinned local address, or a short-lived child process over stdin/stdout. Go starts or calls it,
  owns its timeout and treats it as optional unless the feature says otherwise.

## Required for every non-Go component (Python, C++ or other)

- A validated, versioned contract at the boundary. Go validates every request it sends and every
  response it receives; a malformed or unknown response is refused, never repaired.
- No authority: it never approves, reviews, dispatches, executes, promotes or writes canonical
  records. It gets no privileged database role and no credentials beyond what its own job needs;
  if it reads the database at all, it uses a read-only role under the same RLS (rule 03).
- Tenant and site scope come from Go per call. It holds no cross-tenant state, and anything it
  caches or indexes is keyed by tenant, site and source revision and honours revocation (rule 06).
- Its output is data, like model output (rule 08): citations it returns are checked by Go against
  the run's captured scope, and anything outside that scope rejects the whole output.
- Parsers keep the rule 11 limits: no network, bounded CPU, memory and wall time, explicit refusal
  codes, no substituted text.
- Network egress follows rule 08: no call to a host outside the compiled-in allowlist, and no
  hosted service receives customer data unless that site's switch allows it.
- Dependencies are pinned (lock file with hashes) and the image builds offline-reproducibly;
  customers run on-premises without internet access.
- A missing or failing component produces a stable reason code and a visible degraded state, never
  a silent fallback that pretends to be the real method (rule 09).

## Forbidden

- Moving approval, executor, idempotency, workflow or canonical-persistence authority out of Go.
- C++ in the core. C++ stays reserved for the edge/native requirement that
  `src/wisedo_fixture_vision_cpp/` covers, and for the recorded exception below (this relaxation
  covers Python only).
- A second durable-workflow engine. DBOS is a documented fallback only after a reproducible
  Temporal blocker is recorded and the architecture amended.
- Letting a non-Go agent loop choose its own tools, budgets or read scope. Agent orchestration,
  the tool allowlist and budgets stay in the Go runtime (`internal/agent`); a Python method may be
  called as a tool or step inside a run, within that run's captured scope.

## Recorded exception: WIS-520 CP-SAT shift planner

Approved by the product owner on 2026-10-20. `src/workstation_cpsat/` is a C++ executable built
against a SHA-pinned Google OR-Tools CP-SAT archive and packaged in the root image (amd64 only).
It already follows the requirements above and is bounded further:

- Advisory only. Go (`internal/workstation`) validates every request before invoking it and
  every response before returning it. Its output is a suggestion; it never approves, persists,
  dispatches or changes a shift plan. Plan approval stays human (rule 05).
- A short-lived child process: stdin/stdout only, a cleared environment (no credentials or
  connection strings), a 6-second timeout, and no network or database code.
- Optional. A missing or failing solver returns `ErrSolverUnavailable`; manual planning is
  unaffected.
- No other C++ enters the Supervisor core on the strength of this exception.
