package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/operational"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/production"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Import limits (engineering defaults, WIS-272): 10 MiB and 50k rows per file.
const (
	MaxImportBytes = limits.ImportBytes
	MaxImportRows  = limits.ImportRows
)

// ErrImportTooLarge is returned for files over the byte or row limit.
var ErrImportTooLarge = errors.New("import exceeds the size or row limit")

// ImportRow is the recorded outcome of one file row.
type ImportRow struct {
	Index          int    `json:"row_index"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	SourceSequence int64  `json:"source_sequence,omitempty"`
}

// ImportJob is one durable import. Counts are exact; a partial job names what was not attempted.
type ImportJob struct {
	ID              string      `json:"job_id"`
	SourceID        string      `json:"source_id"`
	Format          string      `json:"format"`
	FileSHA256      string      `json:"file_sha256"`
	Status          string      `json:"status"`
	RowCount        int         `json:"row_count"`
	StartSequence   int64       `json:"start_sequence"`
	SourceRevision  int64       `json:"source_revision"`
	MappingRevision int64       `json:"mapping_revision"`
	AcceptedRows    int         `json:"accepted_rows"`
	DuplicateRows   int         `json:"duplicate_rows"`
	QuarantinedRows int         `json:"quarantined_rows"`
	RejectedRows    int         `json:"rejected_rows"`
	Rows            []ImportRow `json:"rows"`
	Replayed        bool        `json:"replayed"`
	CreatedAt       time.Time   `json:"created_at"`
	FinishedAt      *time.Time  `json:"finished_at"`
}

// ImportRequest is one authenticated import. Revisions are the source's current revisions;
// the caller reads them from the reviewed registry.
type ImportRequest struct {
	SourceID        string
	SourceRevision  int64
	MappingRevision int64
	Format          string
	Data            []byte
	Key             string
}

var csvRequired = []string{"external_key", "revision", "kind", "event_at", "payload_json"}
var csvOptional = []string{"asset_external_key", "order_external_key"}

// ParseImport decodes a CSV or JSON file into events without sequence numbers. It is pure:
// it reads nothing and writes nothing, so a malformed file is rejected before persistence.
func ParseImport(format string, data []byte) ([]Event, error) {
	if len(data) > MaxImportBytes {
		return nil, ErrImportTooLarge
	}
	if !utf8.Valid(data) {
		return nil, ErrInvalid
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	var events []Event
	var err error
	switch format {
	case "csv":
		events, err = parseCSVImport(data)
	case "json":
		events, err = parseJSONImport(data)
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrInvalid
	}
	if len(events) > MaxImportRows {
		return nil, ErrImportTooLarge
	}
	return events, nil
}

func parseCSVImport(data []byte) ([]Event, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	header, err := reader.Read()
	if err != nil {
		return nil, ErrInvalid
	}
	index := map[string]int{}
	for i, name := range header {
		if _, dup := index[name]; dup {
			return nil, ErrInvalid
		}
		index[name] = i
	}
	for _, name := range csvRequired {
		if _, ok := index[name]; !ok {
			return nil, ErrInvalid
		}
	}
	for name := range index {
		if !contains(csvRequired, name) && !contains(csvOptional, name) {
			return nil, ErrInvalid
		}
	}
	var events []Event
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrInvalid
		}
		if len(events) == MaxImportRows {
			return nil, ErrImportTooLarge
		}
		field := func(name string) string {
			if i, ok := index[name]; ok && i < len(record) {
				return record[i]
			}
			return ""
		}
		revision, err := strconv.ParseInt(field("revision"), 10, 64)
		if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != field("revision") {
			return nil, ErrInvalid
		}
		eventAt, err := time.Parse(time.RFC3339Nano, field("event_at"))
		if err != nil {
			return nil, ErrInvalid
		}
		payload := field("payload_json")
		if !json.Valid([]byte(payload)) {
			return nil, ErrInvalid
		}
		events = append(events, Event{
			ExternalKey:      field("external_key"),
			Revision:         revision,
			Kind:             field("kind"),
			EventAt:          eventAt,
			Payload:          json.RawMessage(payload),
			AssetExternalKey: field("asset_external_key"),
			OrderExternalKey: field("order_external_key"),
		})
	}
	return events, nil
}

type jsonImportRow struct {
	ExternalKey      string          `json:"external_key"`
	Revision         int64           `json:"revision"`
	Kind             string          `json:"kind"`
	EventAt          time.Time       `json:"event_at"`
	Payload          json.RawMessage `json:"payload"`
	AssetExternalKey string          `json:"asset_external_key,omitempty"`
	OrderExternalKey string          `json:"order_external_key,omitempty"`
}

func parseJSONImport(data []byte) ([]Event, error) {
	var rows []jsonImportRow
	if err := strictjson.Decode(data, &rows, MaxImportBytes); err != nil {
		return nil, ErrInvalid
	}
	if len(rows) > MaxImportRows {
		return nil, ErrImportTooLarge
	}
	events := make([]Event, len(rows))
	for i, r := range rows {
		events[i] = Event{ExternalKey: r.ExternalKey, Revision: r.Revision, Kind: r.Kind, EventAt: r.EventAt, Payload: r.Payload, AssetExternalKey: r.AssetExternalKey, OrderExternalKey: r.OrderExternalKey}
	}
	return events, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// importChunks assigns contiguous sequences from start and splits into atomic batches.
func importChunks(req ImportRequest, events []Event, start int64) ([]Batch, error) {
	var batches []Batch
	for offset := 0; offset < len(events); offset += MaxEvents {
		end := offset + MaxEvents
		if end > len(events) {
			end = len(events)
		}
		chunk := make([]Event, end-offset)
		copy(chunk, events[offset:end])
		for i := range chunk {
			chunk[i].SourceSequence = start + int64(offset+i)
		}
		batch := Batch{SchemaVersion: 1, SourceID: req.SourceID, SourceRevision: req.SourceRevision, MappingRevision: req.MappingRevision, Events: chunk}
		if err := batch.Validate(); err != nil {
			return nil, ErrInvalid
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// chunkReason maps a rejected chunk's error to a stable row reason code.
func chunkReason(err error) string {
	switch {
	case errors.Is(err, ErrRevision):
		return "REVISION_CHANGED"
	case errors.Is(err, ErrSequence):
		return "SEQUENCE_CONFLICT"
	case errors.Is(err, ErrSourceRevisionConflict):
		return "EVENT_REVISION_CONFLICT"
	case errors.Is(err, ErrConflict):
		return "IDEMPOTENCY_CONFLICT"
	case errors.Is(err, ErrDenied):
		return "PERMISSION_DENIED"
	case errors.Is(err, ErrNotFound):
		return "SOURCE_UNAVAILABLE"
	case errors.Is(err, ErrInvalid):
		return "INVALID_EVENT_BATCH"
	case errors.Is(err, ErrImportCancelled):
		return "IMPORT_CANCELLED"
	default:
		return "STORAGE_UNAVAILABLE"
	}
}

// digestImport identifies a request body: the same source, format and file. Source and
// mapping revisions are not part of it: they are read from the registry when the job is
// created and stored on the job, so a retry after a registry change replays the job instead
// of conflicting with itself.
func digestImport(req ImportRequest, fileSHA string, rows int) string {
	b, _ := json.Marshal(struct {
		Format   string `json:"format"`
		FileSHA  string `json:"file_sha256"`
		SourceID string `json:"source_id"`
		Rows     int    `json:"rows"`
	}{req.Format, fileSHA, req.SourceID, rows})
	return digest(b)
}

// ImportPageRows is how many row outcomes an import response carries; GET pages the rest.
const ImportPageRows = 100

// Import persists one file as a job.
//
// The whole file is validated before anything is written: every chunk's format and typed
// plans, in-file content conflicts, the source's approval and mapping currency, and conflicts
// with records already stored. Only then is the job created, and its start sequence is
// reserved under the source's sequence lock; while the job runs, live intake on that source
// is refused (ErrImportRunning), so the reserved range cannot be taken.
//
// Each 100-row chunk goes through the atomic batch intake with a key derived from the job,
// and its row outcomes are recorded as soon as it commits. The first failed chunk stops the
// import: its rows and all later rows are recorded as rejected, never dropped silently. A
// retry with the same key replays a finished job and resumes a running one.
func (s *Service) Import(ctx context.Context, a Actor, sc Scope, req ImportRequest) (ImportJob, error) {
	var out ImportJob
	if !validID(sc.TenantID) || !validID(sc.SiteID) || !validID(req.SourceID) || req.SourceRevision < 1 || req.MappingRevision < 1 ||
		!validText(req.Key, 128) || !validText(a.Issuer, 255) || !validText(a.Subject, 255) || !validText(a.SessionHash, 128) {
		return out, ErrInvalid
	}
	events, err := ParseImport(req.Format, req.Data)
	if err != nil {
		return out, err
	}
	fileSum := sha256.Sum256(req.Data)
	fileSHA := hex.EncodeToString(fileSum[:])
	requestHash := digestImport(req, fileSHA, len(events))

	if err = s.requireIntakeRole(ctx, a, sc); err != nil {
		return out, err
	}
	existing, found, err := s.findJob(ctx, a, sc, req.Key)
	if err != nil {
		return out, err
	}
	if found {
		if existing.requestHash != requestHash {
			return out, ErrConflict
		}
		if existing.status != "running" {
			out, err = s.loadJob(ctx, a, sc, existing.id, 0, ImportPageRows)
			out.Replayed = true
			return out, err
		}
		// An interrupted job resumes with the revisions and start it was created with;
		// committed chunks replay through their chunk keys.
		req.SourceRevision, req.MappingRevision = existing.sourceRevision, existing.mappingRevision
		return s.runJob(ctx, a, sc, existing.id, req, events, existing.start)
	}

	canonical, err := canonicalImport(req, events)
	if err != nil {
		return out, err
	}
	if err = s.preflightImport(ctx, sc, req, canonical); err != nil {
		return out, err
	}
	jobID := uuid.NewString()
	start, err := s.insertJob(ctx, a, sc, jobID, req, fileSHA, requestHash, len(events))
	if err != nil {
		return out, err
	}
	return s.runJob(ctx, a, sc, jobID, req, events, start)
}

// canonicalImport validates every chunk as intake would and returns the canonical events.
// Plans must decode, and the file must not carry two different contents for one
// (external_key, revision).
func canonicalImport(req ImportRequest, events []Event) ([]Event, error) {
	batches, err := importChunks(req, events, 1)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(events))
	seen := make(map[string]string, len(events))
	for _, batch := range batches {
		canonical, _, err := canonicalBatch(batch)
		if err != nil {
			return nil, ErrInvalid
		}
		for _, ev := range canonical.Events {
			if ev.Kind == "production_plan" {
				if _, err = production.DecodePlan(ev.Payload); err != nil {
					return nil, ErrInvalid
				}
			}
			identity := ev.ExternalKey + "\x00" + strconv.FormatInt(ev.Revision, 10)
			hash := eventDigest(ev)
			if prior, ok := seen[identity]; ok && prior != hash {
				return nil, ErrSourceRevisionConflict
			}
			seen[identity] = hash
			out = append(out, ev)
		}
	}
	return out, nil
}

// preflightImport checks, before any write, what intake would check per chunk: the source is
// approved at the request's revisions, its mappings are current, no import is running, and no
// stored record has different content for an (external_key, revision) in the file.
func (s *Service) preflightImport(ctx context.Context, sc Scope, req ImportRequest, events []Event) error {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var srcRev, mapRev, headMapRev, reviewSrc, reviewMap int64
	var enabled bool
	var authorityScope, configHash, mapHash, reviewConfig, reviewMapping, verdict string
	err = tx.QueryRowContext(ctx, operational.SourceStateSQL, sc.TenantID, sc.SiteID, req.SourceID).Scan(&srcRev, &mapRev, &headMapRev, &enabled, &authorityScope, &configHash, &mapHash, &reviewSrc, &reviewMap, &reviewConfig, &reviewMapping, &verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !operational.SourceCurrent(enabled, srcRev, mapRev, headMapRev, reviewSrc, reviewMap, verdict, configHash, mapHash, reviewConfig, reviewMapping, req.SourceRevision, req.MappingRevision) {
		return ErrRevision
	}
	var stale int
	if err = tx.QueryRowContext(ctx, operational.StaleMappingSQL, sc.TenantID, sc.SiteID, req.SourceID, mapRev).Scan(&stale); err != nil {
		return err
	}
	if stale > 0 {
		return ErrRevision
	}
	// An import abandoned past its lease no longer blocks a new one (WIS-699).
	if err = cancelStaleImports(ctx, tx, sc, req.SourceID); err != nil {
		return err
	}
	var running bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND status='running')`, sc.TenantID, sc.SiteID, req.SourceID).Scan(&running); err != nil {
		return err
	}
	if running {
		return ErrImportRunning
	}
	keys, revisions, hashes := make([]string, len(events)), make([]int64, len(events)), make([]string, len(events))
	for i, ev := range events {
		keys[i], revisions[i], hashes[i] = ev.ExternalKey, ev.Revision, eventDigest(ev)
	}
	var conflict bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.operational_event_records r
 JOIN unnest($4::text[],$5::bigint[],$6::text[]) AS u(k,rev,h) ON r.external_key=u.k AND r.revision=u.rev
 WHERE r.tenant_id=$1 AND r.site_id=$2 AND r.source_id=$3 AND r.event_sha256<>u.h)`, sc.TenantID, sc.SiteID, req.SourceID, keys, revisions, hashes).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return ErrSourceRevisionConflict
	}
	return tx.Commit()
}

// runJob persists the chunks of a created or resumed job and records every row's outcome.
func (s *Service) runJob(ctx context.Context, a Actor, sc Scope, jobID string, req ImportRequest, events []Event, start int64) (_ ImportJob, err error) {
	// WIS-719: an import that cannot finish (its request was interrupted, or a store call failed)
	// ends cancelled at once. Left 'running', it blocked every live batch and import for its source
	// until importLease passed.
	defer func() {
		if err != nil {
			if e := s.abandonJob(ctx, sc, req.SourceID, jobID); e != nil {
				err = errors.Join(err, e)
			}
		}
	}()
	batches, err := importChunks(req, events, start)
	if err != nil {
		return ImportJob{}, err
	}
	outcomes := make([]ImportRow, 0, len(events))
	failed := ""
	for i, batch := range batches {
		first := i * MaxEvents
		var rows []ImportRow
		reason := "NOT_ATTEMPTED"
		if failed == "" {
			result, err := s.acceptBatch(ctx, a, sc, req.SourceID, batch, chunkIdempotencyKey(jobID, i), jobID)
			if err != nil {
				failed = chunkReason(err)
				reason = failed
			} else if rows, err = s.batchRows(ctx, sc, req.SourceID, result.BatchID, start, len(batch.Events)); err != nil {
				return ImportJob{}, err
			}
		}
		if rows == nil {
			// The failed chunk carries its reason; later chunks were not attempted.
			for j := range batch.Events {
				rows = append(rows, ImportRow{Index: first + j + 1, Status: "rejected", Reason: reason})
			}
		}
		if err = s.recordRows(ctx, sc, jobID, rows); errors.Is(err, ErrImportCancelled) {
			// The source was revoked, or the import abandoned, while it ran: the job is already final.
			return s.loadJob(ctx, a, sc, jobID, 0, ImportPageRows)
		} else if err != nil {
			return ImportJob{}, err
		}
		outcomes = append(outcomes, rows...)
	}
	if err = s.finishJob(ctx, a, sc, jobID, outcomes); err != nil {
		return ImportJob{}, err
	}
	return s.loadJob(ctx, a, sc, jobID, 0, ImportPageRows)
}

// chunkIdempotencyKey is stable per job and chunk, so a retried import replays each chunk.
func chunkIdempotencyKey(jobID string, chunk int) string {
	sum := sha256.Sum256([]byte(jobID + ":" + strconv.Itoa(chunk)))
	return hex.EncodeToString(sum[:])
}

// Job reads one page of a job's rows (after is the last row index already seen).
func (s *Service) Job(ctx context.Context, a Actor, sc Scope, jobID string, after, limit int) (ImportJob, error) {
	if !validID(jobID) || after < 0 || limit < 1 || limit > 100 {
		return ImportJob{}, ErrInvalid
	}
	if err := s.requireReadAccess(ctx, a, sc); err != nil {
		return ImportJob{}, err
	}
	return s.loadJob(ctx, a, sc, jobID, after, limit)
}

type jobRecord struct {
	id              string
	requestHash     string
	start           int64
	status          string
	sourceRevision  int64
	mappingRevision int64
}

func (s *Service) findJob(ctx context.Context, a Actor, sc Scope, key string) (jobRecord, bool, error) {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return jobRecord{}, false, err
	}
	defer tx.Rollback()
	var j jobRecord
	err = tx.QueryRowContext(ctx, `SELECT id::text,request_sha256,start_sequence,status,source_revision,mapping_revision FROM supervisor.operational_import_jobs WHERE tenant_id=$1 AND site_id=$2 AND actor_issuer=$3 AND actor_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&j.id, &j.requestHash, &j.start, &j.status, &j.sourceRevision, &j.mappingRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return jobRecord{}, false, nil
	}
	if err != nil {
		return jobRecord{}, false, err
	}
	return j, true, tx.Commit()
}

