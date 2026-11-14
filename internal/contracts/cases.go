package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/advisor"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
)

// caseCatalog publishes the WIS-273 case contracts. There is no trigger input: triggers are
// service-only and not routed until service principals exist (WIS-290).
func caseCatalog() []named {
	return []named{
		{"CaseCancelInputV1", reflect.TypeOf(cases.CancelInput{})},
		{"CaseClarifyInputV1", reflect.TypeOf(cases.ClarifyInput{})},
		{"CaseCloseInputV1", reflect.TypeOf(cases.CloseInput{})},
		{"CasePageV1", reflect.TypeOf(cases.PageV1{})},
		{"CaseReopenInputV1", reflect.TypeOf(cases.ReopenInput{})},
		{"CaseResumeInputV1", reflect.TypeOf(cases.ResumeInput{})},
		{"CaseV1", reflect.TypeOf(cases.CaseV1{})},
		// WIS-629 read-only advisor suggestion, assembled on read; it offers no action.
		{"CaseSuggestionV1", reflect.TypeOf(advisor.SuggestionV1{})},
		// WIS-277 internal tasks: read-only here; only the executor creates one.
		{"TaskCompleteInputV1", reflect.TypeOf(tasks.CompleteInput{})},
		{"TaskPageV1", reflect.TypeOf(tasks.PageV1{})},
		{"TaskTransitionInputV1", reflect.TypeOf(tasks.TransitionInput{})},
		{"TaskV1", reflect.TypeOf(tasks.TaskV1{})},
	}
}

