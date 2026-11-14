package workstation

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func validProfile() StationProfileBodyV1 {
	return StationProfileBodyV1{
		SchemaVersion: 1, WorkstationID: "7ee1e85e-7683-46b2-815e-38ce4d4681d1", LineID: "1c6b4260-a7fc-4fef-8e80-19b611f83f7b",
		DisplayName: "Press 4 / 50% load", Template: "quality",
		Fields: []ProfileFieldV1{
			{Name: "bore_diameter", Label: "Bore diameter (mm)", Kind: "decimal", Required: true, Unit: "mm", Min: "9.95", Max: "10.05"},
			{Name: "disposition", Label: "Disposition", Kind: "choice", Required: true, Choices: []string{"accept", "hold", "reject"}},
			{Name: "gauge_checked", Label: "Gauge checked", Kind: "boolean"},
			{Name: "measured_at", Label: "Measured at", Kind: "timestamp"},
			{Name: "note", Label: "Note", Kind: "text"},
		},
		InstructionReferences: []InstructionRefV1{{DocumentID: "605ff7bd-07a9-4017-a8c5-bb01d918bb7f", Revision: 2, SHA256: strings.Repeat("ab", 32)}},
	}
}

// Every rejected profile names its member and says whether it is invalid or unsupported.
func TestStationProfileValidationNamesThePath(t *testing.T) {
	if err := validProfile().Validate(); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	tests := map[string]struct {
		mutate func(*StationProfileBodyV1)
		want   ConfigError
	}{
		"template outside the fixed set": {func(p *StationProfileBodyV1) { p.Template = "custom_html" }, ConfigError{CodeUnsupportedStation, "template"}},
		"field kind outside the fixed set": {func(p *StationProfileBodyV1) { p.Fields[2].Kind = "script" },
			ConfigError{CodeUnsupportedStation, "fields[2].kind"}},
		"backtick label":      {func(p *StationProfileBodyV1) { p.Fields[0].Label = "Bore `rm`" }, ConfigError{CodeUnsupportedStation, "fields[0].label"}},
		"template expression": {func(p *StationProfileBodyV1) { p.Fields[4].Label = "Note {{ order.id }}" }, ConfigError{CodeUnsupportedStation, "fields[4].label"}},
		"javascript url choice": {func(p *StationProfileBodyV1) { p.Fields[1].Choices[2] = "javascript:alert(1)" },
			ConfigError{CodeUnsupportedStation, "fields[1].choices[2]"}},
		"formula unit":               {func(p *StationProfileBodyV1) { p.Fields[0].Unit = "=HYPERLINK(1)" }, ConfigError{CodeUnsupportedStation, "fields[0].unit"}},
		"script scheme display name": {func(p *StationProfileBodyV1) { p.DisplayName = " JavaScript:alert(1)" }, ConfigError{CodeUnsupportedStation, "display_name"}},
		"data scheme choice":         {func(p *StationProfileBodyV1) { p.Fields[1].Choices[0] = "DATA:text/html;base64,PHN" }, ConfigError{CodeUnsupportedStation, "fields[1].choices[0]"}},
		"interpolation label":        {func(p *StationProfileBodyV1) { p.Fields[4].Label = "Note ${x}" }, ConfigError{CodeUnsupportedStation, "fields[4].label"}},
		"non-canonical negative min": {func(p *StationProfileBodyV1) { p.Fields[0].Min = "-0" }, ConfigError{CodeInvalidStationProfile, "fields[0].min"}},
		"range on a text field":      {func(p *StationProfileBodyV1) { p.Fields[4].Max = "5" }, ConfigError{CodeUnsupportedStation, "fields[4].max"}},
		"schema version":             {func(p *StationProfileBodyV1) { p.SchemaVersion = 2 }, ConfigError{CodeInvalidStationProfile, "schema_version"}},
		"workstation id":             {func(p *StationProfileBodyV1) { p.WorkstationID = "press-4" }, ConfigError{CodeInvalidStationProfile, "workstation_id"}},
		"no fields":                  {func(p *StationProfileBodyV1) { p.Fields = nil }, ConfigError{CodeInvalidStationProfile, "fields"}},
		"duplicate field":            {func(p *StationProfileBodyV1) { p.Fields[3].Name = "note" }, ConfigError{CodeInvalidStationProfile, "fields[4].name"}},
		"decimal without unit":       {func(p *StationProfileBodyV1) { p.Fields[0].Unit = "" }, ConfigError{CodeInvalidStationProfile, "fields[0]"}},
		"unit on a choice":           {func(p *StationProfileBodyV1) { p.Fields[1].Unit = "mm" }, ConfigError{CodeInvalidStationProfile, "fields[1]"}},
		"choice without options":     {func(p *StationProfileBodyV1) { p.Fields[1].Choices = nil }, ConfigError{CodeInvalidStationProfile, "fields[1]"}},
		"inverted range":             {func(p *StationProfileBodyV1) { p.Fields[0].Min = "10.1" }, ConfigError{CodeInvalidStationProfile, "fields[0].min"}},
		"seven-digit range":          {func(p *StationProfileBodyV1) { p.Fields[0].Max = "10.0000001" }, ConfigError{CodeInvalidStationProfile, "fields[0].max"}},
		"instruction hash":           {func(p *StationProfileBodyV1) { p.InstructionReferences[0].SHA256 = "abc" }, ConfigError{CodeInvalidStationProfile, "instruction_references[0]"}},
		"duplicate instruction": {func(p *StationProfileBodyV1) {
			p.InstructionReferences = append(p.InstructionReferences, p.InstructionReferences[0])
		}, ConfigError{CodeInvalidStationProfile, "instruction_references[1].document_id"}},
		"unbounded display name":  {func(p *StationProfileBodyV1) { p.DisplayName = strings.Repeat("a", 129) }, ConfigError{CodeInvalidStationProfile, "display_name"}},
		"control char in a label": {func(p *StationProfileBodyV1) { p.Fields[4].Label = "Note\u0007" }, ConfigError{CodeInvalidStationProfile, "fields[4]"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			p := validProfile()
			p.Fields = append([]ProfileFieldV1(nil), p.Fields...)
			p.Fields[1].Choices = append([]string(nil), p.Fields[1].Choices...)
			tc.mutate(&p)
			if got := p.Validate(); got == nil || *got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// An unknown member is unsupported configuration at its path, not a generic decode failure.
func TestDecodeStationProfileRejectsUnknownMembersByPath(t *testing.T) {
	valid, err := json.Marshal(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeStationProfile(valid); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	cases := map[string]struct {
		body string
		want ConfigError
	}{
		"top-level script":   {strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"script":"alert(1)"`, 1), ConfigError{CodeUnsupportedStation, "*"}},
		"field handler":      {strings.Replace(string(valid), `"name":"note"`, `"name":"note","on_change":"x()"`, 1), ConfigError{CodeUnsupportedStation, "fields[4].*"}},
		"fields as object":   {`{"schema_version":1,"fields":{"name":"x"}}`, ConfigError{CodeInvalidStationProfile, "fields"}},
		"field as array":     {`{"schema_version":1,"fields":[["name"]]}`, ConfigError{CodeInvalidStationProfile, "fields[0]"}},
		"body as array":      {`[1]`, ConfigError{CodeInvalidStationProfile, ""}},
		"non-identifier key": {strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"<b>":1`, 1), ConfigError{CodeUnsupportedStation, "*"}},
		"case-folded key":    {strings.Replace(string(valid), `"template"`, `"TEMPLATE"`, 1), ConfigError{CodeUnsupportedStation, "*"}},
		"duplicate key":      {strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), ConfigError{CodeInvalidStationProfile, ""}},
		"float revision":     {strings.Replace(string(valid), `"revision":2`, `"revision":2.5`, 1), ConfigError{CodeInvalidStationProfile, ""}},
	}
	for name, tc := range cases {
		_, err := DecodeStationProfile([]byte(tc.body))
		var got *ConfigError
		if !errors.As(err, &got) || *got != tc.want {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	preview := `{"profile":` + strings.Replace(string(valid), `"min":"9.95"`, `"min":"9.95","formula":"=A1"`, 1) + `}`
	var got *ConfigError
	if _, err = DecodeStationProfilePreview([]byte(preview)); !errors.As(err, &got) || *got != (ConfigError{CodeUnsupportedStation, "profile.fields[0].*"}) {
		t.Fatalf("preview unknown member: %v", err)
	}
	half := `{"profile":` + string(valid) + `,"job_order_id":"ed6ceeb6-23a2-41e9-8f25-d88aedf78b58"}`
	if _, err = DecodeStationProfilePreview([]byte(half)); !errors.As(err, &got) || *got != (ConfigError{CodeInvalidStationProfile, "job_order_id"}) {
		t.Fatalf("job order without revision: %v", err)
	}
}

// Labels are rendered as plain text: comparison signs and URLs are ordinary text, while
// template, interpolation, script-scheme and formula content is refused. Job orders and shift
// plans share the field validator, so the same rule applies there.
func TestExecutableTextFilter(t *testing.T) {
	for _, ok := range []string{"Torque > 25", "Gap < 0.2 mm", "See https://example.invalid/sop", "Data entry", "-12.5 offset"} {
		if executable(ok) {
			t.Errorf("plain text refused: %q", ok)
		}
	}
	for _, bad := range []string{"{{x}}", " JavaScript:alert(1)", "vbscript:x", "data:text/html,x", "${x}", "a `b`", "=SUM(A1)", "  =1+1"} {
		if !executable(bad) {
			t.Errorf("executable text accepted: %q", bad)
		}
	}
	plan := validShiftPlan()
	plan.Assignments[0].Fields[0].Label = "{{order.secret}}"
	if err := plan.Validate(); !errors.Is(err, ErrInvalidShiftPlan) {
		t.Fatalf("template label accepted on a shift plan: %v", err)
	}
	plan.Assignments[0].Fields[0].Label = "Torque > 25"
	if err := plan.Validate(); err != nil {
		t.Fatalf("plain label refused on a shift plan: %v", err)
	}
}

func TestResolveFormPrecedence(t *testing.T) {
	p := validProfile()
	order := &JobOrderBodyV1{Template: "quality", Fields: []FieldV1{
		{Name: "bore_diameter", Label: "Bore diameter (mm)", Kind: "decimal", Required: true, Unit: "mm"},
		{Name: "note", Label: "Note", Kind: "text", Required: true},
		{Name: "good_units", Label: "Good units", Kind: "decimal", Required: true, Unit: "count"},
		// Required by the profile, optional on the job order: stays required.
		{Name: "disposition", Label: "Disposition", Kind: "choice", Choices: []string{"accept", "hold", "reject"}},
	}}
	fields, conflicts := ResolveForm(p, order)
	if len(conflicts) != 0 || len(fields) != 6 {
		t.Fatalf("fields %+v conflicts %v", fields, conflicts)
	}
	want := []struct {
		name     string
		source   FieldSource
		required bool
	}{{"bore_diameter", SourceBoth, true}, {"disposition", SourceBoth, true}, {"gauge_checked", SourceStationProfile, false},
		{"measured_at", SourceStationProfile, false}, {"note", SourceBoth, true}, {"good_units", SourceJobOrder, true}}
	for i, w := range want {
		if f := fields[i]; f.Field.Name != w.name || f.Source != w.source || f.Field.Required != w.required {
			t.Errorf("field %d = %+v, want %+v", i, f, w)
		}
	}
	// The profile's range survives the merge; the job order may only tighten requirement.
	if fields[0].Field.Min != "9.95" || fields[0].Field.Max != "10.05" {
		t.Errorf("profile range lost: %+v", fields[0].Field)
	}

	order.Template = "production"
	order.Fields[0].Unit = "in"
	order.Fields[1].Choices = nil
	order.Fields[1].Label = "Remark"
	_, conflicts = ResolveForm(p, order)
	if want := []string{"template", "job_order.fields[0]", "job_order.fields[1]"}; !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("conflicts %v, want %v", conflicts, want)
	}
	if fields, conflicts = ResolveForm(p, nil); len(fields) != 5 || conflicts == nil || len(conflicts) != 0 {
		t.Fatalf("profile only: %v %v", fields, conflicts)
	}
}

func TestValidateEntry(t *testing.T) {
	form, _ := ResolveForm(validProfile(), nil)
	valid := []EntryValueV1{
		{Field: "bore_diameter", Value: "10.012", Unit: "mm"},
		{Field: "disposition", Value: "hold"},
		{Field: "gauge_checked", Value: "true"},
		{Field: "measured_at", Value: "2026-10-20T08:15:00+02:00"},
	}
	if got := ValidateEntry(form, valid); got != nil {
		t.Fatalf("valid entry rejected: %v", got)
	}
	with := func(i int, v EntryValueV1) []EntryValueV1 {
		out := append([]EntryValueV1(nil), valid...)
		out[i] = v
		return out
	}
	tests := map[string]struct {
		values []EntryValueV1
		want   []EntryViolationV1
	}{
		"missing required":        {valid[1:], []EntryViolationV1{{"fields[0]", EntryMissingRequired}}},
		"nothing submitted":       {nil, []EntryViolationV1{{"fields[0]", EntryMissingRequired}, {"fields[1]", EntryMissingRequired}}},
		"wrong unit":              {with(0, EntryValueV1{Field: "bore_diameter", Value: "10", Unit: "in"}), []EntryViolationV1{{"values[0]", EntryWrongUnit}}},
		"missing unit":            {with(0, EntryValueV1{Field: "bore_diameter", Value: "10"}), []EntryViolationV1{{"values[0]", EntryWrongUnit}}},
		"unit on a choice":        {with(1, EntryValueV1{Field: "disposition", Value: "hold", Unit: "mm"}), []EntryViolationV1{{"values[1]", EntryUnitNotAllowed}}},
		"choice not allowed":      {with(1, EntryValueV1{Field: "disposition", Value: "release"}), []EntryViolationV1{{"values[1]", EntryChoiceNotAllowed}}},
		"choice is case-exact":    {with(1, EntryValueV1{Field: "disposition", Value: "Hold"}), []EntryViolationV1{{"values[1]", EntryChoiceNotAllowed}}},
		"seven fractional digits": {with(0, EntryValueV1{Field: "bore_diameter", Value: "10.0000001", Unit: "mm"}), []EntryViolationV1{{"values[0]", EntryInvalidDecimal}}},
		"trailing zero":           {with(0, EntryValueV1{Field: "bore_diameter", Value: "10.010", Unit: "mm"}), []EntryViolationV1{{"values[0]", EntryInvalidDecimal}}},
		"exponent":                {with(0, EntryValueV1{Field: "bore_diameter", Value: "1e1", Unit: "mm"}), []EntryViolationV1{{"values[0]", EntryInvalidDecimal}}},
		"below range":             {with(0, EntryValueV1{Field: "bore_diameter", Value: "9.949999", Unit: "mm"}), []EntryViolationV1{{"values[0]", EntryOutOfRange}}},
		"above range":             {with(0, EntryValueV1{Field: "bore_diameter", Value: "10.050001", Unit: "mm"}), []EntryViolationV1{{"values[0]", EntryOutOfRange}}},
		"unknown field": {append(append([]EntryValueV1(nil), valid...), EntryValueV1{Field: "torque", Value: "25", Unit: "Nm"}),
			[]EntryViolationV1{{"values[4]", EntryUnknownField}}},
		"duplicate field":     {append(append([]EntryValueV1(nil), valid...), valid[1]), []EntryViolationV1{{"values[4]", EntryDuplicate}}},
		"empty is not absent": {with(1, EntryValueV1{Field: "disposition", Value: ""}), []EntryViolationV1{{"values[1]", EntryEmpty}}},
		"boolean spelling":    {with(2, EntryValueV1{Field: "gauge_checked", Value: "yes"}), []EntryViolationV1{{"values[2]", EntryInvalidBoolean}}},
		"timestamp offset":    {with(3, EntryValueV1{Field: "measured_at", Value: "2026-10-20T08:15:00"}), []EntryViolationV1{{"values[3]", EntryInvalidTimestamp}}},
		"text control char":   {append(append([]EntryValueV1(nil), valid...), EntryValueV1{Field: "note", Value: "a\x00b"}), []EntryViolationV1{{"values[4]", EntryInvalidText}}},
		// WIS-520 acceptance 3 (no executable operator content): an entered text value is
		// operator content that reaches summaries, handovers and exports, so it gets the same
		// filter as comments, labels and choices.
		"text formula":  {append(append([]EntryValueV1(nil), valid...), EntryValueV1{Field: "note", Value: "=HYPERLINK(\"https://x.invalid\")"}), []EntryViolationV1{{"values[4]", EntryInvalidText}}},
		"text template": {append(append([]EntryValueV1(nil), valid...), EntryValueV1{Field: "note", Value: "{{constructor}}"}), []EntryViolationV1{{"values[4]", EntryInvalidText}}},
	}
	for name, tc := range tests {
		if got := ValidateEntry(form, tc.values); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	// Negative values and bounds follow the published signed decimal6 pattern.
	temperature, _ := ResolveForm(StationProfileBodyV1{Template: "quality", Fields: []ProfileFieldV1{{Name: "oven_temp", Label: "Oven", Kind: "decimal", Required: true, Unit: "C", Min: "-40", Max: "120"}}}, nil)
	for value, want := range map[string][]EntryViolationV1{
		"-12.5": nil, "-40": nil, "120": nil,
		"-40.000001": {{"values[0]", EntryOutOfRange}},
		"-0":         {{"values[0]", EntryInvalidDecimal}},
		"--1":        {{"values[0]", EntryInvalidDecimal}},
	} {
		if got := ValidateEntry(temperature, []EntryValueV1{{Field: "oven_temp", Value: value, Unit: "C"}}); !reflect.DeepEqual(got, want) {
			t.Errorf("oven_temp %s: got %v, want %v", value, got, want)
		}
	}
	negative := validProfile()
	negative.Fields[0].Min, negative.Fields[0].Max = "-40", "-12.5"
	if err := negative.Validate(); err != nil {
		t.Fatalf("negative range refused: %v", err)
	}
	// Boundaries are inclusive.
	for _, v := range []string{"9.95", "10.05"} {
		if got := ValidateEntry(form, with(0, EntryValueV1{Field: "bore_diameter", Value: v, Unit: "mm"})); got != nil {
			t.Errorf("boundary %s rejected: %v", v, got)
		}
	}
}
