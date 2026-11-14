package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/evidence"
)

// MaxDocumentSpanRunes and DocumentSpans moved to internal/evidence (WIS-625), so an OCR run
// splits model-extracted text exactly as an upload splits extracted text.
const MaxDocumentSpanRunes = evidence.MaxDocumentSpanRunes

var DocumentSpans = evidence.DocumentSpans

// DocumentUpload is one uploaded file. The file name is never used: the kind comes from the bytes.
type DocumentUpload struct {
	Title        string
	DeclaredType string
	Data         []byte
}

// DocumentResult is the stored document source with its automatic spans, the kind verified from
// the bytes and the original file's SHA-256. The original bytes are not kept; the extracted size is
// the source's byte_length.
//
// A typed PDF's figures (WIS-625 post-merge) are queued in the same transaction, one scan job per
// figure, titled "<title> (page P, image K)": ImageReads. Each becomes its own model-extracted
// source once the vision model reads it, which the owner can designate only with
// confirm_model_extracted. ImagesSkipped counts the images too small to be read (logos, icons).
type DocumentResult struct {
	evidence.Document
	Kind           Kind               `json:"kind"`
	OriginalSHA256 string             `json:"original_sha256" contract:"sha256"`
	ImageReads     []evidence.ScanJob `json:"image_reads,omitempty"`
	ImagesSkipped  int                `json:"images_skipped,omitempty"`
}

// UploadDocument verifies and extracts an uploaded document, then stores its text as a document
// evidence source together with its automatic spans, as the uploader under the evidence service's
// own authority (WIS-613). Write authority is checked before the file is inspected, so a member who
// cannot write never makes the server extract anything. Every refusal is decided before anything is
// written, and text that does not fit an evidence source is refused, never truncated.
//
// A PDF with figures needs the vision model: without one (s.Enabled false) it is refused with
// EXTRACTION_UNAVAILABLE, as a scan is, never stored as its typed text alone.
func UploadDocument(ctx context.Context, sources *evidence.Service, a evidence.Actor, sc evidence.Scope, in DocumentUpload, key string, s ScanSettings) (DocumentResult, error) {
	if err := sources.CheckWrite(ctx, a, sc); err != nil {
		return DocumentResult{}, err
	}
	prepared, err := prepareDocument(ctx, in)
	if err != nil {
		return DocumentResult{}, err
	}
	// Without a vision model no new figure can be queued, but a replay needs none: the evidence
	// service decides, after finding the receipt (WIS-625 post-merge review).
	var start evidence.StartRun
	if len(prepared.images) > 0 && s.Enabled {
		start = scanRun(sc, s)
	}
	doc, jobs, err := sources.CreateDocumentWithScans(ctx, a, sc, evidence.DocumentInput{Title: in.Title, Text: prepared.text, OriginalSHA256: prepared.original, Spans: prepared.spans},
		key, prepared.images, start)
	if errors.Is(err, evidence.ErrScansUnavailable) {
		return DocumentResult{}, ErrExtractionUnavailable
	}
	if err != nil {
		return DocumentResult{}, err
	}
	skipped := prepared.skipped
	if len(prepared.images) > 0 && len(jobs) == 0 {
		skipped = 0 // a replay of an upload stored before figures were read reports none, as it did
	}
	return DocumentResult{Document: doc, Kind: prepared.kind, OriginalSHA256: prepared.original, ImageReads: jobs, ImagesSkipped: skipped}, nil
}

type preparedDocument struct {
	kind     Kind
	text     string
	spans    []evidence.Range
	original string
	images   []evidence.ScanInput // a PDF's figures, verified as uploaded photos are
	skipped  int
}

// prepareDocument decides everything about an upload from its bytes alone: its kind, its extracted
// text, its spans and the original file's SHA-256. It writes nothing.
func prepareDocument(ctx context.Context, in DocumentUpload) (preparedDocument, error) {
	kind, err := Inspect(in.Data, in.DeclaredType, DefaultLimits)
	if err != nil {
		return preparedDocument{}, err
	}
	var text string
	var images []evidence.ScanInput
	skipped := 0
	if kind == KindPDF {
		x, err := extractPDF(ctx, in.Data, DefaultLimits)
		if err != nil {
			return preparedDocument{}, err
		}
		text, skipped = x.text, x.skipped
		for _, f := range x.figures {
			media, w, h, err := InspectImage(f.image.Data, f.image.MediaType)
			if err != nil {
				return preparedDocument{}, err
			}
			// The figure's identity (receipts, replays) is its image as embedded; what is sent may be
			// that image scaled to how the page shows it (figureAsShown).
			original, err := imageIdentity(f.image)
			if err != nil {
				return preparedDocument{}, err
			}
			images = append(images, evidence.ScanInput{Title: figureTitle(in.Title, f.page, f.index), MediaType: media, Width: w, Height: h, Image: f.image.Data, OriginalSHA256: original})
		}
	} else if text, err = Extract(ctx, in.Data, kind, DefaultLimits); err != nil {
		return preparedDocument{}, err
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) > evidence.MaxTextBytes {
		return preparedDocument{}, ErrTooLarge
	}
	spans := DocumentSpans(text)
	if len(spans) == 0 {
		return preparedDocument{}, ErrUnreadableSource // only whitespace: nothing to cite
	}
	sum := sha256.Sum256(in.Data)
	return preparedDocument{kind: kind, text: text, spans: spans, original: hex.EncodeToString(sum[:]), images: images, skipped: skipped}, nil
}

// imageIdentity is an image's identity for receipts and replays: the SHA-256 of the image as
// embedded in the PDF (Original, when what is sent was scaled to the page), else of what is sent.
func imageIdentity(img PDFPageImage) (string, error) {
	if img.Original == "" {
		sum := sha256.Sum256(img.Data)
		return hex.EncodeToString(sum[:]), nil
	}
	if b, err := hex.DecodeString(img.Original); err != nil || len(b) != 32 || img.Original != strings.ToLower(img.Original) {
		return "", ErrExtractionUnavailable
	}
	return img.Original, nil
}

// figureTitle names image k of page p of a document within the 200-character title limit, so its
// model-extracted source says where on the original it came from.
func figureTitle(title string, page, k int) string {
	suffix := " (page " + strconv.Itoa(page) + ", image " + strconv.Itoa(k) + ")"
	runes := []rune(title)
	if max := 200 - len([]rune(suffix)); len(runes) > max {
		runes = runes[:max]
	}
	return string(runes) + suffix
}
