package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// WIS-625 slice B1: a photographed page is read by a local vision model in an agent run, never in
// the upload request. QueueScan stores the job (with the image) under the uploader's session; the
// worker finishes it with CompleteScanTx or QuarantineScanTx, which are bound to the job and its
// run: they take no actor, read the uploader from the job and re-check that uploader's membership
// in the same transaction (rule 03: a revocation reaches queued work).

// Extraction records that a model extracted a revision's text, with what.
type Extraction struct {
	Method        string `json:"method"` // model_ocr
	Model         string `json:"model"`
	ModelRevision string `json:"model_revision"`
	PromptSHA256  string `json:"prompt_sha256"`
	RunID         string `json:"run_id"`
}

// Scan quarantine reasons (migration 076's enumerated CHECK).
const (
	ScanIllegible        = "PAGE_ILLEGIBLE"
	ScanTruncated        = "PAGE_TRUNCATED"
	ScanOutputInvalid    = "OUTPUT_INVALID"
	ScanToolNotAllowed   = "TOOL_NOT_ALLOWED"
	ScanFailed           = "EXTRACTION_FAILED"
	ScanUploaderRevoked  = "UPLOADER_NOT_AUTHORIZED"
	ScanTextTooLarge     = "TEXT_TOO_LARGE"
	ScanBudgetExhausted  = "BUDGET_EXHAUSTED"
	ScanRunCanceled      = "RUN_CANCELED"
	scanImageMaxBytes    = 4 << 20
	ExtractionModelOCR   = "model_ocr"
	scanReceiptKeyPrefix = "scan:"
)

var scanReasons = map[string]bool{ScanIllegible: true, ScanTruncated: true, ScanOutputInvalid: true, ScanToolNotAllowed: true, ScanFailed: true,
	ScanUploaderRevoked: true, ScanTextTooLarge: true, ScanBudgetExhausted: true, ScanRunCanceled: true}

// ScanInput is one verified page image. The caller has checked the bytes (type, size, pixel bound).
type ScanInput struct {
	Title          string
	MediaType      string
	Width, Height  int
	Image          []byte
	OriginalSHA256 string
}

