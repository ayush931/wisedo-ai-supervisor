package oee

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/google/uuid"
)

// Wire contracts for the WIS-229 service boundary. Requests carry bounded owner facts inline
// and are never stored; only the canonical input digest, the governed policy revision and
// the calculated result are persisted. Decimals are canonical strings and times RFC 3339
// with an explicit offset; tenant and actor never appear in a payload.

// Request bounds, checked before any fact is materialized (the kernel adds its own).
const (
	MaxRequestFacts      = MaxTimelineFacts
	MaxRequestExclusions = 1000
	MaxRequestCounts     = 1000
	MaxRequestCycles     = 200
	ServiceSchemaVersion = 1
)

// ErrInvalidRequest means a request breaks the OeeCalculationRequestV1 contract (422).
var ErrInvalidRequest = errors.New("invalid OEE calculation request")

// StateBucketV1 maps one owner-fact state to its time bucket.
type StateBucketV1 struct {
	State  TimelineState `json:"state"`
	Bucket StateBucket   `json:"bucket"`
}

// OeePolicyBodyV1 is a governed calculation policy: timeline and KPI rules together. It is
// stored as an immutable revision and used only after a second person publishes it.
type OeePolicyBodyV1 struct {
	PolicyVersion             string                 `json:"policy_version"`
	PrecedenceOrder           []TimelineState        `json:"precedence_order"`
	MinCoverage               string                 `json:"min_coverage"`
	MaxSourceSilenceSeconds   int64                  `json:"max_source_silence_seconds"`
	RoundingScale             int                    `json:"rounding_scale"`
	RoundingMode              RoundingMode           `json:"rounding_mode"`
	ZeroDenominatorRule       ZeroDenomBehavior      `json:"zero_denominator_rule"`
	PerformanceCapRule        PerformanceCapBehavior `json:"performance_cap_rule"`
	ReworkCountsAsGood        bool                   `json:"rework_counts_as_good"`
	MicrostopThresholdSeconds string                 `json:"microstop_threshold_seconds"`
	StateBuckets              []StateBucketV1        `json:"state_buckets"`
}

type OeeFactV1 struct {
	EventID       string        `json:"event_id"`
	SourceKey     string        `json:"source_key"`
	State         TimelineState `json:"state"`
	Start         string        `json:"start"`
	End           string        `json:"end"`
	EventAt       string        `json:"event_at"`
	RecordedAt    string        `json:"recorded_at"`
	Revision      int64         `json:"revision"`
	PayloadDigest string        `json:"payload_digest"`
}

type OeeExclusionV1 struct {
	ExclusionID string        `json:"exclusion_id"`
	State       TimelineState `json:"state"` // PLANNED_BREAK, CHANGEOVER or SHUTDOWN_EXCLUSION
	Start       string        `json:"start"`
	End         string        `json:"end"`
	Revision    int64         `json:"revision"`
	RecordedAt  string        `json:"recorded_at"`
}

type OeeCountV1 struct {
	ContributionID string `json:"contribution_id"`
	SourceKey      string `json:"source_key"`
	ProductID      string `json:"product_id"`
	RoutingID      string `json:"routing_id,omitempty"`
	Start          string `json:"start"`
	End            string `json:"end"`
	Total          string `json:"total"`
	Conforming     string `json:"conforming,omitempty"`
	Scrap          string `json:"scrap,omitempty"`
	Rework         string `json:"rework,omitempty"`
	Unit           string `json:"unit"`
}

type OeeIdealCycleV1 struct {
	SourceKey      string `json:"source_key"`
	ProductID      string `json:"product_id"`
	RoutingID      string `json:"routing_id,omitempty"`
	Revision       int64  `json:"revision"`
	SecondsPerUnit string `json:"seconds_per_unit"`
	Unit           string `json:"unit"`
	EffectiveStart string `json:"effective_start,omitempty"`
	EffectiveEnd   string `json:"effective_end,omitempty"`
}

func parseWireTime(s string) (time.Time, error) {
	if !explicitOffsetTime(s) {
		return time.Time{}, fmt.Errorf("%w: time %q must be RFC 3339 with an explicit offset", ErrInvalidRequest, s)
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), nil
}

func parseWireInterval(start, end string) (HalfOpenInterval, error) {
	s, err := parseWireTime(start)
	if err != nil {
		return HalfOpenInterval{}, err
	}
	e, err := parseWireTime(end)
	if err != nil {
		return HalfOpenInterval{}, err
	}
	iv, err := NewHalfOpenInterval(s, e)
	if err != nil {
		return HalfOpenInterval{}, fmt.Errorf("%w: interval %s..%s", ErrInvalidRequest, start, end)
	}
	return iv, nil
}

