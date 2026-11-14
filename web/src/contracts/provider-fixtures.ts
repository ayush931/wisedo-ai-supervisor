// Recorded provider responses from TestOEEServiceHTTPPostgres; each was validated against its
// published JSON Schema first. Regenerate with SUPERVISOR_PROVIDER_FIXTURE_OUT set. Do not edit.
import type { OeeCalculationEvidenceV1, OeeCalculationRunV1, OeePolicyV1, ProblemDetailsV1 } from "../../../contracts/typescript/oee";

export const providerOeeCalculationEvidence = {
  "asset_id": "22222222-2222-4222-8222-222222222222",
  "dataset_id": "32323232-3232-4232-8232-323232323232",
  "digests": {
    "calculation": "75169efc94960a5b59e9c5f12ca8ad21fa4bcdb4acce8f4fd9bae20c72dd3570",
    "dataset": "30f5febb124de5bceb99f44114565cf5fc56e2870f67bd6b7cc57f57cfe4e9cd",
    "timeline": "6261ab7a3de74aaf9da7e9d259497ba15012527fb78a5db9e60afca87d5a1660"
  },
  "durations": {
    "calendar": "28800",
    "gap": "0",
    "microstop": "0",
    "planned_exclusion": "1800",
    "planned_loss": "",
    "planned_production": "27000",
    "run": "23400",
    "stop": "3600",
    "unclassified_loss": "",
    "unplanned_loss": ""
  },
  "evidence_id": "ecb2bb45-475d-40a5-827b-0715273a06f6",
  "formula_version": "2026.1-nakajima-iso22400",
  "kernel_version": "v1.0.0",
  "kpis": {
    "availability": {
      "denominator": "27000",
      "numerator": "23400",
      "value": "0.866667"
    },
    "loading": {
      "denominator": "28800",
      "numerator": "27000",
      "value": "0.937500"
    },
    "oee": {
      "denominator": "12320100000000",
      "numerator": "8760960000000",
      "value": "0.711111"
    },
    "performance": {
      "denominator": "23400",
      "numerator": "19500",
      "value": "0.833333"
    },
    "quality": {
      "denominator": "19500",
      "numerator": "19200",
      "value": "0.984615"
    },
    "teep": {
      "denominator": "354818880000000000",
      "numerator": "236545920000000000",
      "value": "0.666667"
    }
  },
  "policy": {
    "digest": "f2b675b283cb50c1a024f0879630c605f47e4f874592089b29320ccd4394f1c9",
    "policy_id": "05733d5a-1e9b-4a48-ad57-7e185eed802f",
    "policy_revision_id": "11103756-0fb3-4e04-985c-8e0205bedafb",
    "revision": 1
  },
  "reason_codes": [],
  "run_id": "90c409e0-772d-4517-96ac-fd63e00bf19a",
  "schema_version": 1,
  "sealed_at": "2026-10-17T08:13:42.520655Z",
  "sources": [
    {
      "consistency_mode": "SNAPSHOT",
      "contribution_sha256": "0bf1bd708ee6459b26e46eda061d75b5ad676660969903fb1f5ee0c4060bcacf",
      "fact_count": 6,
      "owner_service": "mes",
      "snapshot_ref": "snapshot-1",
      "source_key": "plc",
      "source_revision": "r1"
    }
  ],
  "status": "READY",
  "timeline": {
    "coverage_ratio": "1",
    "gap_seconds": "0",
    "reason_codes": [],
    "status": "READY"
  },
  "truth_state": "CANDIDATE",
  "window_end": "2026-10-01T16:00:00Z",
  "window_start": "2026-10-01T08:00:00Z"
} as const satisfies OeeCalculationEvidenceV1;

export const providerOeeCalculationRun = {
  "accepted_at": "2026-10-17T08:13:42.513688Z",
  "asset_id": "22222222-2222-4222-8222-222222222222",
  "cancel_generation": 0,
  "dataset_id": "32323232-3232-4232-8232-323232323232",
  "deadline_at": "2026-10-17T08:14:42.513688Z",
  "evidence_id": "ecb2bb45-475d-40a5-827b-0715273a06f6",
  "finished_at": "2026-10-17T08:13:42.521743Z",
  "policy": {
    "digest": "f2b675b283cb50c1a024f0879630c605f47e4f874592089b29320ccd4394f1c9",
    "policy_id": "05733d5a-1e9b-4a48-ad57-7e185eed802f",
    "policy_revision_id": "11103756-0fb3-4e04-985c-8e0205bedafb",
    "revision": 1
  },
  "replayed": false,
  "result_status": "READY",
  "row_version": 3,
  "run_id": "90c409e0-772d-4517-96ac-fd63e00bf19a",
  "started_at": "2026-10-17T08:13:42.518764Z",
  "status": "SUCCEEDED",
  "truth_state": "CANDIDATE",
  "window_end": "2026-10-01T16:00:00Z",
  "window_start": "2026-10-01T08:00:00Z"
} as const satisfies OeeCalculationRunV1;

export const providerOeePolicy = {
  "current_publication": {
    "action": "PUBLISH",
    "policy_revision_id": "11103756-0fb3-4e04-985c-8e0205bedafb",
    "publication_id": "a8471f88-7b98-4313-a3bf-5468d8959bea",
    "publication_no": 1,
    "published_at": "2026-10-17T08:13:42.504694Z"
  },
  "policy_code": "line-oee",
  "policy_id": "05733d5a-1e9b-4a48-ad57-7e185eed802f",
  "replayed": false,
  "revisions": [
    {
      "created_at": "2026-10-17T08:13:42.491544Z",
      "digest": "f2b675b283cb50c1a024f0879630c605f47e4f874592089b29320ccd4394f1c9",
      "effective_start": "2026-01-01T00:00:00Z",
      "policy": {
        "max_source_silence_seconds": 0,
        "microstop_threshold_seconds": "300",
        "min_coverage": "1",
        "performance_cap_rule": "ALLOW_AND_RETAIN",
        "policy_version": "1.0.0",
        "precedence_order": [
          "SHUTDOWN_EXCLUSION",
          "PLANNED_BREAK",
          "CHANGEOVER",
          "UNPLANNED_DOWNTIME",
          "STANDBY",
          "RUNNING"
        ],
        "rework_counts_as_good": false,
        "rounding_mode": "HALF_EVEN",
        "rounding_scale": 6,
        "state_buckets": [
          {
            "bucket": "STOP",
            "state": "CHANGEOVER"
          },
          {
            "bucket": "EXCLUDED",
            "state": "PLANNED_BREAK"
          },
          {
            "bucket": "RUN",
            "state": "RUNNING"
          },
          {
            "bucket": "EXCLUDED",
            "state": "SHUTDOWN_EXCLUSION"
          },
          {
            "bucket": "STOP",
            "state": "STANDBY"
          },
          {
            "bucket": "STOP",
            "state": "UNPLANNED_DOWNTIME"
          }
        ],
        "zero_denominator_rule": "NOT_READY"
      },
      "policy_revision_id": "11103756-0fb3-4e04-985c-8e0205bedafb",
      "revision_no": 1
    }
  ],
  "row_version": 2
} as const satisfies OeePolicyV1;

export const providerProblemDetails = {
  "code": "NOT_FOUND",
  "correlation_id": "071575e9-347e-467c-afd0-cf19a4a8ad4e",
  "status": 404,
  "title": "NOT_FOUND",
  "type": "about:blank"
} as const satisfies ProblemDetailsV1;
