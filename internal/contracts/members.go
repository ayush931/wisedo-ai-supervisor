package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/membership"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
)

// memberCatalog publishes the WIS-290 session and member directory contracts. Display names
// come only from the verified OIDC ID token at login; no route accepts one.
func memberCatalog() []named {
	return []named{
		{"MeV1", reflect.TypeOf(store.Me{})},
		{"SiteMemberPageV1", reflect.TypeOf(membership.DirectoryPage{})},
	}
}

func memberPaths() map[string]any {
	session := []any{map[string]any{"session": []any{}}}
	site := map[string]any{"name": "site", "in": "path", "required": true, "schema": map[string]any{"type": "string", "format": "uuid"}}
	limit := map[string]any{"name": "limit", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}
	cursor := map[string]any{"name": "cursor", "in": "query", "required": false, "schema": map[string]any{"type": "string", "maxLength": 4096}}
	return map[string]any{
		"/v1/me": map[string]any{
			"get": map[string]any{"operationId": "getMe", "security": session,
				"responses": merge(map[string]any{"200": json200("Verified subject, IdP display name (null when unknown), CSRF secret and active sites with agent availability. Agent availability is deployment-wide and reported per site until a site agent policy exists; an unavailable site includes AGENT_NOT_CONFIGURED or AGENT_MODE_DISABLED; provider and model settings are never returned.", "MeV1")}, problemDetails("401", "503"))},
		},
		"/v1/sites/{site}/members": map[string]any{
			"get": map[string]any{"operationId": "listSiteMembers", "security": session, "parameters": []any{site, limit, cursor},
				"responses": merge(map[string]any{"200": json200("Active members of the site with display names, mapped by subject (all members of a site share one issuer, so it is not returned); any active member reads, others get 404", "SiteMemberPageV1")}, problemDetails("401", "404", "422", "503"))},
		},
	}
}
