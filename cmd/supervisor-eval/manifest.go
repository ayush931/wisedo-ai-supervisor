package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

// frozenThresholdsSHA256 pins the WIS-280 thresholds (canonical digest of Thresholds). Changing
// a threshold changes this digest, so a weakened threshold is a reviewed code change, never a
// quiet manifest edit (rule 09). Regenerate only with the issue's approval: -print-hash.
const frozenThresholdsSHA256 = "90b803ca3c21135ee52ed9599db1c22e0966c7fe4665e33ca3bf87d7f581df00"

// Thresholds are copied from WIS-280 "Frozen engineering qualification thresholds" and the
// 2026-10-16 targets. Every leaf is a pointer so an absent threshold is refused, never read as
// zero. Decimals are canonical strings (rule 04).
type Thresholds struct {
	Status    *string `json:"status"`
	HardGates *struct {
		UnauthorizedEffectsMax           *int    `json:"unauthorized_effects_max"`
		CrossTenantOrSiteDisclosuresMax  *int    `json:"cross_tenant_or_site_disclosures_max"`
		ExpiredOrRevokedAuthorityExecMax *int    `json:"expired_or_revoked_authority_executions_max"`
		DuplicateInternalTasksMax        *int    `json:"duplicate_internal_tasks_max"`
		DeterministicAssertionPassRate   *string `json:"deterministic_assertion_pass_rate"`
	} `json:"hard_gates"`
	Citations *struct {
		ActionDrivingAuthorizedRate *string `json:"action_driving_authorized_source_rate"`
		CorrectnessMin              *string `json:"correctness_min"`
	} `json:"citations"`
	Disposition *struct {
		SupportedHeldOutMin       *string `json:"supported_held_out_min"`
		AbstentionMin             *string `json:"insufficient_evidence_abstention_min"`
		DisputedLabelHumanReviews *int    `json:"disputed_label_human_reviews_min"`
	} `json:"disposition"`
	Detection *struct {
		ActionableIncidentsMin       *int    `json:"actionable_incidents_min"`
		NormalWindowsMin             *int    `json:"normal_windows_min"`
		RecallMin                    *string `json:"recall_min"`
		PrecisionMin                 *string `json:"precision_min"`
		NonActionablePerLineShiftMax *int    `json:"non_actionable_alerts_per_line_per_8h_shift_max"`
	} `json:"detection"`
	Latency *struct {
		CompletenessEligibilitySeconds *int `json:"completeness_eligibility_seconds"`
		ContextToCaseP95Seconds        *int `json:"context_to_case_p95_seconds"`
		InvestigationP95Seconds        *int `json:"investigation_p95_seconds"`
	} `json:"latency"`
	Runs *struct {
		ActualRunsPerCase *int `json:"actual_runs_per_case"`
	} `json:"runs"`
	BaselineCorpus *struct {
		Cases                *int `json:"cases"`
		SupportedFlow        *int `json:"supported_flow"`
		MissingStaleConflict *int `json:"missing_stale_conflicting_identity"`
		AdversarialSecurity  *int `json:"adversarial_security"`
		RecoveryConcurrency  *int `json:"recovery_concurrency"`
		HeldOutMin           *int `json:"held_out_min"`
	} `json:"baseline_corpus"`
	Specialists *struct {
		DispositionGainPPMin       *int    `json:"disposition_gain_percentage_points_min"`
		ReviewerEffortReductionMin *string `json:"reviewer_effort_reduction_min"`
	} `json:"specialists"`
	Chat20261016 *struct {
		ReleaseCasesMin           *int    `json:"release_cases_min"`
		ProcessDocumentQA         *int    `json:"process_document_qa"`
		ExactProductionShift      *int    `json:"exact_production_shift"`
		MaterialQuality           *int    `json:"material_quality"`
		InsufficientConflicting   *int    `json:"insufficient_conflicting"`
		SecurityAdversarialMin    *int    `json:"security_adversarial_min"`
		RetrievalRecallAt10Min    *string `json:"retrieval_recall_at_10_min"`
		CitationSupportMin        *string `json:"citation_support_min"`
		ExactQuantitiesRate       *string `json:"exact_quantities_units_time_rate"`
		AbstentionMin             *string `json:"abstention_min"`
		UnauthorizedDisclosureMax *int    `json:"unauthorized_disclosure_or_effect_max"`
		InteractiveP95Seconds     *int    `json:"interactive_p95_seconds"`
		ConcurrentUsers           *int    `json:"concurrent_users"`
		HardDeadlineSeconds       *int    `json:"hard_deadline_seconds"`
	} `json:"chat_2026_10_16"`
	// Onboarding holds the WIS-640 interview-turn thresholds. Unlike the WIS-280 numbers above,
	// these are engineering proposals until the reviewer signs them off (Status), and an actual run
	// is not qualification evidence before then (qualificationBlockers).
	Onboarding *struct {
		Status                  *string `json:"status"`
		CasesMin                *int    `json:"cases_min"`
		SupportedMin            *int    `json:"supported_min"`
		AdversarialMin          *int    `json:"adversarial_min"`
		SupportedFactRecallMin  *string `json:"supported_fact_recall_min"`
		UngroundedPersistedMax  *int    `json:"ungrounded_persisted_facts_max"`
		UnauthorizedEffectsMax  *int    `json:"unauthorized_effects_max"`
		CrossTenantDisclosedMax *int    `json:"cross_tenant_disclosures_max"`
	} `json:"onboarding"`
	// OCR holds the WIS-625 page-reading thresholds: engineering proposals until signed off.
	OCR *struct {
		Status            *string `json:"status"`
		CasesMin          *int    `json:"cases_min"`
		CleanPageExactMin *string `json:"clean_page_exact_text_min"`
	} `json:"ocr"`
	// Verbatim keeps the issue's wording next to the numbers it was transcribed into.
	Verbatim []string `json:"verbatim"`
}

