package workstation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/canonical"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/google/uuid"
)

var (
	configurationColumns           = []string{"kind", "ref", "id", "revision", "display_name", "active", "unit", "line", "item", "steps", "time_zone", "shifts"}
	configurationRef               = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	configurationStep              = regexp.MustCompile(`^([1-7]) ([0-9]{2}:[0-9]{2})-([0-9]{2}:[0-9]{2})$`)
	ErrInvalidReportETag           = errors.New("invalid configuration import report etag")
	ErrConfigurationImportRejected = errors.New("configuration import contains rejected rows")
)

type ConfigurationImportFileError struct {
	Code   string
	Column int
}

func (e *ConfigurationImportFileError) Error() string { return e.Code }

type ConfigurationImportRowError struct {
	Code string
	Path string
}

func (e *ConfigurationImportRowError) Error() string { return e.Code + " at " + e.Path }

type ConfigurationCSVRow struct {
	Number      int    `json:"row"`
	Kind        string `json:"kind"`
	Ref         string `json:"ref"`
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	DisplayName string `json:"display_name"`
	Active      string `json:"active"`
	Unit        string `json:"unit"`
	Line        string `json:"line"`
	Item        string `json:"item"`
	Steps       string `json:"steps"`
	TimeZone    string `json:"time_zone"`
	Shifts      string `json:"shifts"`
}

type configurationRowValue struct {
	ID       string
	Revision int64
	Body     any
}

type ConfigurationImportRowV1 struct {
	Row              int    `json:"row"`
	Kind             string `json:"kind"`
	Ref              string `json:"ref,omitempty"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Path             string `json:"path,omitempty"`
	ObjectID         string `json:"object_id,omitempty"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
	DisplayName      string `json:"display_name,omitempty"`
}

type ConfigurationImportCountsV1 struct {
	Accepted  int `json:"accepted"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

type ConfigurationImportReportV1 struct {
	SchemaVersion int                         `json:"schema_version"`
	FileSHA256    string                      `json:"file_sha256"`
	ReportSHA256  string                      `json:"report_sha256"`
	Applicable    bool                        `json:"applicable"`
	Replayed      bool                        `json:"replayed,omitempty"`
	Counts        ConfigurationImportCountsV1 `json:"counts"`
	Rows          []ConfigurationImportRowV1  `json:"rows"`
}

type configurationImportExisting struct {
	ID       string
	Revision int64
	Digest   string
	Name     string
}

type configurationImportSiteState struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

type configurationImportDigestRow struct {
	Row              int    `json:"row"`
	Kind             string `json:"kind"`
	Ref              string `json:"ref,omitempty"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Path             string `json:"path,omitempty"`
	ObjectID         string `json:"object_id,omitempty"`
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
}

type configurationImportDigestBody struct {
	FileSHA256      string                         `json:"file_sha256"`
	SiteStateSHA256 string                         `json:"site_state_sha256"`
	Rows            []configurationImportDigestRow `json:"rows"`
}

func ParseConfigurationCSV(data []byte) ([]ConfigurationCSVRow, error) {
	if len(data) > limits.ImportBytes {
		return nil, &ConfigurationImportFileError{Code: "IMPORT_TOO_LARGE"}
	}
	if len(data) >= 3 && bytes.Equal(data[:3], []byte{0xef, 0xbb, 0xbf}) {
		data = data[3:]
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, &ConfigurationImportFileError{Code: "INVALID_CSV"}
	}
	for i, b := range data {
		if b == '\r' && (i+1 == len(data) || data[i+1] != '\n') {
			return nil, &ConfigurationImportFileError{Code: "INVALID_CSV"}
		}
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = 0
	r.LazyQuotes = false
	header, err := r.Read()
	if err != nil {
		return nil, &ConfigurationImportFileError{Code: "INVALID_CSV"}
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		if _, ok := columns[name]; ok {
			return nil, &ConfigurationImportFileError{Code: "DUPLICATE_COLUMN", Column: i + 1}
		}
		known := false
		for _, column := range configurationColumns {
			if name == column {
				known = true
				break
			}
		}
		if !known {
			return nil, &ConfigurationImportFileError{Code: "UNKNOWN_COLUMN", Column: i + 1}
		}
		columns[name] = i
	}
	if _, ok := columns["kind"]; !ok {
		return nil, &ConfigurationImportFileError{Code: "MISSING_COLUMN"}
	}
	rows := make([]ConfigurationCSVRow, 0)
	for {
		cells, readErr := r.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, &ConfigurationImportFileError{Code: "INVALID_CSV"}
		}
		for _, cell := range cells {
			if len(cell) > 16<<10 {
				return nil, &ConfigurationImportFileError{Code: "INVALID_CSV"}
			}
		}
		row := ConfigurationCSVRow{Number: len(rows) + 1}
		for _, name := range configurationColumns {
			if index, ok := columns[name]; ok {
				field := configurationColumn(&row, name)
				*field = cells[index]
			}
		}
		rows = append(rows, row)
		if len(rows) > limits.ConfigImportRows {
			return nil, &ConfigurationImportFileError{Code: "IMPORT_ROWS_EXCEEDED"}
		}
	}
	if len(rows) == 0 {
		return nil, &ConfigurationImportFileError{Code: "EMPTY_IMPORT"}
	}
	return rows, nil
}

