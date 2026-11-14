//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

// stationCycleWorld is the WIS-709 shape: eight machines whose approved facts all share the
// predicate cycle_time_seconds, one station passage each. KN-03 is the only 45 s station.
type stationCycleWorld struct {
	entities, claims, spans, sources map[string]string // by station name
	register                         string            // the machine register source every entity cites
	snapshot                         string
}

// Each station card is padded with notes to cardBytes (0 leaves it as one line).
func seedStationCycleWorld(f *stationFixture, cardBytes int) stationCycleWorld {
	f.t.Helper()
	sc := knowledge.Scope{TenantID: f.tenant, SiteID: f.site}
	ka := correctionActor(f, f.owner)
	ks, ev := knowledge.New(f.runtime), evidence.New(f.runtime)
	ea := evidence.Actor{Issuer: ka.Issuer, Subject: ka.Subject, SessionHash: ka.SessionHash}
	es := evidence.Scope{TenantID: f.tenant, SiteID: f.site}
	source := func(title, text string) (string, string) {
		f.t.Helper()
		src, err := ev.CreateOrRevise(f.ctx, ea, es, evidence.Input{SourceID: uuid.NewString(), Kind: "manual", Title: title, Text: text}, uuid.NewString(), 0)
		if err != nil {
			f.t.Fatal(err)
		}
		span, err := ev.CreateSpan(f.ctx, ea, es, evidence.SpanInput{SourceID: src.SourceID, Revision: 1, Start: 0, End: len(text)}, uuid.NewString(), 1)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err = ev.DecideAuthority(f.ctx, ea, es, src.SourceID, evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Reviewed " + title}, uuid.NewString(), 0); err != nil {
			f.t.Fatal(err)
		}
		return src.SourceID, span.ID
	}
	register, _ := source("Machine register", "Machines: CNC KN-01 to CNC KN-08.")
	w := stationCycleWorld{map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}, register, ""}
	for i, seconds := range []int{52, 60, 45, 38, 71, 49, 64, 57} {
		name := fmt.Sprintf("CNC KN-%02d", i+1)
		text := fmt.Sprintf("Station: %s | Rated cycle time: %d seconds.", name, seconds)
		text += strings.Repeat(" Setup, changeover and crew notes.", cardBytes/34+1)[:max(0, cardBytes-len(text))]
		card, span := source(name+" card", text)
		entity, err := ks.CreateEntity(f.ctx, ka, sc, knowledge.EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "machine", Name: name, SourceID: register, SourceRevision: 1}, uuid.NewString(), 0)
		if err != nil {
			f.t.Fatal(err)
		}
		claim := knowledge.ClaimInput{ClaimID: uuid.NewString(), SubjectID: entity.EntityID, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(fmt.Sprintf(`"%d"`, seconds)), Unit: "s"}, EffectiveFrom: "2026-09-27T00:00:00Z", EvidenceClass: "report", SourceSpanIDs: []string{span}}
		if _, err = ks.CreateClaim(f.ctx, ka, sc, claim, uuid.NewString(), 0); err != nil {
			f.t.Fatal(err)
		}
		if _, err = ks.DecideReview(f.ctx, ka, sc, claim.ClaimID, knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Rated cycle time"}, uuid.NewString(), 1); err != nil {
			f.t.Fatal(err)
		}
		w.entities[name], w.claims[name], w.spans[name], w.sources[name] = entity.EntityID, claim.ClaimID, span, card
	}
	inventory, err := ks.ReadInventory(f.ctx, ka, sc)
	if err != nil {
		f.t.Fatal(err)
	}
	w.snapshot = uuid.NewString()
	if _, err = ks.CreateSnapshot(f.ctx, ka, sc, knowledge.SnapshotInput{SnapshotID: w.snapshot, Reason: "Rated cycle times"}, uuid.NewString(), inventory.Revision); err != nil {
		f.t.Fatal(err)
	}
	return w
}