// ScanJob is a scan's state as its uploader and the site's members read it. The image is never
// returned.
type ScanJob struct {
	JobID            string     `json:"job_id"`
	RunID            string     `json:"run_id"`
	State            string     `json:"state"` // running | completed | quarantined
	QuarantineReason string     `json:"quarantine_reason,omitempty"`
	Title            string     `json:"title"`
	MediaType        string     `json:"media_type"`
	Width            int        `json:"width"`
	Height           int        `json:"height"`
	OriginalSHA256   string     `json:"original_sha256" contract:"sha256"`
	SourceID         string     `json:"source_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

// ErrScansUnavailable: a scan job would have to be queued, and the caller has no vision model to
// start its run with. Replays need none.
var ErrScansUnavailable = errors.New("SCANS_UNAVAILABLE")

// ErrScanQueueFull: queuing the pages would take the tenant past its cap on running scan pages
// (WIS-715). Nothing is stored; the same key can be retried once pages finish.
var ErrScanQueueFull = errors.New("SCAN_QUEUE_FULL")

// DefaultScanTenantCap is the running scan pages a tenant may hold: two full scanned PDFs (WIS-715).
const DefaultScanTenantCap = 40

// StartRun creates the extraction run for a new job inside QueueScan's transaction and returns
// its ID. It is supplied by the caller (onboarding), which owns the agent run settings.
type StartRun func(ctx context.Context, tx *sql.Tx, jobID string) (runID string, err error)

func validScan(in ScanInput) bool {
	return text(in.Title, 200) && (in.MediaType == "image/png" || in.MediaType == "image/jpeg") && in.Width > 0 && in.Width <= 20000 &&
		in.Height > 0 && in.Height <= 20000 && len(in.Image) > 0 && len(in.Image) <= scanImageMaxBytes && lowerHexSHA256(in.OriginalSHA256)
}

// QueueScan stores a scan job, its extraction run and its outbox row in one transaction, as the
// uploader under the evidence write authority. The same key with the same file replays the job;
// another file under that key is ErrConflict.
func (s *Service) QueueScan(ctx context.Context, a Actor, sc Scope, in ScanInput, key string, start StartRun) (ScanJob, error) {
	jobs, err := s.queue(ctx, a, sc, []ScanInput{in}, []string{key}, nil, start, "")
	if err != nil {
		return ScanJob{}, err
	}
	return jobs[0], nil
}

// ScanBatch is the jobs of one scanned PDF, one per page in page order (WIS-625 slice B2).
type ScanBatch struct {
	Jobs []ScanJob `json:"jobs"`
	// A PDF with typed and scanned pages (WIS-645) also stores its typed pages as one document
	// source, typed text and never model-extracted, linked with the page jobs by UploadID. Both are
	// absent for a PDF whose every page is scanned.
	UploadID *string   `json:"upload_id,omitempty" contract:"uuid"`
	Document *Document `json:"document,omitempty"`
}

// QueueScanBatch queues every page of one scanned PDF in one transaction: all pages or none. Page
// i is keyed batch:<sha256 of key>#i, which any valid key fits within the mutation key limit
// (WIS-625 review: key#i did not), so the same key with the same pages replays the batch, and any
// other pages under it are ErrConflict. A batch stored before that change under key#i is found
// under that key too, so a retry across the upgrade replays it instead of queuing it again.
func (s *Service) QueueScanBatch(ctx context.Context, a Actor, sc Scope, pages []ScanInput, key string, start StartRun) (ScanBatch, error) {
	if len(pages) == 0 || len(pages) > 64 || !mutation(key, 0) {
		return ScanBatch{}, ErrInvalid
	}
	keys, legacy := make([]string, len(pages)), make([]string, len(pages))
	for i := range pages {
		keys[i] = "batch:" + hash([]byte(key)) + "#" + strconv.Itoa(i+1)
		if old := key + "#" + strconv.Itoa(i+1); mutation(old, 0) {
			legacy[i] = old
		}
	}
	jobs, err := s.queue(ctx, a, sc, pages, keys, legacy, start, key)
	return ScanBatch{Jobs: jobs}, err
}

// queue stores each input under keys[i], replaying a job already stored under keys[i] or, when
// set, legacy[i]. A batch passes its request key as batchKey: a mixed upload already stored under
// that key is another upload, so the batch is ErrConflict (WIS-645: both shapes share an endpoint).
func (s *Service) queue(ctx context.Context, a Actor, sc Scope, ins []ScanInput, keys, legacy []string, start StartRun, batchKey string) ([]ScanJob, error) {
	for i, in := range ins {
		if !validScan(in) || !mutation(keys[i], 0) {
			return nil, ErrInvalid
		}
	}
	// start may be nil (no vision model): a replay needs no new run; queueTx refuses a new job.
	tx, err := s.begin(ctx, a, sc, true, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if batchKey != "" {
		var mixed bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.document_uploads WHERE tenant_id=$1 AND site_id=$2 AND uploader_issuer=$3 AND uploader_subject=$4 AND idempotency_key=$5)`,
			sc.TenantID, sc.SiteID, a.Issuer, a.Subject, batchKey).Scan(&mixed); err != nil {
			return nil, err
		}
		if mixed {
			return nil, ErrConflict
		}
	}
	jobs, replayed, err := s.queueTx(ctx, tx, a, sc, ins, keys, legacy, start, "")
	if err != nil {
		return nil, err
	}
	if err = auditQueued(ctx, tx, a, sc, replayed); err != nil {
		return nil, err
	}
	return jobs, s.finish(ctx, tx, a, false)
}

// auditQueued records a scan queue as scan.queued, or as mutation.replayed when its jobs replayed.
func auditQueued(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, replayed int) error {
	event := "scan.queued"
	if replayed > 0 {
		event = "mutation.replayed"
	}
	return audit(ctx, tx, a, sc, nil, nil, event, nil)
}

// presentedVersion names the transform from an embedded image to the image queued for the model
// (resized to its placement at a fixed resolution, a JPEG kept JPEG at quality 92, turned by the
// page's /Rotate). It is part of every presented digest: a change to the transform bumps it, so a
// replay of a job presented the old way is a conflict, not a silent match.
const presentedVersion = "presented-v1"

