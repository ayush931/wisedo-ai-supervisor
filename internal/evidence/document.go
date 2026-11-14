package evidence

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxDocumentSpans bounds the spans created with one document, so an upload is one bounded
// transaction. The automatic passages of a full source (WIS-613) stay well below it.
const MaxDocumentSpans = 512

// Range is a span's position in a source text, in runes, end exclusive.
type Range struct {
	Start int `json:"start_char"`
	End   int `json:"end_char"`
}

// DocumentInput is a document source and its spans, created together (WIS-613). OriginalSHA256
// identifies the file the text was extracted from: it is bound into the idempotency key, so a
// different file under the same key conflicts even when its text is identical. The file itself is
// not stored.
type DocumentInput struct {
	Title          string  `json:"title"`
	Text           string  `json:"text"`
	OriginalSHA256 string  `json:"original_sha256"`
	Spans          []Range `json:"spans"`
}

// Document is a document source and its spans, in the order they were given.
type Document struct {
	Source Source `json:"source"`
	Spans  []Span `json:"spans"`
}

// errDocumentIntegrity reports a receipt whose spans are no longer stored as recorded.
var errDocumentIntegrity = errors.New("evidence document integrity")

func validDocument(in DocumentInput) bool {
	if !text(in.Title, 200) || !text(in.Text, MaxTextBytes) || len(in.Text) > MaxTextBytes || !lowerHexSHA256(in.OriginalSHA256) {
		return false
	}
	if len(in.Spans) < 1 || len(in.Spans) > MaxDocumentSpans {
		return false
	}
	runes := utf8.RuneCountInString(in.Text)
	for _, r := range in.Spans {
		if r.Start < 0 || r.End <= r.Start || r.End > runes || r.End-r.Start > 4096 {
			return false
		}
	}
	return true
}

func lowerHexSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}

