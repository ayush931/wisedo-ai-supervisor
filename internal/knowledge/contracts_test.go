package knowledge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func contractEntity() EntityInput {
	return EntityInput{EntityID: uuid.NewString(), Type: "asset", AssetKind: "line", Name: "Line 一", Aliases: []string{"L1"}, SourceID: uuid.NewString(), SourceRevision: 1}
}
func contractClaim() ClaimInput {
	end := "2026-09-27T12:00:00.123456+02:00"
	confidence := "0.95"
	return ClaimInput{ClaimID: uuid.NewString(), SubjectID: uuid.NewString(), Predicate: "performs", ObjectEntityID: uuid.NewString(), EffectiveFrom: "2026-09-27T11:00:00.123456+02:00", EffectiveTo: &end, EvidenceClass: "report", SourceSpanIDs: []string{uuid.NewString()}, Confidence: &confidence, Extractor: &Extractor{Provider: "fixture", Model: "synthetic", Version: "1", PromptSHA256: strings.Repeat("a", 64)}}
}
func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func decodeClaim(t *testing.T, fields map[string]any) (ClaimInput, error) {
	t.Helper()
	b, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	var out ClaimInput
	e = json.Unmarshal(b, &out)
	return out, e
}
func TestCandidateJSONRoundTrip(t *testing.T) {
	in := contractEntity()
	encoded, e := json.Marshal(in)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(encoded), `"display_name":"Line 一"`) {
		t.Fatal("incorrect display name wire field")
	}
	var out EntityInput
	if e = json.Unmarshal(encoded, &out); e != nil || out.EntityID != in.EntityID || out.SourceID != in.SourceID {
		t.Fatalf("entity roundtrip: %v", e)
	}
	c := contractClaim()
	wire, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var decoded ClaimInput
	if e = json.Unmarshal(wire, &decoded); e != nil {
		t.Fatal(e)
	}
	if decoded.EffectiveFrom != "2026-09-27T09:00:00.123456Z" || decoded.EffectiveTo == nil || *decoded.EffectiveTo != "2026-09-27T10:00:00.123456Z" {
		t.Fatalf("timestamp normalization: %+v", decoded)
	}
	again, e := json.Marshal(decoded)
	if e != nil {
		t.Fatal(e)
	}
	var repeated ClaimInput
	if e = json.Unmarshal(again, &repeated); e != nil || repeated.EffectiveFrom != decoded.EffectiveFrom {
		t.Fatal("normalization not stable", e)
	}
}
func TestEntityBoundaryAndAuthorityInjection(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"client approval", func(v map[string]any) { v["state"] = "approved" }},
		{"client reviewer", func(v map[string]any) { v["reviewer_id"] = uuid.NewString() }},
		{"client authority", func(v map[string]any) { v["authority"] = "approved_document" }},
		{"unknown type", func(v map[string]any) { v["type"] = "admin" }},
		{"missing asset kind", func(v map[string]any) { delete(v, "asset_kind") }},
		{"nonasset kind", func(v map[string]any) { v["type"] = "process" }},
		{"nonUUID", func(v map[string]any) { v["source_id"] = "machine one" }},
		{"nil UUID", func(v map[string]any) { v["entity_id"] = uuid.Nil.String() }},
		{"revision zero", func(v map[string]any) { v["source_revision"] = 0 }},
		{"fraction revision", func(v map[string]any) { v["source_revision"] = 1.5 }},
		{"long name", func(v map[string]any) { v["display_name"] = strings.Repeat("界", 201) }},
		{"empty name", func(v map[string]any) { v["display_name"] = "   " }},
		{"nul", func(v map[string]any) { v["display_name"] = "x\x00y" }},
		{"duplicate alias", func(v map[string]any) { v["aliases"] = []string{"x", "x"} }},
		{"alias count", func(v map[string]any) { v["aliases"] = make([]string, 21) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			v := jsonObject(t, contractEntity())
			test.change(v)
			b, _ := json.Marshal(v)
			var e EntityInput
			if err := json.Unmarshal(b, &e); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("accepted %s: %v", test.name, err)
			}
		})
	}
	for _, kind := range []string{"site", "area", "process", "operation", "order", "material", "tool", "role", "qualification", "document", "constraint"} {
		e := contractEntity()
		e.Type = kind
		e.AssetKind = ""
		if err := e.Validate(); err != nil {
			t.Fatalf("type %s: %v", kind, err)
		}
	}
}
func TestClaimWireBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"approved", func(v map[string]any) { v["approved"] = true }},
		{"state", func(v map[string]any) { v["state"] = "approved" }},
		{"reviewer", func(v map[string]any) { v["reviewer_id"] = uuid.NewString() }},
		{"two object variants", func(v map[string]any) { v["literal"] = map[string]any{"type": "string", "value": "x"} }},
		{"no object", func(v map[string]any) { delete(v, "object_entity_id") }},
		{"self relation", func(v map[string]any) { v["object_entity_id"] = v["subject_id"] }},
		{"unknown predicate", func(v map[string]any) { v["predicate"] = "execute_tool" }},
		{"numeric time", func(v map[string]any) { v["effective_from"] = 1790000000 }},
		{"naive time", func(v map[string]any) { v["effective_from"] = "2026-09-27T11:00:00" }},
		{"time submicrosecond", func(v map[string]any) { v["effective_from"] = "2026-09-27T11:00:00.1234567+02:00" }},
		{"empty interval", func(v map[string]any) { v["effective_to"] = v["effective_from"] }},
		{"reversed interval", func(v map[string]any) { v["effective_to"] = "2026-09-26T01:00:00Z" }},
		{"year zero", func(v map[string]any) { v["effective_from"] = "0000-01-01T00:00:00Z" }},
		{"invalid offset", func(v map[string]any) { v["effective_from"] = "2026-09-27T11:00:00+24:00" }},
		{"unknown evidence", func(v map[string]any) { v["evidence_class"] = "approved_rule" }},
		{"numeric confidence", func(v map[string]any) { v["confidence"] = 0.95 }},
		{"confidence range", func(v map[string]any) { v["confidence"] = "1.000001" }},
		{"negative confidence", func(v map[string]any) { v["confidence"] = "-0.1" }},
		{"confidence NaN", func(v map[string]any) { v["confidence"] = "NaN" }},
		{"zero spans", func(v map[string]any) { v["source_span_ids"] = []string{} }},
		{"duplicate spans", func(v map[string]any) { id := uuid.NewString(); v["source_span_ids"] = []string{id, id} }},
		{"too many spans", func(v map[string]any) {
			ids := []string{}
			for range 21 {
				ids = append(ids, uuid.NewString())
			}
			v["source_span_ids"] = ids
		}},
		{"extractor authority", func(v map[string]any) { v["extractor"].(map[string]any)["authority"] = "owner" }},
		{"extractor bad digest", func(v map[string]any) { v["extractor"].(map[string]any)["prompt_sha256"] = "digest" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			v := jsonObject(t, contractClaim())
			test.change(v)
			if _, e := decodeClaim(t, v); !errors.Is(e, ErrInvalidContract) {
				t.Fatalf("accepted invalid boundary: %v", e)
			}
		})
	}
}
func TestTypedLiteralsAndCanonicalDecimals(t *testing.T) {
	for _, v := range []string{"0", "1", "-2", "0.000001", "12.123456", "-0.000001", "99999999999999999999999999999999"} {
		l := Literal{Type: "decimal", Value: json.RawMessage(`"` + v + `"`), Unit: "count/hour"}
		if e := l.Validate(); e != nil {
			t.Fatalf("valid decimal %q: %v", v, e)
		}
	}
	for _, v := range []string{"-0", "01", "1.0", "1.2300", "1e2", "+1", ".1", "1.", "NaN", "Inf", "-Inf", "0.0000001", strings.Repeat("9", 33)} {
		l := Literal{Type: "decimal", Value: json.RawMessage(`"` + v + `"`)}
		if e := l.Validate(); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid decimal %q: %v", v, e)
		}
	}
	for _, wire := range []string{`{"type":"decimal","value":1.2}`, `{"type":"boolean","value":"true"}`, `{"type":"boolean","value":null}`, `{"type":"string","value":false}`, `{"type":"timestamp","value":1790000000}`, `{"type":"string","value":"x","unit":"s"}`, `{"type":"decimal","value":"1","unit":"x/../../host?"}`, `{"type":"string","value":"x","approved":true}`} {
		var l Literal
		if e := json.Unmarshal([]byte(wire), &l); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid literal accepted: %s: %v", wire, e)
		}
	}
	for _, wire := range []string{`{"type":"boolean","value":false}`, `{"type":"boolean","value":true}`, `{"type":"timestamp","value":"2026-09-27T12:00:00.123456+02:00"}`, `{"type":"string","value":"Actual operator step"}`} {
		var l Literal
		if e := json.Unmarshal([]byte(wire), &l); e != nil {
			t.Fatalf("valid literal: %v", e)
		}
	}
	for _, property := range []string{"objective", "actual_step", "capacity_per_hour", "escalation_after_seconds"} {
		v := jsonObject(t, contractClaim())
		delete(v, "object_entity_id")
		v["predicate"] = property
		v["literal"] = map[string]any{"type": "string", "value": "Reduce downtime"}
		if property == "capacity_per_hour" || property == "escalation_after_seconds" {
			v["literal"] = map[string]any{"type": "decimal", "value": "15", "unit": "count/hour"}
		}
		if _, e := decodeClaim(t, v); e != nil {
			t.Fatalf("property %s: %v", property, e)
		}
	}
	v := jsonObject(t, contractClaim())
	delete(v, "object_entity_id")
	v["predicate"] = "capacity_per_hour"
	v["literal"] = map[string]any{"type": "decimal", "value": "-1"}
	if _, e := decodeClaim(t, v); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("negative capacity: %v", e)
	}
}
func TestRelationDomainMatrix(t *testing.T) {
	valid := [][3]string{{"located_in", "asset", "area"}, {"located_in", "area", "site"}, {"part_of_asset", "asset", "asset"}, {"performs", "asset", "operation"}, {"part_of", "operation", "process"}, {"precedes", "operation", "operation"}, {"consumes", "operation", "material"}, {"produces", "operation", "material"}, {"requires_tool", "operation", "tool"}, {"requires_qualification", "operation", "qualification"}, {"responsible_role", "process", "role"}, {"follows_document", "operation", "document"}, {"has_constraint", "asset", "constraint"},
		// WIS-611 value stream.
		{"supplied_by", "material", "supplier"}, {"received_at", "material", "location"}, {"stored_at", "material", "location"},
		{"inspected_at", "material", "inspection_point"}, {"inspected_at", "operation", "inspection_point"}, {"ships_to", "material", "customer"}}
	for _, pair := range valid {
		if e := ValidateEntityPair(pair[0], pair[1], pair[2]); e != nil {
			t.Fatalf("valid relation %v", pair)
		}
		if e := ValidateEntityPair(pair[0], "unknown", pair[2]); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("unknown subject type: %v", e)
		}
		// Every relation predicate must also survive the strict wire decoder.
		c := contractClaim()
		c.Predicate = pair[0]
		wire, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ClaimInput
		if err = json.Unmarshal(wire, &decoded); err != nil || decoded.Predicate != pair[0] {
			t.Fatalf("relation %s wire decode: %v", pair[0], err)
		}
	}
	for _, pair := range [][3]string{{"precedes", "asset", "asset"}, {"consumes", "material", "operation"}, {"capacity_per_hour", "asset", "constraint"}, {"performs", "role", "operation"}, {"part_of_asset", "asset", "area"},
		{"supplied_by", "supplier", "material"}, {"received_at", "material", "site"}, {"stored_at", "operation", "location"}, {"inspected_at", "asset", "inspection_point"},
		{"ships_to", "material", "supplier"}, {"ships_to", "customer", "material"}} {
		if e := ValidateEntityPair(pair[0], pair[1], pair[2]); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid relation %v: %v", pair, e)
		}
	}
	for _, pair := range [][2]string{{"capacity_per_hour", "asset"}, {"escalation_after_seconds", "process"}, {"objective", "site"}, {"actual_step", "operation"},
		{"lead_time_days", "material"}, {"lead_time_days", "supplier"}, {"cycle_time_seconds", "operation"}, {"cycle_time_seconds", "asset"}, {"setup_time_seconds", "asset"},
		{"batch_size", "operation"}, {"yield_ratio", "operation"}, {"sample_size", "inspection_point"}, {"inspection_method", "inspection_point"}} {
		if e := ValidateLiteralSubject(pair[0], pair[1]); e != nil {
			t.Fatalf("valid property subject %v: %v", pair, e)
		}
	}
	for _, pair := range [][2]string{{"actual_step", "qualification"}, {"cycle_time_seconds", "material"}, {"lead_time_days", "operation"}, {"yield_ratio", "asset"},
		{"sample_size", "operation"}, {"inspection_method", "operation"}, {"batch_size", "supplier"}} {
		if e := ValidateLiteralSubject(pair[0], pair[1]); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid property subject %v: %v", pair, e)
		}
	}
}

