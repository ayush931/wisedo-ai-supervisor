package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func stationID(kind string, i, j int) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "%s-%d-%d", kind, i, j)).String()
}

// stationContents is a pinned snapshot of n stations, each with one passage (a station card) on its
// own source backing perStation approved facts that share predicates across stations (WIS-709 shape).
func stationContents(n, perStation int) knowledge.SnapshotContents {
	c := knowledge.SnapshotContents{}
	predicates := []string{"cycle_time_seconds", "setup_time_seconds", "changeover_minutes", "crew_size", "target_units_per_shift", "scrap_rate_percent"}
	for i := range n {
		entity, source, span := stationID("entity", i, 0), stationID("source", i, 0), stationID("span", i, 0)
		c.Entities = append(c.Entities, knowledge.SnapshotEntity{EntityInput: knowledge.EntityInput{EntityID: entity, Type: "asset", AssetKind: "machine", Name: fmt.Sprintf("CNC KN-%02d", i+1)}, Revision: 1})
		c.Spans = append(c.Spans, knowledge.SnapshotSpan{SpanID: span, SourceID: source, Revision: 1})
		for j := range perStation {
			claim := decimalClaim(stationID("claim", i, j), predicates[j%len(predicates)], fmt.Sprint(40+i), "s", span)
			claim.SubjectID, claim.SubjectRevision, claim.Revision = entity, 1, 1
			c.Claims = append(c.Claims, claim)
		}
	}
	return c
}

// sentFact is one fact of the brief as the model reads it: its entities resolved through the brief's
// entity list and its evidence IDs through evidence_by_span.
type sentFact struct {
	ClaimID     string
	Subject     chatFactEntity
	Object      *chatFactEntity
	EvidenceIDs []string
}

// sentFacts decodes the brief's index from its JSON, as sent, and resolves each fact.
func sentFacts(t *testing.T, facts chatFacts) ([]sentFact, string) {
	t.Helper()
	raw, err := json.Marshal(facts)
	var brief struct {
		Entities []chatFactEntity  `json:"entities"`
		Evidence map[string]string `json:"evidence_by_span"`
		Facts    []struct {
			ClaimID       string   `json:"claim_id"`
			SubjectID     string   `json:"subject_id"`
			ObjectID      string   `json:"object_id"`
			SourceSpanIDs []string `json:"source_span_ids"`
		} `json:"approved_facts"`
	}
	if err != nil || json.Unmarshal(raw, &brief) != nil {
		t.Fatalf("facts %s: %v", raw, err)
	}
	names := map[string]chatFactEntity{}
	for _, e := range brief.Entities {
		if _, dup := names[e.EntityID]; dup {
			t.Fatalf("entity %s listed twice: %s", e.EntityID, raw)
		}
		names[e.EntityID] = e
	}
	resolve := func(id string) chatFactEntity {
		e, ok := names[id]
		if !ok {
			t.Fatalf("entity %s not listed: %s", id, raw)
		}
		return e
	}
	out := []sentFact{}
	for _, f := range brief.Facts {
		sf := sentFact{ClaimID: f.ClaimID, Subject: resolve(f.SubjectID), EvidenceIDs: []string{}}
		if f.ObjectID != "" {
			o := resolve(f.ObjectID)
			sf.Object = &o
		}
		for _, span := range f.SourceSpanIDs {
			if src, ok := brief.Evidence[span]; ok && !slices.Contains(sf.EvidenceIDs, src) {
				sf.EvidenceIDs = append(sf.EvidenceIDs, src)
			}
		}
		out = append(out, sf)
	}
	return out, string(raw)
}

