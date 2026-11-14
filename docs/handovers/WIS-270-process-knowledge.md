# Handover: WIS-270 process knowledge (claims, reviews, snapshots, graph)

Checked: 2026-10-20

Repository: `Wisedo-Technology/wisedo-ai-supervisor`
Updated 2026-10-20 22:45 after the merge train: `main` now carries migrations through 041.
Linear: WIS-270, status In Progress. Merged increments: PRs #21–#37, #39, #73, #94, #103.
Increments #109, #111–#117 are merged (without an independent approving review, at the owner's direction; post-merge review requested). #110, #118 and #119 are open. Nothing is deployed.

**Owner and reviewer.** The Linear "Delivery boundary" and "Grooming baseline" blocks name
Ayush Kumar as owner and Anshul Rathore as independent reviewer. The 2026-09-27 checkpoint
blocks in the same description say Anshul owns the implementation ("Anshul remains
implementation owner", "WIS270 remains In Progress under Anshul") and hand each increment to
Ayush for integration and testing. The comments are one by Anshul (2026-09-28, PR #39) and
three by Ayush (2026-10-12 to 2026-10-17). Linear does not
settle who implements and who reviews. Rule 02 needs them to be different people, so confirm the
pairing for each open PR before review.

## Acceptance matrix

Status meanings: **main** means a test on `origin/main` covers the item. **#n** means it is
only on that open PR's branch. **partial** means part of the item is tested. **missing** means
there is no test. The gate command is `make supervisor-integration CASE=WIS-270`, which
`scripts/supervisor/test.sh` maps to `./internal/knowledge ./internal/httpapi ./internal/evidence`.

| Item | Status | Evidence (test) |
|---|---|---|
| Claim contracts (typed literals, relation domains, pins) | main | `TestClaimWireBoundaries`, `TestRelationDomainMatrix`, `TestTypedLiteralsAndCanonicalDecimals`, `TestClaimEntityRevisionPins` (unit) |
| Temporal conflicts | main | `TestPostgresCandidateConflicts`, `TestKnowledgeHTTPConflictDetectionAndVisibility` |
| Snapshot transactions | main | `TestPostgresApprovedSnapshots`, `TestKnowledgeHTTPSnapshots` |
| Scope and revocation | main | `TestPostgresApprovedGraphAccess`, `TestEvidenceHTTPProvenanceAuthorityAndRevocation` |
| Graph health (no dangling refs, missing provenance, orphan entities flagged) | partial | FKs and `TestApprovedGraph` validate the approved set. No orphan-entity flag or graph-health report exists |
| Numeric timestamp rejection | main | `TestClaimWireBoundaries` case "numeric time" (unit) |
| HTTP enum roundtrip | main | `TestKnowledgeHTTPConflictDetectionAndVisibility` (`contested` state over HTTP, from #103) |
| Repeated source replay (no inflated corroboration) | #113 | `TestPostgresWIS270Corroboration`, `TestPostgresWIS270SourceIndependence` |
| Correlated-source false corroboration | #113 | same tests (copies of one SOP count as one independence group) |
| Missing source refs | main | `TestClaimWireBoundaries` cases "zero spans", "duplicate spans", "too many spans" (unit) |
| Changed SOP/hash | main | `TestKnowledgeHTTPCandidatesProvenanceAndScope` (`SOURCE_CHANGED`) |
| Overlapping vs non-overlapping claims | main | `TestApprovedGraph` ("overlap", "disjoint"; unit), `TestPostgresCandidateConflicts` |
| Multivalue predicates | main | `TestPostgresCandidateConflicts` (`consumes`), `TestKnowledgeHTTPConflictDetectionAndVisibility` |
| Cycle detection | main | `TestApprovedGraph` (unit), `TestPostgresTemporalCandidateCycles` |
| Concurrent reviews | main | `TestPostgresClaimReviews` (parallel reviewers) |
| Source revocation during snapshot creation | #110 | new case in `TestPostgresApprovedSnapshots` (both lock orders) |
| Cross-site/tenant RLS via non-owner role | partial | Cross-tenant leak checks in `TestPostgresClaimReviews` and `TestPostgresApprovedSnapshots` run on `SUPERVISOR_TEST_DATABASE_URL`. Main has a cross-site check only for sources (`TestPostgresProvenanceAuthority`). Sibling-site seeding for knowledge reads comes with #116/#117. Tests check that the migration owner is not a superuser and lacks `BYPASSRLS`. No WIS-270 test checks those attributes on the runtime role |
| Migration fresh/populated | main | `TestPostgresPopulatedEntityRevisionUpgrade`, `TestPostgresPopulatedConflictUpgrade`, `TestMigrationReadbackAndRepeat` |
| Cache/projection restart | N/A | No cache or projection exists; reads come from PostgreSQL |
| Snapshot digest reproducibility | main | `TestPostgresApprovedSnapshots` (same inventory gives a different ID and the same SHA-256) |
| Audit row per retrieval/review, no raw text | #110 (partial on main) | `TestPostgresClaimReviews` subtest "WIS-270 one audit row per retrieval and review without raw text". Main asserts audit rows per service but not exactly one per action |
| Perf 10k entities/100k claims, 2-hop p95 ≤ 500 ms | #114, not qualified | `make supervisor-load` with `TestEvaluateThresholds`, `TestCommittedManifestFreezesSpecThresholds`. Measured on a 24-CPU host, not the 4 vCPU/8 GiB reference host |
| Zero unauthorized claims returned | #114 | admin-SQL oracle plus nine negative probes in `cmd/supervisor-load` |
| `GET /knowledge?entity_id&as_of&depth` | #117 | `TestKnowledgeHTTPReadAsOf` |
| `GET /coverage` (site level) | #116 | `TestKnowledgeHTTPSiteCoverage`. Main has only the per-inventory `.../discovery-inventories/{id}/coverage` |
| ExternalIdentity: exact approved mappings | #118 | `TestWIS270ExternalIdentityResolvePostgres` |
| ExternalIdentity: merge creates a redirect decision | #119 | `TestEntityRedirectsUpgradeAndRuntimeGuards`, `TestEntityRedirectTriggerSerializesConcurrentMerges` |
| ExternalIdentity: fuzzy candidates need review | missing | No fuzzy matcher. #118 skips it because every mapping is already reviewed |
| `knowledge_outbox` | partial | Snapshot creation writes one row in the same transaction (`TestPostgresApprovedSnapshots`). Nothing reads or delivers it |
| OpenAPI for knowledge/evidence routes | #111 | `TestPublishedKnowledgeRoutesMatchRegisteredRoutes`, `contracts/openapi.json` |
| Decision reason 1..2000 chars | #112 | `TestPostgresDecisionReasonUpgradeTo2000Characters` and HTTP cases. Main caps it at 1000 |
| Deletion tombstones (rule 06) | missing | Nothing in `internal/` |

## PR status

All merges were made at the owner's direction **without an independent approving review**
(CONTRIBUTING / rule 02). Each was gated locally after merging `main`: `make supervisor-check`,
`make supervisor-contracts` and every integration package except dispatch on a fresh database.
Ayush is requested as post-merge reviewer on each.

| PR | Covers | State | Migration |
|---|---|---|---|
| #115 | CI unblock: vite 8.3.4, runner ownership, contracts in both images | merged | — |
| #108 | WIS-520 workstation workflow | merged | 032–038 |
| #114 | Knowledge load gate (reference-host run still needed) | merged | — |
| #109 | Test hygiene and rule 10 | merged | — |
| #111 | OpenAPI for knowledge and evidence routes | merged | — |
| #112 | 2000-character review decision reasons | merged | 041 (renumbered) |
| #113 | Source independence groups and corroboration | merged | 039 |
| #116 | Site-level `GET /coverage` (published in OpenAPI on merge) | merged | — |
| #117 | `GET /knowledge` with `as_of` (published in OpenAPI on merge) | merged | — |
| #110 | Audit row per retrieval and review; revocation racing snapshots | open, gated locally, awaiting CI | — |
| #118 | ExternalIdentity slice A: exact approved resolve | open, gated locally, awaiting CI | — |
| #119 | ExternalIdentity slice B: entity merge redirects, stacked on #118 | open, gated locally | 040 |

## Product decisions needed (owner and reviewer)

- **Self-review of claims.** An owner can create and approve their own claim, and rule 05 needs a
  separate approver. Raised in Ayush's 2026-10-16 comment. It stays as is until Ayush decides.
- **Self-review of operational mappings and merges.** `operational.Service.Decide` lets a mapping's
  proposer approve it (#118), and any owner or supervisor can merge entities they created (#119).
- **Snapshot digest.** `internal/knowledge/snapshots.go` hashes `json.Marshal` output, not
  `internal/canonical` (rule 04). Raised 2026-10-16 and left as is.
- **Several inventories per site.** Migration 015 keys inventories by area, process and
  capability. #116 returns `NOT_READY/INVENTORY_AMBIGUOUS` instead of listing each inventory.
- **Past `as_of` reads go stale.** In #117, any older `as_of` reads `SNAPSHOT_STALE` once the
  inventory changes, so historical reads work only while the inventory is unchanged.
- **Entity absent from the snapshot.** #117 returns `NOT_READY/ENTITY_NOT_IN_SNAPSHOT`, not an
  empty `READY`, because "unknown" and "isolated" cannot be told apart (rule 06).
- **WIS-272 intake and redirects.** In #119, new operational events still record against the
  merged-away entity. Plant context would then have to follow redirects when it reads.
- **Text-review conflicts.** `actual_step` and `objective` are single-valued, so two
  differently worded real steps get a `text_review` conflict. The spec says "different wording
  alone is not a contradiction". Raised 2026-10-16.
- **Snapshot input.** The spec's `POST /process-snapshots {claim_revisions[], expected_inventory_revision}`
  differs from the code. `SnapshotInput` on main takes `snapshot_id` and `reason`, and the
  snapshot captures the whole eligible approved set. A client cannot choose a subset.
- **Freshness default.** The spec says documents default to 90 days. The 2026-09-27 grooming
  says "no silent 90-day TTL", and main uses an explicit owner-defined `max_age_seconds`
  policy with no default. Choose one and update the spec.

## Error-code alignment proposal (documentation only)

The spec lists six codes. Current codes come from `internal/httpapi/{knowledge,evidence,operational}.go`
on main and from the open branches.

| Spec | Current | Proposal |
|---|---|---|
| 409 `KNOWLEDGE_CONFLICT` | Specific 409s: `REVIEW_CONFLICT`, `REVIEW_BLOCKED`, `RESOLUTION_BLOCKED`, `SNAPSHOT_CONFLICT`, `DISCOVERY_CONFLICT`, `BINDING_BLOCKED`, `FRESHNESS_BLOCKED`, `SOURCE_CHANGED`, `KNOWLEDGE_CYCLE`, `SNAPSHOT_STALE` | Keep the specific codes. Record in the spec that they refine `KNOWLEDGE_CONFLICT`. Optionally add a `category` member to the problem body later, under a versioned contract |
| 409 `UNKNOWN_MAPPING` | Quarantine reason in `internal/ingestion` (main). 409 problem code in #118/#119 | Keep. It matches the spec |
| 422 `INVALID_CLAIM` | `INVALID_CLAIM` (claim body), `INVALID_REQUEST` (query/body shape), `INVALID_REVISION`, `INVALID_SOURCE`, `BINDING_UNSUPPORTED`. #117 uses `INVALID_CLAIM` for a bad `as_of` | Keep. Document `INVALID_REQUEST` as the shape-level code |
| 413 `LIMIT_EXCEEDED` | `LIMIT_EXCEEDED` (since #94) | Keep |
| 503 `KNOWLEDGE_UNAVAILABLE` | `STORAGE_UNAVAILABLE` (knowledge, operational), `EVIDENCE_UNAVAILABLE` (evidence) | Alias in docs now. If a rename is wanted, do it only with a contract version bump and a dual-publish period |
| `NOT_READY` (stale/inaccessible graph) | A 200 body `readiness: NOT_READY` with reason codes in #116 (`NO_INVENTORY`, `INVENTORY_AMBIGUOUS`, `INVENTORY_UNAVAILABLE`, `COVERAGE_STALE`) and #117 (`NO_SNAPSHOT`, `SNAPSHOT_STALE`, `SNAPSHOT_UNAVAILABLE`, `ENTITY_NOT_IN_SNAPSHOT`). The snapshot graph route returns 409 `SNAPSHOT_STALE` | Keep the body-level readiness. For knowledge, evidence and operational routes, document `NOT_READY` as a readiness value, not a problem code. The OEE route already emits a 503 `NOT_READY` problem (`internal/httpapi/oee.go`), so record that exception |

Recommendation: do not rename any wire code that is already published without a contract version
bump. Rule 07 requires codes in the published contract, and rule 02 forbids changing a contract
without an approved versioned migration. Record the spec-to-code mapping in #111's OpenAPI
descriptions.

## Migration numbering

Final order (nothing deployed, so renumbering was safe):

| Version | File | From |
|---|---|---|
| 032–038 | WIS-520 shift plans … shift handovers | #108 |
| 039 | `039_source_independence.sql` (`relaxedForRLS[39]`) | #113 |
| 040 | `040_entity_redirects.sql` (forced-RLS count 121) | #119 (open) |
| 041 | `041_knowledge_decision_reason_2000.sql` | #112 |

The decision-reason migration moved twice (032 → 039 → 040 → 041). Its upgrade test seeds a
populated database through the current evidence and knowledge services, so it must stay after
every migration those services depend on. Follow-up: seed that test with raw SQL (like
`legacyEvidence`) so later migrations stop forcing renumbering.

Migrations are checksummed by version (`supervisor_schema_versions`). Renumber only before any
deployment applies them; after a deployment, a renumber breaks `Apply` on that database. Any
local development database that applied the earlier numbering must be recreated.

## Remaining work

- **Outbox delivery.** `knowledge_outbox` rows are written with each snapshot, and nothing reads
  or delivers them.
- **Tombstones (rule 06).** Deletion must leave a non-sensitive tombstone, and citations must
  become explicitly unavailable. Neither is built.
- **Reference-host load run.** After #114 merges, run
  `docker update --cpus=4 --memory=8g <postgres container>` and then
  `REFERENCE=1 SUPERVISOR_LOAD_PG_LIMITS=cpus=4,memory=8g make supervisor-load`, and record the
  report on WIS-270. Make no performance claim before that. A run that meets every threshold on
  any other host ends `BLOCKED` (exit 2), not `PASS`.
- **Graph health.** Orphan-entity flagging and a consistency report are not implemented.
- **Fuzzy identity candidates and dependency manifests.** The 2026-10-16 grooming asks for
  manifests over chunks, embeddings, summaries and answers. Neither these nor fuzzy candidates
  are started.
- Excluded: the CP-SAT planner and rule 01 question belong to WIS-520 (#108), not WIS-270.