func TestClaimEntityRevisionPins(t *testing.T) {
	legacy := contractClaim()
	wire, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "subject_revision") || strings.Contains(string(wire), "object_revision") {
		t.Fatal("legacy missing pins were serialized")
	}
	var decoded ClaimInput
	if err = json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("legacy decode must remain valid: %v", err)
	}
	if err = decoded.ValidateEntityRevisions(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("legacy accepted at write boundary: %v", err)
	}
	for _, pins := range [][2]int64{{1, 1}, {2, 3}, {9223372036854775807, 9223372036854775807}} {
		candidate := contractClaim()
		candidate.SubjectRevision = pins[0]
		candidate.ObjectRevision = pins[1]
		b, e := json.Marshal(candidate)
		if e != nil {
			t.Fatal(e)
		}
		var got ClaimInput
		if e = json.Unmarshal(b, &got); e != nil {
			t.Fatal(e)
		}
		if e = got.ValidateEntityRevisions(); e != nil || got.SubjectRevision != pins[0] || got.ObjectRevision != pins[1] {
			t.Fatalf("valid exact pins %+v err=%v", pins, e)
		}
	}
	for _, pins := range [][2]int64{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		c := contractClaim()
		c.SubjectRevision = pins[0]
		c.ObjectRevision = pins[1]
		if e := c.ValidateEntityRevisions(); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid write pins accepted: %v: %v", pins, e)
		}
	}
	literal := contractClaim()
	literal.Predicate = "objective"
	literal.ObjectEntityID = ""
	literal.Literal = &Literal{Type: "string", Value: json.RawMessage(`"reduce downtime"`)}
	literal.SubjectRevision = 1
	if e := literal.Validate(); e != nil {
		t.Fatal(e)
	}
	if e := literal.ValidateEntityRevisions(); e != nil {
		t.Fatal(e)
	}
	literal.ObjectRevision = 1
	if e := literal.Validate(); !errors.Is(e, ErrInvalidContract) {
		t.Fatal("literal accepted object pin", e)
	}
	if e := literal.ValidateEntityRevisions(); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("literal object pin passed write boundary: %v", e)
	}
}

