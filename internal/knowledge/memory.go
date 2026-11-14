package knowledge

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrSelfReview = errors.New("correction author cannot review their correction")

// MemoryRevisionInput keeps incomplete feedback separate from the canonical claim registry.
type MemoryRevisionInput struct {
	Reason               string      `json:"reason" contract:"max=2000"`
	Claim                *ClaimInput `json:"claim,omitempty"`
	ExpectedBaseRevision int64       `json:"expected_base_revision"`
	Question             string      `json:"question" contract:"max=4000"`
	ExpectedAnswer       string      `json:"expected_answer" contract:"max=8000"`
}

func (in MemoryRevisionInput) Validate() error {
	if !contractText(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || in.ExpectedBaseRevision < 0 || (in.Claim == nil && in.ExpectedBaseRevision != 0) {
		return ErrInvalidContract
	}
	if (in.Question != "" && !contractText(in.Question, 4000)) || (in.ExpectedAnswer != "" && !contractText(in.ExpectedAnswer, 8000)) {
		return ErrInvalidContract
	}
	if in.Claim != nil {
		if err := in.Claim.Validate(); err != nil {
			return err
		}
		if err := in.Claim.ValidateEntityRevisions(); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(in)
	// jsonb adds spaces: reserve its bounded serialization overhead before entering storage.
	if err != nil || len(raw) > 15000 {
		return ErrInvalidContract
	}
	return nil
}

func (in MemoryRevisionInput) Missing() []string {
	out := []string{}
	if in.Claim == nil {
		out = append(out, "proposed_claim")
	}
	if strings.TrimSpace(in.Question) == "" {
		out = append(out, "regression_question")
	}
	if strings.TrimSpace(in.ExpectedAnswer) == "" {
		out = append(out, "expected_answer")
	} else if utf8.RuneCountInString(in.ExpectedAnswer) > 1000 {
		// An exact whole-statement assertion must fit one generated chat statement.
		out = append(out, "expected_answer_under_1001_characters")
	}
	return out
}

type CorrectionProvenance struct {
	RunID             *string `json:"run_id,omitempty" contract:"uuid"`
	ArtifactID        *string `json:"artifact_id,omitempty" contract:"uuid"`
	ArtifactSHA256    *string `json:"artifact_sha256,omitempty" contract:"sha256"`
	TerminalSHA256    *string `json:"terminal_sha256,omitempty" contract:"sha256"`
	RunRefsSHA256     *string `json:"run_refs_sha256,omitempty" contract:"sha256"`
	ProcessSnapshotID *string `json:"process_snapshot_id,omitempty" contract:"uuid"`
}

type CorrectionOriginal struct {
	OutcomeStatus          string               `json:"outcome_status,omitempty"`
	OutcomeReasons         []string             `json:"outcome_reasons,omitempty"`
	InvestigationRationale *string              `json:"investigation_rationale,omitempty" contract:"max=2000"`
	Kind                   string               `json:"kind"`
	Question               *string              `json:"question,omitempty" contract:"max=4000"`
	Answer                 *string              `json:"answer,omitempty" contract:"max=8000"`
	Available              bool                 `json:"available"`
	Provenance             CorrectionProvenance `json:"provenance"`
}

type MemoryReviewInput struct {
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason" contract:"max=2000"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type MemoryReview struct {
	ReviewID          string    `json:"review_id" contract:"uuid"`
	Verdict           string    `json:"verdict"`
	Reason            string    `json:"reason"`
	ActorIssuer       string    `json:"actor_issuer"`
	ActorSubject      string    `json:"actor_subject"`
	CreatedAt         time.Time `json:"created_at"`
	ClaimID           *string   `json:"claim_id,omitempty" contract:"uuid"`
	ClaimRevision     *int64    `json:"claim_revision,omitempty"`
	CanonicalReviewID *string   `json:"canonical_review_id,omitempty" contract:"uuid"`
}

type MemoryCandidate struct {
	CandidateID         string              `json:"candidate_id" contract:"uuid"`
	SiteID              string              `json:"site_id" contract:"uuid"`
	Kind                string              `json:"kind"`
	SourceFeedbackIDs   []string            `json:"source_feedback_ids"`
	ProposerIssuer      string              `json:"proposer_issuer"`
	ProposerSubject     string              `json:"proposer_subject"`
	Revision            int64               `json:"revision"`
	CreatedAt           time.Time           `json:"created_at"`
	ContentSHA256       string              `json:"content_sha256" contract:"sha256"`
	Content             MemoryRevisionInput `json:"content"`
	Original            CorrectionOriginal  `json:"original"`
	MissingRequirements []string            `json:"missing_requirements"`
	ReviewState         string              `json:"review_state"`
	Review              *MemoryReview       `json:"review,omitempty"`
	PublishedSnapshotID *string             `json:"published_snapshot_id,omitempty" contract:"uuid"`
}

type MemoryCandidatePage struct {
	Candidates []MemoryCandidate `json:"candidates"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type CorrectionSummary struct {
	ApprovedCount int64     `json:"approved_count"`
	MonthStart    time.Time `json:"month_start"`
	MonthEnd      time.Time `json:"month_end"`
	Timezone      string    `json:"timezone"`
}

type CorrectionEvaluationCase struct {
	CaseID            string           `json:"case_id" contract:"uuid"`
	CandidateID       string           `json:"candidate_id" contract:"uuid"`
	CandidateRevision int64            `json:"candidate_revision"`
	SourceFeedbackID  string           `json:"source_feedback_id" contract:"uuid"`
	ReviewID          string           `json:"review_id" contract:"uuid"`
	CanonicalReviewID string           `json:"canonical_review_id" contract:"uuid"`
	Question          string           `json:"question" contract:"max=4000"`
	ExpectedAnswer    string           `json:"expected_answer" contract:"max=8000"`
	ClaimID           string           `json:"claim_id" contract:"uuid"`
	ClaimRevision     int64            `json:"claim_revision"`
	Claim             ClaimInput       `json:"claim"`
	SourcePins        []SnapshotSource `json:"source_pins"`
	ProposerIssuer    string           `json:"proposer_issuer"`
	ProposerSubject   string           `json:"proposer_subject"`
	ReviewerIssuer    string           `json:"reviewer_issuer"`
	ReviewerSubject   string           `json:"reviewer_subject"`
	CaseSHA256        string           `json:"case_sha256" contract:"sha256"`
	CreatedAt         time.Time        `json:"created_at"`
}

type CorrectionEvaluationPage struct {
	Cases      []CorrectionEvaluationCase `json:"cases"`
	NextCursor string                     `json:"next_cursor,omitempty" contract:"uuid"`
}