func configurationColumn(row *ConfigurationCSVRow, name string) *string {
	switch name {
	case "kind":
		return &row.Kind
	case "ref":
		return &row.Ref
	case "id":
		return &row.ID
	case "revision":
		return &row.Revision
	case "display_name":
		return &row.DisplayName
	case "active":
		return &row.Active
	case "unit":
		return &row.Unit
	case "line":
		return &row.Line
	case "item":
		return &row.Item
	case "steps":
		return &row.Steps
	case "time_zone":
		return &row.TimeZone
	case "shifts":
		return &row.Shifts
	default:
		return nil
	}
}

func decodeConfigurationRow(row ConfigurationCSVRow) (configurationRowValue, error) {
	kind := row.Kind
	allowed := map[string]bool{"kind": true, "ref": true, "id": true, "revision": true, "display_name": true}
	switch kind {
	case "line":
		allowed["active"] = true
	case "item":
		allowed["active"], allowed["unit"] = true, true
	case "shift_calendar":
		allowed["time_zone"], allowed["shifts"] = true, true
	case "routing":
		allowed["active"], allowed["line"], allowed["item"], allowed["steps"] = true, true, true, true
	default:
		return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "kind"}
	}
	// After kind validation, schema-ordered cell errors win before ref, id, revision, and kind-specific validation.
	for _, name := range configurationColumns {
		field := configurationColumn(&row, name)
		value := *field
		if value != "" && !allowed[name] {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "COLUMN_NOT_APPLICABLE", Path: name}
		}
		if name == "kind" {
			continue
		}
		if name == "display_name" && strings.HasPrefix(strings.TrimLeftFunc(value, unicode.IsSpace), "=") {
			continue // D's shared display-name validation supplies the INVALID_MASTER_DATA reason.
		}
		if configurationFormulaCell(value) {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: name}
		}
	}
	if row.Ref != "" && (!configurationRef.MatchString(row.Ref) || canonicalID(row.Ref) || strings.ContainsRune("=+-@", rune(row.Ref[0]))) {
		return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "ref"}
	}
	if (row.ID == "") != (row.Revision == "") {
		return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "id"}
	}
	if row.ID != "" && !canonicalID(row.ID) {
		return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "id"}
	}
	var expected int64
	if row.Revision != "" {
		parsed, err := strconv.ParseInt(row.Revision, 10, 64)
		if err != nil || parsed < 1 || strconv.FormatInt(parsed, 10) != row.Revision {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "revision"}
		}
		expected = parsed
	}
	switch kind {
	case "line":
		active, err := parseConfigurationBool(row.Active)
		if err != nil {
			return configurationRowValue{}, err
		}
		return configurationRowValue{ID: row.ID, Revision: expected, Body: LineBodyV1{SchemaVersion: 1, DisplayName: row.DisplayName, Active: active}}, nil
	case "item":
		active, err := parseConfigurationBool(row.Active)
		if err != nil {
			return configurationRowValue{}, err
		}
		if row.Unit != "count" {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "unit"}
		}
		return configurationRowValue{ID: row.ID, Revision: expected, Body: ItemBodyV1{SchemaVersion: 1, DisplayName: row.DisplayName, Unit: MasterDataUnit(row.Unit), Active: active}}, nil
	case "shift_calendar":
		if row.TimeZone == "" || row.Shifts == "" {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "shifts"}
		}
		parts := strings.Split(row.Shifts, "|")
		shifts := make([]ShiftCalendarShiftV1, 0, len(parts))
		for i, part := range parts {
			match := configurationStep.FindStringSubmatch(part)
			if match == nil {
				return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: fmt.Sprintf("shifts[%d]", i)}
			}
			day, _ := strconv.Atoi(match[1])
			shifts = append(shifts, ShiftCalendarShiftV1{DayOfWeek: day, StartLocal: match[2], EndLocal: match[3]})
		}
		return configurationRowValue{ID: row.ID, Revision: expected, Body: ShiftCalendarBodyV1{SchemaVersion: 1, DisplayName: row.DisplayName, TimeZone: row.TimeZone, Shifts: shifts}}, nil
	case "routing":
		active, err := parseConfigurationBool(row.Active)
		if err != nil {
			return configurationRowValue{}, err
		}
		if !configurationReference(row.Line) {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "line"}
		}
		if !configurationReference(row.Item) {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "item"}
		}
		parts := strings.Split(row.Steps, "|")
		steps := make([]RoutingStepV1, 0, len(parts))
		if row.Steps == "" || len(parts) > 256 {
			return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "steps"}
		}
		for i, part := range parts {
			if !canonicalID(part) {
				return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: fmt.Sprintf("steps[%d]", i)}
			}
			steps = append(steps, RoutingStepV1{StepNumber: i + 1, WorkstationID: part})
		}
		return configurationRowValue{ID: row.ID, Revision: expected, Body: RoutingBodyV1{SchemaVersion: 1, DisplayName: row.DisplayName, Active: active, Steps: steps}}, nil
	}
	return configurationRowValue{}, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "kind"}
}