// queueTx stores each input under keys[i] in tx, replaying a job already stored under keys[i] or,
// when set, legacy[i]: every page replays or none does. A new job names uploadID when it is set
// (a page of a mixed PDF, WIS-645). It returns the jobs in input order and how many replayed; the
// caller audits the queue. New jobs are admitted only within the tenant's scan cap (WIS-715): the
// first new page checks the whole input, so a batch is admitted whole or not at all, and a replay
// never reaches the check.
func (s *Service) queueTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, ins []ScanInput, keys, legacy []string, start StartRun, uploadID string) ([]ScanJob, int, error) {
	var err error
	jobs := make([]ScanJob, 0, len(ins))
	replayed := 0
	for i, in := range ins {
		var prior, priorDigest, priorTitle string
		var priorPresented sql.NullString
		// presented is the image as queued for the model (scaled and turned as the page shows it),
		// under its presentation version; a replay must queue the same one. A job stored before 078
		// has none and replays as before.
		presented := hash(append([]byte(presentedVersion+"\n"), in.Image...))
		lookup := func(key string) error {
			return tx.QueryRowContext(ctx, `SELECT job_id::text,original_sha256,title,presented_sha256 FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND site_id=$2 AND uploader_issuer=$3 AND uploader_subject=$4 AND idempotency_key=$5`,
				sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&prior, &priorDigest, &priorTitle, &priorPresented)
		}
		err = lookup(keys[i])
		if errors.Is(err, sql.ErrNoRows) && legacy != nil && legacy[i] != "" {
			err = lookup(legacy[i])
		}
		switch {
		case err == nil:
			if priorDigest != in.OriginalSHA256 || priorTitle != in.Title || priorPresented.Valid && priorPresented.String != presented {
				return nil, 0, ErrConflict
			}
			replayed++
		case errors.Is(err, sql.ErrNoRows):
			prior = ""
		default:
			return nil, 0, err
		}
		if prior == "" {
			if replayed > 0 {
				return nil, 0, ErrConflict // a replay must match every page, not some of them
			}
			if start == nil {
				return nil, 0, ErrScansUnavailable // a new job needs a run, and there is no vision model
			}
			if i == 0 { // an earlier replay would already be ErrConflict, so the first new page is page 0
				if err = admitScans(ctx, tx, sc, len(ins), s.scanCap); err != nil {
					return nil, 0, err
				}
			}
			jobID := uuid.NewString()
			runID, err := start(ctx, tx, jobID)
			if err != nil {
				return nil, 0, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.document_scan_jobs(tenant_id,site_id,job_id,uploader_issuer,uploader_subject,idempotency_key,title,original_sha256,media_type,width,height,image,run_id,presented_sha256,upload_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,'')::uuid)`, sc.TenantID, sc.SiteID, jobID, a.Issuer, a.Subject, keys[i], in.Title, in.OriginalSHA256, in.MediaType, in.Width, in.Height, in.Image, runID, presented, uploadID); err != nil {
				return nil, 0, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.document_scan_outbox(tenant_id,site_id,id,job_id,run_id) VALUES($1,$2,$3,$4,$5)`, sc.TenantID, sc.SiteID, uuid.NewString(), jobID, runID); err != nil {
				return nil, 0, err
			}
			prior = jobID
		}
		job, err := readScan(ctx, tx, sc, prior)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, job)
	}
	if replayed > 0 && replayed != len(ins) {
		return nil, 0, ErrConflict
	}
	return jobs, replayed, nil
}

// scanAdmissionWindow is how long a running page holds one of its tenant's places. A page
// normally ends well within it. A page still running after it was most likely left behind (the worker or relay was
// down, its workflow was terminated, or its history was lost), and it must not keep the tenant
// out for ever (WIS-715 review): it stops counting, as a lost chat or investigation run does
// (internal/chat/answer.go, internal/agent/runtime.go admitSite). The page itself stays visible
// as running; only its place is released.
var scanAdmissionWindow = time.Hour

// SetScanAdmissionWindowForTest shortens the window for an integration test and returns a
// function that restores it. Production never calls it.
func SetScanAdmissionWindowForTest(d time.Duration) (restore func()) {
	old := scanAdmissionWindow
	scanAdmissionWindow = d
	return func() { scanAdmissionWindow = old }
}

