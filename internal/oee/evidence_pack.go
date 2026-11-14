package oee

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
)

// OeeEvidencePackV1 is the read-only contract Reporting consumes (WIS-228, WIS-221). It is
// self-contained: a consumer renders it without database access and without recomputing
// OEE. It carries identifiers and digests for provenance, never actor identities,
// justifications or raw payloads.

// OeeEvidencePackSchemaVersion versions the canonical pack body.
const OeeEvidencePackSchemaVersion = 1

// ErrInvalidEvidenceInput means the pack inputs are incomplete or inconsistent.
var ErrInvalidEvidenceInput = errors.New("invalid evidence pack input")

type PackRatio struct {
	Value       string `json:"value"` // decimal at the policy scale; empty when not published
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
}

type PackKPIs struct {
	Availability PackRatio `json:"availability"`
	Performance  PackRatio `json:"performance"`
	Quality      PackRatio `json:"quality"`
	OEE          PackRatio `json:"oee"`
	Loading      PackRatio `json:"loading"`
	TEEP         PackRatio `json:"teep"`
}

// PackDurations are exact seconds as rational strings.
type PackDurations struct {
	Calendar          string `json:"calendar"`
	PlannedExclusion  string `json:"planned_exclusion"`
	PlannedProduction string `json:"planned_production"`
	Run               string `json:"run"`
	Stop              string `json:"stop"`
	Microstop         string `json:"microstop"`
	Gap               string `json:"gap"`
	PlannedLoss       string `json:"planned_loss"`
	UnplannedLoss     string `json:"unplanned_loss"`
	UnclassifiedLoss  string `json:"unclassified_loss"`
}

type PackParetoRow struct {
	Rank                 int                `json:"rank"`
	ReasonCode           string             `json:"reason_code"`
	Name                 string             `json:"name"`
	LossCategory         LossCategory       `json:"loss_category"`
	Classification       LossClassification `json:"classification"`
	DurationSeconds      string             `json:"duration_seconds"`
	LostQuantity         string             `json:"lost_quantity"`
	Percentage           string             `json:"percentage"`
	CumulativePercentage string             `json:"cumulative_percentage"`
}

type PackLossNode struct {
	Path                 string             `json:"path"`
	ReasonCode           string             `json:"reason_code"`
	LossCategory         LossCategory       `json:"loss_category"`
	Classification       LossClassification `json:"classification"`
	TotalDurationSeconds string             `json:"total_duration_seconds"`
	TotalLostQuantity    string             `json:"total_lost_quantity"`
	ContributionCount    int                `json:"contribution_count"`
}

type PackDataQuality struct {
	CoverageRatio string       `json:"coverage_ratio"`
	GapSeconds    string       `json:"gap_seconds"`
	Status        Status       `json:"status"`
	ReasonCodes   []ReasonCode `json:"reason_codes"`
}

type PackVersions struct {
	FormulaVersion     string   `json:"formula_version"`
	PolicyVersion      string   `json:"policy_version"`
	KernelVersion      string   `json:"kernel_version"`
	PolicyDigest       string   `json:"policy_digest"`
	ReworkCountsAsGood bool     `json:"rework_counts_as_good"`
	MicrostopThreshold string   `json:"microstop_threshold_sec"`
	TaxonomyRevision   string   `json:"taxonomy_revision"`
	TaxonomyDigest     string   `json:"taxonomy_digest"`
	CostPolicyRevision int64    `json:"cost_policy_revision"`
	RateRevisions      []string `json:"rate_revisions"`
	FXRevisions        []string `json:"fx_revisions"`
}

type PackSources struct {
	SourceEventIDs      []string `json:"source_event_ids"`
	ContributionDigests []string `json:"contribution_digests"`
}

type PackDigests struct {
	NormalizedInput string `json:"normalized_input"`
	Calculation     string `json:"calculation"`
	Classification  string `json:"classification"`
}