type Lifecycle struct {
	RunState          string   `json:"run_state"`
	OutcomeStatus     string   `json:"outcome_status"`
	OutcomeReasons    []string `json:"outcome_reasons"`
	ArtifactStored    *bool    `json:"artifact_stored"`
	ProviderCalls     *int     `json:"provider_calls"`
	ToolCodes         []string `json:"tool_codes"`
	ReplayMakesNoCall bool     `json:"replay_makes_no_call"`
}

type ReviewerLabels struct {
	Disposition        string `json:"disposition"`
	LabelledBy         string `json:"labelled_by"`
	IndependentReviews int    `json:"independent_reviews"`
}

// Case is one immutable eval case. Tenant and site are symbolic: every run creates fresh UUIDs
// for them, recorded in the report. Scenario names the scripted model behaviour (deterministic
// mode); everything asserted about the outcome is data here, not code.
type Case struct {
	CaseID            string            `json:"case_id"`
	Category          string            `json:"category"`
	Provenance        string            `json:"provenance"`
	IncidentID        string            `json:"incident_id"`
	Tenant            string            `json:"tenant"`
	Site              string            `json:"site"`
	Scenario          string            `json:"scenario"`
	SourceRevisions   map[string]int64  `json:"source_revisions"`
	ApprovalRevisions map[string]int64  `json:"approval_revisions"`
	SimulatedClock    string            `json:"simulated_clock"`
	SourcePermissions map[string]string `json:"source_permissions"`
	ExpectedReadiness string            `json:"expected_readiness"`
	AllowedTools      []string          `json:"allowed_tools"`
	ForbiddenTools    []string          `json:"forbidden_tools"`
	ExpectedCitations []string          `json:"expected_citations"`
	Lifecycle         Lifecycle         `json:"lifecycle_assertions"`
	ReviewerLabels    ReviewerLabels    `json:"reviewer_labels"`
	ActualEligible    bool              `json:"actual_eligible"`
	// ExpectedFailure is the reason code a gate control must FAIL with; empty for corpus cases.
	ExpectedFailure string `json:"expected_failure,omitempty"`
}

type Corpus struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

type Manifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Issue         string              `json:"issue"`
	Corpus        Corpus              `json:"corpus"`
	Thresholds    *Thresholds         `json:"thresholds"`
	Partitions    map[string][]string `json:"partitions"`
	Cases         []Case              `json:"cases"`
	// Controls are deliberately violating fixtures (WIS-280: a violating policy fixture and a
	// modified citation fixture). Every run executes them and requires each to FAIL.
	Controls []Case `json:"gate_controls"`
	// WIS-640: interview-turn and chat case sets with their own gate controls, which must FAIL.
	Onboarding         []OnboardingCase `json:"onboarding_cases"`
	OnboardingControls []OnboardingCase `json:"onboarding_controls"`
	ChatWorld          *ChatWorld       `json:"chat_world"`
	Chat               []ChatCase       `json:"chat_cases"`
	ChatControls       []ChatCase       `json:"chat_controls"`
	// ChatSynonyms is the WIS-647 retrieval comparison set (CASE=chat_synonyms only).
	ChatSynonyms []ChatCase `json:"chat_synonym_cases"`
	// WIS-625: OCR pages and their gate controls.
	OCR          []OCRCase `json:"ocr_cases"`
	OCRControls  []OCRCase `json:"ocr_controls"`
	CorpusSHA256 string    `json:"corpus_sha256"`
}

var (
	categories      = []string{"supported_flow", "missing_stale_conflicting_identity", "adversarial_security", "recovery_concurrency"}
	partitionNames  = []string{"train", "development", "held_out"}
	modelTools      = []string{agent.ToolContextGet, agent.ToolEvidenceGet, agent.ToolClaimsSearch, agent.ToolMeasurementsGet}
	sourceKeys      = []string{"shift_plan", "asset_register", "operational_source", "mapping"}
	approvalKeys    = []string{"plan", "policy"}
	permissionKeys  = []string{"shift_plan", "asset_register", "foreign_tenant_note"}
	citationTargets = []string{"measurement:" + recovery.MetricExpected, "measurement:" + recovery.MetricActual, "measurement:" + recovery.MetricDeficit}
	runStates       = []string{agent.RunCompleted, agent.RunDegraded, agent.RunCanceled, agent.RunFailed}
	outcomeStatuses = []string{"", recovery.StatusProposed, recovery.StatusNeedsInput, recovery.StatusRefused, recovery.StatusEscalated}
)

// loadManifest strictly decodes the manifest and refuses it unless the thresholds match the
// frozen digest and the declared corpus hash matches the cases. checkHash=false is only for
// -print-hash.
func loadManifest(path string, checkHash bool) (Manifest, string, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, "", err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, "", fmt.Errorf("manifest: %w", err)
	}
	if d.More() {
		return m, "", errors.New("manifest: trailing data")
	}
	if err = m.validate(); err != nil {
		return m, "", err
	}
	th, err := thresholdsDigest(m)
	if err != nil {
		return m, "", err
	}
	if checkHash && th != frozenThresholdsSHA256 {
		return m, "", fmt.Errorf("thresholds differ from the frozen WIS-280 set (digest %s)", th)
	}
	sum, err := m.hash()
	if err != nil {
		return m, "", err
	}
	if checkHash && sum != m.CorpusSHA256 {
		return m, "", fmt.Errorf("corpus_sha256 %q does not match the cases (%s)", m.CorpusSHA256, sum)
	}
	return m, sum, nil
}

func thresholdsDigest(m Manifest) (string, error) {
	_, sum, err := canonical.Digest("supervisor_eval_thresholds", 1, m.Thresholds)
	return sum, err
}

// hash is the manifest hash: every case, control, partition and threshold, excluding the
// declared hash itself (rule 04).
func (m Manifest) hash() (string, error) {
	_, sum, err := canonical.Digest("supervisor_eval_corpus", 1, struct {
		SchemaVersion int                 `json:"schema_version"`
		Issue         string              `json:"issue"`
		Corpus        Corpus              `json:"corpus"`
		Thresholds    *Thresholds         `json:"thresholds"`
		Partitions    map[string][]string `json:"partitions"`
		Cases         []Case              `json:"cases"`
		Controls      []Case              `json:"gate_controls"`
		Onboarding    []OnboardingCase    `json:"onboarding_cases"`
		OnbControls   []OnboardingCase    `json:"onboarding_controls"`
		ChatWorld     *ChatWorld          `json:"chat_world"`
		Chat          []ChatCase          `json:"chat_cases"`
		ChatControls  []ChatCase          `json:"chat_controls"`
		ChatSynonyms  []ChatCase          `json:"chat_synonym_cases"`
		OCR           []OCRCase           `json:"ocr_cases"`
		OCRControls   []OCRCase           `json:"ocr_controls"`
	}{m.SchemaVersion, m.Issue, m.Corpus, m.Thresholds, m.Partitions, m.Cases, m.Controls, m.Onboarding, m.OnboardingControls, m.ChatWorld, m.Chat, m.ChatControls, m.ChatSynonyms, m.OCR, m.OCRControls})
	return sum, err
}

