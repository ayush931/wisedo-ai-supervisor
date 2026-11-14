package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrDiscoveryConflict = errors.New("discovery inventory scope cannot change")

type DiscoveryRequirement struct {
	RequirementID string `json:"requirement_id"`
	Key           string `json:"key"`
	Description   string `json:"description"`
	InterviewRole string `json:"interview_role"`
}
type DiscoveryInventory struct {
	SchemaVersion   int                    `json:"schema_version"`
	TemplateVersion int                    `json:"template_version"`
	InventoryID     string                 `json:"inventory_id"`
	Revision        int64                  `json:"revision"`
	CurrentRevision int64                  `json:"current_revision"`
	AreaID          string                 `json:"area_id"`
	AreaRevision    int64                  `json:"area_revision"`
	ProcessID       string                 `json:"process_id"`
	ProcessRevision int64                  `json:"process_revision"`
	Capability      string                 `json:"capability"`
	Reason          string                 `json:"reason"`
	ActorIssuer     string                 `json:"actor_issuer"`
	ActorSubject    string                 `json:"actor_subject"`
	AuthenticatedAt time.Time              `json:"authenticated_at"`
	CreatedAt       time.Time              `json:"created_at"`
	ScopeCurrent    bool                   `json:"scope_current"`
	Requirements    []DiscoveryRequirement `json:"requirements"`
}

type DiscoveryInventoryListItem struct {
	InventoryID        string `json:"inventory_id"`
	ProcessID          string `json:"process_id"`
	ProcessDisplayName string `json:"process_display_name"`
	// Capability names the checklist, so a person can choose between a process's inventories (WIS-617).
	Capability      string    `json:"capability"`
	CurrentRevision int64     `json:"current_revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type DiscoveryInventoryPage struct {
	Inventories []DiscoveryInventoryListItem `json:"inventories"`
	NextCursor  string                       `json:"next_cursor,omitempty"`
}

// Scope anchors establish provenance only; they do not assert requirement coverage.
func discoveryScope(ctx context.Context, tx *sql.Tx, sc Scope, areaID string, areaRevision int64, processID string, processRevision int64) (bool, error) {
	area, err := loadEntity(ctx, tx, sc, areaID, areaRevision)
	if err != nil {
		return false, err
	}
	process, err := loadEntity(ctx, tx, sc, processID, processRevision)
	if err != nil {
		return false, err
	}
	refs := []sourceRef{{id: area.SourceID, revision: area.SourceRevision}, {id: process.SourceID, revision: process.SourceRevision}}
	states, err := lockSources(ctx, tx, sc, refs)
	if err != nil {
		return false, err
	}
	current, err := eligible(refs, states, false)
	if err != nil {
		return false, err
	}
	if area.Type != "area" || process.Type != "process" {
		return false, ErrInvalidContract
	}
	heads, err := lockEntities(ctx, tx, sc, []string{areaID, processID})
	if err != nil {
		return false, err
	}
	return current && heads[areaID] == areaRevision && heads[processID] == processRevision, nil
}

func loadDiscoveryInventory(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (DiscoveryInventory, error) {
	var out DiscoveryInventory
	err := tx.QueryRowContext(ctx, `SELECT r.schema_version,r.template_version,r.inventory_id,r.revision,h.current_revision,r.area_id,r.area_revision,r.process_id,r.process_revision,r.capability,r.reason,r.actor_issuer,r.actor_subject,r.auth_time,r.created_at FROM supervisor.knowledge_discovery_heads h JOIN supervisor.knowledge_discovery_revisions r ON (r.tenant_id,r.site_id,r.inventory_id)=(h.tenant_id,h.site_id,h.inventory_id) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.inventory_id=$3 AND r.revision=CASE WHEN $4=0 THEN h.current_revision ELSE $4 END`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.SchemaVersion, &out.TemplateVersion, &out.InventoryID, &out.Revision, &out.CurrentRevision, &out.AreaID, &out.AreaRevision, &out.ProcessID, &out.ProcessRevision, &out.Capability, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DiscoveryInventory{}, ErrNotFound
	}
	if err != nil {
		return DiscoveryInventory{}, err
	}
	out.ScopeCurrent, err = discoveryScope(ctx, tx, sc, out.AreaID, out.AreaRevision, out.ProcessID, out.ProcessRevision)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT requirement_id,definition FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND revision=$4 ORDER BY requirement_key LIMIT 12`, sc.TenantID, sc.SiteID, id, out.Revision)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	out.Requirements = []DiscoveryRequirement{}
	for rows.Next() {
		var req DiscoveryRequirement
		var definition []byte
		if err = rows.Scan(&req.RequirementID, &definition); err != nil {
			rows.Close()
			return DiscoveryInventory{}, err
		}
		var d DiscoveryRequirementDefinition
		if err = json.Unmarshal(definition, &d); err != nil {
			rows.Close()
			return DiscoveryInventory{}, err
		}
		req.Key = d.Key
		req.Description = d.Description
		req.InterviewRole = d.InterviewRole
		out.Requirements = append(out.Requirements, req)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return DiscoveryInventory{}, err
	}
	template, err := DiscoveryTemplate(out.Capability)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	if len(out.Requirements) != len(template) {
		return DiscoveryInventory{}, ErrInvalidContract
	}
	return out, nil
}