type OeeEvidencePackV1 struct {
	SchemaVersion  int             `json:"schema_version"`
	EvidenceID     string          `json:"evidence_id"`
	RunID          string          `json:"run_id"`
	SiteID         string          `json:"site_id"`
	WindowStart    string          `json:"window_start"`
	WindowEnd      string          `json:"window_end"`
	CalculatedAt   string          `json:"calculated_at"`
	Status         Status          `json:"status"`
	ReasonCodes    []ReasonCode    `json:"reason_codes"`
	KPIs           PackKPIs        `json:"kpis"`
	Durations      PackDurations   `json:"durations"`
	DowntimePareto []PackParetoRow `json:"downtime_pareto"`
	LossTree       []PackLossNode  `json:"loss_tree"`
	Cost           CostResult      `json:"cost"`
	DataQuality    PackDataQuality `json:"data_quality"`
	Versions       PackVersions    `json:"versions"`
	Sources        PackSources     `json:"sources"`
	Digests        PackDigests     `json:"digests"`
	Digest         string          `json:"digest"`
}

// EvidencePackInput is everything the pure builder reads. CalculatedAt is supplied by the
// orchestrator; the builder reads no clock.
type EvidencePackInput struct {
	TenantID, SiteID      string
	EvidenceID, RunID     string
	CalculatedAt          time.Time
	Policy                KPIPolicy
	KPI                   KPICalculationResult
	Losses                *ClassificationResult
	Timeline              TimelineResolutionResult
	Cost                  CostResult
	NormalizedInputDigest string
}

// packRat keeps an absent value empty rather than "0": unknown is not zero (rule 06).
func packRat(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.RatString()
}

func packRatio(m RatioMetric) PackRatio {
	return PackRatio{Value: m.Formatted, Numerator: packRat(m.Numerator), Denominator: packRat(m.Denominator)}
}

