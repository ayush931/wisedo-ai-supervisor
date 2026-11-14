// supervisor-demo-seed adds a synthetic WIS-520 workstation dataset to the local fixture that
// supervisor-seed provisions. It is local-only demo data, never a customer site.
//
// The configuration guards and the Keycloak admin client duplicate supervisor-seed's so that
// command stays byte-identical; keep the two in step.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	tenantID  = "00000000-0000-4000-8000-000000000290"
	siteID    = "00000000-0000-4000-8000-000000000291"
	realmPath = "/admin/realms/supervisor-local"
)

type config struct {
	base, origin, admin, adminPassword, userPassword, clientSecret string
	database, runtime                                              *pgx.ConnConfig
}

func loopback(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
func localURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || !loopback(parsed.Hostname()) ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("fixture URLs require a literal loopback address without credentials, path or query")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// localDatabase accepts only a PostgreSQL URL whose effective host and every fallback is loopback.
func localDatabase(raw string) (*pgx.ConnConfig, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !loopback(parsed.Hostname()) || (parsed.Scheme != "postgresql" && parsed.Scheme != "postgres") || parsed.Fragment != "" {
		return nil, errors.New("fixture database must be a loopback PostgreSQL URL")
	}
	c, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("invalid fixture database configuration")
	}
	if !loopback(c.Host) {
		return nil, errors.New("fixture database effective host must be loopback")
	}
	for _, fallback := range c.Fallbacks {
		if !loopback(fallback.Host) {
			return nil, errors.New("fixture database fallback must be loopback")
		}
	}
	c.ConnectTimeout = 5 * time.Second
	return c, nil
}
func loadConfig(getenv func(string) string) (config, error) {
	var c config
	if getenv("SUPERVISOR_ENVIRONMENT") != "local" {
		return c, errors.New("synthetic fixture requires explicit local environment")
	}
	required := []string{"SUPERVISOR_LOCAL_KEYCLOAK", "SUPERVISOR_TEST_ADMIN_URL", "SUPERVISOR_DATABASE_URL", "SUPERVISOR_LOCAL_ADMIN", "SUPERVISOR_LOCAL_ADMIN_PASSWORD", "SUPERVISOR_LOCAL_USER_PASSWORD", "SUPERVISOR_OIDC_CLIENT_SECRET", "SUPERVISOR_PUBLIC_ORIGIN"}
	for _, key := range required {
		if getenv(key) == "" {
			return c, fmt.Errorf("required fixture setting is missing: %s", key)
		}
	}
	var err error
	if c.base, err = localURL(getenv("SUPERVISOR_LOCAL_KEYCLOAK")); err != nil {
		return c, err
	}
	if c.origin, err = localURL(getenv("SUPERVISOR_PUBLIC_ORIGIN")); err != nil {
		return c, err
	}
	if c.database, err = localDatabase(getenv("SUPERVISOR_TEST_ADMIN_URL")); err != nil {
		return c, err
	}
	if c.runtime, err = localDatabase(getenv("SUPERVISOR_DATABASE_URL")); err != nil {
		return c, err
	}
	c.admin = getenv("SUPERVISOR_LOCAL_ADMIN")
	c.adminPassword = getenv("SUPERVISOR_LOCAL_ADMIN_PASSWORD")
	c.userPassword = getenv("SUPERVISOR_LOCAL_USER_PASSWORD")
	c.clientSecret = getenv("SUPERVISOR_OIDC_CLIENT_SECRET")
	return c, nil
}

type keycloak struct {
	client      *http.Client
	base, token string
}

func (k *keycloak) request(ctx context.Context, method, path, contentType string, body io.Reader, result any, accepted ...int) error {
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, body)
	if err != nil {
		return errors.New("cannot construct local identity request")
	}
	req.Header.Set("Content-Type", contentType)
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	response, err := k.client.Do(req)
	if err != nil {
		return errors.New("local identity request failed")
	}
	defer response.Body.Close()
	ok := false
	for _, status := range accepted {
		if response.StatusCode == status {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("local identity rejected fixture request (HTTP %d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid or oversized local identity response")
	}
	if result != nil {
		if err = json.Unmarshal(data, result); err != nil {
			return errors.New("invalid local identity response")
		}
	}
	return nil
}
func (k *keycloak) json(ctx context.Context, method, path string, payload, result any, accepted ...int) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return errors.New("cannot encode fixture request")
		}
		body = bytes.NewReader(encoded)
	}
	return k.request(ctx, method, path, "application/json", body, result, accepted...)
}
func (k *keycloak) subject(ctx context.Context, username string) (string, error) {
	var users []struct {
		ID string `json:"id"`
	}
	if err := k.json(ctx, http.MethodGet, realmPath+"/users?exact=true&username="+url.QueryEscape(username), nil, &users, http.StatusOK); err != nil {
		return "", err
	}
	if len(users) != 1 {
		return "", fmt.Errorf("synthetic user %s must resolve exactly one user (run supervisor-seed first)", username)
	}
	id, err := uuid.Parse(users[0].ID)
	if err != nil {
		return "", errors.New("invalid synthetic user identifier")
	}
	return id.String(), nil
}

