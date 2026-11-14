package onboarding

import (
	"bytes"
	"testing"
)

// Codex round 35 on b2e253ed (WIS-625 post-merge).
//   - The scan placement tolerance scaled with the page: on a 99,000-unit page it was 9.9 units, so
//     a scan 5 units past the top edge reached OCR (a 96-pixel-wide image is not a figure, so the
//     figure containment check did not see it). The tolerance is capped at maxRounding.
//   - Wrappers separated by a transform ("q 1 0 0 1 0 0 cm q ... cm /Im1 Do Q Q") are two cm: only
//     single-cm scans are supported, and this one is refused (an accepted false refusal).
//   - Text-state operators outside a text object paint nothing and no longer refuse a scan.
func TestPDFScanToleranceTransformWrappersAndTextState(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	scan := func(page string, extra string) ([]PDFPageImage, error) {
		return ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{extra}}.bytes())
	}
	t.Run("a narrow scan 5 units past the top of a tall page", func(t *testing.T) {
		b := pdfDoc{pages: []string{"q 612 0 0 99005 0 0 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{figureObject(scanImage(t, 96, 9600, "jpeg"), 96, 9600)}}.bytes()
		b = rebuildXref(t, bytes.Replace(b, []byte("/MediaBox [0 0 612 792]"), []byte("/MediaBox [0 0 612 99000]"), 1))
		if pages, err := ScanPagesOfPDF(t.Context(), b); pages != nil || err == nil {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
	t.Run("wrappers separated by a transform (accepted false refusal)", func(t *testing.T) {
		if pages, err := scan("q 1 0 0 1 0 0 cm q 612 0 0 792 0 0 cm /Im1 Do Q Q", figure); pages != nil || err == nil {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
	// Security review: surplus operands before a text-state operator refuse.
	for name, page := range map[string]string{
		"surplus operands before Tf": "q 612 0 0 792 0 0 cm /Im1 Do Q 1 2 /F1 12 Tf",
		"surplus operands before Tc": "q 612 0 0 792 0 0 cm /Im1 Do Q 1 0 Tc",
	} {
		t.Run(name, func(t *testing.T) {
			if pages, err := scan(page, figure); pages != nil || err == nil {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
	for name, page := range map[string]string{
		"a font set before the layer":   "q 612 0 0 792 0 0 cm /Im1 Do Q /F1 12 Tf BT 3 Tr 72 700 Td (Layer) Tj ET",
		"spacing and scaling set first": "0 Tc 0 Tw 100 Tz 14 TL 0 Ts q 612 0 0 792 0 0 cm /Im1 Do Q",
	} {
		t.Run(name, func(t *testing.T) {
			if pages, err := scan(page, figure); err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
}
