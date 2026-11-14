// Package identity binds a verified OIDC issuer/subject to an opaque DB session.
// Threat boundaries: browser state requires a separate HttpOnly cookie; login
// attempts are single-use; provider tokens never persist; email is not identity.
// API callers must authorize current membership for every request, not cache it.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/text/unicode/norm"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/session"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/workstation"
)

var ErrUnauthenticated = errors.New("authentication required")
var ErrCSRF = errors.New("request rejected")
var ErrReauthenticationRequired = errors.New("recent authentication required")

type Config struct {
	Issuer, ClientID, ClientSecret, RedirectURI string
	SecureCookies                               bool
	// Sessions is the browser-session policy (WIS-290); the zero value means session.Default.
	Sessions session.Policy
	// Limit wraps the two unauthenticated routes, /auth/login and /auth/callback, which fetch
	// the provider and write login state for anyone (WIS-699). Nil leaves them unlimited.
	Limit func(http.Handler) http.Handler
}
type Principal struct {
	Issuer, Subject, CSRFToken string
	AuthenticatedAt            time.Time
	SessionHash                string `json:"-"`
}
type Service struct {
	DB                          *sql.DB
	Config                      Config
	HTTPClient                  *http.Client
	origin, cookie, loginCookie string
}

func New(_ context.Context, config Config, db *sql.DB, client *http.Client) (*Service, error) {
	for _, raw := range []string{config.Issuer, config.RedirectURI} {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid identity URL")
		}
		local := !config.SecureCookies && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "keycloak")
		if u.Scheme != "https" && !local {
			return nil, errors.New("identity URL requires HTTPS")
		}
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return nil, errors.New("identity client configuration required")
	}
	redirect, _ := url.Parse(config.RedirectURI)
	s := &Service{DB: db, Config: config, HTTPClient: client, origin: redirect.Scheme + "://" + redirect.Host, cookie: "supervisor", loginCookie: "supervisor-login"}
	if config.SecureCookies {
		s.cookie = "__Host-supervisor"
		s.loginCookie = "__Host-supervisor-login"
	}
	if s.HTTPClient == nil {
		s.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return s, nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func secret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *Service) cookieHeader(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteLaxMode})
}

// sessions is the configured session policy, or the WIS-290 default when none was configured.
func (s *Service) sessions() session.Policy { return s.Config.Sessions.OrDefault() }

// Sessions returns the session policy this identity service enforces, so every other service
// can enforce the same one. A nil service reports the default.
func (s *Service) Sessions() session.Policy {
	if s == nil {
		return session.Default()
	}
	return s.sessions()
}

