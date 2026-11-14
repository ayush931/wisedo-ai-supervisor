package chat

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"slices"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/google/uuid"
)

const (
	StatePending = "pending"

	// ReasonNoApprovedProcess: answering is commissioned but the site has no approved process
	// snapshot, so there is nothing a cited answer could rest on and no run is started.
	ReasonNoApprovedProcess = "NO_APPROVED_PROCESS"
	// ReasonAnswersBusy: the site already has MaxRunningAnswersPerSite chat answers waiting for the
	// model (WIS-722). Chat answers share the worker's model slots with every tenant's case
	// investigations, so one site's questions must not queue without bound; the person asks again
	// once an answer has finished.
	ReasonAnswersBusy                 = "ANSWERS_BUSY"
	ReasonNoReadableEvidence          = "NO_READABLE_EVIDENCE"
	ReasonSnapshotEvidenceUnavailable = "SNAPSHOT_EVIDENCE_UNAVAILABLE"
	// ReasonCaptureLimit: no approved claim fits the passage budget because each one's cited spans
	// alone exceed MaxChatPassageBytes or agent.MaxRunPassages, and the asker can read at least one
	// source of those claims (otherwise NO_READABLE_EVIDENCE, so existence is not disclosed). Before WIS-620 it was the refusal
	// for a snapshot citing more than 70 sources, and stored messages may still carry that meaning.
	ReasonCaptureLimit = "CAPTURE_LIMIT"

	// MaxChatPassageBytes bounds the passage text one question captures, as for interview turns.
	MaxChatPassageBytes = 12 << 10
)

// AnswerSettings commission WIS-517 slice 2 answering: a local model, agent mode and the
// explicit SUPERVISOR_CHAT_ANSWERING_ENABLED switch, set only after WIS-280 qualification.
type AnswerSettings struct {
	Enabled                   bool
	Provider, Model, ModelRev string
	ProviderActual            bool
}

// WithAnswering returns a copy that starts an answer run for each question when enabled.
func (s *Service) WithAnswering(settings AnswerSettings) *Service {
	c := *s
	c.answer = settings
	return &c
}

// WithEvaluationSnapshot pins the approved snapshot for an offline regression. It changes no
// published state; the normal membership, source and approval checks still apply. HTTP callers
// never select this override.
func (s *Service) WithEvaluationSnapshot(id string) (*Service, error) {
	if !validID(id) {
		return nil, ErrInvalid
	}
	c := *s
	c.snapshotID = id
	return &c, nil
}

func selectedSnapshot(ctx context.Context, tx *sql.Tx, sc Scope, id string) (string, error) {
	if id != "" {
		return id, nil
	}
	var selected, digest string
	var revision int64
	err := tx.QueryRowContext(ctx, latestSnapshotSQL, sc.TenantID, sc.SiteID).Scan(&selected, &revision, &digest)
	return selected, err
}