func parseConfigurationBool(value string) (bool, error) {
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &ConfigurationImportRowError{Code: "INVALID_VALUE", Path: "active"}
	}
}

func configurationReference(value string) bool {
	if canonicalID(value) {
		return true
	}
	return configurationRef.MatchString(value) && !strings.ContainsRune("=+-@", rune(value[0]))
}

func configurationFormulaCell(value string) bool {
	if value != "" && (value[0] == '\t' || value[0] == '\r') {
		return true
	}
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	return trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0]))
}

func (s *MasterDataService) DryRunConfigurationImport(ctx context.Context, a Actor, sc Scope, data []byte) (ConfigurationImportReportV1, error) {
	rows, err := ParseConfigurationCSV(data)
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	defer tx.Rollback()
	report, err := evaluateConfigurationImport(ctx, tx, a, sc, data, rows, uuid.NewString())
	return report, err
}

func (s *MasterDataService) ApplyConfigurationImport(ctx context.Context, a Actor, sc Scope, data []byte, key, ifMatch string) (ConfigurationImportReportV1, string, bool, error) {
	rows, err := ParseConfigurationCSV(data)
	if err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	if key == "" || len(key) > 128 || strings.TrimSpace(key) != key {
		return ConfigurationImportReportV1{}, "", false, ErrInvalidShiftPlan
	}
	if !ValidConfigurationImportETag(ifMatch) {
		return ConfigurationImportReportV1{}, "", false, ErrInvalidReportETag
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftWrite)
	if err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	defer tx.Rollback()
	fileHash := sha256.Sum256(data)
	fileSHA := hex.EncodeToString(fileHash[:])
	var oldFile, oldReport, oldID string
	var oldJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT file_sha256,report_sha256,id::text,report FROM supervisor.configuration_imports WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&oldFile, &oldReport, &oldID, &oldJSON)
	if err == nil {
		if oldFile != fileSHA || ifMatch != `"`+oldReport+`"` {
			return ConfigurationImportReportV1{}, "", false, ErrConflict
		}
		var report ConfigurationImportReportV1
		if err = json.Unmarshal(oldJSON, &report); err != nil {
			return ConfigurationImportReportV1{}, "", false, err
		}
		report.Replayed = true
		return report, oldID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConfigurationImportReportV1{}, "", false, err
	}
	importID := uuid.NewString()
	report, err := evaluateConfigurationImport(ctx, tx, a, sc, data, rows, importID)
	if err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	if ifMatch != `"`+report.ReportSHA256+`"` {
		return report, "", false, ErrRevision
	}
	if report.Counts.Rejected != 0 {
		return report, "", false, ErrConfigurationImportRejected
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO supervisor.configuration_imports(tenant_id,site_id,id,actor_issuer,actor_subject,idempotency_key,file_sha256,report_sha256,accepted_rows,duplicate_rows,report) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`, sc.TenantID, sc.SiteID, importID, a.Issuer, a.Subject, key, report.FileSHA256, report.ReportSHA256, report.Counts.Accepted, report.Counts.Duplicate, string(reportJSON))
	if err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	if err = s.plans.finish(ctx, tx, a); err != nil {
		return ConfigurationImportReportV1{}, "", false, err
	}
	return report, importID, false, nil
}

func (s *MasterDataService) GetConfigurationImport(ctx context.Context, a Actor, sc Scope, id string) (ConfigurationImportReportV1, error) {
	if !canonicalID(id) {
		return ConfigurationImportReportV1{}, ErrNotFound
	}
	tx, _, _, err := s.plans.begin(ctx, a, sc, shiftRead)
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT report FROM supervisor.configuration_imports WHERE tenant_id=$1 AND site_id=$2 AND id=$3`, sc.TenantID, sc.SiteID, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigurationImportReportV1{}, ErrNotFound
	}
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	var report ConfigurationImportReportV1
	if err = json.Unmarshal(raw, &report); err != nil {
		return ConfigurationImportReportV1{}, err
	}
	return report, s.plans.finish(ctx, tx, a)
}

