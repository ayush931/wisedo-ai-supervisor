//go:build integration

package oeeservice

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Each case below is named after the reference it proves: the WIS-229 acceptance list and
// ADR 0002 Sec. 7, 12, 16 and 18. The dataset and policy are the ADR 0005 Sec. 9 fixtures.

func fixture(t *testing.T, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "oee", "testdata", "service", name))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func dataset(t *testing.T, mutate func(*oee.OeeDatasetV1)) oee.OeeDatasetV1 {
	t.Helper()
	var d oee.OeeDatasetV1
	fixture(t, "dataset_adr0005.json", &d)
	if mutate != nil {
		mutate(&d)
	}
	digest, err := oee.DatasetProvenanceDigest(d)
	if err != nil {
		t.Fatal(err)
	}
	d.ProvenanceDigest = digest
	return d
}

func sessionHash() string {
	h := sha256.Sum256([]byte(uuid.NewString()))
	return hex.EncodeToString(h[:])
}

func TestOEEServicePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	adminURL, runtimeURL := os.Getenv("SUPERVISOR_TEST_ADMIN_URL"), os.Getenv("SUPERVISOR_TEST_DATABASE_URL")
	if adminURL == "" || runtimeURL == "" {
		t.Fatal("disposable PostgreSQL admin/runtime URLs required")
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
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if e := admin.QueryRowContext(ctx, q, args...).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	scope := Scope{uuid.NewString(), uuid.NewString()}
	other := Scope{uuid.NewString(), uuid.NewString()}
	for _, sc := range []Scope{scope, other} {
		exec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic oee test')`, sc.TenantID)
		exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'synthetic oee line')`, sc.TenantID, sc.SiteID)
	}
	emptySite := Scope{scope.TenantID, uuid.NewString()}
	exec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'site without policy')`, emptySite.TenantID, emptySite.SiteID)
	actor := func(sc Scope, role string) Actor {
		t.Helper()
		a := Actor{"https://oee.example/realm", uuid.NewString(), sessionHash()}
		exec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor, operator, viewer := actor(scope, "owner"), actor(scope, "supervisor"), actor(scope, "operator"), actor(scope, "viewer")
	foreign := actor(other, "supervisor")
	exec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner')`, emptySite.TenantID, emptySite.SiteID, owner.Issuer, owner.Subject)

	limits := Config{RunDeadline: time.Minute, LeaseDuration: 15 * time.Second, MaxOpenRuns: 64, MaxConcurrentExecutions: 8, IdempotencyRetention: 24 * time.Hour, ExecutionOwner: "oee-integration"}
	s, err := New(runtime, limits)
	if err != nil {
		t.Fatal(err)
	}
	var policyBody oee.OeePolicyBodyV1
	fixture(t, "policy.json", &policyBody)
	policyID := uuid.NewString()
	revisionInput := oee.OeePolicyRevisionInputV1{PolicyCode: "line-oee", EffectiveStart: "2026-01-01T00:00:00Z", Policy: policyBody}

	var policy oee.OeePolicyV1
	t.Run("ADR0002 15.1 separate authoring and publication permissions", func(t *testing.T) {
		for _, a := range []Actor{operator, viewer} {
			if _, e := s.CreateRevision(ctx, a, scope, policyID, revisionInput, "denied-"+a.Subject, "c"); !errors.Is(e, ErrDenied) {
				t.Fatalf("authoring by non-author role: %v", e)
			}
		}
		if policy, err = s.CreateRevision(ctx, owner, scope, policyID, revisionInput, "revise-1", "c"); err != nil {
			t.Fatal(err)
		}
		if len(policy.Revisions) != 1 || policy.Revisions[0].RevisionNo != 1 || policy.CurrentPublication != nil {
			t.Fatalf("revision: %+v", policy)
		}
		if _, e := s.Publish(ctx, owner, scope, policyID, policy.Revisions[0].PolicyRevisionID, policy.RowVersion, "publish-owner", "c"); !errors.Is(e, ErrDenied) {
			t.Fatalf("publication by owner: %v", e)
		}
	})
	t.Run("ADR0002 12 optimistic publication conflict and idempotent replay", func(t *testing.T) {
		revisionID := policy.Revisions[0].PolicyRevisionID
		if _, e := s.Publish(ctx, supervisor, scope, policyID, revisionID, policy.RowVersion+1, "publish-stale", "c"); !errors.Is(e, ErrRevision) {
			t.Fatalf("stale If-Match: %v", e)
		}
		published, e := s.Publish(ctx, supervisor, scope, policyID, revisionID, policy.RowVersion, "publish-1", "c")
		if e != nil || published.CurrentPublication == nil || published.CurrentPublication.PolicyRevisionID != revisionID || published.RowVersion != policy.RowVersion+1 {
			t.Fatalf("publish: %+v %v", published, e)
		}
		replayed, e := s.Publish(ctx, supervisor, scope, policyID, revisionID, policy.RowVersion, "publish-1", "c")
		if e != nil || !replayed.Replayed || replayed.CurrentPublication.PublicationID != published.CurrentPublication.PublicationID {
			t.Fatalf("publish replay: %+v %v", replayed, e)
		}
		if _, e = s.Publish(ctx, supervisor, scope, policyID, revisionID, published.RowVersion, "publish-1", "c"); !errors.Is(e, ErrConflict) {
			t.Fatalf("publish key reuse: %v", e)
		}
		if again, e := s.CreateRevision(ctx, owner, scope, policyID, revisionInput, "revise-1", "c"); e != nil || !again.Replayed {
			t.Fatalf("revision replay: %+v %v", again, e)
		}
		policy = published
	})
	t.Run("ADR0002 16 overlapping effective policies fail", func(t *testing.T) {
		otherPolicy := uuid.NewString()
		in := oee.OeePolicyRevisionInputV1{PolicyCode: "overlapping", EffectiveStart: "2026-06-01T00:00:00Z", EffectiveEnd: "2027-01-01T00:00:00Z", Policy: policyBody}
		created, e := s.CreateRevision(ctx, owner, scope, otherPolicy, in, "revise-overlap", "c")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Publish(ctx, supervisor, scope, otherPolicy, created.Revisions[0].PolicyRevisionID, created.RowVersion, "publish-overlap", "c"); !errors.Is(e, ErrPolicyOverlap) {
			t.Fatalf("overlap: %v", e)
		}
	})
	t.Run("ADR0002 5 stage 3 missing policy is not ready and creates no run", func(t *testing.T) {
		if _, e := s.Accept(ctx, owner, emptySite, oee.OeeCalculationRequestV1{Dataset: dataset(t, nil)}, "no-policy", "c"); !errors.Is(e, ErrPolicyNotReady) {
			t.Fatalf("missing policy: %v", e)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_calculation_run WHERE site_id=$1`, emptySite.SiteID); n != 0 {
			t.Fatalf("runs without policy: %d", n)
		}
	})

	d := dataset(t, nil)
	var run oee.OeeCalculationRunV1
	t.Run("ADR0002 7 accept execute seal candidate evidence", func(t *testing.T) {
		if _, e := s.Accept(ctx, viewer, scope, oee.OeeCalculationRequestV1{Dataset: d}, "viewer", "c"); !errors.Is(e, ErrDenied) {
			t.Fatalf("viewer calculation: %v", e)
		}
		if run, err = s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 1}, "calc-1", "c"); err != nil {
			t.Fatal(err)
		}
		if run.Status != oee.RunAccepted || run.Policy.PolicyID != policyID || run.Policy.Revision != 1 || run.ResultStatus != "" {
			t.Fatalf("accepted: %+v", run)
		}
		if err = s.Execute(ctx, scope, run.RunID, d, "c"); err != nil {
			t.Fatal(err)
		}
		done, e := s.GetRun(ctx, viewer, scope, run.RunID, "c")
		if e != nil || done.Status != oee.RunSucceeded || done.ResultStatus != oee.StatusReady || done.TruthState != oee.TruthStateCandidate || done.EvidenceID == "" {
			t.Fatalf("sealed run: %+v %v", done, e)
		}
		ev, e := s.GetEvidence(ctx, viewer, scope, done.EvidenceID, "c")
		if e != nil || ev.RunID != run.RunID || ev.KPIs.OEE.Value != "0.711111" || ev.KPIs.TEEP.Value != "0.666667" || ev.TruthState != oee.TruthStateCandidate {
			t.Fatalf("evidence: %+v %v", ev, e)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_run_source WHERE run_id=$1`, run.RunID); n != 1 {
			t.Fatalf("run sources: %d", n)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_audit_entry WHERE resource_id=$1 AND action='evidence.read'`, done.EvidenceID); n != 1 {
			t.Fatalf("evidence read audit: %d", n)
		}
		run = done
	})
	t.Run("WIS-229 same key and body replays, changed body conflicts", func(t *testing.T) {
		again, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 1}, "calc-1", "c")
		if e != nil || !again.Replayed || again.RunID != run.RunID || again.Status != oee.RunSucceeded {
			t.Fatalf("replay: %+v %v", again, e)
		}
		if _, e = s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "calc-1", "c"); !errors.Is(e, ErrConflict) {
			t.Fatalf("changed version: %v", e)
		}
		changed := dataset(t, func(x *oee.OeeDatasetV1) { x.Counts[0].Scrap = "301" })
		if _, e = s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: changed}, "calc-1", "c"); !errors.Is(e, ErrConflict) {
			t.Fatalf("changed body: %v", e)
		}
	})
	t.Run("WIS-229 concurrent duplicate requests create one calculation", func(t *testing.T) {
		var wg sync.WaitGroup
		ids := make(chan string, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "concurrent", "c")
				if e != nil {
					t.Error(e)
					return
				}
				ids <- r.RunID
			}()
		}
		wg.Wait()
		close(ids)
		seen := map[string]bool{}
		for id := range ids {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Fatalf("runs for one key: %v", seen)
		}
		for id := range seen {
			if _, e := s.Cancel(ctx, owner, scope, id, "cancel-concurrent", "c"); e != nil {
				t.Fatal(e)
			}
		}
	})
	t.Run("ADR0002 4.3 expected policy revision is a precondition", func(t *testing.T) {
		if _, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 2}, "stale-policy", "c"); !errors.Is(e, ErrRevision) {
			t.Fatalf("expected revision: %v", e)
		}
	})
	t.Run("ADR0002 9 invalid input fails before side effects", func(t *testing.T) {
		before := count(`SELECT count(*) FROM supervisor.oee_calculation_run`) + count(`SELECT count(*) FROM supervisor.oee_idempotency_key`)
		tampered := d
		tampered.Counts = append([]oee.OeeCountV1(nil), d.Counts...)
		tampered.Counts[0].Scrap = "0"
		if _, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: tampered}, "tampered", "c"); !errors.Is(e, ErrInvalid) {
			t.Fatalf("provenance digest: %v", e)
		}
		if after := count(`SELECT count(*) FROM supervisor.oee_calculation_run`) + count(`SELECT count(*) FROM supervisor.oee_idempotency_key`); after != before {
			t.Fatalf("side effects %d -> %d", before, after)
		}
	})
	t.Run("ADR0002 7.4 18 cancellation and terminal states", func(t *testing.T) {
		accepted, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "cancel-accepted", "c")
		if e != nil {
			t.Fatal(e)
		}
		cancelled, e := s.Cancel(ctx, owner, scope, accepted.RunID, "cancel-1", "c")
		if e != nil || cancelled.Status != oee.RunCancelled || cancelled.FailureCode != "CANCELLED" || cancelled.EvidenceID != "" {
			t.Fatalf("cancel accepted: %+v %v", cancelled, e)
		}
		if e = s.Execute(ctx, scope, accepted.RunID, d, "c"); e != nil {
			t.Fatal(e)
		}
		if _, e = s.Cancel(ctx, owner, scope, run.RunID, "cancel-succeeded", "c"); !errors.Is(e, ErrLifecycle) {
			t.Fatalf("cancel terminal: %v", e)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_evidence_pack WHERE run_id=$1`, accepted.RunID); n != 0 {
			t.Fatalf("evidence for cancelled run: %d", n)
		}
	})
	t.Run("ADR0002 12 cancellation during final seal leaves no evidence", func(t *testing.T) {
		accepted, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "cancel-running", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := s.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		if r, e := s.Cancel(ctx, owner, scope, accepted.RunID, "cancel-2", "c"); e != nil || r.Status != oee.RunRunning || r.CancelGeneration != 1 {
			t.Fatalf("cancel running: %+v %v", r, e)
		}
		body, _ := verifiedPolicy(scope, c.policyJSON, c.ref.Digest)
		ev, e := oee.CalculateDataset(scope.TenantID, scope.SiteID, d, c.ref, body)
		if e != nil {
			t.Fatal(e)
		}
		if sealed, e := s.seal(ctx, scope, accepted.RunID, c, ev, "c"); e != nil || sealed {
			t.Fatalf("seal after cancel: %v %v", sealed, e)
		}
		if e = s.resolveUnsealed(ctx, scope, accepted.RunID, c, "c"); e != nil {
			t.Fatal(e)
		}
		r, _ := s.GetRun(ctx, viewer, scope, accepted.RunID, "c")
		if r.Status != oee.RunCancelled || r.EvidenceID != "" || count(`SELECT count(*) FROM supervisor.oee_run_source WHERE run_id=$1`, accepted.RunID) != 0 {
			t.Fatalf("partial result after cancel: %+v", r)
		}
	})
	t.Run("ADR0002 12 18 late executor past its deadline cannot publish", func(t *testing.T) {
		short := limits
		short.RunDeadline = time.Second
		fast, _ := New(runtime, short)
		accepted, e := fast.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "late", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := fast.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		body, _ := verifiedPolicy(scope, c.policyJSON, c.ref.Digest)
		ev, _ := oee.CalculateDataset(scope.TenantID, scope.SiteID, d, c.ref, body)
		time.Sleep(1200 * time.Millisecond)
		if sealed, e := fast.seal(ctx, scope, accepted.RunID, c, ev, "c"); e != nil || sealed {
			t.Fatalf("late seal: %v %v", sealed, e)
		}
		if e = fast.resolveUnsealed(ctx, scope, accepted.RunID, c, "c"); e != nil {
			t.Fatal(e)
		}
		r, _ := s.GetRun(ctx, viewer, scope, accepted.RunID, "c")
		if r.Status != oee.RunFailed || r.FailureCode != "DEADLINE_EXCEEDED" || r.EvidenceID != "" {
			t.Fatalf("late executor: %+v", r)
		}
		// The database itself refuses success after the deadline and any exit from a terminal state.
		if _, e = admin.ExecContext(ctx, `UPDATE supervisor.oee_calculation_run SET status='SUCCEEDED',result_status='READY',truth_state='CANDIDATE',failure_code=NULL,row_version=row_version+1 WHERE run_id=$1`, accepted.RunID); e == nil {
			t.Fatal("terminal run left its terminal state")
		}
	})
	t.Run("ADR0002 18 abandoned runs are reaped and free capacity", func(t *testing.T) {
		short := limits
		short.RunDeadline = time.Second
		fast, _ := New(runtime, short)
		abandoned, e := fast.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "abandoned", "c")
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(1200 * time.Millisecond)
		if e = fast.Execute(ctx, scope, abandoned.RunID, d, "c"); e != nil {
			t.Fatal(e)
		}
		next, e := fast.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "after-abandoned", "c")
		if e != nil {
			t.Fatal(e)
		}
		r, _ := s.GetRun(ctx, viewer, scope, abandoned.RunID, "c")
		if r.Status != oee.RunFailed || r.FailureCode != "DEADLINE_EXCEEDED" {
			t.Fatalf("reaped: %+v", r)
		}
		_, _ = s.Cancel(ctx, owner, scope, next.RunID, "cancel-after-abandoned", "c")
	})
	t.Run("WIS-229 overload rejects predictably", func(t *testing.T) {
		one := limits
		one.MaxOpenRuns = 1
		tight, _ := New(runtime, one)
		first, e := tight.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "budget-1", "c")
		if e != nil {
			t.Fatal(e)
		}
		before := count(`SELECT count(*) FROM supervisor.oee_calculation_run`)
		if _, e = tight.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "budget-2", "c"); !errors.Is(e, ErrOverloaded) {
			t.Fatalf("over budget: %v", e)
		}
		if after := count(`SELECT count(*) FROM supervisor.oee_calculation_run`); after != before {
			t.Fatalf("overload created a run: %d -> %d", before, after)
		}
		_, _ = s.Cancel(ctx, owner, scope, first.RunID, "cancel-budget", "c")
		if _, e = tight.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "budget-2", "c"); e != nil {
			t.Fatalf("budget released: %v", e)
		}
	})
	t.Run("rule 03 revocation stops queued work", func(t *testing.T) {
		calc := actor(scope, "supervisor")
		accepted, e := s.Accept(ctx, calc, scope, oee.OeeCalculationRequestV1{Dataset: d}, "revoked", "c")
		if e != nil {
			t.Fatal(e)
		}
		exec(`UPDATE supervisor.memberships SET active=false WHERE issuer=$1 AND subject=$2`, calc.Issuer, calc.Subject)
		if e = s.Execute(ctx, scope, accepted.RunID, d, "c"); e != nil {
			t.Fatal(e)
		}
		r, _ := s.GetRun(ctx, viewer, scope, accepted.RunID, "c")
		if r.Status != oee.RunFailed || r.FailureCode != "ACCESS_REVOKED" || r.EvidenceID != "" {
			t.Fatalf("revoked dispatch: %+v", r)
		}
	})
	t.Run("ADR0002 7.2 18 a lost executor is reaped after one lease, audited, and cannot seal", func(t *testing.T) {
		leased := limits
		leased.LeaseDuration = time.Second
		svc, _ := New(runtime, leased)
		accepted, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "lost-executor", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := svc.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		// The executor disappears: no renewal, no seal. Its lease ends long before the deadline.
		time.Sleep(1200 * time.Millisecond)
		r, e := s.GetRun(ctx, viewer, scope, accepted.RunID, "c")
		if e != nil || r.Status != oee.RunFailed || r.FailureCode != "LEASE_EXPIRED" {
			t.Fatalf("lost executor: %+v %v", r, e)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_audit_entry WHERE resource_id=$1 AND action='calculation.failed' AND reason_code='LEASE_EXPIRED'`, accepted.RunID); n != 1 {
			t.Fatalf("reaper audit rows: %d", n)
		}
		body, _ := verifiedPolicy(scope, c.policyJSON, c.ref.Digest)
		ev, _ := oee.CalculateDataset(scope.TenantID, scope.SiteID, d, c.ref, body)
		if sealed, e := svc.seal(ctx, scope, accepted.RunID, c, ev, "c"); e != nil || sealed {
			t.Fatalf("reaped run sealed: %v %v", sealed, e)
		}
	})
	t.Run("ADR0002 7.4 18 cancelling a run whose executor died ends CANCELLED", func(t *testing.T) {
		leased := limits
		leased.LeaseDuration = time.Second
		svc, _ := New(runtime, leased)
		accepted, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "cancel-lost", "c")
		if e != nil {
			t.Fatal(e)
		}
		if c, e := svc.claim(ctx, scope, accepted.RunID, "c"); e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		if r, e := s.Cancel(ctx, owner, scope, accepted.RunID, "cancel-lost-1", "c"); e != nil || r.Status != oee.RunRunning {
			t.Fatalf("cancel running: %+v %v", r, e)
		}
		time.Sleep(1200 * time.Millisecond)
		r, e := s.GetRun(ctx, viewer, scope, accepted.RunID, "c")
		if e != nil || r.Status != oee.RunCancelled || r.FailureCode != "CANCELLED" || r.EvidenceID != "" {
			t.Fatalf("cancel of lost run: %+v %v", r, e)
		}
	})
	t.Run("ADR0002 7.2 lease renewal keeps a long execution alive", func(t *testing.T) {
		leased := limits
		leased.LeaseDuration = time.Second
		svc, _ := New(runtime, leased)
		accepted, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "renewed", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := svc.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		stop := svc.renewLease(ctx, scope, accepted.RunID, c, func() { t.Error("renewal reported a lost lease") })
		time.Sleep(2500 * time.Millisecond) // two and a half leases
		stop()
		body, _ := verifiedPolicy(scope, c.policyJSON, c.ref.Digest)
		ev, _ := oee.CalculateDataset(scope.TenantID, scope.SiteID, d, c.ref, body)
		if sealed, e := svc.seal(ctx, scope, accepted.RunID, c, ev, "c"); e != nil || !sealed {
			t.Fatalf("renewed run did not seal: %v %v", sealed, e)
		}
	})
	t.Run("ADR0002 7.4 cancelling a running run stops its lease renewal and interrupts it", func(t *testing.T) {
		leased := limits
		leased.LeaseDuration = time.Second
		svc, _ := New(runtime, leased)
		accepted, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "cancel-renewing", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := svc.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		interrupted := make(chan struct{})
		stop := svc.renewLease(ctx, scope, accepted.RunID, c, func() { close(interrupted) })
		defer stop()
		if _, e = s.Cancel(ctx, owner, scope, accepted.RunID, "cancel-renewing-1", "c"); e != nil {
			t.Fatal(e)
		}
		select {
		case <-interrupted:
		case <-time.After(3 * time.Second):
			t.Fatal("renewal kept a cancelled run alive")
		}
		if e = svc.resolveUnsealed(ctx, scope, accepted.RunID, c, "c"); e != nil {
			t.Fatal(e)
		}
		if r, _ := s.GetRun(ctx, viewer, scope, accepted.RunID, "c"); r.Status != oee.RunCancelled {
			t.Fatalf("cancelled running run: %+v", r)
		}
	})
	t.Run("rule 07 a replay answers even when the execution budget is full", func(t *testing.T) {
		single := limits
		single.MaxConcurrentExecutions = 1
		svc, _ := New(runtime, single)
		release, ok := svc.reserveSlot()
		if !ok {
			t.Fatal("slot refused")
		}
		defer release()
		replayed, e := svc.Submit(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d, ExpectedPolicyRevision: 1}, "calc-1", "c")
		if e != nil || !replayed.Replayed || replayed.RunID != run.RunID || replayed.Status != oee.RunSucceeded {
			t.Fatalf("replay under a full budget: %+v %v", replayed, e)
		}
		if _, e = svc.Submit(ctx, viewer, scope, oee.OeeCalculationRequestV1{Dataset: d}, "viewer-full", "c"); !errors.Is(e, ErrDenied) {
			t.Fatalf("unauthorized request under a full budget: %v", e)
		}
	})
	t.Run("migration 030 lease may only be renewed forward, while held, within the deadline", func(t *testing.T) {
		accepted, e := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "lease-guard", "c")
		if e != nil {
			t.Fatal(e)
		}
		c, e := s.claim(ctx, scope, accepted.RunID, "c")
		if e != nil || c == nil {
			t.Fatalf("claim: %v", e)
		}
		for name, q := range map[string]string{
			"backward":       `UPDATE supervisor.oee_calculation_run SET execution_lease_until=execution_lease_until-interval '1 second',row_version=row_version+1 WHERE run_id=$1`,
			"past deadline":  `UPDATE supervisor.oee_calculation_run SET execution_lease_until=deadline_at+interval '1 second',row_version=row_version+1 WHERE run_id=$1`,
			"fence takeover": `UPDATE supervisor.oee_calculation_run SET execution_fence=gen_random_uuid(),row_version=row_version+1 WHERE run_id=$1`,
		} {
			if _, e = admin.ExecContext(ctx, q, accepted.RunID); e == nil {
				t.Errorf("%s lease change accepted", name)
			}
		}
		if e = s.fail(ctx, scope, accepted.RunID, c, statusFailed, codeInternal, "c"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("rule 07 an expired idempotency key is rebound, a live one conflicts", func(t *testing.T) {
		brief := limits
		brief.IdempotencyRetention = time.Second
		svc, _ := New(runtime, brief)
		first, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "expiring", "c")
		if e != nil {
			t.Fatal(e)
		}
		changed := dataset(t, func(x *oee.OeeDatasetV1) { x.Counts[0].Scrap = "299" })
		if _, e = svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: changed}, "expiring", "c"); !errors.Is(e, ErrConflict) {
			t.Fatalf("live key reuse: %v", e)
		}
		if _, e = s.Cancel(ctx, owner, scope, first.RunID, "cancel-expiring", "c"); e != nil {
			t.Fatal(e)
		}
		time.Sleep(1200 * time.Millisecond)
		second, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: changed}, "expiring", "c")
		if e != nil || second.Replayed || second.RunID == first.RunID {
			t.Fatalf("expired key not rebound: %+v %v", second, e)
		}
		if _, e = s.Cancel(ctx, owner, scope, second.RunID, "cancel-expiring-2", "c"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("ADR0002 10 the execution budget rejects before acceptance and drains on close", func(t *testing.T) {
		single := limits
		single.MaxConcurrentExecutions = 1
		svc, _ := New(runtime, single)
		release, ok := svc.reserveSlot()
		if !ok {
			t.Fatal("slot refused")
		}
		before := count(`SELECT count(*) FROM supervisor.oee_calculation_run`)
		if _, e := svc.Submit(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "budget-full", "c"); !errors.Is(e, ErrOverloaded) {
			t.Fatalf("full budget: %v", e)
		}
		if after := count(`SELECT count(*) FROM supervisor.oee_calculation_run`); after != before {
			t.Fatalf("rejected submission created a run: %d -> %d", before, after)
		}
		release()
		run, e := svc.Submit(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "budget-free", "c")
		if e != nil {
			t.Fatal(e)
		}
		if e = svc.Close(ctx); e != nil {
			t.Fatalf("drain: %v", e)
		}
		if r, _ := s.GetRun(ctx, viewer, scope, run.RunID, "c"); r.Status != oee.RunSucceeded {
			t.Fatalf("drained run: %+v", r)
		}
		if _, e = svc.Submit(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "after-close", "c"); !errors.Is(e, ErrOverloaded) {
			t.Fatalf("closed service accepted work: %v", e)
		}
	})
	t.Run("ADR0002 7.1 retrying an accepted run whose executor was lost re-dispatches it once", func(t *testing.T) {
		svc, _ := New(runtime, limits)
		accepted, e := svc.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "redispatch", "c")
		if e != nil {
			t.Fatal(e)
		}
		retried, e := svc.Submit(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, "redispatch", "c")
		if e != nil || !retried.Replayed || retried.RunID != accepted.RunID {
			t.Fatalf("retry: %+v %v", retried, e)
		}
		if e = svc.Close(ctx); e != nil {
			t.Fatal(e)
		}
		if r, _ := s.GetRun(ctx, viewer, scope, accepted.RunID, "c"); r.Status != oee.RunSucceeded || r.EvidenceID == "" {
			t.Fatalf("re-dispatched run: %+v", r)
		}
		if n := count(`SELECT count(*) FROM supervisor.oee_evidence_pack WHERE run_id=$1`, accepted.RunID); n != 1 {
			t.Fatalf("evidence packs: %d", n)
		}
	})
	t.Run("rule 04 sealed evidence digest excludes generated identifiers and verifies on read", func(t *testing.T) {
		var stored string
		if e := admin.QueryRowContext(ctx, `SELECT evidence_sha256 FROM supervisor.oee_evidence_pack WHERE run_id=$1`, run.RunID).Scan(&stored); e != nil {
			t.Fatal(e)
		}
		ev, e := s.GetEvidence(ctx, viewer, scope, run.EvidenceID, "c")
		if e != nil {
			t.Fatal(e)
		}
		want, _ := oee.EvidenceDigest(scope.TenantID, scope.SiteID, ev)
		ev.EvidenceID, ev.RunID, ev.SealedAt = uuid.NewString(), uuid.NewString(), "2030-01-01T00:00:00Z"
		again, _ := oee.EvidenceDigest(scope.TenantID, scope.SiteID, ev)
		if stored != want || again != want {
			t.Fatalf("evidence digest: stored %s want %s regenerated %s", stored, want, again)
		}
	})
	t.Run("WIS-229 cross-tenant direct-RLS pool-reuse and worker default deny", func(t *testing.T) {
		if _, e := s.GetRun(ctx, foreign, other, run.RunID, "c"); !errors.Is(e, ErrNotFound) {
			t.Fatalf("cross-tenant run: %v", e)
		}
		if _, e := s.GetEvidence(ctx, foreign, other, run.EvidenceID, "c"); !errors.Is(e, ErrNotFound) {
			t.Fatalf("cross-tenant evidence: %v", e)
		}
		if _, e := s.GetPolicy(ctx, foreign, other, policyID); !errors.Is(e, ErrNotFound) {
			t.Fatalf("cross-tenant policy: %v", e)
		}
		// A worker scoped to another tenant cannot see, claim or seal the run.
		if e := s.Execute(ctx, other, run.RunID, d, "c"); !errors.Is(e, ErrNotFound) {
			t.Fatalf("worker cross-tenant: %v", e)
		}
		pool, e := sql.Open("pgx", runtimeURL)
		if e != nil {
			t.Fatal(e)
		}
		defer pool.Close()
		pool.SetMaxOpenConns(1)
		tables := []string{"oee_policy", "oee_policy_revision", "oee_policy_publication", "oee_calculation_run", "oee_run_source", "oee_evidence_pack", "oee_idempotency_key", "oee_audit_entry"}
		visible := func(q func(string) *sql.Row) int {
			total := 0
			for _, table := range tables {
				var n int
				if e := q(`SELECT count(*) FROM supervisor.` + table).Scan(&n); e != nil {
					t.Fatal(e)
				}
				total += n
			}
			return total
		}
		// No tenant context: default deny.
		if n := visible(func(q string) *sql.Row { return pool.QueryRowContext(ctx, q) }); n != 0 {
			t.Fatalf("rows without tenant context: %d", n)
		}
		tx, e := pool.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, other.TenantID); e != nil {
			t.Fatal(e)
		}
		if n := visible(func(q string) *sql.Row { return tx.QueryRowContext(ctx, q) }); n != 0 {
			t.Fatalf("other tenant sees rows: %d", n)
		}
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope.TenantID); e != nil {
			t.Fatal(e)
		}
		if n := visible(func(q string) *sql.Row { return tx.QueryRowContext(ctx, q) }); n == 0 {
			t.Fatal("owning tenant sees no rows")
		}
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
		// The same pooled connection keeps no tenant context after commit.
		if n := visible(func(q string) *sql.Row { return pool.QueryRowContext(ctx, q) }); n != 0 {
			t.Fatalf("pooled connection leaked tenant context: %d", n)
		}
		// A cross-tenant reference is unrepresentable.
		if _, e = admin.ExecContext(ctx, `INSERT INTO supervisor.oee_run_source(tenant_id,site_id,run_id,source_ordinal,source_key,owner_service,source_revision,dataset_schema_id,consistency_mode,contribution_sha256,fact_count) VALUES($1,$2,$3,9,'x','x','x','x','NON_REPLAYABLE',repeat('a',64),0)`, other.TenantID, other.SiteID, run.RunID); e == nil {
			t.Fatal("cross-tenant run source accepted")
		}
	})
	t.Run("ADR0002 6.2 append-only tables deny runtime update and delete", func(t *testing.T) {
		tx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, scope.TenantID); e != nil {
			t.Fatal(e)
		}
		for _, q := range []string{
			`UPDATE supervisor.oee_evidence_pack SET superseded_at=now()`,
			`DELETE FROM supervisor.oee_evidence_pack`,
			`UPDATE supervisor.oee_policy_revision SET policy_json='{}'`,
			`DELETE FROM supervisor.oee_calculation_run`,
			`DELETE FROM supervisor.oee_audit_entry`,
		} {
			if _, e = tx.ExecContext(ctx, `SAVEPOINT s`); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.ExecContext(ctx, q); e == nil {
				t.Fatalf("runtime mutated append-only data: %s", q)
			}
			if _, e = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT s`); e != nil {
				t.Fatal(e)
			}
		}
	})
}
