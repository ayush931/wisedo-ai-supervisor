// Package onboarding holds the deterministic source-intake gate for discovery
// interviews (WIS-271). It classifies and bounds untrusted uploaded bytes before
// any parser sees them. Plain text (including CSV), Word (DOCX) and Excel (XLSX)
// are extracted with the standard library only, within the same budgets (WIS-613).
// A born-digital PDF is extracted in a short-lived, resource-capped child process
// (WIS-624, pdf.go); encrypted, active, externally linked and image-only PDFs are
// refused with their own reasons, and nothing is ever fabricated for missing text.
package onboarding

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/limits"
)

// Stable errors; the HTTP layer maps them to 413, 415, 422 and 503.
var (
	ErrTooLarge              = errors.New("LIMIT_EXCEEDED")
	ErrUnsupportedMedia      = errors.New("UNSUPPORTED_MEDIA")
	ErrUnreadableSource      = errors.New("UNREADABLE_SOURCE")
	ErrExtractionUnavailable = errors.New("EXTRACTION_UNAVAILABLE")
)

// Kind is the verified content class, derived from bytes, never from the name
// or the declared media type.
type Kind string

const (
	KindText Kind = "text"
	KindPDF  Kind = "pdf"
	KindDOCX Kind = "docx"
	KindXLSX Kind = "xlsx"
)

// Limits are the WIS-271 engineering defaults for the first deployment.
type Limits struct {
	MaxFileBytes         int64
	MaxExtractedText     int64
	MaxDecompressedBytes int64
	MaxZipEntries        int
}

var DefaultLimits = Limits{MaxFileBytes: limits.DocumentBytes, MaxExtractedText: limits.ExtractedTextBytes, MaxDecompressedBytes: limits.DecompressedBytes, MaxZipEntries: 2000}

// ReadBounded counts the bytes actually received. A missing, false or malformed
// Content-Length cannot bypass the cap because it is never consulted.
func ReadBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, ErrUnreadableSource
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

var declaredTypes = map[string]Kind{
	"text/plain":      "text",
	"text/markdown":   "text",
	"text/csv":        "text",
	"application/pdf": "pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       "xlsx",
}

// Inspect verifies the bytes against the declared media type and returns the
// content class. An empty declared type is allowed; a declared type that
// disagrees with the bytes is rejected rather than trusted.
func Inspect(data []byte, declared string, l Limits) (Kind, error) {
	if len(data) == 0 {
		return "", ErrUnreadableSource
	}
	if int64(len(data)) > l.MaxFileBytes {
		return "", ErrTooLarge
	}
	declared = strings.ToLower(strings.TrimSpace(strings.SplitN(declared, ";", 2)[0]))
	want, known := declaredTypes[declared]
	if declared != "" && !known {
		return "", ErrUnsupportedMedia
	}
	var kind Kind
	var err error
	switch {
	case isExecutable(data):
		return "", ErrUnsupportedMedia
	case bytes.HasPrefix(data, []byte("%PDF-")):
		kind, err = KindPDF, checkPDF(data)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		kind, err = checkOOXML(data, l)
	default:
		kind, err = KindText, checkText(data, l)
	}
	if err != nil {
		return "", err
	}
	if declared != "" && want != kind {
		return "", ErrUnsupportedMedia
	}
	return kind, nil
}

// Extract returns the text of an inspected document: plain text as is, a Word document's
// paragraphs (table rows as tab-separated cells) and a workbook's sheets as "# <sheet>" followed
// by their rows, and a PDF's pages in order, one line per text row. Values are taken as stored: no
// number, date or format conversion. The result is bounded by MaxExtractedText.
func Extract(ctx context.Context, data []byte, kind Kind, l Limits) (string, error) {
	var out string
	var err error
	switch kind {
	case KindText:
		out = string(data)
	case KindDOCX:
		out, err = extractDOCX(data, l)
	case KindXLSX:
		out, err = extractXLSX(data, l)
	case KindPDF:
		var x pdfExtraction
		x, err = extractPDF(ctx, data, l)
		// The typed text alone leaves out the figures the vision model reads: only
		// prepareDocument, which queues them, takes such a PDF.
		if err == nil && len(x.figures) > 0 {
			err = ErrPDFNoTextLayer
		}
		out = x.text
	default:
		return "", ErrExtractionUnavailable
	}
	if err != nil {
		return "", err
	}
	if int64(len(out)) > l.MaxExtractedText {
		return "", ErrTooLarge
	}
	return out, nil
}