// captureRefs fixes the run's read scope at question time (WIS-620). It selects the snapshot's
// approved claims most relevant to the question (selectPassages), captures the sources of their
// spans, each read under the asker's own evidence authority, and returns those spans as the run's
// passages. Missing sources are skipped; a denied read or an empty readable set refuses the run.
// The model can retrieve nothing else, and evidence.get returns only the captured passages (rule 08).
func captureRefs(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, sources *evidence.Service, question string, plan *retrievalPlan, pinnedSnapshot string) ([]agent.RunRef, []agent.RunPassage, *agent.ChatSnapshotProvenance, string, error) {
	snapshotID, err := selectedSnapshot(ctx, tx, sc, pinnedSnapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, "", nil
	}
	if err != nil {
		return nil, nil, nil, "", err
	}
	snap, err := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, snapshotID)
	if reason, fatal := captureSnapshotError(err); reason != "" || fatal != nil {
		if reason != "" {
			return nil, nil, nil, reason, nil
		}
		return nil, nil, nil, "", fatal
	}
	provenance := &agent.ChatSnapshotProvenance{SnapshotID: snap.SnapshotID, SnapshotCreatedAt: snap.CreatedAt.UTC(), SnapshotStale: snap.Stale}
	selected, method, overBudget, err := selectPassages(ctx, tx, sc, snapshotID, snap.Contents.Claims, snap.Contents.Entities, question, plan)
	if err != nil {
		return nil, nil, nil, "", err
	}
	provenance.Retrieval = method
	refs, passages := []agent.RunRef{}, []agent.RunPassage{}
	readable := map[string]bool{}
	for _, p := range selected {
		ok, seen := readable[p.SourceID]
		if !seen {
			src, e := sources.ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, p.SourceID, p.SourceRevision)
			skip, reason, e := captureSourceError(e)
			if reason != "" {
				return nil, nil, nil, reason, nil
			}
			if e != nil {
				return nil, nil, nil, "", e
			}
			ok, readable[p.SourceID] = !skip, !skip
			if ok {
				refs = append(refs, agent.RunRef{Kind: agent.RefEvidence, RefID: src.SourceID, Revision: src.Revision, SHA256: src.SHA256, AccessEpoch: src.AccessEpoch})
			}
		}
		if ok {
			passages = append(passages, p)
		}
	}
	if len(selected) == 0 {
		// Nothing fit the budget: CAPTURE_LIMIT only if the asker can read a source it passed over.
		for _, p := range overBudget {
			if _, seen := readable[p.SourceID]; seen {
				continue
			}
			_, e := sources.ReadParticipantTx(ctx, tx, evidence.Actor{Issuer: a.Issuer, Subject: a.Subject, SessionHash: a.SessionHash}, evidence.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, p.SourceID, p.SourceRevision)
			skip, reason, e := captureSourceError(e)
			if reason != "" {
				return nil, nil, nil, reason, nil
			}
			if e != nil {
				return nil, nil, nil, "", e
			}
			if !skip {
				return nil, nil, nil, ReasonCaptureLimit, nil
			}
			readable[p.SourceID] = false
		}
	}
	if len(refs) == 0 {
		return nil, nil, nil, ReasonNoReadableEvidence, nil
	}
	return append(refs, agent.RunRef{Kind: agent.RefSnapshot, RefID: snapshotID, Revision: snap.InventoryRevision, SHA256: snap.SHA256}), passages, provenance, "", nil
}

// RankSQL returns, for each cited span of this site, its source revision, its text size in bytes
// and the distinct question words (English lexemes) it contains (WIS-626). It reads the stored
// evidence_span_search rows written when the span was created (migration 073) instead of
// re-reading every revision body and re-running to_tsvector per question; the words compared are
// exactly the lexemes WIS-620 compared, so the ranking is unchanged. Rows are tenant and site
// filtered here and by forced RLS. Exported for the supervisor-load probe.
const RankSQL = `WITH q AS (SELECT tsvector_to_array(to_tsvector('english',$4)) AS words)
SELECT x.span_id::text,x.source_id::text,x.revision,x.text_bytes,
 CASE WHEN x.lexemes && q.words THEN array_to_string(ARRAY(SELECT w FROM unnest(x.lexemes) w WHERE w=ANY(q.words)),' ') ELSE '' END
FROM supervisor.evidence_span_search x CROSS JOIN q
WHERE x.tenant_id=$1 AND x.site_id=$2 AND x.span_id=ANY($3::uuid[])`