// seedIdentity creates the demo users (existing ones are kept, their password reset) and
// returns every demo username's subject, including the supervisor-seed owner.
func seedIdentity(ctx context.Context, c config) (map[string]string, error) {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 4,
	}
	defer transport.CloseIdleConnections()
	k := keycloak{base: c.base, client: &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	credentials := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {c.admin}, "password": {c.adminPassword}}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := k.request(ctx, http.MethodPost, "/realms/master/protocol/openid-connect/token", "application/x-www-form-urlencoded", strings.NewReader(credentials.Encode()), &token, http.StatusOK); err != nil {
		return nil, err
	}
	if token.AccessToken == "" || len(token.AccessToken) > 16384 || strings.ContainsAny(token.AccessToken, "\r\n") {
		return nil, errors.New("invalid local identity token")
	}
	k.token = token.AccessToken
	subjects := map[string]string{}
	var err error
	if subjects[ownerUsername], err = k.subject(ctx, ownerUsername); err != nil {
		return nil, err
	}
	type named struct{ username, first, last string }
	var everyone []named
	for _, u := range demoUsers {
		everyone = append(everyone, named{u.username, demoNames[u.username][0], demoNames[u.username][1]})
	}
	for _, p := range pilotTenants {
		for _, u := range p.users {
			everyone = append(everyone, named{u.username, u.first, u.last})
		}
	}
	for _, u := range everyone {
		user := map[string]any{"username": u.username, "enabled": true, "firstName": u.first, "lastName": u.last, "email": u.username + "@example.invalid", "emailVerified": true}
		if err = k.json(ctx, http.MethodPost, realmPath+"/users", user, nil, http.StatusCreated, http.StatusConflict); err != nil {
			return nil, err
		}
		if subjects[u.username], err = k.subject(ctx, u.username); err != nil {
			return nil, err
		}
		// An existing user (409 above) keeps its old name; set it so the next login carries it
		// to the member directory (WIS-290).
		if err = k.json(ctx, http.MethodPut, realmPath+"/users/"+subjects[u.username], user, nil, http.StatusNoContent); err != nil {
			return nil, err
		}
		password := map[string]any{"type": "password", "value": c.userPassword, "temporary": false}
		if err = k.json(ctx, http.MethodPut, realmPath+"/users/"+subjects[u.username]+"/reset-password", password, nil, http.StatusNoContent); err != nil {
			return nil, err
		}
	}
	return subjects, nil
}
func run(ctx context.Context, getenv func(string) string) (demoResult, error) {
	c, err := loadConfig(getenv)
	if err != nil {
		return demoResult{}, err
	}
	subjects, err := seedIdentity(ctx, c)
	if err != nil {
		return demoResult{}, err
	}
	control := stdlib.OpenDB(*c.database)
	defer control.Close()
	runtime := &store.Store{DB: stdlib.OpenDB(*c.runtime)}
	defer runtime.Close()
	issuer := c.base + "/realms/supervisor-local"
	result, err := buildDemo(ctx, demoScope{control: control, runtime: runtime, tenant: tenantID, site: siteID,
		issuer: issuer, origin: c.origin, clientSecret: c.clientSecret, subjects: subjects, now: time.Now})
	if err != nil {
		return result, err
	}
	return result, seedPilotTenants(ctx, control, issuer, subjects)
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := run(ctx, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Synthetic WIS-520 demo failed:", err)
		os.Exit(1)
	}
	origin, _ := localURL(os.Getenv("SUPERVISOR_PUBLIC_ORIGIN"))
	site := origin + "/sites/" + siteID
	fmt.Println("Synthetic WIS-520 demo ready; this is not customer data.")
	fmt.Println("Site:               ", siteID)
	fmt.Println("Workstation press-01:", site+"/workstations/"+result.Press)
	fmt.Println("Workstation weld-01: ", site+"/workstations/"+result.Weld)
	fmt.Println("Shift plan:         ", site+"/shifts/"+result.Plan)
	fmt.Println("Shortfall case:     ", site+"/cases/"+result.Case)
	fmt.Println("Users (password from SUPERVISOR_LOCAL_USER_PASSWORD):")
	fmt.Println("  " + ownerUsername + " (owner)")
	for _, u := range demoUsers {
		fmt.Printf("  %s (%s)\n", u.username, u.role)
	}
	for _, p := range pilotTenants {
		fmt.Printf("WIS-631 pilot site %s (%s):\n", p.siteName, p.site)
		for _, u := range p.users {
			fmt.Printf("  %s (%s)\n", u.username, u.role)
		}
	}
}
