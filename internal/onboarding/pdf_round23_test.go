package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 18 on 1f111a00 (WIS-625 post-merge).
//   - A rectangle built under a shear and stroked after Q kept the right-angle reach, though its
//     corners are sharp: a white miter spike covered the text unseen.
//   - Italic text split across operators ("(f) Tj (i) Tj") was refused for the f's overhang into
//     the next code, which "(fi) Tj" allows.
//   - An image clipped away entirely still covered the text under it.
//   - A paint clipped away entirely still made a blank page "painted", refusing the upload.
func TestPDFShearedMitersAdjacentRunsAndClippedDraws(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	italic := func(content string) []byte {
		b := pdfDoc{pages: []string{content}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica"), []byte("/BaseFont /Times-Italic"), 1))
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// A rhombus with a 44 degree corner at (100, 330): its miter reaches down to about y 303.
		"a sheared rectangle stroked after Q":  {data: pdfDoc{pages: []string{visibleLine + " q 1 G 20 w 10 M 0 j q 28 70 -28 70 100 330 cm 0 0 1 1 re Q S Q"}}.bytes(), want: ErrPDFScanHiddenContent},
		"a sheared rectangle with bevel joins": {data: pdfDoc{pages: []string{visibleLine + " q 1 G 20 w 10 M 2 j q 28 70 -28 70 100 330 cm 0 0 1 1 re Q S Q"}}.bytes(), text: "Visible line\n"},
		"italic f then i, two operators":       {data: italic("BT /F1 12 Tf 72 300 Td (f) Tj (i) Tj ET"), text: "fi\n"},
		"italic f then i, one operator":        {data: italic("BT /F1 12 Tf 72 300 Td (fi) Tj ET"), text: "fi\n"},
		"an image clipped away over the text":  {data: pdfDoc{pages: []string{visibleLine + " q 0 0 10 10 re W n 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{imageObject}}.bytes(), text: "Visible line\n"},
		"an image clipped onto the text":       {data: pdfDoc{pages: []string{visibleLine + " q 0 290 100 25 re W n 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{imageObject}}.bytes(), want: ErrPDFScanHiddenContent},
		"a fill clipped away on a blank page":  {data: pdfDoc{pages: []string{visibleLine, "q 0 0 10 10 re W n 0 g 100 100 200 200 re f Q"}}.bytes(), text: "Visible line\n"},
		"a fill on a blank page":               {data: pdfDoc{pages: []string{visibleLine, "q 0 g 100 100 200 200 re f Q"}}.bytes(), want: ErrPDFNoTextLayer},
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
}