// WIS-712: eight cycle_time_seconds facts reached the model without the entity they describe
// (WIS-709 run c70badac). Each fact carries its subject's ID and snapshot name; a relation names its
// object; a claim pinning an entity revision the snapshot does not hold gets the ID without a name.
// Each fact also lists the evidence IDs (sources) of its spans, for the model's evidence.get.
func TestWIS712ApprovedFactsNameTheirSnapshotEntities(t *testing.T) {
	c := stationContents(9, 1)
	precedes := knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: stationID("claim", 99, 0), SubjectID: c.Entities[0].EntityID, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: c.Entities[1].EntityID, ObjectRevision: 1, SourceSpanIDs: []string{c.Spans[0].SpanID}}, Revision: 1}
	// A second fact on the ninth station pins an entity revision the snapshot does not hold. The
	// entity is listed once, so it loses its name for both facts.
	pinnedElsewhere := c.Claims[8]
	pinnedElsewhere.ClaimID, pinnedElsewhere.SubjectRevision = stationID("claim", 98, 0), 2
	got, raw := sentFacts(t, chatApprovedFacts(append(append(c.Claims[:8:8], precedes, c.Claims[8]), pinnedElsewhere), c, nil, nil, math.MaxInt))
	if len(got) != 11 {
		t.Fatalf("facts %s", raw)
	}
	for i, f := range got[:8] {
		if f.ClaimID != c.Claims[i].ClaimID || f.Subject != (chatFactEntity{c.Entities[i].EntityID, fmt.Sprintf("CNC KN-%02d", i+1)}) || f.Object != nil ||
			len(f.EvidenceIDs) != 1 || f.EvidenceIDs[0] != stationID("source", i, 0) {
			t.Errorf("fact %d: %+v", i, f)
		}
	}
	if f := got[8]; f.Subject.Name != "CNC KN-01" || f.Object == nil || *f.Object != (chatFactEntity{c.Entities[1].EntityID, "CNC KN-02"}) {
		t.Errorf("relation: %+v", f)
	}
	for _, f := range got[9:] {
		if f.Subject != (chatFactEntity{EntityID: c.Entities[8].EntityID}) {
			t.Errorf("entity pinned at another revision named: %+v", f)
		}
	}
}

// WIS-712 negative control (rules 03 and 11): a fact the reader cannot read stays out of the brief,
// and nothing of its subject (ID or name) is supplied through another fact or an entity list.
func TestWIS712WithheldFactLeaksNoSubjectName(t *testing.T) {
	c := stationContents(3, 1)
	c.Entities[2].Name = "Restricted press"
	captured := map[capturedSource]bool{{stationID("source", 0, 0), 1}: true, {stationID("source", 1, 0), 1}: true}
	got, raw := sentFacts(t, chatApprovedFacts(readableChatClaims(c, captured), c, captured, nil, math.MaxInt))
	if len(got) != 2 || got[0].Subject.Name != "CNC KN-01" || got[1].Subject.Name != "CNC KN-02" {
		t.Fatalf("readable facts: %s", raw)
	}
	for _, leak := range []string{c.Claims[2].ClaimID, c.Entities[2].EntityID, "Restricted press", stationID("source", 2, 0)} {
		if strings.Contains(raw, leak) {
			t.Errorf("withheld fact leaked %q: %s", leak, raw)
		}
	}
}

