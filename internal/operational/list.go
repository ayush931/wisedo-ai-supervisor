package operational

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// SourceSummary is one row of a site's operational-source list: the current source revision
// and the mapping revision it pins. Credential references are never part of a summary.
type SourceSummary struct {
	SourceID             string    `json:"source_id" contract:"uuid"`
	Type                 string    `json:"type"`
	AuthorityScope       string    `json:"authority_scope" contract:"max=255"`
	Enabled              bool      `json:"enabled"`
	AccessPolicyRevision int64     `json:"access_policy_revision"`
	Revision             int64     `json:"revision"`
	MappingRevision      int64     `json:"mapping_revision"`
	MappingCount         int64     `json:"mapping_count"`
	ReviewVerdict        string    `json:"review_verdict,omitempty"`
	ReviewSourceRevision int64     `json:"review_source_revision,omitempty"`
	ActorIssuer          string    `json:"actor_issuer"`
	ActorSubject         string    `json:"actor_subject"`
	CreatedAt            time.Time `json:"created_at"`
}

// SourcePage is a stable cursor page ordered by source_id. NextCursor is the last source_id
// of this page when more rows exist.
type SourcePage struct {
	Sources    []SourceSummary `json:"sources"`
	NextCursor string          `json:"next_cursor,omitempty" contract:"uuid"`
}

// Detail is GET /operational-sources/{source}: the source revision and its mapping rows.
type Detail struct {
	Source   Source    `json:"source"`
	Mappings []Mapping `json:"mappings"`
}

// MappingsInput is the body of POST /operational-sources/{source}/mappings: the complete
// mapping set for the next mapping revision.
type MappingsInput struct {
	Mappings []Mapping `json:"mappings"`
}

// List returns one page of the site's sources. Any active member may read. Like
// knowledge.ListEntities it writes no audit row: a page holds summaries only (no mapping rows,
// no credential reference), operational_source_audit has no site-level event and requires a
// source_id, and evidence_audit's source.listed belongs to evidence sources. Get, which returns
// the mapping rows, still records source.read and mapping.read.
func (s *Service) List(ctx context.Context, a Actor, sc Scope, limit int, cursor string) (SourcePage, error) {
	out := SourcePage{Sources: []SourceSummary{}}
	if limit < 1 || limit > 100 || (cursor != "" && !validID(cursor)) {
		return out, ErrInvalid
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	tx, _, e := s.begin(ctx, a, sc, false, false)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT r.source_id,r.source_type,r.authority_scope,r.enabled,r.access_policy_revision,r.revision,r.mapping_revision,
  (SELECT count(*) FROM supervisor.operational_source_mappings m WHERE (m.tenant_id,m.site_id,m.source_id,m.mapping_revision)=(r.tenant_id,r.site_id,r.source_id,r.mapping_revision)),
  v.verdict,v.source_revision,r.actor_issuer,r.actor_subject,r.created_at
 FROM supervisor.operational_source_heads h
 JOIN supervisor.operational_source_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(h.tenant_id,h.site_id,h.source_id,h.current_revision)
 LEFT JOIN supervisor.operational_source_review_heads vh ON (vh.tenant_id,vh.site_id,vh.source_id)=(h.tenant_id,h.site_id,h.source_id)
 LEFT JOIN supervisor.operational_source_reviews v ON (v.tenant_id,v.site_id,v.source_id,v.revision)=(vh.tenant_id,vh.site_id,vh.source_id,vh.current_revision)
 WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.source_id>$3 ORDER BY h.source_id LIMIT $4`, sc.TenantID, sc.SiteID, cursor, limit+1)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var v SourceSummary
		var verdict sql.NullString
		var reviewed sql.NullInt64
		if e = rows.Scan(&v.SourceID, &v.Type, &v.AuthorityScope, &v.Enabled, &v.AccessPolicyRevision, &v.Revision, &v.MappingRevision, &v.MappingCount, &verdict, &reviewed, &v.ActorIssuer, &v.ActorSubject, &v.CreatedAt); e != nil {
			rows.Close()
			return out, e
		}
		v.ReviewVerdict, v.ReviewSourceRevision, v.CreatedAt = verdict.String, reviewed.Int64, v.CreatedAt.UTC()
		out.Sources = append(out.Sources, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(out.Sources) > limit {
		out.Sources = out.Sources[:limit]
		out.NextCursor = out.Sources[limit-1].SourceID
	}
	return out, s.finish(ctx, tx, a)
}
