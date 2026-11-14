package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrResolutionBlocked = errors.New("conflict resolution blocked")

type ResolutionInput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
type ResolutionRef struct {
	ConflictID   string `json:"conflict_id"`
	ResolutionID string `json:"resolution_id"`
	Revision     int64  `json:"revision"`
}
type Resolution struct {
	SchemaVersion           int              `json:"schema_version"`
	ResolutionID            string           `json:"resolution_id"`
	ConflictID              string           `json:"conflict_id"`
	Revision                int64            `json:"revision"`
	CurrentRevision         int64            `json:"current_revision"`
	Decision                string           `json:"decision"`
	Left                    ConflictClaimRef `json:"left"`
	Right                   ConflictClaimRef `json:"right"`
	Reason                  string           `json:"reason"`
	ActorIssuer             string           `json:"actor_issuer"`
	ActorSubject            string           `json:"actor_subject"`
	AuthenticatedAt         time.Time        `json:"authenticated_at"`
	CreatedAt               time.Time        `json:"created_at"`
	AuthorityDecisionID     string           `json:"authority_decision_id,omitempty"`
	AuthoritySourceID       string           `json:"authority_source_id,omitempty"`
	AuthoritySourceRevision int64            `json:"authority_source_revision,omitempty"`
	AuthorityRevision       int64            `json:"authority_revision,omitempty"`
	ResolutionEligible      bool             `json:"resolution_eligible"`
}

