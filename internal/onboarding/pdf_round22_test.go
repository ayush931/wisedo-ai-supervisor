package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 17 on 2e8a51f8 (WIS-625 post-merge).
//   - Text over text compared advance cells only: Times-Italic f advances 278 but its glyph
//     reaches from -147 to 424, so a large f set after small text covered it while the advances
//     stayed apart. A standard font's codes are compared by their glyph box too.
//   - A figure was refused for a blend mode declared in the resources, even one reset or unused;
//     the state in force at the draw decides.
//   - A paint clipped away entirely still covered the text under its unclipped box.
func TestPDFGlyphOverhangDrawStateAndClippedPaint(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	italic := func(content string) []byte {
		b := pdfDoc{pages: []string{content}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica"), []byte("/BaseFont /Times-Italic"), 1))
	}
	figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
	states := "/XObject << /Im1 6 0 R >> /ExtGState << /Mul << /BM /Multiply >> /Nor << /BM /Normal >> /Half << /ca 0.5 >> >>"
	draw := " q 300 0 0 300 150 400 cm /Im1 Do Q"
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// "a" at 10 pt spans 100 to 105; the 100 pt f at 107 reaches back to 92.3.
		"a large italic f over small text":    {data: italic("BT /F1 10 Tf 100 300 Td (a) Tj ET BT /F1 100 Tf 107 300 Td (f) Tj ET"), want: ErrPDFScanHiddenContent},
		"the large f clear of the small text": {data: italic("BT /F1 10 Tf 100 300 Td (a) Tj ET BT /F1 100 Tf 130 300 Td (f) Tj ET"), text: "a f\n"},
		"italic text set run after run":       {data: italic("BT /F1 12 Tf 72 300 Td (Visible) Tj ( line) Tj ET"), text: "Visible line\n"},
		"a fill clipped away over the text":   {data: pdfDoc{pages: []string{visibleLine + " q 0 0 10 10 re W n 1 g 60 290 200 25 re f Q"}}.bytes(), text: "Visible line\n"},
		"a fill clipped onto the text":        {data: pdfDoc{pages: []string{visibleLine + " q 0 290 100 25 re W n 1 g 60 290 200 25 re f Q"}}.bytes(), want: ErrPDFScanHiddenContent},
		"a fill under a clip not a rectangle": {data: pdfDoc{pages: []string{visibleLine + " q 0 0 m 10 0 l 0 10 l h W n 1 g 60 290 200 25 re f Q"}}.bytes(), want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Extract(t.Context(), c.data, KindPDF, DefaultLimits)
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
	// Figures are read on the document path (prepareDocument), which sends them to the vision model.
	for name, c := range map[string]struct {
		page   string
		images int
		want   error
	}{
		"a figure after a blend reset":    {page: "/Mul gs /Nor gs " + visibleLine + draw, images: 1},
		"a figure beside an unused blend": {page: visibleLine + draw, images: 1},
		"a figure drawn blended":          {page: visibleLine + " /Mul gs" + draw, want: ErrPDFScanHiddenContent},
		"a figure drawn translucent":      {page: visibleLine + " /Half gs" + draw, want: ErrPDFScanHiddenContent},
	} {
		t.Run(name, func(t *testing.T) {
			doc := pdfDoc{pages: []string{c.page}, resources: states, extra: []string{figure}}.bytes()
			got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc})
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %d images %v, want %v", len(got.images), err, c.want)
				}
				return
			}
			if err != nil || len(got.images) != c.images || got.text != "Visible line\n" {
				t.Fatalf("got %q, %d images, %v", got.text, len(got.images), err)
			}
		})
	}
}
