package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// Codex round 36 on 6e43689c (WIS-625 post-merge).
//   - The simulated-bold exemption matched a redraw to any earlier span with the same text: a
//     large "1" redrawn 2 pt from its original covered a small "1" set beside the original, and
//     the overlap was exempt. Only the redraw's own original is exempt now.
//   - "/Filter [/DCTDecode]" (a one-element array) refused a scan or a figure; a chain still does.
func TestPDFBoldRedrawPairsAndSingletonFilters(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	t.Run("a bold redraw over an unrelated small 1", func(t *testing.T) {
		page := "BT /F1 40 Tf 100 295 Td (1) Tj ET BT /F1 8 Tf 123 305 Td (1) Tj ET BT /F1 40 Tf 102 295 Td (1) Tj ET BT /F1 12 Tf 72 500 Td (Visible line) Tj ET"
		if got, err := Extract(t.Context(), pdfDoc{pages: []string{page}}.bytes(), KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got, err, ErrPDFScanHiddenContent)
		}
	})
	t.Run("a bold redraw over its own original", func(t *testing.T) {
		page := "BT /F1 40 Tf 100 400 Td (1) Tj ET BT /F1 40 Tf 100.5 400 Td (1) Tj ET " + visibleLine
		if got, err := Extract(t.Context(), pdfDoc{pages: []string{page}}.bytes(), KindPDF, DefaultLimits); err != nil || got != "1\nVisible line\n" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	jpeg := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	single := strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter [/DCTDecode]", 1)
	chain := strings.Replace(jpeg, "/Filter /DCTDecode", "/Filter [/DCTDecode /DCTDecode]", 1)
	scan := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	t.Run("a scan filtered by [/DCTDecode]", func(t *testing.T) {
		if pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{scan}, resources: imageResources(6), extra: []string{single}}.bytes()); err != nil || len(pages) != 1 {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
	t.Run("a scan filtered by a chain", func(t *testing.T) {
		if pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{scan}, resources: imageResources(6), extra: []string{chain}}.bytes()); pages != nil || err == nil {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
	t.Run("a figure filtered by [/DCTDecode]", func(t *testing.T) {
		doc := pdfDoc{pages: []string{visibleLine + " q 300 0 0 300 150 400 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{single}}.bytes()
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc})
		if err != nil || len(got.images) != 1 {
			t.Fatalf("%d images %v", len(got.images), err)
		}
	})
}