func (m Manifest) validate() error {
	if m.SchemaVersion != 1 || m.Issue != "WIS-280" || m.Corpus.Kind == "" || m.Corpus.Description == "" {
		return errors.New("manifest header is incomplete")
	}
	if m.Thresholds == nil {
		return errors.New("thresholds are missing")
	}
	if path := nilLeaf(reflect.ValueOf(*m.Thresholds), "thresholds"); path != "" {
		return fmt.Errorf("threshold %s is missing", path)
	}
	if len(m.Thresholds.Verbatim) == 0 {
		return errors.New("threshold thresholds.verbatim is missing")
	}
	if len(m.Cases) == 0 || len(m.Controls) == 0 {
		return errors.New("cases and gate_controls are required")
	}
	ids := map[string]bool{}
	for _, c := range m.Cases {
		if c.ExpectedFailure != "" {
			return fmt.Errorf("case %q: expected_failure belongs only to gate_controls", c.CaseID)
		}
	}
	for _, c := range m.Controls {
		if !slices.Contains(failureCodes, c.ExpectedFailure) {
			return fmt.Errorf("gate control %q: expected_failure %q is not a known failure code", c.CaseID, c.ExpectedFailure)
		}
	}
	for _, c := range append(slices.Clone(m.Cases), m.Controls...) {
		if ids[c.CaseID] {
			return fmt.Errorf("duplicate case_id %q", c.CaseID)
		}
		ids[c.CaseID] = true
		if err := c.validate(); err != nil {
			return fmt.Errorf("case %q: %w", c.CaseID, err)
		}
	}
	// Knowledge cases first: a duplicate ID is reported as one, not as a partition mismatch.
	if err := m.validateKnowledge(ids); err != nil {
		return err
	}
	// Every corpus case sits in exactly one partition, and an incident never spans partitions
	// (WIS-280: partition by incident, never excerpts of one incident across splits). A
	// knowledge corpus case (onboarding, chat, OCR) is its own incident: each onboarding case and
	// OCR page has its own document, and chat cases are separate questions over one approved
	// world, which is the fixture they all ask, not an incident (WIS-646). Controls sit outside.
	for name := range m.Partitions {
		if !slices.Contains(partitionNames, name) {
			return fmt.Errorf("unknown partition %q", name)
		}
	}
	incident := map[string]string{}
	for _, c := range m.Cases {
		incident[c.CaseID] = c.IncidentID
	}
	for _, id := range m.knowledgeCaseIDs() {
		if _, dup := incident[id]; !dup {
			incident[id] = id
		}
	}
	partitionOf, incidentPartition := map[string]string{}, map[string]string{}
	for _, name := range partitionNames {
		ids, ok := m.Partitions[name]
		if !ok {
			return fmt.Errorf("partition %q is missing", name)
		}
		for _, id := range ids {
			inc, known := incident[id]
			switch {
			case !known:
				return fmt.Errorf("partition %q lists unknown case %q", name, id)
			case partitionOf[id] != "":
				return fmt.Errorf("case %q is in partitions %q and %q", id, partitionOf[id], name)
			case incidentPartition[inc] != "" && incidentPartition[inc] != name:
				return fmt.Errorf("incident %q leaks across partitions %q and %q", inc, incidentPartition[inc], name)
			}
			partitionOf[id], incidentPartition[inc] = name, name
		}
	}
	corpus := m.knowledgeCaseIDs()
	for _, c := range m.Cases {
		corpus = append(corpus, c.CaseID)
	}
	for _, id := range corpus {
		if partitionOf[id] == "" {
			return fmt.Errorf("case %q is in no partition", id)
		}
	}
	return nil
}

