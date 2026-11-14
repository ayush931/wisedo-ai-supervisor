package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// Codex round 16 on e561fee9 (WIS-625 post-merge).
//   - A code a ToUnicode CMap maps to nothing decodes to no glyph, so the show counted as empty
//     and a scan's mode-0 "(H) Tj" over its image was dropped while a viewer paints the H. Shows
//     are counted by the codes they show, not by what those decode to.
//   - A later ExtGState with /BM /Normal or /SMask /None did not clear an earlier blend or mask.
func TestPDFCodesMappedToNothingAndBlendResets(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /Empty def 1 begincodespacerange <00> <FF> endcodespacerange 1 beginbfchar <48> <> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end"
	emptyMapped := func(page string) []byte {
		b := pdfDoc{pages: []string{page}, resources: imageResources(6), extra: []string{
			figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(cmap), cmap),
		}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica >>"), []byte("/BaseFont /Helvetica /ToUnicode 7 0 R >>"), 1))
	}
	scan := "q 612 0 0 792 0 0 cm /Im1 Do Q"
	t.Run("a code mapped to nothing painted over a scan", func(t *testing.T) {
		pages, err := ScanPagesOfPDF(t.Context(), emptyMapped(scan+" BT /F1 48 Tf 300 400 Td (H) Tj ET"))
		if pages != nil || !errors.Is(err, ErrPDFScanHiddenContent) {
			t.Fatalf("%d pages %v, want %v", len(pages), err, ErrPDFScanHiddenContent)
		}
	})
	t.Run("the same scan without the painted code", func(t *testing.T) {
		if pages, err := ScanPagesOfPDF(t.Context(), emptyMapped(scan)); err != nil || len(pages) != 1 {
			t.Fatalf("%d pages %v", len(pages), err)
		}
	})
	states := "/ExtGState << /Mul << /BM /Multiply >> /Nor << /BM /Normal >> /Msk << /SMask /Luminosity >> /NoMsk << /SMask /None >> >>"
	for name, c := range map[string]struct {
		page string
		text string
		want error
	}{
		"a blend reset to Normal":   {page: "/Mul gs /Nor gs " + visibleLine, text: "Visible line\n"},
		"a soft mask reset to None": {page: "/Msk gs /NoMsk gs " + visibleLine, text: "Visible line\n"},
		"a blend left on":           {page: "/Mul gs " + visibleLine, want: ErrPDFScanHiddenContent},
		"a soft mask left on":       {page: "/Msk gs /Nor gs " + visibleLine, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), pdfDoc{pages: []string{c.page}, resources: states}.bytes(), KindPDF, DefaultLimits)
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