func isExecutable(b []byte) bool {
	for _, magic := range [][]byte{
		[]byte("MZ"), []byte("\x7fELF"), []byte("#!"),
		{0xFE, 0xED, 0xFA, 0xCE}, {0xFE, 0xED, 0xFA, 0xCF}, {0xCE, 0xFA, 0xED, 0xFE}, {0xCF, 0xFA, 0xED, 0xFE}, {0xCA, 0xFE, 0xBA, 0xBE},
	} {
		if bytes.HasPrefix(b, magic) {
			return true
		}
	}
	return false
}

func checkText(b []byte, l Limits) error {
	if int64(len(b)) > l.MaxExtractedText {
		return ErrTooLarge
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return ErrUnsupportedMedia
	}
	return nil
}

// checkPDF rejects encrypted documents before any parser runs. It is a conservative byte
// scan: an /Encrypt name anywhere is treated as encrypted. PDF names may spell any character
// as #xx, so escapes are decoded first (/Encr#79pt is /Encrypt). The child checks again from
// the parsed trailer.
func checkPDF(b []byte) error {
	if bytes.Contains(decodePDFNameEscapes(b), []byte("/Encrypt")) {
		return ErrPDFEncrypted
	}
	if !bytes.Contains(b, []byte("%%EOF")) {
		return ErrUnreadableSource
	}
	return nil
}

// checkOOXML verifies an Office Open XML package and reports whether it is a Word document or a
// workbook; any other ZIP, or a package claiming to be both, is unsupported.
func checkOOXML(b []byte, l Limits) (Kind, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", ErrUnreadableSource
	}
	if len(zr.File) > l.MaxZipEntries {
		return "", ErrTooLarge
	}
	var declared, inflated int64
	var hasTypes, hasDocument, hasWorkbook bool
	for _, f := range zr.File {
		name := f.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, "\\") || path.Clean(name) != strings.TrimSuffix(name, "/") {
			return "", ErrUnreadableSource
		}
		if f.Flags&0x1 != 0 { // encrypted entry
			return "", ErrUnreadableSource
		}
		// Headers are attacker controlled. Check them first for a cheap
		// reject, then enforce the cap on bytes actually inflated.
		if f.UncompressedSize64 > uint64(l.MaxDecompressedBytes) {
			return "", ErrTooLarge
		}
		declared += int64(f.UncompressedSize64)
		if declared > l.MaxDecompressedBytes {
			return "", ErrTooLarge
		}
		switch name {
		case "[Content_Types].xml":
			hasTypes = true
		case "word/document.xml":
			hasDocument = true
		case "xl/workbook.xml":
			hasWorkbook = true
		}
		if f.FileInfo().IsDir() {
			continue
		}
		// OPC part names are case-insensitive: word/_rels/document.xml.RELS is a relationship part.
		lower := strings.ToLower(name)
		if lower == "word/vbaproject.bin" || strings.HasSuffix(lower, "/vbaproject.bin") || strings.Contains("/"+lower, "/macrosheets/") {
			// Macro projects and Excel 4.0 macro sheets are executable content, not document text.
			return "", ErrUnreadableSource
		}
		var scan func([]byte) (bool, error)
		switch {
		case strings.HasSuffix(lower, ".rels"):
			scan = unsafeRelationship
		case lower == "[content_types].xml":
			scan = macroContentType
		}
		n, external, err := inflate(f, l.MaxDecompressedBytes-inflated, scan)
		if err != nil {
			return "", err
		}
		if external {
			return "", ErrUnreadableSource
		}
		inflated += n
	}
	switch {
	case !hasTypes || hasDocument == hasWorkbook:
		// Any other ZIP, including arbitrary archives, is unsupported input.
		return "", ErrUnsupportedMedia
	case hasDocument:
		return KindDOCX, nil
	}
	return KindXLSX, nil
}

