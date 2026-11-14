package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/chat"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

type manualUploadReport struct {
	SchemaVersion int                                  `json:"schema_version"`
	Synthetic     bool                                 `json:"synthetic"`
	Passed        bool                                 `json:"passed"`
	Failure       string                               `json:"failure,omitempty"`
	Provider      string                               `json:"provider"`
	Documents     map[string]onboarding.DocumentResult `json:"documents"`
	CycleSpans    map[string]evidence.Span             `json:"cycle_spans"`
	SnapshotID    string                               `json:"snapshot_id"`
	Question      string                               `json:"question"`
	ExpectedValue string                               `json:"expected_value"`
	ExpectedSpan  string                               `json:"expected_span"`
	ExpectedClaim string                               `json:"expected_claim"`
	Reply         chat.Message                         `json:"reply"`
}

func uploadManuals(ctx context.Context, l *loader, d dataset, key manualAnswerKey, dir string) (report manualUploadReport, err error) {
	report = manualUploadReport{SchemaVersion: 1, Synthetic: true, Provider: "server-configured chat provider; see run provenance; synthetic plant, not qualification", Documents: map[string]onboarding.DocumentResult{}, CycleSpans: map[string]evidence.Span{}}
	defer func() {
		if err != nil {
			report.Failure = err.Error()
		}
		err = errors.Join(err, os.WriteFile(filepath.Join(dir, "manual-upload-report.json"), wire(report), 0600))
	}()
	if err = l.identify(ctx); err != nil {
		return report, err
	}
	l.keyPrefix = "manuals-" + sha(d.files["manual-answer-key.json"])[:24] + "-"
	for _, doc := range key.Documents {
		var uploaded onboarding.DocumentResult
		status, e := l.requestHeaders(ctx, "owner", "POST", "/v1/sites/"+factorysim.SiteID+"/source-documents", doc.Name, "", "application/pdf", d.files["manuals/"+doc.Name+".pdf"], http.Header{"X-Source-Title": []string{url.PathEscape(doc.Title)}}, &uploaded)
		if e != nil {
			return report, e
		}
		if status != 200 || uploaded.Kind != onboarding.KindPDF || uploaded.OriginalSHA256 != doc.PDFSHA256 || len(uploaded.Spans) == 0 || strings.Join(strings.Fields(uploaded.Source.Text), " ") != strings.Join(strings.Fields(doc.Text), " ") {
			return report, fmt.Errorf("uploaded PDF differs from answer key: %s", doc.Name)
		}
		path := "/sources/" + uploaded.Source.SourceID
		var stored evidence.Source
		if err = l.call(ctx, "owner", path+"?revision=1", "", "", nil, &stored); err != nil {
			return report, err
		}
		if stored.Text != uploaded.Source.Text || stored.SHA256 != uploaded.Source.SHA256 {
			return report, errors.New("stored document readback differs")
		}
		var authority evidence.Authority
		if err = l.call(ctx, "owner", path+"/authority-decisions", doc.Name+"-authority", "0", evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Scripted fictional facility manual checked against simulator parameters"}, &authority); err != nil {
			return report, err
		}
		if err = l.call(ctx, "owner", path+"/currentness-decisions", doc.Name+"-current", "0", evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: 1, AuthorityDecisionID: authority.DecisionID, Verdict: "confirm_current", Reason: "Scripted fixture revision confirmed current"}, nil); err != nil {
			return report, err
		}
		report.Documents[doc.Name] = uploaded
		for _, sheet := range key.Sheets {
			if sheet.Class != doc.Class {
				continue
			}
			for _, station := range sheet.Stations {
				start := strings.Index(stored.Text, "Station: "+station.Name+"\n")
				rated := fmt.Sprintf("Rated cycle time: %d seconds.", station.RatedCycleSeconds)
				if start < 0 {
					return report, errors.New("station absent from extracted manual")
				}
				offset := strings.Index(stored.Text[start:], rated)
				if offset < 0 || offset > 200 {
					return report, errors.New("rated cycle absent from station passage")
				}
				end := start + offset + len(rated)
				var span evidence.Span
				if err = l.call(ctx, "owner", path+"/spans", station.StationID+"-span", "1", map[string]any{"revision": 1, "start_char": len([]rune(stored.Text[:start])), "end_char": len([]rune(stored.Text[:end]))}, &span); err != nil {
					return report, err
				}
				report.CycleSpans[station.StationID] = span
				claim := knowledge.ClaimInput{ClaimID: d.id("manual-cycle/" + station.StationID), SubjectID: station.StationID, SubjectRevision: 1, Predicate: "cycle_time_seconds", Literal: &knowledge.Literal{Type: "decimal", Value: json.RawMessage(wire(strconv.FormatInt(station.RatedCycleSeconds, 10))), Unit: "s"}, EffectiveFrom: key.ModelStart.Format(time.RFC3339), EvidenceClass: "report", SourceSpanIDs: []string{span.ID}}
				if err = l.call(ctx, "planner", "/claims", claim.ClaimID, "0", claim, nil); err != nil {
					return report, err
				}
				if err = l.call(ctx, "approver", "/claims/"+claim.ClaimID+"/reviews", claim.ClaimID+"-review", "1", knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Scripted fixture identity checked the stated fictional cycle; not independent human review"}, nil); err != nil {
					return report, err
				}
			}
		}
	}
	var inventory knowledge.Inventory
	if err = l.call(ctx, "owner", "/knowledge-inventory", "", "", nil, &inventory); err != nil {
		return report, err
	}
	report.SnapshotID = d.id("manual-snapshot")
	if err = l.call(ctx, "owner", "/process-snapshots", "manual-snapshot", strconv.FormatInt(inventory.Revision, 10), knowledge.SnapshotInput{SnapshotID: report.SnapshotID, Reason: "Scripted fixture snapshot of reviewed fictional manual facts"}, nil); err != nil {
		return report, err
	}
	station := d.manifest.Lines[1].Stations[2]
	report.Question = "What is the rated cycle time of " + station.Name + " in its fictional CNC machine manual?"
	report.ExpectedValue = strconv.FormatInt(station.CycleSeconds, 10)
	report.ExpectedSpan = report.CycleSpans[station.ID].ID
	report.ExpectedClaim = d.id("manual-cycle/" + station.ID)
	report.Reply, err = askManual(ctx, l, d.id("manual-question"), report.Question)
	if err != nil {
		return report, err
	}
	if err = checkManualAnswer(report.Reply, report.ExpectedClaim, report.ExpectedValue, report.ExpectedSpan); err != nil {
		return report, err
	}
	var span evidence.Span
	if err = l.call(ctx, "owner", "/source-spans/"+report.ExpectedSpan, "", "", nil, &span); err != nil {
		return report, err
	}
	if !strings.Contains(span.Text, "Rated cycle time: "+report.ExpectedValue+" seconds.") {
		return report, errors.New("cited span does not contain the answer-key value")
	}
	report.Passed = true
	return report, nil
}

