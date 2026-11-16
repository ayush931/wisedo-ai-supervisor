package contracts

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

// Version identifies the published OEE contract revision.
const Version = "oee.wisedo.io/v1"

// ProblemCodeV1 is the error body the /v1/oee routes actually emit today.
// RFC 9457 problem+json with correlation IDs is tracked under WIS-229.
type ProblemCodeV1 struct {
	Code string `json:"code"`
}

// ProblemDetailsV1 is the RFC 9457 body the /v1/sites/{site}/oee service routes emit
// (rule 07): a stable code and the request's correlation ID, never request content.
type ProblemDetailsV1 struct {
	Type          string `json:"type" contract:"max=64"`
	Title         string `json:"title" contract:"max=64"`
	Status        int    `json:"status"`
	Code          string `json:"code" contract:"max=64"`
	CorrelationID string `json:"correlation_id" contract:"max=128"`
	// Path names the offending request member for a rejected configuration, for example
	// "fields[2].kind"; it is built from closed member names and indexes only.
	Path string `json:"path,omitempty" contract:"max=256"`
}

type named struct {
	Name string
	Type reflect.Type
}

// oeeCatalog lists the published OEE wire contracts; Catalog (knowledge.go) adds the rest.
func oeeCatalog() []named {
	return []named{
		{"CalculationAcceptedV1", reflect.TypeOf(oee.CalculationAcceptedV1{})},
		{"CalculationRequestV1", reflect.TypeOf(oee.CalculationRequestV1{})},
		{"CalculationStatusV1", reflect.TypeOf(oee.CalculationStatusV1{})},
		{"DatasetV1", reflect.TypeOf(oee.DatasetV1{})},
		{"EvidencePackV1", reflect.TypeOf(oee.EvidencePackV1{})},
		{"OeeCalculationEvidenceV1", reflect.TypeOf(oee.OeeCalculationEvidenceV1{})},
		{"OeeCalculationRequestV1", reflect.TypeOf(oee.OeeCalculationRequestV1{})},
		{"OeeCalculationRunV1", reflect.TypeOf(oee.OeeCalculationRunV1{})},
		{"OeeEvidencePackV1", reflect.TypeOf(oee.OeeEvidencePackV1{})},
		{"OeePolicyRevisionInputV1", reflect.TypeOf(oee.OeePolicyRevisionInputV1{})},
		{"OeePolicyV1", reflect.TypeOf(oee.OeePolicyV1{})},
		{"PolicyV1", reflect.TypeOf(oee.PolicyV1{})},
		{"ProblemCodeV1", reflect.TypeOf(ProblemCodeV1{})},
		{"ProblemDetailsV1", reflect.TypeOf(ProblemDetailsV1{})},
		{"CurrentJobOrderV1", reflect.TypeOf(workstation.CurrentJobOrderV1{})},
		{"CurrentShiftPlanV1", reflect.TypeOf(workstation.CurrentShiftPlanV1{})},
		{"JobOrderBodyV1", reflect.TypeOf(workstation.JobOrderBodyV1{})},
		{"JobOrderPageV1", reflect.TypeOf(workstation.JobOrderPageV1{})},
		{"JobOrderV1", reflect.TypeOf(workstation.JobOrderV1{})},
		{"ShiftPlanApproveInputV1", reflect.TypeOf(workstation.ShiftPlanApproveInputV1{})},
		{"ShiftPlanBodyV1", reflect.TypeOf(workstation.ShiftPlanBodyV1{})},
		{"ShiftPlanSelectionV1", reflect.TypeOf(workstation.ShiftPlanSelectionV1{})},
		{"ShiftPlanSuggestionV1", reflect.TypeOf(workstation.ShiftPlanSuggestionV1{})},
		{"ShiftPlanPageV1", reflect.TypeOf(workstation.ShiftPlanPageV1{})},
		{"ShiftPlanV1", reflect.TypeOf(workstation.ShiftPlanV1{})},
		{"StationProfileBodyV1", reflect.TypeOf(workstation.StationProfileBodyV1{})},
		{"StationProfilePageV1", reflect.TypeOf(workstation.StationProfilePageV1{})},
		{"StationProfilePreviewInputV1", reflect.TypeOf(workstation.StationProfilePreviewInputV1{})},
		{"StationProfilePreviewV1", reflect.TypeOf(workstation.StationProfilePreviewV1{})},
		{"StationProfileV1", reflect.TypeOf(workstation.StationProfileV1{})},
		{"StationAuthorizationInputV1", reflect.TypeOf(workstation.StationAuthorizationInputV1{})},
		{"StationAuthorizationPageV1", reflect.TypeOf(workstation.StationAuthorizationPageV1{})},
		{"StationAuthorizationV1", reflect.TypeOf(workstation.StationAuthorizationV1{})},
		{"StationRecordBodyV1", reflect.TypeOf(workstation.StationRecordBodyV1{})},
		{"StationRecordDetailV1", reflect.TypeOf(workstation.StationRecordDetailV1{})},
		{"StationRecordPageV1", reflect.TypeOf(workstation.StationRecordPageV1{})},
		{"StationRecordTotalsV1", reflect.TypeOf(workstation.StationRecordTotalsV1{})},
		{"StationRecordV1", reflect.TypeOf(workstation.StationRecordV1{})},
		{"ShiftSummaryRequestV1", reflect.TypeOf(workstation.ShiftSummaryRequestV1{})},
		{"ShiftSummarySnapshotV1", reflect.TypeOf(workstation.ShiftSummarySnapshotV1{})},
		{"ShiftSummaryPageV1", reflect.TypeOf(workstation.ShiftSummaryPageV1{})},
		{"ShiftHandoverRequestV1", reflect.TypeOf(workstation.ShiftHandoverRequestV1{})},
		{"ShiftHandoverAdoptInputV1", reflect.TypeOf(workstation.ShiftHandoverAdoptInputV1{})},
		{"ShiftHandoverSnapshotV1", reflect.TypeOf(workstation.ShiftHandoverSnapshotV1{})},
		{"ShiftHandoverPageV1", reflect.TypeOf(workstation.ShiftHandoverPageV1{})},
		{"ShiftRunStartInputV1", reflect.TypeOf(workstation.ShiftRunStartInputV1{})},
		{"ShiftRunEndInputV1", reflect.TypeOf(workstation.ShiftRunEndInputV1{})},
		{"ShiftRunV1", reflect.TypeOf(workstation.ShiftRunV1{})},
	}
}

