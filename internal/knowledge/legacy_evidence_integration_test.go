//go:build integration

package knowledge

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// Legacy-upgrade fixtures seed evidence with raw SQL: the evidence service targets the current
// schema (039 added independence columns), so it cannot write to a pre-039 database.
type legacyEvidence struct {
	t     *testing.T
	ctx   context.Context
	db    *sql.DB
	actor Actor
}

func (l legacyEvidence) exec(sc Scope, statements ...func(*sql.Tx) error) {
	l.t.Helper()
	tx, err := l.db.BeginTx(l.ctx, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(l.ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, sc.TenantID); err != nil {
		l.t.Fatal(err)
	}
	for _, statement := range statements {
		if err = statement(tx); err != nil {
			l.t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		l.t.Fatal(err)
	}
}

// source writes revision 1, or the next revision of an existing source.
func (l legacyEvidence) source(sc Scope, id, kind, text string, revision int64) {
	l.t.Helper()
	l.exec(sc, func(tx *sql.Tx) error {
		q := `INSERT INTO supervisor.evidence_heads(tenant_id,site_id,source_id,current_revision,state_revision) VALUES($1,$2,$3,1,1)`
		if revision > 1 {
			q = `UPDATE supervisor.evidence_heads SET current_revision=current_revision+1,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`
		}
		_, e := tx.ExecContext(l.ctx, q, sc.TenantID, sc.SiteID, id)
		return e
	}, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(l.ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,$5,'Legacy source',$6,encode(sha256(convert_to($6,'UTF8')),'hex'),octet_length($6),length($6),$7,$8)`, sc.TenantID, sc.SiteID, id, revision, kind, text, l.actor.Issuer, l.actor.Subject)
		return e
	})
}

func (l legacyEvidence) span(sc Scope, source string, start, end int) string {
	l.t.Helper()
	id := uuid.NewString()
	l.exec(sc, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(l.ctx, `INSERT INTO supervisor.evidence_spans(tenant_id,site_id,id,source_id,revision,start_rune,end_rune,actor_issuer,actor_subject) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8)`, sc.TenantID, sc.SiteID, id, source, start, end, l.actor.Issuer, l.actor.Subject)
		return e
	})
	return id
}

func (l legacyEvidence) revoke(sc Scope, source string) {
	l.t.Helper()
	l.exec(sc, func(tx *sql.Tx) error {
		_, e := tx.ExecContext(l.ctx, `WITH h AS (UPDATE supervisor.evidence_heads SET revoked=true,access_epoch=access_epoch+1,state_revision=state_revision+1 WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 RETURNING state_revision,access_epoch)
 INSERT INTO supervisor.evidence_revocations(tenant_id,site_id,source_id,state_revision,access_epoch,actor_issuer,actor_subject,reason) SELECT $1,$2,$3,state_revision,access_epoch,$4,$5,'Withdrawn synthetic evidence' FROM h`, sc.TenantID, sc.SiteID, source, l.actor.Issuer, l.actor.Subject)
		return e
	})
}