// askStation posts question as the lead and returns the chat service, conversation and answer run.
func askStation(f *stationFixture, settings chat.AnswerSettings, question string) (*chat.Service, chat.Actor, chat.Scope, string, string) {
	f.t.Helper()
	actor := correctionActor(f, f.lead)
	a := chat.Actor{Issuer: actor.Issuer, Subject: actor.Subject, SessionHash: actor.SessionHash}
	sc := chat.Scope{TenantID: f.tenant, SiteID: f.site}
	settings.Enabled = true
	svc := chat.NewService(f.runtime).WithAssistant(true).WithAnswering(settings)
	conv, err := svc.Create(f.ctx, a, sc, chat.CreateInput{Title: "Cycle times"}, uuid.NewString())
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := svc.Post(f.ctx, a, sc, conv.ConversationID, chat.PostInput{RequestID: uuid.NewString(), Text: question}, uuid.NewString(), conv.Revision)
	if err != nil || result.Reply.State != "pending" {
		f.t.Fatalf("no answer run: %+v %v", result.Reply, err)
	}
	var run string
	if err = f.control.QueryRowContext(f.ctx, `SELECT run_id::text FROM supervisor.chat_messages WHERE tenant_id=$1 AND site_id=$2 AND message_id=$3`, f.tenant, f.site, result.Reply.MessageID).Scan(&run); err != nil {
		f.t.Fatal(err)
	}
	return svc, a, sc, conv.ConversationID, run
}

// WIS-712: approved facts reached the model without the entity they describe, so eight
// cycle_time_seconds facts were indistinguishable (WIS-709 run c70badac). The brief as sent names
// each fact's subject from the pinned snapshot, not from the live entity row.
func TestWIS712ChatBriefNamesSnapshotSubjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	w := seedStationCycleWorld(f, 0)
	// Renamed after the snapshot: the live row now disagrees with the approved snapshot.
	ka := correctionActor(f, f.owner)
	if _, err := knowledge.New(f.runtime).CreateEntity(ctx, ka, knowledge.Scope{TenantID: f.tenant, SiteID: f.site}, knowledge.EntityInput{EntityID: w.entities["CNC KN-03"], Type: "asset", AssetKind: "machine", Name: "Live rename KN-99", SourceID: w.register, SourceRevision: 1}, uuid.NewString(), 1); err != nil {
		t.Fatal(err)
	}
	brief := sentChatBrief(f, "What is the rated cycle time of CNC KN-03?")
	subjects := briefSubjects(t, brief)
	if len(subjects) != 8 {
		t.Fatalf("brief as sent: %s", brief)
	}
	for name, claim := range w.claims {
		if subjects[claim] != [2]string{w.entities[name], name} {
			t.Errorf("%s: fact %s lacks its snapshot subject in %s", name, claim, brief)
		}
	}
	if strings.Contains(brief, "Live rename") {
		t.Errorf("live entity name reached the model: %s", brief)
	}
}

// sentChatBrief asks question as the lead and returns the brief ChatAnswer sent to the model.
func sentChatBrief(f *stationFixture, question string) string {
	f.t.Helper()
	_, _, _, _, run := askStation(f, chat.AnswerSettings{Provider: agent.ProviderFixture, Model: "fixture-model", ModelRev: "sha256:fixture"}, question)
	var brief string
	body, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: uuid.NewSHA1(uuid.MustParse(run), []byte(agent.RoleDiscoveryMapping)).String(), Role: agent.RoleDiscoveryMapping, Status: "needs_input", Statements: []agent.ChatStatement{}, MissingInputs: []string{"Nothing; brief recorded"}})
	p := agent.NewFixtureProvider(agent.FixtureStep{Response: agent.ModelResponse{Content: string(body)}, OnRequest: func(r agent.ModelRequest) { brief = r.Messages[1].Content }})
	rt := &agent.Runtime{Store: agent.NewStore(f.runtime), Provider: p, Tools: &agent.Tools{Store: agent.NewStore(f.runtime)}, Slots: agent.NewSlots(2, 8), Limits: agent.DefaultLimits(), Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
	if _, err := rt.ChatAnswer(f.ctx, agent.ChatAnswerRef{TenantID: f.tenant, SiteID: f.site, RunID: run}); err != nil {
		f.t.Fatal(err)
	}
	if brief == "" {
		f.t.Fatal("no brief reached the model")
	}
	return brief
}