func evaluateConfigurationImport(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, data []byte, rows []ConfigurationCSVRow, importID string) (ConfigurationImportReportV1, error) {
	fileHash := sha256.Sum256(data)
	report := ConfigurationImportReportV1{SchemaVersion: 1, FileSHA256: hex.EncodeToString(fileHash[:]), Rows: make([]ConfigurationImportRowV1, len(rows))}
	decoded := make([]configurationRowValue, len(rows))
	rowErrors := make([]error, len(rows))
	includeID := make([]bool, len(rows))
	refStates := make(map[string]struct{ status, id string })
	refIndexes := make(map[string][]int)
	for i, row := range rows {
		report.Rows[i] = ConfigurationImportRowV1{Row: row.Number, Status: "rejected"}
		if row.Kind == "line" || row.Kind == "item" || row.Kind == "shift_calendar" || row.Kind == "routing" {
			report.Rows[i].Kind = row.Kind
		}
		if configurationRef.MatchString(row.Ref) && !canonicalID(row.Ref) && !configurationFormulaCell(row.Ref) {
			report.Rows[i].Ref = row.Ref
		}
		decoded[i], rowErrors[i] = decodeConfigurationRow(row)
		if rowErrors[i] != nil {
			setConfigurationRowError(&report.Rows[i], rowErrors[i])
			continue
		}
		expected := decoded[i].Revision
		report.Rows[i].ExpectedRevision = &expected
		if row.Ref != "" {
			refIndexes[row.Kind+"\x00"+row.Ref] = append(refIndexes[row.Kind+"\x00"+row.Ref], i)
		}
	}
	for key, indexes := range refIndexes {
		if len(indexes) > 1 {
			refStates[key] = struct{ status, id string }{status: "rejected"}
			for _, i := range indexes {
				setConfigurationRowError(&report.Rows[i], &ConfigurationImportRowError{Code: "DUPLICATE_REF", Path: "ref"})
			}
		}
	}
	states, stateSHA, err := loadConfigurationImportState(ctx, tx, sc)
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	nameInFile := map[string]map[string]bool{}
	createNamesInFile := map[string]map[string]bool{}
	nameCounts := map[string]map[string]int{}
	digestMatches := map[string]map[string]map[string]configurationImportExisting{}
	for kind, kindState := range states {
		nameCounts[kind] = make(map[string]int)
		digestMatches[kind] = make(map[string]map[string]configurationImportExisting)
		for id, existing := range kindState {
			nameCounts[kind][existing.Name]++
			if digestMatches[kind][existing.Digest] == nil {
				digestMatches[kind][existing.Digest] = make(map[string]configurationImportExisting)
			}
			digestMatches[kind][existing.Digest][id] = existing
		}
	}
	for i, row := range rows {
		if row.ID != "" || rowErrors[i] != nil || report.Rows[i].Reason != "" {
			continue
		}
		if createNamesInFile[row.Kind] == nil {
			createNamesInFile[row.Kind] = make(map[string]bool)
		}
		createNamesInFile[row.Kind][configurationDisplayName(decoded[i].Body)] = true
	}
	for _, kind := range []string{"line", "item", "shift_calendar", "routing"} {
		for i, row := range rows {
			if row.Kind != kind {
				continue
			}
			result := &report.Rows[i]
			refKey := row.Kind + "\x00" + row.Ref
			if rowErrors[i] != nil || result.Reason != "" {
				if row.Ref != "" && configurationRef.MatchString(row.Ref) {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			body := decoded[i].Body
			if kind == "routing" {
				routing := body.(RoutingBodyV1)
				lineID, lineStatus := resolveConfigurationReference(row.Line, "line", refStates)
				if lineStatus == "rejected" {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "REFERENCE_REJECTED", Path: "line"})
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
					continue
				}
				if lineStatus == "missing" {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "UNKNOWN_REF", Path: "line"})
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
					continue
				}
				itemID, itemStatus := resolveConfigurationReference(row.Item, "item", refStates)
				if itemStatus == "rejected" {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "REFERENCE_REJECTED", Path: "item"})
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
					continue
				}
				if itemStatus == "missing" {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "UNKNOWN_REF", Path: "item"})
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
					continue
				}
				routing.LineID, routing.ItemID = lineID, itemID
				body = routing
			}
			normalized, encoded, digest, path, normalizeErr := normalizeConfigurationImportBody(sc, kind, body)
			if normalizeErr != nil {
				setConfigurationRowError(result, &ConfigurationImportRowError{Code: "INVALID_MASTER_DATA", Path: path})
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			result.DisplayName = configurationDisplayName(normalized)
			if decoded[i].ID == "" {
				if nameInFile[kind] == nil {
					nameInFile[kind] = make(map[string]bool)
				}
				if nameInFile[kind][result.DisplayName] {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "DUPLICATE_IN_FILE", Path: "display_name"})
					if row.Ref != "" {
						refStates[refKey] = struct{ status, id string }{status: "rejected"}
					}
					continue
				}
				nameInFile[kind][result.DisplayName] = true
			}
			kindState := states[kind]
			if decoded[i].ID != "" {
				existing, ok := kindState[decoded[i].ID]
				if !ok {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "NOT_FOUND", Path: "id"})
					continue
				}
				if existing.Revision != decoded[i].Revision {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "REVISION_CONFLICT", Path: "revision"})
					continue
				}
				nameCollisions := nameCounts[kind][result.DisplayName]
				if existing.Name == result.DisplayName {
					nameCollisions--
				}
				if nameCollisions > 0 || createNamesInFile[kind][result.DisplayName] {
					setConfigurationRowError(result, &ConfigurationImportRowError{Code: "DISPLAY_NAME_EXISTS", Path: "display_name"})
					if row.Ref != "" {
						refStates[refKey] = struct{ status, id string }{status: "rejected"}
					}
					continue
				}
				result.ExpectedRevision = &existing.Revision
				if existing.Digest == digest {
					result.Status = "duplicate"
					result.ObjectID = existing.ID
					result.ExpectedRevision = &existing.Revision
					includeID[i] = true
					if row.Ref != "" {
						refStates[refKey] = struct{ status, id string }{status: "duplicate", id: existing.ID}
					}
					continue
				}
				rowError, fatal := appendConfigurationImportRow(ctx, tx, a, sc, importID, report.FileSHA256, row, decoded[i], normalized, encoded, digest, kind)
				if fatal != nil {
					return ConfigurationImportReportV1{}, fatal
				}
				if rowError != nil {
					setConfigurationRowError(result, rowError)
					if row.Ref != "" {
						refStates[refKey] = struct{ status, id string }{status: "rejected"}
					}
					continue
				}
				result.Status, result.ObjectID = "accepted", existing.ID
				result.ExpectedRevision = &existing.Revision
				includeID[i] = true
				delete(digestMatches[kind][existing.Digest], existing.ID)
				if len(digestMatches[kind][existing.Digest]) == 0 {
					delete(digestMatches[kind], existing.Digest)
				}
				if existing.Name != result.DisplayName {
					nameCounts[kind][existing.Name]--
					nameCounts[kind][result.DisplayName]++
				}
				updated := configurationImportExisting{ID: existing.ID, Revision: existing.Revision + 1, Digest: digest, Name: result.DisplayName}
				kindState[existing.ID] = updated
				if digestMatches[kind][digest] == nil {
					digestMatches[kind][digest] = make(map[string]configurationImportExisting)
				}
				digestMatches[kind][digest][existing.ID] = updated
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "accepted", id: existing.ID}
				}
				continue
			}
			matches := digestMatches[kind][digest]
			if len(matches) > 1 {
				setConfigurationRowError(result, &ConfigurationImportRowError{Code: "AMBIGUOUS_DUPLICATE", Path: "display_name"})
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			if len(matches) == 1 {
				for id, existing := range matches {
					result.Status, result.ObjectID = "duplicate", id
					result.ExpectedRevision = &existing.Revision
				}
				includeID[i] = true
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "duplicate", id: result.ObjectID}
				}
				continue
			}
			generatedID := configurationImportObjectID(sc, report.FileSHA256, row.Number, kind)
			if _, exists := kindState[generatedID]; exists {
				// A current deterministic ID means a prior create was changed outside this replay.
				setConfigurationRowError(result, &ConfigurationImportRowError{Code: "OBJECT_ALREADY_CREATED", Path: "ref"})
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			if nameCounts[kind][result.DisplayName] > 0 {
				setConfigurationRowError(result, &ConfigurationImportRowError{Code: "DISPLAY_NAME_EXISTS", Path: "display_name"})
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			rowError, fatal := appendConfigurationImportRow(ctx, tx, a, sc, importID, report.FileSHA256, row, decoded[i], normalized, encoded, digest, kind)
			if fatal != nil {
				return ConfigurationImportReportV1{}, fatal
			}
			if rowError != nil {
				setConfigurationRowError(result, rowError)
				if row.Ref != "" {
					refStates[refKey] = struct{ status, id string }{status: "rejected"}
				}
				continue
			}
			result.Status, result.ObjectID = "accepted", generatedID
			expected := int64(0)
			result.ExpectedRevision = &expected
			created := configurationImportExisting{ID: generatedID, Revision: 1, Digest: digest, Name: result.DisplayName}
			kindState[generatedID] = created
			nameCounts[kind][result.DisplayName]++
			if digestMatches[kind][digest] == nil {
				digestMatches[kind][digest] = make(map[string]configurationImportExisting)
			}
			digestMatches[kind][digest][generatedID] = created
			if row.Ref != "" {
				refStates[refKey] = struct{ status, id string }{status: "accepted", id: generatedID}
			}
		}
	}
	for i := range report.Rows {
		switch report.Rows[i].Status {
		case "accepted":
			report.Counts.Accepted++
		case "duplicate":
			report.Counts.Duplicate++
		default:
			report.Counts.Rejected++
		}
	}
	report.Applicable = report.Counts.Rejected == 0
	digestRows := make([]configurationImportDigestRow, len(report.Rows))
	for i, row := range report.Rows {
		objectID := ""
		if includeID[i] {
			objectID = row.ObjectID
		}
		digestRows[i] = configurationImportDigestRow{Row: row.Row, Kind: row.Kind, Ref: row.Ref, Status: row.Status, Reason: row.Reason, Path: row.Path, ObjectID: objectID, ExpectedRevision: row.ExpectedRevision}
	}
	_, digest, err := canonical.Envelope("ConfigurationImportReportV1", 1, sc.TenantID, sc.SiteID, configurationImportDigestBody{FileSHA256: report.FileSHA256, SiteStateSHA256: stateSHA, Rows: digestRows})
	if err != nil {
		return ConfigurationImportReportV1{}, err
	}
	report.ReportSHA256 = digest
	return report, nil
}

