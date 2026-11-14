package onboarding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// Codex round 7 on 8f7b013c (WIS-625 post-merge): a mirrored redraw is not simulated bold; scans
// and figures are sent at most at a fixed resolution of their largest placement; an indirect
// /UserUnit is read.
func TestPDFMirroredRedrawsAndPlacedResolution(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	t.Run("a mirrored redraw over the text", func(t *testing.T) {
		// "Visible line" in Helvetica is 57.348 pt wide: mirrored from its end it covers the same box.
		doc := pdfDoc{pages: []string{visibleLine + " BT /F1 12 Tf -1 0 0 1 129.348 300 Tm (Visible line) Tj ET"}}
		if got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc.bytes()}); !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got.text, err, ErrPDFScanHiddenContent)
		}
	})
	big := scanImage(t, 2000, 2000, "jpeg")
	sum := sha256.Sum256(big)
	t.Run("a figure drawn twice is sent at its larger placement", func(t *testing.T) {
		doc := pdfDoc{pages: []string{visibleLine + " q 150 0 0 150 72 350 cm /Im1 Do Q q 300 0 0 300 300 400 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{figureObject(big, 2000, 2000)}}
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc.bytes()})
		// 300 pt at 150 dpi is 625 px (the first placement would give 313).
		if err != nil || len(got.images) != 1 || got.images[0].Width != 625 || got.images[0].OriginalSHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("got %+v %v", got.images, err)
		}
	})
	smallPage := func(pageExtra string, extra ...string) []byte {
		b := pdfDoc{pages: []string{"q 72 0 0 72 0 0 cm /Im1 Do Q"}, resources: imageResources(6), pageExtra: pageExtra, extra: append([]string{figureObject(big, 2000, 2000)}, extra...)}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 72 72]"), 1))
	}
	t.Run("a scan on a one-inch page is sent at 300 dpi of it", func(t *testing.T) {
		pages, err := ScanPagesOfPDF(t.Context(), smallPage(""))
		if err != nil || len(pages) != 1 || pages[0].MediaType != "image/jpeg" || pages[0].Original != hex.EncodeToString(sum[:]) {
			t.Fatalf("got %d pages %v", len(pages), err)
		}
		if _, w, h, err := InspectImage(pages[0].Data, pages[0].MediaType); err != nil || w != 300 || h != 300 {
			t.Fatalf("page image %dx%d %v, want 300x300", w, h, err)
		}
	})
	// Codex round 8: the rounding tolerance does not grow with the page: on a 14400-unit page a
	// 1 pt word mostly below the bottom edge is refused (1.44 units of tolerance let it pass).
	t.Run("a word mostly off a huge page", func(t *testing.T) {
		b := pdfDoc{pages: []string{"BT /F1 1 Tf 72 -0.5 Td (Hidden) Tj ET BT /F1 12 Tf 72 300 Td (Visible line) Tj ET"}}.bytes()
		b = rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 14400 14400]"), 1))
		if got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: b}); !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got.text, err, ErrPDFScanHiddenContent)
		}
	})
	t.Run("a scan page with an indirect UserUnit", func(t *testing.T) {
		if pages, err := ScanPagesOfPDF(t.Context(), smallPage("/UserUnit 7 0 R", "1")); err != nil || len(pages) != 1 {
			t.Fatalf("got %d pages %v", len(pages), err)
		}
	})
}
