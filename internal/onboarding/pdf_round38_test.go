package onboarding

import (
	"errors"
	"testing"
)

// Codex round 32 on a8626113 (WIS-625 post-merge).
//   - A white strip 1/512 high at y 65833 under a -65536 translation vanishes in float32; the
//     float64 reading took it as erasing a dark hairline under the text. A paint only one reading
//     records cannot erase.
//   - A searchable scan's content was bounded at 4,096 bytes before its invisible layer was set
//     aside, so an ordinary layer of a few KB refused the scan. The whole content is bounded at
//     1 MiB and the drawing left once the layer is set aside at 4,096 bytes.
//   - "/Annots []" refused a scan; an empty array shows nothing.
func TestPDFFloat32ErasersOCRLayerSizeAndEmptyAnnots(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	t.Run("an eraser that vanishes in float32", func(t *testing.T) {
		page := "q 0 g 72 300 50 0.001953125 re f Q q 1 0 0 1 0 -65536 cm 1 g 72 65836 50 0.001953125 re f Q " + visibleLine
		if got, err := Extract(t.Context(), pdfDoc{pages: []string{page}}.bytes(), KindPDF, DefaultLimits); got != "" || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("got %q %v, want %v", got, err, ErrPDFScanHiddenContent)
		}
	})
	t.Run("an eraser in both readings", func(t *testing.T) {
		page := "q 0 g 72 300 50 0.001953125 re f 1 g 72 300 50 0.001953125 re f Q " + visibleLine
		if got, err := Extract(t.Context(), pdfDoc{pages: []string{page}}.bytes(), KindPDF, DefaultLimits); err != nil || got != "Visible line\n" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	scan := "q 612 0 0 792 0 0 cm /Im1 Do Q "
	for name, c := range map[string]struct {
		page, pageExtra string
	}{
		"a searchable scan with a 5 KB layer": {page: scan + "BT /F1 10 Tf 3 Tr 12 TL 40 760 Td (Invisible scanner layer line 00 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 01 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 02 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 03 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 04 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 05 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 06 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 07 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 08 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 09 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 10 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 11 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 12 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 13 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 14 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 15 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 16 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 17 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 18 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 19 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 20 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 21 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 22 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 23 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 24 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 25 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 26 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 27 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 28 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 29 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 30 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 31 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 32 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 33 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 34 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 35 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 36 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 37 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 38 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 39 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 40 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 41 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 42 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 43 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 44 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 45 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 46 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 47 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 48 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 49 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 50 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 51 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 52 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 53 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 54 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 55 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 56 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 57 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 58 with ordinary recognised words on it) Tj T* (Invisible scanner layer line 59 with ordinary recognised words on it) Tj T* ET"},
		"a scan with an empty /Annots":        {page: scan, pageExtra: "/Annots []"},
	} {
		t.Run(name, func(t *testing.T) {
			pages, err := ScanPagesOfPDF(t.Context(), pdfDoc{pages: []string{c.page}, pageExtra: c.pageExtra, resources: imageResources(6), extra: []string{figure}}.bytes())
			if err != nil || len(pages) != 1 {
				t.Fatalf("%d pages %v", len(pages), err)
			}
		})
	}
	t.Run("a scan with an annotation", func(t *testing.T) {
		data := pdfDoc{pages: []string{scan}, pageExtra: "/Annots [<< /Type /Annot /Subtype /Square /Rect [0 0 10 10] >>]", resources: imageResources(6), extra: []string{figure}}.bytes()
		if pages, err := ScanPagesOfPDF(t.Context(), data); pages != nil || err == nil {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
}
