package ingestion

import (
	"encoding/csv"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFormatErrorRowsCSVHeaderAndOrdering(t *testing.T) {
	input := []ImportRow{
		{Index: 9, Status: "rejected", Reason: "bad payload", SourceSequence: 28},
		{Index: 2, Status: "quarantined", Reason: "unknown mapping", SourceSequence: 21},
	}
	data, err := FormatErrorRowsCSV(input)
	if err != nil {
		t.Fatal(err)
	}
	reader := csv.NewReader(strings.NewReader(string(data)))
	got, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"row_index", "status", "reason", "source_sequence"},
		{"2", "quarantined", "unknown mapping", "21"},
		{"9", "rejected", "bad payload", "28"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CSV records = %#v, want %#v", got, want)
	}
	if input[0].Index != 9 || input[1].Index != 2 {
		t.Fatal("formatter mutated input order")
	}
}

func TestFormatErrorRowsCSVFormulaSafeReasons(t *testing.T) {
	values := []string{
		" =1+1",
		"\t+SUM(A1:A2)",
		"\r\n-1+1",
		" \t\r\n@SUM(A1:A2)",
	}
	rows := make([]ImportRow, len(values))
	for i, value := range values {
		rows[i] = ImportRow{Index: i + 1, Status: "rejected", Reason: value, SourceSequence: int64(i + 1)}
	}
	data, err := FormatErrorRowsCSV(rows)
	if err != nil {
		t.Fatal(err)
	}
	got, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range values {
		want := "'" + strings.ReplaceAll(value, "\r\n", "\n")
		if got[i+1][2] != want {
			t.Errorf("row %d reason = %q, want %q", i+1, got[i+1][2], want)
		}
	}
}

func TestFormatErrorRowsCSVEmptyAndInvalidRows(t *testing.T) {
	data, err := FormatErrorRowsCSV(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "row_index,status,reason,source_sequence\n" {
		t.Fatalf("empty CSV = %q", data)
	}
	for _, rows := range [][]ImportRow{
		{{Index: 1, Status: "accepted", SourceSequence: 1}},
		{{Index: 0, Status: "rejected"}},
		{{Index: 1, Status: "rejected"}, {Index: 1, Status: "quarantined"}},
		{{Index: 1, Status: "rejected", SourceSequence: -1}},
		{{Index: 1, Status: "unprocessed", Reason: "IMPORT_CANCELLED", SourceSequence: 4}},
	} {
		if _, err := FormatErrorRowsCSV(rows); !errors.Is(err, ErrInvalid) {
			t.Errorf("FormatErrorRowsCSV(%+v) error = %v", rows, err)
		}
	}
	if _, err := FormatErrorRowsCSV(make([]ImportRow, MaxImportRows+1)); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("oversized error rows error = %v", err)
	}
}

// Review finding: a leading tab or carriage return is also a formula trigger, so it is quoted
// even when no =+-@ follows it.
func TestSpreadsheetSafeCell(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\tfoo", "'\tfoo"},
		{"\rfoo", "'\rfoo"},
		{" =x", "' =x"},
		{"@x", "'@x"},
		{"-1", "'-1"},
		{"+1", "'+1"},
		{"=1", "'=1"},
		{"UNKNOWN_MAPPING", "UNKNOWN_MAPPING"},
		{"bad payload - see row", "bad payload - see row"},
		{"", ""},
	} {
		if got := SpreadsheetSafeCell(tc.in); got != tc.want {
			t.Errorf("SpreadsheetSafeCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