func TestClaimPinJSONBoundary(t *testing.T) {
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["subject_revision"] = -1 },
		func(v map[string]any) { v["object_revision"] = -1 },
		func(v map[string]any) { v["subject_revision"] = "1" },
		func(v map[string]any) { v["object_revision"] = 1.5 },
		func(v map[string]any) { v["entity_revision"] = 1 },
		func(v map[string]any) { v["reviewer_id"] = uuid.NewString() },
	} {
		v := jsonObject(t, contractClaim())
		change(v)
		if _, e := decodeClaim(t, v); !errors.Is(e, ErrInvalidContract) {
			t.Fatalf("invalid or unknown pin field accepted: %v", e)
		}
	}
	v := jsonObject(t, contractClaim())
	v["subject_revision"] = json.Number("9223372036854775808")
	if _, e := decodeClaim(t, v); !errors.Is(e, ErrInvalidContract) {
		t.Fatalf("overflowing pin accepted: %v", e)
	}
}

// TestValueStreamProperties (WIS-611): the new properties are non-negative decimals, a yield is at
// most one, counts are positive integers, each number is held in the one unit its name says, and
// inspection_method is the one text property among them.
func TestValueStreamProperties(t *testing.T) {
	withUnit := func(predicate, kind, value, unit string) (ClaimInput, error) {
		t.Helper()
		v := jsonObject(t, contractClaim())
		delete(v, "object_entity_id")
		v["predicate"] = predicate
		literal := map[string]any{"type": kind, "value": value}
		if unit != "" {
			literal["unit"] = unit
		}
		v["literal"] = literal
		return decodeClaim(t, v)
	}
	claim := func(predicate, kind, value string) error {
		t.Helper()
		_, e := withUnit(predicate, kind, value, "")
		return e
	}
	for _, c := range []struct {
		predicate, kind, value string
		ok                     bool
	}{
		{"cycle_time_seconds", "decimal", "42.5", true},
		{"setup_time_seconds", "decimal", "0", true},
		{"lead_time_days", "decimal", "14", true},
		{"batch_size", "decimal", "500", true},
		{"sample_size", "decimal", "5", true},
		{"yield_ratio", "decimal", "1", true},
		{"yield_ratio", "decimal", "0.985", true},
		{"inspection_method", "string", "Go/no-go gauge on bore diameter", true},
		{"cycle_time_seconds", "decimal", "-1", false},
		{"lead_time_days", "decimal", "-0.5", false},
		{"yield_ratio", "decimal", "1.000001", false},
		{"cycle_time_seconds", "string", "about a minute", false},
		{"inspection_method", "decimal", "3", false},
		{"batch_size", "decimal", "0.5", false},
		{"batch_size", "decimal", "0", false},
		{"sample_size", "decimal", "2.5", false},
		{"sample_size", "decimal", "0", false},
	} {
		if e := claim(c.predicate, c.kind, c.value); (e == nil) != c.ok {
			t.Errorf("%s %s %q: got %v, want ok=%v", c.predicate, c.kind, c.value, e, c.ok)
		}
	}

	for _, c := range []struct {
		predicate, unit, stored string
		ok                      bool
	}{
		{"cycle_time_seconds", "s", "s", true},
		{"cycle_time_seconds", "", "s", true},
		{"setup_time_seconds", "", "s", true},
		{"lead_time_days", "day", "day", true},
		{"lead_time_days", "", "day", true},
		{"batch_size", "count", "count", true},
		{"sample_size", "", "count", true},
		{"yield_ratio", "", "", true},
		{"cycle_time_seconds", "min", "", false},
		{"cycle_time_seconds", "sec", "", false},
		{"lead_time_days", "weeks", "", false},
		{"lead_time_days", "d", "", false},
		{"batch_size", "kg", "", false},
		{"yield_ratio", "%", "", false},
	} {
		got, e := withUnit(c.predicate, "decimal", "1", c.unit)
		if (e == nil) != c.ok {
			t.Errorf("%s unit %q: got %v, want ok=%v", c.predicate, c.unit, e, c.ok)
			continue
		}
		if c.ok && got.Literal.Unit != c.stored {
			t.Errorf("%s unit %q stored as %q, want %q", c.predicate, c.unit, got.Literal.Unit, c.stored)
		}
	}
}

