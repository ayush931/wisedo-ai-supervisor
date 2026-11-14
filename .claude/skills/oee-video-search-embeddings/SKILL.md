---
name: oee-video-search-embeddings
description: ADR 0006 - the historical video search proposal - real-time CLIP embeddings from plant cameras, adaptive state-driven frame sampling, pgvector HNSW search, evidence correlation and a semantic search API. Use this skill for any work on video indexing, frame embeddings, camera registration, vector similarity search, or correlating footage with downtime events - and to check what its current status is before building on it.
---

# Video search and real-time embeddings (historical proposal)

Source: `docs/adr/video-embedding-docs/0006-video-search-architecture.md`.
**Status: Historical Proposal.** References ADR 0003 (agent harness) and ADR 0004 (training).

> Read this before using it. The independent AI Supervisor product is governed by
> `docs/independent-supervisor.md`. Edge visual inspection is governed by Future Vision
> (ADR 0007, `future-vision-cpp` and `future-vision-roadmap` skills) — not by this document. Nothing
> here is a committed Supervisor capability; treat it as the reference design if semantic video
> search is revived.

## Purpose

Make plant-floor footage searchable. As IP cameras, machine-vision systems and operator tablets
stream, the pipeline extracts key frames and encodes them into dense semantic vectors, so operators,
quality engineers and agents can query hours of footage in natural language:

- "Show all conveyor belt jams on Packaging Line 3 during shift 2"
- "Find occurrences where safety guards were opened on CNC Mill 4"
- "Show machine display warning ERR-304 on Line 1"

It is an independent bounded context (`video_search.*`) that **consumes** OEE evidence and downtime
markers for correlation and never modifies core state.

## Pipeline

RTSP/WebRTC receiver → adaptive frame sampler → optional HMI/display OCR → self-hosted CLIP vision
encoder → metadata enricher → store.

Per frame: decode, sample, resize to 224×224, normalize, batch up to 32, forward pass, L2-normalize
the output vector, attach timestamp, camera, equipment, site, line, shift, state code and sampling
mode, then insert the embedding into PostgreSQL, put the frame or thumbnail into SSE-KMS S3 and cache
the recent frame in Redis with a one-hour TTL.

### Adaptive state-driven sampling

Embedding full 30 FPS would make 108,000 vectors per camera per hour, so sampling follows equipment
state:

| Mode | Rate | Trigger | Stored |
| --- | --- | --- | --- |
| Idle monitoring | 0.2 FPS | `STANDBY`, `UNSCHEDULED`, `OFF` | embedding only |
| Active production | 1.0 FPS | `PRODUCING`, `RUNNING` | embedding + JPEG thumbnail |
| Event-triggered | 5.0 FPS | downtime trigger, E-stop, interlock trip, scrap burst | high-res image + vector + 10 s clip |
| On-demand | native rate | operator recording or audit | full segment + dense embeddings |

### Model choice

**CLIP ViT-L/14 (768-dim)** — one model encodes both images and text, enabling cross-modal search
with no separate text model; 768 dims balance quality against 3 KB per embedding; ~120 img/s on an
A10G covers the burst mode with headroom; MIT licence; mature ONNX/TensorRT ecosystem. Alternatives
considered: CLIP ViT-B/32 (512), SigLIP ViT-SO400M (1152), EVA-CLIP ViT-G (1024). In line with
ADR 0004 data sovereignty, the encoder is self-hosted — footage never leaves the customer VPC.

## Data model

Schema `video_search` with `vs_camera` (registration; `stream_url_ref` is a secret-manager pointer,
never plaintext) and the frame embedding table carrying `tenant_id`, `camera_id`,
`captured_at_utc`, `equipment_id`, `line_id`, `site_id`, `state_code`, `shift_code`,
`sampling_mode`, `vector(768)`, frame/thumbnail/clip URL references, optional `ocr_text`.

Indexes: HNSW on the vector with `vector_cosine_ops` (m=16, ef_construction=200), and a B-tree on
`(tenant_id, equipment_id, captured_at_utc DESC)` for temporal tenant-scoped queries.

`vs_evidence_correlation` links an `evidence_id` to the `embedding_ids` in its window plus
`downtime_event_ids`, with `correlation_type` of `DOWNTIME`, `QUALITY`, `CHANGEOVER` or `ANOMALY`.

## API

Under `/v1/oee/video/`, same OIDC auth and tenant isolation as the core: `POST /search`
(text/image hybrid with scope, window, `max_results`, `min_similarity`), `GET /frames/{frameId}`,
`GET /evidence/{evidenceId}/frames`, `GET /cameras`, `POST /cameras`, `GET /timeline`.

A result carries the frame ID, capture time, camera name, equipment, state code, sampling mode,
similarity score, thumbnail and clip URLs, OCR text, and the correlated evidence ID and downtime
event.

## Security

Every table has `tenant_id` and forced RLS, with tenant context set transaction-local and never from
a request body. Presigned S3 URLs are scoped to the tenant's frame prefix and expire. A search audit
log records who searched what. Plant footage may capture worker faces and proprietary processes, so
frames are encrypted at rest and the edge processor runs inside the plant network perimeter. CLIP
embeddings are not reversible to images, **but they carry semantic information and are treated as
sensitive data** — tenant-isolated, encrypted at rest, and deleted when their retention window
expires.

## Implementation order

1. Database schema, models and RLS policies. 2. CLIP embedding module (vision encoder, text encoder,
batching). 3. Frame sampling pipeline (RTSP decode, adaptive sampler, S3 upload). 4. Search API
(hybrid query, presigned URLs). 5. Evidence correlation (automatic downtime ↔ frame linking).
6. Agent integration (`search_video`, `query_video_evidence` tools). 7. Tests — unit, integration
against pgvector, and RLS security.
