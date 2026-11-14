package operational

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"
)

// ErrUnknownMapping is every reason an external identity does not resolve to an approved
// entity: unmapped, unreviewed, rejected, disabled or revoked source, or a stale target. The
// reasons are deliberately indistinguishable to the caller (WIS-270: fail closed, no oracle).
var ErrUnknownMapping = errors.New("external identity has no current approved mapping")

// The predicate below is the one definition of "this source is approved to resolve identities
// right now". Intake (internal/ingestion) and Resolve both use it, so they cannot diverge.

// SourceStateSQL reads the source's current revision, mapping head and latest review, locking
// the heads so the decision holds for the rest of the transaction.
const SourceStateSQL = `SELECT sr.revision,sr.mapping_revision,mh.current_revision,sr.enabled,sr.authority_scope,sr.config_sha256,sr.mapping_sha256,rv.source_revision,rv.mapping_revision,rv.config_sha256,rv.mapping_sha256,rv.verdict FROM supervisor.operational_source_heads sh JOIN supervisor.operational_source_revisions sr ON (sr.tenant_id,sr.site_id,sr.source_id,sr.revision)=(sh.tenant_id,sh.site_id,sh.source_id,sh.current_revision) JOIN supervisor.operational_source_mapping_heads mh ON (mh.tenant_id,mh.site_id,mh.source_id)=(sh.tenant_id,sh.site_id,sh.source_id) JOIN supervisor.operational_source_review_heads rh ON (rh.tenant_id,rh.site_id,rh.source_id)=(sh.tenant_id,sh.site_id,sh.source_id) JOIN supervisor.operational_source_reviews rv ON (rv.tenant_id,rv.site_id,rv.source_id,rv.revision)=(rh.tenant_id,rh.site_id,rh.source_id,rh.current_revision) WHERE sh.tenant_id=$1 AND sh.site_id=$2 AND sh.source_id=$3 FOR SHARE OF sh,mh,rh`

// StaleMappingSQL counts mapped entities whose head moved past the reviewed mapping.
const StaleMappingSQL = `SELECT count(*) FROM (SELECT h.current_revision,m.entity_revision FROM supervisor.operational_source_mappings m JOIN supervisor.knowledge_entity_heads h ON (h.tenant_id,h.site_id,h.id)=(m.tenant_id,m.site_id,m.entity_id) WHERE m.tenant_id=$1 AND m.site_id=$2 AND m.source_id=$3 AND m.mapping_revision=$4 FOR SHARE OF h) x WHERE x.current_revision<>x.entity_revision`

// MappingEntitySQL resolves one exact (external_type, external_key) in a mapping revision.
// Text equality is byte-exact: no case folding or Unicode normalization.
const MappingEntitySQL = `SELECT entity_id,entity_revision FROM supervisor.operational_source_mappings WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND mapping_revision=$4 AND external_type=$5 AND external_key=$6`

// SourceCurrent is true when the source is enabled, approved for exactly its current config
// and mapping, and the caller named those current revisions.
func SourceCurrent(enabled bool, srcRev, mapRev, headMapRev, reviewSrc, reviewMap int64, verdict, configHash, mapHash, reviewConfig, reviewMapping string, wantSrc, wantMap int64) bool {
	return enabled && srcRev == wantSrc && mapRev == wantMap && headMapRev == mapRev && reviewSrc == srcRev && reviewMap == mapRev &&
		verdict == "approved" && reviewConfig == configHash && reviewMapping == mapHash
}

// ExternalIdentity is an external key resolved through an exact, currently approved mapping.
type ExternalIdentity struct {
	EntityID         string `json:"entity_id"`
	EntityRevision   int64  `json:"entity_revision"`
	SourceID         string `json:"source_id"`
	SourceRevision   int64  `json:"source_revision"`
	MappingRevision  int64  `json:"mapping_revision"`
	MappingSHA256    string `json:"mapping_sha256"`
	ReviewDecisionID string `json:"review_decision_id"`
	// RedirectPath lists, in order, the merged entity IDs followed from the mapped entity to
	// EntityID; empty when the mapping names EntityID directly. The mapping is never rewritten.
	RedirectPath []string `json:"redirect_path"`
}

// MaxRedirectHops bounds how many merge redirects Resolve follows; a longer chain fails closed.
const MaxRedirectHops = 16

// ValidIdentityQuery applies the rules a stored mapping must satisfy (Mapping.Validate) plus
// valid UTF-8, so a malformed key is 422 before any lookup and never a state-dependent error.
func ValidIdentityQuery(sourceID, entityType, key string) bool {
	return validID(sourceID) && (entityType == "asset" || entityType == "order") && validText(key, 512) && utf8.ValidString(key)
}

