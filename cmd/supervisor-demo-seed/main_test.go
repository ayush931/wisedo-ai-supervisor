package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigRefusesNonLocalTargets(t *testing.T) {
	base := map[string]string{
		"SUPERVISOR_ENVIRONMENT": "local", "SUPERVISOR_LOCAL_KEYCLOAK": "http://127.0.0.1:58077", "SUPERVISOR_PUBLIC_ORIGIN": "http://127.0.0.1:58176",
		"SUPERVISOR_TEST_ADMIN_URL": "postgresql://admin:x@127.0.0.1:55477/supervisor", "SUPERVISOR_DATABASE_URL": "postgresql://runtime:x@127.0.0.1:55477/supervisor",
		"SUPERVISOR_LOCAL_ADMIN": "local-admin", "SUPERVISOR_LOCAL_ADMIN_PASSWORD": "x", "SUPERVISOR_LOCAL_USER_PASSWORD": "x", "SUPERVISOR_OIDC_CLIENT_SECRET": "x",
	}
	for name, tc := range map[string]struct{ key, value, want string }{
		"accepted":             {},
		"production":           {"SUPERVISOR_ENVIRONMENT", "production", "explicit local environment"},
		"missing runtime":      {"SUPERVISOR_DATABASE_URL", "", "SUPERVISOR_DATABASE_URL"},
		"remote runtime":       {"SUPERVISOR_DATABASE_URL", "postgresql://runtime:x@db.example.com/supervisor", "loopback PostgreSQL"},
		"runtime host query":   {"SUPERVISOR_DATABASE_URL", "postgresql://runtime:x@127.0.0.1/supervisor?host=db.example.com", "effective host"},
		"remote admin":         {"SUPERVISOR_TEST_ADMIN_URL", "postgresql://admin:x@10.0.0.5/supervisor", "loopback PostgreSQL"},
		"named keycloak":       {"SUPERVISOR_LOCAL_KEYCLOAK", "http://localhost:58077", "literal loopback"},
		"origin with userinfo": {"SUPERVISOR_PUBLIC_ORIGIN", "http://u:p@127.0.0.1:58176", "literal loopback"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			if tc.key != "" {
				env[tc.key] = tc.value
			}
			_, err := loadConfig(func(k string) string { return env[k] })
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if err != nil && strings.Contains(err.Error(), "x@") {
				t.Fatal("error leaks a credential")
			}
		})
	}
}

func TestShiftWindowContainsNowInKolkataOffsets(t *testing.T) {
	for _, tc := range []struct{ now, start string }{
		{"2026-10-21T00:29:59Z", "2026-10-20T22:00:00+05:30"}, // 05:59:59 IST belongs to the night shift
		{"2026-10-21T00:30:00Z", "2026-10-21T06:00:00+05:30"},
		{"2026-10-21T10:00:00Z", "2026-10-21T14:00:00+05:30"},
		{"2026-10-21T18:00:00Z", "2026-10-21T22:00:00+05:30"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		start, end := shiftWindow(now)
		if start.Format(time.RFC3339) != tc.start || end.Sub(start) != 8*time.Hour || now.Before(start) || !now.Before(end) {
			t.Fatalf("%s: window %s..%s want start %s", tc.now, start.Format(time.RFC3339), end.Format(time.RFC3339), tc.start)
		}
	}
}
