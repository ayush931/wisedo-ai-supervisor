//go:build integration

package httpapi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkstationConcurrentReadsHTTPPostgres is the WIS-520 regression for spurious 403s on
// parallel reads: every request touches its session's last_seen_at in the identity
// middleware, and workstation reads used to take FOR SHARE on that session row inside their
// REPEATABLE READ snapshot. A sibling request's touch committed after the snapshot then
// raised 40001, which begin reported as PERMISSION_DENIED. One session now reads five
// different workstation endpoints concurrently and every response must be 200; a session
// that is really expired or a membership that is really revoked still gets its exact denial.
func TestWorkstationConcurrentReadsHTTPPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	now0 := f.clock()
	s := f.recordingStation(now0.Add(-time.Hour), now0.Add(time.Hour))
	site := "/v1/sites/" + f.site
	plan := site + "/shift-plans/" + s.plan.ID
	reads := []string{
		plan,
		plan + "/summaries",
		plan + "/handovers",
		site + "/workstations/" + s.station + "/current-job-order",
		site + "/job-orders/" + s.order.ID,
	}
	for _, path := range reads {
		f.expect(f.call(f.lead, "GET", path, "", "", ""), 200, "")
	}

	const workers, rounds = 20, 5
	var mu sync.Mutex
	failures := map[string]int{}
	for round := 0; round < rounds; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			path := reads[(i+round)%len(reads)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w := f.call(f.lead, "GET", path, "", "", "")
				if w.Code != 200 {
					mu.Lock()
					code, _, _ := strings.Cut(strings.TrimPrefix(w.Body.String(), `{"code":"`), `"`)
					failures[fmt.Sprintf("%d %s %s", w.Code, code, path[strings.LastIndex(path, "/")+1:])]++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
	}
	if len(failures) > 0 {
		var lines []string
		for k, n := range failures {
			lines = append(lines, fmt.Sprintf("%dx %s", n, k))
		}
		sort.Strings(lines)
		t.Fatalf("concurrent reads by one session must all be 200:\n%s", strings.Join(lines, "\n"))
	}

	// The boundary is still enforced: both sessions read before, and fail exactly after.
	f.expect(f.call(f.viewer, "GET", plan, "", "", ""), 200, "")
	f.expect(f.call(f.spare, "GET", plan, "", "", ""), 200, "")
	f.exec(`UPDATE supervisor.browser_sessions SET expires_at=now()-interval '1 second' WHERE subject=$1`, f.viewer.subject)
	f.exec(`UPDATE supervisor.memberships SET active=false WHERE tenant_id=$1 AND site_id=$2 AND subject=$3`, f.tenant, f.site, f.spare.subject)
	for _, path := range reads {
		f.expect(f.call(f.viewer, "GET", path, "", "", ""), 401, "AUTHENTICATION_REQUIRED")
		f.expect(f.call(f.spare, "GET", path, "", "", ""), 404, "NOT_FOUND")
	}
}
