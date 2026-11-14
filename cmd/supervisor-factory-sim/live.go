package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

type liveReport struct {
	SchemaVersion  int                      `json:"schema_version"`
	Synthetic      bool                     `json:"synthetic"`
	Schedule       factorysim.Schedule      `json:"schedule"`
	ManifestSHA256 string                   `json:"manifest_sha256"`
	Approvals      string                   `json:"approvals"`
	Passed         bool                     `json:"passed"`
	Failure        string                   `json:"failure,omitempty"`
	Batches        []ingestion.Result       `json:"batches"`
	Runs           []workstation.ShiftRunV1 `json:"shift_runs"`
	StationRecords int                      `json:"station_records"`
	Bindings       map[string]string        `json:"bindings"`
	Liveness       *ingestion.Liveness      `json:"liveness,omitempty"`
}

func facilityURL(raw string) error {
	base, err := localURL(raw)
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 62000 || port > 62999 {
		return errors.New("facility requires its own literal loopback port in 62000..62999")
	}
	return nil
}

// Session files are renewed by real browser logins; they never contain authority overrides.
func sessionCookies(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 4096 {
		return nil, errors.New("oversized facility session file")
	}
	var cookies map[string]string
	if err = readJSON(b, &cookies); err != nil {
		return nil, err
	}
	return cookies, nil
}
func refreshSessions(ctx context.Context, l *loader, path string) error {
	if path == "" {
		return nil
	}
	cookies, err := sessionCookies(path)
	if err != nil {
		return err
	}
	checked, err := newLoader(l.base, func(k string) string {
		if k == "SUPERVISOR_ENVIRONMENT" {
			return "local"
		}
		return cookies[strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(k, "SUPERVISOR_SIM_"), "_COOKIE"))]
	})
	if err != nil {
		return err
	}
	defer checked.client.CloseIdleConnections()
	for _, role := range []string{"owner", "planner", "approver", "operator"} {
		current, next := l.actors[role], checked.actors[role]
		if current.cookie == next.cookie {
			continue
		}
		var me store.Me
		if _, err = checked.request(ctx, role, "GET", "/v1/me", "", "", "", nil, &me); err != nil {
			return err
		}
		expected := role
		if role == "planner" || role == "approver" {
			expected = "supervisor"
		}
		if len(me.Sites) != 1 || me.Sites[0].ID != factorysim.SiteID || me.Sites[0].TenantID != factorysim.TenantID || me.Sites[0].Role != expected || me.CSRFToken == "" || (current.subject != "" && current.subject != me.Subject) {
			return errors.New("renewed fixture session changed identity or site")
		}
		l.actors[role] = &actor{cookie: next.cookie, csrf: me.CSRFToken, subject: me.Subject}
	}
	return nil
}

func saveLiveReport(dir string, report liveReport) error {
	tmp := filepath.Join(dir, ".run-report.json")
	if err := os.WriteFile(tmp, wire(report), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "run-report.json"))
}

