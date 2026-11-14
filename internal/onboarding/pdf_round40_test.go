package onboarding

import (
	"errors"
	"testing"
)

// Codex round 34 on 022a568e (WIS-625 post-merge).
//   - "re" added y + h in float64 before the float32 rounding: a 0.003-high strip at y 65536 has
//     no height in a float32 reader but kept one in both readings here, and erased what it lay on.
//     The far corner is now rounded before the transform.
//   - "q q ... cm /Im1 Do Q Q" around a scan, and a placement built by several cm operators, were
//     refused; balanced wrappers are dropped and the cm operators composed.
//   - "... 3 Tr (...) Tj 0 Tr ET": a mode set after the last show paints nothing.
func TestPDFFloat32RectanglesScanWrappersAndModeResets(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	t.Run("a strip float32 adds to nothing, erasing a mark", func(t *testing.T) {
		page := "q 1 0 0 0.1 0 -6253.6 cm 0 g 72 65536.001 50 0.001 re f 1 g 72 65536 50 0.003 re f Q " + visibleLine
		if got, err := Extract(t.Context(), pdfDoc{pages: []string{page}}.bytes(), KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got, err, ErrPDFScanHiddenContent)
		}
	})
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	for name, page := range map[string]string{
		"nested wrappers around the image":     "q q 612 0 0 792 0 0 cm /Im1 Do Q Q",
		"a mode reset after the scanner layer": "q 612 0 0 792 0 0 cm /Im1 Do Q BT /F1 12 Tf 3 Tr 72 700 Td (Layer) Tj 0 Tr ET",
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{figure}}.bytes())
			if err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
	// Security review of round 34: a placement composed of several cm was computed in float64
	// only; one cm places a scan, and two refuse even when they compose to the page.
	t.Run("a placement built by two cm", func(t *testing.T) {
		data := pdfDoc{pages: []string{"q 2 0 0 2 0 0 cm 306 0 0 396 0 0 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{figure}}.bytes()
		if pages, err := ScanPagesOfPDF(t.Context(), data); pages != nil || err == nil {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
}
