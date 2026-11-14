package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/google/uuid"
)

func TestWIS703CorrectionAssertionsFailClosed(t *testing.T) {
	c, o := correctionControlSample()
	if f := correctionAssertions(c, o); len(f) != 0 {
		t.Fatal(f)
	}
	for _, control := range correctionControls() {
		if control.Outcome != "FAIL" || control.Problem != "" {
			t.Fatalf("%+v", control)
		}
	}
	t.Run("wrong-unit", func(t *testing.T) {
		c, o := correctionControlSample()
		c.Claim.Literal.Unit = "min"
		if len(correctionAssertions(c, o)) == 0 {
			t.Fatal("wrong unit passed")
		}
	})
	t.Run("exact-large-decimal", func(t *testing.T) {
		c, o := correctionControlSample()
		c.Claim.Literal.Value = json.RawMessage(`"9007199254740993"`)
		if len(correctionAssertions(c, o)) == 0 {
			t.Fatal("wrong decimal passed")
		}
	})
	t.Run("missing-source-pin", func(t *testing.T) {
		c, o := correctionControlSample()
		c.SourcePins = []knowledge.SnapshotSource{{SourceID: uuid.NewString(), Revision: 1, AccessEpoch: 1, SHA256: strings.Repeat("a", 64)}}
		if len(correctionAssertions(c, o)) == 0 {
			t.Fatal("uncaptured source passed")
		}
	})
	t.Run("extra-contradiction", func(t *testing.T) {
		c, o := correctionControlSample()
		o.Artifact.Statements = append(o.Artifact.Statements, agent.ChatStatement{Kind: "hypothesis", Text: "It is 60 seconds."})
		if len(correctionAssertions(c, o)) == 0 {
			t.Fatal("contradiction passed")
		}
	})
}

func TestWIS703UnpublishedAndWrongSnapshotCannotPass(t *testing.T) {
	c, _ := correctionControlSample()
	c.CanonicalReviewID = uuid.NewString()
	c.Claim.ClaimID = c.ClaimID
	snap := knowledge.Snapshot{}
	if correctionSnapshot(c, snap) != "AWAITING_SNAPSHOT_OR_UNAVAILABLE_APPROVAL" {
		t.Fatal("unpublished case passed")
	}
	snap.Contents.Claims = []knowledge.SnapshotClaim{{ClaimInput: c.Claim, Revision: c.ClaimRevision, ReviewID: c.CanonicalReviewID}}
	if correctionSnapshot(c, snap) != "" {
		t.Fatal("exact claim and review rejected")
	}
	snap.Contents.Claims[0].ReviewID = uuid.NewString()
	if correctionSnapshot(c, snap) == "" {
		t.Fatal("another approval substituted")
	}
}

func TestWIS703CorrectionSelectionIsExplicitAndStrict(t *testing.T) {
	file := filepath.Join(t.TempDir(), "selection.json")
	set := CorrectionSet{SchemaVersion: 1, TenantID: uuid.NewString(), SiteID: uuid.NewString(), SnapshotID: uuid.NewString(), CaseIDs: []string{uuid.NewString()}}
	raw, _ := json.Marshal(set)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, sha, err := loadCorrectionSet(file); err != nil || len(sha) != 64 {
		t.Fatal(sha, err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"passed":true`, 1), strings.Replace(string(raw), set.SiteID, "", 1)} {
		if err := os.WriteFile(file, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadCorrectionSet(file); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
}

func TestWIS703QuantityAssertionDoesNotTreatPinnedEntityDigitsAsAmounts(t *testing.T) {
	c, o := correctionControlSample()
	c.ExpectedAnswer = "The WIS703 press cycle time is 45 seconds."
	o.EntityNames = []string{"WIS703 press"}
	o.Artifact.Statements[0].Text = c.ExpectedAnswer
	body := "- [Approved process] " + c.ExpectedAnswer + " [1]"
	o.Answer.Body = &body
	if f := correctionAssertions(c, o); len(f) > 0 {
		t.Fatal(f)
	}
	o.EntityNames = nil
	if len(correctionAssertions(c, o)) == 0 {
		t.Fatal("unapproved entity digits ignored")
	}
}
