package contracts

func restrictShiftSuggestion(name string, s map[string]any) {
	if name == "JobOrderBodyV1" {
		p := s["properties"].(map[string]any)
		p["step_number"].(map[string]any)["minimum"] = 1
		p["routing_id"].(map[string]any)["description"] = "Optional current master-data routing. Supply item_id, routing_id and step_number together, or omit all three."
		p["item_id"].(map[string]any)["description"] = "Catalog item UUID for the routing. item_code remains the human product code or name."
		s["dependentRequired"] = map[string]any{"item_id": []string{"routing_id", "step_number"}, "routing_id": []string{"item_id", "step_number"}, "step_number": []string{"item_id", "routing_id"}}
		return
	}
	if name != "ShiftPlanSelectionV1" && name != "ShiftPlanSuggestionV1" {
		return
	}
	p := s["properties"].(map[string]any)
	p["schema_version"].(map[string]any)["const"] = 1
	if name == "ShiftPlanSelectionV1" {
		ids := p["job_order_ids"].(map[string]any)
		ids["minItems"], ids["maxItems"], ids["uniqueItems"] = 1, 50, true
		ids["items"] = map[string]any{"type": "string", "format": "uuid"}
	} else {
		p["method"].(map[string]any)["enum"] = []string{"greedy", "ortools_cp_sat"}
	}
}

