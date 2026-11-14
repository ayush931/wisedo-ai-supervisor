package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/knowledge"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plans"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/plantcontext"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/roster"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

type actor struct{ cookie, csrf, subject string }
type loader struct {
	base      string
	client    *http.Client
	actors    map[string]*actor
	keyPrefix string
	bindings  map[string]string
}
type loadReport struct {
	ConfigurationRejected int      `json:"configuration_rejected"`
	Quarantined           int      `json:"quarantined"`
	Events                int64    `json:"events"`
	ReadyContexts         int      `json:"ready_contexts"`
	ReadyOEE              int      `json:"ready_oee_calculations"`
	Answers               int      `json:"new_interview_answers"`
	ContextIDs            []string `json:"context_ids"`
}

func localURL(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return "", errors.New("invalid fixture URL")
	}
	ip, e := netip.ParseAddr(u.Hostname())
	if e != nil || !ip.IsLoopback() || u.Port() == "48176" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("fixture requires literal loopback URL, no credentials/path/query, and not CI port 48176")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
func newLoader(raw string, getenv func(string) string) (*loader, error) {
	if getenv("SUPERVISOR_ENVIRONMENT") != "local" {
		return nil, errors.New("synthetic loader requires SUPERVISOR_ENVIRONMENT=local")
	}
	base, e := localURL(raw)
	if e != nil {
		return nil, e
	}
	l := &loader{base: base, actors: map[string]*actor{}, bindings: map[string]string{}, client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 15 * time.Second, MaxIdleConns: 4}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, role := range []string{"owner", "planner", "approver", "operator"} {
		v := getenv("SUPERVISOR_SIM_" + strings.ToUpper(role) + "_COOKIE")
		cookie := &http.Cookie{Name: "supervisor", Value: v}
		if v == "" || cookie.Valid() != nil || len(v) > 512 {
			return nil, fmt.Errorf("valid SUPERVISOR_SIM_%s_COOKIE required", strings.ToUpper(role))
		}
		l.actors[role] = &actor{cookie: v}
	}
	return l, nil
}
func (l *loader) request(ctx context.Context, role, method, path, key, match, contentType string, body []byte, out any) (int, error) {
	return l.requestHeaders(ctx, role, method, path, key, match, contentType, body, nil, out)
}
func (l *loader) requestHeaders(ctx context.Context, role, method, path, key, match, contentType string, body []byte, extra http.Header, out any) (int, error) {
	a := l.actors[role]
	r, e := http.NewRequestWithContext(ctx, method, l.base+path, bytes.NewReader(body))
	if e != nil {
		return 0, e
	}
	r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.cookie})
	r.Header.Set("Origin", l.base)
	r.Header.Set("X-CSRF-Token", a.csrf)
	r.Header.Set("Content-Type", contentType)
	if key != "" {
		r.Header.Set("Idempotency-Key", l.keyPrefix+sha([]byte(key))[:32])
	}
	if match != "" {
		r.Header.Set("If-Match", strconv.Quote(match))
	}
	for name, values := range extra {
		r.Header[name] = values
	}
	response, e := l.client.Do(r)
	if e != nil {
		return 0, errors.New("local fixture request failed")
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if e != nil || len(b) > 4<<20 {
		return response.StatusCode, errors.New("invalid or oversized fixture response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &problem)
		return response.StatusCode, &fixtureProblem{Status: response.StatusCode, Code: problem.Code, Method: method, Path: path}
	}
	if out != nil {
		if e = json.Unmarshal(b, out); e != nil {
			return response.StatusCode, fmt.Errorf("invalid response for %s: %w", path, e)
		}
	}
	return response.StatusCode, nil
}
func (l *loader) call(ctx context.Context, role, path, key, match string, body, out any) error {
	var b []byte
	method := "GET"
	if body != nil {
		b = wire(body)
		method = "POST"
	}
	_, e := l.request(ctx, role, method, "/v1/sites/"+factorysim.SiteID+path, key, match, "application/json", b, out)
	return e
}
func (l *loader) bind(b []byte) []byte {
	var v any
	_ = json.Unmarshal(b, &v)
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if replacement, ok := l.bindings[x]; ok {
				return replacement
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k, v := range x {
				x[k] = walk(v)
			}
		}
		return v
	}
	return wire(walk(v))
}
func (l *loader) importCSV(ctx context.Context, name string, data []byte) (workstation.ConfigurationImportReportV1, error) {
	var buffer bytes.Buffer
	w := multipart.NewWriter(&buffer)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s.csv"`, name))
	header.Set("Content-Type", "text/csv")
	part, e := w.CreatePart(header)
	if e != nil {
		return workstation.ConfigurationImportReportV1{}, e
	}
	if _, e = part.Write(data); e != nil {
		return workstation.ConfigurationImportReportV1{}, e
	}
	if e = w.Close(); e != nil {
		return workstation.ConfigurationImportReportV1{}, e
	}
	base := "/v1/sites/" + factorysim.SiteID + "/configuration-imports"
	var report workstation.ConfigurationImportReportV1
	if _, e = l.request(ctx, "planner", "POST", base+"/dry-runs", "", "", w.FormDataContentType(), buffer.Bytes(), &report); e != nil {
		return report, e
	}
	if report.Counts.Rejected != 0 || !report.Applicable {
		return report, fmt.Errorf("%s dry-run rejected %d rows: %s", name, report.Counts.Rejected, wire(report.Rows))
	}
	if report.Counts.Accepted == 0 {
		return report, nil
	}
	_, e = l.request(ctx, "planner", "POST", base, name, report.ReportSHA256, w.FormDataContentType(), buffer.Bytes(), &report)
	return report, e
}
func (l *loader) identify(ctx context.Context) error {
	for _, role := range []string{"owner", "planner", "approver", "operator"} {
		a := l.actors[role]
		var me store.Me
		if _, e := l.request(ctx, role, "GET", "/v1/me", "", "", "", nil, &me); e != nil {
			return e
		}
		expected := role
		if role == "planner" || role == "approver" {
			expected = "supervisor"
		}
		if len(me.Sites) != 1 || me.Sites[0].ID != factorysim.SiteID || me.Sites[0].TenantID != factorysim.TenantID || me.Sites[0].Role != expected {
			return fmt.Errorf("%s must belong only to the reserved fixture site with role %s", role, expected)
		}
		a.csrf = me.CSRFToken
		a.subject = me.Subject
		if a.csrf == "" {
			return errors.New("fixture CSRF missing")
		}
	}
	for _, a := range []string{"owner", "planner", "approver", "operator"} {
		for _, b := range []string{"owner", "planner", "approver", "operator"} {
			if a != b && l.actors[a].subject == l.actors[b].subject {
				return errors.New("scripted synthetic fixture identities must be distinct")
			}
		}
	}
	return nil
}

