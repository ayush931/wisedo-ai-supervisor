// Package factorysim defines the synthetic showcase dataset, not production truth.
package factorysim

import (
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
	"strconv"
)

const TenantID = "00000000-0000-4000-8000-000000000290"
const SiteID = "00000000-0000-4000-8000-000000000291"

type File struct {
	Path   string `json:"path"`
	Schema string `json:"schema"`
	SHA256 string `json:"sha256" contract:"sha256"`
}
type SKU struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Customer     string `json:"customer"`
	Line         int    `json:"line"`
	DailyUnits   int64  `json:"daily_units"`
	PriceINR     int64  `json:"price_inr"`
	Material     string `json:"material"`
	ToolLife     int64  `json:"tool_life_parts"`
	CycleSeconds int64  `json:"ideal_cycle_seconds"`
}
type Line struct {
	ID           string    `json:"id" contract:"uuid"`
	Name         string    `json:"name"`
	CycleSeconds int64     `json:"constraint_cycle_seconds"`
	Stations     []Station `json:"stations"`
}
type Station struct {
	ID           string `json:"id" contract:"uuid"`
	Name         string `json:"name"`
	Class        string `json:"class"`
	CycleSeconds int64  `json:"cycle_seconds"`
	MTBFHours    int64  `json:"mtbf_hours"`
	MTTRMinutes  int64  `json:"mttr_minutes"`
}
type Manifest struct {
	SchemaVersion          int    `json:"schema_version"`
	Synthetic              bool   `json:"synthetic"`
	Seed                   int64  `json:"seed"`
	Start                  string `json:"start_date"`
	Days                   int    `json:"days"`
	Plant                  string `json:"plant"`
	Location               string `json:"location"`
	Currency               string `json:"currency"`
	Timezone               string `json:"timezone"`
	WorkingDays            int64  `json:"working_days_per_year"`
	RevenueINR             int64  `json:"simulated_revenue_inr"`
	PlannedDailyRevenueINR int64  `json:"planned_daily_revenue_inr"`
	Events                 int64  `json:"events"`
	Lines                  []Line `json:"lines"`
	SKUs                   []SKU  `json:"skus"`
	Files                  []File `json:"files"`
}
type Scenario struct {
	Name             string   `json:"name"`
	Start            string   `json:"start"`
	End              string   `json:"end"`
	EvidenceKeys     []string `json:"evidence_keys"`
	GroundTruth      string   `json:"ground_truth"`
	ExpectedReaction string   `json:"expected_reaction"`
}
type AnswerKey struct {
	Synthetic bool       `json:"synthetic"`
	Scenarios []Scenario `json:"scenarios"`
	Gaps      []string   `json:"gaps"`
}

// Observation is a simulator-specific payload of the existing schemaless
// machine_state/operator_report kinds; it does not introduce a runtime event kind.
type Observation struct {
	Synthetic             bool   `json:"synthetic"`
	Start                 string `json:"interval_start"`
	End                   string `json:"interval_end"`
	State                 string `json:"state"`
	Reason                string `json:"reason_code"`
	FaultCode             string `json:"fault_code,omitempty"`
	Good                  int64  `json:"good_units"`
	Scrap                 int64  `json:"scrap_units"`
	Rework                int64  `json:"rework_units"`
	RunSeconds            int64  `json:"run_seconds"`
	MicrostopSeconds      int64  `json:"microstop_seconds"`
	IdealCycleSeconds     int64  `json:"ideal_cycle_seconds"`
	EffectiveCycleSeconds int64  `json:"effective_cycle_seconds"`
	WIP                   int64  `json:"upstream_wip_units"`
	Material              string `json:"material,omitempty"`
	Note                  string `json:"note"`
}

func ID(seed int64, name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("factory-sim-v1:"+strconv.FormatInt(seed, 10)+":"+name)).String()
}

type ConfigurationRows struct {
	Rows []workstation.ConfigurationCSVRow `json:"rows"`
}