// shiftPlanPaths documents the WIS-520 shift plan routes (rule 07). Mutations take
// Idempotency-Key; revise and approve also take If-Match on the plan's state_revision.
func shiftPlanPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "minLength": 3, "maxLength": 128})
	site, plan := param("site", "path", true, id), param("plan", "path", true, id)
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/shift-plans": map[string]any{
			"post": op("createShiftPlan", []any{site, key}, "ShiftPlanBodyV1", merge(map[string]any{"201": json200("Plan created with draft revision 1, or replayed", "ShiftPlanV1")}, writes)),
			"get": op("listShiftPlans", []any{site, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Plan heads ordered by ID", "ShiftPlanPageV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/current": map[string]any{
			"get": op("getCurrentShiftPlan", []any{site, param("line_id", "query", true, id), param("at", "query", false, map[string]any{"type": "string", "format": "date-time"})}, "",
				merge(map[string]any{"200": json200("Approved revision in force for the line", "CurrentShiftPlanV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/suggestions": map[string]any{
			"post": merge(op("suggestShiftPlan", []any{site}, "ShiftPlanSelectionV1", merge(map[string]any{"200": json200("Advisory assignments and timing preview; unplaced orders carry reason codes", "ShiftPlanSuggestionV1")}, writes)), map[string]any{"description": "Owner/supervisor only. CSRF required. Like profile-previews this computation persists no plan, approval, receipt or audit mutation, so no Idempotency-Key is required. Current order revisions and active master-data routing steps are resolved server-side. Optional item_id, routing_id and step_number link job orders and must be supplied together. item_code remains human display text. Catalog items have no code field, so unlinked codes cannot infer routing; link the job to a routing step. Knowledge snapshots are optional conflict cross-checks; missing or stale snapshots do not block. Limits: 50 orders, 200 operations, 100 stations, 100 merged downtime intervals; one concurrent solve per site. CP-SAT has a six-second deadline; unavailable, timed-out, invalid or infeasible solver output falls back to greedy. No placements returns 422 NO_FEASIBLE_SCHEDULE. The current plan format permits one job per station. Routing step order and selected knowledge precedence are enforced in the timing preview; cyclic or unplaceable dependencies carry reason codes. Operations outside the selection produce a warning. A dedicated session lock spans the solve, with no open database transaction during the child process."}),
		},
		"/v1/sites/{site}/shift-plans/{plan}": map[string]any{
			"get": op("getShiftPlan", []any{site, plan}, "", merge(map[string]any{"200": json200("Plan with full revision and approval history", "ShiftPlanV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/{plan}/revisions": map[string]any{
			"post": op("reviseShiftPlan", []any{site, plan, key, ifMatch}, "ShiftPlanBodyV1",
				merge(map[string]any{"201": json200("Draft revision appended", "ShiftPlanV1")}, merge(writes, problemDetails("412", "428")))),
		},
		"/v1/sites/{site}/shift-plans/{plan}/approvals": map[string]any{
			"post": op("approveShiftPlan", []any{site, plan, key, ifMatch}, "ShiftPlanApproveInputV1",
				merge(map[string]any{"200": json200("Current revision approved; any earlier approved revision superseded", "ShiftPlanV1")}, merge(writes, problemDetails("412", "428")))),
		},
	}
}

// jobOrderPaths documents the WIS-520 job order and workstation delivery routes (rule 07).
// Mutations take Idempotency-Key; revise also takes If-Match on current_revision.
func jobOrderPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "minLength": 3, "maxLength": 128})
	site, order, station := param("site", "path", true, id), param("job_order", "path", true, id), param("workstation", "path", true, id)
	atParam := param("at", "query", false, map[string]any{"type": "string", "format": "date-time"})
	includeCurrent := param("include", "query", false, map[string]any{"type": "string", "enum": []string{"current"}})
	includeCurrent["description"] = "Set to current to include each job order's current revision body as current_order."
	// at only selects the shift window [starts_at,ends_at); delivery always uses the plan's
	// currently approved revision and the job order's and instruction's current state, never
	// a historical view as of at.
	atParam["description"] = "Instant (RFC 3339, default server time) that selects the shift window [starts_at, ends_at). The plan revision approved now and the current job-order and instruction state are used; this is not a historical as-of query."
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/job-orders": map[string]any{
			"post": op("createJobOrder", []any{site, key}, "JobOrderBodyV1", merge(map[string]any{"201": json200("Job order created at revision 1, or replayed", "JobOrderV1")}, writes)),
			"get": op("listJobOrders", []any{site, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id), includeCurrent}, "",
				merge(map[string]any{"200": json200("Job order heads ordered by ID; include=current adds each head's current_order body", "JobOrderPageV1")}, reads)),
		},
		"/v1/sites/{site}/job-orders/{job_order}": map[string]any{
			"get": op("getJobOrder", []any{site, order}, "", merge(map[string]any{"200": json200("Job order with full revision history", "JobOrderV1")}, reads)),
		},
		"/v1/sites/{site}/job-orders/{job_order}/revisions": map[string]any{
			"post": op("reviseJobOrder", []any{site, order, key, ifMatch}, "JobOrderBodyV1",
				merge(map[string]any{"201": json200("Revision appended; earlier revisions superseded", "JobOrderV1")}, merge(writes, problemDetails("412", "428")))),
		},
		"/v1/sites/{site}/workstations/{workstation}/current-job-order": map[string]any{
			"get": op("getCurrentJobOrder", []any{site, station, atParam}, "",
				merge(map[string]any{"200": json200("Job order delivered to the workstation; needs_replan when its pin is superseded or instruction_unavailable when its instruction is no longer a confirmed-current document, each with a null order", "CurrentJobOrderV1")}, merge(reads, problemDetails("409")))),
		},
	}
}

// stationProfilePaths documents the WIS-520 station profile routes (rule 07). A revision
// takes Idempotency-Key and If-Match on current_revision ("0" creates the profile). A
// rejected configuration is 422 INVALID_STATION_PROFILE or UNSUPPORTED_STATION_CONFIGURATION
// with the offending member in path.
func stationProfilePaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "minLength": 3, "maxLength": 128})
	site, station := param("site", "path", true, id), param("workstation", "path", true, id)
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/workstations": map[string]any{
			"get": op("listStationProfiles", []any{site, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Configured workstations ordered by ID", "StationProfilePageV1")}, reads)),
		},
		"/v1/sites/{site}/workstations/{workstation}/profile": map[string]any{
			"get": op("getStationProfile", []any{site, station}, "", merge(map[string]any{"200": json200("Station profile with full revision history; the current profile is revisions[current_revision-1]", "StationProfileV1")}, reads)),
		},
		"/v1/sites/{site}/workstations/{workstation}/profile-revisions": map[string]any{
			"post": op("reviseStationProfile", []any{site, station, key, ifMatch}, "StationProfileBodyV1",
				merge(map[string]any{"201": json200("Revision appended (If-Match \"0\" creates); earlier revisions superseded", "StationProfileV1")}, merge(writes, problemDetails("412", "428")))),
		},
		"/v1/sites/{site}/workstations/{workstation}/profile-previews": map[string]any{
			"post": op("previewStationProfile", []any{site, station}, "StationProfilePreviewInputV1",
				merge(map[string]any{"200": json200("Resolved operator form for the candidate profile, optionally merged with a current job order revision; nothing is persisted", "StationProfilePreviewV1")}, writes)),
		},
	}
}

