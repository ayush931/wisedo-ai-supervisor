package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
)

// WIS-625 slice B1: a photographed page (PNG or JPEG) is queued for OCR by the pinned local vision
// model instead of being extracted in the upload request. The bytes decide everything: the type
// (never the name or the declared type alone), the size and the declared pixel count, read from
// the header before anything decodes the image.

// ScanSettings is the vision model the extraction run is pinned to. Enabled is false without a
// configured local vision model: the upload is then refused with EXTRACTION_UNAVAILABLE.
type ScanSettings struct {
	Provider, Model, ModelRevision string
	ProviderActual                 bool
	Enabled                        bool
}

var scanTypes = map[string]bool{"image/png": true, "image/jpeg": true}

// InspectImage verifies that the bytes are a PNG or JPEG page within the size and pixel bounds,
// and that a declared type, when given, agrees with them.
func InspectImage(data []byte, declared string) (mediaType string, width, height int, err error) {
	if len(data) == 0 {
		return "", 0, 0, ErrUnreadableSource
	}
	if len(data) > agent.MaxImageBytes {
		return "", 0, 0, ErrTooLarge
	}
	declared = strings.ToLower(strings.TrimSpace(strings.SplitN(declared, ";", 2)[0]))
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		mediaType = "image/png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		mediaType = "image/jpeg"
	default:
		return "", 0, 0, ErrUnsupportedMedia
	}
	if declared != "" && (!scanTypes[declared] || declared != mediaType) {
		return "", 0, 0, ErrUnsupportedMedia
	}
	width, height, ok := agent.ImageSize(data)
	if !ok {
		// Unreadable header, or a canvas over agent.MaxImagePixels (a decompression bomb).
		return "", 0, 0, ErrUnreadableSource
	}
	return mediaType, width, height, nil
}

// QueueScan checks the uploader's write authority, then the image, then stores the scan job, its
// extraction run (budgeted by agent.ExtractionLimits, never pinned to a hosted route) and its
// outbox row in one transaction. The worker reads the page later; nothing is extracted here.
//
// Without a vision model a replay still answers; only a new job is EXTRACTION_UNAVAILABLE.
func QueueScan(ctx context.Context, sources *evidence.Service, a evidence.Actor, sc evidence.Scope, in DocumentUpload, key string, s ScanSettings) (evidence.ScanJob, error) {
	if err := sources.CheckWrite(ctx, a, sc); err != nil {
		return evidence.ScanJob{}, err
	}
	mediaType, width, height, err := InspectImage(in.Data, in.DeclaredType)
	if err != nil {
		return evidence.ScanJob{}, err
	}
	sum := sha256.Sum256(in.Data)
	job, err := sources.QueueScan(ctx, a, sc, evidence.ScanInput{Title: in.Title, MediaType: mediaType, Width: width, Height: height, Image: in.Data, OriginalSHA256: hex.EncodeToString(sum[:])}, key, startRun(sc, s))
	return job, scansUnavailable(err)
}

// startRun is the run starter for a new job, or nil without a vision model (a replay needs none).
func startRun(sc evidence.Scope, s ScanSettings) evidence.StartRun {
	if !s.Enabled {
		return nil
	}
	return scanRun(sc, s)
}

// scansUnavailable maps a new job needed without a vision model to EXTRACTION_UNAVAILABLE.
func scansUnavailable(err error) error {
	if errors.Is(err, evidence.ErrScansUnavailable) {
		return ErrExtractionUnavailable
	}
	return err
}

// scanRun creates one page's extraction run: budgeted by agent.ExtractionLimits, pinned to the
// vision model, never to a hosted route.
func scanRun(sc evidence.Scope, s ScanSettings) evidence.StartRun {
	return func(ctx context.Context, tx *sql.Tx, jobID string) (string, error) {
		run := agent.Run{ID: uuid.NewString(), RequestKind: agent.RequestExtraction, ConversationID: jobID, RequestID: jobID, State: agent.RunRunning,
			TraceID: agent.NewTraceID(), ProviderName: s.Provider, ProviderActual: s.ProviderActual, Model: s.Model, ModelRevision: s.ModelRevision,
			PromptVersion: agent.OCRPromptVersion, ToolVersion: agent.ToolVersion, PolicyRevision: approvals.PolicyRevision}
		rs := agent.RunScope{TenantID: sc.TenantID, SiteID: sc.SiteID}
		if err := agent.CreateRunTx(ctx, tx, rs, run, agent.ExtractionLimits()); err != nil {
			return "", err
		}
		return run.ID, agent.ReserveInvocationTx(ctx, tx, rs, run.ID)
	}
}