// inflate streams an entry through a hard budget without retaining it, except the parts scan
// reads (relationships and content types), which it refuses when scan reports unsafe content.
func inflate(f *zip.File, budget int64, scan func([]byte) (bool, error)) (n int64, refused bool, err error) {
	rc, err := f.Open()
	if err != nil {
		return 0, false, ErrUnreadableSource
	}
	defer rc.Close()
	var sink bytes.Buffer
	var w io.Writer = io.Discard
	if scan != nil {
		w = &sink
	}
	n, err = io.Copy(w, io.LimitReader(rc, budget+1))
	if err != nil {
		return n, false, ErrUnreadableSource
	}
	if n > budget {
		return n, false, ErrTooLarge
	}
	if scan != nil {
		refused, err := scan(sink.Bytes())
		if err != nil {
			return n, false, ErrUnreadableSource
		}
		return n, refused, nil
	}
	return n, false, nil
}

// macroRelationshipTypes end the Type of a relationship to macro content, whatever its part is
// called: a VBA project or an Excel 4.0 (international) macro sheet.
var macroRelationshipTypes = []string{"/vbaproject", "/xlmacrosheet", "/xlintlmacrosheet"}

// unsafeRelationship reports a relationship part with an external target or a macro
// relationship (WIS-613 audit: macros were refused only by the part name vbaProject.bin).
func unsafeRelationship(data []byte) (bool, error) {
	external, err := externalRelationship(data)
	if err != nil || external {
		return external, err
	}
	return xmlAttrMatches(data, "type", func(v string) bool {
		v = strings.ToLower(v)
		for _, suffix := range macroRelationshipTypes {
			if strings.HasSuffix(v, suffix) {
				return true
			}
		}
		return false
	})
}

// macroContentType reports a [Content_Types].xml that declares macro content: a macro-enabled
// main part, a VBA project or a macro sheet.
func macroContentType(data []byte) (bool, error) {
	return xmlAttrMatches(data, "contenttype", func(v string) bool {
		v = strings.ToLower(v)
		return strings.Contains(v, "macroenabled") || strings.Contains(v, "vbaproject") || strings.Contains(v, "macrosheet")
	})
}

// xmlAttrMatches parses data with encoding/xml and reports whether any attribute named local
// (any case, any namespace) satisfies match. A part that does not parse fails closed.
func xmlAttrMatches(data []byte, local string, match func(string) bool) (bool, error) {
	dec := newXMLTokens(data)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			for _, attr := range start.Attr {
				if strings.EqualFold(attr.Name.Local, local) && match(strings.TrimSpace(attr.Value)) {
					return true, nil
				}
			}
		}
	}
}

// externalRelationship parses a relationship part with encoding/xml, so quoting, spacing
// and character references cannot hide an attribute from the check. A relationship is
// external when its TargetMode is External in any case, or its Target names a URI scheme
// or a network path. A part that does not parse as UTF-8 XML fails closed.
func externalRelationship(data []byte) (bool, error) {
	dec := newXMLTokens(data)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, attr := range start.Attr {
			value := strings.TrimSpace(attr.Value)
			switch strings.ToLower(attr.Name.Local) {
			case "targetmode":
				if strings.EqualFold(value, "External") {
					return true, nil
				}
			case "target":
				if strings.HasPrefix(value, "//") || strings.HasPrefix(value, `\\`) || uriScheme.MatchString(value) {
					return true, nil
				}
			}
		}
	}
}

// uriScheme matches a leading RFC 3986 scheme such as http:, file: or mhtml:.
var uriScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// decodePDFNameEscapes rewrites every #xx hex escape to its byte.
func decodePDFNameEscapes(b []byte) []byte {
	if bytes.IndexByte(b, '#') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '#' && i+2 < len(b) && isHex(b[i+1]) && isHex(b[i+2]) {
			out = append(out, unhex(b[i+1])<<4|unhex(b[i+2]))
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