func ValidConfigurationImportETag(value string) bool {
	if len(value) != 66 || value[0] != '"' || value[65] != '"' {
		return false
	}
	for _, c := range value[1:65] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func setConfigurationRowError(row *ConfigurationImportRowV1, err error) {
	row.Status = "rejected"
	var rowErr *ConfigurationImportRowError
	if errors.As(err, &rowErr) {
		row.Reason, row.Path = rowErr.Code, rowErr.Path
		return
	}
	var masterErr *MasterDataValidationError
	if errors.As(err, &masterErr) {
		row.Reason, row.Path = "INVALID_MASTER_DATA", masterErr.Path
		return
	}
	row.Reason = "INVALID_VALUE"
}

func resolveConfigurationReference(value, kind string, refs map[string]struct{ status, id string }) (string, string) {
	if canonicalID(value) {
		return value, "ok"
	}
	state, ok := refs[kind+"\x00"+value]
	if !ok {
		return "", "missing"
	}
	if state.status == "rejected" || state.id == "" {
		return "", "rejected"
	}
	return state.id, "ok"
}

func normalizeConfigurationImportBody(sc Scope, kind string, value any) (any, []byte, string, string, error) {
	var body any
	var spec masterDataSpec
	var err error
	switch b := value.(type) {
	case LineBodyV1:
		spec = lineMasterData
		body, err = normalizeMasterBody(kind, b)
	case ItemBodyV1:
		spec = itemMasterData
		body, err = normalizeMasterBody(kind, b)
	case ShiftCalendarBodyV1:
		spec = calendarMasterData
		body, err = normalizeMasterBody(kind, b)
	case RoutingBodyV1:
		spec = routingMasterData
		body, err = normalizeMasterBody(kind, b)
	default:
		return nil, nil, "", "", invalidMasterData("")
	}
	if err != nil {
		var invalid *MasterDataValidationError
		if errors.As(err, &invalid) {
			return nil, nil, "", invalid.Path, err
		}
		return nil, nil, "", "", err
	}
	encoded, digest, err := canonical.Envelope(spec.schema, 1, sc.TenantID, sc.SiteID, body)
	return body, encoded, digest, "", err
}

func configurationDisplayName(value any) string {
	switch b := value.(type) {
	case LineBodyV1:
		return b.DisplayName
	case ItemBodyV1:
		return b.DisplayName
	case ShiftCalendarBodyV1:
		return b.DisplayName
	case RoutingBodyV1:
		return b.DisplayName
	default:
		return ""
	}
}

func configurationImportObjectID(sc Scope, fileSHA string, row int, kind string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("configuration-import:%s:%s:%s:%d:%s", sc.TenantID, sc.SiteID, fileSHA, row, kind))).String()
}

