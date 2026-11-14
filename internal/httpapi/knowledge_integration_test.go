//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
)

// Response DTOs deliberately do not inherit the strict input unmarshallers.
// Server-generated state/provenance fields must remain absent from input contracts.
type candidateEntityFields knowledge.EntityInput
type candidateEntityResponse struct {
	candidateEntityFields
	State           string `json:"state"`
	Revision        int64  `json:"revision"`
	CurrentRevision int64  `json:"current_revision"`
	SourcesCurrent  bool   `json:"sources_current"`
}
type candidateEntityPage struct {
	Entities []candidateEntityResponse `json:"entities"`
}
type candidateClaimFields knowledge.ClaimInput
type candidateClaimResponse struct {
	candidateClaimFields
	State           string `json:"state"`
	Revision        int64  `json:"revision"`
	CurrentRevision int64  `json:"current_revision"`
	SourcesCurrent  bool   `json:"sources_current"`
	ActorSubject    string `json:"actor_subject"`
}

func TestKnowledgeHTTPCandidatesProvenanceAndScope(t *testing.T) {
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: actual disposable PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db, err := store.New(runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		t.Fatal(err)
	}
	tenant, site, otherSite, foreignTenant, foreignSite := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	owner, viewer := uuid.NewString(), uuid.NewString()
	issuer, origin := "https://knowledge-http.invalid/"+uuid.NewString(), "http://127.0.0.1:58176"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		tx, e := admin.BeginTx(context.Background(), nil)
		if e != nil {
			t.Error(e)
			return
		}
		defer tx.Rollback()
		if _, e = tx.Exec("SET CONSTRAINTS ALL DEFERRED"); e != nil {
			t.Error(e)
			return
		}
		for _, table := range []string{"knowledge_audit", "knowledge_receipts", "knowledge_conflicts", "knowledge_claim_spans", "knowledge_claim_entity_refs", "knowledge_claim_revisions", "knowledge_claim_heads", "knowledge_entity_revisions", "knowledge_entity_heads", "knowledge_entities", "evidence_audit", "evidence_receipts", "evidence_revocations", "evidence_spans", "evidence_revisions", "evidence_heads", "memberships", "sites", "tenants"} {
			var exists bool
			if e = tx.QueryRow("SELECT to_regclass($1) IS NOT NULL", "supervisor."+table).Scan(&exists); e != nil {
				t.Error(e)
				return
			}
			if !exists {
				continue
			}
			column := "tenant_id"
			if table == "tenants" {
				column = "id"
			}
			for _, scopedTenant := range []string{tenant, foreignTenant} {
				if _, e = tx.Exec("SELECT set_config('supervisor.tenant_id',$1,true)", scopedTenant); e != nil {
					t.Error(e)
					return
				}
				if _, e = tx.Exec("DELETE FROM supervisor."+table+" WHERE "+column+"=$1", scopedTenant); e != nil {
					t.Error(e)
					return
				}
			}
		}
		if _, e = tx.Exec("DELETE FROM supervisor.browser_sessions WHERE issuer=$1", issuer); e != nil {
			t.Error(e)
			return
		}
		if e = tx.Commit(); e != nil {
			t.Error(e)
		}
	}()
	exec("INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic candidate HTTP'),($2,'foreign candidate')", tenant, foreignTenant)
	exec("INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'candidate site'),($1,$3,'other candidate site'),($4,$5,'foreign')", tenant, site, otherSite, foreignTenant, foreignSite)
	exec("INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner'),($1,$2,$3,$5,'reviewer'),($1,$6,$3,$4,'owner')", tenant, site, issuer, owner, viewer, otherSite)
	type actorSession struct{ token, csrf, hash string }
	seed := func(subject string) actorSession {
		t.Helper()
		a := actorSession{token: uuid.NewString(), csrf: uuid.NewString()}
		sum := sha256.Sum256([]byte(a.token))
		a.hash = hex.EncodeToString(sum[:])
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, a.hash, subject, issuer, a.csrf)
		return a
	}
	writer, reader := seed(owner), seed(viewer)
	auth, err := identity.New(ctx, identity.Config{Issuer: issuer, ClientID: "knowledge-test", ClientSecret: "synthetic-only", RedirectURI: origin + "/auth/callback"}, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{Environment: "local", WebDir: t.TempDir()}, db, auth)
	call := func(actor actorSession, method, path, key, revision, csrf, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if actor.token != "" {
			r.AddCookie(&http.Cookie{Name: "supervisor", Value: actor.token})
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("If-Match", revision)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		conformsToPublished(t, r, w)
		return w
	}
	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	encode := func(value any) string {
		t.Helper()
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	base := "/v1/sites/" + site
	// This is the initial RED when candidate routes are not yet registered.
	status(call(writer, "GET", base+"/entities", "", "", "", ""), 200)
	sourceID := uuid.NewString()
	source := evidence.Input{SourceID: sourceID, Kind: "interview", Title: "Machine capacity account", Text: "Machine A runs at 125.5 units/hour."}
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(source)), 200)
	spanResponse := call(writer, "POST", base+"/sources/"+sourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":9}`)
	status(spanResponse, 200)
	var span evidence.Span
	if err = json.Unmarshal(spanResponse.Body.Bytes(), &span); err != nil {
		t.Fatal(err)
	}
	entity := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: "Machine A", SourceID: sourceID, SourceRevision: 1}
	entityKey, entityBody := uuid.NewString(), encode(entity)
	status(call(actorSession{}, "POST", base+"/entities", entityKey, `"0"`, writer.csrf, entityBody), 401)
	status(call(writer, "POST", base+"/entities", entityKey, `"0"`, "", entityBody), 403)
	wantProblem(t, call(writer, "POST", base+"/entities", entityKey, "", writer.csrf, entityBody), 428, "REVISION_REQUIRED")
	status(call(writer, "POST", base+"/entities", entityKey, `W/"0"`, writer.csrf, entityBody), 422)
	status(call(reader, "POST", base+"/entities", entityKey, `"0"`, reader.csrf, entityBody), 403)
	for _, extra := range []string{`,"state":"approved"}`, `,"authority":"operational"}`, `,"tenant_id":"forged"}`} {
		status(call(writer, "POST", base+"/entities", entityKey, `"0"`, writer.csrf, strings.TrimSuffix(entityBody, "}")+extra), 422)
	}
	createdEntity := call(writer, "POST", base+"/entities", entityKey, `"0"`, writer.csrf, entityBody)
	status(createdEntity, 200)
	if createdEntity.Header().Get("ETag") != `"1"` {
		t.Fatal("entity ETag is not initial version1")
	}
	var entityResult candidateEntityResponse
	if err = json.Unmarshal(createdEntity.Body.Bytes(), &entityResult); err != nil {
		t.Fatal(err)
	}
	if entityResult.EntityID != entity.EntityID || entityResult.State != "candidate" || !entityResult.SourcesCurrent || entityResult.Revision != 1 || entityResult.CurrentRevision != 1 {
		t.Fatalf("entity authority %+v", entityResult)
	}
	status(call(writer, "POST", base+"/entities", entityKey, `"0"`, writer.csrf, entityBody), 200)
	renamed := entity
	renamed.Name = "Forbidden replacement"
	wantProblem(t, call(writer, "POST", base+"/entities", entityKey, `"0"`, writer.csrf, encode(renamed)), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(renamed)), 412, "REVISION_CONFLICT")
	wantProblem(t, call(writer, "POST", base+"/entities", uuid.NewString(), `"2"`, writer.csrf, entityBody), 412, "REVISION_CONFLICT")
	listed := call(reader, "GET", base+"/entities?limit=1", "", "", "", "")
	status(listed, 200)
	var entities candidateEntityPage
	if err = json.Unmarshal(listed.Body.Bytes(), &entities); err != nil {
		t.Fatal(err)
	}
	if len(entities.Entities) != 1 || entities.Entities[0].Name != "Machine A" {
		t.Fatal("immutable entity changed")
	}
	status(call(writer, "GET", base+"/entities?limit=101", "", "", "", ""), 422)
	status(call(writer, "GET", base+"/entities?cursor=not-a-uuid", "", "", "", ""), 422)

	claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "capacity_per_hour", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"125.5"`), Unit: "units/hour"}, EffectiveFrom: "2026-09-27T10:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
	claimKey, claimBody := uuid.NewString(), encode(claim)
	claimPath := base + "/claims/" + claim.ClaimID
	missingPin := claim
	missingPin.SubjectRevision = 0
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(missingPin)), 422)
	negativePin := claim
	negativePin.SubjectRevision = -1
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(negativePin)), 422)
	unwantedObjectPin := claim
	unwantedObjectPin.ObjectRevision = 1
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(unwantedObjectPin)), 422)

	status(call(reader, "POST", base+"/claims", claimKey, `"0"`, reader.csrf, claimBody), 403)
	for _, extra := range []string{`,"state":"approved"}`, `,"authority":"verified"}`} {
		status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, strings.TrimSuffix(claimBody, "}")+extra), 422)
	}
	machineGenerated := claim
	machineGenerated.Extractor = &knowledge.Extractor{Provider: "forged", Model: "model", Version: "v1", PromptSHA256: strings.Repeat("a", 64)}
	status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, encode(machineGenerated)), 422)
	numeric := strings.Replace(claimBody, `"value":"125.5"`, `"value":125.5`, 1)
	status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, numeric), 422)
	createdClaim := call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, claimBody)
	status(createdClaim, 200)
	if createdClaim.Header().Get("ETag") != `"1"` {
		t.Fatal("missing claim ETag")
	}
	var result candidateClaimResponse
	if err = json.Unmarshal(createdClaim.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "candidate" || result.Revision != 1 || !result.SourcesCurrent || result.Extractor != nil || result.ActorSubject != owner {
		t.Fatalf("candidate receipt %+v", result)
	}
	status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, claimBody), 200)
	changed := claim
	changed.Literal = &knowledge.Literal{Type: "decimal", Value: json.RawMessage(`"130"`), Unit: "units/hour"}
	wantProblem(t, call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, encode(changed)), 409, "IDEMPOTENCY_CONFLICT")
	wantProblem(t, call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(changed)), 412, "REVISION_CONFLICT")
	revised := call(writer, "POST", base+"/claims", uuid.NewString(), `"1"`, writer.csrf, encode(changed))
	status(revised, 200)
	if revised.Header().Get("ETag") != `"2"` {
		t.Fatal("claim revision did not advance")
	}
	oldResponse := call(reader, "GET", claimPath+"?revision=1", "", "", "", "")
	status(oldResponse, 200)
	var old candidateClaimResponse
	if err = json.Unmarshal(oldResponse.Body.Bytes(), &old); err != nil {
		t.Fatal(err)
	}
	if string(old.Literal.Value) != `"125.5"` || old.Revision != 1 || old.CurrentRevision != 2 || old.State != "candidate" {
		t.Fatal("old candidate changed")
	}
	// An unknown claim state is rejected; contested is a listable WIS-270 state.
	for _, path := range []string{base + "/claims?state=disputed", base + "/claims?limit=0", claimPath + "?revision=0"} {
		status(call(writer, "GET", path, "", "", "", ""), 422)
	}
	if approved := call(reader, "GET", base+"/claims?state=approved&subject_id="+entity.EntityID, "", "", "", ""); approved.Code != 200 || strings.Contains(approved.Body.String(), entity.EntityID) {
		t.Fatalf("unreviewed claim listed as approved: %d %s", approved.Code, approved.Body.String())
	}
	pageResponse := call(reader, "GET", base+"/claims?state=candidate&subject_id="+entity.EntityID, "", "", "", "")
	status(pageResponse, 200)
	var claims knowledge.ClaimPage
	if err = json.Unmarshal(pageResponse.Body.Bytes(), &claims); err != nil {
		t.Fatal(err)
	}
	if len(claims.Claims) != 1 || claims.Claims[0].Revision != 2 || claims.Claims[0].State != "candidate" {
		t.Fatal("candidate filter differs")
	}
	// WIS-270 corroboration roundtrip: one current, undesignated cited source.
	const oneGroup = `"corroboration":{"independent_groups":1,"model_extracted_sources":0,"authoritative":false}`
	if current := call(reader, "GET", claimPath, "", "", "", ""); current.Code != 200 || !strings.Contains(current.Body.String(), oneGroup) || !strings.Contains(pageResponse.Body.String(), oneGroup) {
		t.Fatalf("corroboration not serialized exactly: %d %s / %s", current.Code, current.Body.String(), pageResponse.Body.String())
	}
	status(call(writer, "GET", "/v1/sites/"+otherSite+"/claims/"+claim.ClaimID, "", "", "", ""), 404)
	status(call(writer, "GET", "/v1/sites/"+foreignSite+"/claims/"+claim.ClaimID, "", "", "", ""), 404)
	status(call(writer, "POST", "/v1/sites/"+otherSite+"/claims", uuid.NewString(), `"0"`, writer.csrf, claimBody), 404)
	status(call(writer, "POST", claimPath+"/approve", uuid.NewString(), `"2"`, writer.csrf, `{}`), 404)

	source.Text = "Machine A reportedly runs at 140 units/hour now."
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"1"`, writer.csrf, encode(source)), 200)
	stale := call(reader, "GET", claimPath, "", "", "", "")
	status(stale, 200)
	if !strings.Contains(stale.Body.String(), `"corroboration":{"independent_groups":0,"model_extracted_sources":0,"authoritative":false}`) {
		t.Fatalf("superseded citation still corroborates: %s", stale.Body.String())
	}
	var staleClaim candidateClaimResponse
	if err = json.Unmarshal(stale.Body.Bytes(), &staleClaim); err != nil {
		t.Fatal(err)
	}
	if staleClaim.SourcesCurrent || staleClaim.State != "candidate" {
		t.Fatal("stale source silently retained current status")
	}
	newClaim := claim
	newClaim.ClaimID = uuid.NewString()
	staleCreate := call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(newClaim))
	status(staleCreate, 409)
	if !strings.Contains(staleCreate.Body.String(), "SOURCE_CHANGED") {
		t.Fatal("source drift not explicit")
	}
	// Idempotent retrieval can return the old candidate only with stale provenance marked.
	replay := call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, claimBody)
	status(replay, 200)
	var replayed candidateClaimResponse
	if err = json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.SourcesCurrent {
		t.Fatal("replay concealed source drift")
	}

	// A correction retains the entity ID and binds a new immutable source revision.
	replacementSource := evidence.Input{SourceID: uuid.NewString(), Kind: "document", Title: "Updated capacity evidence", Text: "Machine A documented capacity is 140 units/hour."}
	status(call(writer, "POST", base+"/sources", uuid.NewString(), `"0"`, writer.csrf, encode(replacementSource)), 200)
	replacementSpanResponse := call(writer, "POST", base+"/sources/"+replacementSource.SourceID+"/spans", uuid.NewString(), `"1"`, writer.csrf, `{"revision":1,"start_char":0,"end_char":9}`)
	status(replacementSpanResponse, 200)
	var replacementSpan evidence.Span
	if err = json.Unmarshal(replacementSpanResponse.Body.Bytes(), &replacementSpan); err != nil {
		t.Fatal(err)
	}
	replacementEntity := entity
	replacementEntity.Name = "Machine A corrected"
	replacementEntity.SourceID = replacementSource.SourceID
	replacementEntity.SourceRevision = 1
	wrongKind := replacementEntity
	wrongKind.AssetKind = "line"
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"1"`, writer.csrf, encode(wrongKind)), 422)
	wrongType := replacementEntity
	wrongType.Type = "operation"
	wrongType.AssetKind = ""
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"1"`, writer.csrf, encode(wrongType)), 422)
	replacementEntityResponse := call(writer, "POST", base+"/entities", uuid.NewString(), `"1"`, writer.csrf, encode(replacementEntity))
	status(replacementEntityResponse, 200)
	if replacementEntityResponse.Header().Get("ETag") != `"2"` {
		t.Fatal("entity CAS revision did not advance")
	}
	var replacement candidateEntityResponse
	if err = json.Unmarshal(replacementEntityResponse.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.EntityID != entity.EntityID || replacement.Revision != 2 || replacement.CurrentRevision != 2 || !replacement.SourcesCurrent {
		t.Fatal("entity identity/current revision changed incorrectly")
	}
	entityPath := base + "/entities/" + entity.EntityID
	latestEntity := call(reader, "GET", entityPath, "", "", "", "")
	status(latestEntity, 200)
	if latestEntity.Header().Get("ETag") != `"2"` {
		t.Fatal("latest entity ETag incorrect")
	}
	entityHistoryResponse := call(reader, "GET", entityPath+"?revision=1", "", "", "", "")
	status(entityHistoryResponse, 200)
	var entityHistory candidateEntityResponse
	if err = json.Unmarshal(entityHistoryResponse.Body.Bytes(), &entityHistory); err != nil {
		t.Fatal(err)
	}
	if entityHistory.Revision != 1 || entityHistory.CurrentRevision != 2 || entityHistory.Name != "Machine A" || entityHistory.SourceID != sourceID || entityHistory.SourcesCurrent {
		t.Fatal("entity history/provenance was rewritten")
	}
	if entityHistoryResponse.Header().Get("ETag") != `"1"` {
		t.Fatal("historical entity ETag must identify the selected revision")
	}
	status(call(writer, "GET", entityPath+"?revision=0", "", "", "", ""), 422)
	status(call(writer, "GET", "/v1/sites/"+otherSite+"/entities/"+entity.EntityID, "", "", "", ""), 404)
	pinnedOld := call(reader, "GET", claimPath+"?revision=1", "", "", "", "")
	status(pinnedOld, 200)
	if err = json.Unmarshal(pinnedOld.Body.Bytes(), &old); err != nil {
		t.Fatal(err)
	}
	if old.SubjectRevision != 1 || old.SourcesCurrent {
		t.Fatal("old claim lost exact entity revision pin")
	}
	stalePin := changed
	stalePin.ClaimID = uuid.NewString()
	stalePin.SourceSpanIDs = []string{replacementSpan.ID}
	stalePin.SubjectRevision = 1
	staleSubject := call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(stalePin))
	status(staleSubject, 409)
	if !strings.Contains(staleSubject.Body.String(), "SOURCE_CHANGED") {
		t.Fatal("subject revision drift not explicit")
	}
	recovered := changed
	recovered.SubjectRevision = 2
	recovered.SourceSpanIDs = []string{replacementSpan.ID}
	recoveredResponse := call(writer, "POST", base+"/claims", uuid.NewString(), `"2"`, writer.csrf, encode(recovered))
	status(recoveredResponse, 200)
	var recoveredClaim candidateClaimResponse
	if err = json.Unmarshal(recoveredResponse.Body.Bytes(), &recoveredClaim); err != nil {
		t.Fatal(err)
	}
	if recoveredClaim.Revision != 3 || recoveredClaim.SubjectRevision != 2 || !recoveredClaim.SourcesCurrent {
		t.Fatal("explicit repinning did not recover current provenance")
	}
	status(call(writer, "POST", base+"/sources/"+sourceID+"/revocations", uuid.NewString(), `"2"`, writer.csrf, `{"reason":"old source withdrawn"}`), 200)
	status(call(reader, "GET", entityPath+"?revision=1", "", "", "", ""), 404)
	status(call(reader, "GET", entityPath, "", "", "", ""), 200)
	status(call(reader, "GET", claimPath+"?revision=1", "", "", "", ""), 404)
	status(call(reader, "GET", claimPath, "", "", "", ""), 200)
	status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, claimBody), 404)
	// Relationship endpoints are pinned independently: correcting the object
	// cannot silently change the meaning of an existing claim.
	line := knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "line", Name: "Line A", SourceID: replacementSource.SourceID, SourceRevision: 1}
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"0"`, writer.csrf, encode(line)), 200)
	relation := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 2, Predicate: "part_of_asset", ObjectEntityID: line.EntityID, ObjectRevision: 1, EffectiveFrom: claim.EffectiveFrom, EvidenceClass: "report", SourceSpanIDs: []string{replacementSpan.ID}}
	missingObjectPin := relation
	missingObjectPin.ObjectRevision = 0
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(missingObjectPin)), 422)
	status(call(writer, "POST", base+"/claims", uuid.NewString(), `"0"`, writer.csrf, encode(relation)), 200)
	line.Name = "Line A corrected"
	status(call(writer, "POST", base+"/entities", uuid.NewString(), `"1"`, writer.csrf, encode(line)), 200)
	relationPath := base + "/claims/" + relation.ClaimID
	pinnedRelation := call(reader, "GET", relationPath, "", "", "", "")
	status(pinnedRelation, 200)
	var relationResult candidateClaimResponse
	if err = json.Unmarshal(pinnedRelation.Body.Bytes(), &relationResult); err != nil {
		t.Fatal(err)
	}
	if relationResult.ObjectRevision != 1 || relationResult.SubjectRevision != 2 || relationResult.SourcesCurrent {
		t.Fatal("object correction silently repinned a relationship")
	}
	staleObject := call(writer, "POST", base+"/claims", uuid.NewString(), `"1"`, writer.csrf, encode(relation))
	status(staleObject, 409)
	if !strings.Contains(staleObject.Body.String(), "SOURCE_CHANGED") {
		t.Fatal("object revision drift not explicit")
	}
	relation.ObjectRevision = 2
	repinnedRelation := call(writer, "POST", base+"/claims", uuid.NewString(), `"1"`, writer.csrf, encode(relation))
	status(repinnedRelation, 200)
	if err = json.Unmarshal(repinnedRelation.Body.Bytes(), &relationResult); err != nil {
		t.Fatal(err)
	}
	if relationResult.ObjectRevision != 2 || relationResult.Revision != 2 || !relationResult.SourcesCurrent {
		t.Fatal("explicit object repinning did not restore current provenance")
	}
	// Withdrawal of the replacement evidence finally hides the current revision.
	status(call(writer, "POST", base+"/sources/"+replacementSource.SourceID+"/revocations", uuid.NewString(), `"1"`, writer.csrf, `{"reason":"replacement source withdrawn"}`), 200)

	status(call(reader, "GET", claimPath, "", "", "", ""), 404)
	status(call(reader, "GET", claimPath+"?revision=1", "", "", "", ""), 404)
	status(call(writer, "POST", base+"/claims", claimKey, `"0"`, writer.csrf, claimBody), 404)
	hidden := call(reader, "GET", base+"/claims?state=candidate", "", "", "", "")
	status(hidden, 200)
	if err = json.Unmarshal(hidden.Body.Bytes(), &claims); err != nil {
		t.Fatal(err)
	}
	if len(claims.Claims) != 0 {
		t.Fatal("revoked provenance claim listed")
	}
	hiddenEntities := call(reader, "GET", base+"/entities", "", "", "", "")
	status(hiddenEntities, 200)
	if err = json.Unmarshal(hiddenEntities.Body.Bytes(), &entities); err != nil {
		t.Fatal(err)
	}
	if len(entities.Entities) != 0 {
		t.Fatal("revoked source entity listed")
	}
}
