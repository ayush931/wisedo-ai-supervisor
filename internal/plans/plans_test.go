package plans

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

const (
	spanA = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	spanB = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
)

func TestCanonicalSpansAreSortedDistinctAndBounded(t *testing.T) {
	got, err := canonicalSpans([]string{spanB, spanA})
	if err != nil || strings.Join(got, ",") != spanA+","+spanB {
		t.Fatalf("spans=%v err=%v", got, err)
	}
	many := make([]string, MaxSpans+1)
	for i := range many {
		many[i] = spanA
	}
	for name, in := range map[string][]string{
		"empty":     nil,
		"duplicate": {spanA, spanA},
		"uppercase": {strings.ToUpper(spanA)},
		"nil uuid":  {"00000000-0000-0000-0000-000000000000"},
		"not uuid":  {"span-1"},
		"too many":  many,
	} {
		if _, err := canonicalSpans(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestServiceRejectsInvalidInputBeforeStorage(t *testing.T) {
	// A nil database proves validation happens before any transaction is opened.
	s := New(nil)
	ctx := context.Background()
	a := Actor{Issuer: "https://issuer.invalid", Subject: "subject", SessionHash: "hash"}
	sc := Scope{TenantID: spanA, SiteID: spanB}
	if _, err := s.Capture(ctx, a, sc, CaptureInput{SourceSpanIDs: []string{spanA}}, "key"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid plan body accepted: %v", err)
	}
	for name, in := range map[string]ReviewInput{
		"unknown decision": {Decision: "approved", Reason: "ok"},
		"empty reason":     {Decision: "approve", Reason: "   "},
		"long reason":      {Decision: "reject", Reason: strings.Repeat("x", 1001)},
		"nul reason":       {Decision: "reject", Reason: "a\x00b"},
	} {
		if _, err := s.Decide(ctx, a, sc, spanA, in, "key", 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	if _, err := s.Decide(ctx, a, sc, "not-a-plan", ReviewInput{Decision: "approve", Reason: "ok"}, "key", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncanonical plan id accepted: %v", err)
	}
	if _, err := s.Decide(ctx, a, sc, spanA, ReviewInput{Decision: "approve", Reason: "ok"}, "key", -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative expected revision accepted: %v", err)
	}
	if _, err := s.Get(ctx, a, sc, "PLAN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("noncanonical read id must be a non-disclosing 404: %v", err)
	}
}

func TestCaptureContractRejectsApprovalRefAndUnknownFields(t *testing.T) {
	valid := `{"plan":{"order_id":"` + spanA + `","asset_id":"` + spanB + `","window_start":"2026-09-27T00:00:00Z","window_end":"2026-09-27T00:15:00Z","plan_revision":1,"unit":"count","rate_segments":[{"start":"2026-09-27T00:00:00Z","end":"2026-09-27T00:15:00Z","target_good_units_per_second":"1"}],"planned_breaks":[]},"source_span_ids":["` + spanA + `"]}`
	var in CaptureInput
	if err := strictjson.Decode([]byte(valid), &in, 1<<20); err != nil || in.Plan.PlanRevision != 1 {
		t.Fatalf("valid capture rejected: %v", err)
	}
	for name, body := range map[string]string{
		"approval_ref in plan": strings.Replace(valid, `"unit":"count"`, `"unit":"count","approval_ref":"`+spanA+`"`, 1),
		"tenant field":         strings.Replace(valid, `{"plan":`, `{"tenant_id":"`+spanA+`","plan":`, 1),
		"duplicate key":        strings.Replace(valid, `"plan_revision":1`, `"plan_revision":1,"plan_revision":1`, 1),
		"numeric rate":         strings.Replace(valid, `"target_good_units_per_second":"1"`, `"target_good_units_per_second":1`, 1),
	} {
		var v CaptureInput
		if err := strictjson.Decode([]byte(body), &v, 1<<20); !errors.Is(err, strictjson.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	encoded, err := json.Marshal(Approval{})
	if err != nil || !strings.Contains(string(encoded), `"canonical_payload_digest"`) || !strings.Contains(string(encoded), `"reviewer_actor_id"`) {
		t.Fatalf("PlanApprovalV1 wire names drifted: %s", encoded)
	}
}

// Offset timestamps are accepted on the wire and normalized to UTC before validation.
func TestCaptureAcceptsOffsetTimestampsNormalized(t *testing.T) {
	offset := `{"plan":{"order_id":"` + spanA + `","asset_id":"` + spanB + `","window_start":"2026-09-27T05:30:00+05:30","window_end":"2026-09-27T05:45:00+05:30","plan_revision":1,"unit":"count","rate_segments":[{"start":"2026-09-27T05:30:00+05:30","end":"2026-09-27T05:45:00+05:30","target_good_units_per_second":"1"}],"planned_breaks":[]},"source_span_ids":["` + spanA + `"]}`
	var in CaptureInput
	if err := strictjson.Decode([]byte(offset), &in, 1<<20); err != nil {
		t.Fatalf("offset timestamps rejected: %v", err)
	}
	if in.Plan.WindowStart != "2026-09-27T00:00:00Z" || in.Plan.WindowEnd != "2026-09-27T00:15:00Z" {
		t.Fatalf("not normalized to UTC: start=%s end=%s", in.Plan.WindowStart, in.Plan.WindowEnd)
	}
}