// CheckWrite reports whether the actor currently holds the evidence write authority at the site,
// without writing anything. Callers use it to refuse before doing expensive work on a request;
// CreateDocument still checks again inside its own transaction.
func (s *Service) CheckWrite(ctx context.Context, a Actor, sc Scope) error {
	tx, err := s.begin(ctx, a, sc, false, false)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM supervisor.memberships WHERE tenant_id=$1 AND site_id=$2 AND issuer=$3 AND subject=$4 AND active`, sc.TenantID, sc.SiteID, a.Issuer, a.Subject).Scan(&role)
	if err != nil {
		return err
	}
	if role != "owner" && role != "supervisor" && role != "operator" {
		return ErrDenied
	}
	return nil
}

// CreateDocument records a new document source and its spans as the actor, under the evidence
// write authority, in one transaction: either the source and every span exist, or nothing does.
// The key binds the whole input; a replay returns the original source and spans after the
// actor's authority has been checked again.
func (s *Service) CreateDocument(ctx context.Context, a Actor, sc Scope, in DocumentInput, key string) (Document, error) {
	out, _, err := s.CreateDocumentWithScans(ctx, a, sc, in, key, nil, nil)
	return out, err
}

// CreateDocumentWithScans records a document as CreateDocument does and, in the same transaction,
// queues one scan job per image (WIS-625 post-merge: the figures of a typed PDF are read by the
// vision model, each into its own model-extracted source). Either the document and every job
// exist, or nothing does. Image i is keyed images:<sha256 of key>#i, so the same key replays the
// document and its jobs together.
func (s *Service) CreateDocumentWithScans(ctx context.Context, a Actor, sc Scope, in DocumentInput, key string, images []ScanInput, start StartRun) (Document, []ScanJob, error) {
	var out Document
	if !validDocument(in) || !mutation(key, 0) || len(images) > 64 {
		return out, nil, ErrInvalid
	}
	keys := make([]string, len(images))
	for i, img := range images {
		keys[i] = "images:" + hash([]byte(key)) + "#" + strconv.Itoa(i+1)
		if !validScan(img) {
			return out, nil, ErrInvalid
		}
	}
	figures := make([]string, len(images))
	for i, img := range images {
		figures[i] = img.OriginalSHA256
	}
	tx, err := s.begin(ctx, a, sc, true, false)
	if err != nil {
		return out, nil, err
	}
	defer tx.Rollback()
	out, prior, replayed, err := createDocumentTx(ctx, tx, a, sc, in, key, figures)
	if err != nil {
		return out, nil, err
	}
	jobs := []ScanJob{}
	switch {
	case replayed && prior.FigurePolicy == 0:
		// Stored before figures were read: the original operation queued none, so neither does its
		// replay (WIS-625 post-merge review).
		images = nil
	case replayed && !slices.Equal(prior.Figures, figures):
		return out, nil, ErrConflict
	}
	if len(images) > 0 {
		var replayedJobs int
		if jobs, replayedJobs, err = s.queueTx(ctx, tx, a, sc, images, keys, nil, start, ""); err != nil {
			return out, nil, err
		}
		if err = auditQueued(ctx, tx, a, sc, replayedJobs); err != nil {
			return out, nil, err
		}
	}
	return out, jobs, s.finish(ctx, tx, a, false)
}

// DocumentFigurePolicy is the version of the figure reading a document receipt records.
const DocumentFigurePolicy = 1

// createDocumentTx creates the document, or replays it: replayed is true with the stored receipt.
func createDocumentTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in DocumentInput, key string, figures []string) (out Document, prior receipt, replayed bool, err error) {
	digest := fingerprint("document", in, 0)
	prior, ok, err := replay(ctx, tx, a, sc, key, digest)
	if err != nil {
		return out, prior, false, err
	}
	if ok {
		if out, err = readDocument(ctx, tx, sc, prior); err != nil {
			return out, prior, true, err
		}
		return out, prior, true, audit(ctx, tx, a, sc, prior.SourceID, prior.Revision, "mutation.replayed", nil)
	}
	r, err := insertDocumentTx(ctx, tx, a, sc, in)
	if err != nil {
		return out, prior, false, err
	}
	r.FigurePolicy, r.Figures = DocumentFigurePolicy, figures
	if err = saveReceipt(ctx, tx, a, sc, r.SourceID, key, digest, r); err != nil {
		return out, prior, false, err
	}
	out, err = readDocument(ctx, tx, sc, r)
	return out, prior, false, err
}

// insertDocumentTx stores a validated document's typed text as a new document source, revision 1,
// with its spans, in tx. createDocumentTx and a mixed PDF upload (WIS-645) share it. The revision is
// typed text, never model-extracted (rule 11).
func insertDocumentTx(ctx context.Context, tx *sql.Tx, a Actor, sc Scope, in DocumentInput) (receipt, error) {
	source := Input{SourceID: uuid.NewString(), Kind: "document", Title: in.Title, Text: in.Text}
	group, err := independenceGroup(ctx, tx, sc, source, false, 0)
	if err != nil {
		return receipt{}, err
	}
	if _, err = insertHead(ctx, tx, sc, source.SourceID); err != nil {
		return receipt{}, err
	}
	if err = insertRevision(ctx, tx, a, sc, source, 1, "reported", group, nil); err != nil {
		return receipt{}, err
	}
	if err = audit(ctx, tx, a, sc, source.SourceID, 1, "source.created", nil); err != nil {
		return receipt{}, err
	}
	r := receipt{Revision: 1, SourceID: source.SourceID, SpanIDs: make([]string, 0, len(in.Spans))}
	for _, span := range in.Spans {
		id, err := insertSpan(ctx, tx, a, sc, SpanInput{SourceID: source.SourceID, Revision: 1, Start: span.Start, End: span.End})
		if err != nil {
			return receipt{}, err
		}
		r.SpanIDs = append(r.SpanIDs, id)
	}
	return r, nil
}

// readDocument reads a document receipt's source revision and spans in one pass over the text.
// A revoked source is unavailable, as on every other read.
func readDocument(ctx context.Context, tx *sql.Tx, sc Scope, r receipt) (Document, error) {
	var out Document
	h, err := getHead(ctx, tx, sc, r.SourceID, false)
	if err != nil {
		return out, err
	}
	if out.Source, err = readSource(ctx, tx, sc, r.SourceID, r.Revision, h); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text,start_rune,end_rune FROM supervisor.evidence_spans WHERE tenant_id=$1 AND site_id=$2 AND source_id=$3 AND revision=$4 AND id=ANY($5::uuid[])`, sc.TenantID, sc.SiteID, r.SourceID, r.Revision, r.SpanIDs)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	stored := make(map[string]Range, len(r.SpanIDs))
	for rows.Next() {
		var id string
		var p Range
		if err = rows.Scan(&id, &p.Start, &p.End); err != nil {
			return out, err
		}
		stored[id] = p
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	runes := []rune(out.Source.Text)
	out.Spans = make([]Span, 0, len(r.SpanIDs))
	for _, id := range r.SpanIDs {
		p, found := stored[id]
		if !found {
			return out, errDocumentIntegrity
		}
		span, err := spanOf(id, SpanInput{SourceID: r.SourceID, Revision: r.Revision, Start: p.Start, End: p.End}, runes, out.Source.SHA256, h.epoch)
		if err != nil {
			return out, errDocumentIntegrity
		}
		out.Spans = append(out.Spans, span)
	}
	return out, nil
}