func wireDecimal(s, field string, optional bool) (*big.Rat, error) {
	if s == "" && optional {
		return nil, nil
	}
	r, ok := costDecimal(s)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a canonical decimal", ErrInvalidRequest, field)
	}
	return r, nil
}

// Policies converts a policy body into the timeline and KPI policies the kernel runs,
// validating every rule. The KPI policy digest is computed from its content.
func (b OeePolicyBodyV1) Policies(policyID uuid.UUID) (TimelinePolicy, KPIPolicy, error) {
	bad := func(f string, a ...any) (TimelinePolicy, KPIPolicy, error) {
		return TimelinePolicy{}, KPIPolicy{}, fmt.Errorf("%w: "+f, append([]any{ErrInvalidPolicy}, a...)...)
	}
	if b.PolicyVersion == "" || len(b.PolicyVersion) > 64 || len(b.PrecedenceOrder) == 0 || len(b.PrecedenceOrder) > 16 {
		return bad("policy_version and precedence_order are required")
	}
	seen := map[TimelineState]bool{}
	for _, s := range b.PrecedenceOrder {
		if !isBucketedState(s) || seen[s] {
			return bad("precedence_order has an unknown or repeated state %q", s)
		}
		seen[s] = true
	}
	minCoverage, ok := costDecimal(b.MinCoverage)
	if !ok {
		return bad("min_coverage must be a canonical decimal")
	}
	microstop, ok := costDecimal(b.MicrostopThresholdSeconds)
	if !ok {
		return bad("microstop_threshold_seconds must be a canonical decimal")
	}
	if b.MaxSourceSilenceSeconds < 0 {
		return bad("max_source_silence_seconds must not be negative")
	}
	buckets := map[TimelineState]StateBucket{}
	for _, sb := range b.StateBuckets {
		if _, dup := buckets[sb.State]; dup {
			return bad("state_buckets repeats %q", sb.State)
		}
		buckets[sb.State] = sb.Bucket
	}
	tl := TimelinePolicy{PrecedenceOrder: append([]TimelineState(nil), b.PrecedenceOrder...), MinCoverage: minCoverage, MaxSourceSilence: time.Duration(b.MaxSourceSilenceSeconds) * time.Second}
	if err := checkTimelineInput(HalfOpenInterval{Start: time.Unix(0, 0), End: time.Unix(1, 0)}, 0, tl); err != nil {
		return TimelinePolicy{}, KPIPolicy{}, err
	}
	kpi, err := normalizeKPIPolicy(KPIPolicy{
		PolicyRevisionID: policyID, PolicyVersion: b.PolicyVersion, RoundingScale: b.RoundingScale, RoundingMode: b.RoundingMode,
		ZeroDenominatorRule: b.ZeroDenominatorRule, ReworkCountsAsGood: b.ReworkCountsAsGood, PerformanceCapRule: b.PerformanceCapRule,
		MicrostopThresholdSec: microstop, StateBuckets: buckets,
	})
	if err != nil {
		return TimelinePolicy{}, KPIPolicy{}, err
	}
	if b.RoundingMode == "" || b.ZeroDenominatorRule == "" || b.PerformanceCapRule == "" {
		return bad("rounding_mode, zero_denominator_rule and performance_cap_rule are required")
	}
	return tl, kpi, nil
}

// Canonical returns the policy body in canonical order (state buckets sorted) and its
// digest within a tenant and site.
func (b OeePolicyBodyV1) Canonical(tenantID, siteID string) (OeePolicyBodyV1, string, error) {
	out := b
	out.StateBuckets = append([]StateBucketV1(nil), b.StateBuckets...)
	sort.Slice(out.StateBuckets, func(i, j int) bool { return out.StateBuckets[i].State < out.StateBuckets[j].State })
	_, digest, err := canonical.Envelope("OeePolicyBodyV1", ServiceSchemaVersion, tenantID, siteID, out)
	return out, digest, err
}

// Source consistency modes (ADR 0002 Sec. 6.2, oee_run_source).
const (
	ConsistencySnapshot      = "SNAPSHOT"
	ConsistencyWatermarked   = "WATERMARKED"
	ConsistencyNonReplayable = "NON_REPLAYABLE"
)

