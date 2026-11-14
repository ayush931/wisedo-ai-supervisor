package factorysim

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// These fictional service assumptions match the existing interval observations.
const (
	ChangeoverMinutes    int64 = 5
	ServiceIntervalHours int64 = 8
	LubricationMinutes   int64 = 10
	WearMicrostopSeconds int64 = 720
	WearRunSeconds       int64 = 180
)

type Alarm struct {
	Code    string `json:"code"`
	Meaning string `json:"meaning"`
}
type CycleVariant struct {
	SKU     string `json:"sku"`
	Seconds int64  `json:"cycle_seconds"`
}
type MachineParameters struct {
	StationID                string         `json:"station_id"`
	Name                     string         `json:"name"`
	LineID                   string         `json:"line_id"`
	RatedCycleSeconds        int64          `json:"rated_cycle_seconds"`
	SKUCycles                []CycleVariant `json:"sku_cycles"`
	ChangeoverMinutes        int64          `json:"changeover_minutes"`
	MaintenanceIntervalHours int64          `json:"maintenance_interval_hours"`
	LubricationMinutes       int64          `json:"lubrication_minutes"`
	MTBFHours                int64          `json:"mtbf_hours"`
	RepairMinutes            int64          `json:"repair_minutes"`
}
type MachineSheet struct {
	SchemaVersion int                 `json:"schema_version"`
	Synthetic     bool                `json:"synthetic"`
	Class         string              `json:"class"`
	TimeBasis     string              `json:"time_basis"`
	Stations      []MachineParameters `json:"stations"`
	Alarms        []Alarm             `json:"alarms"`
}
type Tool struct {
	ID           string `json:"id"`
	StationID    string `json:"station_id"`
	SKU          string `json:"sku"`
	Kind         string `json:"kind"`
	LifeParts    int64  `json:"life_parts"`
	InitialParts int64  `json:"initial_parts"`
	UsedParts    int64  `json:"used_parts"`
}
type Gauge struct {
	ID                      string    `json:"id"`
	StationID               string    `json:"station_id"`
	CalibrationIntervalDays int       `json:"calibration_interval_days"`
	CalibratedAt            time.Time `json:"calibrated_at"`
	DueAt                   time.Time `json:"due_at"`
}

func (g Gauge) Readiness(at time.Time) (string, string) {
	if !at.Before(g.DueAt) {
		return "NOT_READY", "CALIBRATION_OVERDUE"
	}
	return "READY", "CALIBRATION_CURRENT"
}

type ToolEvent struct {
	ToolID string    `json:"tool_id"`
	At     time.Time `json:"model_at"`
	Kind   string    `json:"kind"`
	Before int64     `json:"before_parts"`
	After  int64     `json:"after_parts"`
}
type ToolRegister struct {
	SchemaVersion int         `json:"schema_version"`
	Synthetic     bool        `json:"synthetic"`
	Tools         []Tool      `json:"tools"`
	Gauges        []Gauge     `json:"gauges"`
	Events        []ToolEvent `json:"events"`
}

func MachineSheets(m Manifest) []MachineSheet {
	classes := map[string]*MachineSheet{}
	for li, line := range m.Lines {
		for _, s := range line.Stations {
			sheet := classes[s.Class]
			if sheet == nil {
				sheet = &MachineSheet{SchemaVersion: 1, Synthetic: true, Class: s.Class, TimeBasis: "simulated seconds and hours; unscaled fictional ratings", Stations: []MachineParameters{}, Alarms: []Alarm{{"DRIVE_TRIP_04", "Drive trip: authorized maintenance inspection required"}, {"TOOL_WEAR_17", "Tool life reached: inspect and replace under authorized procedure"}, {"NO_MATERIAL", "Material shortage; not an equipment fault"}}}
				classes[s.Class] = sheet
			}
			p := MachineParameters{StationID: s.ID, Name: s.Name, LineID: line.ID, RatedCycleSeconds: s.CycleSeconds, ChangeoverMinutes: ChangeoverMinutes, MaintenanceIntervalHours: ServiceIntervalHours, LubricationMinutes: LubricationMinutes, MTBFHours: s.MTBFHours, RepairMinutes: s.MTTRMinutes, SKUCycles: []CycleVariant{}}
			for _, sku := range m.SKUs {
				if sku.Line == li {
					p.SKUCycles = append(p.SKUCycles, CycleVariant{sku.Code, s.CycleSeconds * sku.CycleSeconds / line.CycleSeconds})
				}
			}
			sheet.Stations = append(sheet.Stations, p)
		}
	}
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Strings(names)
	sheets := make([]MachineSheet, 0, len(names))
	for _, name := range names {
		sheets = append(sheets, *classes[name])
	}
	return sheets
}

