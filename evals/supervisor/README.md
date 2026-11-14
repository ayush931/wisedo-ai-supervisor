
### Site correction regressions (WIS-703)

Run `supervisor-eval -mode=deterministic|actual -case=corrections
-correction-set=/private/selection.json -out=/private/reports` against an isolated
local evaluation database with no competing chat worker. Set
`SUPERVISOR_TEST_DATABASE_URL` to its non-owner runtime role and
`SUPERVISOR_EVAL_CORRECTION_SESSION_FILE` to a mode-0600 file containing an active
owner/supervisor browser session cookie. The same session and membership checks as
the product apply. Never put the cookie on the command line or in a report.

The run writes to the site it evaluates: for each case, a "Correction regression" chat
conversation, its messages, agent runs and model calls, as the session's member. It makes no
review or publication writes. Run it against an isolated evaluation copy, never a customer's live
site. The report's `manifest_path`/`manifest_sha256` name the correction set, not the frozen
qualification manifest. In deterministic mode the scripted reply is the expected answer, so it
proves only the plumbing and snapshot pins; the false-answer controls exercise the judging.

The explicit selection is:

```json
{"schema_version":1,"tenant_id":"<uuid>","site_id":"<uuid>","snapshot_id":"<published uuid>","case_ids":["<approved correction case uuid>"]}
```

The scoped `/correction-evaluation-cases` export supplies case IDs. A case is
immutable and created only by the human approval transaction. Select a published
snapshot containing its exact resulting claim revision and canonical review. A
case absent from that snapshot is reported as awaiting snapshot/unavailable
approval, never passed. Historical selections stay pinned even after a newer
snapshot is published; current source and approval revocation checks still apply.

The runner creates private chat turns and normal runtime audit records. It never
approves a claim, publishes a snapshot, or changes the default corpus. Its report
freezes case, selection and snapshot digests and records code/model/prompt/tool
pins, runs, artifacts, source references, usage and assertions. Actual mode uses
only the pinned local model and runs each selected case three times; deterministic
mode scripts provider responses through the same runtime. Neither mode constitutes
full model qualification (`qualified=false`). Reports contain site evidence and
are written with mode 0600.

Assertions require one complete reviewed statement (at most 1000 characters),
whitespace-normalized equality, exact claim/revision and source-span references,
source digest/revision/access epochs, and exact decimal quantities/units. They
conservatively reject paraphrases, extra statements, negation, contradictory
answers and abstention. Five negative judge controls run in both modes; an
incorrectly passing control fails the report. Longer expected answers can be
saved as pending corrections but cannot be approved until expressed within the
supported bound.

The real `make supervisor-e2e CASE=feedback` gate can export its selection and
owner session to private paths supplied in `SUPERVISOR_EVAL_CORRECTION_SET_FILE`
and `SUPERVISOR_EVAL_CORRECTION_SESSION_FILE`. These are explicit local test
outputs, never browser attachments containing a session. Stop the isolated worker
before evaluating that database, and delete the private session file afterwards.
