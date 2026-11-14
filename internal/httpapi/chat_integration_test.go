//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestOperationsChatHTTPPostgres covers WIS-517 slice 1 against real PostgreSQL through the
// public routes: author-only conversations, idempotent posting by key and by client request
// ID, If-Match, role and tenant isolation, ordered paging, the explicit non-answer reasons and
// the database guards that keep human messages immutable and history retained.
func TestOperationsChatHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: real PostgreSQL admin and runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	database := "chat_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	connect := func(raw string) *sql.DB {
		t.Helper()
		u, e := url.Parse(raw)
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + database
		db, e := sql.Open("pgx", u.String())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	control, runtime := connect(adminURL), connect(runtimeURL)
	if err = migrate.Apply(ctx, control); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(runtimeURLFor(runtimeURL, database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := control.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}

	tenant, site, otherTenant, otherSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, lead, colleague, operator, viewer, outsider := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	issuer, origin := "https://chat-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58179"
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic chat test'),($2,'other tenant')`, tenant, otherTenant)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'chat site'),($3,$4,'other site')`, tenant, site, otherTenant, otherSite)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'supervisor'),($1,$2,$3,$6,'supervisor'),($1,$2,$3,$7,'operator'),($1,$2,$3,$8,'viewer')`, tenant, site, issuer, owner, lead, colleague, operator, viewer)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, otherTenant, otherSite, issuer, outsider)
	type session struct{ token, csrf string }
	seed := func(subject string) session {
		t.Helper()
		v := session{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(v.token))
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), subject, issuer, v.csrf)
		return v
	}
	ownerS, leadS, colleagueS, operatorS, viewerS, outsiderS := seed(owner), seed(lead), seed(colleague), seed(operator), seed(viewer), seed(outsider)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "chat-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	disabled := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	configured := New(config.Config{Environment: "local", WebDir: t.TempDir(), Agent: config.Agent{Provider: config.AgentProviderFixture, ModeEnabled: true}}, db, auth)
	call := func(h http.Handler, actor session, method, path, key, revision, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", actor.csrf)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if revision != "" {
			r.Header.Set("If-Match", revision)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	checkResponse := func(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
		if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("problem code want %s: %s", code, w.Body.String())
		}
	}
	expect := func(w *httptest.ResponseRecorder, status int, code string) {
		checkResponse(t, w, status, code)
	}
	notDisclosing404 := func(t *testing.T, w *httptest.ResponseRecorder, secrets ...string) {
		t.Helper()
		checkResponse(t, w, http.StatusNotFound, "NOT_FOUND")
		for _, secret := range secrets {
			if secret != "" && strings.Contains(w.Body.String(), secret) {
				t.Fatalf("404 disclosed %q: %s", secret, w.Body.String())
			}
		}
	}
	decode := func(w *httptest.ResponseRecorder, v any) {
		t.Helper()
		if e := json.Unmarshal(w.Body.Bytes(), v); e != nil {
			t.Fatalf("%v: %s", e, w.Body.String())
		}
	}
	base := "/v1/sites/" + site + "/chat-conversations"
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := control.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	// Create: the lead's conversation, idempotent by key; a different body under the key is 409.
	key := uuid.NewString()
	w := call(disabled, leadS, "POST", base, key, "", `{"title":"Line 2 shortfall"}`)
	expect(w, 201, "")
	var conv chat.Conversation
	decode(w, &conv)
	if conv.Revision != 1 || conv.Title != "Line 2 shortfall" || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("created: %+v etag %s", conv, w.Header().Get("ETag"))
	}
	var replay chat.Conversation
	w = call(disabled, leadS, "POST", base, key, "", `{"title":"Line 2 shortfall"}`)
	expect(w, 201, "")
	decode(w, &replay)
	if replay.ConversationID != conv.ConversationID || count(`SELECT count(*) FROM supervisor.chat_conversations`) != 1 {
		t.Fatalf("create replay made a second conversation: %+v", replay)
	}
	expect(call(disabled, leadS, "POST", base, key, "", `{"title":"Another"}`), 409, "IDEMPOTENCY_CONFLICT")
	expect(call(disabled, leadS, "POST", base, uuid.NewString(), "", `{"title":"x","tenant_id":"`+tenant+`"}`), 422, "INVALID_REQUEST")

	// WIS-703: operators may chat; viewers still cannot.
	t.Run("WIS703_operator_can_create_initial", func(t *testing.T) {
		checkResponse(t, call(disabled, operatorS, "POST", base, uuid.NewString(), "", `{"title":"Mine"}`), http.StatusCreated, "")
	})
	t.Run("viewer_role_cannot_list_initial", func(t *testing.T) {
		checkResponse(t, call(disabled, viewerS, "GET", base, "", "", ""), http.StatusForbidden, "PERMISSION_DENIED")
	})
	expect(call(disabled, ownerS, "POST", base, uuid.NewString(), "", `{"title":"Owner thread"}`), 201, "")

	// Thread access is author-only; site ownership does not bypass the non-disclosing 404.
	path := base + "/" + conv.ConversationID
	expect(call(disabled, leadS, "GET", path, "", "", ""), 200, "")
	t.Run("colleague_cannot_get_conversation", func(t *testing.T) {
		notDisclosing404(t, call(disabled, colleagueS, "GET", path, "", "", ""), conv.ConversationID, conv.Title)
	})
	t.Run("colleague_cannot_get_messages", func(t *testing.T) {
		notDisclosing404(t, call(disabled, colleagueS, "GET", path+"/messages", "", "", ""), conv.ConversationID, conv.Title)
	})
	t.Run("owner_get_conversation", func(t *testing.T) {
		notDisclosing404(t, call(disabled, ownerS, "GET", path, "", "", ""), conv.ConversationID, conv.Title)
	})
	t.Run("owner_get_messages", func(t *testing.T) {
		notDisclosing404(t, call(disabled, ownerS, "GET", path+"/messages", "", "", ""), conv.ConversationID, conv.Title)
	})
	expect(call(disabled, outsiderS, "GET", path, "", "", ""), 404, "NOT_FOUND")
	expect(call(disabled, outsiderS, "GET", "/v1/sites/"+otherSite+"/chat-conversations/"+conv.ConversationID, "", "", ""), 404, "NOT_FOUND")
	var page chat.ConversationPage
	w = call(disabled, colleagueS, "GET", base, "", "", "")
	expect(w, 200, "")
	decode(w, &page)
	if len(page.Conversations) != 0 {
		t.Fatalf("colleague sees another member's conversations: %+v", page)
	}

	// Post: the question and an explicit non-answer reply in one transaction; no model is
	// configured, so the reason is ASSISTANT_DISABLED and no body is invented.
	request := uuid.NewString()
	question := `{"request_id":"` + request + `","text":"What did line 2 deliver last shift?"}`
	postKey := uuid.NewString()
	w = call(disabled, leadS, "POST", path+"/messages", postKey, `"1"`, question)
	expect(w, 201, "")
	storedPost := append([]byte(nil), w.Body.Bytes()...)
	var posted chat.PostResult
	decode(w, &posted)
	if posted.Conversation.Revision != 2 || posted.Question.State != "received" || posted.Question.Seq != 1 || posted.Reply.Seq != 2 ||
		posted.Reply.State != chat.StateUnavailable || posted.Reply.ReasonCode == nil || *posted.Reply.ReasonCode != chat.ReasonDisabled ||
		posted.Reply.Body != nil || posted.Reply.ReplyTo == nil || *posted.Reply.ReplyTo != posted.Question.MessageID || posted.Replayed {
		t.Fatalf("posted: %+v", posted)
	}
	// A replayed create must expose the current revision for the next mutation.
	w = call(disabled, leadS, "POST", base, key, "", `{"title":"Line 2 shortfall"}`)
	expect(w, 201, "")
	decode(w, &replay)
	if replay.ConversationID != conv.ConversationID || replay.Revision != posted.Conversation.Revision || w.Header().Get("ETag") != `"2"` {
		t.Fatalf("current create replay: %+v etag %s", replay, w.Header().Get("ETag"))
	}
	// Same key replays (200); a lost response retried with a new key but the same request ID
	// also replays; the same request ID with different text is 409. No rows are added.
	w = call(disabled, leadS, "POST", path+"/messages", postKey, `"1"`, question)
	expect(w, 200, "")
	w = call(disabled, leadS, "POST", path+"/messages", uuid.NewString(), `"1"`, question)
	expect(w, 200, "")
	var retried chat.PostResult
	decode(w, &retried)
	if !retried.Replayed || retried.Question.MessageID != posted.Question.MessageID || retried.Reply.MessageID != posted.Reply.MessageID {
		t.Fatalf("retry by request ID: %+v", retried)
	}
	expect(call(disabled, leadS, "POST", path+"/messages", uuid.NewString(), `"2"`, `{"request_id":"`+request+`","text":"Something else"}`), 409, "IDEMPOTENCY_CONFLICT")
	if n := count(`SELECT count(*) FROM supervisor.chat_messages WHERE conversation_id=$1`, conv.ConversationID); n != 2 {
		t.Fatalf("retries changed the message count to %d", n)
	}
	// A stale revision is 412; a missing If-Match is 428; a colleague cannot post into it.
	expect(call(disabled, leadS, "POST", path+"/messages", uuid.NewString(), `"1"`, `{"request_id":"`+uuid.NewString()+`","text":"Second question"}`), 412, "REVISION_CONFLICT")
	expect(call(disabled, leadS, "POST", path+"/messages", uuid.NewString(), "", `{"request_id":"`+uuid.NewString()+`","text":"No precondition"}`), 428, "REVISION_REQUIRED")
	t.Run("colleague_cannot_post_to_thread", func(t *testing.T) {
		body := `{"request_id":"` + uuid.NewString() + `","text":"Hijack"}`
		notDisclosing404(t, call(disabled, colleagueS, "POST", path+"/messages", uuid.NewString(), `"2"`, body), conv.ConversationID, conv.Title, "Hijack")
	})

	// With a model configured but chat answering not yet qualified, the reply says so.
	w = call(configured, leadS, "POST", path+"/messages", uuid.NewString(), `"2"`, `{"request_id":"`+uuid.NewString()+`","text":"Compare plan versus actual"}`)
	expect(w, 201, "")
	decode(w, &posted)
	if posted.Reply.ReasonCode == nil || *posted.Reply.ReasonCode != chat.ReasonNotQualified || posted.Reply.Body != nil || posted.Conversation.Revision != 3 {
		t.Fatalf("configured reply: %+v", posted.Reply)
	}
	t.Run("owner_post_to_thread", func(t *testing.T) {
		body := `{"request_id":"` + uuid.NewString() + `","text":"Owner must not post"}`
		notDisclosing404(t, call(disabled, ownerS, "POST", path+"/messages", uuid.NewString(), w.Header().Get("ETag"), body), conv.ConversationID, conv.Title, "Owner must not post")
	})

	// Ordered paging by sequence with a stable cursor.
	var messages chat.MessagePage
	w = call(disabled, leadS, "GET", path+"/messages?limit=3", "", "", "")
	expect(w, 200, "")
	decode(w, &messages)
	if len(messages.Messages) != 3 || messages.Messages[0].Seq != 1 || messages.Messages[2].Seq != 3 || messages.NextCursor != "3" {
		t.Fatalf("first page: %+v", messages)
	}
	w = call(disabled, leadS, "GET", path+"/messages?limit=3&cursor="+messages.NextCursor, "", "", "")
	expect(w, 200, "")
	var second chat.MessagePage
	decode(w, &second)
	if len(second.Messages) != 1 || second.Messages[0].Seq != 4 || second.NextCursor != "" || second.Messages[0].Author != nil || second.Messages[0].Body != nil {
		t.Fatalf("second page: %+v", second)
	}
	expect(call(disabled, leadS, "GET", path+"/messages?cursor=abc", "", "", ""), 422, "INVALID_REQUEST")

	// Membership changes take effect on the next protected request, including an idempotent replay.
	exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, lead)
	t.Run("inactive_member_get_conversation", func(t *testing.T) {
		notDisclosing404(t, call(disabled, leadS, "GET", path, "", "", ""), conv.ConversationID, conv.Title)
	})
	t.Run("inactive_member_get_messages", func(t *testing.T) {
		notDisclosing404(t, call(disabled, leadS, "GET", path+"/messages", "", "", ""), conv.ConversationID, conv.Title,
			"What did line 2 deliver last shift?", retried.Question.MessageID, retried.Reply.MessageID)
	})
	t.Run("inactive_member_post_replay", func(t *testing.T) {
		w := call(disabled, leadS, "POST", path+"/messages", postKey, `"1"`, question)
		notDisclosing404(t, w, conv.ConversationID, conv.Title, "What did line 2 deliver last shift?", retried.Question.MessageID, retried.Reply.MessageID)
		if bytes.Equal(w.Body.Bytes(), storedPost) {
			t.Fatal("revoked member received the stored post response")
		}
	})
	exec(`UPDATE supervisor.memberships SET active=true WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, lead)
	exec(`UPDATE supervisor.memberships SET role='viewer' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, lead)
	t.Run("demoted_viewer_cannot_create_conversation", func(t *testing.T) {
		checkResponse(t, call(disabled, leadS, "POST", base, uuid.NewString(), "", `{"title":"Forbidden viewer thread"}`), http.StatusForbidden, "PERMISSION_DENIED")
	})
	t.Run("demoted_viewer_cannot_post_to_thread", func(t *testing.T) {
		body := `{"request_id":"` + uuid.NewString() + `","text":"Forbidden viewer post"}`
		checkResponse(t, call(disabled, leadS, "POST", path+"/messages", uuid.NewString(), `"3"`, body), http.StatusForbidden, "PERMISSION_DENIED")
	})
	exec(`UPDATE supervisor.memberships SET role='supervisor' WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4`, tenant, site, issuer, lead)

	// Database guards, as the runtime role: a human message cannot be edited, nothing is deleted,
	// and a terminal reply cannot be reopened.
	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		t.Fatal(err)
	}
	sqlState := func(q string, args ...any) string {
		t.Helper()
		if _, e := tx.ExecContext(ctx, "SAVEPOINT g"); e != nil {
			t.Fatal(e)
		}
		_, e := tx.ExecContext(ctx, q, args...)
		if _, re := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT g"); re != nil {
			t.Fatal(re)
		}
		var pgErr *pgconn.PgError
		if e == nil {
			return ""
		}
		if errors.As(e, &pgErr) {
			return pgErr.Code
		}
		return e.Error()
	}
	firstQuestion, firstReply := retried.Question.MessageID, retried.Reply.MessageID
	if got := sqlState(`UPDATE supervisor.chat_messages SET body='edited',revision=revision+1 WHERE message_id=$1`, firstQuestion); got != "23514" {
		t.Fatalf("editing a human message: %q, want 23514", got)
	}
	if got := sqlState(`UPDATE supervisor.chat_messages SET state='answered',body='invented',reason_code=NULL,revision=revision+1 WHERE message_id=$1`, firstReply); got != "23514" {
		t.Fatalf("reopening a terminal reply: %q, want 23514", got)
	}
	if got := sqlState(`DELETE FROM supervisor.chat_messages WHERE message_id=$1`, firstQuestion); got != "42501" {
		t.Fatalf("deleting a message: %q, want 42501", got)
	}
	if got := sqlState(`DELETE FROM supervisor.chat_conversations WHERE conversation_id=$1`, conv.ConversationID); got != "42501" {
		t.Fatalf("deleting a conversation: %q, want 42501", got)
	}
	if got := sqlState(`UPDATE supervisor.chat_conversations SET title='renamed',revision=revision+1 WHERE conversation_id=$1`, conv.ConversationID); got != "42501" {
		t.Fatalf("runtime role renaming a conversation: %q, want 42501 (no column grant)", got)
	}
}