func appendConfigurationImportRow(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, importID, fileSHA string, row ConfigurationCSVRow, decoded configurationRowValue, body any, encoded []byte, digest, kind string) (*ConfigurationImportRowError, error) {
	if decoded.ID == "" && kind != "routing" {
		generatedID, key := configurationImportObjectID(sc, fileSHA, row.Number, kind), fmt.Sprintf("cfgimp:%s:%d", importID, row.Number)
		var err error
		switch b := body.(type) {
		case LineBodyV1:
			err = appendConfigurationImportCreateTx(ctx, tx, a, sc, b, lineMasterData, generatedID, key, encoded, digest)
		case ItemBodyV1:
			err = appendConfigurationImportCreateTx(ctx, tx, a, sc, b, itemMasterData, generatedID, key, encoded, digest)
		case ShiftCalendarBodyV1:
			err = appendConfigurationImportCreateTx(ctx, tx, a, sc, b, calendarMasterData, generatedID, key, encoded, digest)
		default:
			err = invalidMasterData("")
		}
		if err == nil {
			return nil, nil
		}
		var sqlErr interface{ SQLState() string }
		if errors.As(err, &sqlErr) {
			return nil, err
		}
		return configurationImportAppendError(err, true), nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT cfg_row`); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("cfgimp:%s:%d", importID, row.Number)
	generatedID := configurationImportObjectID(sc, fileSHA, row.Number, kind)
	var err error
	switch b := body.(type) {
	case LineBodyV1:
		_, err = appendMasterDataTx(ctx, tx, a, sc, decoded.ID, b, key, decoded.Revision, lineMasterData)
	case ItemBodyV1:
		_, err = appendMasterDataTx(ctx, tx, a, sc, decoded.ID, b, key, decoded.Revision, itemMasterData)
	case ShiftCalendarBodyV1:
		_, err = appendMasterDataTx(ctx, tx, a, sc, decoded.ID, b, key, decoded.Revision, calendarMasterData)
	case RoutingBodyV1:
		if decoded.ID == "" {
			_, err = appendMasterDataTx(ctx, tx, a, sc, "", b, key, decoded.Revision, routingMasterData, generatedID)
		} else {
			_, err = appendMasterDataTx(ctx, tx, a, sc, decoded.ID, b, key, decoded.Revision, routingMasterData)
		}
	default:
		err = invalidMasterData("")
	}
	if err != nil {
		rowError := configurationImportAppendError(err, decoded.ID == "")
		if rowError == nil {
			if rollbackErr := rollbackConfigurationRow(ctx, tx); rollbackErr != nil {
				return nil, rollbackErr
			}
			return nil, err
		}
		if rollbackErr := rollbackConfigurationRow(ctx, tx); rollbackErr != nil {
			return nil, rollbackErr
		}
		return rowError, nil
	}
	if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT cfg_row`); err != nil {
		return nil, err
	}
	return nil, nil
}

