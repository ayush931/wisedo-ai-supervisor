package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
)

func planCatalog() []named {
	return []named{{"PlanCaptureInputV1", reflect.TypeOf(plans.CaptureInput{})}}
}

func planPaths() map[string]any {
	return map[string]any{
		"/v1/sites/{site}/plan-revisions": map[string]any{
			"post": map[string]any{
				"operationId": "capturePlanRevision", "security": []any{map[string]any{"session": []any{}}},
				"parameters": []any{
					map[string]any{"name": "site", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}},
					map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}},
					map[string]any{"name": "X-CSRF-Token", "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1}},
					map[string]any{"name": "Origin", "in": "header", "required": true, "schema": map[string]any{"type": "string", "format": "uri"}},
				},
				"requestBody": jsonBody("PlanCaptureInputV1"),
				"responses": merge(map[string]any{"201": map[string]any{"description": "Captured plan revision, including replay. ETag is the quoted review revision."}}, problemSet(map[string][]string{
					"401": {"AUTHENTICATION_REQUIRED"}, "403": {"CSRF_REJECTED", "PERMISSION_DENIED", "SELF_REVIEW_FORBIDDEN"}, "404": {"NOT_FOUND"},
					"409": {"IDEMPOTENCY_CONFLICT", "PLAN_REVISION_CONFLICT", "PLAN_REVISION_GAP", "PLAN_SUPERSEDED", "SOURCE_SPAN_UNAVAILABLE"}, "412": {"REVISION_CONFLICT"}, "413": {"BODY_TOO_LARGE"}, "415": {"UNSUPPORTED_MEDIA"}, "422": {"INVALID_REQUEST", "INVALID_PLAN", "UNKNOWN_ENTITY", "UNKNOWN_SOURCE_SPAN"}, "428": {"REAUTHENTICATION_REQUIRED"}, "503": {"STORAGE_UNAVAILABLE"},
				})),
			},
		},
	}
}