// DatasetSchemaID identifies the OeeDatasetV1 schema a dataset declares.
const DatasetSchemaID = "oee.wisedo.io/v1/dataset"

// MaxDatasetSources bounds the declared sources of one dataset.
const MaxDatasetSources = 32

// OeeSourceV1 is one tenant-independent source identity with its owner and revision, and
// the snapshot or watermark the facts were read at (ADR 0002 Sec. 4.2).
type OeeSourceV1 struct {
	SourceKey       string `json:"source_key" contract:"max=128"`
	OwnerService    string `json:"owner_service" contract:"max=128"`
	SourceRevision  string `json:"source_revision" contract:"max=128"`
	ConsistencyMode string `json:"consistency_mode"`
	SnapshotRef     string `json:"snapshot_ref,omitempty" contract:"max=256"`
	WatermarkRef    string `json:"watermark_ref,omitempty" contract:"max=256"`
}

// OeeDatasetV1 is the bounded, immutable dataset a calculation request carries (ADR 0002
// Sec. 4.2). It is assembled outside this service; the service verifies it and never stores
// its facts. ProvenanceDigest is the canonical SHA-256 of the dataset with that field empty.
type OeeDatasetV1 struct {
	DatasetID        string            `json:"dataset_id" contract:"max=36"`
	SchemaID         string            `json:"schema_id" contract:"max=64"`
	AssetID          string            `json:"asset_id" contract:"max=36"`
	WindowStart      string            `json:"window_start" contract:"datetime"`
	WindowEnd        string            `json:"window_end" contract:"datetime"`
	Sources          []OeeSourceV1     `json:"sources"`
	Facts            []OeeFactV1       `json:"facts"`
	Exclusions       []OeeExclusionV1  `json:"exclusions"`
	Counts           []OeeCountV1      `json:"counts"`
	IdealCycles      []OeeIdealCycleV1 `json:"ideal_cycles"`
	ProvenanceDigest string            `json:"provenance_digest" contract:"sha256"`
}

// OeeCalculationRequestV1 is the POST body: the dataset and an optional expected policy
// revision, used only as a concurrency precondition (ADR 0002 Sec. 4.3, 8). It carries no
// tenant, policy choice, URL, credential or formula.
type OeeCalculationRequestV1 struct {
	Dataset                OeeDatasetV1 `json:"dataset"`
	ExpectedPolicyRevision int64        `json:"expected_policy_revision,omitempty"`
}

type OeePolicyRefV1 struct {
	PolicyID         string `json:"policy_id" contract:"max=36"`
	PolicyRevisionID string `json:"policy_revision_id" contract:"max=36"`
	Revision         int64  `json:"revision"`
	Digest           string `json:"digest" contract:"sha256"`
}

type OeeTimelineQualityV1 struct {
	CoverageRatio string       `json:"coverage_ratio"`
	GapSeconds    string       `json:"gap_seconds"`
	Status        Status       `json:"status"`
	ReasonCodes   []ReasonCode `json:"reason_codes"`
}

// OeeRunSourceV1 is the minimized provenance kept per source (ADR 0002 oee_run_source): the
// source identity, its snapshot or watermark, a digest of its contribution and a fact count.
// The facts themselves are not kept.
type OeeRunSourceV1 struct {
	SourceKey          string `json:"source_key"`
	OwnerService       string `json:"owner_service"`
	SourceRevision     string `json:"source_revision"`
	ConsistencyMode    string `json:"consistency_mode"`
	SnapshotRef        string `json:"snapshot_ref,omitempty"`
	WatermarkRef       string `json:"watermark_ref,omitempty"`
	ContributionSHA256 string `json:"contribution_sha256" contract:"sha256"`
	FactCount          int    `json:"fact_count"`
}

type OeeEvidenceDigestsV1 struct {
	Dataset     string `json:"dataset" contract:"sha256"`
	Timeline    string `json:"timeline" contract:"sha256"`
	Calculation string `json:"calculation" contract:"sha256"`
}

