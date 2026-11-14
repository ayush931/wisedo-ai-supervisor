//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// WIS-270 entity redirects: 040 upgrades a populated database as the non-superuser migration
// owner, keeps existing receipts/audit rows, and the runtime role can only append redirects
// that are in scope, type-consistent and never branch or chain into a redirected entity.
func TestEntityRedirectsUpgradeAndRuntimeGuards(t *testing.T) {
	db := migratorDB(t)
	ctx := context.Background()
	if err := applyThrough(ctx, db, 39); err != nil {
		t.Fatal(err)
	}
	tenant, site := createSite(t, db)
	other, _ := createSite(t, db)
	a, b, c, line := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	for _, id := range []string{a, b, c} {
		seedOperationalAsset(t, tx, tenant, site, id)
	}
	seedAsset(t, tx, tenant, site, line, "line")
	// A pre-040 receipt and audit row must survive the CHECK replacement.
	if _, err = tx.Exec(`INSERT INTO supervisor.knowledge_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_hash,operation,target_id,revision) VALUES($1,$2,'test','operator','pre-040',$3,'entity',$4,1)`, tenant, site, strings.Repeat("b", 64), a); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO supervisor.knowledge_audit(tenant_id,site_id,actor_issuer,actor_subject,target_id,revision,event_type) VALUES($1,$2,'test','operator',$3,1,'entity.created')`, tenant, site, a); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(ctx, db); err != nil {
		t.Fatalf("upgrade 039 -> 040 as the migration role: %v", err)
	}
	if err = Check(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertTenantTablesForced(t, db)
	runtime := runtimeDB(t, db)

	// run executes one statement as the runtime role under tenant scope (empty scope: no GUC).
	run := func(scope, query string, args ...any) error {
		t.Helper()
		rtx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer rtx.Rollback()
		if scope != "" {
			if _, e = rtx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, scope); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = rtx.Exec(query, args...); e != nil {
			return e
		}
		return rtx.Commit()
	}
	redirect := func(scope, from, to string) error {
		return run(scope, `INSERT INTO supervisor.knowledge_entity_redirects(tenant_id,site_id,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,1,$5,1,'same press','test','reviewer',clock_timestamp())`, tenant, site, uuid.NewString(), from, to)
	}
	count := func(scope string) int {
		t.Helper()
		rtx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer rtx.Rollback()
		if _, e = rtx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, scope); e != nil {
			t.Fatal(e)
		}
		var n int
		if e = rtx.QueryRow(`SELECT count(*) FROM supervisor.knowledge_entity_redirects`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	if err = redirect("", a, b); sqlState(err) != "42501" {
		t.Fatalf("redirect without tenant scope: %v", err)
	}
	if err = redirect(other, a, b); sqlState(err) != "42501" {
		t.Fatalf("redirect under another tenant's scope: %v", err)
	}
	if err = redirect(tenant, a, a); sqlState(err) != "23514" {
		t.Fatalf("self redirect: %v", err)
	}
	if err = redirect(tenant, a, b); err != nil {
		t.Fatalf("valid redirect: %v", err)
	}
	if err = redirect(tenant, a, c); sqlState(err) != "23505" {
		t.Fatalf("second redirect of the same entity: %v", err)
	}
	if err = redirect(tenant, c, a); sqlState(err) != "23514" {
		t.Fatalf("redirect into a redirected entity: %v", err)
	}
	if err = redirect(tenant, b, c); err != nil {
		t.Fatalf("chain B -> C: %v", err)
	}
	if err = redirect(tenant, line, c); sqlState(err) != "23514" {
		t.Fatalf("line asset redirected into a machine: %v", err)
	}
	for name, q := range map[string]string{
		"update": `UPDATE supervisor.knowledge_entity_redirects SET reason='rewritten'`,
		"delete": `DELETE FROM supervisor.knowledge_entity_redirects`,
	} {
		if err = run(tenant, q); sqlState(err) != "42501" {
			t.Fatalf("runtime %s of a redirect: %v", name, err)
		}
	}
	if n := count(tenant); n != 2 {
		t.Fatalf("tenant sees %d redirects, want 2", n)
	}
	if n := count(other); n != 0 {
		t.Fatalf("another tenant sees %d redirects, want 0", n)
	}
	if err = run(tenant, `INSERT INTO supervisor.knowledge_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_hash,operation,target_id,revision) VALUES($1,$2,'test','reviewer','merge-1',$3,'merge',$4,1)`, tenant, site, strings.Repeat("c", 64), a); err != nil {
		t.Fatalf("merge receipt: %v", err)
	}
	if err = run(tenant, `INSERT INTO supervisor.knowledge_audit(tenant_id,site_id,actor_issuer,actor_subject,target_id,revision,event_type) VALUES($1,$2,'test','reviewer',$3,1,'entity.merged')`, tenant, site, a); err != nil {
		t.Fatalf("merge audit: %v", err)
	}
	var kept int
	if err = runtimeCount(ctx, runtime, tenant, `SELECT (SELECT count(*) FROM supervisor.knowledge_receipts WHERE idempotency_key='pre-040')+(SELECT count(*) FROM supervisor.knowledge_audit WHERE event_type='entity.created')`, &kept); err != nil || kept != 2 {
		t.Fatalf("pre-040 rows kept=%d err=%v", kept, err)
	}
}

func runtimeCount(ctx context.Context, db *sql.DB, tenant, query string, out *int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT set_config('supervisor.tenant_id',$1,true)`, tenant); err != nil {
		return err
	}
	return tx.QueryRow(query).Scan(out)
}

