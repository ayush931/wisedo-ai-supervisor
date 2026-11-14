package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

func masterDataCatalog() []named {
	return []named{
		{"LineBodyV1", reflect.TypeOf(workstation.LineBodyV1{})},
		{"LineV1", reflect.TypeOf(workstation.LineV1{})},
		{"LinePageV1", reflect.TypeOf(workstation.LinePageV1{})},
		{"ItemBodyV1", reflect.TypeOf(workstation.ItemBodyV1{})},
		{"ItemV1", reflect.TypeOf(workstation.ItemV1{})},
		{"ItemPageV1", reflect.TypeOf(workstation.ItemPageV1{})},
		{"RoutingStepV1", reflect.TypeOf(workstation.RoutingStepV1{})},
		{"RoutingBodyV1", reflect.TypeOf(workstation.RoutingBodyV1{})},
		{"RoutingV1", reflect.TypeOf(workstation.RoutingV1{})},
		{"RoutingPageV1", reflect.TypeOf(workstation.RoutingPageV1{})},
		{"ShiftCalendarShiftV1", reflect.TypeOf(workstation.ShiftCalendarShiftV1{})},
		{"ShiftCalendarBodyV1", reflect.TypeOf(workstation.ShiftCalendarBodyV1{})},
		{"ShiftCalendarV1", reflect.TypeOf(workstation.ShiftCalendarV1{})},
		{"ShiftCalendarPageV1", reflect.TypeOf(workstation.ShiftCalendarPageV1{})},
		{"SiteMasterDataSettingsBodyV1", reflect.TypeOf(workstation.SiteMasterDataSettingsBodyV1{})},
		{"SiteMasterDataSettingsV1", reflect.TypeOf(workstation.SiteMasterDataSettingsV1{})},
	}
}

func addMasterData(doc map[string]any) {
	merge(doc["paths"].(map[string]any), masterDataPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " Site master data (WIS-520) provides revisioned lines, items, routings, shift calendars and site line-validation settings. All active site members can read; owner and supervisor roles can append revisions. Mutations require Idempotency-Key, quoted If-Match, X-CSRF-Token and same-origin Origin."
}

func masterDataPaths() map[string]any {
	param := func(name, in string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": in, "required": required, "schema": schema}
	}
	id := map[string]any{"type": "string", "format": "uuid"}
	site := param("site", "path", true, id)
	objectID := param("id", "path", true, id)
	key := param("Idempotency-Key", "header", true, map[string]any{"type": "string", "minLength": 1, "maxLength": 128})
	ifMatch := param("If-Match", "header", true, map[string]any{"type": "string", "pattern": `^"(0|[1-9][0-9]*)"$`})
	csrf := param("X-CSRF-Token", "header", true, map[string]any{"type": "string", "minLength": 1})
	origin := param("Origin", "header", true, map[string]any{"type": "string", "format": "uri"})
	limit := param("limit", "query", false, map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50})
	cursor := param("cursor", "query", false, id)
	session := []any{map[string]any{"session": []any{}}}
	etag := map[string]any{"ETag": map[string]any{"description": `Quoted current revision; send it as If-Match.`, "schema": map[string]any{"type": "string"}}}
	readErrors := problemDetails("401", "403", "404", "422", "503")
	writeErrors := problemDetails("401", "403", "404", "409", "412", "413", "415", "422", "428", "503")
	writeErrors["409"] = map[string]any{"description": "IDEMPOTENCY_CONFLICT, LINE_VALIDATION_BLOCKED, MASTER_DATA_IN_USE or MASTER_DATA_UNCHANGED", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	writeErrors["422"] = map[string]any{"description": "INVALID_MASTER_DATA or INVALID_REQUEST", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	writeErrors["403"] = map[string]any{"description": "CSRF_REJECTED or PERMISSION_DENIED", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
	get := func(op, schema string, params []any) map[string]any {
		response := json200("Read site-scoped master data. Active members, including viewers, may read.", schema)
		response["headers"] = etag
		return map[string]any{"operationId": op, "security": session, "parameters": params, "responses": merge(map[string]any{"200": response}, readErrors)}
	}
	post := func(op, body, schema string, params []any) map[string]any {
		response := json200("Created or revised immutable master-data body. Owner and supervisor roles may write.", schema)
		response["headers"] = etag
		return map[string]any{"operationId": op, "security": session, "parameters": append(params, key, ifMatch, csrf, origin), "requestBody": jsonBody(body), "responses": merge(map[string]any{"201": response}, writeErrors)}
	}
	paths := map[string]any{}
	for _, r := range []struct{ name, body, entity, page string }{
		{"lines", "LineBodyV1", "LineV1", "LinePageV1"},
		{"items", "ItemBodyV1", "ItemV1", "ItemPageV1"},
		{"routings", "RoutingBodyV1", "RoutingV1", "RoutingPageV1"},
		{"shift-calendars", "ShiftCalendarBodyV1", "ShiftCalendarV1", "ShiftCalendarPageV1"},
	} {
		base := "/v1/sites/{site}/" + r.name
		paths[base] = map[string]any{
			"get":  get("list"+camel(r.name), r.page, []any{site, limit, cursor}),
			"post": post("create"+camelSingular(r.name), r.body, r.entity, []any{site}),
		}
		paths[base+"/{id}"] = map[string]any{"get": get("get"+camelSingular(r.name), r.entity, []any{site, objectID})}
		paths[base+"/{id}/revisions"] = map[string]any{
			"get":  get("list"+camelSingular(r.name)+"Revisions", r.entity, []any{site, objectID}),
			"post": post("revise"+camelSingular(r.name), r.body, r.entity, []any{site, objectID}),
		}
	}
	settings := "/v1/sites/{site}/master-data-settings"
	paths[settings] = map[string]any{"get": get("getMasterDataSettings", "SiteMasterDataSettingsV1", []any{site})}
	paths[settings+"/revisions"] = map[string]any{"post": post("reviseMasterDataSettings", "SiteMasterDataSettingsBodyV1", "SiteMasterDataSettingsV1", []any{site})}
	return paths
}

func camel(v string) string {
	out := ""
	upper := true
	for _, r := range v {
		if r == '-' {
			upper = true
			continue
		}
		if upper && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		out += string(r)
		upper = false
	}
	return out
}

func camelSingular(v string) string {
	if v == "lines" {
		return "Line"
	}
	if v == "items" {
		return "Item"
	}
	if v == "routings" {
		return "Routing"
	}
	if v == "shift-calendars" {
		return "ShiftCalendar"
	}
	return camel(v)
}
