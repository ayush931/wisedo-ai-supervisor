package onboarding

import (
	"errors"
	"strings"
	"testing"
)

// Security review of b2e253ed (WIS-625 post-merge).
//   - A scan's cm is parsed with its errors: an overflowing operand (ErrRange, Inf) is refused,
//     and the placement test accepts only when every bound holds, so no NaN or infinity passes.
//   - A render mode on a scan page must be an integer from 0 to 3: clip modes 4 to 7 and
//     non-integers are refused (the glyphs shown are still all required in mode 3).
func TestPDFScanOperandsAndRenderModes(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	for name, page := range map[string]string{
		"an overflowing cm operand":   "q 612 0 0 1" + strings.Repeat("0", 400) + " 0 0 cm /Im1 Do Q",
		"a clip mode after the image": "q 612 0 0 792 0 0 cm /Im1 Do Q 5 Tr",
		"a clip mode in the layer":    "q 612 0 0 792 0 0 cm /Im1 Do Q BT /F1 12 Tf 7 Tr 72 700 Td ET",
		"a mode of 3.5":               "q 612 0 0 792 0 0 cm /Im1 Do Q BT /F1 12 Tf 3.5 Tr 72 700 Td ET",
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{figure}}.bytes())
			// The specific refusal (rule 10), not any error: an unparsable operand or render mode
			// makes the page unreadable, never a sandbox or parser failure (503).
			if pages != nil || !errors.Is(err, ErrUnreadableSource) {
				t.Fatalf("%d pages %v, want %v", len(pages), err, ErrUnreadableSource)
			}
		})
	}
	t.Run("a mode of 3.0 in the layer", func(t *testing.T) {
		page := "q 612 0 0 792 0 0 cm /Im1 Do Q BT /F1 12 Tf 3.0 Tr 72 700 Td (Layer) Tj ET"
		if pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{figure}}.bytes()); err != nil || len(pages) != 1 {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
}
