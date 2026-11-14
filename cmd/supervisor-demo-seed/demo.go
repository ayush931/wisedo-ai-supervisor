package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"time"

	apiconfig "github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
	"github.com/google/uuid"
)

const ownerUsername = "foundation-reviewer"

var demoUsers = []struct{ username, role string }{
	{"shift-lead", "supervisor"}, {"shift-approver", "supervisor"}, {"operator-1", "operator"}, {"operator-2", "operator"}, {"read-only-viewer", "viewer"},
}

// demoNames are the first and last names the IdP puts in the ID token name claim (WIS-290).
var demoNames = map[string][2]string{
	"shift-lead": {"Rahul", "Deshmukh"}, "shift-approver": {"Ananya", "Kulkarni"}, "operator-1": {"Sanjay", "Patil"}, "operator-2": {"Meera", "Joshi"}, "read-only-viewer": {"Kiran", "Shah"},
}

const (
	sopTitle = "Assembly SOP — bracket line"
	sopText  = "SOP for bracket line 1. Press: load blank A-100, cycle press, inspect for cracks, record good and reject counts. Weld: clamp bracket, weld seam, read weld temperature, record disposition."
	shiftLen = 8 * time.Hour
)

// demoScope is everything the builder needs. control is the privileged connection, used only
// for memberships and the synthetic browser sessions (as the integration fixtures do); every
// business write goes through the public API handler on the runtime role.
type demoScope struct {
	control                                    *sql.DB
	runtime                                    *store.Store
	tenant, site, issuer, origin, clientSecret string
	subjects                                   map[string]string // username -> OIDC subject
	now                                        func() time.Time
}

type demoResult struct {
	Press, Weld, Plan, Case string
	PressCurrent            workstation.CurrentJobOrderV1
}

type demoActor struct{ token, csrf string }

type demo struct {
	demoScope
	ctx     context.Context
	handler http.Handler
	actors  map[string]demoActor
	hashes  []string
}

// shiftWindow returns the 06:00/14:00/22:00 Asia/Kolkata shift containing now.
func shiftWindow(now time.Time) (time.Time, time.Time) {
	ist := time.FixedZone("IST", 5*3600+1800) // Asia/Kolkata has no DST
	local := now.In(ist)
	start := time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, ist)
	for start.After(local) {
		start = start.Add(-shiftLen)
	}
	for !local.Before(start.Add(shiftLen)) {
		start = start.Add(shiftLen)
	}
	return start, start.Add(shiftLen) // offsets must agree with the plan timezone
}

func (d *demo) id(name string) string {
	return uuid.NewSHA1(uuid.MustParse(d.site), []byte("wis520-demo:"+name)).String()
}

// call drives the in-process API as one synthetic user. A non-accepted status returns the
// HTTP status and problem code only, never the body.
func (d *demo) call(user, method, path, key, ifMatch string, body, out any, accepted ...int) error {
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	a := d.actors[user]
	r := httptest.NewRequestWithContext(d.ctx, method, "/v1/sites/"+d.site+path, reader)
	r.AddCookie(&http.Cookie{Name: "supervisor", Value: a.token})
	r.Header.Set("Origin", d.origin)
	r.Header.Set("X-CSRF-Token", a.csrf)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", `"`+ifMatch+`"`)
	}
	w := httptest.NewRecorder()
	d.handler.ServeHTTP(w, r)
	for _, status := range accepted {
		if w.Code == status {
			if out == nil {
				return nil
			}
			return json.Unmarshal(w.Body.Bytes(), out)
		}
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &problem)
	return fmt.Errorf("%s %s as %s: HTTP %d %s", method, path, user, w.Code, problem.Code)
}