func enums() map[reflect.Type][]string {
	status := []string{}
	for _, s := range oee.Statuses() {
		status = append(status, string(s))
	}
	reasons := []string{}
	for _, r := range oee.ReasonCodes() {
		reasons = append(reasons, string(r))
	}
	sort.Strings(reasons)
	costReasons := []string{}
	for _, c := range oee.CostReasonCodes() {
		costReasons = append(costReasons, string(c))
	}
	sort.Strings(costReasons)
	reasonCodes := []string{}
	for _, r := range workstation.SummaryReasons() {
		reasonCodes = append(reasonCodes, string(r))
	}
	sort.Strings(reasonCodes)
	carry := []string{}
	for _, c := range workstation.CarryReasons() {
		carry = append(carry, string(c))
	}
	sort.Strings(carry)
	kinds := []string{}
	for _, k := range workstation.RecordKinds() {
		kinds = append(kinds, string(k))
	}
	values := map[reflect.Type][]string{
		reflect.TypeOf(oee.Status("")):                      status,
		reflect.TypeOf(oee.ReasonCode("")):                  reasons,
		reflect.TypeOf(oee.CostReasonCode("")):              costReasons,
		reflect.TypeOf(oee.LossCategory("")):                {string(oee.LossCategoryAvailability), string(oee.LossCategoryPerformance), string(oee.LossCategoryQuality)},
		reflect.TypeOf(oee.LossClassification("")):          {string(oee.ClassificationPlanned), string(oee.ClassificationUnclassified), string(oee.ClassificationUnplanned)},
		reflect.TypeOf(workstation.ShiftPlanState("")):      {string(workstation.StateApproved), string(workstation.StateDraft), string(workstation.StateSuperseded)},
		reflect.TypeOf(workstation.JobOrderState("")):       {string(workstation.JobOrderCurrent), string(workstation.JobOrderSuperseded)},
		reflect.TypeOf(workstation.DeliveryState("")):       {string(workstation.DeliveryCurrent), string(workstation.DeliveryInstructionUnavailable), string(workstation.DeliveryNeedsReplan)},
		reflect.TypeOf(workstation.InstructionReason("")):   {string(workstation.InstructionNotConfirmed), string(workstation.InstructionRevoked), string(workstation.InstructionSuperseded)},
		reflect.TypeOf(workstation.FieldSource("")):         {string(workstation.SourceBoth), string(workstation.SourceJobOrder), string(workstation.SourceStationProfile)},
		reflect.TypeOf(workstation.RecordKind("")):          kinds,
		reflect.TypeOf(workstation.RecordSource("")):        {string(workstation.SourceManualTablet)},
		reflect.TypeOf(workstation.QualityDisposition("")):  {string(workstation.DispositionAccept), string(workstation.DispositionHold), string(workstation.DispositionReject)},
		reflect.TypeOf(workstation.MaterialIssue("")):       {string(workstation.MaterialDamaged), string(workstation.MaterialOther), string(workstation.MaterialShortage), string(workstation.MaterialWrong)},
		reflect.TypeOf(workstation.AuthorizationAction("")): {string(workstation.AuthorizationGrant), string(workstation.AuthorizationRevoke)},
		reflect.TypeOf(workstation.SummaryReadiness("")):    {string(workstation.SummaryInvalid), string(workstation.SummaryLimited), string(workstation.SummaryNotReady), string(workstation.SummaryReady)},
		reflect.TypeOf(workstation.SummaryReason("")):       reasonCodes,
		reflect.TypeOf(workstation.CarryReason("")):         carry,
		reflect.TypeOf(store.AgentUnavailableReason("")):    {string(store.AgentModeDisabled), string(store.AgentNotConfigured)},
		reflect.TypeOf(workstation.HandoverStatus("")):      {string(workstation.HandoverAdopted), string(workstation.HandoverDraft)},
		reflect.TypeOf(workstation.ShiftRunState("")):       {string(workstation.RunEnded), string(workstation.RunPlanned), string(workstation.RunStarted)},
		reflect.TypeOf(workstation.CompletionState("")):     {string(workstation.CompletionCompleted), string(workstation.CompletionInProgress), string(workstation.CompletionNotStarted), string(workstation.CompletionUnknown)},
		reflect.TypeOf(workstation.MasterDataUnit("")):      {string(workstation.MasterDataUnitCount)},
	}
	addInterviewEnums(values)
	values[reflect.TypeOf(roster.Decision(""))] = []string{string(roster.DecisionConfirm), string(roster.DecisionWithdraw)}
	addConfigurationDraftEnums(values)
	addFeedbackEnums(values)
	return values
}