// stationRecordPaths documents the WIS-520 operator station record and station
// authorization routes (rule 07). A record takes Idempotency-Key; a grant or revocation also
// takes If-Match on the grantee's current revision ("0" first). Submission is 403
// STATION_NOT_AUTHORIZED without a current authorization, 403 CORRECTION_NOT_PERMITTED when a
// correction comes from neither the recorder nor an owner or supervisor, and 409 with the
// delivery state (NO_JOB_ORDER_TO_RECORD, NEEDS_REPLAN, INSTRUCTION_UNAVAILABLE,
// INSTRUCTION_REVOKED, JOB_ORDER_NOT_CURRENT), LIFECYCLE_CONFLICT, STATION_JOB_COMPLETED,
// CORRECTION_CONFLICT, STATION_NOT_CONFIGURED or STATION_FORM_CONFLICT when the workstation
// cannot accept it; 422 adds INVALID_CORRECTION, QUALITY_RECORD_CANNOT_RELEASE,
// EVENT_TOO_OLD and EVENT_OUTSIDE_SHIFT.
func stationRecordPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "minLength": 3, "maxLength": 128})
	site, station, record := param("site", "path", true, id), param("workstation", "path", true, id), param("record", "path", true, id)
	limit := param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100})
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/workstations/{workstation}/records": map[string]any{
			"post": op("submitStationRecord", []any{site, station, key}, "StationRecordBodyV1", merge(map[string]any{"201": json200("Record appended, or replayed", "StationRecordV1")}, writes)),
			"get": op("listStationRecords", []any{site, station, param("job_order_id", "query", false, id), limit, param("cursor", "query", false, id)}, "",
				merge(map[string]any{"200": json200("Records in received order, superseded ones included", "StationRecordPageV1")}, reads)),
		},
		"/v1/sites/{site}/workstations/{workstation}/records/{record}": map[string]any{
			"get": op("getStationRecord", []any{site, station, record}, "", merge(map[string]any{"200": json200("Record with its correction chain, oldest first", "StationRecordDetailV1")}, reads)),
		},
		"/v1/sites/{site}/workstations/{workstation}/authorizations": map[string]any{
			"post": op("authorizeStation", []any{site, station, key, ifMatch}, "StationAuthorizationInputV1",
				merge(map[string]any{"201": json200("Grant or revocation appended, or replayed", "StationAuthorizationV1")}, merge(writes, problemDetails("412", "428")))),
			"get": op("listStationAuthorizations", []any{site, station, limit, param("cursor", "query", false, map[string]any{"type": "string", "maxLength": 1024})}, "",
				merge(map[string]any{"200": json200("Grantees with their authorization history", "StationAuthorizationPageV1")}, reads)),
		},
	}
}

