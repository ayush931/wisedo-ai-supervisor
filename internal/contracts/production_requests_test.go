package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublishedProductionRequestBodies(t *testing.T) {
	files := Files()
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(files["openapi.json"], &doc); err != nil {
		t.Fatal(err)
	}
	delta := `{"asset_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","order_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","interval_start":"2026-09-01T00:30:00Z","interval_end":"2026-09-01T00:45:00Z","good_units":"10","unit":"count"}`
	for _, tc := range []struct{ path, name, body string }{
		{"/operational-sources/{source}/batches", "OperationalBatchV1", `{"schema_version":1,"source_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","source_revision":2,"mapping_revision":2,"events":[{"external_key":"count-1","revision":1,"kind":"production_delta","source_sequence":1,"event_at":"2026-09-01T00:45:00Z","payload":` + delta + `}]}`},
		{"/plan-revisions", "PlanCaptureInputV1", `{"plan":{"asset_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","order_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","plan_revision":1,"window_start":"2026-09-01T00:30:00Z","window_end":"2026-09-01T08:30:00Z","unit":"count","planned_breaks":[],"rate_segments":[{"start":"2026-09-01T00:30:00Z","end":"2026-09-01T08:30:00Z","target_good_units_per_second":"0.1"}]},"source_span_ids":["dddddddd-dddd-4ddd-8ddd-dddddddddddd"]}`},
		{"/contexts", "PlantContextRequestV1", `{"asset_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","order_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","process_snapshot_id":"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee","window_start":"2026-09-01T00:30:00Z","window_end":"2026-09-01T00:45:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := doc.Paths["/v1/sites/{site}"+tc.path]["post"].RequestBody.Content["application/json"].Schema.Ref
			if ref != "#/components/schemas/"+tc.name {
				t.Fatalf("request body resolves to %q, want %s", ref, tc.name)
			}
			var schema, body map[string]any
			if err := json.Unmarshal(files["schemas/"+strings.TrimPrefix(ref, "#/components/schemas/")+".schema.json"], &schema); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			if err := validateSchemaValue(schema, body); err != nil {
				t.Fatalf("valid wire request rejected: %v", err)
			}
			body["tenant_id"] = "caller-supplied"
			if validateSchemaValue(schema, body) == nil {
				t.Fatal("published request permits caller-supplied tenant")
			}
			delete(body, "tenant_id")
			if tc.name == "OperationalBatchV1" {
				payload := body["events"].([]any)[0].(map[string]any)["payload"].(map[string]any)
				batch := doc.Components.Schemas[tc.name]
				events := batch["properties"].(map[string]any)["events"].(map[string]any)
				deltaEvent := events["items"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
				payloadRef := deltaEvent["properties"].(map[string]any)["payload"].(map[string]any)["$ref"]
				if payloadRef != "#/components/schemas/ProductionDeltaV1" {
					t.Fatalf("production_delta payload resolves to %v", payloadRef)
				}
				if err := validateSchemaValue(doc.Components.Schemas["ProductionDeltaV1"], payload); err != nil {
					t.Fatal(err)
				}
				payload["good_units"] = float64(10)
				if validateSchemaValue(schema, body) == nil {
					t.Fatal("production_delta payload permits numeric rather than decimal-string units")
				}
			}
		})
	}
	// Simulator metadata must not become an API or generated-client contract.
	for _, name := range []string{"FactoryManifestV1", "FactoryAnswerKeyV1", "FactoryObservationV1", "ConfigurationCSVRowsV1"} {
		if _, published := files["schemas/"+name+".schema.json"]; published || strings.Contains(string(files["openapi.json"]), name) || strings.Contains(string(files["typescript/oee.ts"]), name) {
			t.Errorf("simulator-private type exported to API consumers: %s", name)
		}
	}
}
