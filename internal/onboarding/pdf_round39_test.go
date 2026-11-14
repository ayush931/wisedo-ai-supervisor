package onboarding

import (
	"errors"
	"testing"
)

// Codex round 33 on ad08ffb5 (WIS-625 post-merge).
//   - An eraser recorded by both readings within 0.01 can still sit beside the dark mark in
//     float32 rather than over it. The repaint decision now runs on each reading: text is legible
//     on what is under it only if both say the dark region is erased.
//   - "q BT ... 3 Tr ... ET Q" around a scan's invisible layer left an empty q Q, and the scan was
//     refused. Empty q ... Q pairs are dropped before the drawing is matched.
//   - "/Annots 7 0 R" resolving to null refused a scan; null is no annotations.
func TestPDFErasersInBothReadingsLayerWrappersAndNullAnnots(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		// At y 65836 float32 steps by 0.0078: the white strip covers the dark one in float64
		// (300.003-300.009 over 300.004-300.008) and lies just below it in float32.
		"an eraser beside the mark in float32": {page: "q 1 0 0 1 0 -65536 cm 0 g 72 65836.004 50 0.004 re f 1 g 72 65836.003 50 0.006 re f Q " + visibleLine, want: ErrPDFScanHiddenContent},
		"the same eraser without the shift":    {page: "q 0 g 72 300.004 50 0.004 re f 1 g 72 300.003 50 0.006 re f Q " + visibleLine, text: "Visible line\n"},
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
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	for name, c := range map[string]struct {
		page, pageExtra string
		extra           []string
	}{
		"a layer wrapped in q ... Q":       {page: "q BT /F1 12 Tf 3 Tr 72 700 Td (Scanner layer) Tj ET Q q 612 0 0 792 0 0 cm /Im1 Do Q", extra: []string{figure}},
		"nested empty q Q pairs":           {page: "q q Q q BT /F1 12 Tf 3 Tr 72 700 Td (Layer) Tj ET Q Q q 612 0 0 792 0 0 cm /Im1 Do Q", extra: []string{figure}},
		"annotations by reference to null": {page: "q 612 0 0 792 0 0 cm /Im1 Do Q", pageExtra: "/Annots 7 0 R", extra: []string{figure, "null"}},
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{c.page}, pageExtra: c.pageExtra, resources: imageResources(6), extra: c.extra}.bytes())
			if err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
}