// shiftSummaryPaths documents the WIS-520 shift summary routes (rule 07). Generating takes
// Idempotency-Key and is for owners and supervisors; it is 409 SHIFT_PLAN_NOT_APPROVED
// without an approved revision and 409 SHIFT_SUMMARY_UNCHANGED when the computed digest
// equals the latest snapshot's. Any active member reads.
func shiftSummaryPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	site, plan := param("site", "path", true, id), param("plan", "path", true, id)
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/shift-plans/{plan}/summaries": map[string]any{
			"post": op("generateShiftSummary", []any{site, plan, key}, "ShiftSummaryRequestV1",
				merge(map[string]any{"201": json200("Snapshot revision appended over the approved revision and every bound station record, or replayed", "ShiftSummarySnapshotV1")}, writes)),
			"get": op("listShiftSummaries", []any{site, plan, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,18}$"})}, "",
				merge(map[string]any{"200": json200("Snapshot headers ordered by revision", "ShiftSummaryPageV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/{plan}/summaries/{revision}": map[string]any{
			"get": op("getShiftSummary", []any{site, plan, param("revision", "path", true, map[string]any{"type": "integer", "minimum": 1})}, "",
				merge(map[string]any{"200": json200("Snapshot, re-verified against its digest", "ShiftSummarySnapshotV1")}, reads)),
		},
	}
}

// shiftHandoverPaths documents the WIS-520 next-shift handover routes (rule 07). Generating
// and adopting take Idempotency-Key and are for owners and supervisors; any active member
// reads. Generating writes no shift plan; it is 422 INVALID_SHIFT_HANDOVER_REQUEST for an
// unknown summary revision or a next shift that starts before the summarized shift ends, and
// 409 SHIFT_HANDOVER_UNCHANGED when the computed digest equals the latest draft's. Adopting
// binds the draft's digest (412 REVISION_CONFLICT on mismatch) and submits its proposed plan
// as an unapproved draft revision by the adopter: without If-Match a new plan (409
// SHIFT_PLAN_EXISTS when the line already has an overlapping plan), with If-Match a revision
// of the plan for exactly that line and shift. It is 409 SHIFT_HANDOVER_ALREADY_ADOPTED,
// HANDOVER_SUPERSEDED (not the plan's latest draft), SHIFT_HANDOVER_NO_PROPOSAL,
// JOB_ORDER_NOT_CURRENT or INSTRUCTION_UNAVAILABLE (path names the job order) when it
// cannot proceed.
func shiftHandoverPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	site, plan := param("site", "path", true, id), param("plan", "path", true, id)
	revision := param("revision", "path", true, map[string]any{"type": "integer", "minimum": 1})
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "415", "422", "503")
	op := func(opID string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/shift-plans/{plan}/handovers": map[string]any{
			"post": op("generateShiftHandover", []any{site, plan, key}, "ShiftHandoverRequestV1",
				merge(map[string]any{"201": json200("Draft revision appended over the cited summary snapshot and current job orders, or replayed", "ShiftHandoverSnapshotV1")}, writes)),
			"get": op("listShiftHandovers", []any{site, plan, param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100}), param("cursor", "query", false, map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,18}$"})}, "",
				merge(map[string]any{"200": json200("Draft headers ordered by revision", "ShiftHandoverPageV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/{plan}/handovers/{revision}": map[string]any{
			"get": op("getShiftHandover", []any{site, plan, revision}, "",
				merge(map[string]any{"200": json200("Draft, re-verified against its digest", "ShiftHandoverSnapshotV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/{plan}/handovers/{revision}/adopt": map[string]any{
			"post": op("adoptShiftHandover", []any{site, plan, revision, key, param("If-Match", "header", false, map[string]any{"type": "string"})}, "ShiftHandoverAdoptInputV1",
				merge(map[string]any{"201": json200("Unapproved draft shift plan revision submitted by the adopter, or replayed", "ShiftPlanV1")}, merge(writes, problemDetails("412")))),
		},
	}
}

// shiftRunPaths documents shift run start, end and read. Start and end take Idempotency-Key
// and If-Match on the plan's state_revision; start or end does not change it.
func shiftRunPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "minLength": 3, "maxLength": 128})
	site, plan := param("site", "path", true, id), param("plan", "path", true, id)
	session := []any{map[string]any{"session": []any{}}}
	reads := problemDetails("401", "404", "422", "503")
	writes := problemDetails("401", "403", "404", "409", "412", "415", "422", "428", "503")
	op := func(opID, description string, params []any, body string, responses map[string]any) map[string]any {
		o := map[string]any{"operationId": opID, "description": description, "security": session, "parameters": params, "responses": responses}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	return map[string]any{
		"/v1/sites/{site}/shift-plans/{plan}/run": map[string]any{
			"post": op("startShiftRun", "Owner or supervisor with a recent authentication. plan_revision must be the approved revision. Allowed from 30 minutes before the planned start until the planned end. 409 codes: SHIFT_RUN_ALREADY_STARTED, SHIFT_RUN_ENDED, SHIFT_PLAN_NOT_APPROVED, SHIFT_START_TOO_EARLY, SHIFT_WINDOW_ENDED, IDEMPOTENCY_CONFLICT.",
				[]any{site, plan, key, ifMatch}, "ShiftRunStartInputV1", merge(map[string]any{"201": json200("Run started, or replayed", "ShiftRunV1")}, writes)),
			"get": op("getShiftRun", "Any active member. state is planned when the run has not started.", []any{site, plan}, "",
				merge(map[string]any{"200": json200("Run state of the plan", "ShiftRunV1")}, reads)),
		},
		"/v1/sites/{site}/shift-plans/{plan}/run/end": map[string]any{
			"post": op("endShiftRun", "Owner or supervisor with a recent authentication, including the one who started it. Writes the final summary snapshot and a next-shift handover draft in the same transaction. 409 codes: SHIFT_NOT_STARTED, SHIFT_RUN_ENDED, IDEMPOTENCY_CONFLICT.",
				[]any{site, plan, key, ifMatch}, "ShiftRunEndInputV1", merge(map[string]any{"200": json200("Run ended with its summary and handover draft, or replayed", "ShiftRunV1")}, writes)),
		},
	}
}