// WIS-712 review, finding 1: the brief must fit the conversation input budget (InterviewLimits,
// 8000 estimated tokens; stricter than the WIS-702 local 12000) on both requests of a turn: the
// first, and the one after evidence.get has read every captured source, which the prompt asks for
// when no fact matches. Requests are measured as reserveRequest measures them: the whole JSON body.
//
// The plant has 48 stations whose card span each backs 6 approved facts, with the longest display
// names (200 runes) and source titles the contracts admit. Chat captures at most MaxRunPassages
// cards, so 48 facts are readable. No index format carries all 48 within the budget once the cards
// are read: the index keeps the leading facts that fit (chatIndexBytes) and the rest stay captured
// and citable. On 4cd84b88 the brief carried every readable fact, and both requests overflowed.
func TestWIS712ChatBriefWithinBudgetFor48Stations(t *testing.T) {
	const perStation = 6
	c := stationContents(48, perStation)
	for i := range c.Entities {
		c.Entities[i].Name = fmt.Sprintf("CNC KN-%02d ", i+1) + strings.Repeat("x", 190)
	}
	captured := map[capturedSource]bool{}
	refs := make([]string, 0, MaxRunPassages)
	for i := range MaxRunPassages {
		captured[capturedSource{stationID("source", i, 0), 1}] = true
		refs = append(refs, stationID("source", i, 0))
	}
	readable := readableChatClaims(c, captured)
	if len(readable) != perStation*MaxRunPassages {
		t.Fatalf("readable facts: %d", len(readable))
	}
	child := DelegationEnvelopeV1{RunID: stationID("run", 0, 0), Role: RoleDiscoveryMapping, AllowedToolNames: []string{ToolEvidenceGet, ToolClaimsSearch}}
	const question = "What is the rated cycle time of CNC KN-03?"
	limits := InterviewLimits()
	measure := func(msgs []Message) int64 {
		t.Helper()
		body, err := json.Marshal(ModelRequest{SchemaVersion: ModelSchemaVersion, Model: localQwenModel, PromptVersion: ChatPromptVersion, ToolVersion: ToolVersion,
			Messages: msgs, Tools: ToolDefs(child.AllowedToolNames), ResponseSchema: chatOutputSchema, MaxOutputTokens: limits.MaxOutputTokens})
		if err != nil {
			t.Fatal(err)
		}
		return EstimateInputTokens(len(body))
	}
	for _, tc := range []struct {
		name         string
		passageBytes int // total card text, shared by the captured cards
		minFacts     int
	}{
		{"short cards", MaxRunPassages * 60, 1},
		// chat.MaxChatPassageBytes (chat imports agent). The passages and their worst-case titles
		// leave no room for a fact with a worst-case name: the index is empty, the turn still fits.
		{"cards at MaxChatPassageBytes", 12 << 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One evidence.get per captured source, each answered as tools.go answers a source with
			// selected passages.
			call := Message{Role: "assistant"}
			var results []Message
			runes := 0
			for i, ref := range refs {
				// A provider's call ID (llama.cpp sends 32 random characters; 40 here) and arguments.
				id := fmt.Sprintf("call_%035d", i)
				call.ToolCalls = append(call.ToolCalls, ToolCall{ID: id, Name: ToolEvidenceGet, Arguments: json.RawMessage(`{"evidence_id": "` + ref + `"}`)})
				span := stationID("span", i, 0)
				text := fmt.Sprintf("Station: CNC KN-%02d | Rated cycle time: %d seconds. ", i+1, 40+i)
				text += strings.Repeat("Setup, changeover and crew notes. ", tc.passageBytes)[:max(0, tc.passageBytes/MaxRunPassages-len(text))]
				runes += len(text)
				title := fmt.Sprintf("CNC KN-%02d station card ", i+1) + strings.Repeat("x", 177)
				body, err := json.Marshal(struct {
					EvidenceID string        `json:"evidence_id"`
					Revision   int64         `json:"revision"`
					SpanIDs    []string      `json:"span_ids"`
					Title      untrustedText `json:"title"`
					Passages   []any         `json:"passages"`
				}{ref, 1, []string{span}, untrustedText{title}, []any{map[string]string{"span_id": span, "untrusted_source": text}}})
				if err != nil {
					t.Fatal(err)
				}
				results = append(results, Message{Role: "tool", ToolCallID: id, Content: string(body)})
			}
			room, err := chatIndexBytes(limits, localQwenModel, child, question, refs, runes, len(refs))
			if err != nil {
				t.Fatal(err)
			}
			facts := chatApprovedFacts(readable, c, captured, nil, room)
			msgs, err := chatBrief(child, question, refs, facts)
			if err != nil {
				t.Fatal(err)
			}
			first, second := measure(msgs), measure(append(append(slices.Clone(msgs), call), results...))
			t.Logf("%d readable facts, %d in the index: first request %d, after reading all %d sources %d estimated tokens (budget %d)",
				len(readable), len(facts.Facts), first, len(refs), second, limits.MaxInputTokens)
			if budget := int64(limits.MaxInputTokens); first > budget || second > budget {
				t.Fatalf("estimated tokens: first request %d, second %d; over the %d budget", first, second, budget)
			}
			got, raw := sentFacts(t, facts)
			if len(got) < tc.minFacts {
				t.Fatalf("index holds %d of %d facts: %s", len(got), len(readable), raw)
			}
			for i, f := range got {
				if f.ClaimID != readable[i].ClaimID || !strings.HasPrefix(f.Subject.Name, "CNC KN-") || len(f.EvidenceIDs) != 1 {
					t.Fatalf("fact %d without its claim, name or evidence: %+v", i, f)
				}
			}
		})
	}
}

