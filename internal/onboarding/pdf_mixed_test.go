package onboarding

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const fullPageScan = "q 612 0 0 792 0 0 cm /Im1 Do Q" // the page image fills the 612 x 792 page

// WIS-645 slice 2: one parse reads a mixed PDF's typed pages as text and its scanned pages as
// images, each with its own page number. A PDF with no scanned page, or with a blank page, is
// still refused. pageImages is the child's own function, so this runs without the sandbox.
func TestPageImagesOfMixedPDF(t *testing.T) {
	doc := func(pages ...string) []byte {
		return pdfDoc{pages: pages, resources: imageResources(4 + 2*len(pages)), extra: []string{imageObject}}.bytes()
	}
	for _, tc := range []struct {
		name    string
		pdf     []byte
		reason  string
		typed   []int
		scanned []int
	}{
		{"typed first", doc(textPage("Receiving: weigh every coil."), fullPageScan, fullPageScan), "", []int{1}, []int{2, 3}},
		{"typed between scans", doc(fullPageScan, textPage("Storage: rack A."), fullPageScan), "", []int{2}, []int{1, 3}},
		{"every page scanned, as before", doc(fullPageScan, fullPageScan), "", nil, []int{1, 2}},
		{"no scanned page", doc(textPage("Typed only")), pdfReasonScanText, nil, nil},
		// A page with nothing on it (no content and no image resource), as in the scan-only test.
		{"a blank page", pdfDoc{pages: []string{textPage("Typed"), ""}}.bytes(), pdfReasonScanImage, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := pageImages(tc.pdf)
			if tc.reason != "" {
				if reply.Status != "refused" || reply.Reason != tc.reason {
					t.Fatalf("got %s %s, want refused %s", reply.Status, reply.Reason, tc.reason)
				}
				return
			}
			if reply.Status != "ok" {
				t.Fatalf("refused: %s", reply.Reason)
			}
			var typed, scanned []int
			for _, p := range reply.Typed {
				typed = append(typed, p.Number)
				if p.Status != "text" || strings.TrimSpace(p.Text) == "" {
					t.Fatalf("typed page %d: %q %q", p.Number, p.Status, p.Text)
				}
			}
			for _, p := range reply.Pages {
				scanned = append(scanned, p.Number)
			}
			if !reflect.DeepEqual(typed, tc.typed) || !reflect.DeepEqual(scanned, tc.scanned) {
				t.Fatalf("typed %v scanned %v, want %v %v", typed, scanned, tc.typed, tc.scanned)
			}
			scan, err := scanReply(pdfPagesReply{SchemaVersion: 1, Status: reply.Status, Pages: reply.Pages, Typed: reply.Typed})
			if err != nil || scan.PageCount != len(tc.typed)+len(tc.scanned) {
				t.Fatalf("scanReply: %+v %v", scan, err)
			}
		})
	}
}

// The parent accepts a reply only when its scanned and typed pages are pages 1..n, each once, in
// order, with at least one scanned page: anything else is not a reply the child gives.
func TestScanReplyChecksPageNumbers(t *testing.T) {
	img := func(n int) PDFPageImage { return PDFPageImage{Number: n, MediaType: "image/png", Data: []byte{1}} }
	txt := func(n int) pdfPage { return pdfPage{Number: n, Status: "text", Text: "typed"} }
	ok := func(pages []PDFPageImage, typed []pdfPage) pdfPagesReply {
		return pdfPagesReply{SchemaVersion: 1, Status: "ok", Pages: pages, Typed: typed}
	}
	for _, tc := range []struct {
		name  string
		reply pdfPagesReply
		want  error
	}{
		{"mixed", ok([]PDFPageImage{img(2), img(3)}, []pdfPage{txt(1)}), nil},
		{"scanned only", ok([]PDFPageImage{img(1), img(2)}, nil), nil},
		{"a gap", ok([]PDFPageImage{img(1), img(3)}, nil), ErrExtractionUnavailable},
		{"a page twice", ok([]PDFPageImage{img(1)}, []pdfPage{txt(1)}), ErrExtractionUnavailable},
		{"out of order", ok([]PDFPageImage{img(2), img(1)}, nil), ErrExtractionUnavailable},
		{"a typed page that is not text", ok([]PDFPageImage{img(1)}, []pdfPage{{Number: 2, Status: "blank"}}), ErrExtractionUnavailable},
		{"no scanned page", ok(nil, []pdfPage{txt(1)}), ErrExtractionUnavailable},
		{"refused as typed", pdfPagesReply{SchemaVersion: 1, Status: "refused", Reason: pdfReasonScanText}, ErrPDFScanHasText},
		{"refused as hidden", pdfPagesReply{SchemaVersion: 1, Status: "refused", Reason: pdfReasonScanHidden}, ErrPDFScanHiddenContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := scanReply(tc.reply); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTypedDocument(t *testing.T) {
	text, spans, err := typedDocument([]PDFTypedPage{{Number: 1, Text: "Receiving: weigh every coil."}, {Number: 3, Text: "Dispatch: seal the crate.\n"}})
	if err != nil || text != "Receiving: weigh every coil.\nDispatch: seal the crate.\n" || len(spans) == 0 {
		t.Fatalf("%q %v %v", text, spans, err)
	}
	if _, _, err = typedDocument([]PDFTypedPage{{Number: 1, Text: strings.Repeat("x", int(DefaultLimits.MaxExtractedText)+1)}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
}
