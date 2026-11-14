package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCSRFRequiresOriginAndSecret(t *testing.T) {
	s, err := New(context.Background(), Config{Issuer: "https://idp.example", ClientID: "supervisor", ClientSecret: "test", RedirectURI: "https://app.example/auth/callback", SecureCookies: true}, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{CSRFToken: "fixture-token"}
	r, _ := http.NewRequest("POST", "https://app.example/action", nil)
	if !errors.Is(s.CheckCSRF(r, p), ErrCSRF) {
		t.Fatal("missing CSRF accepted")
	}
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("X-CSRF-Token", p.CSRFToken)
	if err := s.CheckCSRF(r, p); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "https://evil.example")
	if !errors.Is(s.CheckCSRF(r, p), ErrCSRF) {
		t.Fatal("foreign origin accepted")
	}
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("X-CSRF-Token", "other-token")
	if !errors.Is(s.CheckCSRF(r, p), ErrCSRF) {
		t.Fatal("wrong token accepted from the right origin")
	}
	r.Header.Del("X-CSRF-Token")
	if !errors.Is(s.CheckCSRF(r, p), ErrCSRF) {
		t.Fatal("missing token accepted from the right origin")
	}
	// A session without a CSRF secret must not match an equally empty header.
	if !errors.Is(s.CheckCSRF(r, Principal{}), ErrCSRF) {
		t.Fatal("empty session secret matched an empty header")
	}
}

func TestConfigRejectsUnsafeIdentityURLs(t *testing.T) {
	for _, raw := range []string{"http://idp.example", "https://user:secret@idp.example", "https://idp.example?redirect=evil", "https://idp.example#fragment"} {
		_, err := New(context.Background(), Config{Issuer: raw, ClientID: "supervisor", ClientSecret: "test", RedirectURI: "https://app.example/auth/callback", SecureCookies: true}, nil, http.DefaultClient)
		if err == nil {
			t.Fatalf("unsafe issuer accepted: %s", raw)
		}
	}
}