// WIS-712 security review: the object of a readable relation is named only when the reader can read
// something about it, as the subject of a readable claim or through its own captured source.
// Otherwise its ID is sent without its name (rules 03 and 11).
func TestWIS712RelationObjectNamedOnlyWhenReadable(t *testing.T) {
	register := stationID("register", 0, 0)
	c := stationContents(3, 1)
	for i := range c.Entities {
		c.Entities[i].SourceID, c.Entities[i].SourceRevision = register, 1
	}
	c.Entities[2].Name = "Restricted press"
	object := c.Entities[2].EntityID
	relation := knowledge.SnapshotClaim{ClaimInput: knowledge.ClaimInput{ClaimID: stationID("claim", 99, 0), SubjectID: c.Entities[0].EntityID, SubjectRevision: 1, Predicate: "precedes", ObjectEntityID: object, ObjectRevision: 1, SourceSpanIDs: []string{c.Spans[0].SpanID}}, Revision: 1}
	c.Claims = append(c.Claims, relation)
	for _, tc := range []struct {
		name     string
		captured []capturedSource
		want     string
	}{
		{"object facts withheld", []capturedSource{{stationID("source", 0, 0), 1}}, ""},
		{"object is a readable subject", []capturedSource{{stationID("source", 0, 0), 1}, {stationID("source", 2, 0), 1}}, "Restricted press"},
		{"object source captured", []capturedSource{{stationID("source", 0, 0), 1}, {register, 1}}, "Restricted press"},
		// WIS-712 review, finding 3: the entity pins register revision 1; revision 2 was captured.
		{"object source captured at another revision", []capturedSource{{stationID("source", 0, 0), 1}, {register, 2}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured := map[capturedSource]bool{}
			for _, src := range tc.captured {
				captured[src] = true
			}
			got, raw := sentFacts(t, chatApprovedFacts(readableChatClaims(c, captured), c, captured, nil, math.MaxInt))
			var rel *sentFact
			for i := range got {
				if got[i].ClaimID == relation.ClaimID {
					rel = &got[i]
				}
			}
			if rel == nil || rel.Subject.Name != "CNC KN-01" || rel.Object == nil || *rel.Object != (chatFactEntity{object, tc.want}) {
				t.Fatalf("relation fact: %s", raw)
			}
			if tc.want == "" && strings.Contains(raw, "Restricted press") {
				t.Fatalf("withheld object name leaked: %s", raw)
			}
		})
	}
}

// WIS-712: the index is trimmed to the room only after ordering facts by the rank of their selected
// passage, so the station the question asks about keeps its fact. Its card is ranked first but sits
// last in snapshot order, and the room fits two facts. Trimmed in snapshot order, it was dropped.
func TestWIS712IndexKeepsTheRankedFactWhenTrimmed(t *testing.T) {
	c := stationContents(8, 1)
	ranks := map[string]int{c.Spans[7].SpanID: 1}
	for i := range 7 {
		ranks[c.Spans[i].SpanID] = i + 2
	}
	two, err := json.Marshal(chatApprovedFacts(c.Claims[:2], c, nil, nil, math.MaxInt))
	if err != nil {
		t.Fatal(err)
	}
	room, err := json.Marshal(string(two)) // the index is measured escaped, as it travels
	if err != nil {
		t.Fatal(err)
	}
	got, raw := sentFacts(t, chatApprovedFacts(c.Claims, c, nil, ranks, len(room)))
	if len(got) != 2 || got[0].ClaimID != c.Claims[7].ClaimID || got[0].Subject.Name != "CNC KN-08" || got[1].ClaimID != c.Claims[0].ClaimID {
		t.Fatalf("index trimmed to two facts, want KN-08 then KN-01: %s", raw)
	}
	// Ties and unranked facts keep snapshot order.
	got, raw = sentFacts(t, chatApprovedFacts(c.Claims, c, nil, map[string]int{c.Spans[5].SpanID: 1}, math.MaxInt))
	want := []int{5, 0, 1, 2, 3, 4, 6, 7}
	for i, f := range got {
		if f.ClaimID != c.Claims[want[i]].ClaimID {
			t.Fatalf("order %d: %s", i, raw)
		}
	}
}