// OeeCalculationEvidenceV1 is the sealed, candidate-only evidence of one calculation run.
// Numeric KPIs appear only as the kernel publishes them for the result status; a NOT_READY
// result carries reason codes and no finalized number (ADR 0002 Sec. 9).
type OeeCalculationEvidenceV1 struct {
	SchemaVersion  int                  `json:"schema_version"`
	EvidenceID     string               `json:"evidence_id"`
	RunID          string               `json:"run_id"`
	DatasetID      string               `json:"dataset_id"`
	AssetID        string               `json:"asset_id"`
	WindowStart    string               `json:"window_start"`
	WindowEnd      string               `json:"window_end"`
	Policy         OeePolicyRefV1       `json:"policy"`
	TruthState     string               `json:"truth_state"`
	Status         Status               `json:"status"`
	ReasonCodes    []ReasonCode         `json:"reason_codes"`
	KPIs           PackKPIs             `json:"kpis"`
	Durations      PackDurations        `json:"durations"`
	Timeline       OeeTimelineQualityV1 `json:"timeline"`
	Sources        []OeeRunSourceV1     `json:"sources"`
	FormulaVersion string               `json:"formula_version"`
	KernelVersion  string               `json:"kernel_version"`
	Digests        OeeEvidenceDigestsV1 `json:"digests"`
	SealedAt       string               `json:"sealed_at"`
}

// TruthStateCandidate is the only truth state this service writes: evidence is candidate
// only, and acceptance belongs to the MES-owned authority router (WIS-231).
const TruthStateCandidate = "CANDIDATE"

// EvidenceDigest is the canonical digest of sealed evidence (rule 04): SHA-256 over the
// schema-ordered body plus schema version and tenant/site. The generated evidence and run IDs
// and the seal time are excluded, so identical inputs under the same policy and code version
// produce the same digest whenever they are sealed, and identical bodies in two tenants never
// share one.
func EvidenceDigest(tenantID, siteID string, ev OeeCalculationEvidenceV1) (string, error) {
	ev.EvidenceID, ev.RunID, ev.SealedAt = "", "", ""
	_, digest, err := canonical.Envelope("OeeCalculationEvidenceV1", ev.SchemaVersion, tenantID, siteID, ev)
	return digest, err
}

// DatasetProvenanceDigest recomputes a dataset's canonical provenance digest.
func DatasetProvenanceDigest(d OeeDatasetV1) (string, error) {
	d.ProvenanceDigest = ""
	return CanonicalSHA256Hex(d)
}

func boundedText(s string, max int) bool {
	return s != "" && len(s) <= max && strings.TrimSpace(s) == s && utf8.ValidString(s)
}

// optionalBoundedText accepts an empty value or one that satisfies boundedText.
func optionalBoundedText(s string, max int) bool {
	return s == "" || boundedText(s, max)
}

// maxWireIDLength bounds every identifier and unit string a dataset carries (ADR 0002
// Sec. 16: every string has a maximum length).
const maxWireIDLength = 128

// preparedDataset is a dataset that passed full contract verification, converted to the
// kernel's inputs. Producing it is the only way to obtain kernel inputs from wire facts, so
// acceptance and calculation can never disagree about what is valid.
type preparedDataset struct {
	window     HalfOpenInterval
	facts      []OwnerFactEvent
	exclusions CalendarExclusionPolicy
	counts     []ProductCountContribution
	cycles     []IdealCycleRevision
}

// VerifyDataset is pipeline stage 2 (contract verification, ADR 0002 Sec. 5): bounds,
// identities, declared sources, every fact's times, digests, units and decimals, and the
// canonical provenance digest. It runs before a run is reserved, so an invalid dataset is
// rejected with no side effects (ADR 0002 Sec. 9).
func VerifyDataset(d OeeDatasetV1) error {
	_, err := verifyAndPrepare(d)
	return err
}

// verifyAndPrepare verifies the dataset and its provenance digest and returns kernel inputs.
func verifyAndPrepare(d OeeDatasetV1) (preparedDataset, error) {
	p, err := prepareDataset(d)
	if err != nil {
		return preparedDataset{}, err
	}
	want, err := DatasetProvenanceDigest(d)
	if err != nil {
		return preparedDataset{}, err
	}
	if d.ProvenanceDigest != want {
		return preparedDataset{}, fmt.Errorf("%w: provenance_digest does not match the dataset", ErrInvalidRequest)
	}
	return p, nil
}

