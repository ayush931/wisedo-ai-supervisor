package evidence

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/google/uuid"
)

// MixedUploadInput is a PDF with typed and scanned pages (WIS-645): its typed pages' text as one
// document, and each scanned page as a scan input in page order. PageNumbers[i] is the PDF page
// Pages[i] came from; PageCount is the whole file's page count.
type MixedUploadInput struct {
	Title          string
	OriginalSHA256 string
	PageCount      int
	Document       DocumentInput
	Pages          []ScanInput
	PageNumbers    []int
}

func validMixed(in MixedUploadInput) bool {
	if !text(in.Title, 200) || !lowerHexSHA256(in.OriginalSHA256) || in.PageCount < 2 || in.PageCount > 1000 ||
		len(in.Pages) == 0 || len(in.Pages) > 64 || len(in.PageNumbers) != len(in.Pages) || !validDocument(in.Document) {
		return false
	}
	last := 0
	for _, n := range in.PageNumbers {
		if n <= last || n > in.PageCount {
			return false
		}
		last = n
	}
	return len(in.Pages) < in.PageCount // at least one page is typed
}

// QueueMixedUpload stores a mixed PDF in one transaction, all or nothing, as the uploader under
// the evidence write authority: the typed document with its spans, the upload row, and one scan job
// per scanned page (each with its run and outbox row) linked to the upload. The same key with the
// same file replays the stored upload; the same key with another file, title or page split is
// ErrConflict. Page job keys are upload:<sha256 of key>#<page number>.
func (s *Service) QueueMixedUpload(ctx context.Context, a Actor, sc Scope, in MixedUploadInput, key string, start StartRun) (ScanBatch, error) {
	// Without a vision model (start == nil) a replay still answers; a new upload is refused by
	// queueTx with ErrScansUnavailable and nothing is stored (WIS-625: as for any scan).
	if !validMixed(in) || !mutation(key, 0) {
		return ScanBatch{}, ErrInvalid
	}
	for _, p := range in.Pages {
		if !validScan(p) {
			return ScanBatch{}, ErrInvalid
		}
	}
	tx, err := s.begin(ctx, a, sc, true, false)
	if err != nil {
		return ScanBatch{}, err
	}
	defer tx.Rollback()
	keys := make([]string, len(in.Pages))
	for i, n := range in.PageNumbers {
		keys[i] = "upload:" + hash([]byte(key)) + "#" + strconv.Itoa(n)
	}
	var uploadID, digest, title, typed string
	var pages int
	err = tx.QueryRowContext(ctx, `SELECT upload_id::text,original_sha256,title,typed_source_id::text,page_count FROM supervisor.document_uploads
 WHERE tenant_id=$1 AND site_id=$2 AND uploader_issuer=$3 AND uploader_subject=$4 AND idempotency_key=$5`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject, key).Scan(&uploadID, &digest, &title, &typed, &pages)
	switch {
	case err == nil:
		if digest != in.OriginalSHA256 || title != in.Title || pages != in.PageCount {
			return ScanBatch{}, ErrConflict
		}
		doc, err := readUploadDocument(ctx, tx, sc, typed)
		if err != nil {
			return ScanBatch{}, err
		}
		jobs, replayed, err := s.queueTx(ctx, tx, a, sc, in.Pages, keys, nil, start, uploadID)
		if err != nil {
			return ScanBatch{}, err
		}
		if replayed != len(in.Pages) {
			return ScanBatch{}, ErrConflict // the same key named other scanned pages
		}
		out := ScanBatch{Jobs: jobs, UploadID: &uploadID, Document: &doc}
		if err = audit(ctx, tx, a, sc, typed, int64(1), "mutation.replayed", nil); err != nil {
			return ScanBatch{}, err
		}
		return out, s.finish(ctx, tx, a, false)
	case !errors.Is(err, sql.ErrNoRows):
		return ScanBatch{}, err
	}
	// A scan-only batch already stored under this key is another upload: its first page job is
	// keyed batch:<sha256 of key>#1, or key#1 if it was stored before that scheme (QueueScanBatch).
	var batch bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM supervisor.document_scan_jobs WHERE tenant_id=$1 AND site_id=$2 AND uploader_issuer=$3 AND uploader_subject=$4 AND idempotency_key IN ($5,$6))`,
		sc.TenantID, sc.SiteID, a.Issuer, a.Subject, "batch:"+hash([]byte(key))+"#1", key+"#1").Scan(&batch); err != nil {
		return ScanBatch{}, err
	}
	if batch {
		return ScanBatch{}, ErrConflict
	}
	r, err := insertDocumentTx(ctx, tx, a, sc, in.Document)
	if err != nil {
		return ScanBatch{}, err
	}
	uploadID = uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO supervisor.document_uploads(tenant_id,site_id,upload_id,uploader_issuer,uploader_subject,idempotency_key,title,original_sha256,typed_source_id,page_count)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, sc.TenantID, sc.SiteID, uploadID, a.Issuer, a.Subject, key, in.Title, in.OriginalSHA256, r.SourceID, in.PageCount); err != nil {
		return ScanBatch{}, err
	}
	jobs, replayed, err := s.queueTx(ctx, tx, a, sc, in.Pages, keys, nil, start, uploadID)
	if err != nil {
		return ScanBatch{}, err
	}
	if replayed != 0 {
		return ScanBatch{}, ErrConflict
	}
	doc, err := readDocument(ctx, tx, sc, r)
	if err != nil {
		return ScanBatch{}, err
	}
	out := ScanBatch{Jobs: jobs, UploadID: &uploadID, Document: &doc}
	// The typed document is audited as source.created by insertDocumentTx; the pages as a scan queue.
	if err = audit(ctx, tx, a, sc, nil, nil, "scan.queued", nil); err != nil {
		return ScanBatch{}, err
	}
	return out, s.finish(ctx, tx, a, false)
}

// readUploadDocument reads an upload's typed document, revision 1, with its spans in text order.
func readUploadDocument(ctx context.Context, tx *sql.Tx, sc Scope, sourceID string) (Document, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id::text FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=1 ORDER BY start_rune,id`, sc.TenantID, sc.SiteID, sourceID)
	if err != nil {
		return Document{}, err
	}
	r := receipt{Revision: 1, SourceID: sourceID}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return Document{}, err
		}
		r.SpanIDs = append(r.SpanIDs, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return Document{}, err
	}
	return readDocument(ctx, tx, sc, r)
}
