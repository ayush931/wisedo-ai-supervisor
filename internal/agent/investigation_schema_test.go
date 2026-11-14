package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// The investigation's response schema is the grammar a local model is held to. Before this fix it
// said only {"result":{},"proposal":{}|null}, so a real model (Qwen3 8B, json_schema strict) never
// learned SpecialistResultV1's fields and every case investigation ended SCHEMA_INVALID; the
// fixture provider hard-codes the shape, so no test noticed.

const (
	tRun      = "5b0e8f0e-6d0a-5c39-9b1e-0f5f9a1c2d3e"
	tCase     = "0d6a3f8e-1b2c-4d5e-8f90-a1b2c3d4e5f6"
	tContext  = "7c1e2d3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	tMeasure  = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	tMachine  = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	tAssignee = "3f2e1d0c-9b8a-4765-8432-10fedcba9876"
)

// validInvestigationOutput is a bounded investigation answer with concrete fixture values.
func validInvestigationOutput() map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(`{
"result":{"schema_version":1,"run_id":"`+tRun+`","role":"investigation","status":"success","artifact_ref":null,
 "cited_evidence_ids":["`+tMeasure+`","`+tMachine+`"],"missing_required_fields":[],"contradictions":[],
 "concise_rationale":"The line fell short of plan while it was stopped without a stated reason.","failure_code":null},
"proposal":{"proposal_id":"`+tRun+`","case_id":"`+tCase+`","case_revision":2,"context_id":"`+tContext+`","context_revision":1,
 "observations":[{"measurement_id":"`+tMeasure+`","evidence_ids":["`+tMeasure+`"],"metric_name":"deficit_good_units","value_decimal":"40","unit":"count","window_start":"2026-11-06T08:00:00Z","window_end":"2026-11-06T09:00:00Z"}],
 "hypotheses":[{"text":"An unexplained machine stoppage during the window may explain the shortfall.","evidence_ids":["`+tMachine+`"],"contradicting_evidence_ids":[]}],
 "unknowns":[],
 "options":[{"kind":"maintenance_investigation","text":"Inspect the stopped line and report what caused the stoppage.","evidence_ids":["`+tMachine+`"]}],
 "selected_option":"maintenance_investigation",
 "task_payload":{"kind":"maintenance_investigation.create","asset_id":"line-a","line_id":"line-a","order_id":"order-1","assignee_id":"`+tAssignee+`","template":"inspect_report_only","description":"Inspect the recorded machine stoppage and report findings to the supervisor under site procedures.","due_at":"2026-11-06T14:00:00Z"},
 "versions":{"policy_revision":1,"plan_revision":3,"graph_revision":1,"model_version":"fixture-model","prompt_version":"`+PromptVersion+`","tool_version":"`+ToolVersion+`"},
 "generated_at":"2026-11-06T09:59:00Z","expires_at":"2026-11-06T10:29:00Z",
 "rationale":"Output fell short of plan in consecutive windows while the line was stopped without a stated reason."}}`), &v); err != nil {
		panic(err)
	}
	return v
}