// Resolve maps (source, entity type, external key) to an entity only when intake would accept
// the source right now and the key is mapped byte-for-byte. Unknown or out-of-scope sources are
// ErrNotFound; every other refusal is ErrUnknownMapping. Each call that reaches an existing
// source records exactly one mapping.read audit row, whether it resolves or not.
func (s *Service) Resolve(ctx context.Context, a Actor, sc Scope, sourceID, entityType, key string) (ExternalIdentity, error) {
	var out ExternalIdentity
	if !ValidIdentityQuery(sourceID, entityType, key) {
		return out, ErrInvalid
	}
	tx, _, e := s.begin(ctx, a, sc, false, false)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	// The state read share-locks the source, mapping and review heads first, so the head read
	// afterwards (for the 404 distinction, review decision and audit refs) is the same one.
	resolved, e := resolveCurrent(ctx, tx, sc, sourceID, entityType, key, &out)
	if e != nil {
		return ExternalIdentity{}, e
	}
	var srcRev, mapRev int64
	var decision sql.NullString
	e = tx.QueryRowContext(ctx, sourceHeadSQL, sc.TenantID, sc.SiteID, sourceID).Scan(&srcRev, &mapRev, &decision)
	if errors.Is(e, sql.ErrNoRows) {
		return ExternalIdentity{}, ErrNotFound
	}
	if e != nil {
		return ExternalIdentity{}, e
	}
	if resolved {
		if !decision.Valid || srcRev != out.SourceRevision || mapRev != out.MappingRevision {
			return ExternalIdentity{}, errors.New("operational source changed under share lock")
		}
		out.ReviewDecisionID = decision.String
	}
	if e = recordAudit(ctx, tx, a, sc, sourceID, mapRev, "mapping.read", auditRefs{source: srcRev, mapping: mapRev}); e != nil {
		return ExternalIdentity{}, e
	}
	if e = s.finish(ctx, tx, a); e != nil {
		return ExternalIdentity{}, e
	}
	if !resolved {
		return ExternalIdentity{}, ErrUnknownMapping
	}
	return out, nil
}

func resolveCurrent(ctx context.Context, tx *sql.Tx, sc Scope, sourceID, entityType, key string, out *ExternalIdentity) (bool, error) {
	var srcRev, mapRev, headMapRev, reviewSrc, reviewMap int64
	var enabled bool
	var authorityScope, configHash, mapHash, reviewConfig, reviewMapping, verdict string
	e := tx.QueryRowContext(ctx, SourceStateSQL, sc.TenantID, sc.SiteID, sourceID).Scan(&srcRev, &mapRev, &headMapRev, &enabled, &authorityScope, &configHash, &mapHash, &reviewSrc, &reviewMap, &reviewConfig, &reviewMapping, &verdict)
	if errors.Is(e, sql.ErrNoRows) { // unknown source (404 from sourceHeadSQL) or never reviewed
		return false, nil
	}
	if e != nil {
		return false, e
	}
	// The caller names no revisions: it asks about the source's own current ones.
	if !SourceCurrent(enabled, srcRev, mapRev, headMapRev, reviewSrc, reviewMap, verdict, configHash, mapHash, reviewConfig, reviewMapping, srcRev, mapRev) {
		return false, nil
	}
	var stale int
	if e = tx.QueryRowContext(ctx, StaleMappingSQL, sc.TenantID, sc.SiteID, sourceID, mapRev).Scan(&stale); e != nil {
		return false, e
	}
	if stale > 0 {
		return false, nil
	}
	e = tx.QueryRowContext(ctx, MappingEntitySQL, sc.TenantID, sc.SiteID, sourceID, mapRev, entityType, key).Scan(&out.EntityID, &out.EntityRevision)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	out.SourceID, out.SourceRevision, out.MappingRevision, out.MappingSHA256 = sourceID, srcRev, mapRev, mapHash
	return followRedirects(ctx, tx, sc, out)
}

