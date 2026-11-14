//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

// draftSpan stores a source as actor on site and cuts one span over its whole text.
func (f *stationFixture) draftSpan(actor stationSession, site, text string) (string, string) {
	f.t.Helper()
	source := uuid.NewString()
	f.expect(f.call(actor, "POST", "/v1/sites/"+site+"/sources", uuid.NewString(), `"0"`, `{"source_id":"`+source+`","kind":"manual","title":"Line notes","text":"`+text+`"}`), 200, "")
	w := f.call(actor, "POST", "/v1/sites/"+site+"/sources/"+source+"/spans", uuid.NewString(), `"1"`, fmt.Sprintf(`{"revision":1,"start_char":0,"end_char":%d}`, len(text)))
	f.expect(w, 200, "")
	var span struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &span); err != nil || span.ID == "" {
		f.t.Fatalf("span: %s", w.Body.String())
	}
	return source, span.ID
}

func (f *stationFixture) revokeSource(source string) {
	f.t.Helper()
	var state int64
	if err := f.control.QueryRowContext(f.ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE source_id=$1`, source).Scan(&state); err != nil {
		f.t.Fatal(err)
	}
	f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/sources/"+source+"/revocations", uuid.NewString(), fmt.Sprintf(`"%d"`, state), `{"reason":"Notes withdrawn"}`), 200, "")
}

// bumpAccessEpoch moves a source's access epoch without revoking it. No product path does this
// today (the ledger bumps the epoch only on revocation), so the test bypasses the head guard
// as the privileged role to pin draft behaviour for a future access-change path.
func (f *stationFixture) bumpAccessEpoch(source string) {
	f.t.Helper()
	tx, err := f.control.BeginTx(f.ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(f.ctx, `SET LOCAL session_replication_role=replica`); err != nil {
		f.t.Fatal(err)
	}
	if _, err = tx.ExecContext(f.ctx, `UPDATE supervisor.evidence_heads SET access_epoch=access_epoch+1 WHERE source_id=$1 AND NOT revoked`, source); err != nil {
		f.t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
}

type draftSpanIn struct {
	Path   string `json:"path"`
	SpanID string `json:"span_id"`
}

func lineDraft(name string, active bool, spans ...draftSpanIn) map[string]any {
	if spans == nil {
		spans = []draftSpanIn{}
	}
	return map[string]any{"kind": "line", "body": masterLineBody(name, active), "field_spans": spans}
}

func (f *stationFixture) draft(w *httptest.ResponseRecorder, status int) workstation.ConfigurationDraftV1 {
	f.t.Helper()
	f.expect(w, status, "")
	var out workstation.ConfigurationDraftV1
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	if w.Header().Get("ETag") != fmt.Sprintf(`"%d"`, out.CurrentRevision) {
		f.t.Fatalf("ETag %q does not carry revision %d", w.Header().Get("ETag"), out.CurrentRevision)
	}
	return out
}

func (f *stationFixture) createDraft(actor stationSession, body any) workstation.ConfigurationDraftV1 {
	f.t.Helper()
	return f.draft(f.call(actor, "POST", "/v1/sites/"+f.site+"/configuration-drafts", uuid.NewString(), "", f.encode(body)), 201)
}

func (f *stationFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.control.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *stationFixture) expectPath(w *httptest.ResponseRecorder, status int, code, path string) {
	f.t.Helper()
	f.expect(w, status, code)
	if !strings.Contains(w.Body.String(), `"path":"`+path+`"`) {
		f.t.Fatalf("problem path want %s: %s", path, w.Body.String())
	}
}

// TestConfigurationDraftsGate is the proposal F slice 2 human-path gate (design §4b): drafts
// never become configuration except through a human promotion by someone who neither wrote nor
// edited them, citations stay in tenant scope, and revoked sources block promotion.
func TestConfigurationDraftsGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	drafts := "/v1/sites/" + f.site + "/configuration-drafts"
	_, nameSpan := f.draftSpan(f.lead, f.site, "Line is called Assembly")
	_, activeSpan := f.draftSpan(f.lead, f.site, "Assembly runs today")

	t.Run("forbidden_kind", func(t *testing.T) {
		f.t = t
		before := f.count(`SELECT count(*) FROM supervisor.configuration_drafts`)
		for _, kind := range []string{"membership", "site", "tenant", "shift_plan", "shift_calendar", "site_settings", "Membership", ""} {
			body := lineDraft("Assembly", true)
			body["kind"] = kind
			f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(body)), 422, "INVALID_CONFIGURATION_DRAFT", "kind")
		}
		if n := f.count(`SELECT count(*) FROM supervisor.configuration_drafts`); n != before {
			t.Fatalf("forbidden kinds stored %d drafts", n-before)
		}
		if d := f.createDraft(f.lead, lineDraft("Assembly", true)); d.Kind != workstation.DraftKindLine || d.Status != workstation.DraftOpen || d.AuthorKind != workstation.DraftAuthorHuman {
			t.Fatalf("control line draft: %+v", d)
		}
	})

	t.Run("strict_contract_and_roles", func(t *testing.T) {
		f.t = t
		valid := f.encode(lineDraft("Assembly", true))
		for _, extra := range []string{`"approved":true`, `"tenant_id":"` + f.otherTenant + `"`, `"status":"promoted"`} {
			f.expect(f.call(f.lead, "POST", drafts, uuid.NewString(), "", strings.TrimSuffix(valid, "}")+","+extra+"}"), 422, "INVALID_CONFIGURATION_DRAFT")
		}
		bodyExtra := `{"kind":"line","body":{"schema_version":1,"display_name":"A","active":true,"promote":true},"field_spans":[]}`
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", bodyExtra), 422, "INVALID_CONFIGURATION_DRAFT", "body")
		f.expect(f.call(f.lead, "POST", drafts, "", "", valid), 422, "INVALID_REQUEST")
		f.expect(f.call(f.viewer, "POST", drafts, uuid.NewString(), "", valid), 403, "PERMISSION_DENIED")
		f.expect(f.call(f.operator, "POST", drafts, uuid.NewString(), "", valid), 403, "PERMISSION_DENIED")
		d := f.createDraft(f.lead, lineDraft("Assembly", true))
		// Drafts are visible to owners and supervisors only.
		f.expect(f.call(f.viewer, "GET", drafts+"/"+d.DraftID, "", "", ""), 403, "PERMISSION_DENIED")
		f.expect(f.call(f.operator, "GET", drafts, "", "", ""), 403, "PERMISSION_DENIED")
		f.draft(f.call(f.owner, "GET", drafts+"/"+d.DraftID, "", "", ""), 200)
		// A cross-tenant lookup is the non-disclosing 404, in the outsider's own site too.
		f.expect(f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/configuration-drafts/"+d.DraftID, "", "", ""), 404, "NOT_FOUND")
		f.expect(f.call(f.outsider, "GET", drafts+"/"+d.DraftID, "", "", ""), 404, "")
		var page workstation.ConfigurationDraftPageV1
		w := f.call(f.outsider, "GET", "/v1/sites/"+f.otherSite+"/configuration-drafts", "", "", "")
		f.expect(w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Drafts) != 0 {
			t.Fatalf("outsider listed %s", w.Body.String())
		}
	})

	t.Run("invented_or_cross_tenant_span", func(t *testing.T) {
		f.t = t
		_, foreignSpan := f.draftSpan(f.outsider, f.otherSite, "Other tenant notes")
		before := f.count(`SELECT count(*) FROM supervisor.configuration_drafts`)
		for _, bad := range []string{uuid.NewString(), foreignSpan} {
			body := lineDraft("Assembly", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", activeSpan}, draftSpanIn{"/display_name", bad})
			f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(body)), 422, "CITATION_NOT_IN_SCOPE", "field_spans[2].span_id")
		}
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(lineDraft("Assembly", true, draftSpanIn{"/schema_version", nameSpan}))), 422, "INVALID_CONFIGURATION_DRAFT", "field_spans[0].path")
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(lineDraft("Assembly", true, draftSpanIn{"/owner", nameSpan}))), 422, "INVALID_CONFIGURATION_DRAFT", "field_spans[0].path")
		many := make([]draftSpanIn, workstation.MaxDraftFieldSpans+1)
		for i := range many {
			many[i] = draftSpanIn{"/display_name", uuid.NewString()}
		}
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(lineDraft("Assembly", true, many...))), 422, "LIMIT_EXCEEDED", "field_spans")
		if n := f.count(`SELECT count(*) FROM supervisor.configuration_drafts`); n != before {
			t.Fatalf("refused citations stored %d drafts", n-before)
		}
		d := f.createDraft(f.lead, lineDraft("Assembly", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", activeSpan}))
		if c := d.Revisions[0].Citations; len(c) != 2 || !c[0].Available || !c[1].Available {
			t.Fatalf("control citations: %+v", c)
		}
		// Bypassing the API, the runtime role cannot cite another tenant's span either.
		var source string
		var revision int64
		if err := f.control.QueryRowContext(f.ctx, `SELECT source_id,revision FROM supervisor.evidence_spans WHERE id=$1`, foreignSpan).Scan(&source, &revision); err != nil {
			t.Fatal(err)
		}
		f.refused(f.tenant, "cited a cross-tenant span", "23514", "configuration draft span must name its evidence revision", `INSERT INTO supervisor.configuration_draft_field_spans(tenant_id,site_id,draft_id,revision,field_path,span_id,source_id,source_revision,source_access_epoch) VALUES($1,$2,$3,1,'/active',$4,$5,$6,1)`, f.tenant, f.site, d.DraftID, foreignSpan, source, revision)
	})

	t.Run("promoter_is_editor", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Paint", true, draftSpanIn{"/display_name", nameSpan}))
		edited := f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Paint shop", true), "field_spans": []draftSpanIn{}})), 201)
		for _, who := range []stationSession{f.lead, f.approver} {
			f.expect(f.call(who, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"2"`, `{}`), 403, "PROMOTER_IS_EDITOR")
		}
		f.expect(f.call(f.owner, "GET", "/v1/sites/"+f.site+"/lines/"+d.DraftID, "", "", ""), 404, "NOT_FOUND")
		if got := f.draft(f.call(f.owner, "GET", drafts+"/"+d.DraftID, "", "", ""), 200); got.Status != workstation.DraftOpen || got.Promotion != nil || edited.CurrentRevision != 2 {
			t.Fatalf("refused promotion changed the draft: %+v", got)
		}
		promoted := f.draft(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"2"`, `{}`), 201)
		if promoted.Status != workstation.DraftPromoted || promoted.Promotion == nil || promoted.Promotion.ObjectID != d.DraftID || promoted.Promotion.ObjectRevision != 1 {
			t.Fatalf("promotion: %+v", promoted)
		}
		var line workstation.LineV1
		w := f.call(f.viewer, "GET", "/v1/sites/"+f.site+"/lines/"+d.DraftID, "", "", "")
		f.expect(w, 200, "")
		if err := json.Unmarshal(w.Body.Bytes(), &line); err != nil || line.Revisions[0].Body.DisplayName != "Paint shop" || line.Revisions[0].SubmittedBy.Subject != f.owner.subject {
			t.Fatalf("promoted line: %s", w.Body.String())
		}
	})

	t.Run("service_identity_and_operator_cannot_promote", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Welding", true))
		f.expect(f.call(f.operator, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 403, "PERMISSION_DENIED")
		f.expect(f.call(f.viewer, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 403, "PERMISSION_DENIED")
		forge := func(issuer string) workstation.Actor {
			token, subject := uuid.NewString(), uuid.NewString()
			sum := sha256.Sum256([]byte(token))
			f.exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'supervisor')`, f.tenant, f.site, issuer, subject)
			f.exec(`INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '1 hour',now())`, hex.EncodeToString(sum[:]), subject, issuer, uuid.NewString())
			return workstation.Actor{Issuer: issuer, Subject: subject, SessionHash: hex.EncodeToString(sum[:])}
		}
		service := workstation.NewConfigurationDrafts(f.runtime)
		scope := workstation.Scope{TenantID: f.tenant, SiteID: f.site}
		if _, err := service.Promote(f.ctx, forge("supervisor:agent"), scope, d.DraftID, uuid.NewString(), 1); !errors.Is(err, workstation.ErrDenied) {
			t.Fatalf("service identity promotion: %v", err)
		}
		if _, err := service.Promote(f.ctx, workstation.Actor{Issuer: "supervisor:agent", Subject: "run-" + uuid.NewString(), SessionHash: strings.Repeat("0", 64)}, scope, d.DraftID, uuid.NewString(), 1); !errors.Is(err, workstation.ErrDenied) {
			t.Fatalf("sessionless model promotion: %v", err)
		}
		if n := f.count(`SELECT count(*) FROM supervisor.line_heads WHERE id=$1`, d.DraftID); n != 0 {
			t.Fatal("refused promotion wrote a line")
		}
		// Control: the same forged session shape with a human issuer promotes.
		out, err := service.Promote(f.ctx, forge(f.issuer), scope, d.DraftID, uuid.NewString(), 1)
		if err != nil || out.Status != workstation.DraftPromoted {
			t.Fatalf("human promotion: %+v %v", out, err)
		}
	})

	t.Run("revoked_source", func(t *testing.T) {
		f.t = t
		source, span := f.draftSpan(f.lead, f.site, "Packing line is active")
		d := f.createDraft(f.lead, lineDraft("Packing", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", span}))
		control := f.createDraft(f.lead, lineDraft("Packing 2", true, draftSpanIn{"/display_name", nameSpan}))
		f.revokeSource(source)
		got := f.draft(f.call(f.lead, "GET", drafts+"/"+d.DraftID, "", "", ""), 200)
		available := map[string]bool{}
		for _, c := range got.Revisions[0].Citations {
			available[c.Path] = c.Available
		}
		if !available["/display_name"] || available["/active"] {
			t.Fatalf("citations after revocation: %+v", got.Revisions[0].Citations)
		}
		w := f.call(f.lead, "GET", drafts+"?status=draft&kind=line&limit=100", "", "", "")
		f.expect(w, 200, "")
		var page workstation.ConfigurationDraftPageV1
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range page.Drafts {
			if s.DraftID == d.DraftID {
				found = s.CitationCount == 2 && s.UnavailableCitationCount == 1
			}
		}
		if !found {
			t.Fatalf("list citation counts: %s", w.Body.String())
		}
		f.expect(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "DRAFT_SOURCE_UNAVAILABLE")
		if n := f.count(`SELECT count(*) FROM supervisor.line_heads WHERE id=$1`, d.DraftID); n != 0 {
			t.Fatal("promotion with a revoked citation wrote a line")
		}
		f.draft(f.call(f.owner, "POST", drafts+"/"+control.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 201)
		// A new draft cannot cite the revoked source at all.
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(lineDraft("Packing", true, draftSpanIn{"/active", span}))), 422, "CITATION_NOT_IN_SCOPE", "field_spans[0].span_id")
	})

	t.Run("idempotency_and_revision", func(t *testing.T) {
		f.t = t
		body := f.encode(lineDraft("Boxing", true, draftSpanIn{"/display_name", nameSpan}))
		first := f.draft(f.call(f.lead, "POST", drafts, "draft-create", "", body), 201)
		replay := f.draft(f.call(f.lead, "POST", drafts, "draft-create", "", body), 201)
		if !replay.Replayed || replay.DraftID != first.DraftID {
			t.Fatalf("replay: %+v", replay)
		}
		f.expect(f.call(f.lead, "POST", drafts, "draft-create", "", f.encode(lineDraft("Other", true))), 409, "IDEMPOTENCY_CONFLICT")
		edit := func(name string) string {
			return f.encode(map[string]any{"body": masterLineBody(name, true), "field_spans": []draftSpanIn{}})
		}
		base := drafts + "/" + first.DraftID
		f.expect(f.call(f.lead, "POST", base+"/revisions", uuid.NewString(), "", edit("Boxing 2")), 428, "REVISION_REQUIRED")
		f.expect(f.call(f.lead, "POST", base+"/revisions", uuid.NewString(), `"2"`, edit("Boxing 2")), 412, "REVISION_CONFLICT")
		f.expect(f.call(f.lead, "POST", base+"/revisions", uuid.NewString(), `"1"`, edit("Boxing")), 409, "DRAFT_UNCHANGED")
		second := f.draft(f.call(f.lead, "POST", base+"/revisions", "draft-edit", `"1"`, edit("Boxing 2")), 201)
		if second.CurrentRevision != 2 || len(second.Revisions) != 2 || string(second.Revisions[0].Body) == string(second.Revisions[1].Body) {
			t.Fatalf("append-only revisions: %+v", second)
		}
		f.expect(f.call(f.lead, "POST", base+"/revisions", uuid.NewString(), `"1"`, edit("Boxing 3")), 412, "REVISION_CONFLICT")
		f.expect(f.call(f.owner, "POST", base+"/promotions", uuid.NewString(), `"1"`, `{}`), 412, "REVISION_CONFLICT")
		f.expect(f.call(f.owner, "POST", base+"/promotions", uuid.NewString(), `"2"`, `{"approved":true}`), 422, "INVALID_CONFIGURATION_DRAFT")
		promoted := f.draft(f.call(f.owner, "POST", base+"/promotions", "draft-promote", `"2"`, `{}`), 201)
		again := f.draft(f.call(f.owner, "POST", base+"/promotions", "draft-promote", `"2"`, `{}`), 201)
		if !again.Replayed || again.Promotion == nil || again.Promotion.ObjectRevision != promoted.Promotion.ObjectRevision {
			t.Fatalf("promotion replay: %+v", again)
		}
		f.expect(f.call(f.approver, "POST", base+"/promotions", uuid.NewString(), `"2"`, `{}`), 409, "DRAFT_NOT_OPEN")
		f.expect(f.call(f.lead, "POST", base+"/discards", uuid.NewString(), `"2"`, `{}`), 409, "DRAFT_NOT_OPEN")
		f.expect(f.call(f.lead, "POST", base+"/revisions", uuid.NewString(), `"2"`, edit("Boxing 4")), 409, "DRAFT_NOT_OPEN")
		if n := f.count(`SELECT count(*) FROM supervisor.line_revisions WHERE object_id=$1`, first.DraftID); n != 1 {
			t.Fatalf("promotion replay wrote %d line revisions", n)
		}
		// Discard closes a draft without touching configuration.
		closed := f.createDraft(f.lead, lineDraft("Scrap", true))
		f.expect(f.call(f.viewer, "POST", drafts+"/"+closed.DraftID+"/discards", uuid.NewString(), `"1"`, `{}`), 403, "PERMISSION_DENIED")
		if out := f.draft(f.call(f.lead, "POST", drafts+"/"+closed.DraftID+"/discards", uuid.NewString(), `"1"`, `{}`), 200); out.Status != workstation.DraftDiscarded {
			t.Fatalf("discard: %+v", out)
		}
		f.expect(f.call(f.owner, "POST", drafts+"/"+closed.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "DRAFT_NOT_OPEN")
	})

	t.Run("citations_carry_forward_only_for_unchanged_fields", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Sorting", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", activeSpan}))
		edited := f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Sorting cell", true), "field_spans": []draftSpanIn{}})), 201)
		if c := edited.Revisions[1].Citations; len(c) != 1 || c[0].Path != "/active" || c[0].SpanID != activeSpan {
			t.Fatalf("revision 2 citations: %+v", c)
		}
		if c := edited.Revisions[0].Citations; len(c) != 2 {
			t.Fatalf("revision 1 citations were rewritten: %+v", c)
		}
		if e := edited.Revisions[1].Editor; e == nil || e.Subject != f.approver.subject || edited.Revisions[1].EditedByKind != workstation.DraftAuthorHuman {
			t.Fatalf("editor: %+v", edited.Revisions[1])
		}
		recited := f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), `"2"`, f.encode(map[string]any{"body": masterLineBody("Sorting cell", false), "field_spans": []draftSpanIn{{"/display_name", nameSpan}}})), 201)
		if c := recited.Revisions[2].Citations; len(c) != 1 || c[0].Path != "/display_name" {
			t.Fatalf("revision 3 citations: %+v", c)
		}
	})

	t.Run("promote_revalidates", func(t *testing.T) {
		f.t = t
		line := masterCreateLine(f, f.owner, f.site, uuid.NewString(), masterLineBody("Final", true))
		item := masterCreateItem(f, f.owner, f.site, uuid.NewString(), masterItemBody("Widget", true))
		station := uuid.NewString()
		masterCreateProfile(f, f.owner, f.site, uuid.NewString(), masterProfileBody(station, line.ID, "Press"))
		route := func(ws string) map[string]any {
			return map[string]any{"kind": "routing", "body": workstation.RoutingBodyV1{SchemaVersion: 1, DisplayName: "Widget route", Active: true, LineID: line.ID, ItemID: item.ID, Steps: []workstation.RoutingStepV1{{StepNumber: 1, WorkstationID: ws}}}, "field_spans": []draftSpanIn{}}
		}
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(route(uuid.NewString()))), 422, "INVALID_CONFIGURATION_DRAFT", "body.steps[0].workstation_id")
		d := f.createDraft(f.lead, route(station))
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/lines/"+line.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(masterLineBody("Final", false))), 201, "")
		f.expectPath(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 422, "INVALID_CONFIGURATION_DRAFT", "body.line_id")
		if n := f.count(`SELECT count(*) FROM supervisor.routing_heads WHERE id=$1`, d.DraftID); n != 0 {
			t.Fatal("refused routing promotion wrote a routing")
		}
		// A revise draft binds the target revision it was written against.
		target, expected := item.ID, int64(1)
		stale := f.createDraft(f.lead, map[string]any{"kind": "item", "target_object_id": target, "target_expected_revision": expected, "body": masterItemBody("Widget v2", true), "field_spans": []draftSpanIn{}})
		f.expect(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(map[string]any{"kind": "item", "target_object_id": target, "target_expected_revision": 2, "body": masterItemBody("Widget v3", true), "field_spans": []draftSpanIn{}})), 409, "TARGET_REVISION_CONFLICT")
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/items/"+item.ID+"/revisions", uuid.NewString(), `"1"`, f.encode(masterItemBody("Widget direct", true))), 201, "")
		// A stale target is not a stale draft: the draft's If-Match is current, so it is 409 and
		// the draft must be discarded and recreated.
		f.expect(f.call(f.owner, "POST", drafts+"/"+stale.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "TARGET_REVISION_CONFLICT")
		if n := f.count(`SELECT count(*) FROM supervisor.item_revisions WHERE object_id=$1`, item.ID); n != 2 {
			t.Fatalf("stale promotion: %d item revisions", n)
		}
		current := f.createDraft(f.lead, map[string]any{"kind": "item", "target_object_id": target, "target_expected_revision": 2, "body": masterItemBody("Widget v3", true), "field_spans": []draftSpanIn{}})
		if out := f.draft(f.call(f.owner, "POST", drafts+"/"+current.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 201); out.Promotion.ObjectID != item.ID || out.Promotion.ObjectRevision != 3 {
			t.Fatalf("revise promotion: %+v", out.Promotion)
		}
	})

	t.Run("workstation_profile", func(t *testing.T) {
		f.t = t
		station := uuid.NewString()
		profile := func(target any, body workstation.StationProfileBodyV1) map[string]any {
			out := map[string]any{"kind": "workstation_profile", "body": body, "field_spans": []draftSpanIn{{"/display_name", nameSpan}}}
			if target != nil {
				out["target_object_id"], out["target_expected_revision"] = target, 0
			}
			return out
		}
		body := masterProfileBody(station, uuid.NewString(), "Drill")
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(profile(nil, body))), 422, "INVALID_CONFIGURATION_DRAFT", "target_object_id")
		f.expectPath(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(profile(uuid.NewString(), body))), 422, "INVALID_CONFIGURATION_DRAFT", "body.workstation_id")
		d := f.createDraft(f.lead, profile(station, body))
		out := f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 201)
		if out.Promotion.ObjectID != station || out.Promotion.ObjectRevision != 1 {
			t.Fatalf("profile promotion: %+v", out.Promotion)
		}
		f.expect(f.call(f.operator, "GET", "/v1/sites/"+f.site+"/workstations/"+station+"/profile", "", "", ""), 200, "")
	})

	t.Run("tenancy_and_immutability", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Ledger", true, draftSpanIn{"/display_name", nameSpan}))
		f.refused(f.tenant, "rewrote a draft revision", "42501", "permission denied", `UPDATE supervisor.configuration_draft_revisions SET body_sha256=repeat('0',64) WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "deleted a draft revision", "42501", "permission denied", `DELETE FROM supervisor.configuration_draft_revisions WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "deleted a citation", "42501", "permission denied", `DELETE FROM supervisor.configuration_draft_field_spans WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "deleted a draft", "42501", "permission denied", `DELETE FROM supervisor.configuration_drafts WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "changed a draft's kind", "42501", "permission denied", `UPDATE supervisor.configuration_drafts SET kind='item' WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "promoted without promotion fields", "23514", "", `UPDATE supervisor.configuration_drafts SET status='promoted' WHERE draft_id=$1`, d.DraftID)
		f.refused(f.tenant, "skipped a revision", "23514", "invalid configuration draft transition", `UPDATE supervisor.configuration_drafts SET current_revision=3 WHERE draft_id=$1`, d.DraftID)
		var visible int
		rtx, err := f.runtime.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rtx.Rollback()
		if _, err = rtx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.otherTenant); err != nil {
			t.Fatal(err)
		}
		if err = rtx.QueryRowContext(f.ctx, `SELECT count(*) FROM supervisor.configuration_drafts`).Scan(&visible); err != nil || visible != 0 {
			t.Fatalf("other tenant sees %d drafts (%v)", visible, err)
		}
		if f.count(`SELECT count(*) FROM supervisor.configuration_drafts WHERE draft_id=$1`, d.DraftID) != 1 {
			t.Fatal("control: the draft exists")
		}
	})
	t.Run("stale_citation_recovery", func(t *testing.T) {
		// Review MAJOR 1 regression: on 8007a36 an unchanged body could not drop or re-cite a
		// revoked citation (DRAFT_UNCHANGED), so the draft could never be promoted.
		f.t = t
		unchanged := f.draft(f.call(f.lead, "POST", drafts, uuid.NewString(), "", f.encode(lineDraft("Kitting", true, draftSpanIn{"/display_name", nameSpan}))), 201)
		f.expect(f.call(f.approver, "POST", drafts+"/"+unchanged.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Kitting", true), "field_spans": []draftSpanIn{}})), 409, "DRAFT_UNCHANGED")
		f.expect(f.call(f.approver, "POST", drafts+"/"+unchanged.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Kitting", true), "field_spans": []draftSpanIn{{"/display_name", nameSpan}}})), 409, "DRAFT_UNCHANGED")
		for _, recite := range []bool{false, true} {
			source, stale := f.draftSpan(f.lead, f.site, "Kitting is active")
			d := f.createDraft(f.lead, lineDraft("Kitting", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", stale}))
			f.revokeSource(source)
			f.expect(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "DRAFT_SOURCE_UNAVAILABLE")
			spans, want := []draftSpanIn{}, 1
			if recite {
				spans, want = []draftSpanIn{{"/active", activeSpan}}, 2
			}
			edited := f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Kitting", true), "field_spans": spans})), 201)
			c := edited.Revisions[1].Citations
			if len(c) != want {
				t.Fatalf("recite=%v citations: %+v", recite, c)
			}
			for _, citation := range c {
				if !citation.Available || citation.SpanID == stale {
					t.Fatalf("recite=%v kept a stale citation: %+v", recite, c)
				}
			}
			f.draft(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"2"`, `{}`), 201)
		}
	})

	t.Run("promotion_requires_recent_authentication", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Recent", true))
		f.expect(f.call(f.stale, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 428, "REAUTHENTICATION_REQUIRED")
		if f.count(`SELECT count(*) FROM supervisor.line_heads WHERE id=$1`, d.DraftID) != 0 || f.count(`SELECT count(*) FROM supervisor.configuration_drafts WHERE draft_id=$1 AND status='draft'`, d.DraftID) != 1 {
			t.Fatal("stale-authentication promotion wrote state")
		}
		f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 201)
	})

	t.Run("trigger_refuses_editor_promotion", func(t *testing.T) {
		f.t = t
		d := f.createDraft(f.lead, lineDraft("Backstop", true))
		f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), `"1"`, f.encode(map[string]any{"body": masterLineBody("Backstop 2", true), "field_spans": []draftSpanIn{}})), 201)
		promote := `UPDATE supervisor.configuration_drafts SET status='promoted',promoted_by_issuer=$2,promoted_by_subject=$3,promoted_object_id=$1,promoted_object_revision=1,promoted_at=now() WHERE draft_id=$1`
		for _, who := range []stationSession{f.lead, f.approver} {
			f.refused(f.tenant, "promoted as an editor", "42501", "promoter authored or edited the draft", promote, d.DraftID, f.issuer, who.subject)
		}
		// Control: the same statement by a non-editor passes the trigger (rolled back).
		rtx, err := f.runtime.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer rtx.Rollback()
		if _, err = rtx.ExecContext(f.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = rtx.ExecContext(f.ctx, promote, d.DraftID, f.issuer, f.owner.subject); err != nil {
			t.Fatalf("non-editor promotion refused: %v", err)
		}
	})

	t.Run("reserved_promotion_key", func(t *testing.T) {
		f.t = t
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/lines", "cfgdraft:"+uuid.NewString()+":1", `"0"`, f.encode(masterLineBody("Direct", true))), 422, "INVALID_REQUEST")
		f.expect(f.call(f.owner, "POST", "/v1/sites/"+f.site+"/workstations/"+uuid.NewString()+"/profile-revisions", "cfgdraft:x", `"0"`, f.encode(masterProfileBody(uuid.NewString(), uuid.NewString(), "Direct"))), 422, "INVALID_REQUEST")
		// A write that already used the derived key (below the HTTP check) makes promotion fail
		// closed instead of reporting someone else's object as the promoted one.
		d := f.createDraft(f.lead, lineDraft("Collide", true))
		sum := sha256.Sum256([]byte(f.owner.token))
		owner := workstation.Actor{Issuer: f.issuer, Subject: f.owner.subject, SessionHash: hex.EncodeToString(sum[:])}
		direct, err := workstation.NewMasterData(f.runtime).CreateLine(f.ctx, owner, workstation.Scope{TenantID: f.tenant, SiteID: f.site}, masterLineBody("Collide", true), "cfgdraft:"+d.DraftID+":1", 0)
		if err != nil {
			t.Fatal(err)
		}
		f.expect(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "IDEMPOTENCY_CONFLICT")
		if f.count(`SELECT count(*) FROM supervisor.configuration_drafts WHERE draft_id=$1 AND status='draft'`, d.DraftID) != 1 || f.count(`SELECT count(*) FROM supervisor.line_heads WHERE id=$1`, d.DraftID) != 0 {
			t.Fatalf("collision promoted the draft onto line %s", direct.ID)
		}
		f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 201)
	})
	t.Run("citation_epoch_refresh_and_drops", func(t *testing.T) {
		// Follow-up to 6de461d: a readable source whose access epoch moved is kept and refreshed
		// on edit; every citation not carried forward is reported with its reason.
		f.t = t
		edit := func(d workstation.ConfigurationDraftV1, revision string, body workstation.LineBodyV1) workstation.ConfigurationDraftV1 {
			return f.draft(f.call(f.approver, "POST", drafts+"/"+d.DraftID+"/revisions", uuid.NewString(), revision, f.encode(map[string]any{"body": body, "field_spans": []draftSpanIn{}})), 201)
		}
		citation := func(d workstation.ConfigurationDraftV1, revision int, path string) *workstation.ConfigurationDraftCitationV1 {
			for _, c := range d.Revisions[revision-1].Citations {
				if c.Path == path {
					return &c
				}
			}
			return nil
		}

		// (a) epoch change, no revocation, edit of an unrelated field: kept, refreshed, promotable.
		source, span := f.draftSpan(f.lead, f.site, "Line is called Trim")
		d := f.createDraft(f.lead, lineDraft("Trim", true, draftSpanIn{"/display_name", span}, draftSpanIn{"/active", activeSpan}))
		f.bumpAccessEpoch(source)
		edited := edit(d, `"1"`, masterLineBody("Trim", false))
		if c := citation(edited, 2, "/display_name"); c == nil || !c.Available {
			t.Fatalf("(a) epoch-changed citation not kept: %+v", edited.Revisions[1])
		}
		if dropped := edited.Revisions[1].DroppedCitations; len(dropped) != 1 || dropped[0] != (workstation.ConfigurationDraftDroppedCitationV1{Path: "/active", SpanID: activeSpan, Reason: workstation.CitationFieldChanged}) {
			t.Fatalf("(a) dropped: %+v", dropped)
		}
		f.draft(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"2"`, `{}`), 201)

		// (b) revoked source: dropped and reported.
		source, span = f.draftSpan(f.lead, f.site, "Hemming is active")
		d = f.createDraft(f.lead, lineDraft("Hem", true, draftSpanIn{"/display_name", nameSpan}, draftSpanIn{"/active", span}))
		f.revokeSource(source)
		edited = edit(d, `"1"`, masterLineBody("Hemming", true))
		want := []workstation.ConfigurationDraftDroppedCitationV1{{Path: "/active", SpanID: span, Reason: workstation.CitationSourceRevoked}, {Path: "/display_name", SpanID: nameSpan, Reason: workstation.CitationFieldChanged}}
		if got := edited.Revisions[1].DroppedCitations; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("(b) dropped: %+v", got)
		}
		if len(edited.Revisions[1].Citations) != 0 || len(edited.Revisions[0].DroppedCitations) != 0 {
			t.Fatalf("(b) revisions: %+v", edited.Revisions)
		}

		// (c) epoch change with no edit: promotion fails closed and the read names the reason;
		// an edit (even with an unchanged body) refreshes the citation and promotion succeeds.
		source, span = f.draftSpan(f.lead, f.site, "Line is called Seal")
		d = f.createDraft(f.lead, lineDraft("Seal", true, draftSpanIn{"/display_name", span}))
		f.bumpAccessEpoch(source)
		f.expect(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"1"`, `{}`), 409, "DRAFT_SOURCE_UNAVAILABLE")
		read := f.draft(f.call(f.owner, "GET", drafts+"/"+d.DraftID, "", "", ""), 200)
		if c := citation(read, 1, "/display_name"); c == nil || c.Available || c.UnavailableReason != workstation.CitationAccessChanged {
			t.Fatalf("(c) citation: %+v", read.Revisions[0].Citations)
		}
		refreshed := edit(d, `"1"`, masterLineBody("Seal", true))
		if c := citation(refreshed, 2, "/display_name"); c == nil || !c.Available || len(refreshed.Revisions[1].DroppedCitations) != 0 {
			t.Fatalf("(c) refresh: %+v", refreshed.Revisions[1])
		}
		f.draft(f.call(f.owner, "POST", drafts+"/"+d.DraftID+"/promotions", uuid.NewString(), `"2"`, `{}`), 201)
	})
}
