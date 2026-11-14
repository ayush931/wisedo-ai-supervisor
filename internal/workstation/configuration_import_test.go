package workstation

import (
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/google/uuid"
)

func TestParseConfigurationCSVDialect(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		code string
	}{
		{name: "invalid utf8", data: []byte("kind\n\xff"), code: "INVALID_CSV"},
		{name: "nul", data: []byte("kind\nline\x00\n"), code: "INVALID_CSV"},
		{name: "bare carriage return", data: []byte("kind\rline\n"), code: "INVALID_CSV"},
		{name: "bare quote", data: []byte("kind,display_name\nline,bad\"name\n"), code: "INVALID_CSV"},
		{name: "ragged", data: []byte("kind,display_name\nline,name,extra\n"), code: "INVALID_CSV"},
		{name: "duplicate header", data: []byte("kind,kind\nline,line\n"), code: "DUPLICATE_COLUMN"},
		{name: "unknown header", data: []byte("kind,secret\nline,x\n"), code: "UNKNOWN_COLUMN"},
		{name: "missing kind", data: []byte("display_name\nline\n"), code: "MISSING_COLUMN"},
		{name: "header only", data: []byte("kind,display_name\n"), code: "EMPTY_IMPORT"},
		{name: "large cell", data: []byte("kind,display_name\nline," + strings.Repeat("x", (16<<10)+1) + "\n"), code: "INVALID_CSV"},
		{name: "too many rows", data: []byte("kind\n" + strings.Repeat("line\n", limits.ConfigImportRows+1)), code: "IMPORT_ROWS_EXCEEDED"},
	}
	valid := []byte("\xef\xbb\xbfkind,display_name,active\r\nline,Assembly,true\r\n")
	rows, err := ParseConfigurationCSV(valid)
	if err != nil || len(rows) != 1 || rows[0].Kind != "line" || rows[0].DisplayName != "Assembly" {
		t.Fatalf("BOM/CRLF parse rows=%+v err=%v", rows, err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfigurationCSV(tc.data)
			fileErr, ok := err.(*ConfigurationImportFileError)
			if !ok || fileErr.Code != tc.code {
				t.Fatalf("ParseConfigurationCSV error=%v, want code %s", err, tc.code)
			}
		})
	}
}

func TestConfigurationColumnMappingMatchesNames(t *testing.T) {
	row := ConfigurationCSVRow{Kind: "kind-value", Ref: "ref-value", ID: "id-value", Revision: "revision-value", DisplayName: "name-value", Active: "active-value", Unit: "unit-value", Line: "line-value", Item: "item-value", Steps: "steps-value", TimeZone: "zone-value", Shifts: "shifts-value"}
	want := map[string]string{"kind": row.Kind, "ref": row.Ref, "id": row.ID, "revision": row.Revision, "display_name": row.DisplayName, "active": row.Active, "unit": row.Unit, "line": row.Line, "item": row.Item, "steps": row.Steps, "time_zone": row.TimeZone, "shifts": row.Shifts}
	if len(configurationColumns) != len(want) {
		t.Fatalf("configuration columns=%d, mapped fields=%d", len(configurationColumns), len(want))
	}
	for _, name := range configurationColumns {
		field := configurationColumn(&row, name)
		if field == nil || *field != want[name] {
			t.Errorf("column %q maps to %v, want %q", name, field, want[name])
		}
	}
}

func TestConfigurationRowCellSyntax(t *testing.T) {
	line := ConfigurationCSVRow{Kind: "line", DisplayName: "Line", Active: "true"}
	for name, ref := range map[string]string{"prefixed": "-x", "uuid": uuid.NewString()} {
		t.Run("ref "+name, func(t *testing.T) {
			row := line
			row.Ref = ref
			_, err := decodeConfigurationRow(row)
			assertConfigurationRowError(t, err, "INVALID_VALUE", "ref")
		})
	}
	_, err := decodeConfigurationRow(ConfigurationCSVRow{Kind: "shift_calendar", Active: "true"})
	assertConfigurationRowError(t, err, "COLUMN_NOT_APPLICABLE", "active")
	_, err = decodeConfigurationRow(ConfigurationCSVRow{Kind: "routing", Active: "true", Line: "line-a", Item: "item-a", Steps: "bad"})
	assertConfigurationRowError(t, err, "INVALID_VALUE", "steps[0]")
	_, err = decodeConfigurationRow(ConfigurationCSVRow{Kind: "shift_calendar", TimeZone: "UTC", Shifts: "1 08:00"})
	assertConfigurationRowError(t, err, "INVALID_VALUE", "shifts[0]")
	_, err = decodeConfigurationRow(ConfigurationCSVRow{Kind: "line", ID: uuid.NewString(), Active: "true"})
	assertConfigurationRowError(t, err, "INVALID_VALUE", "id")
}

func TestDecodeConfigurationRowCellErrorPrecedesInvalidReference(t *testing.T) {
	_, err := decodeConfigurationRow(ConfigurationCSVRow{
		Kind:        "line",
		Ref:         "invalid ref",
		DisplayName: "Line",
		Active:      "=TRUE",
	})
	assertConfigurationRowError(t, err, "INVALID_VALUE", "active")
}

func assertConfigurationRowError(t *testing.T, err error, code, path string) {
	t.Helper()
	rowErr, ok := err.(*ConfigurationImportRowError)
	if !ok || rowErr.Code != code || rowErr.Path != path {
		t.Fatalf("row error=%v, want %s at %s", err, code, path)
	}
}

func TestConfigurationImportReportETag(t *testing.T) {
	good := `"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`
	if !ValidConfigurationImportETag(good) {
		t.Fatal("valid quoted lowercase digest rejected")
	}
	for _, invalid := range []string{"", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", `"ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"`, `"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde"`} {
		if ValidConfigurationImportETag(invalid) {
			t.Errorf("accepted invalid report ETag %q", invalid)
		}
	}
}