// TestValueStreamConflicts: a subject has one cycle time at a time, so two different values are a
// conflict; a material may have several suppliers, so two supplied_by claims never conflict.
func TestValueStreamConflicts(t *testing.T) {
	decimal := func(predicate, value string) ClaimInput {
		raw, _ := json.Marshal(value)
		return ClaimInput{Predicate: predicate, Literal: &Literal{Type: "decimal", Value: raw}}
	}
	if got := conflictingValues(decimal("cycle_time_seconds", "40"), decimal("cycle_time_seconds", "45")); got != "value_conflict" {
		t.Fatalf("two cycle times: %q", got)
	}
	if got := conflictingValues(decimal("yield_ratio", "0.98"), decimal("yield_ratio", "0.98")); got != "" {
		t.Fatalf("equal yields: %q", got)
	}
	a := ClaimInput{Predicate: "supplied_by", ObjectEntityID: uuid.NewString()}
	b := ClaimInput{Predicate: "supplied_by", ObjectEntityID: uuid.NewString()}
	if got := conflictingValues(a, b); got != "" {
		t.Fatalf("two suppliers of one material are not a conflict: %q", got)
	}
	// Audit gap (WIS-611): every single-valued value-stream property conflicts on a different
	// value, the text one goes to text review, and every value-stream relationship is many-valued.
	for _, p := range []string{"lead_time_days", "setup_time_seconds", "batch_size", "sample_size"} {
		if got := conflictingValues(decimal(p, "2"), decimal(p, "3")); got != "value_conflict" {
			t.Errorf("%s 2 vs 3: %q", p, got)
		}
		if got := conflictingValues(decimal(p, "2"), decimal(p, "2")); got != "" {
			t.Errorf("%s equal: %q", p, got)
		}
	}
	text := func(v string) ClaimInput {
		raw, _ := json.Marshal(v)
		return ClaimInput{Predicate: "inspection_method", Literal: &Literal{Type: "string", Value: raw}}
	}
	if got := conflictingValues(text("Visual check"), text("Gauge check")); got != "text_review" {
		t.Errorf("two inspection methods: %q", got)
	}
	for _, p := range []string{"received_at", "stored_at", "inspected_at", "ships_to"} {
		if got := conflictingValues(ClaimInput{Predicate: p, ObjectEntityID: uuid.NewString()}, ClaimInput{Predicate: p, ObjectEntityID: uuid.NewString()}); got != "" {
			t.Errorf("%s with two objects is many-valued: %q", p, got)
		}
	}
}