func (s *Service) CreateDiscoveryInventory(ctx context.Context, a Actor, sc Scope, in DiscoveryInput, key string, expected int64) (DiscoveryInventory, error) {
	if err := in.Validate(); err != nil {
		return DiscoveryInventory{}, err
	}
	if !mutation(key, expected) {
		return DiscoveryInventory{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	defer tx.Rollback()
	auth, err := s.requireReviewer(ctx, tx, a, sc)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	// Access is rechecked even for a replay of an earlier anchor revision.
	current, err := discoveryScope(ctx, tx, sc, in.AreaID, in.AreaRevision, in.ProcessID, in.ProcessRevision)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	digest := fingerprint("discovery", in, expected)
	prior, replayed, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	if replayed {
		out, e := loadDiscoveryInventory(ctx, tx, sc, in.InventoryID, prior)
		if e != nil {
			return DiscoveryInventory{}, e
		}
		if e = audit(ctx, tx, a, sc, in.InventoryID, prior, "mutation.replayed"); e != nil {
			return DiscoveryInventory{}, e
		}
		if e = s.finishReview(ctx, tx, a); e != nil {
			return DiscoveryInventory{}, e
		}
		return out, nil
	}
	var head int64
	var areaID, processID, capability string
	err = tx.QueryRowContext(ctx, `SELECT h.current_revision,i.area_id,i.process_id,i.capability FROM supervisor.knowledge_discovery_heads h JOIN supervisor.knowledge_discovery_inventories i USING(tenant_id,site_id,inventory_id) WHERE h.tenant_id=$1 AND h.site_id=$2 AND h.inventory_id=$3 FOR UPDATE OF h`, sc.TenantID, sc.SiteID, in.InventoryID).Scan(&head, &areaID, &processID, &capability)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DiscoveryInventory{}, err
	}
	if head != expected {
		return DiscoveryInventory{}, ErrRevision
	}
	if head > 0 && (areaID != in.AreaID || processID != in.ProcessID || capability != in.Capability) {
		return DiscoveryInventory{}, ErrDiscoveryConflict
	}
	if !current {
		return DiscoveryInventory{}, ErrSourceChanged
	}
	template, err := DiscoveryTemplate(in.Capability)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	ids := map[string]string{}
	if head > 0 {
		rows, e := tx.QueryContext(ctx, `SELECT requirement_key,requirement_id FROM supervisor.knowledge_discovery_requirements WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3 AND revision=1 ORDER BY requirement_key LIMIT 12`, sc.TenantID, sc.SiteID, in.InventoryID)
		if e != nil {
			return DiscoveryInventory{}, e
		}
		for rows.Next() {
			var key, id string
			if e = rows.Scan(&key, &id); e != nil {
				rows.Close()
				return DiscoveryInventory{}, e
			}
			ids[key] = id
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return DiscoveryInventory{}, e
		}
		if len(ids) != len(template) {
			return DiscoveryInventory{}, ErrInvalidContract
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_inventories(tenant_id,site_id,inventory_id,area_id,process_id,capability) VALUES($1,$2,$3,$4,$5,$6)`, sc.TenantID, sc.SiteID, in.InventoryID, in.AreaID, in.ProcessID, in.Capability)
		if err != nil {
			return DiscoveryInventory{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_heads(tenant_id,site_id,inventory_id,current_revision) VALUES($1,$2,$3,1)`, sc.TenantID, sc.SiteID, in.InventoryID)
		if err != nil {
			return DiscoveryInventory{}, err
		}
	}
	revision := head + 1
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_revisions(tenant_id,site_id,inventory_id,revision,template_version,area_id,area_revision,process_id,process_revision,capability,reason,actor_issuer,actor_subject,auth_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, in.InventoryID, revision, DiscoveryTemplateVersion, in.AreaID, in.AreaRevision, in.ProcessID, in.ProcessRevision, in.Capability, in.Reason, a.Issuer, a.Subject, auth)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	for _, d := range template {
		id := ids[d.Key]
		if id == "" {
			if head > 0 {
				return DiscoveryInventory{}, ErrInvalidContract
			}
			id = uuid.NewString()
		}
		definition, e := json.Marshal(d)
		if e != nil {
			return DiscoveryInventory{}, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_discovery_requirements(tenant_id,site_id,inventory_id,revision,requirement_key,requirement_id,definition) VALUES($1,$2,$3,$4,$5,$6,$7)`, sc.TenantID, sc.SiteID, in.InventoryID, revision, d.Key, id, definition)
		if err != nil {
			return DiscoveryInventory{}, err
		}
	}
	if head > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_discovery_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND inventory_id=$3`, sc.TenantID, sc.SiteID, in.InventoryID, revision)
		if err != nil {
			return DiscoveryInventory{}, err
		}
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "discovery", in.InventoryID, revision); err != nil {
		return DiscoveryInventory{}, err
	}
	event := "discovery.created"
	if head > 0 {
		event = "discovery.revised"
	}
	if err = audit(ctx, tx, a, sc, in.InventoryID, revision, event); err != nil {
		return DiscoveryInventory{}, err
	}
	out, err := loadDiscoveryInventory(ctx, tx, sc, in.InventoryID, revision)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	if err = s.finishReview(ctx, tx, a); err != nil {
		return DiscoveryInventory{}, err
	}
	return out, nil
}

func (s *Service) ReadDiscoveryInventory(ctx context.Context, a Actor, sc Scope, id string, revision int64) (DiscoveryInventory, error) {
	if !contractUUID(id) || revision < 0 {
		return DiscoveryInventory{}, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	defer tx.Rollback()
	out, err := loadDiscoveryInventory(ctx, tx, sc, id, revision)
	if err != nil {
		return DiscoveryInventory{}, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "discovery.read"); err != nil {
		return DiscoveryInventory{}, err
	}
	if err = s.finish(ctx, tx, a); err != nil {
		return DiscoveryInventory{}, err
	}
	return out, nil
}

func (s *Service) ListDiscoveryInventories(ctx context.Context, a Actor, sc Scope, limit int, cursor, processID string) (DiscoveryInventoryPage, error) {
	out := DiscoveryInventoryPage{Inventories: []DiscoveryInventoryListItem{}}
	if !pageArgs(limit, cursor) || processID != "" && !contractUUID(processID) {
		return out, ErrInvalidContract
	}
	if cursor == "" {
		cursor = uuid.Nil.String()
	}
	var processFilter any
	if processID != "" {
		processFilter = processID
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT i.inventory_id,i.process_id,process.payload->>'display_name',i.capability,h.current_revision,r_first.created_at,r_current.created_at
FROM supervisor.knowledge_discovery_inventories i
JOIN supervisor.knowledge_discovery_heads h USING(tenant_id,site_id,inventory_id)
JOIN supervisor.knowledge_discovery_revisions r_current ON (r_current.tenant_id,r_current.site_id,r_current.inventory_id,r_current.revision)=(h.tenant_id,h.site_id,h.inventory_id,h.current_revision)
JOIN supervisor.knowledge_discovery_revisions r_first ON (r_first.tenant_id,r_first.site_id,r_first.inventory_id,r_first.revision)=(i.tenant_id,i.site_id,i.inventory_id,1)
JOIN supervisor.knowledge_entity_revisions area ON (area.tenant_id,area.site_id,area.id,area.revision)=(r_current.tenant_id,r_current.site_id,r_current.area_id,r_current.area_revision)
JOIN supervisor.knowledge_entity_revisions process ON (process.tenant_id,process.site_id,process.id,process.revision)=(r_current.tenant_id,r_current.site_id,r_current.process_id,r_current.process_revision)
JOIN supervisor.evidence_heads area_source ON (area_source.tenant_id,area_source.site_id,area_source.source_id)=(area.tenant_id,area.site_id,(area.payload->>'source_id')::uuid)
JOIN supervisor.evidence_heads process_source ON (process_source.tenant_id,process_source.site_id,process_source.source_id)=(process.tenant_id,process.site_id,(process.payload->>'source_id')::uuid)
WHERE i.tenant_id=$1 AND i.site_id=$2 AND i.inventory_id>$3::uuid AND ($4::uuid IS NULL OR i.process_id=$4::uuid)
 AND NOT area_source.revoked AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(area_source.tenant_id,area_source.site_id,area_source.source_id))
 AND NOT process_source.revoked AND NOT EXISTS(SELECT 1 FROM supervisor.evidence_revocations v WHERE (v.tenant_id,v.site_id,v.source_id)=(process_source.tenant_id,process_source.site_id,process_source.source_id))
ORDER BY i.inventory_id LIMIT $5`, sc.TenantID, sc.SiteID, cursor, processFilter, limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item DiscoveryInventoryListItem
		if err = rows.Scan(&item.InventoryID, &item.ProcessID, &item.ProcessDisplayName, &item.Capability, &item.CurrentRevision, &item.CreatedAt, &item.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Inventories = append(out.Inventories, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Inventories) > limit {
		out.Inventories = out.Inventories[:limit]
		out.NextCursor = out.Inventories[limit-1].InventoryID
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "discovery.listed"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
