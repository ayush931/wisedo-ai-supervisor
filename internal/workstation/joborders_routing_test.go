package workstation

import (
	"errors"
	"strings"
	"testing"
)

// Frozen pre-routing schema-ordered bytes, including the scope, protect persisted
// job revisions from becoming unreadable after adding optional routing fields.
func TestLegacyJobOrderDigestStable(t *testing.T) {
	const body = `{"schema_version":1,"order_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e01","operation_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e02","item_code":"P-100","target_quantity":10,"target_cycle_time_seconds":"1.5","priority":1,"due_at":"2026-10-20T16:00:00Z","instruction_document_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e03","instruction_revision":1,"instruction_sha256":"abababababababababababababababababababababababababababababababab","template":"quality","fields":[{"name":"result","label":"Result","kind":"choice","required":true,"choices":["pass","fail"]}]}`
	const envelope = `{"schema":"JobOrderBodyV1","schema_version":1,"tenant_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","site_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e05","body":{"schema_version":1,"order_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e01","operation_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e02","item_code":"P-100","target_quantity":10,"target_cycle_time_seconds":"1.5","priority":1,"due_at":"2026-10-20T16:00:00Z","instruction_document_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e03","instruction_revision":1,"instruction_sha256":"abababababababababababababababababababababababababababababababab","template":"quality","fields":[{"name":"result","label":"Result","kind":"choice","required":true,"choices":["pass","fail"]}]}}`
	const digest = "58e81dcadc7e43b9506ed779283be26f76b4a526e4d0ce8f01fe174ca5ce8d0a"
	o, err := DecodeJobOrder([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, got, err := normalizedOrder("8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04", "8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e05", o)
	if err != nil || string(encoded) != envelope || got != digest {
		t.Fatalf("legacy digest changed: %s %s %v", encoded, got, err)
	}
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"valid", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","step_number":1,`, true},
		{"missing item", `"routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","step_number":1,`, false},
		{"missing step", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04",`, false},
		{"missing route", `"step_number":1,`, false},
		{"null step", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","step_number":null,`, false},
		{"empty pair", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"","step_number":null,`, false},
		{"zero step", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","step_number":0,`, false},
		{"negative step", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04","step_number":-1,`, false},
		{"bad route", `"item_id":"8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e06","routing_id":"bad","step_number":1,`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linked, e := DecodeJobOrder([]byte(strings.Replace(body, "{", "{"+tc.fields, 1)))
			if tc.valid {
				if e != nil {
					t.Fatal(e)
				}
				_, _, d, e := normalizedOrder("8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e04", "8a7a1c1e-5d55-4c6a-9a50-2f0f7d0f8e05", linked)
				if e != nil || d == digest {
					t.Fatalf("routing link must affect digest: %s %v", d, e)
				}
			} else if !errors.Is(e, ErrInvalidJobOrder) {
				t.Fatalf("invalid link: %v", e)
			}
		})
	}
}