// admitScans refuses n new scan pages when the tenant's running pages plus n would exceed limit.
// The per-tenant advisory lock serializes admission across every site of the tenant (the site
// lock does not), and it is held to commit; the count runs after the lock is granted, so under
// READ COMMITTED it sees every admission committed before it. RLS scopes the count to the tenant.
// A page counts while it is running and younger than scanAdmissionWindow.
func admitScans(ctx context.Context, tx *sql.Tx, sc Scope, n, limit int) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('supervisor.scan_admission:'||$1,0))`, sc.TenantID); err != nil {
		return err
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND state='running'
 AND created_at>clock_timestamp()-make_interval(secs=>$2)`, sc.TenantID, scanAdmissionWindow.Seconds()).Scan(&running); err != nil {
		return err
	}
	if running+n > limit {
		return ErrScanQueueFull
	}
	return nil
}

// ReadScan returns a scan job to an active member of its site; another site's job is ErrNotFound.
func (s *Service) ReadScan(ctx context.Context, a Actor, sc Scope, jobID string) (ScanJob, error) {
	if !identifier(jobID) {
		return ScanJob{}, ErrNotFound
	}
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return ScanJob{}, err
	}
	defer tx.Rollback()
	job, err := readScan(ctx, tx, sc, jobID)
	if err != nil {
		return ScanJob{}, err
	}
	if err = audit(ctx, tx, a, sc, nil, nil, "scan.read", nil); err != nil {
		return ScanJob{}, err
	}
	return job, s.finish(ctx, tx, a, false)
}

func readScan(ctx context.Context, tx *sql.Tx, sc Scope, jobID string) (ScanJob, error) {
	var j ScanJob
	var reason, source sql.NullString
	var finished sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT job_id::text,run_id::text,state,quarantine_reason,title,media_type,width,height,original_sha256,source_id::text,created_at,finished_at
 FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3`, sc.TenantID, sc.SiteID, jobID).Scan(&j.JobID, &j.RunID, &j.State, &reason, &j.Title, &j.MediaType, &j.Width, &j.Height, &j.OriginalSHA256, &source, &j.CreatedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.QuarantineReason, j.SourceID = reason.String, source.String
	if finished.Valid {
		t := finished.Time
		j.FinishedAt = &t
	}
	return j, nil
}

// ScanPage is what an extraction run reads: the running job's image, bound to that run.
type ScanPage struct {
	Title     string
	MediaType string
	Image     []byte
}

// RunningScanTx reads the image of a running job bound to runID. A finished job, or one bound to
// another run, is ErrNotFound: its image is gone or is not this run's.
func RunningScanTx(ctx context.Context, tx *sql.Tx, sc Scope, jobID, runID string) (ScanPage, error) {
	var p ScanPage
	err := tx.QueryRowContext(ctx, `SELECT title,media_type,image FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3 AND run_id=$4 AND state='running'`,
		sc.TenantID, sc.SiteID, jobID, runID).Scan(&p.Title, &p.MediaType, &p.Image)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// lockRunningScan takes the evidence site write lock (the order every evidence write uses), then
// the job row. It returns the uploader and the job's fixed fields.
func lockRunningScan(ctx context.Context, tx *sql.Tx, sc Scope, jobID, runID string) (uploader Actor, title, original string, err error) {
	var found bool
	if err = tx.QueryRowContext(ctx, `SELECT supervisor.lock_evidence_site($1,$2,true)`, sc.TenantID, sc.SiteID).Scan(&found); err != nil {
		return
	}
	if !found {
		err = ErrNotFound
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT uploader_issuer,uploader_subject,title,original_sha256 FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3 AND run_id=$4 AND state='running' FOR UPDATE`,
		sc.TenantID, sc.SiteID, jobID, runID).Scan(&uploader.Issuer, &uploader.Subject, &title, &original)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// QuarantineScanTx ends a running job with a stable reason and clears its image; no text is
// stored. The caller's transaction must be scoped to the tenant.
func QuarantineScanTx(ctx context.Context, tx *sql.Tx, sc Scope, jobID, runID, reason string) error {
	if !scanReasons[reason] {
		return ErrInvalid
	}
	if _, _, _, err := lockRunningScan(ctx, tx, sc, jobID, runID); err != nil {
		return err
	}
	return quarantine(ctx, tx, sc, jobID, runID, reason)
}

