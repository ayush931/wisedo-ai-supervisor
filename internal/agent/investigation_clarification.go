package agent

import (
	"slices"
	"strconv"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/recovery"
)

func missingInspectionInputs(in recovery.Input) []string {
	missing := []string{}
	if !slices.ContainsFunc(in.Evidence, func(e recovery.Evidence) bool { return e.Kind == recovery.EvidenceMachineFault }) {
		missing = append(missing, "machine_fault")
	}
	if len(in.Roster) == 0 {
		missing = append(missing, "confirmed_assignee")
	}
	return missing
}

// missingInputProposal preserves the specialist's questions in the existing advisor artifact.
// Identity, observations and versions come from Go; untrusted text still passes recovery.Judge.
func missingInputProposal(in CaseInput, run Run, res SpecialistResultV1) recovery.RecoveryProposalV1 {
	current := in.Input.Contexts[len(in.Input.Contexts)-1].Snapshot
	p := recovery.RecoveryProposalV1{
		ProposalID: run.ID, CaseID: in.Input.Case.ID, CaseRevision: in.Input.Case.Revision,
		ContextID: current.ContextID, ContextRevision: current.Revision,
		Observations: []recovery.Observation{}, Hypotheses: []recovery.Hypothesis{}, Unknowns: []recovery.Unknown{},
		Options:        []recovery.Option{{Kind: recovery.OptionClarify, Text: "Ask the supervisor to supply the missing input or reconcile the conflicting evidence.", EvidenceIDs: res.CitedEvidenceIDs}},
		SelectedOption: recovery.OptionClarify,
		Versions:       recovery.Versions{Policy: in.Input.PolicyVersion, Graph: in.Input.GraphVersion, Model: run.Model, Prompt: run.PromptVersion, Tool: run.ToolVersion},
		GeneratedAt:    in.Input.Now, ExpiresAt: in.Input.Now.Add(approvals.ApprovalTTL),
		Rationale: "The supervisor must supply the missing input or reconcile the conflicting evidence before an inspection can proceed.",
	}
	if in.Input.CurrentPlan != nil {
		p.Versions.Plan = in.Input.CurrentPlan.PlanRevision
	}
	var snapshots []plantcontext.Snapshot
	for _, context := range in.Input.Contexts {
		snapshots = append(snapshots, context.Snapshot)
	}
	for _, m := range recovery.Measurements(snapshots) {
		if m.MetricName == recovery.MetricDeficit {
			p.Observations = append(p.Observations, recovery.Observation{MeasurementID: m.ID, EvidenceIDs: []string{m.ID}, MetricName: m.MetricName, ValueDecimal: m.Value, Unit: m.Unit, WindowStart: m.WindowStart, WindowEnd: m.WindowEnd})
		}
	}
	for i, question := range res.MissingRequiredFields {
		// Translate exact field names without changing a model's free-text question or facts.
		switch question {
		case "confirmed_assignee":
			question = "Which maintenance assignee can the supervisor confirm for this line?"
		case "machine_fault":
			question = "Can the supervisor provide trustworthy machine-state evidence of an unexplained stoppage during the case windows?"
		}
		p.Unknowns = append(p.Unknowns, recovery.Unknown{Field: "required_input_" + strconv.Itoa(i+1), Question: question, Required: true})
	}
	for i, conflict := range res.Contradictions {
		p.Unknowns = append(p.Unknowns, recovery.Unknown{Field: "conflicting_evidence_" + strconv.Itoa(i+1), Question: conflict, Required: true})
	}
	return p
}
