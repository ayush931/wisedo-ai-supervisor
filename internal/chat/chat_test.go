package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	tenantA  = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	siteA    = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
	convA    = "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"
	requestA = "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"
)

func TestInputBounds(t *testing.T) {
	for name, c := range map[string]struct {
		in   CreateInput
		want bool
	}{
		"title":                  {CreateInput{Title: "Line 2 shortfall"}, true},
		"200 multibyte runes":    {CreateInput{Title: strings.Repeat("é", MaxTitleRunes)}, true},
		"201 runes":              {CreateInput{Title: strings.Repeat("é", MaxTitleRunes+1)}, false},
		"empty":                  {CreateInput{Title: ""}, false},
		"whitespace only":        {CreateInput{Title: "   "}, false},
		"surrounding whitespace": {CreateInput{Title: " Line 2 "}, false},
		"nul":                    {CreateInput{Title: "a\x00b"}, false},
		"invalid utf-8":          {CreateInput{Title: "a\xffb"}, false},
	} {
		if got := c.in.Validate() == nil; got != c.want {
			t.Errorf("CreateInput %s: valid=%v want %v", name, got, c.want)
		}
	}
	for name, c := range map[string]struct {
		in   PostInput
		want bool
	}{
		"question":             {PostInput{RequestID: requestA, Text: "What did line 2 deliver last shift?"}, true},
		"4000 multibyte runes": {PostInput{RequestID: requestA, Text: strings.Repeat("é", MaxQuestionRunes)}, true},
		"4001 runes":           {PostInput{RequestID: requestA, Text: strings.Repeat("é", MaxQuestionRunes+1)}, false},
		"blank text":           {PostInput{RequestID: requestA, Text: " \n\t"}, false},
		"nul":                  {PostInput{RequestID: requestA, Text: "a\x00b"}, false},
		"missing request id":   {PostInput{Text: "hello"}, false},
		"nil request id":       {PostInput{RequestID: "00000000-0000-0000-0000-000000000000", Text: "hello"}, false},
		"uppercase request id": {PostInput{RequestID: strings.ToUpper(requestA), Text: "hello"}, false},
	} {
		if got := c.in.Validate() == nil; got != c.want {
			t.Errorf("PostInput %s: valid=%v want %v", name, got, c.want)
		}
	}
}

// A nil database proves malformed requests are refused before any transaction opens.
func TestServiceRejectsInvalidRequestsBeforeStorage(t *testing.T) {
	s := NewService(nil)
	ctx := context.Background()
	a := Actor{Issuer: "https://issuer.invalid", Subject: "subject", SessionHash: strings.Repeat("a", 64)}
	sc := Scope{TenantID: tenantA, SiteID: siteA}
	if _, err := s.Create(ctx, a, sc, CreateInput{Title: ""}, "key"); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty title: %v", err)
	}
	if _, err := s.Create(ctx, a, sc, CreateInput{Title: "ok"}, " key"); !errors.Is(err, ErrInvalid) {
		t.Errorf("non-canonical idempotency key: %v", err)
	}
	valid := PostInput{RequestID: requestA, Text: "question"}
	if _, err := s.Post(ctx, a, sc, convA, valid, "key", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing If-Match revision: %v", err)
	}
	if _, err := s.Post(ctx, a, sc, convA, PostInput{RequestID: requestA}, "key", 1); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty question: %v", err)
	}
	for name, c := range map[string]struct {
		limit  int
		cursor string
	}{"limit 0": {0, ""}, "limit 101": {101, ""}, "cursor zero": {10, "0"}, "cursor padded": {10, "01"}, "cursor text": {10, "x"}} {
		if _, err := s.Messages(ctx, a, sc, convA, c.limit, c.cursor); !errors.Is(err, ErrInvalid) {
			t.Errorf("messages %s: %v", name, err)
		}
	}
	if _, err := s.List(ctx, a, sc, 10, "not-a-uuid"); !errors.Is(err, ErrInvalid) {
		t.Errorf("list cursor: %v", err)
	}
	if _, err := s.Get(ctx, a, Scope{TenantID: "x", SiteID: siteA}, convA); !errors.Is(err, ErrNotFound) {
		t.Errorf("non-canonical tenant must be the non-disclosing 404: %v", err)
	}
	if _, err := s.Get(ctx, Actor{Issuer: "i", Subject: "s", SessionHash: "short"}, sc, convA); !errors.Is(err, ErrDenied) {
		t.Errorf("malformed session hash: %v", err)
	}
}