// BuildOeeEvidencePack assembles and seals the evidence pack. The pack digest is the
// canonical SHA-256 bound to tenant and site; identical inputs give an identical pack.
func BuildOeeEvidencePack(in EvidencePackInput) (OeeEvidencePackV1, error) {
	bad := func(f string, a ...any) (OeeEvidencePackV1, error) {
		return OeeEvidencePackV1{}, fmt.Errorf("%w: "+f, append([]any{ErrInvalidEvidenceInput}, a...)...)
	}
	if !canonicalCostID(in.EvidenceID) || !canonicalCostID(in.RunID) || !canonicalCostID(in.SiteID) {
		return bad("evidence_id, run_id and site_id must be canonical UUIDs")
	}
	if in.CalculatedAt.IsZero() || in.Losses == nil || in.KPI.CalculationDigest == "" || in.Losses.Digest == "" {
		return bad("calculated_at, a sealed KPI result and a sealed loss result are required")
	}
	if !evidenceSHA256.MatchString(in.NormalizedInputDigest) {
		return bad("normalized_input digest must be sha256 hex")
	}
	// Provenance cannot cross datasets: the loss explanation must belong to this tenant and site.
	if in.Losses.TenantID != in.TenantID || in.Losses.SiteID != in.SiteID {
		return bad("loss result belongs to another tenant or site")
	}
	if !in.KPI.Window.Start.Equal(in.Losses.Window.Start) || !in.KPI.Window.End.Equal(in.Losses.Window.End) {
		return bad("KPI and loss windows differ")
	}
	utc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

	planned, unplanned, unclassified := new(big.Rat), new(big.Rat), new(big.Rat)
	events := map[string]bool{}
	digests := []string{}
	for _, c := range in.Losses.Contributions {
		d := c.DurationSeconds
		if d == nil {
			d = new(big.Rat)
		}
		switch c.Classification {
		case ClassificationPlanned:
			planned.Add(planned, d)
		case ClassificationUnplanned:
			unplanned.Add(unplanned, d)
		default:
			unclassified.Add(unclassified, d)
		}
		for _, id := range c.SourceEventIDs {
			events[id] = true
		}
		digests = append(digests, c.Digest)
	}
	eventIDs := sortedKeys(events)
	sort.Strings(digests)

	pareto := make([]PackParetoRow, 0, len(in.Losses.ParetoDuration.Items))
	for _, it := range in.Losses.ParetoDuration.Items {
		pareto = append(pareto, PackParetoRow{
			Rank: it.Rank, ReasonCode: it.ReasonCode, Name: it.Name, LossCategory: it.LossCategory, Classification: it.Classification,
			DurationSeconds: packRat(it.DurationSeconds), LostQuantity: packRat(it.LostQuantity),
			Percentage: it.Percentage, CumulativePercentage: it.CumulativePercentage,
		})
	}
	tree := []PackLossNode{}
	var walk func(n *LossTreeNode, path string)
	walk = func(n *LossTreeNode, path string) {
		if n == nil {
			return
		}
		p := path + "/" + n.ReasonCode
		if path == "" {
			p = n.ReasonCode
		}
		tree = append(tree, PackLossNode{Path: p, ReasonCode: n.ReasonCode, LossCategory: n.LossCategory, Classification: n.Classification,
			TotalDurationSeconds: packRat(n.TotalDurationSeconds), TotalLostQuantity: packRat(n.TotalLostQuantity), ContributionCount: n.ContributionCount})
		children := append([]*LossTreeNode(nil), n.Children...)
		sort.Slice(children, func(i, j int) bool { return children[i].ReasonCode < children[j].ReasonCode })
		for _, c := range children {
			walk(c, p)
		}
	}
	walk(in.Losses.LossTree, "")

	rates := []string{}
	for _, r := range in.Cost.RateAssumptions {
		rates = append(rates, fmt.Sprintf("%s@%d", r.RateID, r.Revision))
	}
	fx := []string{}
	for _, f := range in.Cost.FXAssumptions {
		fx = append(fx, fmt.Sprintf("%s@%d", f.FXID, f.Revision))
	}
	taxonomyRevision := ""
	if len(in.Losses.Contributions) > 0 {
		taxonomyRevision = in.Losses.Contributions[0].TaxonomyRevisionID
	}
	// The policy flags recorded in the pack must be the ones that produced the KPI: normalize
	// the supplied policy exactly as the kernel did and require the same digest (rule 04).
	policy, err := normalizeKPIPolicy(in.Policy)
	if err != nil || policy.PolicyDigest != in.KPI.PolicyDigest {
		return bad("the policy does not match the KPI result's policy digest")
	}
	reasonCodes := append([]ReasonCode{}, in.KPI.Readiness.ReasonCodes...)
	timelineCodes := append([]ReasonCode{}, in.Timeline.Readiness.ReasonCodes...)
	microstop := policy.MicrostopThresholdSec.RatString()

	pack := OeeEvidencePackV1{
		SchemaVersion: OeeEvidencePackSchemaVersion, EvidenceID: in.EvidenceID, RunID: in.RunID, SiteID: in.SiteID,
		WindowStart: utc(in.KPI.Window.Start), WindowEnd: utc(in.KPI.Window.End), CalculatedAt: utc(in.CalculatedAt),
		Status: in.KPI.Readiness.Status, ReasonCodes: reasonCodes,
		KPIs: PackKPIs{packRatio(in.KPI.Availability), packRatio(in.KPI.Performance), packRatio(in.KPI.Quality), packRatio(in.KPI.OEE), packRatio(in.KPI.Loading), packRatio(in.KPI.TEEP)},
		Durations: PackDurations{
			Calendar: packRat(in.KPI.TotalCalendarSeconds), PlannedExclusion: packRat(in.KPI.PlannedExclusionSeconds),
			PlannedProduction: packRat(in.KPI.PlannedProductionSeconds), Run: packRat(in.KPI.RunSeconds), Stop: packRat(in.KPI.StopSeconds),
			Microstop: packRat(in.KPI.MicrostopSeconds), Gap: packRat(in.KPI.GapSeconds),
			PlannedLoss: planned.RatString(), UnplannedLoss: unplanned.RatString(), UnclassifiedLoss: unclassified.RatString(),
		},
		DowntimePareto: pareto, LossTree: tree, Cost: in.Cost,
		DataQuality: PackDataQuality{CoverageRatio: packRat(in.Timeline.CoverageRatio), GapSeconds: packRat(in.Timeline.GapSeconds), Status: in.Timeline.Readiness.Status, ReasonCodes: timelineCodes},
		Versions: PackVersions{
			FormulaVersion: in.KPI.FormulaVersion, PolicyVersion: in.KPI.PolicyVersion, KernelVersion: in.KPI.KernelVersion, PolicyDigest: in.KPI.PolicyDigest,
			ReworkCountsAsGood: policy.ReworkCountsAsGood, MicrostopThreshold: microstop,
			TaxonomyRevision: taxonomyRevision, TaxonomyDigest: in.Losses.TaxonomyDigest,
			CostPolicyRevision: in.Cost.PolicyRevision, RateRevisions: rates, FXRevisions: fx,
		},
		Sources: PackSources{SourceEventIDs: eventIDs, ContributionDigests: digests},
		Digests: PackDigests{NormalizedInput: in.NormalizedInputDigest, Calculation: in.KPI.CalculationDigest, Classification: in.Losses.Digest},
	}
	if pack.Status == StatusReady && in.Cost.Status != StatusReady {
		// A monetary gap is visible: the pack is LIMITED, never a READY pack with missing cost.
		pack.Status = StatusLimited
	}
	if in.Timeline.Readiness.Status != "" && in.Timeline.Readiness.Status != StatusReady && pack.Status == StatusReady {
		pack.Status = in.Timeline.Readiness.Status
	}
	digest, err := OeeEvidencePackDigest(in.TenantID, pack)
	if err != nil {
		return OeeEvidencePackV1{}, err
	}
	pack.Digest = digest
	return pack, nil
}