// prepareDataset checks the envelope (bounds, identities, declared sources) and converts
// every element, failing on the first violation. Limits are checked before any per-element
// work so an oversized dataset costs nothing to reject.
func prepareDataset(d OeeDatasetV1) (preparedDataset, error) {
	bad := func(f string, a ...any) (preparedDataset, error) {
		return preparedDataset{}, fmt.Errorf("%w: "+f, append([]any{ErrInvalidRequest}, a...)...)
	}
	// The timeline resolves facts and exclusions together, so both count toward its limit.
	if len(d.Facts) > MaxRequestFacts || len(d.Exclusions) > MaxRequestExclusions || len(d.Facts)+len(d.Exclusions) > MaxTimelineFacts ||
		len(d.Counts) > MaxRequestCounts || len(d.IdealCycles) > MaxRequestCycles {
		return preparedDataset{}, fmt.Errorf("%w: dataset exceeds its fact, exclusion, count or cycle limit", ErrTimelineLimit)
	}
	if !canonicalCostID(d.DatasetID) || d.SchemaID != DatasetSchemaID || !canonicalCostID(d.AssetID) {
		return bad("dataset_id, schema_id and asset_id")
	}
	if len(d.Sources) == 0 || len(d.Sources) > MaxDatasetSources {
		return bad("1 to %d sources are required", MaxDatasetSources)
	}
	window, err := parseWireInterval(d.WindowStart, d.WindowEnd)
	if err != nil {
		return preparedDataset{}, err
	}
	if window.Duration() > MaxTimelineWindow {
		return preparedDataset{}, fmt.Errorf("%w: window %s exceeds %s", ErrTimelineLimit, window.Duration(), MaxTimelineWindow)
	}
	declared := make(map[string]bool, len(d.Sources))
	for _, s := range d.Sources {
		if !boundedText(s.SourceKey, maxWireIDLength) || !boundedText(s.OwnerService, maxWireIDLength) || !boundedText(s.SourceRevision, maxWireIDLength) || declared[s.SourceKey] {
			return bad("source %q needs a unique key, owner service and revision", s.SourceKey)
		}
		// The timeline labels facts synthesized from calendar exclusions with this key; a
		// declared source must not impersonate it (it would escape staleness checks).
		if s.SourceKey == calendarSource {
			return bad("source key %q is reserved", s.SourceKey)
		}
		switch s.ConsistencyMode {
		case ConsistencySnapshot, ConsistencyWatermarked:
			if s.SnapshotRef == "" && s.WatermarkRef == "" {
				return bad("source %q in mode %s needs a snapshot_ref or watermark_ref", s.SourceKey, s.ConsistencyMode)
			}
		case ConsistencyNonReplayable:
		default:
			return bad("source %q consistency_mode %q", s.SourceKey, s.ConsistencyMode)
		}
		if !optionalBoundedText(s.SnapshotRef, 256) || !optionalBoundedText(s.WatermarkRef, 256) {
			return bad("source %q snapshot_ref and watermark_ref must be bounded trimmed text", s.SourceKey)
		}
		declared[s.SourceKey] = true
	}

	p := preparedDataset{window: window, facts: make([]OwnerFactEvent, 0, len(d.Facts)), counts: make([]ProductCountContribution, 0, len(d.Counts)), cycles: make([]IdealCycleRevision, 0, len(d.IdealCycles))}
	for _, f := range d.Facts {
		if !declared[f.SourceKey] {
			return bad("fact %q cites undeclared source %q", f.EventID, f.SourceKey)
		}
		if !boundedText(f.EventID, maxWireIDLength) || !evidenceSHA256.MatchString(f.PayloadDigest) {
			return bad("fact %q needs a bounded event_id and a sha256 payload_digest", f.EventID)
		}
		// The same rules DeduplicateOwnerFacts applies, checked here so they reject at acceptance.
		if !inputStates[f.State] || f.Revision < 1 {
			return bad("fact %q needs a known input state and revision >= 1", f.EventID)
		}
		iv, err := parseWireInterval(f.Start, f.End)
		if err != nil {
			return preparedDataset{}, err
		}
		eventAt, err := parseWireTime(f.EventAt)
		if err != nil {
			return preparedDataset{}, err
		}
		recordedAt, err := parseWireTime(f.RecordedAt)
		if err != nil {
			return preparedDataset{}, err
		}
		p.facts = append(p.facts, OwnerFactEvent{EventID: f.EventID, SourceKey: f.SourceKey, State: f.State, Interval: iv, EventAt: eventAt, RecordedAt: recordedAt, Revision: f.Revision, PayloadDigest: f.PayloadDigest})
	}
	for _, x := range d.Exclusions {
		if !boundedText(x.ExclusionID, maxWireIDLength) || x.Revision < 1 {
			return bad("exclusion %q needs a bounded exclusion_id and revision >= 1", x.ExclusionID)
		}
		iv, err := parseWireInterval(x.Start, x.End)
		if err != nil {
			return preparedDataset{}, err
		}
		recordedAt, err := parseWireTime(x.RecordedAt)
		if err != nil {
			return preparedDataset{}, err
		}
		ex := CalendarExclusion{ExclusionID: x.ExclusionID, State: x.State, Interval: iv, Revision: x.Revision, RecordedAt: recordedAt}
		switch x.State {
		case StatePlannedBreak:
			p.exclusions.PlannedBreaks = append(p.exclusions.PlannedBreaks, ex)
		case StateChangeover:
			p.exclusions.Changeovers = append(p.exclusions.Changeovers, ex)
		case StateShutdownExclusion:
			p.exclusions.ShutdownExclusions = append(p.exclusions.ShutdownExclusions, ex)
		default:
			return bad("exclusion %q has state %q", x.ExclusionID, x.State)
		}
	}
	contributions := make(map[string]bool, len(d.Counts))
	for _, c := range d.Counts {
		if !declared[c.SourceKey] {
			return bad("count %q cites undeclared source %q", c.ContributionID, c.SourceKey)
		}
		if !boundedText(c.ContributionID, maxWireIDLength) || !boundedText(c.ProductID, maxWireIDLength) || !boundedText(c.Unit, maxWireIDLength) || !optionalBoundedText(c.RoutingID, maxWireIDLength) {
			return bad("count %q needs bounded contribution_id, product_id, unit and routing_id", c.ContributionID)
		}
		if contributions[c.ContributionID] {
			return bad("count contribution_id %q is repeated", c.ContributionID)
		}
		contributions[c.ContributionID] = true
		iv, err := parseWireInterval(c.Start, c.End)
		if err != nil {
			return preparedDataset{}, err
		}
		if iv.Start.Before(window.Start) || iv.End.After(window.End) {
			return bad("count %q lies outside the dataset window", c.ContributionID)
		}
		pc := ProductCountContribution{ContributionID: c.ContributionID, ProductID: c.ProductID, RoutingID: c.RoutingID, Interval: iv, Unit: UnitCode(c.Unit)}
		if pc.TotalCount, err = wireDecimal(c.Total, "total", false); err != nil {
			return preparedDataset{}, err
		}
		if pc.ConformingCount, err = wireDecimal(c.Conforming, "conforming", true); err != nil {
			return preparedDataset{}, err
		}
		if pc.ScrapCount, err = wireDecimal(c.Scrap, "scrap", true); err != nil {
			return preparedDataset{}, err
		}
		if pc.ReworkCount, err = wireDecimal(c.Rework, "rework", true); err != nil {
			return preparedDataset{}, err
		}
		p.counts = append(p.counts, pc)
	}
	for _, ic := range d.IdealCycles {
		if !declared[ic.SourceKey] {
			return bad("ideal cycle %q cites undeclared source %q", ic.ProductID, ic.SourceKey)
		}
		if !boundedText(ic.ProductID, maxWireIDLength) || !boundedText(ic.Unit, maxWireIDLength) || !optionalBoundedText(ic.RoutingID, maxWireIDLength) {
			return bad("ideal cycle %q needs bounded product_id, unit and routing_id", ic.ProductID)
		}
		spu, err := wireDecimal(ic.SecondsPerUnit, "seconds_per_unit", false)
		if err != nil {
			return preparedDataset{}, err
		}
		rev := IdealCycleRevision{ProductID: ic.ProductID, RoutingID: ic.RoutingID, Revision: ic.Revision, SecondsPerUnit: spu, Unit: UnitCode(ic.Unit)}
		if ic.EffectiveStart != "" || ic.EffectiveEnd != "" {
			if rev.EffectiveWindow, err = parseWireInterval(ic.EffectiveStart, ic.EffectiveEnd); err != nil {
				return preparedDataset{}, err
			}
		}
		p.cycles = append(p.cycles, rev)
	}
	return p, nil
}