func (l *loader) prepare(ctx context.Context, d dataset) (loadReport, error) {
	result := loadReport{ContextIDs: []string{}}
	l.keyPrefix = "fs-" + sha(d.files["manifest.json"])[:24] + "-"
	if err := l.identify(ctx); err != nil {
		return result, err
	}
	imported, e := l.importCSV(ctx, "configuration", d.files["configuration.csv"])
	if e != nil {
		return result, e
	}
	for _, r := range imported.Rows {
		if r.Kind == "line" {
			i, e := strconv.Atoi(strings.TrimPrefix(r.Ref, "line"))
			if e != nil || i < 0 || i >= len(d.manifest.Lines) {
				return result, errors.New("invalid line import result")
			}
			l.bindings[d.manifest.Lines[i].ID] = r.ObjectID
		}
		if r.Kind == "item" {
			l.bindings[d.id("item/"+r.Ref)] = r.ObjectID
		}
	}
	files := func(prefix string) []string {
		var names []string
		for _, f := range d.manifest.Files {
			if strings.HasPrefix(f.Path, prefix) && strings.HasSuffix(f.Path, ".json") {
				names = append(names, f.Path)
			}
		}
		return names
	}
	for _, name := range files("sources/") {
		var in evidence.Input
		_ = json.Unmarshal(d.files[name], &in)
		if name == "sources/plant.json" {
			for from, to := range l.bindings {
				in.Text = strings.ReplaceAll(in.Text, from, to)
			}
		}
		path := "/sources/" + in.SourceID
		if e = l.call(ctx, "owner", "/sources", name, "0", in, nil); e != nil {
			return result, e
		}
		var authority evidence.Authority
		if e = l.call(ctx, "owner", path+"/authority-decisions", name+"-authority", "0", evidence.AuthorityInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, Verdict: "designate", Reason: "Designated as the plant reference"}, &authority); e != nil {
			return result, e
		}
		if e = l.call(ctx, "owner", path+"/currentness-decisions", name+"-current", "0", evidence.CurrentnessInput{SourceRevision: 1, ExpectedSourceStateRevision: 1, AuthorityRevision: 1, AuthorityDecisionID: authority.DecisionID, Verdict: "confirm_current", Reason: "Revision 1 confirmed current"}, nil); e != nil {
			return result, e
		}
	}
	var source evidence.Input
	_ = json.Unmarshal(d.files["sources/plant.json"], &source)
	var spanIDs []string
	for start, length := 0, len([]rune(source.Text)); start < length; start += 4096 {
		var span evidence.Span
		if e = l.call(ctx, "owner", "/sources/"+source.SourceID+"/spans", fmt.Sprintf("plant-span-%d", start), "1", map[string]any{"revision": 1, "start_char": start, "end_char": min(start+4096, length)}, &span); e != nil {
			return result, e
		}
		spanIDs = append(spanIDs, span.ID)
	}
	for _, name := range files("entities/") {
		if e = l.call(ctx, "owner", "/entities", name, "0", json.RawMessage(l.bind(d.files[name])), nil); e != nil {
			return result, e
		}
	}
	for _, name := range files("claims/") {
		var claim knowledge.ClaimInput
		if e = json.Unmarshal(l.bind(d.files[name]), &claim); e != nil {
			return result, e
		}
		claim.SourceSpanIDs = spanIDs
		if e = l.call(ctx, "planner", "/claims", name, "0", claim, nil); e != nil {
			return result, e
		}
		if e = l.call(ctx, "approver", "/claims/"+claim.ClaimID+"/reviews", name+"-review", "1", knowledge.ReviewInput{ClaimRevision: 1, Verdict: "approve", Reason: "Machine to line assignment checked against the plant model"}, nil); e != nil {
			return result, e
		}
	}
	for i, line := range d.manifest.Lines {
		if e = l.call(ctx, "approver", "/maintenance-roster/decisions", fmt.Sprintf("roster-%d", i), "0", roster.DecisionInput{LineID: l.bindings[line.ID], AssigneeSubject: l.actors["planner"].subject, Decision: roster.DecisionConfirm, Reason: "Shift lead assigned inspection responsibility"}, nil); e != nil {
			return result, e
		}
	}
	for _, name := range files("profiles/") {
		id := strings.TrimSuffix(filepath.Base(name), ".json")
		if e = l.call(ctx, "planner", "/workstations/"+id+"/profile-revisions", name, "0", json.RawMessage(l.bind(d.files[name])), nil); e != nil {
			return result, e
		}
	}
	// CSV UUID fields are exact cells; replace only master-data bindings, never free text.
	routing := string(d.files["routings.csv"])
	for from, to := range l.bindings {
		routing = strings.ReplaceAll(routing, from, to)
	}
	routings, e := l.importCSV(ctx, "routings", []byte(routing))
	if e != nil {
		return result, e
	}
	for _, r := range routings.Rows {
		if r.Kind == "routing" {
			l.bindings[d.id("routing/"+r.Ref)] = r.ObjectID
		}
	}
	for _, sku := range d.manifest.SKUs {
		if l.bindings[d.id("item/"+sku.Code)] == "" || l.bindings[d.id("routing/"+sku.Code)] == "" {
			return result, errors.New("item or routing import returned no object ID for " + sku.Code)
		}
	}
	for _, name := range files("jobs/") {
		var job workstation.JobOrderV1
		if e = l.call(ctx, "planner", "/job-orders", name, "", json.RawMessage(l.bind(d.files[name])), &job); e != nil {
			return result, e
		}
		l.bindings[d.id("job/"+strings.TrimSuffix(filepath.Base(name), ".json"))] = job.ID
	}
	for _, name := range files("shifts/") {
		var plan workstation.ShiftPlanV1
		if e = l.call(ctx, "planner", "/shift-plans", name, "", json.RawMessage(l.bind(d.files[name])), &plan); e != nil {
			return result, e
		}
		l.bindings["plan/"+name] = plan.ID
		if len(plan.Revisions) != 1 {
			return result, errors.New("synthetic shift was revised externally")
		}
		if e = l.call(ctx, "approver", "/shift-plans/"+plan.ID+"/approvals", name+"-approve", "1", workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[0].CanonicalPayloadSHA256}, nil); e != nil {
			return result, e
		}
	}
	for _, name := range files("plans/") {
		var plan plans.Revision
		var input plans.CaptureInput
		if e = json.Unmarshal(l.bind(d.files[name]), &input); e != nil {
			return result, e
		}
		input.SourceSpanIDs = spanIDs
		if e = l.call(ctx, "planner", "/plan-revisions", name, "", input, &plan); e != nil {
			return result, e
		}
		if e = l.call(ctx, "approver", "/plan-revisions/"+plan.ID+"/reviews", name+"-approve", "0", plans.ReviewInput{Decision: "approve", Reason: "Schedule checked against the line plan"}, nil); e != nil {
			return result, e
		}
	}
	for _, name := range files("discovery/") {
		if e = l.call(ctx, "owner", "/discovery-inventories", name, "0", json.RawMessage(l.bind(d.files[name])), nil); e != nil {
			return result, e
		}
	}
	for li := range d.manifest.Lines {
		for _, role := range []string{"owner", "supervisor", "operator"} {
			actorRole := role
			if role == "supervisor" {
				actorRole = "planner"
			}
			process := uuid.MustParse(d.id(fmt.Sprintf("process/%d", li)))
			var interview onboarding.InterviewSession
			name := fmt.Sprintf("interview-%d-%s", li, role)
			if e = l.call(ctx, actorRole, "/interviews", name, "", onboarding.CreateInterviewInput{Role: onboarding.InterviewRole(role), ProcessID: &process}, &interview); e != nil {
				return result, e
			}
			path := "/interviews/" + interview.SessionID.String()
			for round := 0; round < 12; round++ {
				var questions onboarding.QuestionPage
				if e = l.call(ctx, actorRole, path+"/questions?limit=100", "", "", nil, &questions); e != nil {
					return result, e
				}
				answered := 0
				for _, q := range questions.Questions {
					if q.AnswerState != onboarding.AnswerUnanswered {
						continue
					}
					var input onboarding.AnswerInput
					_ = json.Unmarshal(d.files[fmt.Sprintf("answers/%d-%s.json", li, role)], &input)
					input.QuestionID = q.QuestionID
					input.QuestionRevision = q.Revision
					input.ClientAnswerID = uuid.MustParse(d.id("answer/" + q.QuestionID.String()))
					if e = l.call(ctx, actorRole, path+"/answers", name+q.QuestionID.String(), "", input, nil); e != nil {
						return result, e
					}
					answered++
					result.Answers++
				}
				if answered == 0 {
					break
				}
				if round == 11 {
					return result, errors.New("interview questions did not converge")
				}
			}
		}
	}
	// Read an existing immutable snapshot on replay; new answer evidence changes inventory revision.
	var snapshot knowledge.Snapshot
	status, e := l.request(ctx, "owner", "GET", "/v1/sites/"+factorysim.SiteID+"/process-snapshots/"+d.id("snapshot"), "", "", "", nil, &snapshot)
	if status == 404 {
		var inventory knowledge.Inventory
		if e = l.call(ctx, "owner", "/knowledge-inventory", "", "", nil, &inventory); e != nil {
			return result, e
		}
		if e = l.call(ctx, "owner", "/process-snapshots", "snapshot", strconv.FormatInt(inventory.Revision, 10), json.RawMessage(d.files["snapshot.json"]), &snapshot); e != nil {
			return result, e
		}
	} else if e != nil {
		return result, e
	}
	sourceID := d.id("telemetry")
	op := "/operational-sources/" + sourceID
	if e = l.call(ctx, "owner", "/operational-sources", "source", "0", json.RawMessage(d.files["operational-source.json"]), nil); e != nil {
		return result, e
	}
	if e = l.call(ctx, "owner", op+"/mappings", "mappings", "1", json.RawMessage(l.bind(d.files["mappings.json"])), nil); e != nil {
		return result, e
	}
	if e = l.call(ctx, "approver", op+"/reviews", "source-review", "2", map[string]string{"verdict": "approved", "reason": "Telemetry scope and machine identities checked"}, nil); e != nil {
		return result, e
	}
	return result, nil
}

