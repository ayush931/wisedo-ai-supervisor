//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/outcome"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/tasks"
	"github.com/google/uuid"
)

// TestOutcomeObservationAndCloseHTTPPostgres measures task outcomes on the real WIS-272 path. The
// pipeline's windows (100 expected each) delivered 80, 80, 80, 80 and 95; the plan ends after the
// fifth. A completion's next two complete productive windows decide the outcome; the observation
// is immutable, and a supervisor closes the case only once the newest task's outcome is recorded.
// The case, task and completion rows are planted at chosen times (they are produced by the WIS-277
// executor and lifecycle, which have their own tests); the measurement itself is real.
func TestOutcomeObservationAndCloseHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	p := newShortfallPipeline(t, ctx)
	planted := func(statements ...string) {
		t.Helper()
		tx, err := p.control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, s := range append([]string{`SET LOCAL session_replication_role = replica`, `SELECT set_config('supervisor.tenant_id','` + p.tenant + `',true)`}, statements...) {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	scalar := func(q string, args ...any) string {
		t.Helper()
		tx, err := p.control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, p.tenant); err != nil {
			t.Fatal(err)
		}
		var v string
		if err = tx.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}

	// One case at observing, bound to the second window's context.
	bound := p.contexts[1]
	caseID := uuid.NewString()
	steps := []string{"none", "detected", "investigating", "proposed", "awaiting_approval", "executing", "observing"}
	stmts := []string{`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('` + p.tenant + `','` + p.site + `','` + caseID + `','` + p.asset.EntityID + `','` + p.order.EntityID + `','production_shortfall',1,6,'` + bound.ContextID + `',1,'` + bound.Digest + `','observing','` + bound.Window.End + `','` + bound.Window.End + `','case:` + p.tenant + `:` + caseID + `')`}
	for i := 1; i < len(steps); i++ {
		stmts = append(stmts, `INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+caseID+`',`+strconv.Itoa(i)+`,'`+steps[i-1]+`','`+steps[i]+`','fixture','svc','fixture')`)
	}
	planted(stmts...)
	// completedFor plants a completed task of a case whose first completion happened at `at`.
	completedFor := func(caseID, at string) string {
		t.Helper()
		action, task := uuid.NewString(), uuid.NewString()
		planted(
			`INSERT INTO supervisor.approved_actions(tenant_id,site_id,id,proposal_id,proposal_revision,action_kind,canonical_payload_sha256,case_id,state,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+action+`','`+uuid.NewString()+`',1,'maintenance_investigation.create','`+strings.Repeat("a", 64)+`','`+caseID+`','committed','supervisor:executor','fixture')`,
			`INSERT INTO supervisor.tasks(tenant_id,site_id,id,action_id,case_id,asset_id,line_id,order_id,assignee_actor_id,assignee_issuer,assignee_subject,template,description,due_at,state,revision,completed_at) VALUES('`+p.tenant+`','`+p.site+`','`+task+`','`+action+`','`+caseID+`','`+p.asset.EntityID+`','`+p.asset.EntityID+`','`+p.order.EntityID+`','`+uuid.NewString()+`','`+p.issuer+`','operator','inspect_report_only','Inspect','2026-09-28T00:00:00Z','completed',4,'`+at+`')`,
			`INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+task+`',1,NULL,'assigned','create','svc','fixture'),('`+p.tenant+`','`+p.site+`','`+task+`',2,'assigned','acknowledged','acknowledge','svc','fixture'),
 ('`+p.tenant+`','`+p.site+`','`+task+`',3,'acknowledged','in_progress','start','svc','fixture'),('`+p.tenant+`','`+p.site+`','`+task+`',4,'in_progress','completed','complete','svc','fixture')`,
			`INSERT INTO supervisor.task_completions(tenant_id,site_id,task_id,epoch,task_revision,completion_note,actor_issuer,actor_subject,created_at) VALUES('`+p.tenant+`','`+p.site+`','`+task+`',1,4,'Inspected','`+p.issuer+`','operator','`+at+`')`)
		return task
	}
	completed := func(at string) string { t.Helper(); return completedFor(caseID, at) }
	// plantCase plants a case at state on the pipeline's line, bound to contextID, with its history.
	plantCase := func(order, contextID, digest, end, state string) string {
		t.Helper()
		id := uuid.NewString()
		chain := []string{"none", "detected", "investigating", "proposed", "awaiting_approval", "executing", "observing"}
		if state == "escalated" {
			chain = append(chain, "escalated")
		}
		q := []string{`INSERT INTO supervisor.cases(tenant_id,site_id,id,line_id,order_id,exception_kind,episode,revision,context_snapshot_id,context_revision,context_digest,state,detected_at,latest_window_end,workflow_id)
 VALUES('` + p.tenant + `','` + p.site + `','` + id + `','` + p.asset.EntityID + `','` + order + `','production_shortfall',1,` + strconv.Itoa(len(chain)-1) + `,'` + contextID + `',1,'` + digest + `','` + state + `','` + end + `','` + end + `','case:` + p.tenant + `:` + id + `')`}
		for i := 1; i < len(chain); i++ {
			q = append(q, `INSERT INTO supervisor.case_transitions(tenant_id,site_id,case_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+id+`',`+strconv.Itoa(i)+`,'`+chain[i-1]+`','`+chain[i]+`','fixture','fixture','svc','fixture')`)
		}
		planted(q...)
		return id
	}
	service := tasks.NewService(p.db.DB)
	observe := func() map[string]outcome.Status {
		t.Helper()
		done, err := service.ObserveOutcomes(ctx, p.tenant, 50)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]outcome.Status{}
		for _, o := range done {
			out[o.TaskID] = o.Status
		}
		return out
	}
	read := func(task string) tasks.TaskV1 {
		t.Helper()
		w := p.call(p.viewerS, "GET", "/v1/sites/"+p.site+"/tasks/"+task, "", "", "")
		p.expect(w, 200, "")
		var v tasks.TaskV1
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	deficits := func(o *tasks.OutcomeV1) []string {
		var out []string
		for _, w := range o.Windows {
			d := "-"
			if w.Deficit != nil {
				d = *w.Deficit
			}
			out = append(out, w.Start+"="+w.Readiness+":"+d)
		}
		return out
	}

	// Completed inside 00:30-00:45: the windows from 00:45 (deficit 20) and 01:00 (deficit 5).
	noImprovement := completed("2026-09-27T00:37:00Z")
	// Completed inside 00:45-01:00: 01:00 (deficit 5) counts, then the plan ends and no
	// trustworthy window follows within two hours.
	insufficient := completed("2026-09-27T00:50:00Z")
	// Reopened after its completion: there is no completion to observe until the next one.
	reopened := completed("2026-09-27T00:37:00Z")
	planted(`UPDATE supervisor.tasks SET state='in_progress',revision=5,completed_at=NULL WHERE id='`+reopened+`'`,
		`INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+reopened+`',5,'completed','in_progress','reopen','Correction','svc','fixture')`)
	got := observe()
	if got[noImprovement] != outcome.NoImprovement || got[insufficient] != outcome.InsufficientData {
		t.Fatalf("observations %+v", got)
	}
	if _, seen := got[reopened]; seen || scalar(`SELECT count(*)::text FROM supervisor.outcome_observations WHERE task_id=$1`, reopened) != "0" {
		t.Fatalf("a reopened task's superseded completion was observed: %+v", got)
	}
	if v := read(reopened); v.Outcome != nil {
		t.Fatalf("a reopened task shows an outcome: %+v", v.Outcome)
	}
	v := read(noImprovement)
	if v.Outcome == nil || v.Outcome.Status != outcome.NoImprovement || v.Outcome.ObservedAt == nil || v.Outcome.RuleVersion != outcome.RuleVersion ||
		strings.Join(deficits(v.Outcome), ",") != "2026-09-27T00:45:00Z=READY:20,2026-09-27T01:00:00Z=READY:5" || len(v.Outcome.Windows[0].RecordIDs) == 0 {
		t.Fatalf("no-improvement outcome %+v", v.Outcome)
	}
	if v.Outcome.CaseID == nil || *v.Outcome.CaseID != caseID || v.Outcome.BaselineContextID == nil || *v.Outcome.BaselineContextID != bound.ContextID {
		t.Fatalf("outcome must name its case and baseline: %+v", v.Outcome)
	}
	if v := read(insufficient); v.Outcome == nil || v.Outcome.Status != outcome.InsufficientData || len(v.Outcome.Windows) != 7 || v.Outcome.Windows[0].Readiness != "READY" || v.Outcome.Windows[1].Readiness == "READY" {
		t.Fatalf("insufficient outcome %+v", v.Outcome)
	}
	// Assessing windows never creates contexts (no case triggers from the observer).
	if n := scalar(`SELECT count(*)::text FROM supervisor.plant_contexts`); n != strconv.Itoa(len(p.contexts)) {
		t.Fatalf("observer created contexts: %s", n)
	}

	// The source corrects the 00:45 window to 96 delivered (deficit 4): a later completion at the
	// same time now observes recovery, and the earlier observation is not re-decided.
	p.correctDelta(t, 3, "96", 2)
	recovered := completed("2026-09-27T00:37:00Z")
	if got := observe(); got[recovered] != outcome.ObservedImprovement || len(got) != 1 {
		t.Fatalf("after the correction: %+v", got)
	}
	if v := read(recovered); !strings.Contains(v.Outcome.Interpretation, "Threshold recovery") || strings.Join(deficits(v.Outcome), ",") != "2026-09-27T00:45:00Z=READY:4,2026-09-27T01:00:00Z=READY:5" {
		t.Fatalf("recovered outcome %+v", v.Outcome)
	}
	if v := read(noImprovement); v.Outcome.Status != outcome.NoImprovement {
		t.Fatalf("a recorded observation changed: %+v", v.Outcome)
	}

	// A completion whose windows have not ended is pending, and the case cannot be closed on it.
	waiting := completed(time.Now().UTC().Format(time.RFC3339Nano))
	if got := observe(); got[waiting] != outcome.Pending {
		t.Fatalf("future windows: %+v", got)
	}
	if v := read(waiting); v.Outcome == nil || v.Outcome.Status != outcome.Pending || v.Outcome.ObservedAt != nil {
		t.Fatalf("pending outcome %+v", v.Outcome)
	}
	closeCase := func(actor pipelineSession, reason string) cases.CaseV1 {
		t.Helper()
		rev := scalar(`SELECT revision::text FROM supervisor.cases WHERE id=$1`, caseID)
		w := p.call(actor, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", uuid.NewString(), `"`+rev+`"`, `{"expected_revision":`+rev+`,"reason":"`+reason+`"}`)
		if w.Code != 200 {
			p.expect(w, 409, "TRANSITION_GUARD_FAILED")
			return cases.CaseV1{}
		}
		return p.decodeCase(w)
	}
	if c := closeCase(p.leadS, "Inspection reported"); c.ID != "" {
		t.Fatal("a case closed while its newest task's outcome is pending")
	}
	if w := p.call(p.staleS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", uuid.NewString(), `"6"`, `{"expected_revision":6,"reason":"x"}`); w.Code != 428 {
		t.Fatalf("stale sign-in: HTTP %d %s", w.Code, w.Body.String())
	}
	if w := p.call(p.viewerS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", uuid.NewString(), `"6"`, `{"expected_revision":6,"reason":"x"}`); w.Code != 403 {
		t.Fatalf("viewer: HTTP %d %s", w.Code, w.Body.String())
	}

	// The newest task's outcome is recorded: the supervisor closes, and the outcome is untouched.
	final := completed("2026-09-27T00:37:00Z")
	if got := observe(); got[final] != outcome.ObservedImprovement {
		t.Fatalf("final task: %+v", got)
	}
	// Review of #191, minor 2: the close route's own boundaries.
	rev := scalar(`SELECT revision::text FROM supervisor.cases WHERE id=$1`, caseID)
	closeBody := `{"expected_revision":` + rev + `,"reason":"Belt re-tensioned by engineering; recovery observed"}`
	p.expect(p.call(p.outsiderS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", uuid.NewString(), `"`+rev+`"`, closeBody), 404, "NOT_FOUND")
	stale := strconv.Itoa(mustAtoi(t, rev) - 1)
	p.expect(p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", uuid.NewString(), `"`+stale+`"`, `{"expected_revision":`+stale+`,"reason":"x"}`), 412, "REVISION_CONFLICT")
	key := uuid.NewString()
	w := p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", key, `"`+rev+`"`, closeBody)
	p.expect(w, 200, "")
	c := p.decodeCase(w)
	replay := p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", key, `"`+rev+`"`, closeBody)
	p.expect(replay, 200, "")
	if again := p.decodeCase(replay); again.Revision != c.Revision || !again.Replayed {
		t.Fatalf("same key and body must replay the close: %+v", again)
	}
	p.expect(p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+caseID+"/close", key, `"`+rev+`"`, `{"expected_revision":`+rev+`,"reason":"Another reason"}`), 409, "IDEMPOTENCY_CONFLICT")
	if c.State != cases.Resolved || c.ResolutionReason == nil || *c.ResolutionReason != "Belt re-tensioned by engineering; recovery observed" {
		t.Fatalf("closed case %+v", c)
	}
	if v := read(final); v.Outcome.Status != outcome.ObservedImprovement || strings.Contains(v.Outcome.Interpretation, "re-tensioned") {
		t.Fatalf("the closure reason reached the measured outcome: %+v", v.Outcome)
	}
	if n := scalar(`SELECT count(*)::text FROM supervisor.case_outbox WHERE case_id=$1 AND case_revision=7`, caseID); n != "1" {
		t.Fatalf("close signals the case workflow once, got %s", n)
	}

	// The observation is immutable: the runtime role has no UPDATE, and even the owner, with the
	// tenant set and triggers on, is stopped by the trigger (review of #191, minor 1).
	if _, err := p.db.DB.ExecContext(ctx, `UPDATE supervisor.outcome_observations SET status='observed_improvement' WHERE task_id=$1`, noImprovement); err == nil {
		t.Fatal("the runtime role must not rewrite an observation")
	}
	func() {
		tx, err := p.control.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, p.tenant); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE supervisor.outcome_observations SET status='observed_improvement' WHERE task_id=$1`, noImprovement); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("the immutability trigger must stop an owner's rewrite: %v", err)
		}
	}()

	// Review of #191, M3: a window that can never be assessed (here the case's order is not a known
	// entity) is NOT_READY with its reason; the observer records insufficient_data instead of
	// failing on every tick, and the case can be closed on it.
	unknownOrder, ghostContext := uuid.NewString(), uuid.NewString()
	planted(`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 SELECT tenant_id,site_id,'` + ghostContext + `','` + unknownOrder + `',asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,
 jsonb_set(jsonb_set(snapshot,'{context_id}',to_jsonb('` + ghostContext + `'::text)),'{order_id}',to_jsonb('` + unknownOrder + `'::text)),digest,actor_issuer,actor_subject,evaluated_at
 FROM supervisor.plant_contexts WHERE id='` + bound.ContextID + `'`)
	// Review of #191 N2: on the shared read path every caller (cases, approvals, dispatch) sees
	// such a context as not current with INPUTS_UNVERIFIABLE, never as an error or as eligible.
	cw := p.call(p.leadS, "GET", "/v1/sites/"+p.site+"/contexts/"+ghostContext, "", "", "")
	p.expect(cw, 200, "")
	var ghostView plantcontext.View
	if err := json.Unmarshal(cw.Body.Bytes(), &ghostView); err != nil {
		t.Fatal(err)
	}
	if ghostView.CurrentStatus.Eligible || !slices.Contains(ghostView.CurrentStatus.Reasons, "INPUTS_UNVERIFIABLE") {
		t.Fatalf("a context whose order left the registry: %+v", ghostView.CurrentStatus)
	}
	ghostCase := plantCase(unknownOrder, ghostContext, bound.Digest, bound.Window.End, "escalated")
	ghostTask := completedFor(ghostCase, "2026-09-27T00:37:00Z")
	done, err := service.ObserveOutcomes(ctx, p.tenant, 50)
	if err != nil {
		t.Fatalf("an unassessable window must not fail the observer: %v", err)
	}
	var observed outcome.Status
	for _, o := range done {
		if o.TaskID == ghostTask {
			observed = o.Status
		}
	}
	gv := read(ghostTask)
	if observed != outcome.InsufficientData || gv.Outcome == nil || len(gv.Outcome.Windows) == 0 || strings.Join(gv.Outcome.Windows[0].ReasonCodes, ",") != "UNKNOWN_ENTITY" || gv.Outcome.Windows[0].ContextDigest != nil {
		t.Fatalf("unassessable windows: observed %s outcome %+v", observed, gv.Outcome)
	}
	// An escalated case closes on an insufficient_data outcome: missing data does not trap it.
	grev := scalar(`SELECT revision::text FROM supervisor.cases WHERE id=$1`, ghostCase)
	gw := p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+ghostCase+"/close", uuid.NewString(), `"`+grev+`"`, `{"expected_revision":`+grev+`,"reason":"No usable data; closing after review"}`)
	p.expect(gw, 200, "")
	if gc := p.decodeCase(gw); gc.State != cases.Resolved {
		t.Fatalf("escalated case close: %+v", gc)
	}

	// Review of #191 N1: a baseline that can never be loaded (here a stored context whose body no
	// longer decodes, an integrity failure) is recorded once the horizon has closed, with the reason, instead of
	// erroring on every tick and holding the first slots of each observer batch.
	corrupt := uuid.NewString()
	planted(`INSERT INTO supervisor.plant_contexts(tenant_id,site_id,id,order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,snapshot,digest,actor_issuer,actor_subject,evaluated_at)
 SELECT tenant_id,site_id,'` + corrupt + `',order_id,asset_id,window_start,window_end,process_snapshot_id,process_snapshot_sha256,readiness,
 jsonb_set(jsonb_set(snapshot,'{context_id}',to_jsonb('` + corrupt + `'::text)),'{source_record_refs}','"not a list"'),digest,actor_issuer,actor_subject,evaluated_at
 FROM supervisor.plant_contexts WHERE id='` + bound.ContextID + `'`)
	corruptCase := plantCase(uuid.NewString(), corrupt, bound.Digest, bound.Window.End, "observing")
	corruptTask := completedFor(corruptCase, "2026-09-27T00:37:00Z")
	if got := observe(); got[corruptTask] != outcome.InsufficientData {
		t.Fatalf("an unloadable baseline: %+v, want insufficient_data", got[corruptTask])
	}
	if v := read(corruptTask); v.Outcome == nil || !strings.Contains(v.Outcome.Interpretation, "BASELINE_INTEGRITY_FAILED") || len(v.Outcome.Windows) != 0 {
		t.Fatalf("the unloadable baseline's reason must be recorded: %+v", v.Outcome)
	}

	// A case whose newest task was canceled has no outcome to close on: refused.
	canceledCase := plantCase(uuid.NewString(), bound.ContextID, bound.Digest, bound.Window.End, "observing") // its own order: one episode per line and order
	action, task := uuid.NewString(), uuid.NewString()
	planted(
		`INSERT INTO supervisor.approved_actions(tenant_id,site_id,id,proposal_id,proposal_revision,action_kind,canonical_payload_sha256,case_id,state,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+action+`','`+uuid.NewString()+`',1,'maintenance_investigation.create','`+strings.Repeat("a", 64)+`','`+canceledCase+`','committed','supervisor:executor','fixture')`,
		`INSERT INTO supervisor.tasks(tenant_id,site_id,id,action_id,case_id,asset_id,line_id,order_id,assignee_actor_id,assignee_issuer,assignee_subject,template,description,due_at,state,revision) VALUES('`+p.tenant+`','`+p.site+`','`+task+`','`+action+`','`+canceledCase+`','`+p.asset.EntityID+`','`+p.asset.EntityID+`','`+scalar(`SELECT order_id::text FROM supervisor.cases WHERE id=$1`, canceledCase)+`','`+uuid.NewString()+`','`+p.issuer+`','operator','inspect_report_only','Inspect','2026-09-28T00:00:00Z','canceled',2)`,
		`INSERT INTO supervisor.task_events(tenant_id,site_id,task_id,revision,from_state,to_state,event,reason,actor_issuer,actor_subject) VALUES('`+p.tenant+`','`+p.site+`','`+task+`',1,NULL,'assigned','create',NULL,'svc','fixture'),('`+p.tenant+`','`+p.site+`','`+task+`',2,'assigned','canceled','cancel','Line down','svc','fixture')`)
	crev := scalar(`SELECT revision::text FROM supervisor.cases WHERE id=$1`, canceledCase)
	p.expect(p.call(p.leadS, "POST", "/v1/sites/"+p.site+"/cases/"+canceledCase+"/close", uuid.NewString(), `"`+crev+`"`, `{"expected_revision":`+crev+`,"reason":"Nothing to observe"}`), 409, "TRANSITION_GUARD_FAILED")
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