// briefSubjects maps each approved fact of the brief to its subject's entity ID and display name.
func briefSubjects(t *testing.T, brief string) map[string][2]string {
	t.Helper()
	var sent struct {
		Entities []struct {
			EntityID string `json:"entity_id"`
			Name     string `json:"display_name"`
		} `json:"entities"`
		Facts []struct {
			ClaimID   string `json:"claim_id"`
			SubjectID string `json:"subject_id"`
		} `json:"approved_facts"`
	}
	if err := json.Unmarshal([]byte(brief), &sent); err != nil {
		t.Fatalf("brief as sent: %v %s", err, brief)
	}
	names := map[string]string{}
	for _, e := range sent.Entities {
		names[e.EntityID] = e.Name
	}
	out := map[string][2]string{}
	for _, fact := range sent.Facts {
		out[fact.ClaimID] = [2]string{fact.SubjectID, names[fact.SubjectID]}
	}
	return out
}

// WIS-712 review, finding 2 (rules 03, 10 and 11): the boundary end to end, through ChatAnswer.
// The lead cannot read CNC KN-05's card. Every member reads every unrevoked source and a revoked
// pinned source refuses the snapshot, so the case is injected as in the WIS-517 CAPTURE_LIMIT test:
// the card is revoked and the newest snapshot is a copy that no longer pins it. The brief as sent
// names the seven readable stations and carries nothing of KN-05: no claim ID, entity ID, name or
// source ID. It failed when ChatAnswer built the brief from every snapshot claim.
func TestWIS712ChatBriefWithholdsUnreadableStation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	w := seedStationCycleWorld(f, 0)
	const hidden = "CNC KN-05"
	ka := correctionActor(f, f.owner)
	var state int64
	if err := f.control.QueryRowContext(ctx, `SELECT state_revision FROM supervisor.evidence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3`, f.tenant, f.site, w.sources[hidden]).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.New(f.runtime).Revoke(ctx, evidence.Actor{Issuer: ka.Issuer, Subject: ka.Subject, SessionHash: ka.SessionHash}, evidence.Scope{TenantID: f.tenant, SiteID: f.site}, w.sources[hidden], "Card withheld from the lead", uuid.NewString(), state); err != nil {
		t.Fatal(err)
	}
	forge, err := f.control.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer forge.Rollback()
	if _, err = forge.ExecContext(ctx, `SELECT set_config('supervisor.tenant_id',$1,true)`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = forge.ExecContext(ctx, `INSERT INTO supervisor.knowledge_process_snapshots(tenant_id,site_id,snapshot_id,inventory_revision,sha256,canonical_contents,reason,actor_issuer,actor_subject,auth_time)
SELECT tenant_id,site_id,$3,inventory_revision,encode(sha256(convert_to(c,'UTF8')),'hex'),c,'Card withheld',actor_issuer,actor_subject,clock_timestamp()
FROM (SELECT p.*,jsonb_set(p.canonical_contents::jsonb,'{sources}',(SELECT coalesce(jsonb_agg(s),'[]'::jsonb) FROM jsonb_array_elements(p.canonical_contents::jsonb->'sources') s WHERE s->>'source_id'<>$4))::text AS c
 FROM supervisor.knowledge_process_snapshots p WHERE p.tenant_id=$1 AND p.site_id=$2 AND p.snapshot_id=$5) x`, f.tenant, f.site, uuid.NewString(), w.sources[hidden], w.snapshot); err != nil {
		t.Fatal(err)
	}
	if err = forge.Commit(); err != nil {
		t.Fatal(err)
	}
	brief := sentChatBrief(f, "What is the rated cycle time of CNC KN-03?")
	subjects := briefSubjects(t, brief)
	if len(subjects) != 7 {
		t.Errorf("brief holds %d facts, want the 7 readable ones: %s", len(subjects), brief)
	}
	for name, claim := range w.claims {
		if name != hidden && subjects[claim] != [2]string{w.entities[name], name} {
			t.Errorf("%s: fact %s lacks its snapshot subject in %s", name, claim, brief)
		}
	}
	for _, leak := range []string{w.claims[hidden], w.entities[hidden], hidden, w.sources[hidden], w.spans[hidden]} {
		if strings.Contains(brief, leak) {
			t.Errorf("unreadable station leaked %q: %s", leak, brief)
		}
	}
}