// knowledgeCaseIDs lists the onboarding, chat and OCR corpus cases (not their controls).
func (m Manifest) knowledgeCaseIDs() []string {
	var ids []string
	for _, c := range m.Onboarding {
		ids = append(ids, c.CaseID)
	}
	for _, c := range append(slices.Clone(m.Chat), m.ChatSynonyms...) {
		ids = append(ids, c.CaseID)
	}
	for _, c := range m.OCR {
		ids = append(ids, c.CaseID)
	}
	return ids
}

// partitionOf maps each partitioned case to its partition.
func (m Manifest) partitionOf() map[string]string {
	out := map[string]string{}
	for name, ids := range m.Partitions {
		for _, id := range ids {
			out[id] = name
		}
	}
	return out
}

// nilLeaf returns the JSON path of the first nil pointer in a thresholds value.
func nilLeaf(v reflect.Value, path string) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return path
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	for i := range v.NumField() {
		tag, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
		if p := nilLeaf(v.Field(i), path+"."+tag); p != "" {
			return p
		}
	}
	return ""
}

func (c Case) validate() error {
	l := c.Lifecycle
	switch {
	case c.CaseID == "" || c.IncidentID == "" || c.Tenant == "" || c.Site == "":
		return errors.New("case_id, incident_id, tenant and site are required")
	case !slices.Contains(categories, c.Category):
		return fmt.Errorf("unknown category %q", c.Category)
	case c.Provenance != "synthetic" && c.Provenance != "sanitized_customer":
		return fmt.Errorf("unknown provenance %q", c.Provenance)
	case scenarios[c.Scenario] == nil:
		return fmt.Errorf("unknown scenario %q", c.Scenario)
	case !sameKeys(c.SourceRevisions, sourceKeys) || !sameKeys(c.ApprovalRevisions, approvalKeys) || !sameKeys(c.SourcePermissions, permissionKeys):
		return errors.New("source_revisions, approval_revisions and source_permissions must name exactly their fixed keys")
	case c.ExpectedReadiness != "READY" && c.ExpectedReadiness != "LIMITED" && c.ExpectedReadiness != "NOT_READY" && c.ExpectedReadiness != "INVALID":
		return fmt.Errorf("unknown expected_readiness %q", c.ExpectedReadiness)
	case c.AllowedTools == nil || c.ForbiddenTools == nil || c.ExpectedCitations == nil || l.OutcomeReasons == nil || l.ToolCodes == nil:
		return errors.New("allowed_tools, forbidden_tools, expected_citations, outcome_reasons and tool_codes are required (use [] for none)")
	case !slices.Contains(runStates, l.RunState) || !slices.Contains(outcomeStatuses, l.OutcomeStatus) || l.ArtifactStored == nil || l.ProviderCalls == nil || *l.ProviderCalls < 0:
		return errors.New("lifecycle_assertions are incomplete")
	case c.ReviewerLabels.Disposition == "" || c.ReviewerLabels.LabelledBy == "" || c.ReviewerLabels.IndependentReviews < 0:
		return errors.New("reviewer_labels are incomplete")
	}
	for _, v := range c.SourceRevisions {
		if v < 1 {
			return errors.New("source revisions start at 1")
		}
	}
	for _, v := range c.ApprovalRevisions {
		if v < 1 {
			return errors.New("approval revisions start at 1")
		}
	}
	for _, v := range c.SourcePermissions {
		if v != "read" && v != "none" {
			return fmt.Errorf("unknown source permission %q", v)
		}
	}
	if t, err := time.Parse(time.RFC3339, c.SimulatedClock); err != nil || t.UTC().Format(time.RFC3339) != c.SimulatedClock {
		return errors.New("simulated_clock must be RFC3339 UTC")
	}
	// Rule 08: a model may only ever be offered the four read tools.
	for _, t := range c.AllowedTools {
		if !slices.Contains(modelTools, t) {
			return fmt.Errorf("allowed tool %q is outside the rule 08 model tool set", t)
		}
	}
	for _, t := range c.ForbiddenTools {
		if t == "" {
			return errors.New("empty forbidden tool")
		}
	}
	for _, ref := range c.ExpectedCitations {
		if !slices.Contains(citationTargets, ref) {
			return fmt.Errorf("unknown expected citation %q", ref)
		}
	}
	return nil
}

func sameKeys[V any](m map[string]V, keys []string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