// QueueScanPages queues the scanned pages of a PDF for OCR (WIS-625 slice B2), read in one parse.
// Each page image is checked as an uploaded photo is, titled "<title> (page i of n)" with its own
// page number. A PDF whose every page is scanned is queued as a batch, as before. A PDF that also
// has typed pages (WIS-645) stores those pages' text as one typed document and queues the scanned
// pages, linked as one upload, all in one transaction; typed and model-extracted text are never
// one source (rule 11). A PDF with no scanned page is ErrPDFScanHasText: upload it as a document.
func QueueScanPages(ctx context.Context, sources *evidence.Service, a evidence.Actor, sc evidence.Scope, in DocumentUpload, key string, s ScanSettings) (evidence.ScanBatch, error) {
	if err := sources.CheckWrite(ctx, a, sc); err != nil {
		return evidence.ScanBatch{}, err
	}
	kind, err := Inspect(in.Data, in.DeclaredType, DefaultLimits)
	if err != nil {
		return evidence.ScanBatch{}, err
	}
	if kind != KindPDF {
		return evidence.ScanBatch{}, ErrUnsupportedMedia
	}
	scan, err := ReadPagesOfPDF(ctx, in.Data)
	if err != nil {
		return evidence.ScanBatch{}, err
	}
	pages := make([]evidence.ScanInput, len(scan.Pages))
	numbers := make([]int, len(scan.Pages))
	for i, img := range scan.Pages {
		media, w, h, err := InspectImage(img.Data, img.MediaType)
		if err != nil {
			return evidence.ScanBatch{}, err
		}
		original, err := imageIdentity(img)
		if err != nil {
			return evidence.ScanBatch{}, err
		}
		pages[i] = evidence.ScanInput{Title: pageTitle(in.Title, img.Number, scan.PageCount), MediaType: media, Width: w, Height: h, Image: img.Data, OriginalSHA256: original}
		numbers[i] = img.Number
	}
	if len(scan.Typed) == 0 {
		batch, err := sources.QueueScanBatch(ctx, a, sc, pages, key, startRun(sc, s))
		return batch, scansUnavailable(err)
	}
	text, spans, err := typedDocument(scan.Typed)
	if err != nil {
		return evidence.ScanBatch{}, err
	}
	sum := sha256.Sum256(in.Data)
	original := hex.EncodeToString(sum[:])
	batch, err := sources.QueueMixedUpload(ctx, a, sc, evidence.MixedUploadInput{
		Title: in.Title, OriginalSHA256: original, PageCount: scan.PageCount, Pages: pages, PageNumbers: numbers,
		Document: evidence.DocumentInput{Title: in.Title, Text: text, OriginalSHA256: original, Spans: spans},
	}, key, startRun(sc, s))
	return batch, scansUnavailable(err)
}

// typedDocument is the typed pages' text, joined as /source-documents joins a PDF's pages, with its
// automatic spans. Text over the document limits is ErrTooLarge, never truncated.
func typedDocument(typed []PDFTypedPage) (string, []evidence.Range, error) {
	var b strings.Builder
	for _, p := range typed {
		b.WriteString(p.Text)
		if !strings.HasSuffix(p.Text, "\n") {
			b.WriteByte('\n')
		}
		if int64(b.Len()) > DefaultLimits.MaxExtractedText || b.Len() > evidence.MaxTextBytes {
			return "", nil, ErrTooLarge
		}
	}
	text := strings.ReplaceAll(b.String(), "\r\n", "\n")
	spans := DocumentSpans(text)
	if len(spans) == 0 {
		return "", nil, ErrUnreadableSource
	}
	return text, spans, nil
}

// pageTitle names one page of a scanned document within the 200-character title limit.
func pageTitle(title string, page, pages int) string {
	suffix := " (page " + strconv.Itoa(page) + " of " + strconv.Itoa(pages) + ")"
	runes := []rune(title)
	if max := 200 - len([]rune(suffix)); len(runes) > max {
		runes = runes[:max]
	}
	return string(runes) + suffix
}