func NewTools(m Manifest, start time.Time) ToolRegister {
	r := ToolRegister{SchemaVersion: 1, Synthetic: true, Tools: []Tool{}, Gauges: []Gauge{}, Events: []ToolEvent{}}
	for li, line := range m.Lines {
		for _, s := range line.Stations {
			kind := "fixture"
			switch {
			case strings.Contains(s.Class, "press"):
				kind = "die"
			case s.Class == "turning" || s.Class == "VMC" || s.Class == "HMC" || s.Class == "CNC" || s.Class == "grinding":
				kind = "cutting_tool"
			}
			for _, sku := range m.SKUs {
				if sku.Line == li {
					r.Tools = append(r.Tools, Tool{ID: ID(m.Seed, "tool/"+s.ID+"/"+sku.Code), StationID: s.ID, SKU: sku.Code, Kind: kind, LifeParts: sku.ToolLife})
				}
			}
			if s.Class == "CMM" || s.Class == "inspection" {
				r.Gauges = append(r.Gauges, Gauge{ID: ID(m.Seed, "gauge/"+s.ID), StationID: s.ID, CalibrationIntervalDays: 30, CalibratedAt: start.AddDate(0, 0, -23), DueAt: start.AddDate(0, 0, 7)})
			}
		}
	}
	return r
}

func (r *ToolRegister) Tool(station, sku string) (*Tool, error) {
	for i := range r.Tools {
		if r.Tools[i].StationID == station && r.Tools[i].SKU == sku {
			return &r.Tools[i], nil
		}
	}
	return nil, fmt.Errorf("no tool for station/SKU %s/%s", station, sku)
}
func (r *ToolRegister) Process(station, sku string, parts int64, at time.Time) (bool, error) {
	tool, err := r.Tool(station, sku)
	if err != nil {
		return false, err
	}
	if parts < 0 || tool.LifeParts < 1 || tool.UsedParts < 0 || parts > math.MaxInt64-tool.UsedParts {
		return false, errors.New("invalid tool counter")
	}
	before := tool.UsedParts
	tool.UsedParts += parts
	if before < tool.LifeParts && tool.UsedParts >= tool.LifeParts {
		r.Events = append(r.Events, ToolEvent{tool.ID, at, "life_reached", before, tool.UsedParts})
	}
	return tool.UsedParts >= tool.LifeParts, nil
}
func (r *ToolRegister) Replace(station, sku string, at time.Time) error {
	tool, err := r.Tool(station, sku)
	if err != nil {
		return err
	}
	r.Events = append(r.Events, ToolEvent{tool.ID, at, "replacement", tool.UsedParts, 0})
	tool.UsedParts = 0
	return nil
}

// WearObservation is driven solely by counter state, not an interval index or inherited fault.
func WearObservation(observation Observation, worn bool) Observation {
	if observation.FaultCode == "TOOL_WEAR_17" || observation.Reason == "TOOL_WEAR_17" {
		observation.FaultCode = ""
		observation.State = "running"
		observation.Reason = "normal"
		observation.RunSeconds = 792
		observation.MicrostopSeconds = 0
	}
	if worn {
		observation.FaultCode = "TOOL_WEAR_17"
		observation.State = "stopped"
		observation.Reason = "TOOL_WEAR_17"
		observation.RunSeconds = WearRunSeconds
		observation.MicrostopSeconds = WearMicrostopSeconds
	}
	return observation
}