// selectPassages picks whole approved claims, never part of one, so a captured claim keeps every
// source its approval rests on. A claim matches the distinct question words (PostgreSQL 'english'
// text-search lexemes) found in the text of its cited spans or in its own terms: the predicate's
// words and the display names and aliases of its subject and object, as the snapshot approved
// them (ClaimTermsSQL; WIS-646: "the assembly cell cycle time" never matched a span that says
// "builds one seat bracket in 75 seconds"). Its score is the sum, over those words, of one over
// the number of candidate claims holding the word, exactly, so a rare word outweighs a word many
// claims share. A claim matching no word scores zero. Claims are taken
// by score, then claim ID, while their not-yet-selected spans fit agent.MaxRunPassages and
// MaxChatPassageBytes in total. A claim that does not fit is passed over and its new spans are returned as overBudget. Scores are exact rationals and
// ties break by ID, so identical inputs select identical passages.
//
// With semantic retrieval on (WIS-627) the claims are taken in the plan's order instead: the word
// order fused with the embedding order (and reranked), computed before this transaction by
// planRetrieval. The plan is used only when it ranked this same snapshot and claim set; without
// one the word order is used and labelled RetrievalWordFallback. The plan only orders claims: the
// budget, and every source read in captureRefs, run here on canonical state.
func selectPassages(ctx context.Context, tx *sql.Tx, sc Scope, snapshotID string, claims []knowledge.SnapshotClaim, entities []knowledge.SnapshotEntity, question string, plan *retrievalPlan) ([]agent.RunPassage, string, []agent.RunPassage, error) {
	if retrieval.Default() == nil {
		p, overBudget, err := selectWith(ctx, tx, sc, claims, entities, question, RankSQL, nil)
		return p, RetrievalWord, overBudget, err
	}
	method := RetrievalWordFallback
	p, overBudget, err := selectWith(ctx, tx, sc, claims, entities, question, RankSQL, func(order []rankedClaim, _ []string) ([]rankedClaim, error) {
		if plan == nil || plan.snapshotID != snapshotID || !sameClaims(order, plan.fused) {
			return order, nil
		}
		method = RetrievalHybrid
		if plan.unreranked {
			method = RetrievalHybridUnreranked
		}
		return plan.fused, nil
	})
	return p, method, overBudget, err
}

// sameClaims reports whether two orders hold the same claims with the same cited spans.
func sameClaims(x, y []rankedClaim) bool {
	if len(x) != len(y) {
		return false
	}
	spans := map[string][]string{}
	for _, c := range x {
		spans[c.id] = c.spans
	}
	for _, c := range y {
		if s, ok := spans[c.id]; !ok || !slices.Equal(s, c.spans) {
			return false
		}
	}
	return true
}

// latestSnapshotSQL is the site's newest process snapshot, the one a question is answered from.
const latestSnapshotSQL = `SELECT snapshot_id::text,inventory_revision,sha256 FROM supervisor.knowledge_process_snapshots WHERE tenant_id=$1 AND site_id=$2 ORDER BY created_at DESC,snapshot_id DESC LIMIT 1`

// retrievalPlan is one question's semantic claim order, pinned to the snapshot it ranked.
type retrievalPlan struct {
	snapshotID string
	fused      []rankedClaim
	// unreranked: a rerank model is configured but did not answer (error or timeout), so the
	// fused order stands unreranked; the reply says so (WIS-647).
	unreranked bool
}