func appendConfigurationImportCreateTx[T any](ctx context.Context, tx *sql.Tx, a Actor, sc Scope, body T, spec masterDataSpec, id, key string, encoded []byte, digest string) error {
	if err := validateMasterDataReference(ctx, tx, sc, id, body); err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return invalidMasterData("")
	}
	requestHash := requestDigest([]any{"master_data." + spec.kind + ".create", "", int64(0), digest})
	// The batch receipt is checked before row appends, so each new row needs only its receipt insert.
	_, err = tx.ExecContext(ctx, `WITH head AS (
		INSERT INTO supervisor.`+spec.heads+`(tenant_id,site_id,id,current_revision) VALUES($1,$2,$3,1) RETURNING id
	), revision AS (
		INSERT INTO supervisor.`+spec.revisions+`(tenant_id,site_id,object_id,revision,payload,canonical_payload,canonical_payload_sha256,digest_schema_version,actor_issuer,actor_subject)
		SELECT $1,$2,id,1,$4::jsonb,$5,$6,1,$7,$8 FROM head RETURNING object_id
	)
	INSERT INTO supervisor.master_data_receipts(tenant_id,site_id,actor_issuer,actor_subject,idempotency_key,request_sha256,object_kind,object_id,revision)
	SELECT $1,$2,$7,$8,$9,$10,$11,object_id,1 FROM revision`, sc.TenantID, sc.SiteID, id, string(payload), string(encoded), digest, a.Issuer, a.Subject, key, requestHash, spec.kind)
	return err
}