func TestRecentAuthenticationRequiresVerifiedTime(t *testing.T) {
	s := Service{}
	for _, value := range []time.Time{{}, time.Now().Add(-16 * time.Minute), time.Now().Add(time.Minute)} {
		if !errors.Is(s.RequireRecent(Principal{AuthenticatedAt: value}), ErrReauthenticationRequired) {
			t.Fatalf("unproven freshness accepted %v", value)
		}
	}
	if err := s.RequireRecent(Principal{AuthenticatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
}

// WIS-290: the display name is untrusted IdP text shown in the UI. name wins, then
// preferred_username, then nothing; unsafe values fall through instead of being stored.
func TestDisplayNameFallsBackAndRejectsUnsafeText(t *testing.T) {
	long := strings.Repeat("é", 129)
	cases := []struct {
		name string
		in   displayClaims
		want string
	}{
		{"name preferred", displayClaims{Name: "Shift Lead", PreferredUsername: "shift-lead"}, "Shift Lead"},
		{"trimmed", displayClaims{Name: "  Operator One \t"}, "Operator One"},
		{"exactly 128 runes", displayClaims{Name: long[:len(long)-len("é")]}, long[:len(long)-len("é")]},
		{"empty name falls back", displayClaims{Name: "   ", PreferredUsername: "operator-1"}, "operator-1"},
		{"control char falls back", displayClaims{Name: "Op\x00One", PreferredUsername: "operator-1"}, "operator-1"},
		{"newline falls back", displayClaims{Name: "Op\nOne", PreferredUsername: "operator-1"}, "operator-1"},
		{"bidi override falls back", displayClaims{Name: "Op\u202eenO", PreferredUsername: "operator-1"}, "operator-1"},
		{"overlong falls back", displayClaims{Name: long, PreferredUsername: "operator-1"}, "operator-1"},
		{"invalid utf8 falls back", displayClaims{Name: "Op\xffOne", PreferredUsername: "operator-1"}, "operator-1"},
		{"template falls back", displayClaims{Name: "{{constructor}}", PreferredUsername: "operator-1"}, "operator-1"},
		{"formula falls back", displayClaims{Name: "=HYPERLINK(1)", PreferredUsername: "operator-1"}, "operator-1"},
		{"script url falls back", displayClaims{Name: "javascript:alert(1)", PreferredUsername: "operator-1"}, "operator-1"},
		{"line separator falls back", displayClaims{Name: "Op\u2028One", PreferredUsername: "operator-1"}, "operator-1"},
		{"paragraph separator falls back", displayClaims{Name: "Op\u2029One", PreferredUsername: "operator-1"}, "operator-1"},
		{"private use falls back", displayClaims{Name: "Op\ue000One", PreferredUsername: "operator-1"}, "operator-1"},
		{"noncharacter FDD0 falls back", displayClaims{Name: "Op\ufdd0One", PreferredUsername: "operator-1"}, "operator-1"},
		{"noncharacter FFFE falls back", displayClaims{Name: "Op\ufffeOne", PreferredUsername: "operator-1"}, "operator-1"},
		{"noncharacter 1FFFF falls back", displayClaims{Name: "Op\U0001ffffOne", PreferredUsername: "operator-1"}, "operator-1"},
		{"accented and CJK accepted", displayClaims{Name: "José 山田"}, "José 山田"},
		// Security review: names must not impersonate the UI's own attribution labels.
		{"reserved you falls back", displayClaims{Name: "You", PreferredUsername: "operator-1"}, "operator-1"},
		{"reserved unknown falls back", displayClaims{Name: "unknown", PreferredUsername: "operator-1"}, "operator-1"},
		{"reserved member prefix falls back", displayClaims{Name: "member 5c897bf1", PreferredUsername: "operator-1"}, "operator-1"},
		{"fullwidth you falls back", displayClaims{Name: "Ｙｏｕ", PreferredUsername: "operator-1"}, "operator-1"},
		{"reserved in both is none", displayClaims{Name: "you", PreferredUsername: "Member"}, ""},
		// Invisible letters render as nothing: "You" plus a Hangul filler displayed as "You" and
		// passed the reserved check; a filler alone displayed as an empty name.
		{"hangul filler after you falls back", displayClaims{Name: "You\u3164", PreferredUsername: "operator-1"}, "operator-1"},
		{"hangul choseong filler falls back", displayClaims{Name: "\u115fAna", PreferredUsername: "operator-1"}, "operator-1"},
		{"hangul jungseong filler falls back", displayClaims{Name: "Ana\u1160", PreferredUsername: "operator-1"}, "operator-1"},
		{"halfwidth hangul filler alone falls back", displayClaims{Name: "\uffa0", PreferredUsername: "operator-1"}, "operator-1"},
		{"variation selector after unknown falls back", displayClaims{Name: "unknown\ufe0f", PreferredUsername: "operator-1"}, "operator-1"},
		{"combining grapheme joiner falls back", displayClaims{Name: "You\u034f", PreferredUsername: "operator-1"}, "operator-1"},
		{"braille blank falls back", displayClaims{Name: "\u2800", PreferredUsername: "operator-1"}, "operator-1"},
		{"visible hangul accepted", displayClaims{Name: "김민준"}, "김민준"},
		// A name carrying its own fake subject tag would read as another member's label.
		{"fake subject tag falls back", displayClaims{Name: "Shift Approver · 9f3c", PreferredUsername: "operator-1"}, "operator-1"},
		{"fake tag without trailing space falls back", displayClaims{Name: "Shift Approver ·9f3c", PreferredUsername: "operator-1"}, "operator-1"},
		{"bullet lookalike tag falls back", displayClaims{Name: "Shift Approver • 9f3c", PreferredUsername: "operator-1"}, "operator-1"},
		{"katakana middle dot tag falls back", displayClaims{Name: "Shift Approver ・ 9f3c", PreferredUsername: "operator-1"}, "operator-1"},
		{"greek ano teleia folds to middle dot", displayClaims{Name: "Shift Approver \u0387 9f3c", PreferredUsername: "operator-1"}, "operator-1"},
		{"catalan middle dot inside a word accepted", displayClaims{Name: "Col·legi"}, "Col·legi"},
		{"name containing member mid-word accepted", displayClaims{Name: "Team member Ana"}, "Team member Ana"},
		{"both unsafe is none", displayClaims{Name: "${x}", PreferredUsername: "\x07"}, ""},
		{"absent is none", displayClaims{}, ""},
	}
	for _, c := range cases {
		if got := displayName(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