// Files returns every generated artifact keyed by path relative to contracts/.
func Files() map[string][]byte {
	files := map[string][]byte{}
	schemas := map[string]any{}
	for _, c := range Catalog() {
		s := Schema(c.Type, enums())
		restrict(c.Name, s)
		files["schemas/"+c.Name+".schema.json"] = encode(s)
		delete(s, "$schema")
		schemas[c.Name] = s
	}
	files["typescript/oee.ts"] = typeScript(schemas)
	doc := openAPI(schemas)
	addKnowledge(doc)
	addInterviews(doc)
	addMasterData(doc)
	addConfigurationImports(doc)
	addConfigurationDrafts(doc)
	addOperational(doc)
	merge(doc["paths"].(map[string]any), planPaths())
	merge(doc["paths"].(map[string]any), contextPaths())
	addChat(doc)
	addFeedback(doc)
	files["openapi.json"] = encode(doc)
	return files
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func problems(codes ...string) map[string]any {
	out := map[string]any{}
	for _, c := range codes {
		out[c] = map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": ref("ProblemCodeV1")}}}
	}
	return out
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func problemDetails(codes ...string) map[string]any {
	out := map[string]any{}
	for _, c := range codes {
		out[c] = map[string]any{"description": "Error", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	}
	return out
}

func jsonBody(name string) map[string]any {
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref(name)}}}
}