// WIS-270 review: the trigger alone (without the service's site lock) keeps concurrent merges
// acyclic under READ COMMITTED. Each blocked insert is released only after it is observed
// waiting on a lock, so the interleaving is the racy one, not a lucky serial order.
func TestEntityRedirectTriggerSerializesConcurrentMerges(t *testing.T) {
	admin := newOperationalMigrationDB(t)
	ctx := context.Background()
	runtime := runtimeDB(t, admin)
	tenant, site := createSite(t, admin)
	ids := make([]string, 5)
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	setTenant(t, tx, tenant)
	for i := range ids {
		ids[i] = uuid.NewString()
		seedOperationalAsset(t, tx, tenant, site, ids[i])
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// begin opens a tenant-scoped runtime transaction and reports its backend PID.
	type pending struct {
		tx  *sql.Tx
		pid int
	}
	begin := func() pending {
		t.Helper()
		rtx, e := runtime.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		var p pending
		p.tx = rtx
		if e = rtx.QueryRow(`SELECT pg_backend_pid() FROM (SELECT set_config('supervisor.tenant_id',$1,true)) x`, tenant).Scan(&p.pid); e != nil {
			t.Fatal(e)
		}
		return p
	}
	insert := func(p pending, from, to string) error {
		_, e := p.tx.Exec(`INSERT INTO supervisor.knowledge_entity_redirects(tenant_id,site_id,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,1,$5,1,'same press','test','reviewer',clock_timestamp())`, tenant, site, uuid.NewString(), from, to)
		if e != nil {
			_ = p.tx.Rollback()
			return e
		}
		return p.tx.Commit()
	}
	// race starts each blocked insert in its own goroutine and waits until it waits on a lock.
	race := func(merges ...[2]string) []chan error {
		t.Helper()
		out := []chan error{}
		for _, m := range merges {
			p, done := begin(), make(chan error, 1)
			go func(from, to string) { done <- insert(p, from, to) }(m[0], m[1])
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting bool
				if e := admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, p.pid).Scan(&waiting); e != nil {
					t.Fatal(e)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("merge %v never waited on the held entity lock", m)
				}
				time.Sleep(10 * time.Millisecond)
			}
			out = append(out, done)
		}
		return out
	}
	acyclic := func() {
		t.Helper()
		var cycles int
		if e := runtimeCount(ctx, runtime, tenant, `WITH RECURSIVE walk(start,id,depth) AS (SELECT from_entity_id,to_entity_id,1 FROM supervisor.knowledge_entity_redirects UNION ALL SELECT w.start,r.to_entity_id,w.depth+1 FROM walk w JOIN supervisor.knowledge_entity_redirects r ON r.from_entity_id=w.id WHERE w.depth<10) SELECT count(*) FROM walk WHERE start=id`, &cycles); e != nil || cycles != 0 {
			t.Fatalf("redirect cycles=%d err=%v", cycles, e)
		}
	}

	// Two-way: A->B is held open while B->A waits; B->A must then see A redirected.
	a, b := ids[0], ids[1]
	held := begin()
	if _, err = held.tx.Exec(`INSERT INTO supervisor.knowledge_entity_redirects(tenant_id,site_id,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,1,$5,1,'same press','test','reviewer',clock_timestamp())`, tenant, site, uuid.NewString(), a, b); err != nil {
		t.Fatal(err)
	}
	waiters := race([2]string{b, a})
	if err = held.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-waiters[0]; sqlState(err) != "23514" {
		t.Fatalf("concurrent B->A after A->B: %v", err)
	}
	acyclic()

	// Three-way ring: X->Y held open while Y->Z and Z->X wait. Z->X must fail; no cycle forms.
	x, y, z := ids[2], ids[3], ids[4]
	held = begin()
	if _, err = held.tx.Exec(`INSERT INTO supervisor.knowledge_entity_redirects(tenant_id,site_id,decision_id,from_entity_id,from_entity_revision,to_entity_id,to_entity_revision,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,1,$5,1,'same press','test','reviewer',clock_timestamp())`, tenant, site, uuid.NewString(), x, y); err != nil {
		t.Fatal(err)
	}
	waiters = race([2]string{y, z}, [2]string{z, x})
	if err = held.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-waiters[0]; err != nil {
		t.Fatalf("Y->Z behind X->Y: %v", err)
	}
	if err = <-waiters[1]; sqlState(err) != "23514" {
		t.Fatalf("Z->X closing the ring: %v", err)
	}
	acyclic()
}