func quarantine(ctx context.Context, tx *sql.Tx, sc Scope, jobID, runID, reason string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE supervisor.document_scan_jobs SET state='quarantined',quarantine_reason=$4,image=NULL,finished_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3 AND state='running'`,
		sc.TenantID, sc.SiteID, jobID, reason); err != nil {
		return err
	}
	return audit(ctx, tx, extractionActor(runID), sc, nil, nil, "scan.quarantined", reason)
}

// extractionActor names the run in the audit of what the worker did with a job.
func extractionActor(runID string) Actor {
	return Actor{Issuer: "supervisor:extraction", Subject: runID}
}

// CompleteScanTx stores the extracted text of a running job as a `document` source with its
// automatic spans, marked model-extracted with x, as the job's uploader, and completes the job.
// The uploader's membership is re-checked here: an uploader no longer allowed to write gets the
// job quarantined (UPLOADER_NOT_AUTHORIZED) and no source. Text that does not fit a source is
// quarantined (TEXT_TOO_LARGE), never truncated. The receipt is keyed by the job, so a retried
// activity cannot store a second source.
func CompleteScanTx(ctx context.Context, tx *sql.Tx, sc Scope, jobID, runID, text string, spans []Range, x Extraction) (doc Document, quarantined string, err error) {
	if x.Method != ExtractionModelOCR || x.Model == "" || x.ModelRevision == "" || !lowerHexSHA256(x.PromptSHA256) || x.RunID != runID || !identifier(runID) {
		return doc, "", ErrInvalid
	}
	uploader, title, original, err := lockRunningScan(ctx, tx, sc, jobID, runID)
	if err != nil {
		return doc, "", err
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active FOR SHARE`, sc.TenantID, sc.SiteID, uploader.Issuer, uploader.Subject).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" && role != "supervisor" && role != "operator" {
		return doc, ScanUploaderRevoked, quarantine(ctx, tx, sc, jobID, runID, ScanUploaderRevoked)
	}
	if err != nil {
		return doc, "", err
	}
	in := DocumentInput{Title: title, Text: text, OriginalSHA256: original, Spans: spans}
	if !validDocument(in) {
		return doc, ScanTextTooLarge, quarantine(ctx, tx, sc, jobID, runID, ScanTextTooLarge)
	}
	source := Input{SourceID: uuid.NewString(), Kind: "document", Title: in.Title, Text: in.Text}
	group, err := independenceGroup(ctx, tx, sc, source, false, 0)
	if err != nil {
		return doc, "", err
	}
	if _, err = insertHead(ctx, tx, sc, source.SourceID); err != nil {
		return doc, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.evidence_revisions(tenant_id,site_id,source_id,revision,kind,title,body,sha256,byte_length,rune_length,actor_issuer,actor_subject,authority_class,independence_group_id,
 extraction,extractor_model,extractor_model_revision,extractor_prompt_sha256,extraction_run_id) VALUES($1,$2,$3,1,'document',$4,$5,$6,$7,$8,$9,$10,'reported',$11,$12,$13,$14,$15,$16)`,
		sc.TenantID, sc.SiteID, source.SourceID, in.Title, in.Text, hash([]byte(in.Text)), len(in.Text), len([]rune(in.Text)), uploader.Issuer, uploader.Subject, group,
		x.Method, x.Model, x.ModelRevision, x.PromptSHA256, x.RunID); err != nil {
		return doc, "", err
	}
	if err = audit(ctx, tx, uploader, sc, source.SourceID, 1, "source.created", nil); err != nil {
		return doc, "", err
	}
	r := receipt{Revision: 1, SourceID: source.SourceID, SpanIDs: make([]string, 0, len(spans))}
	for _, span := range spans {
		id, err := insertSpan(ctx, tx, uploader, sc, SpanInput{SourceID: source.SourceID, Revision: 1, Start: span.Start, End: span.End})
		if err != nil {
			return doc, "", err
		}
		r.SpanIDs = append(r.SpanIDs, id)
	}
	if err = saveReceipt(ctx, tx, uploader, sc, source.SourceID, scanReceiptKeyPrefix+jobID, fingerprint("scan", in, 0), r); err != nil {
		return doc, "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE supervisor.document_scan_jobs SET state='completed',source_id=$4,image=NULL,finished_at=clock_timestamp() WHERE tenant_id=$1 AND site_id=$2 AND job_id=$3 AND state='running'`,
		sc.TenantID, sc.SiteID, jobID, source.SourceID); err != nil {
		return doc, "", err
	}
	if err = audit(ctx, tx, extractionActor(runID), sc, source.SourceID, 1, "scan.completed", nil); err != nil {
		return doc, "", err
	}
	doc, err = readDocument(ctx, tx, sc, r)
	return doc, "", err
}