// sitePaths documents the WIS-229 OEE service under /v1/sites/{site}/oee (ADR 0002 Sec. 8,
// rule 07). Mutations take Idempotency-Key; publication also takes If-Match.
func sitePaths() map[string]any {
	uuidParam := func(name string) map[string]any {
		return map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}}
	}
	header := func(name string) map[string]any {
		return map[string]any{"name": name, "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}
	}
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(id string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": id, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	site := uuidParam("site")
	return map[string]any{
		"/v1/sites/{site}/oee/calculations": map[string]any{
			"post": op("acceptSiteCalculation", []any{site, header("Idempotency-Key")}, "OeeCalculationRequestV1",
				merge(merge(map[string]any{"202": json200("Calculation run accepted or replayed", "OeeCalculationRunV1")}, writes), problemDetails("412", "413", "429"))),
		},
		"/v1/sites/{site}/oee/calculations/{run}": map[string]any{
			"get": op("getSiteCalculation", []any{site, uuidParam("run")}, "", merge(map[string]any{"200": json200("Calculation run", "OeeCalculationRunV1")}, reads)),
		},
		"/v1/sites/{site}/oee/calculations/{run}:cancel": map[string]any{
			"post": op("cancelSiteCalculation", []any{site, uuidParam("run"), header("Idempotency-Key")}, "", merge(map[string]any{"202": json200("Cancellation recorded", "OeeCalculationRunV1")}, writes)),
		},
		"/v1/sites/{site}/oee/evidence/{evidence}": map[string]any{
			"get": op("getSiteEvidence", []any{site, uuidParam("evidence")}, "", merge(map[string]any{"200": json200("Sealed candidate evidence", "OeeCalculationEvidenceV1")}, reads)),
		},
		"/v1/sites/{site}/oee/policies/{policy}": map[string]any{
			"get": op("getSitePolicy", []any{site, uuidParam("policy")}, "", merge(map[string]any{"200": json200("Policy with revisions and current publication", "OeePolicyV1")}, reads)),
		},
		"/v1/sites/{site}/oee/policies/{policy}/revisions": map[string]any{
			"post": op("createSitePolicyRevision", []any{site, uuidParam("policy"), header("Idempotency-Key")}, "OeePolicyRevisionInputV1",
				merge(merge(map[string]any{"201": json200("Immutable revision appended", "OeePolicyV1")}, writes), problemDetails("413"))),
		},
		"/v1/sites/{site}/oee/policies/{policy}/revisions/{revision}:publish": map[string]any{
			"post": op("publishSitePolicyRevision", []any{site, uuidParam("policy"), uuidParam("revision"), header("Idempotency-Key"), header("If-Match")}, "",
				merge(map[string]any{"200": json200("Revision published", "OeePolicyV1")}, merge(writes, problemDetails("412", "428")))),
		},
	}
}

func json200(description, schemaName string) map[string]any {
	return map[string]any{"description": description, "content": map[string]any{"application/json": map[string]any{"schema": ref(schemaName)}}}
}

func openAPI(schemas map[string]any) map[string]any {
	runID := map[string]any{"name": "run_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}}
	// The OEE routes fail closed today: no calculation or persistence exists
	// (WIS-229), so every operation documents 503 as its current outcome.
	unavailable := problems("401", "422", "503")
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "wiseDo AI Supervisor OEE component",
			"version":     Version,
			"description": "OEE component contract. The site-scoped /v1/sites/{site}/oee service (WIS-229) accepts calculations, seals candidate evidence and governs policies, with RFC 9457 errors. The legacy /v1/oee routes stay fail-closed (503). Callers authenticate with the OIDC browser session cookie; tenant and actor are never accepted from a payload.",
		},
		"paths": merge(merge(merge(merge(merge(merge(merge(merge(merge(merge(merge(sitePaths(), memberPaths()), proposalPaths()), casePaths()), shiftPlanPaths()), jobOrderPaths()), stationProfilePaths()), stationRecordPaths()), shiftSummaryPaths()), shiftHandoverPaths()), shiftRunPaths()), map[string]any{
			"/v1/oee/calculations": map[string]any{
				"post": map[string]any{
					"operationId": "requestCalculation",
					"security":    []any{map[string]any{"session": []any{}}},
					"requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref("CalculationRequestV1")}}},
					"responses":   merge(merge(map[string]any{"202": json200("Accepted for processing", "CalculationAcceptedV1")}, unavailable), problems("413", "415")),
				},
			},
			"/v1/oee/calculations/{run_id}": map[string]any{
				"get": map[string]any{
					"operationId": "getCalculation",
					"security":    []any{map[string]any{"session": []any{}}},
					"parameters":  []any{runID},
					"responses":   merge(map[string]any{"200": json200("Calculation status", "CalculationStatusV1")}, unavailable),
				},
			},
			"/v1/oee/calculations/{run_id}:cancel": map[string]any{
				"post": map[string]any{
					"operationId": "cancelCalculation",
					"security":    []any{map[string]any{"session": []any{}}},
					"parameters":  []any{runID},
					"responses":   merge(map[string]any{"200": json200("Calculation status", "CalculationStatusV1")}, unavailable),
				},
			},
			"/v1/oee/evidence/{evidence_id}": map[string]any{
				"get": map[string]any{
					"operationId": "getEvidence",
					"security":    []any{map[string]any{"session": []any{}}},
					"parameters":  []any{map[string]any{"name": "evidence_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}}},
					"responses":   merge(map[string]any{"200": json200("Sealed evidence pack", "EvidencePackV1")}, unavailable),
				},
			},
		}),
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"session": map[string]any{"type": "apiKey", "in": "cookie", "name": "__Host-supervisor"},
			},
		},
	}
}

// encode is deterministic: encoding/json sorts map keys.
func encode(v any) []byte {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