// WIS-712 review, finding 1, end to end: with long station cards (8 of 1,300 bytes, within
// MaxChatPassageBytes) the brief keeps only the facts that leave room for reading every card, and
// the turn that reads them all, as the prompt asks when no fact matches, still fits the input
// budget. It ended BUDGET_EXHAUSTED when the index was not sized to the captured passages, and it
// dropped the asked station's fact when the index was trimmed in snapshot order.
func TestWIS712ChatBriefLeavesRoomToReadEveryCard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStationFixture(t, ctx)
	w := seedStationCycleWorld(f, 1300)
	// KN-08 is last in snapshot order; the brief keeps its fact because its card is ranked first.
	_, _, _, _, run := askStation(f, chat.AnswerSettings{Provider: agent.ProviderFixture, Model: "fixture-model", ModelRev: "sha256:fixture"}, "What is the rated cycle time of CNC KN-08?")
	var calls []agent.ToolCall
	for i := range 8 {
		calls = append(calls, agent.ToolCall{ID: fmt.Sprintf("call_%035d", i), Name: agent.ToolEvidenceGet, Arguments: json.RawMessage(`{"evidence_id": "` + w.sources[fmt.Sprintf("CNC KN-%02d", i+1)] + `"}`)})
	}
	var brief string
	reads := 0
	body, _ := json.Marshal(agent.ChatAnswerOutputV2{SchemaVersion: agent.ChatSchemaVersion, RunID: uuid.NewSHA1(uuid.MustParse(run), []byte(agent.RoleDiscoveryMapping)).String(), Role: agent.RoleDiscoveryMapping, Status: "needs_input", Statements: []agent.ChatStatement{}, MissingInputs: []string{"Nothing; every card read"}})
	p := agent.NewFixtureProvider(
		agent.FixtureStep{Response: agent.ModelResponse{ToolCalls: calls}, OnRequest: func(r agent.ModelRequest) { brief = r.Messages[1].Content }},
		agent.FixtureStep{Response: agent.ModelResponse{Content: string(body)}, OnRequest: func(r agent.ModelRequest) { reads = len(r.Messages) - 3 }})
	rt := &agent.Runtime{Store: agent.NewStore(f.runtime), Provider: p, Tools: &agent.Tools{Store: agent.NewStore(f.runtime)}, Slots: agent.NewSlots(2, 8), Limits: agent.DefaultLimits(), Model: "fixture-model", ModelRevision: "sha256:fixture", Options: agent.LoaderOptions{ModeEnabled: true}}
	res, err := rt.ChatAnswer(ctx, agent.ChatAnswerRef{TenantID: f.tenant, SiteID: f.site, RunID: run})
	if err != nil {
		t.Fatal(err)
	}
	subjects := briefSubjects(t, brief)
	t.Logf("%d of 8 facts in the brief; %d cards read; reply %s %s", len(subjects), reads, res.ReplyState, res.ReasonCode)
	if res.ReplyState != "needs_input" || reads != 8 {
		t.Errorf("the turn reading every card did not complete: %+v after %d reads", res, reads)
	}
	if _, ok := subjects[w.claims["CNC KN-08"]]; !ok {
		t.Errorf("the asked station's fact was trimmed from the brief: %s", brief)
	}
	if len(subjects) == 0 || len(subjects) == 8 {
		t.Errorf("brief holds %d of 8 facts, want some but not all: %s", len(subjects), brief)
	}
	for name, claim := range w.claims {
		if got, ok := subjects[claim]; ok && got != [2]string{w.entities[name], name} {
			t.Errorf("%s: fact %s lacks its snapshot subject in %s", name, claim, brief)
		}
	}
}