// casePaths documents the WIS-273 routes (rule 07). Mutations take a UUID Idempotency-Key and
// If-Match on the case revision, which must equal the body's expected_revision.
func casePaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, id)
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"(0|[1-9][0-9]*)"$`})
	ifMatch["description"] = "Quoted case revision from the ETag; it must equal expected_revision (else 422), and a stale value is 412 REVISION_CONFLICT."
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	site, kase := param("site", "path", true, id), param("case", "path", true, id)
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "412", "413", "415", "422", "428", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	mutation := func(opID, body string, status, description string) map[string]any {
		return op(opID, []any{site, kase, key, ifMatch, csrf}, body, merge(map[string]any{status: json200(description, "CaseV1")}, writes))
	}
	taskMutation := func(opID, body, description string) map[string]any {
		return op(opID, []any{site, param("task", "path", true, id), key, ifMatch, csrf}, body, merge(map[string]any{"200": json200(description, "TaskV1")}, writes))
	}
	return map[string]any{
		"/v1/sites/{site}/cases": map[string]any{
			"get": op("listCases", []any{site, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Site cases ordered by ID; any active member reads", "CasePageV1")}, reads)),
		},
		"/v1/sites/{site}/cases/{case}": map[string]any{
			"get": op("getCase", []any{site, kase}, "", merge(map[string]any{"200": json200("Case with trigger evidence and its full transition history", "CaseV1")}, reads)),
		},
		"/v1/sites/{site}/cases/{case}/suggestion": map[string]any{
			"get": op("getCaseSuggestion", []any{site, kase}, "", merge(map[string]any{"200": json200("Read-only advisor suggestion for the case (WIS-629), assembled on read and never stored. Statements are labelled approved_fact (a claim of the approved process snapshot the case context pins, in effect during the case's bound windows, with its validity interval and approved passages; a passage whose source is revoked leaves its fact out), observation (a server measurement of a bound window) or hypothesis (the stored, validated investigation output; one citation outside its run leaves every hypothesis out). NOT_READY, for example with no plan approved now or a bound window no longer eligible, carries no statements. It approves and dispatches nothing. 404 ADVISOR_DISABLED when switched off", "CaseSuggestionV1")}, reads)),
		},
		"/v1/sites/{site}/cases/{case}/cancel": map[string]any{
			"post": mutation("cancelCase", "CaseCancelInputV1", "200", "Case canceled, or replayed. Recently authenticated site supervisor only; a case with a committed task is 409 TRANSITION_GUARD_FAILED until that task is canceled"),
		},
		"/v1/sites/{site}/cases/{case}/clarifications": map[string]any{
			"post": mutation("clarifyCase", "CaseClarifyInputV1", "200", "Clarification recorded as attributed case evidence and the case returned from needs_input to investigating, or replayed. Active site supervisor or operator; any other state is 409 ILLEGAL_TRANSITION and records nothing; new_context_id must be an eligible READY context for the same order and line"),
		},
		"/v1/sites/{site}/cases/{case}/close": map[string]any{
			"post": mutation("closeCase", "CaseCloseInputV1", "200", "Observing or escalated case resolved by a recently authenticated site supervisor once its task is terminal and the outcome of its current completion is recorded (any decided status); otherwise 409 TRANSITION_GUARD_FAILED. The reason is stored as the resolution reason and never replaces the measured outcome"),
		},
		"/v1/sites/{site}/cases/{case}/resume": map[string]any{
			"post": mutation("resumeCase", "CaseResumeInputV1", "200", "Escalated case returned to investigating bound to a new eligible READY context for the same order and line (else 409 CONTEXT_NOT_ELIGIBLE or 422); refused when the episode has an approved payload"),
		},
		// WIS-277: tasks are created only by the deterministic executor when it consumes an
		// approval; there is no create route.
		"/v1/sites/{site}/tasks": map[string]any{
			"get": op("listTasks", []any{site, param("case_id", "query", false, id), param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Site tasks ordered by ID, optionally for one case; any active member reads", "TaskPageV1")}, reads)),
		},
		"/v1/sites/{site}/tasks/{task}": map[string]any{
			"get": op("getTask", []any{site, param("task", "path", true, id)}, "", merge(map[string]any{"200": json200("Task with its append-only event history and completions; ETag is the task revision. outcome is the immutable observation of the current completion: late data never revises it (a supervisor reopen starts a new epoch, observed afresh), it is decided after two comparable windows or when the two-hour horizon plus the evaluation delay has passed, and a window under another approved plan revision is not_comparable", "TaskV1")}, reads)),
		},
		"/v1/sites/{site}/tasks/{task}/acknowledge": map[string]any{
			"post": taskMutation("acknowledgeTask", "TaskTransitionInputV1", "Assigned task acknowledged by its assignee, or replayed. Any other actor is 403 TASK_ROLE_REQUIRED; any other state 409 ILLEGAL_TRANSITION"),
		},
		"/v1/sites/{site}/tasks/{task}/start": map[string]any{
			"post": taskMutation("startTask", "TaskTransitionInputV1", "Task started by its assignee from acknowledged, or resumed from blocked with a required reason (422 REASON_REQUIRED), or replayed"),
		},
		"/v1/sites/{site}/tasks/{task}/block": map[string]any{
			"post": taskMutation("blockTask", "TaskTransitionInputV1", "Acknowledged or in-progress task blocked by a recently authenticated site supervisor with a required reason, or replayed"),
		},
		"/v1/sites/{site}/tasks/{task}/complete": map[string]any{
			"post": taskMutation("completeTask", "TaskCompleteInputV1", "In-progress task completed by its assignee with a note and 1..20 same-site, unrevoked evidence sources whose current revision the assignee wrote, at least one a written (manual) report (else 422 COMPLETION_EVIDENCE_UNUSABLE), or replayed. Completion is a report, not proof of recovery"),
		},
		"/v1/sites/{site}/tasks/{task}/cancel": map[string]any{
			"post": taskMutation("cancelTask", "TaskTransitionInputV1", "Active task canceled by a recently authenticated site supervisor with a required reason, or replayed. A replacement assignee needs a newly approved proposal"),
		},
		"/v1/sites/{site}/tasks/{task}/reopen": map[string]any{
			"post": taskMutation("reopenTask", "TaskTransitionInputV1", "Completed task returned to in_progress by a recently authenticated site supervisor with a required reason, as a correction; the earlier completion is kept, or replayed"),
		},
		"/v1/sites/{site}/cases/{case}/reopen": map[string]any{
			"post": mutation("reopenCase", "CaseReopenInputV1", "201", "New episode for a resolved or canceled case, referencing it (200 on replay); 409 CASE_EPISODE_ACTIVE when another episode is active or later"),
		},
	}
}
