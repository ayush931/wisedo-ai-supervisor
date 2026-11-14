//go:build integration && oeeperf

package oeeservice

// WIS-229 performance envelope. Not part of the required gates; run explicitly:
//
//	go test -count=1 -tags 'integration oeeperf' -run TestOEEPerformanceEnvelope -v ./internal/oeeservice
//
// It records latency, throughput and allocation on the host it runs on. The numbers are a
// measurement of that host, not an SLA.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func percentile(d []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(float64(len(s)-1)*p)]
}

// largeDataset fills the window with MaxTimelineFacts one-second RUNNING facts.
func largeDataset(t *testing.T) oee.OeeDatasetV1 {
	t.Helper()
	return dataset(t, func(d *oee.OeeDatasetV1) {
		start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
		n := oee.MaxTimelineFacts
		d.WindowStart, d.WindowEnd = start.Format(time.RFC3339), start.Add(time.Duration(n)*time.Second).Format(time.RFC3339)
		d.Exclusions = nil
		d.Facts = make([]oee.OeeFactV1, n)
		for i := range d.Facts {
			at := start.Add(time.Duration(i) * time.Second)
			d.Facts[i] = oee.OeeFactV1{EventID: fmt.Sprintf("run-%d", i), SourceKey: "plc", State: oee.StateRunning,
				Start: at.Format(time.RFC3339), End: at.Add(time.Second).Format(time.RFC3339), EventAt: at.Format(time.RFC3339),
				RecordedAt: at.Add(time.Second).Format(time.RFC3339), Revision: 1, PayloadDigest: fmt.Sprintf("%064x", i)}
		}
		d.Counts[0].Start, d.Counts[0].End, d.Counts[0].Total, d.Counts[0].Scrap = d.WindowStart, d.WindowEnd, "9000", "100"
	})
}

func TestOEEPerformanceEnvelope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	admin, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_ADMIN_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	runtimeDB, err := sql.Open("pgx", os.Getenv("SUPERVISOR_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeDB.Close()
	if err = migrate.Apply(ctx, admin); err != nil {
		t.Fatal(err)
	}
	scope := Scope{uuid.NewString(), uuid.NewString()}
	mustExec := func(q string, args ...any) {
		if _, e := admin.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	mustExec(`INSERT INTO supervisor.tenants(id,name) VALUES($1,'synthetic oee perf')`, scope.TenantID)
	mustExec(`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'perf line')`, scope.TenantID, scope.SiteID)
	actor := func(role string) Actor {
		a := Actor{"https://oee-perf.example/realm", uuid.NewString(), sessionHash()}
		mustExec(`INSERT INTO supervisor.browser_sessions(token_hash,issuer,subject,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,'synthetic',now(),now(),now()+interval '1 hour',now())`, a.SessionHash, a.Issuer, a.Subject)
		mustExec(`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5)`, scope.TenantID, scope.SiteID, a.Issuer, a.Subject, role)
		return a
	}
	owner, supervisor := actor("owner"), actor("supervisor")
	s, err := New(runtimeDB, Config{RunDeadline: time.Minute, LeaseDuration: 15 * time.Second, MaxOpenRuns: 64, MaxConcurrentExecutions: 8, IdempotencyRetention: time.Hour, ExecutionOwner: "oee-perf"})
	if err != nil {
		t.Fatal(err)
	}
	var body oee.OeePolicyBodyV1
	fixture(t, "policy.json", &body)
	policyID := uuid.NewString()
	p, err := s.CreateRevision(ctx, owner, scope, policyID, oee.OeePolicyRevisionInputV1{PolicyCode: "perf", EffectiveStart: "2026-01-01T00:00:00Z", Policy: body}, "perf-rev", "perf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Publish(ctx, supervisor, scope, policyID, p.Revisions[0].PolicyRevisionID, p.RowVersion, "perf-pub", "perf"); err != nil {
		t.Fatal(err)
	}
	runOne := func(d oee.OeeDatasetV1) (time.Duration, error) {
		start := time.Now()
		r, err := s.Accept(ctx, owner, scope, oee.OeeCalculationRequestV1{Dataset: d}, uuid.NewString(), "perf")
		if err != nil {
			return 0, err
		}
		if err = s.Execute(ctx, scope, r.RunID, d, "perf"); err != nil {
			return 0, err
		}
		got, err := s.GetRun(ctx, owner, scope, r.RunID, "perf")
		if err != nil || got.Status != oee.RunSucceeded {
			return 0, fmt.Errorf("run %s: %v", got.Status, err)
		}
		return time.Since(start), nil
	}

	small := dataset(t, nil)
	const sequential = 50
	lat := make([]time.Duration, 0, sequential)
	for i := 0; i < sequential; i++ {
		d, err := runOne(small)
		if err != nil {
			t.Fatal(err)
		}
		lat = append(lat, d)
	}
	t.Logf("ADR 0005 dataset (4 facts), %d sequential runs: p50=%v p95=%v max=%v", sequential, percentile(lat, .5), percentile(lat, .95), percentile(lat, 1))

	const workers, perWorker = 8, 25
	var mu sync.Mutex
	var wg sync.WaitGroup
	concurrent := make([]time.Duration, 0, workers*perWorker)
	begin := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				d, err := runOne(small)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				concurrent = append(concurrent, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(begin)
	t.Logf("ADR 0005 dataset, %d workers x %d runs: %.1f runs/s, p50=%v p95=%v", workers, perWorker, float64(len(concurrent))/elapsed.Seconds(), percentile(concurrent, .5), percentile(concurrent, .95))

	large := largeDataset(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	largeLat := make([]time.Duration, 0, 5)
	for i := 0; i < 5; i++ {
		d, err := runOne(large)
		if err != nil {
			t.Fatal(err)
		}
		largeLat = append(largeLat, d)
	}
	runtime.ReadMemStats(&after)
	t.Logf("max dataset (%d facts), 5 sequential runs: p50=%v max=%v, allocated %.1f MiB per run",
		oee.MaxTimelineFacts, percentile(largeLat, .5), percentile(largeLat, 1), float64(after.TotalAlloc-before.TotalAlloc)/5/(1<<20))
}