// MappedEntityCurrent is intake's check of an entity a mapping names at its pinned revision,
// inside the caller's tenant transaction: false when the entity has been merged into another
// (resolution would return the survivor) or the evidence behind that revision is revoked.
// Intake records an event against the mapped entity itself (the record's foreign key is the
// mapping row), so those cases must quarantine instead of attributing events to an identity
// resolution refuses. A revised (not revoked) evidence source does not withdraw an approved
// mapping in intake, matching plan admission; Resolve is stricter and also requires it current.
func MappedEntityCurrent(ctx context.Context, tx *sql.Tx, tenantID, siteID, entityID string, revision int64) (bool, error) {
	var head int64
	if e := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_entity_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, tenantID, siteID, entityID).Scan(&head); e != nil {
		return false, e
	}
	var merged, revoked bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND site_id=$2 AND from_entity_id=$3),
 coalesce((SELECT h.revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)) FROM supervisor.knowledge_entity_revisions r JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(r.tenant_id,r.site_id,r.source_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$3 AND r.revision=$4),true)`, tenantID, siteID, entityID, revision).Scan(&merged, &revoked)
	if e != nil {
		return false, e
	}
	return head == revision && !merged && !revoked, nil
}

// followRedirects replaces the mapped entity with the end of its merge chain and records the
// original IDs passed through. No redirect: unchanged.
//
// Every head on the walk must still be at the revision the previous human decision pinned: the
// mapping's entity_revision for the first, each redirect's to_entity_revision after that. So a
// revision made after a merge (of the terminal, or of an intermediate before it was merged on)
// fails closed rather than resolving to content nobody reviewed. Every entity on the walk (mapped,
// intermediate and terminal) must also have current, unrevoked evidence at its pinned revision.
// Every refusal is (false, nil): one 409 body.
//
// Each hop share-locks the entity head before reading its redirect, in a separate statement so
// the read sees any merge that held the head. A merge needs that head FOR UPDATE, so no entity
// on the walked chain can gain a redirect before commit: the answer is one consistent state.
// Redirect rows themselves are immutable and written at most once per entity.
func followRedirects(ctx context.Context, tx *sql.Tx, sc Scope, out *ExternalIdentity) (bool, error) {
	out.RedirectPath = []string{}
	current, expected := out.EntityID, out.EntityRevision
	for {
		var revision int64
		if e := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_entity_heads WHERE tenant_id=$1 AND site_id=$2 AND id=$3 FOR SHARE`, sc.TenantID, sc.SiteID, current).Scan(&revision); e != nil {
			return false, e
		}
		if revision != expected {
			return false, nil
		}
		if ok, e := evidenceCurrent(ctx, tx, sc, current, revision); !ok || e != nil {
			return false, e
		}
		var next string
		var fromRev, toRev int64
		e := tx.QueryRowContext(ctx, `SELECT to_entity_id::text,from_entity_revision,to_entity_revision FROM supervisor.knowledge_entity_redirects WHERE tenant_id=$1 AND site_id=$2 AND from_entity_id=$3`, sc.TenantID, sc.SiteID, current).Scan(&next, &fromRev, &toRev)
		if errors.Is(e, sql.ErrNoRows) {
			out.EntityID, out.EntityRevision = current, revision
			return true, nil
		}
		if e != nil {
			return false, e
		}
		// A merged entity cannot be revised, so its head is the merge's from revision; checked
		// anyway so a violated invariant fails closed.
		if fromRev != revision || len(out.RedirectPath) == MaxRedirectHops { // merges never build longer chains
			return false, nil
		}
		out.RedirectPath = append(out.RedirectPath, current)
		current, expected = next, toRev
	}
}

// evidenceCurrent share-locks an entity revision's evidence head and requires it current and
// unrevoked (the knowledge lockSources/eligible predicate), so revoking or revising the evidence
// behind any entity on a resolution walk stops it resolving.
func evidenceCurrent(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (bool, error) {
	var ok bool
	e := tx.QueryRowContext(ctx, `SELECT h.current_revision=r.source_revision AND NOT h.revoked AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(h.tenant_id,h.site_id,h.source_id)) FROM supervisor.knowledge_entity_revisions r JOIN supervisor.evidence_heads h ON (h.tenant_id,h.site_id,h.source_id)=(r.tenant_id,r.site_id,r.source_id) WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.id=$3 AND r.revision=$4 FOR SHARE OF h`, sc.TenantID, sc.SiteID, id, revision).Scan(&ok)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	return ok && e == nil, e
}

// sourceHeadSQL reads the current source and mapping revisions and the current review's
// decision ID (NULL when never reviewed). No row means the source is not in this scope.
const sourceHeadSQL = `SELECT sr.revision,sr.mapping_revision,(SELECT rv.decision_id::text FROM supervisor.operational_source_review_heads rh JOIN supervisor.operational_source_reviews rv ON (rv.tenant_id,rv.site_id,rv.source_id,rv.revision)=(rh.tenant_id,rh.site_id,rh.source_id,rh.current_revision) WHERE (rh.tenant_id,rh.site_id,rh.source_id)=(sh.tenant_id,sh.site_id,sh.source_id)) FROM supervisor.operational_source_heads sh JOIN supervisor.operational_source_revisions sr ON (sr.tenant_id,sr.site_id,sr.source_id,sr.revision)=(sh.tenant_id,sh.site_id,sh.source_id,sh.current_revision) WHERE sh.tenant_id=$1 AND sh.site_id=$2 AND sh.source_id=$3`
