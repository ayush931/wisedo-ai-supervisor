package onboarding

import (
	"bytes"
	"errors"
	"testing"
)

// Codex round 19 on 33f6c832 (WIS-625 post-merge).
//   - A 0 w stroke under ".01 0 0 .01 0 0 cm" was bounded at about 0.01 pt; renderers draw any
//     stroke at least a device pixel wide, so each side now reaches at least half a point.
//   - A rectangle under "2 0 0 1 cm" keeps right angles but was given the miter-limit reach
//     (the test was similarity, not axis alignment); the reach is now per axis.
//   - Times-BoldItalic "(o) Tj (f) Tj": the f overhangs left; runs of one size meet by advance.
//   - A figure the clip leaves nothing of is discarded, not hidden; a figure qualifies by what
//     the clip leaves.
//   - A clip of zero area leaves nothing: such a paint or draw shows nothing.
func TestPDFHairlinesAxisScalingItalicRunsAndClippedFigures(t *testing.T) {
	t.Parallel()
	requirePDFSandbox(t)
	boldItalic := func(content string) []byte {
		b := pdfDoc{pages: []string{content}}.bytes()
		return rebuildXref(t, bytes.Replace(b, []byte("/BaseFont /Helvetica"), []byte("/BaseFont /Times-BoldItalic"), 1))
	}
	for name, c := range map[string]struct {
		data []byte
		text string
		want error
	}{
		// At y 309 the hairline lies just above the line's box; a pixel-wide stroke reaches into it.
		"a hairline under a tiny transform": {data: pdfDoc{pages: []string{visibleLine + " q 0 G 0 w .01 0 0 .01 0 0 cm 7200 30900 m 12900 30900 l S Q"}}.bytes(), want: ErrPDFScanHiddenContent},
		"a hairline clear of the text":      {data: pdfDoc{pages: []string{visibleLine + " q 0 G 0 w .01 0 0 .01 0 0 cm 7200 31100 m 12900 31100 l S Q"}}.bytes(), text: "Visible line\n"},
		// A rectangle 4 pt above the line: a right-angle corner reaches 0.7 pt, a miter 5 pt.
		"a rectangle under a per-axis scale":  {data: pdfDoc{pages: []string{visibleLine + " q 0 G 1 w 10 M 0 j 2 0 0 1 0 0 cm 36 313 50 20 re S Q"}}.bytes(), text: "Visible line\n"},
		"a rectangle under a shear":           {data: pdfDoc{pages: []string{visibleLine + " q 0 G 1 w 10 M 0 j 2 0 0.5 1 0 0 cm -40 313 50 20 re S Q"}}.bytes(), want: ErrPDFScanHiddenContent},
		"bold italic o then f, two operators": {data: boldItalic("BT /F1 12 Tf 72 300 Td (o) Tj (f) Tj ET"), text: "of\n"},
		"a fill under a zero-width clip":      {data: pdfDoc{pages: []string{visibleLine + " q 100 290 0 25 re W n 0 g 60 290 200 25 re f Q"}}.bytes(), text: "Visible line\n"},
		"an image under a zero-height clip":   {data: pdfDoc{pages: []string{visibleLine + " q 60 300 200 0 re W n 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{imageObject}}.bytes(), text: "Visible line\n"},
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
	// A 1000 x 1000 figure drawn over the text but clipped away entirely: nothing of it shows, so
	// it is neither read nor counted, and the text is.
	t.Run("a figure clipped away over the text", func(t *testing.T) {
		figure := figureObject(scanImage(t, 1000, 1000, "jpeg"), 1000, 1000)
		doc := pdfDoc{pages: []string{visibleLine + " q 0 0 10 10 re W n 300 0 0 300 50 200 cm /Im1 Do Q"}, resources: imageResources(6), extra: []string{figure}}.bytes()
		got, err := prepareDocument(t.Context(), DocumentUpload{Title: "SOP", DeclaredType: "application/pdf", Data: doc})
		if err != nil || got.text != "Visible line\n" || len(got.images) != 0 || got.skipped != 0 {
			t.Fatalf("got %q, %d images, %d skipped, %v", got.text, len(got.images), got.skipped, err)
		}
	})
}
