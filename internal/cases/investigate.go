package cases

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"
)

// ReasonContextReady is the stable reason recorded on the detected -> investigating transition.
const ReasonContextReady = "CONTEXT_READY"

// SweepActor records the detected -> investigating move the worker tick makes; it is not the
// case workflow.
var SweepActor = ActorRef{Issuer: "supervisor:worker", Subject: "detected-case-sweep"}

// StartDetected moves the tenant's detected cases to investigating (WIS-636): the context_ready
// transition the state machine allows when the case's immutable bound context is READY and the
// scope is enabled. Before this, no production code applied it, so a case the evaluator opened
// stayed detected and was never investigated.
//
// scopeEnabled is whether the worker's built agent runtime investigates cases, the same answer
// the case workflow uses; a site policy may replace it later as a policy revision. With it off
// nothing moves, so a case opened while the agent was off moves on the first tick after it is
// turned on. Each move is its own transaction and signals the case workflow through the outbox;
// one case's failure never holds back another: limit is a page size, and every detected case is
// visited each call, so cases that keep failing cannot pin the window.
func (s *Service) StartDetected(ctx context.Context, tenantID string, scopeEnabled bool, limit int) (int, error) {
	if !validID(tenantID) || limit < 1 {
		return 0, ErrInvalid
	}
	if !scopeEnabled {
		return 0, nil
	}
	moved := 0
	var failed []error
	// ponytail: no index on state, so each call scans the tenant's whole case history, not just
	// the detected backlog; add a partial index on (tenant_id,id) WHERE state='detected' if it grows.
	for after := "00000000-0000-0000-0000-000000000000"; ; {
		pending, err := s.detectedCases(ctx, tenantID, after, limit)
		if err != nil {
			return moved, errors.Join(append(failed, err)...)
		}
		for _, p := range pending {
			ok, err := s.startOne(ctx, Scope{TenantID: tenantID, SiteID: p.siteID}, p.caseID)
			if err != nil {
				if sweepFailures.allow(tenantID+"/"+p.caseID, time.Now()) {
					log.Printf("detected-case sweep: tenant %s case %s not started: %s", tenantID, p.caseID, sweepErrorClass(err))
				}
				failed = append(failed, err)
				continue
			}
			if ok {
				moved++
			}
		}
		if len(pending) < limit {
			return moved, errors.Join(failed...)
		}
		after = pending[len(pending)-1].caseID
	}
}

// sweepErrorClass names a startOne failure without its text, which may carry SQL (rule 07).
func sweepErrorClass(err error) string {
	switch {
	case errors.Is(err, errIntegrity):
		return "integrity"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "storage"
	}
}

// sweepFailureEvery is how often one case's repeated sweep failure is logged; the tick runs every second.
const sweepFailureEvery = 10 * time.Minute

// failureLog rate-limits a log line per key.
// ponytail: in-process and never pruned, one entry per case that ever failed; prune on success if it grows.
type failureLog struct {
	sync.Mutex
	last map[string]time.Time
}

var sweepFailures = &failureLog{last: map[string]time.Time{}}

// allow reports whether key's failure should be logged at now, and records it if so.
func (l *failureLog) allow(key string, now time.Time) bool {
	l.Lock()
	defer l.Unlock()
	if last, ok := l.last[key]; ok && now.Sub(last) < sweepFailureEvery {
		return false
	}
	l.last[key] = now
	return true
}

type detectedCase struct{ siteID, caseID string }

// detectedCases lists the tenant's detected cases with an ID after after, in ID order, up to
// limit. A tenant-wide read: the RLS tenant setting scopes it.
func (s *Service) detectedCases(ctx context.Context, tenantID, after string, limit int) ([]detectedCase, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true),set_config('statement_timeout','10s',true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT site_id,id FROM supervisor.cases WHERE tenant_id=$1 AND state='detected' AND id>$2 ORDER BY id LIMIT $3`, tenantID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []detectedCase
	for rows.Next() {
		var d detectedCase
		if err = rows.Scan(&d.siteID, &d.caseID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// startOne applies context_ready to one case under the site scope and the case lock, re-reading
// its state there: a case another actor moved meanwhile, a deactivated tenant, or a bound context
// that is not READY is left as it is.
func (s *Service) startOne(ctx context.Context, sc Scope, caseID string) (bool, error) {
	tx, _, err := s.workflowTx(ctx, sc.TenantID, sc.SiteID, true)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.active_tenant_ids() a(id) WHERE a.id=$1)`, sc.TenantID).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `SELECT 1 FROM supervisor.cases WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, caseID); err != nil {
		return false, err
	}
	current, err := load(ctx, tx, sc, caseID)
	if err != nil {
		return false, err
	}
	if current.State != Detected {
		return false, nil
	}
	var ready bool
	err = tx.QueryRowContext(ctx, `SELECT readiness='READY' FROM supervisor.plant_contexts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, current.ContextSnapshotID).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	revision, err := move(ctx, tx, sc, current, ContextReady, Guards{ContextReady: ready, ScopeEnabled: true}, ReasonContextReady, SweepActor, "")
	if errors.Is(err, ErrGuardFailed) {
		return false, nil // not READY: it stays detected, visibly
	}
	if err != nil {
		return false, err
	}
	return true, s.signalAndCommit(ctx, tx, sc, caseID, revision)
}