// OeeEvidencePackDigest recomputes the canonical pack digest, so a consumer can verify a pack
// it received. Rule 04: the digest covers the schema-ordered body plus schema version and
// tenant/site, and excludes the digest itself, the generated evidence and run IDs and the
// calculation timestamp. The pack still carries those fields; they just do not change the
// digest, so identical inputs sealed at different times share one digest.
func OeeEvidencePackDigest(tenantID string, pack OeeEvidencePackV1) (string, error) {
	pack.Digest, pack.EvidenceID, pack.RunID, pack.CalculatedAt = "", "", "", ""
	_, digest, err := canonical.Envelope("OeeEvidencePackV1", OeeEvidencePackSchemaVersion, tenantID, pack.SiteID, pack)
	return digest, err
}

// safeCell neutralizes spreadsheet formula injection: a cell that a spreadsheet would
// evaluate (=, +, -, @, tab, carriage return) is prefixed with a single quote.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// RenderEvidenceCSV is the WIS-221 consumer rendering: it reads only the pack (no database,
// no recomputation) and writes section,key,value rows with formula-safe cells. Monetary rows
// are labelled modeled_opportunity; nothing is labelled savings.
func RenderEvidenceCSV(pack OeeEvidencePackV1) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	row := func(cells ...string) {
		for i := range cells {
			cells[i] = safeCell(cells[i])
		}
		_ = w.Write(cells)
	}
	row("section", "key", "value")
	row("pack", "evidence_id", pack.EvidenceID)
	row("pack", "window", pack.WindowStart+"/"+pack.WindowEnd)
	row("pack", "status", string(pack.Status))
	row("pack", "digest", pack.Digest)
	for _, k := range []struct {
		name string
		r    PackRatio
	}{{"availability", pack.KPIs.Availability}, {"performance", pack.KPIs.Performance}, {"quality", pack.KPIs.Quality}, {"oee", pack.KPIs.OEE}, {"loading", pack.KPIs.Loading}, {"teep", pack.KPIs.TEEP}} {
		row("kpi", k.name, k.r.Value)
	}
	row("duration", "planned_loss_seconds", pack.Durations.PlannedLoss)
	row("duration", "unplanned_loss_seconds", pack.Durations.UnplannedLoss)
	row("duration", "unclassified_loss_seconds", pack.Durations.UnclassifiedLoss)
	for _, p := range pack.DowntimePareto {
		row("pareto", strconv.Itoa(p.Rank)+" "+p.ReasonCode, p.Name+" | "+p.DurationSeconds+"s | "+p.Percentage)
	}
	row("cost", "currency", string(pack.Cost.Currency))
	row("cost", "modeled_opportunity", pack.Cost.ModeledOpportunity)
	row("cost", "avoided_loss", "not computed")
	row("cost", "realized_savings", "not computed")
	for _, l := range pack.Cost.Lines {
		value := l.ModeledOpportunity
		if len(l.CostReasonCodes) > 0 {
			codes := make([]string, len(l.CostReasonCodes))
			for i, c := range l.CostReasonCodes {
				codes[i] = string(c)
			}
			value = "not priced: " + strings.Join(codes, ",")
		}
		row("cost_line", l.ContributionID+" "+l.ReasonCode, value)
	}
	for _, a := range pack.Cost.RateAssumptions {
		row("assumption", a.RateID+"@"+strconv.FormatInt(a.Revision, 10), fmt.Sprintf("%s hourly=%s burden=%s unit_margin=%s %s..%s", a.Currency, a.HourlyRate, a.BurdenRatio, a.UnitMargin, a.EffectiveStart, a.EffectiveEnd))
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// ReferenceCalculation is an approved customer or reference OEE calculation to reconcile
// against, with the policy choices it made.
type ReferenceCalculation struct {
	Source       string `json:"source"`
	Availability string `json:"availability"`
	Performance  string `json:"performance"`
	Quality      string `json:"quality"`
	OEE          string `json:"oee"`
	// The reference's policy choices. Nil means the reference did not state the choice: it is
	// unknown, not false, so it never produces a policy variance (rule 06).
	ReworkCountsAsGood           *bool `json:"rework_counts_as_good,omitempty"`
	MicrostopsAsAvailabilityLoss *bool `json:"microstops_as_availability_loss,omitempty"`
	PlannedBreaksExcluded        *bool `json:"planned_breaks_excluded,omitempty"`
}

type ReconciliationRow struct {
	Metric     string `json:"metric"`
	Pack       string `json:"pack"`
	Reference  string `json:"reference"`
	Difference string `json:"difference"`
	Matches    bool   `json:"matches"`
}

// ReconciliationReport compares an evidence pack with a reference calculation and names
// the policy variances that explain differences. Matching is exact on the decimal values.
type ReconciliationReport struct {
	Source          string              `json:"source"`
	Rows            []ReconciliationRow `json:"rows"`
	PolicyVariances []string            `json:"policy_variances"`
	Unexplained     bool                `json:"unexplained"`
}

// Reconcile is the reconciliation template: per-metric exact differences plus the stated
// policy differences between the pack and the reference that can explain them. Each variance
// names the metrics it can move; a metric that differs with no variance covering it makes the
// report unexplained, so an unrelated policy difference never excuses a mismatch.
func Reconcile(pack OeeEvidencePackV1, ref ReferenceCalculation) (ReconciliationReport, error) {
	report := ReconciliationReport{Source: ref.Source, Rows: []ReconciliationRow{}, PolicyVariances: []string{}}
	explained := map[string]bool{}
	variance := func(text string, metrics ...string) {
		report.PolicyVariances = append(report.PolicyVariances, text)
		for _, m := range metrics {
			explained[m] = true
		}
	}
	if ref.ReworkCountsAsGood != nil && *ref.ReworkCountsAsGood != pack.Versions.ReworkCountsAsGood {
		variance(fmt.Sprintf("rework_counts_as_good: pack %t, reference %t (affects quality and OEE)", pack.Versions.ReworkCountsAsGood, *ref.ReworkCountsAsGood), "quality", "oee")
	}
	if ref.MicrostopsAsAvailabilityLoss != nil && *ref.MicrostopsAsAvailabilityLoss && pack.Versions.MicrostopThreshold != "0" {
		variance("microstops: pack treats stops up to "+pack.Versions.MicrostopThreshold+"s as performance loss (ADR 0005 Sec. 11); reference counts them against availability (affects availability, performance and OEE)", "availability", "performance", "oee")
	}
	if ref.PlannedBreaksExcluded != nil && !*ref.PlannedBreaksExcluded {
		// The pack does not record its state buckets; its policy digest identifies them.
		variance("planned breaks: reference keeps them in planned production time; the pack applies its policy's state buckets (policy "+pack.Versions.PolicyDigest+") (affects availability and OEE)", "availability", "oee")
	}
	unexplained := false
	for _, m := range []struct{ name, pack, ref string }{
		{"availability", pack.KPIs.Availability.Value, ref.Availability},
		{"performance", pack.KPIs.Performance.Value, ref.Performance},
		{"quality", pack.KPIs.Quality.Value, ref.Quality},
		{"oee", pack.KPIs.OEE.Value, ref.OEE},
	} {
		row := ReconciliationRow{Metric: m.name, Pack: m.pack, Reference: m.ref}
		p, okP := new(big.Rat).SetString(m.pack)
		r, okR := new(big.Rat).SetString(m.ref)
		if !okP || !okR {
			if m.ref != "" && m.pack != "" {
				return ReconciliationReport{}, fmt.Errorf("%w: %s is not a decimal", ErrInvalidEvidenceInput, m.name)
			}
			row.Difference = "not comparable"
		} else {
			d := new(big.Rat).Sub(p, r)
			row.Difference = d.FloatString(6)
			row.Matches = d.Sign() == 0
		}
		if !row.Matches && !explained[m.name] {
			unexplained = true
		}
		report.Rows = append(report.Rows, row)
	}
	report.Unexplained = unexplained
	return report, nil
}