// insertJob creates a running job and reserves its start sequence under the source's sequence
// lock, the same lock intake takes. Intake refuses live batches while the job runs, and the
// partial unique index allows one running import per source.
func (s *Service) insertJob(ctx context.Context, a Actor, sc Scope, jobID string, req ImportRequest, fileSHA, requestHash string, rows int) (int64, error) {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_event_sequence_heads(tenant_id,site_id,source_id,last_sequence) VALUES($1,$2,$3,0) ON CONFLICT DO NOTHING`, sc.TenantID, sc.SiteID, req.SourceID); err != nil {
		return 0, err
	}
	var last int64
	if err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM supervisor.operational_event_sequence_heads WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 FOR UPDATE`, sc.TenantID, sc.SiteID, req.SourceID).Scan(&last); err != nil {
		return 0, err
	}
	start := last + 1
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.operational_import_jobs(tenant_id,site_id,id,source_id,format,file_sha256,request_sha256,idempotency_key,actor_issuer,actor_subject,source_revision,mapping_revision,start_sequence,row_count,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'running')`, sc.TenantID, sc.SiteID, jobID, req.SourceID, req.Format, fileSHA, requestHash, req.Key, a.Issuer, a.Subject, req.SourceRevision, req.MappingRevision, start, rows); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "operational_import_one_running" {
			return 0, ErrImportRunning
		}
		return 0, err
	}
	return start, tx.Commit()
}

// batchRows reads each row's outcome from the persisted batch: a duplicate is a
// DUPLICATE_EVENT quarantine, any other quarantine keeps its reason, and the rest are accepted.
func (s *Service) batchRows(ctx context.Context, sc Scope, sourceID, batchID string, start int64, count int) ([]ImportRow, error) {
	tx, err := s.tenantTx(ctx, sc.TenantID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT source_sequence,coalesce(quarantine_reason,'') FROM supervisor.operational_event_records WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND batch_id=$4 ORDER BY source_sequence`, sc.TenantID, sc.SiteID, sourceID, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportRow
	for rows.Next() {
		var seq int64
		var reason string
		if err = rows.Scan(&seq, &reason); err != nil {
			return nil, err
		}
		row := ImportRow{Index: int(seq-start) + 1, SourceSequence: seq, Status: "accepted"}
		switch {
		case reason == "DUPLICATE_EVENT":
			row.Status, row.Reason = "duplicate", reason
		case reason != "":
			row.Status, row.Reason = "quarantined", reason
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != count {
		return nil, errors.New("import batch rows incomplete")
	}
	return out, nil
}
