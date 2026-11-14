package feedback

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

const (
	idA = "7f6f2b1e-3c2d-4a8b-9e1f-0a1b2c3d4e5f"
	idB = "1a2b3c4d-5e6f-4a1b-8c2d-3e4f5a6b7c8d"
	idC = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b"
)

func ptr[T any](v T) *T { return &v }

func validCase() FeedbackInputV1 {
	return FeedbackInputV1{SchemaVersion: 1, Subject: SubjectInput{Kind: SubjectCase, CaseID: ptr(idA)},
		Kind: KindObservation, Verdict: VerdictHelpful, Reason: "Line 2 restarted after the jam was cleared.", EvidenceIDs: []string{}}
}

// TestValidate drives the contract check the way Submit does. Every refused case changes exactly
// one thing from a valid input, so each row fails on the rule it names.
func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*FeedbackInputV1)
		ok     bool
	}{
		{"valid case observation", func(*FeedbackInputV1) {}, true},
		{"valid answer", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectAnswer, ConversationID: ptr(idA), MessageID: ptr(idB)}
		}, true},
		{"valid claim correction without challenged source", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectClaim, ClaimID: ptr(idA), ClaimRevision: ptr(int64(2))}
			in.Kind = KindCorrection
		}, true},
		{"valid mapping correction challenging its own revision", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectMapping, SourceID: ptr(idA), MappingRevision: ptr(int64(1))}
			in.Kind = KindCorrection
		}, true},
		{"valid successor of earlier feedback", func(in *FeedbackInputV1) { in.SupersedesFeedbackID = ptr(idB) }, true},
		{"invalid superseded feedback ID", func(in *FeedbackInputV1) { in.SupersedesFeedbackID = ptr("feedback-1") }, false},
		{"valid mapping correction naming its source revision", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectMapping, SourceID: ptr(idA), MappingRevision: ptr(int64(1))}
			in.Kind, in.Challenged = KindCorrection, &SourceRevision{SourceID: idB, Revision: 3}
		}, true},
		{"valid task with proposal and window", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectTask, TaskID: ptr(idA)}
			in.ProposalID = ptr(idB)
			in.ObservationStart, in.ObservationEnd = ptr("2026-10-28T10:00:00+05:30"), ptr("2026-10-28T04:45:00Z")
		}, true},
		{"reason at 2000 characters", func(in *FeedbackInputV1) { in.Reason = strings.Repeat("é", MaxReason) }, true},
		{"20 evidence IDs", func(in *FeedbackInputV1) { in.EvidenceIDs = ids(MaxEvidence) }, true},

		{"schema version 2", func(in *FeedbackInputV1) { in.SchemaVersion = 2 }, false},
		{"unknown kind", func(in *FeedbackInputV1) { in.Kind = "instruction" }, false},
		{"unknown verdict", func(in *FeedbackInputV1) { in.Verdict = "approve" }, false},
		{"unknown subject kind", func(in *FeedbackInputV1) { in.Subject.Kind = "proposal" }, false},
		{"empty reason", func(in *FeedbackInputV1) { in.Reason = "" }, false},
		{"whitespace reason", func(in *FeedbackInputV1) { in.Reason = " \n\t " }, false},
		{"reason over 2000 characters", func(in *FeedbackInputV1) { in.Reason = strings.Repeat("a", MaxReason+1) }, false},
		{"reason with a control character", func(in *FeedbackInputV1) { in.Reason = "ok\x1b[2Jcleared" }, false},
		{"reason with invalid UTF-8", func(in *FeedbackInputV1) { in.Reason = "ok\xff" }, false},
		{"missing evidence list", func(in *FeedbackInputV1) { in.EvidenceIDs = nil }, false},
		{"21 evidence IDs", func(in *FeedbackInputV1) { in.EvidenceIDs = ids(MaxEvidence + 1) }, false},
		{"duplicate evidence ID", func(in *FeedbackInputV1) { in.EvidenceIDs = []string{idA, idA} }, false},
		{"uppercase evidence ID", func(in *FeedbackInputV1) { in.EvidenceIDs = []string{strings.ToUpper(idA)} }, false},
		{"nil evidence ID", func(in *FeedbackInputV1) { in.EvidenceIDs = []string{"00000000-0000-0000-0000-000000000000"} }, false},
		{"window start only", func(in *FeedbackInputV1) { in.ObservationStart = ptr("2026-10-28T04:30:00Z") }, false},
		{"window end not after start", func(in *FeedbackInputV1) {
			in.ObservationStart, in.ObservationEnd = ptr("2026-10-28T10:00:00+05:30"), ptr("2026-10-28T04:30:00Z")
		}, false},
		{"window time without offset", func(in *FeedbackInputV1) {
			in.ObservationStart, in.ObservationEnd = ptr("2026-10-28T04:30:00"), ptr("2026-10-28T05:30:00Z")
		}, false},
		{"case subject missing case", func(in *FeedbackInputV1) { in.Subject.CaseID = nil }, false},
		{"case subject with a task", func(in *FeedbackInputV1) { in.Subject.TaskID = ptr(idB) }, false},
		{"task subject with its case", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectTask, TaskID: ptr(idA), CaseID: ptr(idB)}
		}, false},
		{"answer missing message", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectAnswer, ConversationID: ptr(idA)}
		}, false},
		{"claim missing revision", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectClaim, ClaimID: ptr(idA)}
		}, false},
		{"claim revision zero", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectClaim, ClaimID: ptr(idA), ClaimRevision: ptr(int64(0))}
		}, false},
		{"mapping with a claim", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectMapping, SourceID: ptr(idA), MappingRevision: ptr(int64(1)), ClaimID: ptr(idB)}
		}, false},
		{"proposal on an answer", func(in *FeedbackInputV1) {
			in.Subject = SubjectInput{Kind: SubjectAnswer, ConversationID: ptr(idA), MessageID: ptr(idB)}
			in.ProposalID = ptr(idC)
		}, false},
		{"correction of a case without a challenged source", func(in *FeedbackInputV1) { in.Kind = KindCorrection }, false},
		{"challenged source on an observation", func(in *FeedbackInputV1) {
			in.Challenged = &SourceRevision{SourceID: idB, Revision: 1}
		}, false},
		{"challenged revision zero", func(in *FeedbackInputV1) {
			in.Kind, in.Challenged = KindCorrection, &SourceRevision{SourceID: idB, Revision: 0}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validCase()
			c.mutate(&in)
			_, err := validate(in)
			if c.ok && err != nil {
				t.Fatalf("validate: %v", err)
			}
			if !c.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateNormalizes(t *testing.T) {
	in := validCase()
	in.EvidenceIDs = []string{idC, idA, idB}
	in.ObservationStart, in.ObservationEnd = ptr("2026-10-28T10:00:00+05:30"), ptr("2026-10-28T11:00:00+05:30")
	out, err := validate(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{idB, idA, idC}; !slices.Equal(out.evidence, want) {
		t.Fatalf("evidence = %v, want sorted %v", out.evidence, want)
	}
	if got := out.observationStart.Format("2006-01-02T15:04:05Z07:00"); got != "2026-10-28T04:30:00Z" {
		t.Fatalf("observation start = %s, want UTC", got)
	}
	if out.caseID != idA {
		t.Fatalf("case = %q", out.caseID)
	}
}

// TestDecodeRefusesServerFields: the actor, site, case binding and evidence revisions are derived
// by the server; a body that tries to supply them is refused before it reaches validate (rule 03).
func TestDecodeRefusesServerFields(t *testing.T) {
	base := `{"schema_version":1,"subject":{"kind":"case","case_id":"` + idA + `"},"kind":"observation","verdict":"helpful","reason":"r","evidence_ids":[]`
	var in FeedbackInputV1
	if err := strictjson.Decode([]byte(base+`}`), &in, 1<<16); err != nil {
		t.Fatalf("valid body refused: %v", err)
	}
	for _, extra := range []string{`"actor_id":"` + idB + `"`, `"site_id":"` + idB + `"`, `"context_snapshot_id":"` + idB + `"`, `"tenant_id":"` + idB + `"`} {
		var in FeedbackInputV1
		if err := strictjson.Decode([]byte(base+`,`+extra+`}`), &in, 1<<16); err == nil {
			t.Errorf("body with %s was accepted", extra)
		}
	}
	var nested FeedbackInputV1
	if err := strictjson.Decode([]byte(`{"schema_version":1,"subject":{"kind":"task","task_id":"`+idA+`","case_revision":3},"kind":"observation","verdict":"helpful","reason":"r","evidence_ids":[]}`), &nested, 1<<16); err == nil {
		t.Error("subject with a server-bound case_revision was accepted")
	}
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Replace(idA, "0a1b2c3d4e5f", strings.Repeat("0", 10)+string(rune('a'+i/16))+string("0123456789abcdef"[i%16]), 1)
	}
	return out
}