func runLive(ctx context.Context, l *loader, d dataset, schedule factorysim.Schedule, clock factorysim.Clock, dir, sessions string, out io.Writer) (report liveReport, err error) {
	report = liveReport{SchemaVersion: 1, Synthetic: true, Schedule: schedule, ManifestSHA256: sha(d.files["manifest.json"]), Approvals: "scripted synthetic fixture identities; not independent human review", Batches: []ingestion.Result{}, Runs: []workstation.ShiftRunV1{}, Bindings: l.bindings}
	defer func() {
		if err != nil {
			report.Failure = err.Error()
		}
		err = errors.Join(err, saveLiveReport(dir, report))
	}()
	if !clock.Now().Before(schedule.WallStart) {
		return report, errors.New("wall start must be in the future before bootstrap")
	}
	if err = refreshSessions(ctx, l, sessions); err != nil {
		return report, err
	}
	plans, err := liveBootstrap(ctx, l, d)
	if err != nil {
		return report, err
	}
	if !clock.Now().Before(schedule.WallStart) {
		return report, errors.New("bootstrap missed the frozen wall start; choose a later anchor on a fresh facility")
	}
	if err = saveLiveReport(dir, report); err != nil {
		return report, err
	}
	fmt.Fprintf(out, "SYNTHETIC bootstrap complete; model day=%s wall start=%s speed=%d\n", schedule.ModelStart.Format(time.RFC3339), schedule.WallStart.Format(time.RFC3339), schedule.Speed)
	op := "/operational-sources/" + d.id("telemetry")
	states := map[string]bool{}
	for shift := range 3 {
		start := schedule.Wall(schedule.ModelStart.Add(time.Duration(shift) * 8 * time.Hour))
		if err = clock.WaitUntil(ctx, start); err != nil {
			return report, err
		}
		if err = refreshSessions(ctx, l, sessions); err != nil {
			return report, err
		}
		runs := make([]workstation.ShiftRunV1, 6)
		for li, plan := range plans[shift] {
			path := "/shift-plans/" + plan.ID + "/run"
			if err = l.call(ctx, "planner", path, path, "2", workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: 1}, &runs[li]); err != nil {
				return report, err
			}
			if runs[li].State != workstation.RunStarted || runs[li].StartedAt == nil {
				return report, errors.New("shift did not start")
			}
			for _, a := range plan.Revisions[0].Plan.Assignments {
				states[a.WorkstationID] = false
				for _, kind := range []workstation.RecordKind{workstation.KindAcknowledged, workstation.KindStarted} {
					b := workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: a.WorkstationID, JobOrderID: a.JobOrderID, JobOrderRevision: 1, EventAt: start.Format(time.RFC3339Nano)}
					if err = liveRecord(ctx, l, b, plan.ID+string(kind), &report); err != nil {
						return report, err
					}
				}
			}
		}
		for q := shift * 32; q < (shift+1)*32; q++ {
			at := schedule.IntervalEnd(q)
			if err = clock.WaitUntil(ctx, at); err != nil {
				return report, err
			}
			if err = refreshSessions(ctx, l, sessions); err != nil {
				return report, err
			}
			// One interval of tolerance covers HTTP receipt latency, but never silently skips a sample.
			if clock.Now().After(at.Add(15 * time.Minute / time.Duration(schedule.Speed))) {
				return report, fmt.Errorf("missed interval %d deadline", q)
			}
			var batch ingestion.Batch
			if err = json.Unmarshal(l.bind(d.files[fmt.Sprintf("batches/%06d.json", q)]), &batch); err != nil {
				return report, err
			}
			var receipt ingestion.Result
			status, e := l.request(ctx, "owner", "POST", "/v1/sites/"+factorysim.SiteID+op+"/batches", fmt.Sprintf("live-%d", q), "", "application/json", wire(batch), &receipt)
			if e != nil {
				return report, e
			}
			if status != 202 || receipt.Replayed || receipt.EventCount != len(batch.Events) || receipt.LastSequence != batch.Events[len(batch.Events)-1].SourceSequence {
				return report, fmt.Errorf("interval %d has unexpected receipt", q)
			}
			report.Batches = append(report.Batches, receipt)
			for _, event := range batch.Events {
				if event.Kind != "machine_state" {
					continue
				}
				var observation factorysim.Observation
				if err = json.Unmarshal(event.Payload, &observation); err != nil {
					return report, err
				}
				var assignment workstation.AssignmentV1
				for _, plan := range plans[shift] {
					for _, a := range plan.Revisions[0].Plan.Assignments {
						if a.WorkstationID == event.AssetExternalKey {
							assignment = a
						}
					}
				}
				if assignment.WorkstationID == "" {
					return report, errors.New("machine event has no shift assignment")
				}
				b := workstation.StationRecordBodyV1{SchemaVersion: 1, WorkstationID: assignment.WorkstationID, JobOrderID: assignment.JobOrderID, JobOrderRevision: 1, EventAt: at.Add(-time.Millisecond).Format(time.RFC3339Nano)}
				stopped := observation.State != "running"
				if stopped != states[b.WorkstationID] {
					b.Kind = workstation.KindResumed
					if stopped {
						b.Kind = workstation.KindPaused
						b.Comment = "SYNTHETIC: " + observation.Reason
					}
					if err = liveRecord(ctx, l, b, event.ExternalKey+"-state", &report); err != nil {
						return report, err
					}
					states[b.WorkstationID] = stopped
				}
				b.Kind = workstation.KindQuantity
				b.Comment = ""
				b.GoodQuantity = &observation.Good
				b.RejectQuantity = &observation.Scrap
				if err = liveRecord(ctx, l, b, event.ExternalKey+"-quantity", &report); err != nil {
					return report, err
				}
			}
			if err = saveLiveReport(dir, report); err != nil {
				return report, err
			}
		}
		for li, plan := range plans[shift] {
			nextStart := schedule.Wall(schedule.ModelStart.Add(time.Duration(shift+1) * 8 * time.Hour))
			zone := schedule.ModelStart.Location()
			next := workstation.NextShiftV1{LineID: plan.LineID, StartsAt: nextStart.In(zone).Format(time.RFC3339Nano), EndsAt: nextStart.Add(8 * time.Hour / time.Duration(schedule.Speed)).In(zone).Format(time.RFC3339Nano), Timezone: "Asia/Kolkata"}
			path := "/shift-plans/" + plan.ID + "/run"
			var ended workstation.ShiftRunV1
			if err = l.call(ctx, "planner", path+"/end", path+"/end", strconv.FormatInt(runs[li].PlanStateRevision, 10), workstation.ShiftRunEndInputV1{SchemaVersion: 1, NextShift: next}, &ended); err != nil {
				return report, err
			}
			if err = l.call(ctx, "owner", path, "", "", nil, &ended); err != nil {
				return report, err
			}
			if ended.State != workstation.RunEnded || ended.Summary == nil || ended.HandoverRevision == nil || ended.EndedAt == nil {
				return report, errors.New("ended shift lacks summary or handover")
			}
			for _, suffix := range []string{"summaries", "handovers"} {
				var evidence map[string]json.RawMessage
				if err = l.call(ctx, "owner", "/shift-plans/"+plan.ID+"/"+suffix, "", "", nil, &evidence); err != nil {
					return report, err
				}
				if len(evidence[suffix]) < 3 {
					return report, fmt.Errorf("missing stored %s", suffix)
				}
			}
			report.Runs = append(report.Runs, ended)
		}
		fmt.Fprintf(out, "SYNTHETIC shift %d ended: six summaries and handovers; batches=%d station_records=%d\n", shift+1, len(report.Batches), report.StationRecords)
	}
	if err = awaitLiveFold(ctx, l, clock, op, d.manifest.Events, &report); err != nil {
		return report, err
	}
	report.Passed = len(report.Runs) == 18 && len(report.Batches) == 96
	return report, nil
}