// checkManualAnswer gates AC3 on what the server resolved, not on the model's wording: the
// quantity is resolved against the approved claim and its span (WIS-640), so it is stricter than
// matching a sentence. The body must still state the value with its unit.
func checkManualAnswer(reply chat.Message, claimID, value, spanID string) error {
	if reply.State != "answered" || reply.Body == nil {
		reason := ""
		if reply.ReasonCode != nil {
			reason = string(*reply.ReasonCode)
		}
		return fmt.Errorf("manual chat did not answer: state=%s reason=%s", reply.State, reason)
	}
	exact := 0
	for _, q := range reply.Quantities {
		if q.Predicate != "cycle_time_seconds" {
			continue
		}
		if q.Value != value {
			return fmt.Errorf("manual chat resolved another cycle time %s %s (claim %s)", q.Value, q.Unit, q.ClaimID)
		}
		if q.ClaimID == claimID && q.Unit == "s" && q.SpanID == spanID {
			exact++
		}
	}
	if exact != 1 {
		return fmt.Errorf("manual chat resolved %d cycle_time_seconds quantities for claim %s, value %s s, span %s; want exactly 1", exact, claimID, value, spanID)
	}
	// The value must be a whole token: no digit, letter, decimal point, comma or sign may touch it, so
	// 145.45, 0.45, -45, +45 and 45.0 are refused. 45.0 is refused because the server resolves the
	// canonical decimal "45" (rule 04); the body must state that token, not another spelling of it.
	if !regexp.MustCompile(`(?:^|[^\w.,+\-\x{2212}])` + regexp.QuoteMeta(value) + `\s*seconds?\b`).MatchString(strings.ToLower(*reply.Body)) {
		return errors.New("manual chat body does not state the value in seconds")
	}
	for _, citation := range reply.Citations {
		if citation.Kind == "evidence_span" && citation.ID == spanID {
			return nil
		}
	}
	return errors.New("manual chat did not cite the expected station span")
}

func askManual(ctx context.Context, l *loader, requestID, question string) (chat.Message, error) {
	reply, _, err := askManualConversation(ctx, l, requestID, question)
	return reply, err
}

func askManualConversation(ctx context.Context, l *loader, requestID, question string) (chat.Message, string, error) {
	var conversation chat.Conversation
	if err := l.call(ctx, "owner", "/chat-conversations", requestID+"-conversation", "", chat.CreateInput{Title: "Fictional facility manual check"}, &conversation); err != nil {
		return chat.Message{}, conversation.ConversationID, err
	}
	path := "/chat-conversations/" + conversation.ConversationID + "/messages"
	var posted chat.PostResult
	if err := l.call(ctx, "owner", path, requestID, strconv.FormatInt(conversation.Revision, 10), chat.PostInput{RequestID: requestID, Text: question}, &posted); err != nil {
		return chat.Message{}, conversation.ConversationID, err
	}
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		var page chat.MessagePage
		if err := l.call(ctx, "owner", path+"?limit=100", "", "", nil, &page); err != nil {
			return chat.Message{}, conversation.ConversationID, err
		}
		for _, message := range page.Messages {
			if message.MessageID == posted.Reply.MessageID && message.State != "pending" {
				return message, conversation.ConversationID, nil
			}
		}
		select {
		case <-ctx.Done():
			return chat.Message{}, conversation.ConversationID, ctx.Err()
		case <-deadline.C:
			return chat.Message{}, conversation.ConversationID, errors.New("manual chat answer timed out")
		case <-ticker.C:
		}
	}
}
