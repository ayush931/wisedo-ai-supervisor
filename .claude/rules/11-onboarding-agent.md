# Rule 11: Onboarding agent and factory knowledge

Source: WIS-610 and its merged child issues (WIS-611 to WIS-617) and `docs/onboarding-agent/README.md`
§4 and §7.2. WIS-618, WIS-620, WIS-621 and WIS-628 are in review (PRs #219 to #224); lines marked
"after WIS-620" bind only once that PR merges. This rule collects requirements those issues already
state. It adds no new policy, and rules 03, 04, 05, 06 and 08 still apply in full.

Onboarding turns what a factory says and uploads into knowledge. Model output becomes a fact only
through human review.

## Required

- Everything a model proposes during onboarding is a **candidate**: entities, claims, property
  values and checklist bindings. Only a human reviewer makes one approved, through the WIS-270
  review path.
- Every proposed entity name and every number must appear in a span the output cites, and the span
  must come from a source captured for that run. If one does not, the whole turn is refused with a
  stable code (`ENTITY_NOT_IN_SOURCE`, `CITATION_NOT_IN_SCOPE`, `CLAIM_INVALID`). It is never
  repaired.
- Numeric property values are canonical decimal strings with a unit (rule 04); text properties such
  as `inspection_method` carry no unit. The model never computes a number; Go does.
- Sites, areas, assets, processes and orders come from commissioning and master data, never from a
  model. The specialist may propose only the entity types listed in `internal/agent/interview.go`.
- The onboarding specialist reads raw, unreviewed onboarding evidence: the session's answers, the
  sources cited by the interviewed process's current entity and claim revisions (reviewed or not),
  and the sources of any passages the turn selects, such as an uploaded document. It is captured when the turn is created, under the participant's own
  evidence authority, at most the 50 most recent sources plus any the turn selects
  (`captureInterviewRefs` in `internal/onboarding/turns.go`). Reading unreviewed content is the
  purpose of onboarding; what it produces stays a candidate.
- A turn may name `source_span_ids`. The server checks each one (readable, current revision, at
  most `agent.MaxRunPassages` and `MaxTurnPassageBytes`; `selectPassages`, `agent.AddPassagesTx`)
  and records them on the run (WIS-614). For such a source, `evidence.get` returns only those
  passages. A captured source with no selected passages is still returned whole, with all its
  spans (`internal/agent/tools.go`, `evidence.get`); this is the compatibility path #214 kept for
  turns without passages, bounded by the run's model budget (`BUDGET_EXHAUSTED`).
- After WIS-620, chat ranks and selects passages on the server under the asker's own evidence
  authority, taking whole claims so that every span an approved answer cites is readable. Until
  then, chat on main reads every source cited by the snapshot, up to 70.
- Revoked and cross-tenant content is never selected, read or returned (rules 03, 06). Revocation
  is checked on every read (`readSource` in `internal/evidence/evidence.go`), and every query is
  scoped to the tenant and site.
- Uploaded files are classified from their bytes, extracted without network or tools within fixed
  limits, and stored as `document` sources with server-generated spans. A PDF is parsed only in the
  short-lived, resource-capped child process (`internal/onboarding/pdf.go`): cleared environment,
  CPU, memory and wall-time limits, at most two at once. A file that cannot pass is refused with its
  own 4xx code (encrypted, active content, external reference, no text layer, over a limit); only a
  busy or missing parser is a 503. No text is ever substituted. Text from model OCR is labelled
  model-extracted and has lower authority (WIS-625):
  - it never counts as an independent corroborating group;
  - the owner can mark it authoritative only with `confirm_model_extracted`, stating they checked it
    against the original page. Without it the request is refused with `409 MODEL_EXTRACTED_UNCONFIRMED`.
- Downstream consumers read only approved snapshot content, never candidates, and cite claim and
  span IDs. What they produce is advice and changes no record. A snapshot is stale when the
  knowledge inventory has moved past it (`Snapshot.Stale`, `internal/knowledge/snapshots.go`). Each
  consumer handles that in its own way, and none substitutes newer unapproved knowledge:
  - chat is available to active owners, supervisors and operators (product-owner decision,
    WIS-703, 2026-11-11). Conversations remain author-only. Every cited span is reauthorized
    under the reader's own evidence authority; operators may correct their own answer but
    cannot review or approve corrections. Chat uses the latest snapshot and labels a stale one in the answer ("newer knowledge edits are
    not yet approved"; `internal/chat/answer.go`, `internal/agent/chat.go`);
  - the graph read refuses a stale snapshot with `409 SNAPSHOT_STALE`
    (`internal/knowledge/graph_store.go`), and a checklist binding refuses one
    (`internal/knowledge/requirement_decisions.go`);
  - shift suggestions ignore a stale snapshot and proceed as if there were none
    (`internal/workstation/suggestion_routing.go`);
  - after WIS-629 (#228), the advisor uses the snapshot the case context pins, not the latest one,
    and labels it `SNAPSHOT_STALE` when it is stale.
- Every onboarding change records its gates and at least one negative control per boundary on the
  Linear issue (rules 09 and 10).

## Forbidden

- A route, activity or job that approves a candidate, binds a checklist cell or creates a snapshot
  without a human reviewer. A model or service identity never does any of these.
- Widening what a run reads after it is captured: a source the turn did not capture, a passage
  outside the turn's selection for a source that has one, or any unbounded search tool for any
  model. (Whole-source reads by the onboarding specialist without selected passages, and chat's
  whole-source capture on main, are the existing behaviour described above; WIS-620 replaces the
  chat one.) A new model tool (for example `retrieval.search`) needs a change to rule 08 and a
  review first.
- Calling a hosted model endpoint outside rule 08's "Model endpoints" section (WIS-622): for a
  site whose switch is off, without a recorded DPA, or without pseudonymising people's names and
  identifiers first.
- A separate vector or graph database as a source of truth, or a parser or indexing component
  that does not meet rule 01's requirements for non-Go components. Python parsers, layout and
  indexing methods are allowed under rule 01; what they produce is data that Go validates and
  scopes, and the read limits above still apply to whatever a model is shown. OCR text from any
  component, Go or Python, stays machine-extracted with the lower authority above. pgvector is a
  PostgreSQL extension (WIS-627, migration 075), created by the superuser at bootstrap; embeddings
  never authorize a passage, capture re-reads the source.
- Writing to job orders, shift plans or master data from onboarding advice. A difference between an
  approved value and a typed value is reported, never silently applied.
