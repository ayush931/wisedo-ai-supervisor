package agent

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

type ChatClaimRef struct {
	ClaimID  string `json:"claim_id" contract:"uuid"`
	Revision int64  `json:"revision"`
}

type ChatClaimCitation struct {
	ChatClaimRef
	Statement     int      `json:"statement"`
	SourceSpanIDs []string `json:"source_span_ids"`
}

// Claims sharing a source are not necessarily in the selected passages.
func capturedChatClaims(ctx context.Context, tx *sql.Tx, sc RunScope, runID string, contents knowledge.SnapshotContents, captured map[capturedSource]bool) ([]knowledge.SnapshotClaim, error) {
	selected := map[capturedSource]map[string]bool{}
	for src := range captured {
		passages, err := runPassages(ctx, tx, sc, runID, src.id, src.revision)
		if err != nil {
			return nil, err
		}
		ids := map[string]bool{}
		for _, p := range passages {
			ids[p.id] = true
		}
		selected[src] = ids
	}
	allowed := map[string]bool{}
	for _, span := range contents.Spans {
		src := capturedSource{span.SourceID, span.Revision}
		ids, ok := selected[src]
		if ok && (len(ids) == 0 || ids[span.SpanID]) {
			allowed[span.SpanID] = true
		}
	}
	out := []knowledge.SnapshotClaim{}
	for _, c := range readableChatClaims(contents, captured) {
		if !slices.ContainsFunc(c.SourceSpanIDs, func(id string) bool { return !allowed[id] }) {
			out = append(out, c)
		}
	}
	return out, nil
}