// runSources computes the minimized per-source provenance in one pass over the dataset: a
// canonical digest of each source's contribution (the facts, counts and cycles it supplied,
// in dataset order) and its element count. Facts themselves are not kept (ADR 0002 Sec. 4.2).
func runSources(d OeeDatasetV1) ([]OeeRunSourceV1, error) {
	type contribution struct {
		Facts       []OeeFactV1       `json:"facts"`
		Counts      []OeeCountV1      `json:"counts"`
		IdealCycles []OeeIdealCycleV1 `json:"ideal_cycles"`
	}
	bySource := make(map[string]*contribution, len(d.Sources))
	for _, s := range d.Sources {
		bySource[s.SourceKey] = &contribution{}
	}
	source := func(key string) (*contribution, error) {
		if c, ok := bySource[key]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("%w: element cites undeclared source %q", ErrInvalidRequest, key)
	}
	for _, f := range d.Facts {
		c, err := source(f.SourceKey)
		if err != nil {
			return nil, err
		}
		c.Facts = append(c.Facts, f)
	}
	for _, ct := range d.Counts {
		c, err := source(ct.SourceKey)
		if err != nil {
			return nil, err
		}
		c.Counts = append(c.Counts, ct)
	}
	for _, ic := range d.IdealCycles {
		c, err := source(ic.SourceKey)
		if err != nil {
			return nil, err
		}
		c.IdealCycles = append(c.IdealCycles, ic)
	}
	out := make([]OeeRunSourceV1, 0, len(d.Sources))
	for _, s := range d.Sources {
		c := bySource[s.SourceKey]
		digest, err := CanonicalSHA256Hex(c)
		if err != nil {
			return nil, err
		}
		out = append(out, OeeRunSourceV1{
			SourceKey: s.SourceKey, OwnerService: s.OwnerService, SourceRevision: s.SourceRevision, ConsistencyMode: s.ConsistencyMode,
			SnapshotRef: s.SnapshotRef, WatermarkRef: s.WatermarkRef, ContributionSHA256: digest, FactCount: len(c.Facts) + len(c.Counts) + len(c.IdealCycles),
		})
	}
	return out, nil
}