func TestInvestigationOutputSchemaMatchesValidators(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(outputSchema, &schema); err != nil {
		t.Fatalf("outputSchema is not JSON: %v", err)
	}
	// The known-valid output passes the schema and the Go contracts it stands in front of.
	valid := validInvestigationOutput()
	if err := schemaCheck(schema, valid, "$"); err != nil {
		t.Fatalf("schema rejects the known-valid investigation output: %v", err)
	}
	body, _ := json.Marshal(valid)
	var out SpecialistOutputV1
	if err := json.Unmarshal(body, &out); err != nil || out.Result.Validate() != nil {
		t.Fatalf("fixture result is not a valid SpecialistResultV1: %v %v", err, out.Result.Validate())
	}
	if _, err := recovery.DecodeProposal(out.Proposal); err != nil {
		t.Fatalf("fixture proposal does not strictly decode: %v", err)
	}
	noProposal := validInvestigationOutput()
	noProposal["proposal"] = nil
	if err := schemaCheck(schema, noProposal, "$"); err != nil {
		t.Fatalf("schema rejects a null proposal: %v", err)
	}

	set := func(path string, v any) func(map[string]any) {
		return func(m map[string]any) {
			parts := strings.Split(path, ".")
			cur := m
			for _, p := range parts[:len(parts)-1] {
				cur = cur[p].(map[string]any)
			}
			if v == nil {
				delete(cur, parts[len(parts)-1])
			} else {
				cur[parts[len(parts)-1]] = v
			}
		}
	}
	for _, tc := range []struct {
		name     string
		mutate   func(map[string]any)
		resultGo bool // the Go result validator rejects it too
	}{
		{"wrong status enum", set("result.status", "done"), true},
		{"missing cited_evidence_ids", set("result.cited_evidence_ids", nil), true},
		{"non-uuid run_id", set("result.run_id", "run-1"), true},
		{"wrong schema_version", set("result.schema_version", 2), true},
		{"extra result field", set("result.approved", true), true},
		{"wrong role", set("result.role", "orchestrator"), true},
		{"empty rationale", set("result.concise_rationale", ""), true},
		{"extra proposal field", set("proposal.tenant_id", tCase), false},
		{"unsupported option kind", func(m map[string]any) {
			m["proposal"].(map[string]any)["options"].([]any)[0].(map[string]any)["kind"] = "restart_line"
		}, false},
		{"unsupported task kind", set("proposal.task_payload.kind", "plc.write"), false},
		{"too many options", func(m map[string]any) {
			p := m["proposal"].(map[string]any)
			opts := []any{}
			for _, kind := range []string{"defer", "clarify", "escalate", "maintenance_investigation"} {
				opts = append(opts, map[string]any{"kind": kind, "text": "A schema-valid option.", "evidence_ids": []any{tMachine}})
			}
			p["options"] = opts
		}, false},
		{"non-uuid case_id", set("proposal.case_id", "case-7"), false},
		{"missing versions", set("proposal.versions", nil), false},
		{"numeric timestamp", set("proposal.generated_at", 1794008436), false},
		// Live Qwen3 8B run, 2026-11-06: selected escalate but still sent a task (TASK_PAYLOAD_MISMATCH).
		{"task payload on a non-maintenance option", func(m map[string]any) {
			p := m["proposal"].(map[string]any)
			p["selected_option"] = "escalate"
			p["options"].([]any)[0].(map[string]any)["kind"] = "escalate"
		}, false},
		{"maintenance without a task payload", func(m map[string]any) { m["proposal"].(map[string]any)["task_payload"] = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validInvestigationOutput()
			tc.mutate(v)
			if schemaCheck(schema, v, "$") == nil {
				t.Fatal("schema accepts output the contract rejects")
			}
			if tc.resultGo {
				b, _ := json.Marshal(v["result"])
				if r, err := DecodeSpecialistResult(b); err == nil && r.Validate() == nil {
					t.Fatal("test case is not invalid for the Go validator")
				}
			}
		})
	}
}

func TestWIS702CompactSchemaExcludesUnsupportedMaintenance(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(investigationSchema(false, nil), &schema); err != nil {
		t.Fatal(err)
	}
	valid := validInvestigationOutput()
	if err := schemaCheck(schema, valid, "$"); err == nil {
		t.Fatal("maintenance admitted without captured fault evidence")
	}
	p := valid["proposal"].(map[string]any)
	p["selected_option"], p["task_payload"] = "clarify", nil
	p["options"] = []any{map[string]any{"kind": "clarify", "text": "Ask the line lead about the cause.", "evidence_ids": []any{tMeasure}}}
	if err := schemaCheck(schema, valid, "$"); err != nil {
		t.Fatalf("grounded clarification refused: %v", err)
	}
}

// A required prerequisite belongs in needs_input, not in a maintenance proposal. The old
// grammar let the local model mark inspection questions required even after explicit guidance.
func TestWIS710InvestigationRequiredUnknownCannotSelectMaintenance(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(investigationSchema(true, nil), &schema); err != nil {
		t.Fatal(err)
	}
	output := validInvestigationOutput()
	proposal := output["proposal"].(map[string]any)
	proposal["unknowns"] = []any{map[string]any{"field": "root_cause", "question": "What explains the stoppage?", "required": true}}
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("grammar permits a required prerequisite alongside a maintenance proposal")
	}
	proposal["unknowns"].([]any)[0].(map[string]any)["required"] = false
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatalf("inspection question refused: %v", err)
	}
	output["proposal"] = nil
	result := output["result"].(map[string]any)
	result["status"], result["missing_required_fields"] = "needs_input", []any{"The supervisor must confirm a maintenance assignee."}
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatalf("required input cannot be requested: %v", err)
	}
}

// The known demo's stated reason TOOL_WEAR_17 leaked into a question/rationale and was
// correctly refused as an unsupported number. Constrain generation, not the Go validator.
func TestWIS710InvestigationTextGrammarKeepsReasonCodesOutOfProse(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(investigationSchema(true, nil), &schema); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"concise_rationale", "missing_required_fields", "contradictions"} {
		t.Run(field, func(t *testing.T) {
			output := validInvestigationOutput()
			result := output["result"].(map[string]any)
			result["status"] = "needs_input"
			output["proposal"] = nil
			text := "Supervisor: clarify the reported TOOL_WEAR_17 reason."
			if field == "concise_rationale" {
				result[field] = text
			} else {
				result[field] = []any{text}
			}
			if schemaCheck(schema, output, "$") == nil {
				t.Fatal("grammar lets a reason-code number enter human-facing text")
			}
			text = "Supervisor: clarify the reported stoppage reason."
			if field == "concise_rationale" {
				result[field] = text
			} else {
				result[field] = []any{text}
			}
			if err := schemaCheck(schema, output, "$"); err != nil {
				t.Fatal(err)
			}
		})
	}
	output := validInvestigationOutput()
	output["proposal"].(map[string]any)["rationale"] = "The reported TOOL_WEAR_17 reason needs clarification."
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("proposal prose accepts unsupported number")
	}
}

