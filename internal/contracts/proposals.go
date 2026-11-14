package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/modelhosting"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
)

// proposalCatalog publishes the WIS-274 approval contracts. There is no proposal creation
// input: proposals are submitted by proposal generation, never by an approval caller.
func proposalCatalog() []named {
	return []named{
		{"ProposalDecisionInputV1", reflect.TypeOf(approvals.DecisionInput{})},
		{"ProposalPageV1", reflect.TypeOf(approvals.Page{})},
		{"ProposalRevokeInputV1", reflect.TypeOf(approvals.RevokeInput{})},
		{"ProposalV1", reflect.TypeOf(approvals.View{})},
		{"MaintenanceRosterDecisionInputV1", reflect.TypeOf(roster.DecisionInput{})},
		{"MaintenanceRosterEntryV1", reflect.TypeOf(roster.Entry{})},
		{"MaintenanceRosterPageV1", reflect.TypeOf(roster.Page{})},
		{"ModelHostingDecisionInputV1", reflect.TypeOf(modelhosting.DecisionInput{})},
		{"SiteModelHostingV1", reflect.TypeOf(modelhosting.SiteModelHostingV1{})},
	}
}

// proposalPaths documents the WIS-274 routes (rule 07). Decisions and revocation take
// Idempotency-Key (a UUID) and If-Match on the approval state revision, which must equal the
// body's expected_revision.
func proposalPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, id)
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"(0|[1-9][0-9]*)"$`})
	ifMatch["description"] = "Quoted approval state revision from the ETag; it must equal expected_revision (else 422), and a stale value is 412 REVISION_CONFLICT."
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	site, proposal := param("site", "path", true, id), param("proposal", "path", true, id)
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
	return map[string]any{
		"/v1/sites/{site}/proposals": map[string]any{
			"get": op("listProposals", []any{site, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Site proposals ordered by ID; any active member reads", "ProposalPageV1")}, reads)),
		},
		"/v1/sites/{site}/proposals/{proposal}": map[string]any{
			"get": op("getProposal", []any{site, proposal}, "",
				merge(map[string]any{"200": json200("Current proposal revision with its exact canonical payload, approval state and decision history; a closed window is recorded as expired first", "ProposalV1")}, reads)),
		},
		"/v1/sites/{site}/proposals/{proposal}/decisions": map[string]any{
			"post": op("decideProposal", []any{site, proposal, key, ifMatch, csrf}, "ProposalDecisionInputV1",
				merge(map[string]any{"200": json200("Approval or rejection recorded, or replayed. Only a recently authenticated human site supervisor who did not submit or edit the proposal may decide, once per proposal revision (403 REVIEWER_NOT_ELIGIBLE); expiry is 409 APPROVAL_EXPIRED", "ProposalV1")}, writes)),
		},
		"/v1/sites/{site}/proposals/{proposal}/revoke": map[string]any{
			"post": op("revokeProposal", []any{site, proposal, key, ifMatch, csrf}, "ProposalRevokeInputV1",
				merge(map[string]any{"200": json200("Approval revoked before consumption, or replayed", "ProposalV1")}, writes)),
		},
		// WIS-276: the supervisor-confirmed maintenance assignee roster a recovery proposal may
		// assign from. If-Match is the line/assignee entry revision, "0" for a first decision.
		"/v1/sites/{site}/maintenance-roster": map[string]any{
			"get": op("listMaintenanceRoster", []any{site, param("line_id", "query", false, id), param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, map[string]any{"type": "string", "maxLength": 128})}, "",
				merge(map[string]any{"200": json200("Each line/assignee pair at its latest decision, ordered by line then assignee; site owners and supervisors read", "MaintenanceRosterPageV1")}, problemDetails("401", "403", "404", "422", "503"))),
		},
		"/v1/sites/{site}/maintenance-roster/decisions": map[string]any{
			"post": op("decideMaintenanceRoster", []any{site, key, ifMatch, csrf}, "MaintenanceRosterDecisionInputV1",
				merge(map[string]any{"200": json200("Confirmation or withdrawal recorded, or replayed. Only a recently authenticated site supervisor decides; the line must be a current line asset (422 LINE_UNAVAILABLE) and a confirmed assignee an active operator or supervisor (422 ASSIGNEE_NOT_ELIGIBLE); a decision that changes nothing is 409 ROSTER_UNCHANGED", "MaintenanceRosterEntryV1")}, writes)),
		},
		// WIS-623: the per-site switch for hosted model inference (rule 08). Off by default; the
		// ETag and If-Match are the switch revision, "0" before the first decision.
		"/v1/sites/{site}/model-hosting": map[string]any{
			"get": op("getModelHosting", []any{site}, "",
				merge(map[string]any{"200": json200("The site's hosted-inference switch and its decisions, newest first; revision 0 and off when never decided. Site owners and supervisors read", "SiteModelHostingV1")}, problemDetails("401", "403", "404", "422", "503"))),
		},
		"/v1/sites/{site}/model-hosting/decisions": map[string]any{
			"post": op("decideModelHosting", []any{site, key, ifMatch, csrf}, "ModelHostingDecisionInputV1",
				merge(map[string]any{"200": json200("Switch turned on (with a data processing agreement reference) or off, or replayed. Only a recently authenticated owner of the site decides (403 PERMISSION_DENIED, 428 REAUTHENTICATION_REQUIRED); turning it off takes effect on the next model call, and a run pinned to the hosted endpoint then stops with HOSTED_MODEL_DISABLED; a decision that changes nothing is 409 MODEL_HOSTING_UNCHANGED", "SiteModelHostingV1")}, writes)),
		},
	}
}