func resolutionPair(ctx context.Context, tx *sql.Tx, sc Scope, id string) (Conflict, error) {
	var c Conflict
	err := tx.QueryRowContext(ctx, `SELECT id,left_claim_id,left_revision,right_claim_id,right_revision FROM supervisor.knowledge_conflicts WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&c.ID, &c.Left.ClaimID, &c.Left.Revision, &c.Right.ClaimID, &c.Right.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// The caller holds the exclusive site lock. Plain dependency reads avoid taking
// peer claim locks out of order against normal globally sorted conflict readers.
func resolutionPairCurrent(ctx context.Context, tx *sql.Tx, sc Scope, c Conflict) (bool, error) {
	current := true
	for _, ref := range []ConflictClaimRef{c.Left, c.Right} {
		claim, err := loadClaim(ctx, tx, sc, ref.ClaimID, ref.Revision)
		if err != nil {
			return false, err
		}
		refs, _, _, err := claimRefs(ctx, tx, sc, claim.ClaimInput)
		if err != nil {
			return false, err
		}
		for _, source := range refs {
			var revision int64
			var revoked bool
			err = tx.QueryRowContext(ctx, `SELECT current_revision,(revoked OR EXISTS(SELECT 1 FROM supervisor.evidence_revocations rv WHERE (rv.tenant_id,rv.site_id,rv.source_id)=(h.tenant_id,h.site_id,h.source_id))) FROM supervisor.evidence_heads h WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, sc.TenantID, sc.SiteID, source.id).Scan(&revision, &revoked)
			if errors.Is(err, sql.ErrNoRows) || revoked {
				return false, ErrNotFound
			}
			if err != nil {
				return false, err
			}
			current = current && revision == source.revision
		}
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT `+currentClaimSQL("r")+` FROM supervisor.knowledge_claim_revisions r WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.claim_id=$3 AND r.revision=$4`, sc.TenantID, sc.SiteID, ref.ClaimID, ref.Revision).Scan(&active)
		if err != nil {
			return false, err
		}
		current = current && active
	}
	return current, nil
}
func resolutionHead(ctx context.Context, tx *sql.Tx, sc Scope, id string) (int64, error) {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT current_revision FROM supervisor.knowledge_resolution_heads WHERE tenant_id=$1 AND site_id=$2 AND conflict_id=$3`, sc.TenantID, sc.SiteID, id).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}
func loadResolution(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64) (Resolution, error) {
	var out Resolution
	current, err := resolutionHead(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	if revision == 0 {
		revision = current
	}
	var authorityID, sourceID sql.NullString
	var sourceRevision, authorityRevision sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT d.schema_version,d.resolution_id,d.conflict_id,d.revision,d.decision,d.reason,d.actor_issuer,d.actor_subject,d.auth_time,d.created_at,d.authority_decision_id,d.authority_source_id,d.authority_source_revision,d.authority_revision,c.left_claim_id,c.left_revision,c.right_claim_id,c.right_revision FROM supervisor.knowledge_resolution_decisions d JOIN supervisor.knowledge_conflicts c ON (c.tenant_id,c.site_id,c.id)=(d.tenant_id,d.site_id,d.conflict_id) WHERE d.tenant_id=$1 AND d.site_id=$2 AND d.conflict_id=$3 AND d.revision=$4`, sc.TenantID, sc.SiteID, id, revision).Scan(&out.SchemaVersion, &out.ResolutionID, &out.ConflictID, &out.Revision, &out.Decision, &out.Reason, &out.ActorIssuer, &out.ActorSubject, &out.AuthenticatedAt, &out.CreatedAt, &authorityID, &sourceID, &sourceRevision, &authorityRevision, &out.Left.ClaimID, &out.Left.Revision, &out.Right.ClaimID, &out.Right.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CurrentRevision = current
	out.AuthorityDecisionID = authorityID.String
	out.AuthoritySourceID = sourceID.String
	out.AuthoritySourceRevision = sourceRevision.Int64
	out.AuthorityRevision = authorityRevision.Int64
	return out, nil
}
func resolutionWinner(out Resolution) (ConflictClaimRef, bool) {
	if out.Decision == "prefer_left" {
		return out.Left, true
	}
	if out.Decision == "prefer_right" {
		return out.Right, true
	}
	return ConflictClaimRef{}, false
}
func resolutionLosersClear(ctx context.Context, tx *sql.Tx, sc Scope, out Resolution) (bool, error) {
	winner, hasWinner := resolutionWinner(out)
	for _, ref := range []ConflictClaimRef{out.Left, out.Right} {
		if hasWinner && ref == winner {
			continue
		}
		review, err := loadReview(ctx, tx, sc, ref.ClaimID, ref.Revision)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if review.Verdict == "approve" && review.Revocation == nil {
			return false, nil
		}
	}
	return true, nil
}
func resolutionEligibility(ctx context.Context, tx *sql.Tx, sc Scope, out Resolution, current bool) (bool, error) {
	if !current || out.Revision != out.CurrentRevision || out.Decision == "withdraw" {
		return false, nil
	}
	clear, err := resolutionLosersClear(ctx, tx, sc, out)
	if err != nil || !clear {
		return false, err
	}
	winner, hasWinner := resolutionWinner(out)
	if !hasWinner {
		return out.Decision == "neither", nil
	}
	_, found, err := reviewAuthority(ctx, tx, sc, winner.ClaimID, winner.Revision, out.AuthorityDecisionID)
	return found, err
}
func (s *Service) DecideResolution(ctx context.Context, a Actor, sc Scope, id string, in ResolutionInput, key string, expected int64) (Resolution, error) {
	var out Resolution
	if !contractUUID(id) || !mutation(key, expected) || !contractText(in.Reason, decisionReasonChars) || strings.TrimSpace(in.Reason) == "" {
		return out, ErrInvalidContract
	}
	switch in.Decision {
	case "prefer_left", "prefer_right", "neither", "withdraw":
	default:
		return out, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, true, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	auth, err := s.requireReviewer(ctx, tx, a, sc)
	if err != nil {
		return out, err
	}
	pair, err := resolutionPair(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	current, err := resolutionPairCurrent(ctx, tx, sc, pair)
	if err != nil {
		return out, err
	}
	latest, err := resolutionHead(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	digest := fingerprint("resolution", struct {
		ConflictID string
		Input      ResolutionInput
	}{id, in}, expected)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, err
	}
	if ok {
		out, err = loadResolution(ctx, tx, sc, id, prior)
		if err != nil {
			return out, err
		}
		out.ResolutionEligible, err = resolutionEligibility(ctx, tx, sc, out, current)
		if err != nil {
			return out, err
		}
		if err = audit(ctx, tx, a, sc, id, prior, "mutation.replayed"); err != nil {
			return out, err
		}
		return out, s.finishReview(ctx, tx, a)
	}
	if latest != expected {
		return out, ErrRevision
	}
	out = Resolution{ConflictID: id, Revision: latest + 1, CurrentRevision: latest + 1, Decision: in.Decision, Left: pair.Left, Right: pair.Right}
	var pin Review
	if in.Decision == "withdraw" {
		if latest == 0 {
			return Resolution{}, ErrInvalidContract
		}
		previous, e := loadResolution(ctx, tx, sc, id, latest)
		if e != nil {
			return Resolution{}, e
		}
		if previous.Decision == "withdraw" {
			return Resolution{}, ErrInvalidContract
		}
	} else {
		if !current {
			return Resolution{}, ErrSourceChanged
		}
		clear, e := resolutionLosersClear(ctx, tx, sc, out)
		if e != nil {
			return Resolution{}, e
		}
		if !clear {
			return Resolution{}, ErrResolutionBlocked
		}
		if winner, hasWinner := resolutionWinner(out); hasWinner {
			var found bool
			pin, found, err = reviewAuthority(ctx, tx, sc, winner.ClaimID, winner.Revision, "")
			if err != nil {
				return Resolution{}, err
			}
			if !found {
				return Resolution{}, ErrResolutionBlocked
			}
		}
	}
	if latest == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_resolution_heads(tenant_id,site_id,conflict_id,current_revision) VALUES($1,$2,$3,$4)`, sc.TenantID, sc.SiteID, id, out.Revision)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE supervisor.knowledge_resolution_heads SET current_revision=$4 WHERE tenant_id=$1 AND site_id=$2 AND conflict_id=$3`, sc.TenantID, sc.SiteID, id, out.Revision)
	}
	if err != nil {
		return Resolution{}, err
	}
	var authorityID, sourceID, sourceRevision, authorityRevision any
	if pin.AuthorityDecisionID != "" {
		authorityID = pin.AuthorityDecisionID
		sourceID = pin.AuthoritySourceID
		sourceRevision = pin.AuthoritySourceRevision
		authorityRevision = pin.AuthorityRevision
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.knowledge_resolution_decisions(tenant_id,site_id,conflict_id,revision,resolution_id,decision,reason,actor_issuer,actor_subject,auth_time,authority_decision_id,authority_source_id,authority_source_revision,authority_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, sc.TenantID, sc.SiteID, id, out.Revision, uuid.NewString(), in.Decision, in.Reason, a.Issuer, a.Subject, auth, authorityID, sourceID, sourceRevision, authorityRevision)
	if err != nil {
		return Resolution{}, err
	}
	if err = saveReceipt(ctx, tx, a, sc, key, digest, "resolution", id, out.Revision); err != nil {
		return Resolution{}, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "resolution.decided"); err != nil {
		return Resolution{}, err
	}
	out, err = loadResolution(ctx, tx, sc, id, out.Revision)
	if err != nil {
		return Resolution{}, err
	}
	out.ResolutionEligible = in.Decision != "withdraw"
	return out, s.finishReview(ctx, tx, a)
}
func (s *Service) ReadResolution(ctx context.Context, a Actor, sc Scope, id string, revision int64) (Resolution, error) {
	var out Resolution
	if !contractUUID(id) || revision < 0 {
		return out, ErrInvalidContract
	}
	tx, err := s.beginLocked(ctx, a, sc, false, true)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	pair, err := resolutionPair(ctx, tx, sc, id)
	if err != nil {
		return out, err
	}
	current, err := resolutionPairCurrent(ctx, tx, sc, pair)
	if err != nil {
		return out, err
	}
	out, err = loadResolution(ctx, tx, sc, id, revision)
	if err != nil {
		return out, err
	}
	out.ResolutionEligible, err = resolutionEligibility(ctx, tx, sc, out, current)
	if err != nil {
		return out, err
	}
	if err = audit(ctx, tx, a, sc, id, out.Revision, "resolution.read"); err != nil {
		return out, err
	}
	return out, s.finish(ctx, tx, a)
}
func loadReviewResolutionRefs(ctx context.Context, tx *sql.Tx, sc Scope, reviewID string) ([]ResolutionRef, error) {
	refs := []ResolutionRef{}
	rows, err := tx.QueryContext(ctx, `SELECT conflict_id,resolution_id,resolution_revision FROM supervisor.knowledge_review_resolution_refs WHERE tenant_id=$1 AND site_id=$2 AND review_id=$3 ORDER BY conflict_id LIMIT 1001`, sc.TenantID, sc.SiteID, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref ResolutionRef
		if err = rows.Scan(&ref.ConflictID, &ref.ResolutionID, &ref.Revision); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(refs) > 1000 {
		return nil, ErrTraversalLimit
	}
	return refs, nil
}

// Every historical pair needs its own exact pin. A later pair or replacement
// decision cannot silently restore an older human approval.
func reviewResolutionPins(ctx context.Context, tx *sql.Tx, sc Scope, id string, revision int64, expected []ResolutionRef, requirePins bool) ([]ResolutionRef, bool, error) {
	refs := []ResolutionRef{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM supervisor.knowledge_conflicts WHERE tenant_id=$1 AND site_id=$2 AND ((left_claim_id=$3 AND left_revision=$4) OR (right_claim_id=$3 AND right_revision=$4)) ORDER BY id LIMIT 1001`, sc.TenantID, sc.SiteID, id, revision)
	if err != nil {
		return nil, false, err
	}
	ids := []string{}
	for rows.Next() {
		var cid string
		if err = rows.Scan(&cid); err != nil {
			rows.Close()
			return nil, false, err
		}
		ids = append(ids, cid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	if len(ids) > 1000 {
		return nil, false, ErrTraversalLimit
	}
	pins := map[string]ResolutionRef{}
	for _, ref := range expected {
		pins[ref.ConflictID] = ref
	}
	if requirePins && len(pins) != len(ids) {
		return refs, false, nil
	}
	for _, cid := range ids {
		pin, exists := pins[cid]
		if requirePins && !exists {
			return refs, false, nil
		}
		selected := int64(0)
		if requirePins {
			selected = pin.Revision
		}
		decision, e := loadResolution(ctx, tx, sc, cid, selected)
		if errors.Is(e, ErrNotFound) {
			return refs, false, nil
		}
		if e != nil {
			return nil, false, e
		}
		if requirePins && (pin.ResolutionID != decision.ResolutionID || pin.Revision != decision.CurrentRevision) {
			return refs, false, nil
		}
		winner, hasWinner := resolutionWinner(decision)
		if !hasWinner || winner.ClaimID != id || winner.Revision != revision {
			return refs, false, nil
		}
		pair := Conflict{ID: cid, Left: decision.Left, Right: decision.Right}
		current, e := resolutionPairCurrent(ctx, tx, sc, pair)
		if errors.Is(e, ErrNotFound) {
			return refs, false, nil
		}
		if e != nil {
			return nil, false, e
		}
		valid, e := resolutionEligibility(ctx, tx, sc, decision, current)
		if e != nil {
			return nil, false, e
		}
		if !valid {
			return refs, false, nil
		}
		refs = append(refs, ResolutionRef{ConflictID: cid, ResolutionID: decision.ResolutionID, Revision: decision.Revision})
	}
	return refs, true, nil
}
