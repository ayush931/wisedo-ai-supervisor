// supervisor-seed provisions an explicit synthetic local fixture, never a customer site.
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
	database                                                       *pgx.ConnConfig
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
func loadConfig(getenv func(string) string) (config, error) {
	var c config
	if getenv("SUPERVISOR_ENVIRONMENT") != "local" {
		return c, errors.New("synthetic fixture requires explicit local environment")
	}
	required := []string{"SUPERVISOR_LOCAL_KEYCLOAK", "SUPERVISOR_TEST_ADMIN_URL", "SUPERVISOR_LOCAL_ADMIN", "SUPERVISOR_LOCAL_ADMIN_PASSWORD", "SUPERVISOR_LOCAL_USER_PASSWORD", "SUPERVISOR_OIDC_CLIENT_SECRET", "SUPERVISOR_PUBLIC_ORIGIN"}
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
	rawDatabase := getenv("SUPERVISOR_TEST_ADMIN_URL")
	parsed, err := url.Parse(rawDatabase)
	if err != nil || !loopback(parsed.Hostname()) || (parsed.Scheme != "postgresql" && parsed.Scheme != "postgres") || parsed.Fragment != "" {
		return c, errors.New("fixture database must be a loopback PostgreSQL URL")
	}
	c.database, err = pgx.ParseConfig(rawDatabase)
	if err != nil {
		return c, errors.New("invalid fixture database configuration")
	}
	// pgx permits query/environment overrides and multiple fallback endpoints; verify effective hosts.
	if !loopback(c.database.Host) {
		return c, errors.New("fixture database effective host must be loopback")
	}
	for _, fallback := range c.database.Fallbacks {
		if !loopback(fallback.Host) {
			return c, errors.New("fixture database fallback must be loopback")
		}
	}
	c.database.ConnectTimeout = 5 * time.Second
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
func seedIdentity(ctx context.Context, c config) (string, error) {
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
		return "", err
	}
	if token.AccessToken == "" || len(token.AccessToken) > 16384 || strings.ContainsAny(token.AccessToken, "\r\n") {
		return "", errors.New("invalid local identity token")
	}
	k.token = token.AccessToken
	if err := k.json(ctx, http.MethodPost, "/admin/realms", map[string]any{"realm": "supervisor-local", "enabled": true, "sslRequired": "none"}, nil, http.StatusCreated, http.StatusConflict); err != nil {
		return "", err
	}
	var clients []struct {
		ID string `json:"id"`
	}
	if err := k.json(ctx, http.MethodGet, realmPath+"/clients?clientId=supervisor", nil, &clients, http.StatusOK); err != nil {
		return "", err
	}
	if len(clients) > 1 {
		return "", errors.New("ambiguous local identity client")
	}
	// post.logout.redirect.uris lets sign out end the Keycloak session and return to the app (WIS-713).
	clientBody := map[string]any{"clientId": "supervisor", "enabled": true, "publicClient": false, "secret": c.clientSecret, "standardFlowEnabled": true, "directAccessGrantsEnabled": false, "redirectUris": []string{c.origin + "/auth/callback"}, "attributes": map[string]string{"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": c.origin + "/"}}
	method, path := http.MethodPost, realmPath+"/clients"
	if len(clients) == 1 {
		id, err := uuid.Parse(clients[0].ID)
		if err != nil {
			return "", errors.New("invalid local client identifier")
		}
		method, path = http.MethodPut, path+"/"+id.String()
	}
	if err := k.json(ctx, method, path, clientBody, nil, http.StatusCreated, http.StatusNoContent); err != nil {
		return "", err
	}
	reviewer := map[string]any{"username": "foundation-reviewer", "enabled": true, "firstName": "Vikram", "lastName": "Rao", "email": "vikram.rao@example.invalid", "emailVerified": true}
	if err := k.json(ctx, http.MethodPost, realmPath+"/users", reviewer, nil, http.StatusCreated, http.StatusConflict); err != nil {
		return "", err
	}
	var users []struct {
		ID string `json:"id"`
	}
	if err := k.json(ctx, http.MethodGet, realmPath+"/users?username=foundation-reviewer&exact=true", nil, &users, http.StatusOK); err != nil {
		return "", err
	}
	if len(users) != 1 {
		return "", errors.New("synthetic reviewer lookup must resolve exactly one user")
	}
	subject, err := uuid.Parse(users[0].ID)
	if err != nil {
		return "", errors.New("invalid synthetic reviewer identifier")
	}
	password := map[string]any{"type": "password", "value": c.userPassword, "temporary": false}
	if err = k.json(ctx, http.MethodPut, realmPath+"/users/"+subject.String()+"/reset-password", password, nil, http.StatusNoContent); err != nil {
		return "", err
	}
	return subject.String(), nil
}
func seedDatabase(ctx context.Context, c config, subject string) error {
	db := stdlib.OpenDB(*c.database)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("local fixture database unavailable")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = '15s'"); err != nil {
		return errors.New("local fixture database transaction failed")
	}
	if _, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return errors.New("local fixture database transaction failed")
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO supervisor.tenants(id,name) VALUES($1,'Sahyadri Precision Components') ON CONFLICT DO NOTHING`, []any{tenantID}},
		{`INSERT INTO supervisor.sites(tenant_id,id,name) VALUES($1,$2,'Chakan Plant') ON CONFLICT DO NOTHING`, []any{tenantID, siteID}},
		{`INSERT INTO supervisor.memberships(tenant_id,site_id,issuer,subject,role) VALUES($1,$2,$3,$4,'owner') ON CONFLICT DO NOTHING`, []any{tenantID, siteID, c.base + "/realms/supervisor-local", subject}},
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return errors.New("local fixture database transaction failed")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("local fixture database commit failed")
	}
	return nil
}
func run(ctx context.Context, getenv func(string) string) error {
	c, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	subject, err := seedIdentity(ctx, c)
	if err != nil {
		return err
	}
	return seedDatabase(ctx, c, subject)
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "Synthetic local fixture failed:", err)
		os.Exit(1)
	}
	fmt.Println("Synthetic local fixture ready; this is not customer commissioning.")
}