// The local decoder uses pattern instead of min/maxLength when both are present.
func TestWIS710InvestigationTextPatternsBoundDecodedStrings(t *testing.T) {
	var schema any
	if err := json.Unmarshal(outputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	checked := 0
	var visit func(any)
	visit = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if pattern, ok := node["pattern"].(string); ok && node["maxLength"] != nil {
				checked++
				re := regexp.MustCompile(pattern)
				max := int(node["maxLength"].(float64))
				if !re.MatchString(strings.Repeat("é", max)) {
					t.Fatalf("pattern rejects bounded Unicode prose: %s", pattern)
				}
				for _, invalid := range []string{"", strings.Repeat("a", max+1), "reason 17", "a\"b", "a\\b", "a\nb", "a\x00b"} {
					if re.MatchString(invalid) {
						t.Errorf("pattern alone accepts unsafe or unbounded text (%d bytes): %s", len(invalid), pattern)
					}
				}
			}
			for _, child := range node {
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(schema)
	if checked == 0 {
		t.Fatal("no prose patterns checked")
	}
}

func TestWIS710InvestigationTaskDescriptionCannotAssertCause(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(outputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	output := validInvestigationOutput()
	task := output["proposal"].(map[string]any)["task_payload"].(map[string]any)
	task["description"] = "Investigate the machine stoppage that contributed to the production deficit."
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("task generation can claim an unestablished causal contribution")
	}
	task["description"] = "Inspect the recorded machine stoppage and report findings to the supervisor under site procedures."
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatal(err)
	}
}

func TestWIS710InvestigationCannotAskForAnAlreadyConfirmedAssignee(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(investigationSchema(false, []string{"machine_fault"}), &schema); err != nil {
		t.Fatal(err)
	}
	output := validInvestigationOutput()
	output["proposal"] = nil
	result := output["result"].(map[string]any)
	result["status"] = "needs_input"
	result["missing_required_fields"] = []any{"confirmed_assignee"}
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("generation can request an assignee already confirmed in the case")
	}
	result["missing_required_fields"] = []any{"machine_fault"}
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(investigationSchema(true, []string{}), &schema); err != nil {
		t.Fatal(err)
	}
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("generation can invent a missing prerequisite")
	}
	result["missing_required_fields"] = []any{}
	result["contradictions"] = []any{"The supervisor must reconcile the running log and stopped telemetry."}
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatalf("a real conflict cannot be raised: %v", err)
	}
}

func TestWIS710InvestigationKnownGapsRequireDirectQuestions(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(investigationSchema(false, []string{"confirmed_assignee"}), &schema); err != nil {
		t.Fatal(err)
	}
	output := validInvestigationOutput()
	output["proposal"] = nil
	result := output["result"].(map[string]any)
	result["status"] = "success"
	result["missing_required_fields"] = []any{"confirmed_assignee"}
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("known required gap can be presented as success")
	}
	result["status"] = "needs_input"
	result["missing_required_fields"] = []any{}
	if schemaCheck(schema, output, "$") == nil {
		t.Fatal("known gap can be omitted from the supervisor's questions")
	}
	result["missing_required_fields"] = []any{"confirmed_assignee"}
	if err := schemaCheck(schema, output, "$"); err != nil {
		t.Fatal(err)
	}
}

// TestInvestigationOutputSchemaCoversContract fails when a JSON field is added to (or removed
// from) SpecialistResultV1, RecoveryProposalV1 or their nested types without the schema
// following, so the model-facing grammar cannot silently drift from the strict decoders.
func TestInvestigationOutputSchemaCoversContract(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(outputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatal("schema has no properties")
	}
	covers(t, "$", schema, reflect.TypeOf(SpecialistOutputV1{}))
	covers(t, "$.result", props["result"], reflect.TypeOf(SpecialistResultV1{}))
	branches := 0
	for _, b := range asSlice(props["proposal"].(map[string]any)["anyOf"]) {
		if b.(map[string]any)["type"] != "null" {
			branches++
			covers(t, fmt.Sprintf("$.proposal(anyOf %d)", branches), b, reflect.TypeOf(recovery.RecoveryProposalV1{}))
		}
	}
	if branches == 0 {
		t.Error("$.proposal has no object branch")
	}
}