func (s *Service) clearCookie(w http.ResponseWriter, name string) { s.cookieHeader(w, name, "", -1) }
func fail(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"request could not be completed"}`))
}

func (s *Service) audit(ctx context.Context, event, subject string) error {
	id, err := secret()
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO supervisor.identity_audit(id,event_type,subject,created_at) VALUES($1,$2,NULLIF($3,''),now())", id, event, subject)
	return err
}
func (s *Service) reject(ctx context.Context, w http.ResponseWriter, event string, status int) {
	if err := s.audit(ctx, event, ""); err != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	fail(w, status)
}

func (s *Service) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	var p Principal
	var authenticatedAt sql.NullTime
	token := cookieValue(r, s.cookie)
	err := s.DB.QueryRowContext(ctx, `UPDATE supervisor.browser_sessions SET last_seen_at=now()
 WHERE token_hash=$1 AND expires_at>now() AND last_seen_at>now()-make_interval(secs=>$2)
 RETURNING issuer,subject,csrf_token,auth_time`, hash(token), s.sessions().IdleSeconds()).Scan(&p.Issuer, &p.Subject, &p.CSRFToken, &authenticatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if auditErr := s.audit(ctx, "session_rejected", ""); auditErr != nil {
			return Principal{}, auditErr
		}
		return Principal{}, ErrUnauthenticated
	}
	if err == nil {
		p.SessionHash = hash(token)
		if authenticatedAt.Valid {
			p.AuthenticatedAt = authenticatedAt.Time
		}
	}
	return p, err
}

// RequireRecent accepts only persisted verified OIDC auth_time, not token iat or login time.
func (s *Service) RequireRecent(p Principal) error {
	now := time.Now()
	if !s.sessions().Recent(p.AuthenticatedAt, now) {
		return ErrReauthenticationRequired
	}
	return nil
}

func (s *Service) CheckCSRF(r *http.Request, p Principal) error {
	if r.Header.Get("Origin") != s.origin || p.CSRFToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.CSRFToken)) != 1 {
		return ErrCSRF
	}
	return nil
}
func (s *Service) Register(mux *http.ServeMux) {
	limit := s.Config.Limit
	if limit == nil {
		limit = func(h http.Handler) http.Handler { return h }
	}
	mux.Handle("GET /auth/login", limit(http.HandlerFunc(s.login)))
	mux.Handle("GET /auth/callback", limit(http.HandlerFunc(s.callback)))
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("POST /auth/reauthenticate", s.reauthenticate)
}

func (s *Service) provider(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	// Never follow a provider redirect into another origin while exchanging secrets.
	clone := *s.HTTPClient
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	ctx = oidc.ClientContext(ctx, &clone)
	provider, err := oidc.NewProvider(ctx, s.Config.Issuer)
	if err != nil {
		return nil, nil, err
	}
	var discovery struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, nil, err
	}
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, discovery.JWKSURL} {
		if !s.safeEndpoint(raw) {
			return nil, nil, errors.New("unsafe provider endpoint")
		}
	}
	return provider, &oauth2.Config{ClientID: s.Config.ClientID, ClientSecret: s.Config.ClientSecret, Endpoint: endpoint, RedirectURL: s.Config.RedirectURI, Scopes: []string{oidc.ScopeOpenID, oidc.ScopeProfile}}, nil
}

// safeEndpoint reports whether a discovered provider endpoint may be used: absolute, without
// userinfo or fragment, and HTTPS unless it is the local plain-HTTP development provider.
func (s *Service) safeEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "keycloak"
	return u.Scheme == "https" || (!s.Config.SecureCookies && u.Scheme == "http" && local)
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) { s.beginLogin(w, r, nil) }
func (s *Service) reauthenticate(w http.ResponseWriter, r *http.Request) {
	p, err := s.Authenticate(r.Context(), r)
	if err != nil {
		fail(w, Status(err))
		return
	}
	if err = s.CheckCSRF(r, p); err != nil {
		s.reject(r.Context(), w, "reauthentication_csrf_rejected", 403)
		return
	}
	s.beginLogin(w, r, &p)
}
func (s *Service) beginLogin(w http.ResponseWriter, r *http.Request, reauth *Principal) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_, config, err := s.provider(ctx)
	if err != nil {
		s.reject(ctx, w, "login_provider_unavailable", 503)
		return
	}
	values := make([]string, 4)
	for i := range values {
		values[i], err = secret()
		if err != nil {
			fail(w, 503)
			return
		}
	}
	state, browser, nonce, verifier := values[0], values[1], values[2], values[3]
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM supervisor.oidc_logins WHERE expires_at<=now()"); err != nil {
		fail(w, 503)
		return
	}
	var sessionHash, issuer, subject any
	var requestedAt any
	event := "login_started"
	if reauth != nil {
		sessionHash = reauth.SessionHash
		issuer = reauth.Issuer
		subject = reauth.Subject
		requestedAt = time.Now().UTC()
		event = "reauthentication_started"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.oidc_logins(state_hash,browser_hash,nonce,verifier,expires_at,reauth_session_hash,reauth_issuer,reauth_subject,reauth_requested_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes',$5,$6,$7,$8)`, hash(state), hash(browser), nonce, verifier, sessionHash, issuer, subject, requestedAt); err != nil {
		fail(w, 503)
		return
	}
	id, err := secret()
	if err != nil {
		fail(w, 503)
		return
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO supervisor.identity_audit(id,event_type,created_at) VALUES($1,$2,now())", id, event); err != nil {
		fail(w, 503)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, 503)
		return
	}
	s.cookieHeader(w, s.loginCookie, browser, 300)
	// Every sign-in shows the provider's form (WIS-713): a provider session left behind (sign out
	// never reached the provider, or a shared plant computer) must never sign the next person in
	// silently as the previous account.
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce), oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0")}
	target := config.AuthCodeURL(state, options...)
	if reauth != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"authorization_url": target})
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var nonce, verifier string
	var originalHash, originalIssuer, originalSubject sql.NullString
	var requestedAt sql.NullTime
	// DELETE RETURNING is the one-time state consumption authority, before token exchange.
	err := s.DB.QueryRowContext(ctx, `DELETE FROM supervisor.oidc_logins WHERE state_hash=$1 AND browser_hash=$2 AND expires_at>clock_timestamp() AND (reauth_session_hash IS NULL OR reauth_session_hash=$3) RETURNING nonce,verifier,reauth_session_hash,reauth_issuer,reauth_subject,reauth_requested_at`, hash(r.URL.Query().Get("state")), hash(cookieValue(r, s.loginCookie)), hash(cookieValue(r, s.cookie))).Scan(&nonce, &verifier, &originalHash, &originalIssuer, &originalSubject, &requestedAt)
	if errors.Is(err, sql.ErrNoRows) || r.URL.Query().Get("code") == "" {
		s.reject(ctx, w, "login_state_rejected", 401)
		return
	}
	if err != nil {
		fail(w, 503)
		return
	}
	provider, config, err := s.provider(ctx)
	if err != nil {
		s.reject(ctx, w, "login_provider_unavailable", 503)
		return
	}
	clone := *s.HTTPClient
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	ctx = oidc.ClientContext(ctx, &clone)
	token, err := config.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		s.reject(ctx, w, "login_exchange_rejected", 401)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		s.reject(ctx, w, "login_token_rejected", 401)
		return
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: s.Config.ClientID, SupportedSigningAlgs: []string{oidc.RS256}}).Verify(ctx, raw)
	if err != nil {
		s.reject(ctx, w, "login_token_rejected", 401)
		return
	}
	if err = validateClaims(verified, nonce, s.Config.ClientID); err != nil {
		s.reject(ctx, w, "login_claims_rejected", 401)
		return
	}
	authenticatedAt, err := verifiedAuthTime(verified)
	if err != nil {
		s.reject(ctx, w, "login_auth_time_rejected", 401)
		return
	}
	if originalHash.Valid {
		if verified.Issuer != originalIssuer.String || verified.Subject != originalSubject.String || !requestedAt.Valid || authenticatedAt.Before(requestedAt.Time.Truncate(time.Second)) || s.RequireRecent(Principal{AuthenticatedAt: authenticatedAt}) != nil {
			s.reject(ctx, w, "reauthentication_proof_rejected", 401)
			return
		}
	}
	session, err := secret()
	if err != nil {
		fail(w, 503)
		return
	}
	csrf, err := secret()
	if err != nil {
		fail(w, 503)
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503)
		return
	}
	defer tx.Rollback()
	createdAt, expiresAt := time.Now().UTC(), time.Now().UTC().Add(s.sessions().Absolute)
	event := "login_succeeded"
	if originalHash.Valid {
		var lastSeen time.Time
		err = tx.QueryRowContext(ctx, `SELECT created_at,expires_at,last_seen_at FROM supervisor.browser_sessions WHERE token_hash=$1 AND issuer=$2 AND subject=$3 AND expires_at>clock_timestamp() AND last_seen_at>clock_timestamp()-make_interval(secs=>$4) FOR UPDATE`, originalHash.String, originalIssuer.String, originalSubject.String, s.sessions().IdleSeconds()).Scan(&createdAt, &expiresAt, &lastSeen)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !s.sessions().Active(expiresAt, lastSeen, time.Now())) {
			_ = tx.Rollback()
			s.reject(ctx, w, "reauthentication_session_rejected", 401)
			return
		}
		if err != nil {
			fail(w, 503)
			return
		}
		event = "reauthentication_succeeded"
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", hash(cookieValue(r, s.cookie))); err != nil {
		fail(w, 503)
		return
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.browser_sessions(token_hash,subject,issuer,csrf_token,created_at,last_seen_at,expires_at,auth_time) VALUES($1,$2,$3,$4,$5,clock_timestamp(),$6,$7)`, hash(session), verified.Subject, s.Config.Issuer, csrf, createdAt, expiresAt, nullableTime(authenticatedAt)); err != nil {
		fail(w, 503)
		return
	}
	if err = s.storeDisplayName(ctx, tx, verified); err != nil {
		fail(w, 503)
		return
	}
	id, err := secret()
	if err != nil {
		fail(w, 503)
		return
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO supervisor.identity_audit(id,event_type,subject,created_at) VALUES($1,$2,$3,now())", id, event, verified.Subject); err != nil {
		fail(w, 503)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, 503)
		return
	}
	s.cookieHeader(w, s.cookie, session, max(1, int(time.Until(expiresAt).Seconds())))
	s.clearCookie(w, s.loginCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// storeDisplayName records the verified ID token's display name for every tenant the principal
// is an active member of (WIS-290; no acceptable claim stores NULL so a removed name clears).
// Names are cosmetic: a failed write (for example 043 not yet applied during a rolling deploy)
// rolls back to a savepoint, logs a fixed message without the name or claims, and the login
// continues. Only a failure to manage the savepoint itself aborts the login transaction.
func (s *Service) storeDisplayName(ctx context.Context, tx *sql.Tx, verified *oidc.IDToken) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT member_profile"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT set_config('supervisor.issuer',$1,true),set_config('supervisor.subject',$2,true)`, s.Config.Issuer, verified.Subject)
	if err == nil {
		_, err = tx.ExecContext(ctx, migrate.MemberProfileUpsert, s.Config.Issuer, verified.Subject, nullableText(verifiedDisplayName(verified)))
	}
	if err != nil {
		slog.Warn("member display name not stored; login continues", "code", "MEMBER_PROFILE_UNAVAILABLE")
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT member_profile")
		return err
	}
	_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT member_profile")
	return err
}