// CalculateDataset runs pipeline stages 4 to 6 and 8 (ADR 0002 Sec. 5) on a dataset under the
// selected published policy. It re-verifies the dataset itself (including its provenance
// digest), so it is safe to call with any input; the service pays one extra digest per run for
// that guarantee. It is pure: the caller reserves the run, supplies the identifiers and seal time, and
// seals the result.
func CalculateDataset(tenantID, siteID string, d OeeDatasetV1, ref OeePolicyRefV1, body OeePolicyBodyV1) (OeeCalculationEvidenceV1, error) {
	p, err := verifyAndPrepare(d)
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	revisionID, err := uuid.Parse(ref.PolicyRevisionID)
	if err != nil {
		return OeeCalculationEvidenceV1{}, fmt.Errorf("%w: policy_revision_id", ErrInvalidPolicy)
	}
	tlPolicy, kpiPolicy, err := body.Policies(revisionID)
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	window, facts, exclusions, counts, cycles := p.window, p.facts, p.exclusions, p.counts, p.cycles

	timeline, err := ResolveTimelineWithExclusions(window, facts, exclusions, tlPolicy)
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	timelineDigest, err := TimelineDigest(tenantID, siteID, timeline, tlPolicy)
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	// An unresolvable timeline has no segments; the kernel then sees no time and fails closed
	// with the timeline's reasons.
	kpiTimeline := timeline
	if len(kpiTimeline.Segments) == 0 {
		kpiTimeline.Window = window
	}
	kpi, err := CalculateKPIs(KPICalculationInput{Window: window, Timeline: kpiTimeline, CountContributions: counts, IdealCycles: cycles, Policy: kpiPolicy})
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	sources, err := runSources(d)
	if err != nil {
		return OeeCalculationEvidenceV1{}, err
	}
	utc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	return OeeCalculationEvidenceV1{
		SchemaVersion: ServiceSchemaVersion, DatasetID: d.DatasetID, AssetID: d.AssetID,
		WindowStart: utc(window.Start), WindowEnd: utc(window.End), Policy: ref, TruthState: TruthStateCandidate,
		Status: kpi.Readiness.Status, ReasonCodes: append([]ReasonCode{}, kpi.Readiness.ReasonCodes...),
		KPIs: PackKPIs{packRatio(kpi.Availability), packRatio(kpi.Performance), packRatio(kpi.Quality), packRatio(kpi.OEE), packRatio(kpi.Loading), packRatio(kpi.TEEP)},
		Durations: PackDurations{
			Calendar: packRat(kpi.TotalCalendarSeconds), PlannedExclusion: packRat(kpi.PlannedExclusionSeconds), PlannedProduction: packRat(kpi.PlannedProductionSeconds),
			Run: packRat(kpi.RunSeconds), Stop: packRat(kpi.StopSeconds), Microstop: packRat(kpi.MicrostopSeconds), Gap: packRat(kpi.GapSeconds),
		},
		Timeline:       OeeTimelineQualityV1{CoverageRatio: packRat(timeline.CoverageRatio), GapSeconds: packRat(timeline.GapSeconds), Status: timeline.Readiness.Status, ReasonCodes: append([]ReasonCode{}, timeline.Readiness.ReasonCodes...)},
		Sources:        sources,
		FormulaVersion: kpi.FormulaVersion, KernelVersion: kpi.KernelVersion,
		Digests: OeeEvidenceDigestsV1{Dataset: d.ProvenanceDigest, Timeline: timelineDigest, Calculation: kpi.CalculationDigest},
	}, nil
}

