package contracts

import (
	"reflect"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

func chatCatalog() []named {
	return []named{
		{"ChatConversationCreateInputV1", reflect.TypeOf(chat.CreateInput{})},
		{"ChatConversationV1", reflect.TypeOf(chat.Conversation{})},
		{"ChatConversationPageV1", reflect.TypeOf(chat.ConversationPage{})},
		{"ChatMessageInputV1", reflect.TypeOf(chat.PostInput{})},
		{"ChatMessageV1", reflect.TypeOf(chat.Message{})},
		{"ChatMessagePageV1", reflect.TypeOf(chat.MessagePage{})},
		{"ChatPostResultV1", reflect.TypeOf(chat.PostResult{})},
	}
}

func restrictChat(name string, schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	switch name {
	case "ChatMessageV1":
		props["author_kind"].(map[string]any)["enum"] = []any{chat.AuthorAssistant, chat.AuthorHuman}
		props["state"].(map[string]any)["enum"] = []any{"answered", "canceled", "failed", "needs_input", "pending", "received", "unavailable"}
		restrictChatReason(props)
		props["citations"].(map[string]any)["maxItems"] = 50
	case "ChatMessagePageV1":
		props["next_cursor"].(map[string]any)["pattern"] = `^[1-9][0-9]*$`
		restrictChatReason(props["messages"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any))
	case "ChatPostResultV1":
		for _, field := range []string{"question", "reply"} {
			restrictChatReason(props[field].(map[string]any)["properties"].(map[string]any))
		}
	}
}

// restrictChatReason restricts a chat message's reason_code and retrieval label to their values.
func restrictChatReason(props map[string]any) {
	// WIS-700: how the passages were chosen is one of the four labels the server records.
	props["retrieval"].(map[string]any)["enum"] = []any{chat.RetrievalHybrid, chat.RetrievalHybridUnreranked, chat.RetrievalWord, chat.RetrievalWordFallback}
	props["reason_code"].(map[string]any)["enum"] = []any{
		agent.CodeAnswerInvalid, chat.ReasonDisabled, chat.ReasonNotQualified, agent.CodeBudgetExhausted, chat.ReasonCaptureLimit,
		agent.CodeChatActivity, agent.CodeCitationNotInScope, agent.CodeDeadlineExceeded, agent.CodeEvidenceRevoked, agent.CodeEvidenceUnavailable,
		agent.CodeFactNotApproved, agent.CodeMembershipRevoked, agent.CodeMissingInput, recovery.ReasonModeDisabled,
		chat.ReasonNoApprovedProcess, chat.ReasonAnswersBusy, chat.ReasonNoReadableEvidence, agent.CodeNumberNotSourced, agent.CodeQuantityNotApproved, agent.CodeRunCanceled,
		agent.CodeSchemaInvalid, agent.CodeScopeDenied, chat.ReasonSnapshotEvidenceUnavailable, agent.CodeSpecialistFailed,
		agent.CodeStaleScope, agent.CodeStatementUnsafe, agent.CodeToolArgumentRejected, agent.CodeToolNotAllowed,
		agent.CodeToolRefNotInScope,
	}
}

func chatPaths() map[string]any {
	id := map[string]any{"type": "string", "format": "uuid"}
	path := func(name string) map[string]any {
		return map[string]any{"name": name, "in": "path", "required": true, "schema": id}
	}
	header := func(name string, max int) map[string]any {
		return map[string]any{"name": name, "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": max}}
	}
	key, csrf := header("Idempotency-Key", 128), header("X-CSRF-Token", 512)
	ifMatch := header("If-Match", 32)
	ifMatch["schema"].(map[string]any)["pattern"] = `^"[1-9][0-9]*"$`
	limit := map[string]any{"name": "limit", "in": "query", "required": false, "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}}
	cursor := map[string]any{"name": "cursor", "in": "query", "required": false, "schema": id}
	seqCursor := map[string]any{"name": "cursor", "in": "query", "required": false, "schema": map[string]any{"type": "string", "pattern": `^[1-9][0-9]*$`}}
	session := []any{map[string]any{"session": []any{}}}
	ok := func(status, desc, name string) map[string]any {
		return map[string]any{status: json200(desc, name)}
	}
	problemSet := func(codes ...string) map[string]any {
		out := map[string]any{}
		for _, code := range codes {
			out[code] = map[string]any{"description": "Error", "content": map[string]any{"application/problem+json": map[string]any{"schema": ref("ProblemDetailsV1")}}}
		}
		return out
	}
	operation := func(id string, params []any, body string, response map[string]any, errors ...string) map[string]any {
		o := map[string]any{"operationId": id, "security": session, "parameters": params, "responses": merge(response, problemSet(errors...))}
		if body != "" {
			o["requestBody"] = jsonBody(body)
		}
		return o
	}
	site := path("site")
	posted := merge(ok("201", "Question and its reply recorded", "ChatPostResultV1"), ok("200", "Idempotently replayed question and reply", "ChatPostResultV1"))
	return map[string]any{
		"/v1/sites/{site}/chat-conversations": map[string]any{
			"get":  operation("listChatConversations", []any{site, cursor, limit}, "", ok("200", "The caller's own conversations", "ChatConversationPageV1"), "401", "403", "404", "422", "503"),
			"post": operation("createChatConversation", []any{site, key, csrf}, "ChatConversationCreateInputV1", ok("201", "Created or idempotently replayed", "ChatConversationV1"), "401", "403", "404", "409", "413", "415", "422", "503"),
		},
		"/v1/sites/{site}/chat-conversations/{id}": map[string]any{
			"get": operation("getChatConversation", []any{site, path("id")}, "", ok("200", "The caller's conversation", "ChatConversationV1"), "401", "403", "404", "422", "503"),
		},
		"/v1/sites/{site}/chat-conversations/{id}/messages": map[string]any{
			"get":  operation("listChatMessages", []any{site, path("id"), seqCursor, limit}, "", ok("200", "Messages in order", "ChatMessagePageV1"), "401", "403", "404", "422", "503"),
			"post": operation("postChatMessage", []any{site, path("id"), key, ifMatch, csrf}, "ChatMessageInputV1", posted, "401", "403", "404", "409", "412", "413", "415", "422", "428", "503"),
		},
	}
}

func addChat(doc map[string]any) {
	merge(doc["paths"].(map[string]any), chatPaths())
	info := doc["info"].(map[string]any)
	info["description"] = info["description"].(string) + " WIS-517 operations chat persists author-only, site-scoped conversations; a reply is data, never authority, and is an explicit non-answer until the answering loop is qualified."
}
