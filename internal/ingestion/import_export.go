package ingestion

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// JobErrorsCSV exports persisted rejected and quarantined rows as CSV, only after the import
// has reached a terminal state. For a cancelled job (its source was revoked mid-import) it
// also lists every file row that was never processed as "unprocessed", IMPORT_CANCELLED, so a
// partial error list cannot be mistaken for a complete one (rule 07: no silent partial
// success). Access check, read and audit share one tenant transaction, so a
// membership revocation cannot slip between the check and the read, and every export leaves
// one audit row (actor, job, row count; never row content).
func (s *Service) JobErrorsCSV(ctx context.Context, a Actor, sc Scope, jobID string) ([]byte, error) {
	if !validID(jobID) {
		return nil, ErrInvalid
	}
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.readAccessTx(ctx, tx, a, sc); err != nil {
		return nil, err
	}

	// A job holds at most MaxImportRows rows: ParseImport rejects larger files before a job
	// exists and migration 026 checks row_count BETWEEN 1 AND 50000. Reading one row past the
	// bound turns a violated invariant into an explicit ErrImportTooLarge, never a silently
	// truncated export.
	rows, err := tx.QueryContext(ctx, `
SELECT j.status,j.source_id::text,j.source_revision,j.row_count,r.row_index,r.status,coalesce(r.reason,''),coalesce(r.source_sequence,0)
FROM supervisor.operational_import_jobs j
LEFT JOIN supervisor.operational_import_rows r
 ON r.tenant_id=j.tenant_id AND r.site_id=j.site_id AND r.job_id=j.id
 AND r.status IN ('quarantined','rejected')
WHERE j.tenant_id=$1 AND j.site_id=$2 AND j.id=$3
ORDER BY r.row_index
LIMIT $4`, sc.TenantID, sc.SiteID, jobID, MaxImportRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]ImportRow, 0)
	var found bool
	var sourceID string
	var sourceRevision int64
	var jobStatus string
	var rowCount int
	for rows.Next() {
		found = true
		var index sql.NullInt64
		var status sql.NullString
		var reason string
		var sequence int64
		if err = rows.Scan(&jobStatus, &sourceID, &sourceRevision, &rowCount, &index, &status, &reason, &sequence); err != nil {
			return nil, err
		}
		switch jobStatus {
		case "running":
			return nil, ErrImportRunning
		case "completed", "partial", "cancelled":
		default:
			return nil, errors.New("unknown import job status")
		}
		if !index.Valid {
			continue
		}
		result = append(result, ImportRow{Index: int(index.Int64), Status: status.String, Reason: reason, SourceSequence: sequence})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	if jobStatus == "cancelled" {
		if result, err = withUnprocessedRows(ctx, tx, sc, jobID, rowCount, result); err != nil {
			return nil, err
		}
	}
	if len(result) > MaxImportRows {
		return nil, ErrImportTooLarge
	}
	body, err := FormatErrorRowsCSV(result)
	if err != nil {
		// Persisted rows broke an invariant: a server fault, not the caller's invalid input.
		return nil, errors.New("persisted import rows cannot be exported")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_source_audit(tenant_id,site_id,actor_issuer,actor_subject,source_id,revision,event_type,source_revision,job_id,exported_rows) VALUES($1,$2,$3,$4,$5,$6,'import.errors_exported',$6,$7,$8)`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, sourceID, sourceRevision, jobID, len(result)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return body, nil
}

// UnprocessedReason marks a file row a cancelled import never reached.
const UnprocessedReason = "IMPORT_CANCELLED"

// withUnprocessedRows adds an "unprocessed" row for every index in 1..rowCount that the
// cancelled job never recorded, whatever its outcome would have been.
func withUnprocessedRows(ctx context.Context, tx *sql.Tx, sc Scope, jobID string, rowCount int, result []ImportRow) ([]ImportRow, error) {
	if rowCount < 1 || rowCount > MaxImportRows {
		return nil, errors.New("persisted import job row count out of range")
	}
	recorded := make([]bool, rowCount+1)
	rows, err := tx.QueryContext(ctx, `SELECT row_index FROM supervisor.operational_import_rows WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3`, sc.TenantID, sc.SiteID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		if err = rows.Scan(&index); err != nil {
			return nil, err
		}
		if index < 1 || index > rowCount {
			return nil, errors.New("persisted import row outside the job's file")
		}
		recorded[index] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for index := 1; index <= rowCount; index++ {
		if !recorded[index] {
			result = append(result, ImportRow{Index: index, Status: "unprocessed", Reason: UnprocessedReason})
		}
	}
	return result, nil
}

// FormatErrorRowsCSV writes a spreadsheet-safe, stable CSV export. Rows must be
// persisted error outcomes, or rows a cancelled import never processed; the input slice is
// not modified.
func FormatErrorRowsCSV(rows []ImportRow) ([]byte, error) {
	if len(rows) > MaxImportRows {
		return nil, ErrImportTooLarge
	}
	ordered := append([]ImportRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	var output bytes.Buffer
	w := csv.NewWriter(&output)
	if err := w.Write([]string{"row_index", "status", "reason", "source_sequence"}); err != nil {
		return nil, err
	}
	previous := 0
	for _, row := range ordered {
		if row.Index < 1 || row.Index > MaxImportRows || row.Index == previous ||
			(row.Status != "quarantined" && row.Status != "rejected" && row.Status != "unprocessed") || row.SourceSequence < 0 ||
			(row.Status == "unprocessed" && (row.Reason != UnprocessedReason || row.SourceSequence != 0)) {
			return nil, ErrInvalid
		}
		previous = row.Index
		if err := w.Write([]string{
			fmt.Sprint(row.Index),
			SpreadsheetSafeCell(row.Status),
			SpreadsheetSafeCell(row.Reason),
			fmt.Sprint(row.SourceSequence),
		}); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// SpreadsheetSafeCell prefixes a quote to values a spreadsheet could read as a formula: a
// leading tab or carriage return, or =+-@ after any leading whitespace.
func SpreadsheetSafeCell(value string) string {
	if value != "" && (value[0] == '\t' || value[0] == '\r') {
		return "'" + value
	}
	first := strings.TrimLeftFunc(value, unicode.IsSpace)
	if first != "" && strings.ContainsRune("=+-@", rune(first[0])) {
		return "'" + value
	}
	return value
}