// liveBootstrap loads master data and reads the day's shift plans. It is a variable only so the
// fake-clock test can replace the HTTP bootstrap and drive runLive's deadlines.
var liveBootstrap = func(ctx context.Context, l *loader, d dataset) ([][]workstation.ShiftPlanV1, error) {
	if _, err := l.prepare(ctx, d); err != nil {
		return nil, err
	}
	plans := make([][]workstation.ShiftPlanV1, 3)
	for shift := range 3 {
		for li := range d.manifest.Lines {
			id := l.bindings[fmt.Sprintf("plan/shifts/d00-l%d-s%d.json", li, shift)]
			var plan workstation.ShiftPlanV1
			if err := l.call(ctx, "planner", "/shift-plans/"+id, "", "", nil, &plan); err != nil {
				return nil, err
			}
			plans[shift] = append(plans[shift], plan)
			if shift == 0 {
				for _, a := range plan.Revisions[0].Plan.Assignments {
					body := workstation.StationAuthorizationInputV1{GranteeIssuer: plan.Revisions[0].SubmittedBy.Issuer, GranteeSubject: l.actors["operator"].subject, Action: workstation.AuthorizationGrant}
					if err := l.call(ctx, "planner", "/workstations/"+a.WorkstationID+"/authorizations", "grant-"+a.WorkstationID, "0", body, nil); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return plans, nil
}

// awaitLiveFold waits, on the injected clock, until every event of the day has folded.
func awaitLiveFold(ctx context.Context, l *loader, clock factorysim.Clock, op string, events int64, report *liveReport) error {
	deadline := clock.Now().Add(60 * time.Second)
	for {
		var live ingestion.Liveness
		if err := l.call(ctx, "owner", op+"/liveness", "", "", nil, &live); err != nil {
			return err
		}
		report.Liveness = &live
		if live.Completeness != nil && live.Completeness.QuarantinedCount > 0 {
			return fmt.Errorf("normal run quarantined %d events", live.Completeness.QuarantinedCount)
		}
		if live.Ingest.ProcessedSequence != nil && *live.Ingest.ProcessedSequence == events && live.Completeness != nil && live.Completeness.CompleteSequence == events {
			return nil
		}
		if !clock.Now().Before(deadline) {
			return errors.New("normal run did not finish folding")
		}
		if err := clock.WaitUntil(ctx, clock.Now().Add(200*time.Millisecond)); err != nil {
			return err
		}
	}
}

func liveRecord(ctx context.Context, l *loader, b workstation.StationRecordBodyV1, key string, report *liveReport) error {
	var record workstation.StationRecordV1
	if err := l.call(ctx, "operator", "/workstations/"+b.WorkstationID+"/records", key+b.WorkstationID, "", b, &record); err != nil {
		return err
	}
	if record.ID == "" || record.Replayed {
		return errors.New("station record not newly accepted")
	}
	report.StationRecords++
	return nil
}