// planRetrieval computes the semantic order before the posting transaction (WIS-627 review): the
// embedding and rerank calls never run while the site write lock or a source lock is held, so a
// slow endpoint cannot block other posts, evidence writes or revocations. It embeds the question,
// ranks the latest snapshot's claims in a short read transaction, closes it, then reranks. Any
// failure returns nil (word fallback); the posting transaction re-reads all canonical state and
// reports its own errors. The embed call, the read and the rerank call share one deadline of the
// configured timeout (WIS-700): with separate per-call budgets a question could wait about twice
// the timeout before it is posted. A rerank cut short by it leaves the order hybrid_unreranked.
func (s *Service) planRetrieval(ctx context.Context, a Actor, sc Scope, question string) *retrievalPlan {
	c := retrieval.Default()
	if c == nil {
		return nil
	}
	if d := c.Timeout(); d > 0 { // a zero timeout means none, not an already expired deadline
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	vecs, err := c.Embed(ctx, []string{question})
	if err != nil {
		return nil
	}
	tx, err := s.begin(ctx, a, sc, false)
	if err != nil {
		return nil
	}
	plan, texts, err := func() (*retrievalPlan, []string, error) {
		defer tx.Rollback()
		snapshotID, err := selectedSnapshot(ctx, tx, sc, s.snapshotID)
		if err != nil {
			return nil, nil, err
		}
		snap, err := knowledge.LoadSnapshotTx(ctx, tx, knowledge.Scope{TenantID: sc.TenantID, SiteID: sc.SiteID}, snapshotID)
		if err != nil {
			return nil, nil, err
		}
		var plan *retrievalPlan
		var texts []string
		_, _, err = selectWith(ctx, tx, sc, snap.Contents.Claims, snap.Contents.Entities, question, RankSQL, func(order []rankedClaim, ids []string) ([]rankedClaim, error) {
			fused, ok, err := fuse(ctx, tx, c, sc, order, ids, vecs[0])
			if err != nil || !ok {
				return order, err
			}
			plan = &retrievalPlan{snapshotID: snapshotID, fused: fused}
			if c.Reranks() {
				texts, err = claimTexts(ctx, tx, sc, fused[:min(len(fused), rerankTop)])
			}
			return order, err
		})
		return plan, texts, err
	}()
	if err != nil || plan == nil {
		return nil
	}
	// The read transaction is closed: the rerank call holds no lock.
	scores, err := c.Rerank(ctx, question, texts)
	if c.Reranks() && len(texts) > 0 && (err != nil || scores == nil) {
		plan.unreranked = true
	}
	if err == nil && scores != nil {
		top := plan.fused[:len(texts)]
		score := map[string]float64{}
		for i, cl := range top {
			score[cl.id] = scores[i]
		}
		slices.SortStableFunc(top, func(x, y rankedClaim) int {
			if score[x.id] != score[y.id] {
				if score[x.id] > score[y.id] {
					return -1
				}
				return 1
			}
			return strings.Compare(x.id, y.id)
		})
	}
	return plan
}

// Retrieval methods recorded on a chat reply (WIS-627).
const (
	RetrievalWord         = "word"          // semantic retrieval is off
	RetrievalHybrid       = "hybrid"        // word and embedding order fused (and reranked)
	RetrievalWordFallback = "word_fallback" // semantic retrieval is on but could not be used
	// RetrievalHybridUnreranked: the hybrid order was used, but the configured rerank model did not
	// answer (WIS-647). Before, such a reply was labelled hybrid and the skip was invisible.
	RetrievalHybridUnreranked = "hybrid_unreranked"
)

// rankedClaim is one approved claim in ranking order with its cited spans.
type rankedClaim struct {
	id    string
	score *big.Rat // the sum, over matched question words, of 1/(claims holding the word)
	spans []string
}

// rrfK is the reciprocal-rank fusion constant (the usual 60).
const rrfK = 60

// rerankTop bounds how many fused claims go to the rerank model.
const rerankTop = 20

// fuse fuses the word order with the embedding order of the question vector. It makes no model
// call. ok is false (word order kept) when a candidate span has no embedding of the current
// revision; err is a database failure.
func fuse(ctx context.Context, tx *sql.Tx, c *retrieval.Client, sc Scope, order []rankedClaim, ids []string, query []float32) ([]rankedClaim, bool, error) {
	sims, complete, err := retrieval.SimilarityTx(ctx, tx, c, sc.TenantID, sc.SiteID, ids, query)
	if err != nil {
		return nil, false, err
	}
	if !complete {
		return nil, false, nil
	}
	best := map[string]float64{}
	for _, cl := range order {
		b := -2.0
		for _, id := range cl.spans {
			b = max(b, sims[id])
		}
		best[cl.id] = b
	}
	semantic := slices.Clone(order)
	slices.SortStableFunc(semantic, func(x, y rankedClaim) int {
		if best[x.id] != best[y.id] {
			if best[x.id] > best[y.id] {
				return -1
			}
			return 1
		}
		return strings.Compare(x.id, y.id)
	})
	// Tied claims share a rank in each list (competition ranking): an order among equal scores is
	// only the ID tie-break and must not decide the fusion.
	fusedScore := map[string]float64{}
	for i, cl := range order {
		rank := i
		for rank > 0 && order[rank-1].score.Cmp(cl.score) == 0 {
			rank--
		}
		fusedScore[cl.id] += 1 / float64(rrfK+rank+1)
	}
	for i, cl := range semantic {
		rank := i
		for rank > 0 && best[semantic[rank-1].id] == best[cl.id] {
			rank--
		}
		fusedScore[cl.id] += 1 / float64(rrfK+rank+1)
	}
	fused := slices.Clone(order)
	slices.SortStableFunc(fused, func(x, y rankedClaim) int {
		if fusedScore[x.id] != fusedScore[y.id] {
			if fusedScore[x.id] > fusedScore[y.id] {
				return -1
			}
			return 1
		}
		return strings.Compare(x.id, y.id)
	})
	return fused, true, nil
}

// claimTexts returns each claim's cited span texts, joined, for the reranker.
func claimTexts(ctx context.Context, tx *sql.Tx, sc Scope, claims []rankedClaim) ([]string, error) {
	var ids []string
	for _, c := range claims {
		ids = append(ids, c.spans...)
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id::text,substr(r.body,s.start_rune+1,s.end_rune-s.start_rune) FROM supervisor.evidence_spans s
 JOIN supervisor.evidence_revisions r ON (r.tenant_id,r.site_id,r.source_id,r.revision)=(s.tenant_id,s.site_id,s.source_id,s.revision)
 WHERE s.tenant_id=$1 AND s.site_id=$2 AND s.id=ANY($3::uuid[])`, sc.TenantID, sc.SiteID, ids)
	if err != nil {
		return nil, err
	}
	text := map[string]string{}
	for rows.Next() {
		var id, t string
		if err = rows.Scan(&id, &t); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		text[id] = t
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]string, len(claims))
	for i, c := range claims {
		parts := make([]string, 0, len(c.spans))
		for _, id := range c.spans {
			parts = append(parts, text[id])
		}
		out[i] = strings.Join(parts, "\n")
	}
	return out, nil
}

// SelectPassagesUsing is selectPassages with the ranking query given: RankSQL in the product, or
// another query returning the same columns, so the supervisor-load probe can time the WIS-620
// on-the-fly ranking against the indexed one and check both select the same passages.
func SelectPassagesUsing(ctx context.Context, tx *sql.Tx, sc Scope, claims []knowledge.SnapshotClaim, entities []knowledge.SnapshotEntity, question, rankSQL string) ([]agent.RunPassage, error) {
	p, _, err := selectWith(ctx, tx, sc, claims, entities, question, rankSQL, nil)
	return p, err
}

// ClaimTermsSQL returns, for each claim term text ($2, in order), the distinct question words
// ($1, English lexemes, stemmed as RankSQL stems them) it contains. It reads no table: the texts
// come from the approved snapshot the caller loaded, never from live (possibly unreviewed)
// entity rows.
const ClaimTermsSQL = `WITH q AS (SELECT tsvector_to_array(to_tsvector('english',$1)) AS words)
SELECT x.i::int,array_to_string(ARRAY(SELECT w FROM unnest(tsvector_to_array(to_tsvector('english',x.t))) w WHERE w=ANY(q.words)),' ')
FROM unnest($2::text[]) WITH ORDINALITY AS x(t,i) CROSS JOIN q`

// ClaimTermTexts is, per claim in order, the text ClaimTermsSQL matches: see claimTerms. Exported
// for the WIS-700 load probe, which times and explains ClaimTermsSQL on these texts.
func ClaimTermTexts(claims []knowledge.SnapshotClaim, entities []knowledge.SnapshotEntity) []string {
	names := map[string][]string{}
	for _, e := range entities {
		names[e.EntityID] = append([]string{e.Name}, e.Aliases...)
	}
	texts := make([]string, len(claims))
	for i, c := range claims {
		texts[i] = claimTerms(c, names)
	}
	return texts
}

// claimTerms is the text a claim is known by besides its spans: its predicate's words and the
// snapshot's display names and aliases of its subject and object.
func claimTerms(c knowledge.SnapshotClaim, names map[string][]string) string {
	parts := []string{strings.ReplaceAll(c.Predicate, "_", " ")}
	parts = append(parts, names[c.SubjectID]...)
	if c.ObjectEntityID != "" {
		parts = append(parts, names[c.ObjectEntityID]...)
	}
	return strings.Join(parts, " ")
}

// rankClaims orders claims by score, then claim ID. words[i] holds claim i's matched question
// words (from its cited spans and its own terms; repeats are counted once). A word weighs
// 1/(claims holding it), exactly (rule 04): a word every lead-time claim holds ("lead", "time")
// weighs little, a word one entity's claims hold ("fasteners") a lot. Plain counting let generic
// predicate words crowd specific matches out of the passage cap (WIS-646). df counts every claim
// given, including one selectWith later passes over because a cited span is outside the site:
// such a claim still dilutes the words it holds. It is pure: no clock, database or randomness.
func rankClaims(claims []knowledge.SnapshotClaim, words [][]string) []rankedClaim {
	matched := make([]map[string]bool, len(claims))
	df := map[string]int64{} // how many claims hold each word
	for i := range claims {
		set := map[string]bool{}
		for _, w := range words[i] {
			set[w] = true
		}
		for w := range set {
			df[w]++
		}
		matched[i] = set
	}
	order := make([]rankedClaim, 0, len(claims))
	for i, c := range claims {
		score := new(big.Rat)
		for w := range matched[i] {
			score.Add(score, big.NewRat(1, df[w]))
		}
		order = append(order, rankedClaim{c.ClaimID, score, c.SourceSpanIDs})
	}
	slices.SortStableFunc(order, func(x, y rankedClaim) int {
		if c := y.score.Cmp(x.score); c != 0 {
			return c
		}
		return strings.Compare(x.id, y.id)
	})
	return order
}

func selectWith(ctx context.Context, tx *sql.Tx, sc Scope, claims []knowledge.SnapshotClaim, entities []knowledge.SnapshotEntity, question, rankSQL string, reorder func([]rankedClaim, []string) ([]rankedClaim, error)) ([]agent.RunPassage, []agent.RunPassage, error) {
	var ids []string
	for _, c := range claims {
		ids = append(ids, c.SourceSpanIDs...)
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	rows, err := tx.QueryContext(ctx, rankSQL, sc.TenantID, sc.SiteID, ids, question)
	if err != nil {
		return nil, nil, err
	}
	type span struct {
		passage agent.RunPassage
		size    int
		words   []string
	}
	spans := map[string]span{}
	for rows.Next() {
		var sp span
		var words string
		if err = rows.Scan(&sp.passage.SpanID, &sp.passage.SourceID, &sp.passage.SourceRevision, &sp.size, &words); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		sp.words = strings.Fields(words)
		spans[sp.passage.SpanID] = sp
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	terms := make([][]string, len(claims))
	rows, err = tx.QueryContext(ctx, ClaimTermsSQL, question, ClaimTermTexts(claims, entities))
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var i int
		var words string
		if err = rows.Scan(&i, &words); err != nil {
			return nil, nil, errors.Join(err, rows.Close())
		}
		if i >= 1 && i <= len(claims) {
			terms[i-1] = strings.Fields(words)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	words := make([][]string, len(claims))
	for i, c := range claims {
		words[i] = terms[i]
		for _, id := range c.SourceSpanIDs {
			words[i] = append(words[i], spans[id].words...)
		}
	}
	order := rankClaims(claims, words)
	if reorder != nil {
		if order, err = reorder(order, ids); err != nil {
			return nil, nil, err
		}
	}
	out, chosen, total := []agent.RunPassage{}, map[string]bool{}, 0
	var overBudget []agent.RunPassage
	for _, c := range order {
		var add []string
		size := 0
		for _, id := range c.spans {
			sp, ok := spans[id]
			if !ok {
				// A cited span outside this site cannot be read; the claim is not offered.
				add = nil
				break
			}
			if !chosen[id] && !slices.Contains(add, id) {
				add, size = append(add, id), size+sp.size
			}
		}
		if len(add) == 0 {
			continue
		}
		if len(out)+len(add) > agent.MaxRunPassages || total+size > MaxChatPassageBytes {
			for _, id := range add {
				overBudget = append(overBudget, spans[id].passage)
			}
			continue
		}
		for _, id := range add {
			chosen[id], out = true, append(out, spans[id].passage)
		}
		total += size
	}
	return out, overBudget, nil
}

func captureSnapshotError(err error) (reason string, fatal error) {
	switch {
	case errors.Is(err, knowledge.ErrSnapshotEvidenceUnavailable):
		return ReasonSnapshotEvidenceUnavailable, nil
	case errors.Is(err, knowledge.ErrNotFound):
		return ReasonNoReadableEvidence, nil
	default:
		return "", err
	}
}

func captureSourceError(err error) (skip bool, reason string, fatal error) {
	switch {
	case errors.Is(err, evidence.ErrNotFound):
		return true, "", nil
	case errors.Is(err, evidence.ErrDenied):
		// One denied source refuses the whole question so omitted approved evidence cannot mislead the answer (rule 06).
		return false, ReasonNoReadableEvidence, nil
	default:
		return false, "", err
	}
}

// MaxRunningAnswersPerSite bounds a site's chat answers still running (WIS-722), as
// MaxActivePerSite bounds its case investigations.
const MaxRunningAnswersPerSite = 2

// startAnswer creates the captured conversation run for one question inside the posting
// transaction. It returns the run ID, or "" and a reason when no run can start.
func (s *Service) startAnswer(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, conversationID, requestID, question string, plan *retrievalPlan) (string, string, *agent.ChatSnapshotProvenance, error) {
	// Posts hold the site write lock (begin(..., true)), so this count cannot race another post at
	// the site. A run older than its wall time does not count, so a run left "running" by a lost
	// worker stops holding a place. It can no longer execute: Runtime.ChatAnswer clamps its deadline
	// to started_at + WallTime and ends an expired run without a model call.
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.chat_answer_turns t
 JOIN supervisor.agent_runs r ON (r.tenant_id,r.site_id,r.id)=(t.tenant_id,t.site_id,t.run_id)
 WHERE t.tenant_id=$1 AND t.site_id=$2 AND r.state='running' AND r.started_at>clock_timestamp()-make_interval(secs=>$3)`,
		sc.TenantID, sc.SiteID, agent.InterviewLimits().WallTime.Seconds()).Scan(&running); err != nil {
		return "", "", nil, err
	}
	if running >= MaxRunningAnswersPerSite {
		return "", ReasonAnswersBusy, nil, nil
	}
	refs, passages, provenance, reason, err := captureRefs(ctx, tx, a, sc, evidence.New(s.db).WithSessionPolicy(s.sessions), question, plan, s.snapshotID)
	if err != nil {
		return "", "", nil, err
	}
	if reason != "" {
		return "", reason, nil, nil
	}
	if len(refs) == 0 {
		return "", ReasonNoApprovedProcess, nil, nil
	}
	run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestConversation, ConversationID: conversationID, RequestID: requestID, State: agent.RunRunning,
		TraceID: agent.NewTraceID(), ProviderName: s.answer.Provider, ProviderActual: s.answer.ProviderActual, Model: s.answer.Model, ModelRevision: s.answer.ModelRev,
		PromptVersion: agent.ChatPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
	rs := agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}
	if err = agent.CreateRunTx(ctx, tx, rs, run, agent.InterviewLimits()); err != nil {
		return "", "", nil, err
	}
	if err = agent.ReserveInvocationTx(ctx, tx, rs, run.ID); err != nil {
		return "", "", nil, err
	}
	if err = agent.AddRefsTx(ctx, tx, rs, run.ID, refs); err != nil {
		return "", "", nil, err
	}
	if err = agent.AddPassagesTx(ctx, tx, rs, run.ID, passages); err != nil {
		return "", "", nil, err
	}
	return run.ID, "", provenance, nil
}