// chatFactEntity names an entity of an approved fact as the pinned snapshot recorded it.
type chatFactEntity struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"display_name,omitempty"`
}

// chatFact is one approved fact of the brief. It refers to its entities by ID (chatFacts.Entities)
// and to the sources of its spans through chatFacts.Evidence.
type chatFact struct {
	ChatClaimRef
	SubjectID     string             `json:"subject_id"`
	Predicate     string             `json:"predicate"`
	ObjectID      string             `json:"object_id,omitempty"`
	Literal       *knowledge.Literal `json:"literal,omitempty"`
	SourceSpanIDs []string           `json:"source_span_ids"`
}

// chatFacts is the brief's index of the facts readable in this turn. Each entity is listed once and
// each span's evidence ID once, so a station card backing several facts costs one name and one
// evidence ID, not one per fact (WIS-712 review, finding 1).
type chatFacts struct {
	Entities []chatFactEntity  `json:"entities"`
	Evidence map[string]string `json:"evidence_by_span"`
	Facts    []chatFact        `json:"approved_facts"`
}

// chatApprovedFacts supplies only the exact readable facts and the entities they name, not unrelated
// snapshot identities or review metadata. Names come from the pinned snapshot's entities, never from
// live rows, so facts sharing a predicate can be told apart (WIS-712). Evidence maps each fact span
// to its source; for a readable claim every one is a captured ref (readableChatClaims). Aliases are
// left out: up to 20 of 200 runes per entity would crowd the brief's input budget.
//
// claims are the facts readable in this turn. A name is sent only for an entity the reader can read
// something about (rules 03 and 11): the subject of a readable claim, or an object whose own source
// is captured for the reader. Any other entity, such as the object of a relation whose own facts
// are withheld, is sent by ID without its name. A fact pinning an entity revision other than the
// snapshot's withholds that entity's name from the whole index, since the entity is listed once.
//
// Facts are ordered by the best rank (runPassageRanks) of a selected passage among their spans, so
// the facts on the passages ranked first for the question come first; ties, and facts with no ranked
// span, keep snapshot order. The index takes the leading facts whose JSON fits maxBytes
// (chatIndexBytes). Facts left out of the index are still captured: their passages stay readable
// and citable, and approval is still derived server-side from the spans.
func chatApprovedFacts(claims []knowledge.SnapshotClaim, contents knowledge.SnapshotContents, captured map[capturedSource]bool, ranks map[string]int, maxBytes int) chatFacts {
	pinned := make(map[string]knowledge.SnapshotEntity, len(contents.Entities))
	for _, e := range contents.Entities {
		pinned[e.EntityID] = e
	}
	sources := make(map[string]string, len(contents.Spans))
	for _, s := range contents.Spans {
		sources[s.SpanID] = s.SourceID
	}
	readable := map[string]bool{}
	for _, c := range claims {
		readable[c.SubjectID] = true
	}
	rank := func(c knowledge.SnapshotClaim) int {
		best := math.MaxInt
		for _, id := range c.SourceSpanIDs {
			if r, ok := ranks[id]; ok {
				best = min(best, r)
			}
		}
		return best
	}
	claims = slices.Clone(claims)
	slices.SortStableFunc(claims, func(a, b knowledge.SnapshotClaim) int { return cmp.Compare(rank(a), rank(b)) })
	build := func(claims []knowledge.SnapshotClaim) chatFacts {
		out := chatFacts{Entities: []chatFactEntity{}, Evidence: map[string]string{}, Facts: make([]chatFact, 0, len(claims))}
		named := map[string]bool{}
		entity := func(id string, revision int64) {
			e, ok := pinned[id]
			allowed := ok && (revision == 0 || revision == e.Revision) && (readable[id] || captured[capturedSource{e.SourceID, e.SourceRevision}])
			if was, seen := named[id]; seen {
				named[id] = was && allowed
				return
			}
			named[id] = allowed
			out.Entities = append(out.Entities, chatFactEntity{EntityID: id})
		}
		for _, c := range claims {
			entity(c.SubjectID, c.SubjectRevision)
			if c.ObjectEntityID != "" {
				entity(c.ObjectEntityID, c.ObjectRevision)
			}
			for _, id := range c.SourceSpanIDs {
				if src, ok := sources[id]; ok {
					out.Evidence[id] = src
				}
			}
			out.Facts = append(out.Facts, chatFact{ChatClaimRef{c.ClaimID, c.Revision}, c.SubjectID, c.Predicate, c.ObjectEntityID, c.Literal, c.SourceSpanIDs})
		}
		for i, e := range out.Entities {
			if named[e.EntityID] {
				out.Entities[i].Name = pinned[e.EntityID].Name
			}
		}
		return out
	}
	// The index travels as a JSON string inside the brief message, so it is measured escaped.
	// ponytail: drops trailing facts one at a time, O(n²) marshalling over at most a few hundred facts.
	for n := len(claims); n > 0; n-- {
		out := build(claims[:n])
		if b, err := json.Marshal(out); err == nil {
			if b, err = json.Marshal(string(b)); err == nil && len(b) <= maxBytes {
				return out
			}
		}
	}
	return build(nil)
}

func resolveChatClaimCitations(out ChatAnswerOutputV2, standing []knowledge.SnapshotClaim) ([]ChatClaimCitation, string) {
	claims := map[ChatClaimRef]knowledge.SnapshotClaim{}
	for _, c := range standing {
		claims[ChatClaimRef{c.ClaimID, c.Revision}] = c
	}
	result := []ChatClaimCitation{}
	for i, stmt := range out.Statements {
		if len(stmt.ClaimRefs) > 10 || (len(stmt.ClaimRefs) > 0 && stmt.Kind == "hypothesis") {
			return nil, CodeFactNotApproved
		}
		seen := map[ChatClaimRef]bool{}
		for _, ref := range stmt.ClaimRefs {
			c, ok := claims[ref]
			if !ok || seen[ref] || len(c.SourceSpanIDs) == 0 || slices.ContainsFunc(c.SourceSpanIDs, func(id string) bool { return !slices.Contains(stmt.SourceSpanIDs, id) }) {
				return nil, CodeFactNotApproved
			}
			seen[ref] = true
			result = append(result, ChatClaimCitation{ref, i, slices.Clone(c.SourceSpanIDs)})
		}
	}
	return result, ""
}

type ChatAnswerArtifactV2 struct {
	SchemaVersion  int                 `json:"schema_version"`
	ReplyID        string              `json:"reply_message_id"`
	Status         string              `json:"status"`
	Statements     []ChatStatement     `json:"statements"`
	ClaimCitations []ChatClaimCitation `json:"claim_citations"`
}

func decodeChatArtifactV2(b []byte) (any, error) {
	var out ChatAnswerArtifactV2
	if strictjson.Decode(b, &out, MaxContractBytes) != nil || out.SchemaVersion != ChatSchemaVersion || !isUUID(out.ReplyID) || (out.Status != "answered" && out.Status != "needs_input") || out.Statements == nil || len(out.Statements) > maxChatStatements || (out.Status == "answered" && len(out.Statements) == 0) || (out.Status == "needs_input" && len(out.Statements) > 0) {
		return nil, ErrRunStore
	}
	refs := 0
	for _, stmt := range out.Statements {
		if (stmt.Kind != "approved_fact" && stmt.Kind != "reported_observation" && stmt.Kind != "hypothesis") || !utf8.ValidString(stmt.Text) || strings.TrimSpace(stmt.Text) == "" || utf8.RuneCountInString(stmt.Text) > maxChatStatementRune || len(stmt.SourceSpanIDs) == 0 || len(stmt.SourceSpanIDs) > 10 || len(stmt.ClaimRefs) > 10 {
			return nil, ErrRunStore
		}
		for _, id := range stmt.SourceSpanIDs {
			if !isUUID(id) {
				return nil, ErrRunStore
			}
		}
		refs += len(stmt.ClaimRefs)
	}
	if refs != len(out.ClaimCitations) {
		return nil, ErrRunStore
	}
	seen := map[struct {
		statement int
		ref       ChatClaimRef
	}]bool{}
	for _, c := range out.ClaimCitations {
		key := struct {
			statement int
			ref       ChatClaimRef
		}{c.Statement, c.ChatClaimRef}
		if seen[key] {
			return nil, ErrRunStore
		}
		seen[key] = true
		if !isUUID(c.ClaimID) || c.Revision < 1 || c.Statement < 0 || c.Statement >= len(out.Statements) || len(c.SourceSpanIDs) == 0 || !slices.Contains(out.Statements[c.Statement].ClaimRefs, c.ChatClaimRef) {
			return nil, ErrRunStore
		}
		for _, id := range c.SourceSpanIDs {
			if !isUUID(id) || !slices.Contains(out.Statements[c.Statement].SourceSpanIDs, id) {
				return nil, ErrRunStore
			}
		}
	}
	return out, nil
}