func (l *loader) load(ctx context.Context, d dataset) (loadReport, error) {
	result, e := l.prepare(ctx, d)
	if e != nil {
		return result, e
	}
	sourceID := d.id("telemetry")
	op := "/operational-sources/" + sourceID
	files := func(prefix string) []string { return datasetPaths(d, prefix) }
	var last ingestion.Result
	for _, name := range files("batches/") {
		if e = l.call(ctx, "owner", op+"/batches", name, "", json.RawMessage(l.bind(d.files[name])), &last); e != nil {
			return result, e
		}
		result.Events += int64(last.EventCount)
	}
	// Transport time comes from an idempotent server receipt: historical event times stay intact.
	// Replays reuse exactly this timestamp; a wall-clock heartbeat would conflict on re-load.
	when := last.AcceptedAt
	tail := ingestion.Batch{SchemaVersion: 1, SourceID: sourceID, SourceRevision: 2, MappingRevision: 2, Events: []ingestion.Event{{ExternalKey: "fixture-heartbeat", Revision: 1, Kind: "heartbeat", SourceSequence: last.LastSequence + 1, EventAt: when, Payload: wire(map[string]string{"observed_at": stamp(when)})}, {ExternalKey: "fixture-watermark", Revision: 1, Kind: "watermark", SourceSequence: last.LastSequence + 2, EventAt: when, Payload: wire(map[string]any{"complete_through": d.manifest.Start + "T00:00:00Z", "through_sequence": last.LastSequence + 1})}}}
	zone, _ := time.LoadLocation("Asia/Kolkata")
	start, _ := time.ParseInLocation("2006-01-02", d.manifest.Start, zone)
	tail.Events[1].Payload = wire(map[string]any{"complete_through": stamp(start.AddDate(0, 0, d.manifest.Days).Add(6 * time.Hour)), "through_sequence": last.LastSequence + 1})
	if e = l.call(ctx, "owner", op+"/batches", "tail", "", tail, nil); e != nil {
		return result, e
	}
	result.Events += 2
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var live ingestion.Liveness
		if e = l.call(ctx, "owner", op+"/liveness", "", "", nil, &live); e != nil {
			return result, e
		}
		if live.Completeness != nil && live.Completeness.QuarantinedCount > 0 {
			return result, fmt.Errorf("source quarantined %d events", live.Completeness.QuarantinedCount)
		}
		if live.Ingest.ProcessedSequence != nil && *live.Ingest.ProcessedSequence == result.Events && live.Completeness != nil && live.Completeness.CompleteSequence == result.Events {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-deadline.C:
			return result, errors.New("operational worker did not finish folding")
		case <-ticker.C:
		}
	}
	for _, name := range files("contexts/") {
		var view plantcontext.View
		if e = l.call(ctx, "owner", "/contexts", name, "", json.RawMessage(l.bind(d.files[name])), &view); e != nil {
			return result, e
		}
		if view.Readiness != plantcontext.ReadyState || !view.CurrentStatus.Eligible {
			return result, fmt.Errorf("context %s is %s: %v eligibility=%v", name, view.Readiness, view.ReasonCodes, view.CurrentStatus.Reasons)
		}
		result.ReadyContexts++
		result.ContextIDs = append(result.ContextIDs, view.ContextID)
	}
	result.ReadyOEE, e = l.loadOEE(ctx, d)
	return result, e
}