func rollbackConfigurationRow(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT cfg_row`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT cfg_row`)
	return err
}

func configurationImportAppendError(err error, create bool) *ConfigurationImportRowError {
	var invalid *MasterDataValidationError
	if errors.As(err, &invalid) {
		return &ConfigurationImportRowError{Code: "INVALID_MASTER_DATA", Path: invalid.Path}
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return &ConfigurationImportRowError{Code: "NOT_FOUND", Path: "id"}
	case errors.Is(err, ErrRevision):
		return &ConfigurationImportRowError{Code: "REVISION_CONFLICT", Path: "revision"}
	case errors.Is(err, ErrLineValidationBlocked):
		return &ConfigurationImportRowError{Code: "LINE_VALIDATION_BLOCKED"}
	case errors.Is(err, ErrMasterDataInUse):
		return &ConfigurationImportRowError{Code: "MASTER_DATA_IN_USE"}
	}
	var sqlErr interface{ SQLState() string }
	if errors.As(err, &sqlErr) {
		if create && sqlErr.SQLState() == "23505" {
			// A deterministic import ID already exists with content changed outside this replay.
			return &ConfigurationImportRowError{Code: "OBJECT_ALREADY_CREATED", Path: "ref"}
		}
		if strings.HasPrefix(sqlErr.SQLState(), "23") {
			return &ConfigurationImportRowError{Code: "INVALID_MASTER_DATA"}
		}
	}
	return nil
}

func loadConfigurationImportState(ctx context.Context, tx *sql.Tx, sc Scope) (map[string]map[string]configurationImportExisting, string, error) {
	states := map[string]map[string]configurationImportExisting{
		"line": {}, "item": {}, "shift_calendar": {}, "routing": {},
	}
	snapshot := make([]configurationImportSiteState, 0)
	for _, spec := range []masterDataSpec{lineMasterData, itemMasterData, calendarMasterData, routingMasterData} {
		rows, err := tx.QueryContext(ctx, `SELECT h.id::text,h.current_revision,r.canonical_payload_sha256,r.payload->>'display_name' FROM supervisor.`+spec.heads+` h JOIN supervisor.`+spec.revisions+` r ON (r.tenant_id,r.site_id,r.object_id,r.revision)=(h.tenant_id,h.site_id,h.id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 ORDER BY h.id`, sc.TenantID, sc.SiteID)
		if err != nil {
			return nil, "", err
		}
		for rows.Next() {
			var existing configurationImportExisting
			if err = rows.Scan(&existing.ID, &existing.Revision, &existing.Digest, &existing.Name); err != nil {
				rows.Close()
				return nil, "", err
			}
			states[spec.kind][existing.ID] = existing
			snapshot = append(snapshot, configurationImportSiteState{Kind: spec.kind, ID: existing.ID, Revision: existing.Revision, Digest: existing.Digest})
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, "", err
		}
		rows.Close()
	}
	rows, err := tx.QueryContext(ctx, `SELECT h.workstation_id::text,h.current_revision,r.canonical_payload_sha256 FROM supervisor.station_profile_heads h JOIN supervisor.station_profile_revisions r ON (r.tenant_id,r.site_id,r.workstation_id,r.revision)=(h.tenant_id,h.site_id,h.workstation_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2 ORDER BY h.workstation_id`, sc.TenantID, sc.SiteID)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var state configurationImportSiteState
		state.Kind = "workstation_profile"
		if err = rows.Scan(&state.ID, &state.Revision, &state.Digest); err != nil {
			rows.Close()
			return nil, "", err
		}
		snapshot = append(snapshot, state)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	rows.Close()
	var settingRev int64
	var settingDigest string
	err = tx.QueryRowContext(ctx, `SELECT h.current_revision,r.canonical_payload_sha256 FROM supervisor.site_master_data_setting_heads h JOIN supervisor.site_master_data_setting_revisions r ON (r.tenant_id,r.site_id,r.revision)=(h.tenant_id,h.site_id,h.current_revision) WHERE h.tenant_id=$1 AND h.site_id=$2`, sc.TenantID, sc.SiteID).Scan(&settingRev, &settingDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	if err == nil {
		snapshot = append(snapshot, configurationImportSiteState{Kind: "site_settings", ID: sc.SiteID, Revision: settingRev, Digest: settingDigest})
	}
	_, digest, err := canonical.Envelope("ConfigurationImportSiteStateV1", 1, sc.TenantID, sc.SiteID, snapshot)
	return states, digest, err
}