var timeType = reflect.TypeOf(time.Time{})

func covers(t *testing.T, path string, node any, typ reflect.Type) {
	t.Helper()
	s, _ := node.(map[string]any)
	if s == nil {
		t.Errorf("%s: no schema", path)
		return
	}
	if typ.Kind() == reflect.Pointer {
		if s["type"] == "null" {
			return // null-only in this branch (task_payload outside maintenance)
		}
		s, typ = nonNull(s), typ.Elem()
		if s == nil {
			t.Errorf("%s: nullable field has no non-null branch", path)
			return
		}
	}
	switch {
	case typ == timeType:
		if s["format"] != "date-time" {
			t.Errorf("%s: time field is not a date-time string", path)
		}
	case typ == reflect.TypeOf(json.RawMessage{}):
		// SpecialistOutputV1.Proposal is checked separately against RecoveryProposalV1.
	case typ.Kind() == reflect.Struct:
		props, _ := s["properties"].(map[string]any)
		var want []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			want = append(want, name)
			covers(t, path+"."+name, props[name], f.Type)
		}
		got := make([]string, 0, len(props))
		for k := range props {
			got = append(got, k)
		}
		req := []string{}
		for _, r := range asSlice(s["required"]) {
			req = append(req, r.(string))
		}
		sort.Strings(want)
		sort.Strings(got)
		sort.Strings(req)
		if !slices.Equal(want, got) || !slices.Equal(want, req) || s["additionalProperties"] != false {
			t.Errorf("%s: schema properties %v required %v, contract fields %v (closed object required)", path, got, req, want)
		}
	case typ.Kind() == reflect.Slice:
		covers(t, path+"[]", s["items"], typ.Elem())
	}
}

// nonNull returns the non-null branch of an anyOf [null, X] schema, or the schema itself.
func nonNull(node any) map[string]any {
	s, _ := node.(map[string]any)
	for _, b := range asSlice(s["anyOf"]) {
		if bm := b.(map[string]any); bm["type"] != "null" {
			return bm
		}
	}
	return s
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// schemaCheck is the small JSON Schema subset the investigation schema uses (go.mod carries no
// validator): type, const, enum, anyOf, properties/required/additionalProperties, items,
// min/maxItems, min/maxLength, pattern and the uuid/date-time formats.
func schemaCheck(s map[string]any, v any, path string) error {
	if anyOf := asSlice(s["anyOf"]); anyOf != nil {
		for _, b := range anyOf {
			if schemaCheck(b.(map[string]any), v, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: matches no anyOf branch", path)
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, v) {
		return fmt.Errorf("%s: %v is not const %v", path, v, c)
	}
	if e := asSlice(s["enum"]); e != nil && !slices.ContainsFunc(e, func(x any) bool { return reflect.DeepEqual(x, v) }) {
		return fmt.Errorf("%s: %v not in enum", path, v)
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "null":
		if v != nil {
			return fmt.Errorf("%s: not null", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: not boolean", path)
		}
	case "integer":
		if f, ok := v.(float64); !ok || f != float64(int64(f)) {
			return fmt.Errorf("%s: not integer", path)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: not string", path)
		}
		n := float64(utf8.RuneCountInString(str))
		if m, ok := s["maxLength"].(float64); ok && n > m {
			return fmt.Errorf("%s: longer than %v", path, m)
		}
		if m, ok := s["minLength"].(float64); ok && n < m {
			return fmt.Errorf("%s: shorter than %v", path, m)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			return fmt.Errorf("%s: does not match %s", path, p)
		}
		switch s["format"] {
		case "uuid":
			if _, err := uuid.Parse(str); err != nil {
				return fmt.Errorf("%s: not a uuid", path)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, str); err != nil {
				return fmt.Errorf("%s: not a date-time", path)
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: not array", path)
		}
		if m, ok := s["maxItems"].(float64); ok && float64(len(arr)) > m {
			return fmt.Errorf("%s: more than %v items", path, m)
		}
		if m, ok := s["minItems"].(float64); ok && float64(len(arr)) < m {
			return fmt.Errorf("%s: fewer than %v items", path, m)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range arr {
				if err := schemaCheck(items, x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: not object", path)
		}
		props, _ := s["properties"].(map[string]any)
		for _, r := range asSlice(s["required"]) {
			if _, ok := obj[r.(string)]; !ok {
				return fmt.Errorf("%s: missing %s", path, r)
			}
		}
		for k, x := range obj {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if s["additionalProperties"] == false {
					return fmt.Errorf("%s: unexpected field %s", path, k)
				}
				continue
			}
			if err := schemaCheck(ps, x, path+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}