// OeePolicyRevisionInputV1 is the POST policies/{policyId}/revisions body: an immutable
// policy document and its effective range (ADR 0002 Sec. 6.2 oee_policy_revision). The first
// revision of a policy also fixes its policy_code.
type OeePolicyRevisionInputV1 struct {
	PolicyCode     string          `json:"policy_code" contract:"max=128"`
	EffectiveStart string          `json:"effective_start" contract:"datetime"`
	EffectiveEnd   string          `json:"effective_end,omitempty" contract:"datetime"`
	Policy         OeePolicyBodyV1 `json:"policy"`
}

type OeePolicyRevisionV1 struct {
	PolicyRevisionID string          `json:"policy_revision_id"`
	RevisionNo       int64           `json:"revision_no"`
	EffectiveStart   string          `json:"effective_start"`
	EffectiveEnd     string          `json:"effective_end,omitempty"`
	Policy           OeePolicyBodyV1 `json:"policy"`
	Digest           string          `json:"digest"`
	CreatedAt        string          `json:"created_at"`
}

type OeePolicyPublicationV1 struct {
	PublicationID    string `json:"publication_id"`
	PublicationNo    int64  `json:"publication_no"`
	Action           string `json:"action"`
	PolicyRevisionID string `json:"policy_revision_id"`
	PublishedAt      string `json:"published_at"`
}

// OeePolicyV1 is a policy with its revisions and current publication. RowVersion is the
// If-Match token for publication.
type OeePolicyV1 struct {
	PolicyID           string                  `json:"policy_id"`
	PolicyCode         string                  `json:"policy_code"`
	RowVersion         int64                   `json:"row_version"`
	CurrentPublication *OeePolicyPublicationV1 `json:"current_publication"`
	Revisions          []OeePolicyRevisionV1   `json:"revisions"`
	Replayed           bool                    `json:"replayed"`
}

// Calculation run lifecycle states (ADR 0002 Sec. 18).
const (
	RunAccepted  = "ACCEPTED"
	RunRunning   = "RUNNING"
	RunSucceeded = "SUCCEEDED"
	RunFailed    = "FAILED"
	RunCancelled = "CANCELLED"
)

// OeeCalculationRunV1 is a calculation run. Transport success does not imply readiness:
// ResultStatus is set only once the run SUCCEEDED, and the evidence holds the numbers.
type OeeCalculationRunV1 struct {
	RunID            string         `json:"run_id"`
	DatasetID        string         `json:"dataset_id"`
	AssetID          string         `json:"asset_id"`
	WindowStart      string         `json:"window_start"`
	WindowEnd        string         `json:"window_end"`
	Policy           OeePolicyRefV1 `json:"policy"`
	Status           string         `json:"status"`
	ResultStatus     Status         `json:"result_status,omitempty"`
	TruthState       string         `json:"truth_state,omitempty"`
	FailureCode      string         `json:"failure_code,omitempty"`
	EvidenceID       string         `json:"evidence_id,omitempty"`
	AcceptedAt       string         `json:"accepted_at"`
	DeadlineAt       string         `json:"deadline_at"`
	StartedAt        string         `json:"started_at,omitempty"`
	FinishedAt       string         `json:"finished_at,omitempty"`
	CancelGeneration int64          `json:"cancel_generation"`
	RowVersion       int64          `json:"row_version"`
	Replayed         bool           `json:"replayed"`
}

// Request body limits, enforced before decoding (ADR 0002 Sec. 15.2). A calculation request
// must fit MaxTimelineFacts facts; a policy revision fits its 64 KiB stored document.
const (
	MaxDatasetRequestBytes        = 4 << 20
	MaxPolicyRevisionRequestBytes = 64 << 10
)
