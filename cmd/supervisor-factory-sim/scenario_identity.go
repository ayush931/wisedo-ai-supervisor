package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Only identity fixtures use SQL, as in the integration harness. All plant writes use HTTP.
func (s scenarioRunner) fixtureSession(ctx context.Context, role string) (string, error) {
	return s.fixtureSessionFor(ctx, role, "facility-edge-"+s.id(role), role == "expired")
}

// fixtureSessionFor forges a browser session for subject. Two sessions for one subject differ
// only in expiry, so an expired-session refusal has an unexpired positive control.
func (s scenarioRunner) fixtureSessionFor(ctx context.Context, role, subject string, expired bool) (string, error) {
	raw := os.Getenv("SUPERVISOR_TEST_ADMIN_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Port() != "62477" || u.Scheme != "postgres" && u.Scheme != "postgresql" || u.Path != "/supervisor" {
		return "", errors.New("identity edge fixtures require facility admin URL at 127.0.0.1:62477/supervisor")
	}
	db, err := sql.Open("pgx", raw)
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var issuer string
	if err = tx.QueryRowContext(ctx, `SELECT issuer FROM supervisor.browser_sessions WHERE token_hash=$1 AND subject=$2`, sha([]byte(s.l.actors["owner"].cookie)), s.l.actors["owner"].subject).Scan(&issuer); err != nil {
		return "", err
	}
	token, csrf := uuid.NewString(), uuid.NewString()
	expires := time.Now().Add(time.Hour)
	if expired {
		expires = time.Now().Add(-time.Minute)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,now()-interval '2 minutes',now(),$5,now())`, sha([]byte(token)), subject, issuer, csrf, expires)
	if err != nil {
		return "", err
	}
	if role == "foreign" {
		tenant, site := s.id("foreign-tenant"), s.id("foreign-site")
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.tenants(id,name) VALUES($1,'SYNTHETIC facility edge tenant')`, tenant); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'SYNTHETIC foreign site')`, tenant, site); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'viewer')`, tenant, site, issuer, subject); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	s.l.actors[role] = &actor{cookie: token, csrf: csrf, subject: subject}
	return issuer, nil
}
func (s scenarioRunner) identity(ctx context.Context, id string) (scenarioObservation, error) {
	if id == "expired-session" {
		return s.expiredSession(ctx)
	}
	role := "foreign"
	if _, err := s.fixtureSession(ctx, role); err != nil {
		return scenarioObservation{}, err
	}
	path := "/entities/" + s.d.manifest.Lines[0].Stations[0].ID
	first, err := s.request(ctx, role, "GET", path, "", "", "", nil, nil)
	if err != nil {
		return first, err
	}
	absent, err := s.request(ctx, role, "GET", "/entities/"+factorysim.ID(0, "absent-probe"), "", "", "", nil, nil)
	if err != nil {
		return first, err
	}
	if first.Status != absent.Status || first.Code != absent.Code {
		return first, errors.New("foreign request reveals existence")
	}
	first.Evidence = map[string]any{"existing": first.Status, "absent": absent.Status}
	return first, nil
}

// expiredSession gives one operator subject an active membership and two sessions that differ
// only in expiry. The unexpired one must write (positive control); the expired one must get 401
// and leave no source behind.
func (s scenarioRunner) expiredSession(ctx context.Context) (scenarioObservation, error) {
	subject := "facility-edge-" + s.id("session-subject")
	issuer, err := s.fixtureSessionFor(ctx, "fresh", subject, false)
	if err != nil {
		return scenarioObservation{}, err
	}
	if _, err = s.fixtureSessionFor(ctx, "expired", subject, true); err != nil {
		return scenarioObservation{}, err
	}
	if err = s.l.call(ctx, "owner", "/membership-changes", s.prefix+"grant-member", "0", map[string]any{"issuer": issuer, "subject": subject, "role": "operator", "active": true}, nil); err != nil {
		return scenarioObservation{}, err
	}
	write := func(role, name string) (scenarioObservation, error) {
		body := wire(map[string]string{"source_id": s.id(name), "kind": "document", "title": "SYNTHETIC session control", "text": "Session expiry control"})
		return s.request(ctx, role, "POST", "/sources", name, "0", "application/json", body, nil)
	}
	control, err := write("fresh", "control-source")
	if err != nil {
		return control, err
	}
	if control.Status != 200 {
		return control, fmt.Errorf("unexpired session for the same subject did not write: %d %s", control.Status, control.Code)
	}
	refused, err := write("expired", "refused-source")
	if err != nil {
		return refused, err
	}
	readback := map[string]scenarioObservation{}
	for _, name := range []string{"control-source", "refused-source"} {
		if readback[name], err = s.request(ctx, "owner", "GET", "/sources/"+s.id(name), "", "", "", nil, nil); err != nil {
			return refused, err
		}
	}
	if readback["control-source"].Status != 200 || readback["refused-source"].Status != 404 {
		return refused, fmt.Errorf("source readback after session control: control=%d refused=%d", readback["control-source"].Status, readback["refused-source"].Status)
	}
	refused.Evidence = map[string]any{"unexpired_control": control, "readback": readback}
	return refused, nil
}