// displayClaims are the ID token's standard profile claims offered for display only.
type displayClaims struct {
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// displayName picks name, else preferred_username, and returns "" when neither is acceptable.
// The value is untrusted IdP text rendered in the UI: it is trimmed, and rejected when it is
// not valid UTF-8, holds a control, format (bidi override), line/paragraph separator,
// private-use, noncharacter or invisible (unsafeRune) code point, exceeds 128 runes, or looks executable to a
// template, script URL or spreadsheet (workstation.Executable), or carries a fake subject tag.
func displayName(c displayClaims) string {
	for _, raw := range []string{c.Name, c.PreferredUsername} {
		v := strings.TrimSpace(raw)
		if v != "" && utf8.ValidString(v) && utf8.RuneCountInString(v) <= 128 && !workstation.Executable(v) && strings.IndexFunc(v, unsafeRune) < 0 && !reservedLabel(v) && !tagLike(v) {
			return v
		}
	}
	return ""
}

// reservedLabel rejects names that would read as one of the UI's own attribution labels
// ("you", "unknown", "member 5c89…"), so a profile name cannot impersonate them.
func reservedLabel(v string) bool {
	// NFKC folds compatibility lookalikes such as fullwidth "Ｙｏｕ" onto the plain label.
	l := strings.ToLower(norm.NFKC.String(v))
	return l == "you" || l == "unknown" || strings.HasPrefix(l, "member")
}

// unsafeRune also rejects code points that render as nothing yet are not format characters:
// the default-ignorable letters (Hangul fillers U+115F, U+1160, U+3164, U+FFA0, the combining
// grapheme joiner U+034F and the rest of Other_Default_Ignorable_Code_Point), variation
// selectors and the braille blank U+2800. Without this "You\u3164" displays as "You" while
// passing reservedLabel, and a filler alone displays as an empty name. Emoji that need a
// variation selector are refused too; the name then falls back like any other unsafe value.
// tagSeparator matches a middle dot or a lookalike (after NFKC, which folds the Greek ano
// teleia onto U+00B7) next to whitespace: the shape of the UI's own " · 1a2b" subject tag.
var tagSeparator = regexp.MustCompile(`\s[\x{00B7}\x{2022}\x{2219}\x{22C5}\x{2027}\x{30FB}\x{FF65}\x{2E31}\x{16EB}]|[\x{00B7}\x{2022}\x{2219}\x{22C5}\x{2027}\x{30FB}\x{FF65}\x{2E31}\x{16EB}]\s`)

// tagLike rejects a name that carries its own fake subject tag: "Shift Approver · 9f3c" would
// render as "Shift Approver · 9f3c · 1a2b" and pass for the real member. A middle dot inside a
// word (Catalan "l·l") is kept.
func tagLike(v string) bool { return tagSeparator.MatchString(norm.NFKC.String(v)) }

func unsafeRune(r rune) bool {
	noncharacter := (r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
	invisible := r == 0x2800 || unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
	return noncharacter || invisible || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Co)
}

func verifiedDisplayName(token *oidc.IDToken) string {
	var c displayClaims
	if token.Claims(&c) != nil {
		return ""
	}
	return displayName(c)
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
func verifiedAuthTime(token *oidc.IDToken) (time.Time, error) {
	var claims struct {
		AuthTime json.RawMessage `json:"auth_time"`
	}
	if err := token.Claims(&claims); err != nil {
		return time.Time{}, err
	}
	if len(claims.AuthTime) == 0 {
		return time.Time{}, nil
	}
	var seconds int64
	if string(claims.AuthTime) == "null" {
		return time.Time{}, ErrUnauthenticated
	}
	if err := json.Unmarshal(claims.AuthTime, &seconds); err != nil || seconds <= 0 {
		return time.Time{}, ErrUnauthenticated
	}
	authenticated := time.Unix(seconds, 0).UTC()
	if authenticated.After(time.Now()) || authenticated.After(token.IssuedAt) {
		return time.Time{}, ErrUnauthenticated
	}
	return authenticated, nil
}

func validateClaims(token *oidc.IDToken, nonce, clientID string) error {
	var claims struct {
		Nonce string          `json:"nonce"`
		AZP   string          `json:"azp"`
		Exp   json.RawMessage `json:"exp"`
		Iat   json.RawMessage `json:"iat"`
	}
	if err := token.Claims(&claims); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 || nonce == "" || token.Subject == "" || len(token.Subject) > 255 {
		return ErrUnauthenticated
	}
	if (claims.AZP != "" && claims.AZP != clientID) || (len(token.Audience) > 1 && claims.AZP != clientID) {
		return ErrUnauthenticated
	}
	for _, value := range []json.RawMessage{claims.Exp, claims.Iat} {
		var integer int64
		if len(value) == 0 || string(value) == "null" {
			return ErrUnauthenticated
		}
		if err := json.Unmarshal(value, &integer); err != nil {
			return ErrUnauthenticated
		}
	}
	if token.IssuedAt.After(time.Now()) {
		return ErrUnauthenticated
	}
	return nil
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.Authenticate(ctx, r)
	if errors.Is(err, ErrUnauthenticated) {
		fail(w, 401)
		return
	}
	if err != nil {
		fail(w, 503)
		return
	}
	if err = s.CheckCSRF(r, p); err != nil {
		s.reject(ctx, w, "csrf_rejected", 403)
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM supervisor.browser_sessions WHERE token_hash=$1", hash(cookieValue(r, s.cookie))); err != nil {
		fail(w, 503)
		return
	}
	id, err := secret()
	if err != nil {
		fail(w, 503)
		return
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO supervisor.identity_audit(id,event_type,subject,created_at) VALUES($1,'logout',$2,now())", id, p.Subject); err != nil {
		fail(w, 503)
		return
	}
	if err = tx.Commit(); err != nil {
		fail(w, 503)
		return
	}
	s.clearCookie(w, s.cookie)
	// The Supervisor session is gone. Also end the provider session (RP-initiated logout), so the
	// next sign-in cannot reuse it (WIS-713). No ID token is stored (provider tokens never persist),
	// so the URL carries client_id only and the provider asks the person to confirm. Without a
	// usable end_session_endpoint this stays a local sign out, recorded so operators can see the
	// provider session survived; the next sign-in still forces the provider's form (prompt=login).
	target, skipped := s.endSessionURL(ctx)
	if target != "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"end_session_url": target})
		return
	}
	// Best effort: the local sign out already committed, so a failed record never turns it into an
	// error the browser would read as "still signed in". The request context may be spent by an
	// unreachable provider, hence a fresh short one.
	slog.Warn("provider session not ended at sign out; local sign out only", "code", skipped)
	actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer acancel()
	if err := s.audit(actx, "provider_logout_skipped", p.Subject); err != nil {
		slog.Warn("provider_logout_skipped audit not written", "code", "IDENTITY_AUDIT_UNAVAILABLE")
	}
	w.WriteHeader(http.StatusNoContent)
}

// endSessionURL is the provider's RP-initiated logout URL (client_id and post_logout_redirect_uri,
// no id_token_hint), or "" and a stable reason code when the provider is unreachable or publishes
// no safe end_session_endpoint.
func (s *Service) endSessionURL(ctx context.Context) (string, string) {
	provider, _, err := s.provider(ctx)
	if err != nil {
		return "", "PROVIDER_UNAVAILABLE"
	}
	var discovery struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if provider.Claims(&discovery) != nil || discovery.EndSession == "" {
		return "", "END_SESSION_NOT_PUBLISHED"
	}
	if !s.safeEndpoint(discovery.EndSession) {
		return "", "END_SESSION_UNSAFE"
	}
	u, _ := url.Parse(discovery.EndSession)
	q := u.Query()
	q.Set("client_id", s.Config.ClientID)
	q.Set("post_logout_redirect_uri", s.origin+"/")
	u.RawQuery = q.Encode()
	return u.String(), ""
}

// Status lets API handlers hide internal/provider details while retaining errors
// for internal categorization. No credential-bearing error strings are returned.
func Status(err error) int {
	if errors.Is(err, ErrReauthenticationRequired) {
		return 428
	}
	if errors.Is(err, ErrUnauthenticated) {
		return 401
	}
	if errors.Is(err, ErrCSRF) {
		return 403
	}
	return 503
}