func (d *demo) session(user string) error {
	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	a := demoActor{token: hex.EncodeToString(secret[:32]), csrf: hex.EncodeToString(secret[32:])}
	sum := sha256.Sum256([]byte(a.token))
	hash := hex.EncodeToString(sum[:])
	if _, err := d.control.ExecContext(d.ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now(),now(),now()+interval '10 minutes',now())`,
		hash, d.subjects[user], d.issuer, a.csrf); err != nil {
		return errors.New("cannot create synthetic session")
	}
	d.hashes = append(d.hashes, hash)
	d.actors[user] = a
	return nil
}

// buildDemo creates the WIS-520 demo dataset. Every step is idempotent: creates use stable
// Idempotency-Keys over stable bodies (the API replays them), and the only time-dependent
// steps (the shift plan for the current shift and its station records) are keyed by shift.
func buildDemo(ctx context.Context, s demoScope) (result demoResult, err error) {
	auth, err := identity.New(ctx, identity.Config{Issuer: s.issuer, ClientID: "supervisor", ClientSecret: s.clientSecret, RedirectURI: s.origin + "/auth/callback"}, s.runtime.DB, nil)
	if err != nil {
		return result, err
	}
	handler, drain := httpapi.NewWithShutdown(apiconfig.Config{Environment: "local", WebDir: os.DevNull}, s.runtime, auth)
	d := &demo{demoScope: s, ctx: ctx, handler: handler, actors: map[string]demoActor{}}
	defer func() {
		// The synthetic sessions are credentials: never leave them behind.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = drain(cleanup)
		for _, hash := range d.hashes {
			if _, e := s.control.ExecContext(cleanup, `DELETE FROM supervisor.browser_sessions WHERE token_hash=$1`, hash); e != nil && err == nil {
				err = errors.New("cannot remove synthetic session")
			}
		}
	}()

	users := append([]struct{ username, role string }{{ownerUsername, "owner"}}, demoUsers...)
	for _, u := range users {
		if _, err = s.control.ExecContext(ctx, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
			s.tenant, s.site, s.issuer, s.subjects[u.username], u.role); err != nil {
			return result, errors.New("cannot provision synthetic membership")
		}
		if err = d.session(u.username); err != nil {
			return result, err
		}
	}

	// Instruction document, designated authoritative and confirmed current by the owner.
	source := d.id("source:sop")
	if err = d.call(ownerUsername, "POST", "/sources", "demo-seed:source", "0", map[string]string{"source_id": source, "kind": "document", "title": sopTitle, "text": sopText}, nil, 200, 201); err != nil {
		return result, err
	}
	var authority struct {
		DecisionID string `json:"decision_id"`
	}
	if err = d.call(ownerUsername, "POST", "/sources/"+source+"/authority-decisions", "demo-seed:authority", "0",
		map[string]any{"source_revision": 1, "expected_source_state_revision": 1, "verdict": "designate", "reason": "Owner designated the bracket line SOP"}, &authority, 200, 201); err != nil {
		return result, err
	}
	if err = d.call(ownerUsername, "POST", "/sources/"+source+"/currentness-decisions", "demo-seed:currentness", "0",
		map[string]any{"source_revision": 1, "expected_source_state_revision": 1, "authority_decision_id": authority.DecisionID, "authority_revision": 1, "verdict": "confirm_current", "reason": "Owner confirms the SOP is current"}, nil, 200, 201); err != nil {
		return result, err
	}
	sum := sha256.Sum256([]byte(sopText))
	sha := hex.EncodeToString(sum[:])

	line := d.id("line:bracket-line-1")
	type station struct {
		name, item, cycle string
		order             workstation.JobOrderBodyV1
		profile           workstation.StationProfileBodyV1
		job               workstation.JobOrderV1
	}
	press := &station{name: "press-01", item: "Bracket A-100, op 10 Press", cycle: "30"}
	press.profile = workstation.StationProfileBodyV1{Template: "production", Fields: []workstation.ProfileFieldV1{
		{Name: "good_units", Label: "Good quantity", Kind: "decimal", Required: true, Unit: "count"},
		{Name: "reject_units", Label: "Reject quantity", Kind: "decimal", Required: true, Unit: "count"},
		{Name: "cycle_time", Label: "Cycle time", Kind: "decimal", Required: true, Unit: "s"}}}
	weld := &station{name: "weld-01", item: "Bracket A-100, op 20 Weld", cycle: "45"}
	weld.profile = workstation.StationProfileBodyV1{Template: "quality", Fields: []workstation.ProfileFieldV1{
		{Name: "weld_temp_c", Label: "Weld temperature", Kind: "decimal", Required: true, Unit: "C", Min: "350", Max: "450"},
		{Name: "disposition", Label: "Disposition", Kind: "choice", Required: true, Choices: []string{"accept", "rework", "scrap"}}}}
	assignments := []workstation.AssignmentV1{}
	for _, st := range []*station{press, weld} {
		ws := d.id("workstation:" + st.name)
		st.profile.SchemaVersion, st.profile.WorkstationID, st.profile.LineID, st.profile.DisplayName = 1, ws, line, st.name
		if err = d.call("shift-lead", "POST", "/workstations/"+ws+"/profile-revisions", "demo-seed:profile:"+st.name, "0", st.profile, nil, 200, 201); err != nil {
			return result, err
		}
		fields := make([]workstation.FieldV1, 0, len(st.profile.Fields))
		for _, f := range st.profile.Fields {
			fields = append(fields, workstation.FieldV1{Name: f.Name, Label: f.Label, Kind: f.Kind, Required: f.Required, Unit: f.Unit, Choices: f.Choices})
		}
		st.order = workstation.JobOrderBodyV1{SchemaVersion: 1, OrderID: d.id("order:A-100"), OperationID: d.id("operation:" + st.name), ItemCode: st.item,
			TargetQuantity: 120, TargetCycleTimeSeconds: st.cycle, Priority: 2, InstructionDocumentID: source, InstructionRevision: 1, InstructionSHA256: sha,
			Template: st.profile.Template, Fields: fields}
		if err = d.call("shift-lead", "POST", "/job-orders", "demo-seed:order:"+st.name, "", st.order, &st.job, 200, 201); err != nil {
			return result, err
		}
		assignments = append(assignments, workstation.AssignmentV1{WorkstationID: ws, JobOrderID: st.job.ID, JobOrderRevision: 1, OrderID: st.order.OrderID,
			OperationID: st.order.OperationID, ItemCode: st.item, TargetQuantity: 120, TargetCycleTimeSeconds: st.cycle, InstructionDocumentID: source,
			InstructionRevision: 1, InstructionSHA256: sha, Template: st.order.Template, Fields: fields})
	}
	result.Press, result.Weld = assignments[0].WorkstationID, assignments[1].WorkstationID

	// Shift plan for the current shift: submitted by shift-lead, approved by shift-approver.
	starts, ends := shiftWindow(s.now())
	shift := strconv.FormatInt(starts.Unix(), 10)
	var plan workstation.ShiftPlanV1
	if err = d.call("shift-lead", "POST", "/shift-plans", "demo-seed:plan:"+shift, "", workstation.ShiftPlanBodyV1{SchemaVersion: 1, LineID: line,
		StartsAt: starts.Format(time.RFC3339), EndsAt: ends.Format(time.RFC3339), Timezone: "Asia/Kolkata", Assignments: assignments}, &plan, 200, 201); err != nil {
		return result, err
	}
	if plan.ApprovedRevision == nil {
		if err = d.call("shift-approver", "POST", "/shift-plans/"+plan.ID+"/approvals", "demo-seed:approve:"+plan.ID, strconv.FormatInt(plan.StateRevision, 10),
			workstation.ShiftPlanApproveInputV1{CanonicalPayloadSHA256: plan.Revisions[plan.CurrentRevision-1].CanonicalPayloadSHA256}, nil, 200); err != nil {
			return result, err
		}
	}
	result.Plan = plan.ID
	// Operators record only on a started shift. Start it once: a blind replay would carry a
	// newer If-Match after the plan is re-approved and conflict with the stored receipt.
	var run workstation.ShiftRunV1
	if err = d.call("shift-lead", "GET", "/shift-plans/"+plan.ID+"/run", "", "", nil, &run, 200); err != nil {
		return result, err
	}
	if run.State == workstation.RunPlanned {
		if err = d.call("shift-lead", "GET", "/shift-plans/"+plan.ID, "", "", nil, &plan, 200); err != nil {
			return result, err
		}
		if err = d.call("shift-lead", "POST", "/shift-plans/"+plan.ID+"/run", "demo-seed:start:"+plan.ID, strconv.FormatInt(plan.StateRevision, 10),
			workstation.ShiftRunStartInputV1{SchemaVersion: 1, PlanRevision: *plan.ApprovedRevision}, nil, 201); err != nil {
			return result, err
		}
	}

	for ws, operator := range map[string]string{result.Press: "operator-1", result.Weld: "operator-2"} {
		if err = d.call("shift-lead", "POST", "/workstations/"+ws+"/authorizations", "demo-seed:authorize:"+ws, "0",
			workstation.StationAuthorizationInputV1{GranteeIssuer: s.issuer, GranteeSubject: s.subjects[operator], Action: workstation.AuthorizationGrant}, nil, 200, 201); err != nil {
			return result, err
		}
	}

	// A few press-01 entries by operator-1, once per shift plan.
	// ponytail: only the first page (100) is checked; the demo writes four records per plan.
	var page workstation.StationRecordPageV1
	if err = d.call("operator-1", "GET", "/workstations/"+result.Press+"/records?limit=100", "", "", nil, &page, 200); err != nil {
		return result, err
	}
	recorded := false
	for _, r := range page.Records {
		recorded = recorded || r.PlanID == plan.ID
	}
	if !recorded {
		// Event times follow the approval and stay inside the tablet clock-skew allowance.
		at := s.now().UTC().Truncate(time.Millisecond).Add(time.Second)
		entry := func(kind workstation.RecordKind, ms int) workstation.StationRecordBodyV1 {
			return workstation.StationRecordBodyV1{SchemaVersion: 1, Kind: kind, WorkstationID: result.Press, JobOrderID: press.job.ID, JobOrderRevision: 1,
				EventAt: at.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)}
		}
		good, reject := int64(36), int64(1)
		quantity := entry(workstation.KindQuantity, 3)
		quantity.GoodQuantity, quantity.RejectQuantity = &good, &reject
		cycle := entry(workstation.KindCycleTime, 4)
		cycle.CycleTimeSeconds = "29.4"
		for _, body := range []workstation.StationRecordBodyV1{entry(workstation.KindAcknowledged, 1), entry(workstation.KindStarted, 2), quantity, cycle} {
			if err = d.call("operator-1", "POST", "/workstations/"+result.Press+"/records", uuid.NewString(), "", body, nil, 201); err != nil {
				return result, err
			}
		}
	}
	if err = d.call("operator-1", "GET", "/workstations/"+result.Press+"/current-job-order", "", "", nil, &result.PressCurrent, 200); err != nil {
		return result, err
	}
	result.Case, err = d.seedCase()
	return result, err
}
