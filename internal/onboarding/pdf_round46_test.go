package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 39 on c813d848 (WIS-625 post-merge).
//   - A stroked Helvetica-Oblique "f" (200 pt, 20 w, 2 Tr) at (116.4, 300) beside a 200 x 200
//     figure at (200, 300): its glyph box reaches past its advance and its stroke over the
//     figure. Already refused (each glyph cell is the union of its AFM box and advance, and the
//     span is widened by its stroke); kept as a guard.
//   - Paint in the space a TJ leaves between glyphs ("[(A) -8000 (B)] TJ") met the span's box and
//     refused the page. Paint over, under or beside text is now tested against each glyph's cell,
//     widened by its stroke, not the span's box.
func TestPDFStrokedObliqueGlyphsAndPaintBetweenGlyphs(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	t.Run("a stroked oblique f beside a figure", func(t *testing.T) {
		figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
		page := "q 200 0 0 200 200 300 cm /Im1 Do Q BT /F1 200 Tf 0 g 0 G 20 w 2 Tr 116.4 300 Td (f) Tj ET"
		b := pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{figure}}.bytes()
		b = rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica >>"), []byte("/BaseFont /Helvetica-Oblique >>"), 1))
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: b})
		if !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q, %d images, %v, want %v", got.text, len(got.images), err, ErrPDFScanHiddenContent)
		}
	})
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"a divider drawn in a TJ gap":   {page: "BT /F1 12 Tf 72 300 Td [(A) -8000 (B)] TJ ET q 0 G 1 w 120 295 m 120 315 l S Q", text: "A B\n"},
		"a divider under a TJ gap":      {page: "q 0 G 1 w 120 295 m 120 315 l S Q BT /F1 12 Tf 72 300 Td [(A) -8000 (B)] TJ ET", text: "A B\n"},
		"a divider drawn through the A": {page: "BT /F1 12 Tf 72 300 Td [(A) -8000 (B)] TJ ET q 0 G 1 w 75 295 m 75 315 l S Q", want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: []string{c.page}}.bytes(), KindPDF, DefaultLimits)
			if c.want != nil {
				if got != "" || !errors.Is(err, c.want) {
					t.Fatalf("got %q %v, want %v", got, err, c.want)
				}
				return
			}
			if err != nil || got != c.text {
				t.Fatalf("got %q %v, want %q", got, err, c.text)
			}
		})
	}
}
