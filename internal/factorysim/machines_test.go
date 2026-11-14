package factorysim

import (
	"testing"
	"time"
)

func TestWIS709ToolLifeAndGaugeBoundaries(t *testing.T) {
	at := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	r := ToolRegister{Tools: []Tool{{ID: "tool", StationID: "station", SKU: "sku", LifeParts: 100, InitialParts: 98, UsedParts: 98}}}
	if worn, err := r.Process("station", "sku", 1, at); err != nil || worn {
		t.Fatal(worn, err)
	}
	if worn, err := r.Process("station", "sku", 1, at.Add(time.Minute)); err != nil || !worn {
		t.Fatal(worn, err)
	}
	if len(r.Events) != 1 || r.Events[0].Kind != "life_reached" || r.Events[0].Before != 99 || r.Events[0].After != 100 {
		t.Fatal(r.Events)
	}
	if err := r.Replace("station", "sku", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(r.Events) != 2 || r.Events[1].Before != 100 || r.Tools[0].UsedParts != 0 || r.Tools[0].InitialParts != 98 {
		t.Fatal("replacement erased history", r)
	}
	if _, err := r.Process("station", "sku", -1, at); err == nil {
		t.Fatal("negative count")
	}
	if _, err := r.Process("missing", "sku", 1, at); err == nil {
		t.Fatal("unknown tool")
	}
	g := Gauge{DueAt: at}
	if state, reason := g.Readiness(at.Add(-time.Nanosecond)); state != "READY" || reason != "CALIBRATION_CURRENT" {
		t.Fatal(state, reason)
	}
	if state, reason := g.Readiness(at); state != "NOT_READY" || reason != "CALIBRATION_OVERDUE" {
		t.Fatal(state, reason)
	}
	inherited := Observation{FaultCode: "TOOL_WEAR_17", Reason: "TOOL_WEAR_17", State: "stopped"}
	if got := WearObservation(inherited, false); got.FaultCode != "" || got.State != "running" {
		t.Fatal("wear still hardcoded", got)
	}
	if got := WearObservation(Observation{}, true); got.FaultCode != "TOOL_WEAR_17" || got.MicrostopSeconds != 720 || got.RunSeconds != 180 {
		t.Fatal(got)
	}
}
