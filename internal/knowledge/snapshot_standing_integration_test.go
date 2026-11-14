//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/google/uuid"
)

// WIS-517 post-merge review P1: a snapshot is immutable, but a claim in it whose approval no
// longer stands (review withdrawn, pinned designation superseded or withdrawn, or the pinned
// source revision is model-extracted text the designation never confirmed, as migration 077
// leaves legacy OCR approvals) must not reach any snapshot reader. On main, LoadSnapshotTx
// checked only source revocation and returned all three claims.
func TestLoadSnapshotDropsClaimsWhoseApprovalNoLongerStands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("BLOCKED: disposable PostgreSQL admin/runtime URLs required")
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtime, err := sql.Open("pgx", runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	sc := Scope{uuid.NewString(), uuid.NewString()}
	exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'standing approval')`, sc.TenantID)
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'standing approval')`, sc.TenantID, sc.SiteID)
	owner := Actor{"https://standing.invalid", uuid.NewString(), strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")}
	exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'fixture',now(),now(),now()+interval '1 hour',now())`, owner.SessionHash, owner.Issuer, owner.Subject)
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, sc.TenantID, sc.SiteID, owner.Issuer, owner.Subject)
	service, ev := New(runtime), evidence.New(runtime)
	ea := evidence.Actor{Issuer: owner.Issuer, Subject: owner.Subject, SessionHash: owner.SessionHash}
	es := evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	type approved struct{ claim, review, source, entity, span string }
	// claim records a claim on entity (a new one when empty) from a new designated source.
	claim := func(entity, text string) approved {
		t.Helper()
		source := uuid.NewString()
		if _, e := ev.CreateOrRevise(ctx, ea, es, evidence.Input{SourceID: source, Kind: "document", Title: "SOP", Text: text}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if entity == "" {
			entity = uuid.NewString()
			if _, e := service.CreateEntity(ctx, owner, sc, EntityInput{EntityID: entity, Type: "operation", Name: "Op " + entity[:8], SourceID: source, SourceRevision: 1}, uuid.NewString(), 0); e != nil {
				t.Fatal(e)
			}
		}
		span, e := ev.CreateSpan(ctx, ea, es, evidence.SpanInput{SourceID: source, Revision: 1, Start: 0, End: len(text)}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		in := ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity, SubjectRevision: 1, Predicate: "actual_step", Literal: &Literal{Type: "string", Value: json.RawMessage(`"` + text + `"`)}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
		if _, e = service.CreateClaim(ctx, owner, sc, in, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		if _, e = ev.DecideAuthority(ctx, ea, es, source, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Owner verified"}, uuid.NewString(), 0); e != nil {
			t.Fatal(e)
		}
		return approved{claim: in.ClaimID, source: source, entity: entity, span: span.ID}
	}
	review := func(c approved) approved {
		t.Helper()
		r, e := service.DecideReview(ctx, owner, sc, c.claim, ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Reviewed"}, uuid.NewString(), 1)
		if e != nil {
			t.Fatal(e)
		}
		c.review = r.ReviewID
		return c
	}
	approve := func(text string) approved { return review(claim("", text)) }
	kept, withdrawn, legacyOCR, unDesignated, contested := approve("Keep this step"), approve("Withdrawn step"), approve("Scanned step"), approve("Undesignated step"), approve("Contested step")
	// A conflict resolved for the winner, whose approval pins that resolution.
	winner := claim("", "Preferred step")
	claim(winner.entity, "Rejected step")
	conflicts, err := service.ListConflicts(ctx, owner, sc, winner.claim, 1, 10, "")
	if err != nil || len(conflicts.Conflicts) != 1 {
		t.Fatalf("conflict fixture: %+v %v", conflicts, err)
	}
	conflict := conflicts.Conflicts[0]
	prefer := "prefer_left"
	if conflict.Right.ClaimID == winner.claim {
		prefer = "prefer_right"
	}
	if _, err = service.DecideResolution(ctx, owner, sc, conflict.ID, ResolutionInput{Decision: prefer, Reason: "Owner prefers it"}, uuid.NewString(), 0); err != nil {
		t.Fatal(err)
	}
	resolved := review(winner)
	inv, err := service.ReadInventory(ctx, owner, sc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := service.CreateSnapshot(ctx, owner, sc, SnapshotInput{SnapshotID: uuid.NewString(), Reason: "Reviewed"}, uuid.NewString(), inv.Revision)
	if err != nil || len(snap.Contents.Claims) != 6 || len(snap.Contents.Resolutions) != 1 {
		t.Fatalf("snapshot: %d claims, %v", len(snap.Contents.Claims), err)
	}

	if _, err = service.RevokeReview(ctx, owner, sc, withdrawn.claim, RevocationInput{ClaimRevision: 1, ReviewID: withdrawn.review, Reason: "Withdrawn"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err = service.DecideResolution(ctx, owner, sc, conflict.ID, ResolutionInput{Decision: "withdraw", Reason: "Reconsider"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	// A candidate claim recorded after the snapshot (an agent can write one) contests the approved
	// one, but must not remove it from readers: only a human decision does.
	claim(contested.entity, "Competing step")
	if _, err = ev.DecideAuthority(ctx, ea, es, unDesignated.source, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "withdraw", Reason: "No longer authoritative"}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	// What migration 077 does to a legacy OCR revision: its text is marked model-extracted, and
	// the designation made before 077 carries model_extracted_confirmed=false.
	run := uuid.NewString()
	exec(`INSERT INTO supervisor.agent_runs(tenant_id,site_id,id,request_kind,conversation_id,request_id,trace_id,provider_name,provider_actual,model,model_revision,prompt_version,tool_version,policy_revision,invocations_left,tool_calls_left,requests_left,tokens_left,cost_left_microusd)
 VALUES($1,$2,$3,'extraction',gen_random_uuid(),gen_random_uuid(),$4,'fixture',false,'vision','r','p','t',1,1,1,1,1,1)`, sc.TenantID, sc.SiteID, run, strings.Repeat("a", 32))
	exec(`ALTER TABLE supervisor.evidence_revisions DISABLE TRIGGER evidence_immutable, DISABLE TRIGGER knowledge_inventory_changed`)
	exec(`UPDATE supervisor.evidence_revisions SET extraction='model_ocr',extractor_model='vision',extractor_model_revision='r',extractor_prompt_sha256=repeat('b',64),extraction_run_id=$4 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, legacyOCR.source, run)
	exec(`ALTER TABLE supervisor.evidence_revisions ENABLE TRIGGER evidence_immutable, ENABLE TRIGGER knowledge_inventory_changed`)

	tx, err := runtime.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSnapshotTx(ctx, tx, sc, snap.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got.Contents.Claims {
		ids = append(ids, c.ClaimID)
	}
	want := []string{kept.claim, contested.claim}
	slices.Sort(want)
	slices.Sort(ids)
	if !slices.Equal(ids, want) {
		t.Fatalf("snapshot claims %v, want only the standing approvals %v (withdrawn %s, legacy OCR %s, undesignated %s, resolution withdrawn %s)", ids, want, withdrawn.claim, legacyOCR.claim, unDesignated.claim, resolved.claim)
	}
	// Nothing only a dropped claim referenced reaches the reader.
	var entities []string
	for _, e := range got.Contents.Entities {
		entities = append(entities, e.EntityID)
	}
	wantEntities := []string{kept.entity, contested.entity}
	slices.Sort(wantEntities)
	if !slices.Equal(entities, wantEntities) || len(got.Contents.Resolutions) != 0 {
		t.Fatalf("pruned entities %+v resolutions %+v", got.Contents.Entities, got.Contents.Resolutions)
	}
	for _, sp := range got.Contents.Spans {
		if sp.SpanID != kept.span && sp.SpanID != contested.span {
			t.Fatalf("span %s of a dropped claim still listed", sp.SpanID)
		}
	}
	if got.SHA256 != snap.SHA256 {
		t.Fatalf("stored digest changed: %s want %s", got.SHA256, snap.SHA256)
	}
	read, err := service.ReadSnapshot(ctx, owner, sc, snap.SnapshotID)
	if err != nil || len(read.Contents.Claims) != 2 {
		t.Fatalf("ReadSnapshot returned %+v, %v", read.Contents.Claims, err)
	}
}
