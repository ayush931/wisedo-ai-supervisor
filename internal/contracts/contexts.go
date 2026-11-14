package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
)

func contextCatalog() []named {
	return []named{{"PlantContextRequestV1", reflect.TypeOf(plantcontext.Request{})}}
}

func contextPaths() map[string]any {
	return map[string]any{
		"/v1/sites/{site}/contexts": map[string]any{
			"post": map[string]any{
				"operationId": "createPlantContext", "security": []any{map[string]any{"session": []any{}}},
				"parameters": []any{
					map[string]any{"name": "site", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}},
					map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}},
					map[string]any{"name": "X-CSRF-Token", "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1}},
					map[string]any{"name": "Origin", "in": "header", "required": true, "schema": map[string]any{"type": "string", "format": "uri"}},
				},
				"requestBody": jsonBody("PlantContextRequestV1"),
				"responses": merge(map[string]any{"201": map[string]any{"description": "Immutable production context and current eligibility, including replay. Cache-Control: no-store."}}, problemSet(map[string][]string{
					"401": {"AUTHENTICATION_REQUIRED"}, "403": {"CSRF_REJECTED", "PERMISSION_DENIED"}, "404": {"NOT_FOUND"}, "409": {"IDEMPOTENCY_CONFLICT"}, "413": {"BODY_TOO_LARGE"}, "415": {"UNSUPPORTED_MEDIA"}, "422": {"INVALID_REQUEST", "INVALID_CONTEXT", "WINDOW_TOO_LARGE", "UNKNOWN_ENTITY", "UNKNOWN_PROCESS_SNAPSHOT"}, "503": {"STORAGE_UNAVAILABLE"},
				})),
			},
		},
	}
}
